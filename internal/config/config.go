// Package config loads and validates sentinel.yaml.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// Version is the current config schema major version. Configs with a
// different major version are rejected.
const Version = "1"

// Config is the parsed sentinel.yaml.
type Config struct {
	Version  string                  `yaml:"version"`
	Audit    AuditConfig             `yaml:"audit"`
	Defaults Defaults                `yaml:"defaults"`
	Policy   PolicyConfig            `yaml:"policy"`
	Central  CentralConfig           `yaml:"central,omitempty"`
	Servers  map[string]ServerConfig `yaml:"servers"`
}

// CentralConfig opts the agent into pushing its audit events to a
// self-hosted sentinel-server. Leaving this block empty / unset keeps
// the agent purely local (v0.1 behavior).
type CentralConfig struct {
	// URL is the central server base URL, e.g. https://central.acme.internal.
	URL string `yaml:"url,omitempty"`

	// Token is the bearer token issued by `sentinel-server agent create`.
	Token string `yaml:"token,omitempty"`

	// AgentName is a friendly display name for this agent. Defaults to
	// the host's name if unset.
	AgentName string `yaml:"agent_name,omitempty"`

	// FlushIntervalSeconds is how often the pump flushes new events.
	// Defaults to 5 if unset.
	FlushIntervalSeconds int `yaml:"flush_interval_seconds,omitempty"`

	// BatchSize is the max number of events per POST. Defaults to 100.
	BatchSize int `yaml:"batch_size,omitempty"`

	// Enabled is true by default when URL+Token are set. Explicitly setting
	// false disables the pump without removing the credentials, useful for
	// debugging.
	Enabled *bool `yaml:"enabled,omitempty"`
}

// IsActive returns true if telemetry should run with this config.
func (c CentralConfig) IsActive() bool {
	if c.URL == "" || c.Token == "" {
		return false
	}
	if c.Enabled != nil && !*c.Enabled {
		return false
	}
	return true
}

// PolicyConfig configures the security engine.
type PolicyConfig struct {
	// Enabled toggles the policy engine. Default true.
	Enabled *bool `yaml:"enabled,omitempty"`

	// DenyPaths is a list of substring or glob patterns. Any string argument
	// in a tools/call whose value matches is blocked. Built-in sensitive
	// paths (~/.ssh, ~/.aws, etc.) are always denied regardless of this list.
	DenyPaths []string `yaml:"deny_paths,omitempty"`

	// Scoring configures the risk-score cutoffs. Zero/missing → defaults
	// (approve_threshold=30, block_threshold=80).
	Scoring ScoringConfig `yaml:"scoring,omitempty"`
}

// ScoringConfig configures the risk-score thresholds and approval timeout.
//
//	score < ApproveThreshold     → allow
//	score < BlockThreshold       → require human approval
//	score >= BlockThreshold      → block
type ScoringConfig struct {
	ApproveThreshold       int `yaml:"approve_threshold,omitempty"`
	BlockThreshold         int `yaml:"block_threshold,omitempty"`
	ApprovalTimeoutSeconds int `yaml:"approval_timeout_seconds,omitempty"`
}

// AuditConfig configures the audit log.
type AuditConfig struct {
	Path string `yaml:"path"`
}

// Defaults are settings inherited by every server unless overridden.
type Defaults struct {
	Env EnvConfig `yaml:"env"`
}

// ServerConfig is the configuration for one upstream MCP server.
//
// Exactly one transport must be configured:
//   - `command` (+ optional args/env) for a local stdio subprocess
//   - `url` (+ optional headers) for a remote Streamable HTTP server
type ServerConfig struct {
	Command string    `yaml:"command,omitempty"`
	Args    []string  `yaml:"args,omitempty"`
	Env     EnvConfig `yaml:"env,omitempty"`

	URL     string            `yaml:"url,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
}

// EnvConfig controls which environment variables reach the upstream.
type EnvConfig struct {
	// AllowSystem includes a curated list of OS-required vars (PATH, HOME,
	// USERPROFILE, etc.) automatically. Default: true. Set to false for a
	// fully empty environment.
	AllowSystem *bool `yaml:"allow_system,omitempty"`

	// Allow lists additional env var names to pass through (case-sensitive
	// on POSIX, case-insensitive on Windows).
	Allow []string `yaml:"allow,omitempty"`

	// Deny lists env var names to strip even if they would otherwise be
	// allowed. Deny wins over Allow.
	Deny []string `yaml:"deny,omitempty"`
}

// Load parses the YAML at path and returns a validated Config.
// If path is empty, the default config locations are searched.
func Load(path string) (*Config, error) {
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	return Parse(raw, path)
}

// Parse is Load without the filesystem read.
func Parse(raw []byte, source string) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", source, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", source, err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.Version != "" && c.Version != Version {
		return fmt.Errorf("unsupported config version %q (this build expects %q)", c.Version, Version)
	}
	if len(c.Servers) == 0 {
		return fmt.Errorf("no servers configured")
	}
	for name, s := range c.Servers {
		if !validName(name) {
			return fmt.Errorf("server name %q is invalid (must match [a-zA-Z0-9_-]+)", name)
		}
		hasCmd := strings.TrimSpace(s.Command) != ""
		hasURL := strings.TrimSpace(s.URL) != ""
		switch {
		case !hasCmd && !hasURL:
			return fmt.Errorf("server %q: one of 'command' or 'url' is required", name)
		case hasCmd && hasURL:
			return fmt.Errorf("server %q: cannot set both 'command' and 'url'", name)
		}
		if hasURL && !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
			return fmt.Errorf("server %q: url must start with http:// or https://", name)
		}
	}
	return nil
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// Server returns the resolved server config (merged with defaults) for name,
// or an error if no such server exists.
func (c *Config) Server(name string) (ServerConfig, error) {
	s, ok := c.Servers[name]
	if !ok {
		known := make([]string, 0, len(c.Servers))
		for k := range c.Servers {
			known = append(known, k)
		}
		return ServerConfig{}, fmt.Errorf("server %q not found (configured: %s)", name, strings.Join(known, ", "))
	}
	s.Env = mergeEnv(c.Defaults.Env, s.Env)
	return s, nil
}

// mergeEnv layers a server's env config on top of the defaults.
//
// AllowSystem: server value wins if set, else default value wins.
// Allow, Deny: concatenated (server values appended after defaults).
func mergeEnv(def, srv EnvConfig) EnvConfig {
	out := EnvConfig{
		Allow: append([]string{}, def.Allow...),
		Deny:  append([]string{}, def.Deny...),
	}
	out.Allow = append(out.Allow, srv.Allow...)
	out.Deny = append(out.Deny, srv.Deny...)

	switch {
	case srv.AllowSystem != nil:
		out.AllowSystem = srv.AllowSystem
	case def.AllowSystem != nil:
		out.AllowSystem = def.AllowSystem
	default:
		t := true
		out.AllowSystem = &t
	}
	return out
}

// FilterEnv builds the env slice (KEY=VALUE format) that should be passed to
// the upstream subprocess, given the source environment and the EnvConfig.
//
// The rules:
//  1. If AllowSystem is true (default), the OS-specific system env names
//     are included.
//  2. Names in Allow are included.
//  3. Names in Deny are excluded, regardless of (1) and (2).
//  4. Comparison on Windows is case-insensitive (matches Win32 behavior);
//     elsewhere it is exact.
func FilterEnv(source []string, cfg EnvConfig) []string {
	allowSystem := true
	if cfg.AllowSystem != nil {
		allowSystem = *cfg.AllowSystem
	}

	allow := make(map[string]struct{})
	if allowSystem {
		for _, k := range systemAllowlist() {
			allow[normalizeEnvName(k)] = struct{}{}
		}
	}
	for _, k := range cfg.Allow {
		allow[normalizeEnvName(k)] = struct{}{}
	}
	deny := make(map[string]struct{})
	for _, k := range cfg.Deny {
		deny[normalizeEnvName(k)] = struct{}{}
	}

	out := make([]string, 0, len(source))
	for _, kv := range source {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		name := normalizeEnvName(kv[:eq])
		if _, denied := deny[name]; denied {
			continue
		}
		if _, allowed := allow[name]; allowed {
			out = append(out, kv)
		}
	}
	return out
}

func normalizeEnvName(s string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(s)
	}
	return s
}

// systemAllowlist returns OS-required env vars that are passed through by
// default. Intentionally small — any tool-specific var (NODE_PATH,
// GITHUB_TOKEN, etc.) must be added explicitly.
func systemAllowlist() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{
			"PATH", "PATHEXT",
			"USERPROFILE", "USERNAME", "USERDOMAIN", "COMPUTERNAME",
			"APPDATA", "LOCALAPPDATA", "PROGRAMDATA",
			"TEMP", "TMP",
			"SYSTEMROOT", "SYSTEMDRIVE", "COMSPEC", "WINDIR",
			"PROCESSOR_ARCHITECTURE", "NUMBER_OF_PROCESSORS",
			"OS", "HOMEDRIVE", "HOMEPATH",
		}
	default:
		return []string{
			"PATH", "HOME", "USER", "LOGNAME", "SHELL",
			"LANG", "LC_ALL", "LC_CTYPE", "TZ",
			"TMPDIR", "TERM", "PWD",
		}
	}
}

// DefaultPath returns the OS-appropriate default config path.
func DefaultPath() (string, error) {
	if runtime.GOOS == "windows" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, "sentinel", "sentinel.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".sentinel", "sentinel.yaml"), nil
}

// Starter returns the YAML body of a starter config file. Used by
// `sentinel init` to bootstrap a new install.
func Starter() string {
	return `# sentinel.yaml - runtime security policy for MCP servers
#
# Each server entry tells sentinel how to launch an upstream MCP server and
# what environment it is allowed to see. By default no env vars beyond the
# OS-required ones reach the upstream - add explicit names under env.allow
# for anything the server needs (API tokens, NODE_PATH, etc.).
#
# Run a configured server:
#   sentinel run --server <name>

version: "1"

audit:
  # Path defaults to OS config dir (~/.sentinel/audit.db on Unix,
  # %APPDATA%\sentinel\audit.db on Windows). Override here if you want it
  # somewhere else.
  # path: /var/log/sentinel/audit.db

defaults:
  env:
    # The OS-required system env (PATH, HOME, USERPROFILE, etc.) is included
    # automatically. Set allow_system: false to start from an empty env.
    allow_system: true

    # Names to pass through to every server in addition to the system defaults.
    # allow: []

    # Names to always strip even if otherwise allowed.
    # deny:
    #   - SUDO_ASKPASS

servers:
  # Example: filesystem server scoped to a directory.
  # filesystem:
  #   command: npx
  #   args:
  #     - "-y"
  #     - "@modelcontextprotocol/server-filesystem"
  #     - "~/Documents"
  #   env:
  #     allow:
  #       - NODE_PATH

  # Example: GitHub server that needs a token.
  # github:
  #   command: npx
  #   args:
  #     - "-y"
  #     - "@modelcontextprotocol/server-github"
  #   env:
  #     allow:
  #       - GITHUB_TOKEN
  #       - NODE_PATH
`
}
