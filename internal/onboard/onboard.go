// Package onboard wires up an employee laptop in one step: detect their
// AI client config, transplant the MCP server entries into sentinel.yaml,
// rewrite the client config to launch each server through sentinel, and
// keep a timestamped .bak of anything we overwrite.
//
// This package is used by `sentinel init` (solo case) and `sentinel enroll`
// (company case). It's a thin orchestrator: the heavy lifting lives in
// detect-paths and merge-config helpers. Nothing here is MCP-spec
// dependent; we only know that mcpServers is a JSON object whose values
// are {command, args?, env?}.
package onboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// CentralBlock is the optional self-hosted-server block that the enroll
// flow injects into sentinel.yaml. Nil for the solo flow.
type CentralBlock struct {
	URL       string `yaml:"url"`
	Token     string `yaml:"token"`
	AgentName string `yaml:"agent_name,omitempty"`
}

// ClientReport is the per-client migration summary.
type ClientReport struct {
	// DisplayName is a friendly label ("Claude Desktop", "Cursor", or the
	// basename of the path when we don't know the client).
	DisplayName           string
	ConfigPath            string
	BackupPath            string // empty if file was untouched
	ServersMigrated       []string
	ServersAlreadyWrapped []string
}

// Report summarises what changed. Used by callers to print a "what just
// happened" trace and for tests.
type Report struct {
	AgentConfigPath   string
	AgentConfigBackup string

	// Clients is one entry per client config file we processed (whether
	// successfully migrated, idempotent skip, or "no servers found"). The
	// list is empty if no clients were detected on this machine.
	Clients []ClientReport

	// NoClientsFound is true when neither the explicit ClientConfigPath /
	// ClientPaths nor auto-detection found any MCP-capable client config.
	// Not necessarily an error — central enrollment without any local
	// client still writes sentinel.yaml so a future client install picks
	// it up.
	NoClientsFound bool

	// Backwards-compat fields. Populated from Clients[0] when exactly one
	// Claude Desktop config was processed; otherwise empty/zero. Existing
	// callers (and tests written against slice 0.2.5) keep working.
	ClaudeConfigPath      string
	ClaudeConfigBackup    string
	ServersMigrated       []string
	ServersAlreadyWrapped []string
	ClaudeConfigMissing   bool
}

// Client identifies one AI tool whose MCP config we know how to migrate.
// All known clients share the same on-disk JSON shape (object with
// "mcpServers"), so once we find the file we use the same code path.
type Client struct {
	Name string
	// Path is the absolute path to the config file. Empty if we don't
	// know where to look on this OS.
	Path string
	// Exists is true if a file is present at Path.
	Exists bool
}

// DetectClaudeDesktopConfigPath returns the OS-specific config path and
// whether the file exists. Empty path + false means we don't know where
// to look on this platform.
func DetectClaudeDesktopConfigPath() (string, bool) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", false
	}
	candidate := ""
	switch runtime.GOOS {
	case "darwin", "windows":
		candidate = filepath.Join(base, "Claude", "claude_desktop_config.json")
	case "linux":
		// Claude Desktop on Linux is community-built; the path isn't
		// universally established. We probe the most common one.
		candidate = filepath.Join(base, "Claude", "claude_desktop_config.json")
	default:
		return "", false
	}
	if _, err := os.Stat(candidate); err == nil {
		return candidate, true
	}
	return candidate, false
}

// DetectCursorConfigPath probes the known Cursor MCP config locations
// and returns the first one that exists. Cursor's MCP support has moved
// around; we check the global user-config locations first. Returns
// ("", false) if no Cursor MCP config is present (the most common case
// when Cursor isn't installed on this machine).
func DetectCursorConfigPath() (string, bool) {
	home, err := os.UserHomeDir()
	if err == nil {
		// Most stable Cursor location across recent versions.
		candidate := filepath.Join(home, ".cursor", "mcp.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true
		}
	}
	base, err := os.UserConfigDir()
	if err == nil {
		// Older path Cursor used; keep probing for migration users.
		candidate := filepath.Join(base, "Cursor", "User", "globalStorage", "mcp.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true
		}
	}
	return "", false
}

// DetectClients returns the list of MCP-capable AI clients we found on
// this machine. Only clients with an existing config are returned;
// callers iterate and pass each path through Run.
func DetectClients() []Client {
	var out []Client
	if p, ok := DetectClaudeDesktopConfigPath(); ok {
		out = append(out, Client{Name: "Claude Desktop", Path: p, Exists: true})
	}
	if p, ok := DetectCursorConfigPath(); ok {
		out = append(out, Client{Name: "Cursor", Path: p, Exists: true})
	}
	return out
}

// Options controls a single onboarding pass.
type Options struct {
	// ClientConfigPath is the Claude Desktop config path. If empty AND
	// ClientPaths is empty, we auto-detect every known client (currently
	// Claude Desktop + Cursor). Kept for backward compatibility with
	// callers from slice 0.2.5; new callers should use ClientPaths.
	ClientConfigPath string

	// ClientPaths is an explicit list of client config files to migrate.
	// If both ClientConfigPath and ClientPaths are empty, DetectClients()
	// is used.
	ClientPaths []string

	// AgentConfigPath is where sentinel.yaml lives. Required.
	AgentConfigPath string

	// SentinelCmd is the command name (or absolute path) to put in the
	// client config. Almost always "sentinel"; on Windows when we know
	// the binary isn't on PATH yet, the absolute path is safer.
	SentinelCmd string

	// Central, if non-nil, is written to sentinel.yaml's central: block.
	Central *CentralBlock

	// DryRun does the planning but writes nothing. Useful for the SPA-side
	// "what would change" preview if we ever add one.
	DryRun bool
}

// Run executes one onboarding pass and returns a Report. Errors are
// fatal-to-this-pass; partial writes are not left behind because every
// destination is written atomically via a temp file + rename.
//
// Slice 0.2.5.1: now iterates over all detected (or explicitly listed)
// AI-client configs instead of only Claude Desktop. Backwards-compat
// fields on Report are populated from the first Claude Desktop client
// so callers from 0.2.5 keep working.
func Run(opts Options) (*Report, error) {
	if opts.AgentConfigPath == "" {
		return nil, errors.New("onboard: AgentConfigPath is required")
	}
	if opts.SentinelCmd == "" {
		opts.SentinelCmd = "sentinel"
	}

	// Resolve which client configs we're going to migrate.
	clients := resolveClients(opts)
	rpt := &Report{
		AgentConfigPath: opts.AgentConfigPath,
		NoClientsFound:  len(clients) == 0,
	}

	// Load existing sentinel.yaml (if any) into a generic node tree so we
	// can preserve user comments and unknown keys on rewrite.
	agentRaw, agentExists, err := readAgentYAML(opts.AgentConfigPath)
	if err != nil {
		return nil, err
	}

	// Plan all client edits first; only write to disk after every plan
	// succeeds so a parse error on one client doesn't half-onboard the
	// laptop.
	type plannedClient struct {
		c       Client
		raw     map[string]json.RawMessage
		servers map[string]claudeEntry
		report  ClientReport
	}
	var plans []plannedClient

	for _, c := range clients {
		p := plannedClient{c: c, report: ClientReport{DisplayName: c.Name, ConfigPath: c.Path}}
		data, err := os.ReadFile(c.Path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// File vanished between detection and read; treat as no-op.
				plans = append(plans, p)
				continue
			}
			return nil, fmt.Errorf("read %s config %q: %w", c.Name, c.Path, err)
		}
		if err := json.Unmarshal(data, &p.raw); err != nil {
			return nil, fmt.Errorf("parse %s config %q: %w", c.Name, c.Path, err)
		}
		if v, ok := p.raw["mcpServers"]; ok {
			if err := json.Unmarshal(v, &p.servers); err != nil {
				return nil, fmt.Errorf("parse mcpServers in %s: %w", c.Name, err)
			}
		}
		if p.servers == nil {
			p.servers = map[string]claudeEntry{}
		}

		// Plan the migration for this client.
		wrapped := wrappedCmd(opts.SentinelCmd)
		for name, entry := range p.servers {
			if isAlreadyWrapped(entry, wrapped) {
				p.report.ServersAlreadyWrapped = append(p.report.ServersAlreadyWrapped, name)
				continue
			}
			if !agentHasServer(agentRaw, name) {
				setAgentServer(agentRaw, name, entry)
			}
			p.servers[name] = claudeEntry{
				Command: opts.SentinelCmd,
				Args:    []string{"run", "--server", name},
			}
			p.report.ServersMigrated = append(p.report.ServersMigrated, name)
		}
		sort.Strings(p.report.ServersMigrated)
		sort.Strings(p.report.ServersAlreadyWrapped)
		plans = append(plans, p)
	}

	// Inject the central: block.
	if opts.Central != nil {
		setAgentCentral(agentRaw, opts.Central)
	}

	// Collect per-client reports.
	for _, p := range plans {
		rpt.Clients = append(rpt.Clients, p.report)
	}
	populateBackwardsCompat(rpt)

	if opts.DryRun {
		return rpt, nil
	}

	// Write sentinel.yaml first so a failure mid-flight doesn't leave a
	// client pointing at a server that isn't yet configured.
	if agentExists {
		bak, err := writeBackup(opts.AgentConfigPath)
		if err != nil {
			return nil, err
		}
		rpt.AgentConfigBackup = bak
	}
	if err := writeAgentYAML(opts.AgentConfigPath, agentRaw); err != nil {
		return nil, err
	}

	// Then rewrite each client config that had at least one migration.
	for i, p := range plans {
		if len(p.report.ServersMigrated) == 0 {
			continue
		}
		bak, err := writeBackup(p.c.Path)
		if err != nil {
			return nil, err
		}
		rpt.Clients[i].BackupPath = bak

		newServers, err := json.MarshalIndent(p.servers, "", "  ")
		if err != nil {
			return nil, err
		}
		if p.raw == nil {
			p.raw = map[string]json.RawMessage{}
		}
		p.raw["mcpServers"] = newServers
		out, err := json.MarshalIndent(p.raw, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := atomicWrite(p.c.Path, append(out, '\n'), 0o644); err != nil {
			return nil, err
		}
	}
	// Update backwards-compat backup field after disk writes.
	populateBackwardsCompat(rpt)

	return rpt, nil
}

// resolveClients picks the list of client config files to migrate, in
// priority order:
//  1. opts.ClientPaths if non-empty (explicit list).
//  2. opts.ClientConfigPath if non-empty (legacy single-client API).
//  3. DetectClients() — auto-detect everything we know about.
func resolveClients(opts Options) []Client {
	if len(opts.ClientPaths) > 0 {
		out := make([]Client, 0, len(opts.ClientPaths))
		for _, p := range opts.ClientPaths {
			out = append(out, Client{Name: clientNameFromPath(p), Path: p, Exists: true})
		}
		return out
	}
	if opts.ClientConfigPath != "" {
		// Legacy single-path API. Only include if it actually exists so
		// callers that pass a default path don't get spurious "missing
		// file" errors.
		if _, err := os.Stat(opts.ClientConfigPath); err == nil {
			return []Client{{Name: clientNameFromPath(opts.ClientConfigPath), Path: opts.ClientConfigPath, Exists: true}}
		}
		return nil
	}
	return DetectClients()
}

func clientNameFromPath(p string) string {
	low := strings.ToLower(p)
	switch {
	case strings.Contains(low, "claude_desktop_config"):
		return "Claude Desktop"
	case strings.Contains(low, "cursor") && strings.HasSuffix(low, "mcp.json"):
		return "Cursor"
	default:
		return filepath.Base(p)
	}
}

// populateBackwardsCompat fills the deprecated single-Claude-config
// fields on Report from the first Claude Desktop entry in Clients.
// Existing callers from slice 0.2.5 keep working unchanged.
func populateBackwardsCompat(r *Report) {
	for _, c := range r.Clients {
		if c.DisplayName == "Claude Desktop" {
			r.ClaudeConfigPath = c.ConfigPath
			r.ClaudeConfigBackup = c.BackupPath
			r.ServersMigrated = c.ServersMigrated
			r.ServersAlreadyWrapped = c.ServersAlreadyWrapped
			return
		}
	}
	// No Claude Desktop in the set → keep the legacy "missing" signal.
	r.ClaudeConfigMissing = true
}

// ---------------------------------------------------------------------------
// YAML helpers — operate on a yaml.Node tree to preserve comments / order
// ---------------------------------------------------------------------------

func readAgentYAML(path string) (*yaml.Node, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Build a minimal skeleton: a mapping with version: "1".
			root := &yaml.Node{
				Kind: yaml.DocumentNode,
				Content: []*yaml.Node{{
					Kind: yaml.MappingNode,
					Content: []*yaml.Node{
						{Kind: yaml.ScalarNode, Value: "version"},
						{Kind: yaml.ScalarNode, Tag: "!!str", Value: "1", Style: yaml.DoubleQuotedStyle},
					},
				}},
			}
			return root, false, nil
		}
		return nil, false, fmt.Errorf("read agent config: %w", err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, true, fmt.Errorf("parse agent config: %w", err)
	}
	// yaml.Unmarshal into a *Node yields a DocumentNode wrapping the real
	// content. An empty file gives an empty Document — recover by
	// installing a fresh mapping.
	if len(root.Content) == 0 {
		root.Kind = yaml.DocumentNode
		root.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	return &root, true, nil
}

func writeAgentYAML(path string, root *yaml.Node) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	buf, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return atomicWrite(path, buf, 0o644)
}

// mappingFor returns the inner mapping node — i.e., the top-level
// {version, servers, central, ...} map — from a Document root.
func mappingFor(root *yaml.Node) *yaml.Node {
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		return root.Content[0]
	}
	return root
}

// findKey returns the value node for the given top-level key, or nil.
func findKey(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(m.Content)-1; i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// upsertKey sets m[key] = value, replacing or appending as needed.
func upsertKey(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i < len(m.Content)-1; i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		value,
	)
}

func agentHasServer(root *yaml.Node, name string) bool {
	m := mappingFor(root)
	servers := findKey(m, "servers")
	if servers == nil || servers.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i < len(servers.Content)-1; i += 2 {
		if servers.Content[i].Value == name {
			return true
		}
	}
	return false
}

func setAgentServer(root *yaml.Node, name string, entry claudeEntry) {
	m := mappingFor(root)
	servers := findKey(m, "servers")
	if servers == nil {
		servers = &yaml.Node{Kind: yaml.MappingNode}
		upsertKey(m, "servers", servers)
	}

	serverMap := &yaml.Node{Kind: yaml.MappingNode}
	// command (scalar)
	serverMap.Content = append(serverMap.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "command"},
		&yaml.Node{Kind: yaml.ScalarNode, Value: entry.Command},
	)
	// args (sequence) — only if non-empty
	if len(entry.Args) > 0 {
		argsNode := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, a := range entry.Args {
			argsNode.Content = append(argsNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: a})
		}
		serverMap.Content = append(serverMap.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "args"},
			argsNode,
		)
	}
	// env (mapping) — only if env vars are present. The Claude shape
	// inlines values; our config splits allow/deny lists. We map by
	// putting each key under env.allow plus appending an env_allow_values
	// comment so the operator can decide whether the inlined value
	// should remain.
	if len(entry.Env) > 0 {
		envMap := &yaml.Node{Kind: yaml.MappingNode}
		allowSeq := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		keys := make([]string, 0, len(entry.Env))
		for k := range entry.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			allowSeq.Content = append(allowSeq.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k})
		}
		envMap.Content = append(envMap.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "allow"},
			allowSeq,
		)
		serverMap.Content = append(serverMap.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "env"},
			envMap,
		)
	}

	upsertKey(servers, name, serverMap)
}

func setAgentCentral(root *yaml.Node, c *CentralBlock) {
	m := mappingFor(root)
	centralMap := &yaml.Node{Kind: yaml.MappingNode}
	add := func(k, v string) {
		if v == "" {
			return
		}
		centralMap.Content = append(centralMap.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: k},
			&yaml.Node{Kind: yaml.ScalarNode, Value: v},
		)
	}
	add("url", c.URL)
	add("token", c.Token)
	add("agent_name", c.AgentName)
	upsertKey(m, "central", centralMap)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type claudeEntry struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

func isAlreadyWrapped(e claudeEntry, wrapped string) bool {
	// Heuristic: command ends in "sentinel" (or matches exactly) AND args
	// start with "run". This is robust against absolute vs bare-name
	// invocations and against operating system path separators.
	cmd := e.Command
	base := filepath.Base(strings.TrimSuffix(cmd, ".exe"))
	if base != "sentinel" {
		_ = wrapped // keep var; we may use it for richer matching later
		return false
	}
	return len(e.Args) > 0 && e.Args[0] == "run"
}

func wrappedCmd(s string) string { return s }

// writeBackup copies path → path.bak.<ISO-time>. Returns the backup path.
func writeBackup(path string) (string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read for backup: %w", err)
	}
	stamp := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	bak := path + ".bak." + stamp
	if err := os.WriteFile(bak, src, 0o644); err != nil {
		return "", fmt.Errorf("write backup: %w", err)
	}
	return bak, nil
}

// atomicWrite writes via tmp file + rename so a crash mid-write doesn't
// leave the file truncated.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".sentinel.tmp.")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		// Best-effort on Windows; rename will still proceed.
	}
	return os.Rename(tmpPath, path)
}
