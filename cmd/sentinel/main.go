// sentinel is the MCP runtime security proxy.
//
// Slice 2 usage:
//
//	sentinel init [--path PATH]
//	sentinel run --server NAME [--config PATH] [--audit PATH]
//	sentinel run -- <upstream-command> [args...]    (ad-hoc, no config)
//	sentinel version
//	sentinel help
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"time"

	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ironmark056/sentinel/internal/approval"
	"github.com/ironmark056/sentinel/internal/audit"
	"github.com/ironmark056/sentinel/internal/centralpolicy"
	"github.com/ironmark056/sentinel/internal/config"
	"github.com/ironmark056/sentinel/internal/dashboard"
	"github.com/ironmark056/sentinel/internal/onboard"
	"github.com/ironmark056/sentinel/internal/policy"
	"github.com/ironmark056/sentinel/internal/proxy"
	"github.com/ironmark056/sentinel/internal/telemetry"
)

const version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "init":
		initCmd(os.Args[2:])
	case "enroll":
		enrollCmd(os.Args[2:])
	case "run":
		runCmd(os.Args[2:])
	case "dashboard":
		dashboardCmd(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("sentinel %s\n", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `sentinel %s — runtime security proxy for MCP servers

Usage:
  sentinel init [--path PATH] [--wrap-claude]
      Write a starter sentinel.yaml. With --wrap-claude, also detect
      the local Claude Desktop config and rewrite each MCP server entry
      to launch through sentinel. A timestamped .bak is saved.

  sentinel enroll <enrollment-url>
      Onboard this machine into a company Sentinel deployment in one
      command. Exchanges the one-time URL for a bearer token, writes
      sentinel.yaml with the central: block, and (unless --no-wrap)
      rewrites the local Claude Desktop config to launch through
      sentinel. Backups are saved.

  sentinel run --server NAME [--config PATH] [--audit PATH]
      Run the named upstream from your config file.

  sentinel run <upstream-command> [args...]
      Run an upstream by inline command, bypassing the config file.
      Useful for ad-hoc testing. No env policy is applied in this mode.
      The "--" separator is optional but accepted for clarity.

  sentinel dashboard [--addr ADDR] [--audit PATH]
      Open the local dashboard for the audit log
      (default http://127.0.0.1:7842).

  sentinel version
  sentinel help

Config:
  Default config path is platform-specific:
    Linux/macOS:  ~/.sentinel/sentinel.yaml
    Windows:      %%APPDATA%%\sentinel\sentinel.yaml

Example (after 'sentinel init' and editing the config):
  sentinel run --server filesystem
`, version)
}

func initCmd(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	pathFlag := fs.String("path", "", "destination path (default: OS config dir)")
	force := fs.Bool("force", false, "overwrite if the file already exists")
	wrapClaude := fs.Bool("wrap-claude", false, "also rewrite the local Claude Desktop config to launch through sentinel")
	_ = fs.Parse(args)

	path := *pathFlag
	if path == "" {
		p, err := config.DefaultPath()
		if err != nil {
			fmt.Fprintf(os.Stderr, "default config path: %v\n", err)
			os.Exit(1)
		}
		path = p
	}

	existed := false
	if _, err := os.Stat(path); err == nil {
		existed = true
	}
	if existed && !*force && !*wrapClaude {
		fmt.Fprintf(os.Stderr, "config already exists at %s (use --force to overwrite, or --wrap-claude to migrate from Claude Desktop)\n", path)
		os.Exit(1)
	}

	if !existed {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "mkdir: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(path, []byte(config.Starter()), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "✓ wrote starter config to %s\n", path)
	}

	if !*wrapClaude {
		return
	}
	runOnboardingWrap(path, nil)
}

func enrollCmd(args []string) {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	configFlag := fs.String("config", "", "path to sentinel.yaml (default: OS config dir)")
	noWrap := fs.Bool("no-wrap", false, "do not rewrite the local Claude Desktop config")
	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "enroll: exactly one argument required — the enrollment URL the admin sent you")
		fmt.Fprintln(os.Stderr, "  e.g. sentinel enroll https://sentinel.acme.internal/e/ott_...")
		os.Exit(2)
	}
	enrollURL := rest[0]

	configPath := *configFlag
	if configPath == "" {
		p, err := config.DefaultPath()
		if err != nil {
			fmt.Fprintf(os.Stderr, "default config path: %v\n", err)
			os.Exit(1)
		}
		configPath = p
	}

	// Exchange the OTT for a bearer token.
	resp, err := exchangeEnrollment(enrollURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ enroll failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "✓ exchanged enrollment token (agent: %s)\n", resp.AgentName)

	central := &onboard.CentralBlock{
		URL:       resp.CentralURL,
		Token:     resp.Token,
		AgentName: resp.AgentName,
	}

	rpt, err := onboard.Run(onboard.Options{
		AgentConfigPath: configPath,
		Central:         central,
		SentinelCmd:     sentinelBinary(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ wrap step failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "  Your bearer token is valid but config was not fully written.")
		fmt.Fprintln(os.Stderr, "  You can paste these into sentinel.yaml manually:")
		fmt.Fprintf(os.Stderr, "    central.url:        %s\n", resp.CentralURL)
		fmt.Fprintf(os.Stderr, "    central.token:      %s\n", resp.Token)
		fmt.Fprintf(os.Stderr, "    central.agent_name: %s\n", resp.AgentName)
		os.Exit(1)
	}
	printOnboardReport(rpt, *noWrap)
}

// runOnboardingWrap is the shared bottom half: do the wrap, print the report.
// central is nil for `init`, non-nil for `enroll`.
func runOnboardingWrap(agentConfigPath string, central *onboard.CentralBlock) {
	rpt, err := onboard.Run(onboard.Options{
		AgentConfigPath: agentConfigPath,
		Central:         central,
		SentinelCmd:     sentinelBinary(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		os.Exit(1)
	}
	printOnboardReport(rpt, false)
}

func printOnboardReport(rpt *onboard.Report, noWrap bool) {
	fmt.Fprintf(os.Stderr, "✓ wrote %s", rpt.AgentConfigPath)
	if rpt.AgentConfigBackup != "" {
		fmt.Fprintf(os.Stderr, " (backup: %s)", filepath.Base(rpt.AgentConfigBackup))
	}
	fmt.Fprintln(os.Stderr)

	if noWrap {
		fmt.Fprintln(os.Stderr, "(skipped AI client config rewrite per --no-wrap)")
		fmt.Fprintln(os.Stderr, "Edit your client config manually to point each server at:")
		fmt.Fprintln(os.Stderr, `    "command": "sentinel", "args": ["run", "--server", "<name>"]`)
		return
	}

	if rpt.NoClientsFound {
		fmt.Fprintln(os.Stderr, "ℹ no AI client config found (Claude Desktop / Cursor); skipping client rewrite")
		fmt.Fprintln(os.Stderr, "  If you use Cline (workspace-scoped) or another client, wrap their config manually.")
		return
	}

	anyMigrated := false
	for _, c := range rpt.Clients {
		switch {
		case len(c.ServersMigrated) > 0:
			anyMigrated = true
			fmt.Fprintf(os.Stderr, "✓ %s (%s) — migrated: %v",
				c.DisplayName, c.ConfigPath, c.ServersMigrated)
			if c.BackupPath != "" {
				fmt.Fprintf(os.Stderr, " (backup: %s)", filepath.Base(c.BackupPath))
			}
			fmt.Fprintln(os.Stderr)
			if len(c.ServersAlreadyWrapped) > 0 {
				fmt.Fprintf(os.Stderr, "  (already wrapped: %v)\n", c.ServersAlreadyWrapped)
			}
		case len(c.ServersAlreadyWrapped) > 0:
			fmt.Fprintf(os.Stderr, "✓ %s — already wrapped: %v\n",
				c.DisplayName, c.ServersAlreadyWrapped)
		default:
			fmt.Fprintf(os.Stderr, "ℹ %s (%s) had no MCP servers to migrate.\n",
				c.DisplayName, c.ConfigPath)
		}
	}
	if anyMigrated {
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Done. Restart your AI client(s) to begin reporting events.")
	}
}

// sentinelBinary returns the command name to write into the Claude config.
// We prefer the bare name "sentinel" so the config stays portable; if the
// binary isn't on PATH the user will hit a clear "sentinel not found"
// error at first launch, which is easier to diagnose than a stale
// absolute path that breaks after a Homebrew upgrade.
func sentinelBinary() string { return "sentinel" }

// centralPolicyCachePath returns where the central-policy disk cache
// lives. We pick a directory next to the agent's audit DB so a single
// $XDG_CONFIG_HOME / %APPDATA% setting moves both. If we can't figure
// it out, we return "" and the fetcher just skips on-disk caching.
func centralPolicyCachePath(auditFlag, configFlag string) string {
	// 1. Prefer the dir of an explicitly-flagged audit path.
	if auditFlag != "" {
		return filepath.Join(filepath.Dir(auditFlag), "central-policy.json")
	}
	// 2. Then the dir of the loaded config.
	if configFlag != "" {
		return filepath.Join(filepath.Dir(configFlag), "central-policy.json")
	}
	// 3. Default config dir (mirrors config.DefaultPath logic).
	if p, err := config.DefaultPath(); err == nil {
		return filepath.Join(filepath.Dir(p), "central-policy.json")
	}
	return ""
}

// enrollResponse mirrors the JSON returned by POST /e/{ott}.
type enrollResponse struct {
	AgentID    int64  `json:"agent_id"`
	AgentName  string `json:"agent_name"`
	Token      string `json:"token"`
	CentralURL string `json:"central_url"`
}

func exchangeEnrollment(enrollURL string) (*enrollResponse, error) {
	if enrollURL == "" {
		return nil, fmt.Errorf("empty URL")
	}
	parsed, err := url.Parse(enrollURL)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("URL must be http(s)")
	}
	req, err := http.NewRequest(http.MethodPost, enrollURL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
	}
	var out enrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if out.Token == "" || out.AgentName == "" {
		return nil, fmt.Errorf("server response missing token / agent_name")
	}
	return &out, nil
}

func runCmd(args []string) {
	// Inline mode is detected in either of two ways:
	//   1. Explicit "--" separator anywhere in args (slice 1/2 form).
	//   2. Any positional args left after flag parsing (real clients like
	//      the MCP Inspector pass args as a flat array without "--").
	dashIdx := -1
	for i, a := range args {
		if a == "--" {
			dashIdx = i
			break
		}
	}

	fs := flag.NewFlagSet("run", flag.ExitOnError)
	serverFlag := fs.String("server", "", "name of the configured upstream server to run")
	configFlag := fs.String("config", "", "path to sentinel.yaml (default: OS config dir)")
	auditFlag := fs.String("audit", "", "audit DB path (default: OS config dir)")

	var inlineArgs []string
	if dashIdx >= 0 {
		_ = fs.Parse(args[:dashIdx])
		inlineArgs = args[dashIdx+1:]
	} else {
		_ = fs.Parse(args)
		// Anything left over after flag parsing is the upstream command.
		inlineArgs = fs.Args()
	}

	var ups proxy.Upstream
	var policyEngine *policy.Engine
	approvalTimeout := 60 * time.Second
	var centralCfg config.CentralConfig
	var centralPolicyFetcher *centralpolicy.Fetcher
	switch {
	case len(inlineArgs) > 0:
		ups = proxy.Upstream{
			Name:    nonEmpty(*serverFlag, "inline"),
			Command: inlineArgs[0],
			Args:    inlineArgs[1:],
			// No env filtering in inline mode — caller is debugging.
		}
		// Inline mode still gets the default built-in rules.
		policyEngine = policy.NewEngine(nil)
	case *serverFlag != "":
		cfg, err := config.Load(*configFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load config: %v\n", err)
			os.Exit(1)
		}
		srv, err := cfg.Server(*serverFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		ups = proxy.Upstream{
			Name:    *serverFlag,
			Command: srv.Command,
			Args:    srv.Args,
			URL:     srv.URL,
			Headers: srv.Headers,
		}
		// Env filtering only applies to stdio upstreams.
		if srv.Command != "" {
			ups.Env = config.FilterEnv(os.Environ(), srv.Env)
		}
		// Audit path: CLI flag overrides config which overrides default.
		if *auditFlag == "" && cfg.Audit.Path != "" {
			*auditFlag = cfg.Audit.Path
		}
		centralCfg = cfg.Central

		// Fetch central policy (if configured) BEFORE building the engine
		// so the merged result snapshots into the engine at startup.
		// Failures here are non-fatal; the agent falls back to disk cache,
		// then to local-only policy.
		var centralEffective *centralpolicy.Effective
		if centralCfg.IsActive() {
			f, perr := centralpolicy.New(centralpolicy.Options{
				URL:       centralCfg.URL,
				Token:     centralCfg.Token,
				CachePath: centralPolicyCachePath(*auditFlag, *configFlag),
			})
			if perr != nil {
				fmt.Fprintf(os.Stderr, "central policy init: %v (continuing with local-only policy)\n", perr)
			} else {
				fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
				centralEffective = f.Refresh(fctx)
				fcancel()
				centralPolicyFetcher = f
			}
		}

		// Policy engine: enabled by default with built-ins + user deny paths,
		// optionally merged with central policy (slice 0.2.7).
		if cfg.Policy.Enabled == nil || *cfg.Policy.Enabled {
			denyPaths, approveT, blockT := centralpolicy.Merge(
				centralEffective,
				cfg.Policy.DenyPaths,
				cfg.Policy.Scoring.ApproveThreshold,
				cfg.Policy.Scoring.BlockThreshold,
			)
			policyEngine = policy.NewEngine(denyPaths)
			thresh := policy.DefaultThresholds()
			if approveT > 0 {
				thresh.ApproveThreshold = approveT
			}
			if blockT > 0 {
				thresh.BlockThreshold = blockT
			}
			policyEngine.WithThresholds(thresh)
		}
		if cfg.Policy.Scoring.ApprovalTimeoutSeconds > 0 {
			approvalTimeout = time.Duration(cfg.Policy.Scoring.ApprovalTimeoutSeconds) * time.Second
		}
	default:
		fmt.Fprintln(os.Stderr, "error: provide --server NAME or an inline upstream command")
		fmt.Fprintln(os.Stderr, "       e.g. sentinel run --server filesystem")
		fmt.Fprintln(os.Stderr, "            sentinel run npx -y @modelcontextprotocol/server-everything")
		fmt.Fprintln(os.Stderr, "            sentinel run -- npx -y @modelcontextprotocol/server-everything")
		os.Exit(2)
	}

	au, err := audit.New(*auditFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit init: %v\n", err)
		os.Exit(1)
	}
	defer au.Close()

	// Open the approval store on the same SQLite file the audit log uses.
	approvalPath := *auditFlag
	if approvalPath == "" {
		approvalPath, _ = audit.DefaultPath()
	}
	approvals, err := approval.Open(approvalPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "approval store init: %v\n", err)
		os.Exit(1)
	}
	defer approvals.Close()

	p, err := proxy.New(proxy.Options{
		Upstream:        ups,
		Audit:           au,
		Policy:          policyEngine,
		Approvals:       approvals,
		ApprovalTimeout: approvalTimeout,
	})

	// Start the telemetry pump if central is configured.
	if centralCfg.IsActive() {
		agentName := centralCfg.AgentName
		if agentName == "" {
			h, _ := os.Hostname()
			agentName = h
		}
		pump, perr := telemetry.New(telemetry.Options{
			URL:       centralCfg.URL,
			Token:     centralCfg.Token,
			AgentName: agentName,
			Audit:     au,
			Interval:  time.Duration(centralCfg.FlushIntervalSeconds) * time.Second,
			BatchSize: centralCfg.BatchSize,
		})
		if perr != nil {
			fmt.Fprintf(os.Stderr, "telemetry init: %v\n", perr)
			os.Exit(1)
		}
		// Verify connectivity before going live so misconfig surfaces fast.
		hctx, hcancel := context.WithTimeout(context.Background(), 5*time.Second)
		if herr := pump.CheckHealth(hctx); herr != nil {
			hcancel()
			fmt.Fprintf(os.Stderr, "telemetry: central health check failed: %v\n", herr)
			fmt.Fprintf(os.Stderr, "telemetry: the proxy will run, but events will not be shipped until central is reachable.\n")
		} else {
			hcancel()
		}
		// Run the pump alongside the proxy. We don't block on it.
		pumpCtx, pumpCancel := context.WithCancel(context.Background())
		go func() { _ = pump.Run(pumpCtx) }()
		defer pumpCancel()

		// Central-policy background refresh. Engine is snapshotted at
		// startup; this loop only keeps the on-disk cache fresh so next
		// startup picks up the latest. Dynamic engine reload lands in
		// slice 0.2.7.1.
		if centralPolicyFetcher != nil {
			go centralPolicyFetcher.Run(pumpCtx)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "proxy init: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	if err := p.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "proxy run: %v\n", err)
		os.Exit(1)
	}
}

func dashboardCmd(args []string) {
	fs := flag.NewFlagSet("dashboard", flag.ExitOnError)
	addr := fs.String("addr", dashboard.DefaultAddr, "listen address (host:port)")
	auditPath := fs.String("audit", "", "audit DB path (default: OS config dir)")
	_ = fs.Parse(args)

	srv, err := dashboard.New(dashboard.Options{
		Addr:      *addr,
		AuditPath: *auditPath,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "dashboard init: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "shutting down dashboard...")
		cancel()
	}()

	if err := srv.Run(ctx); err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "dashboard run: %v\n", err)
		os.Exit(1)
	}
}

func nonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
