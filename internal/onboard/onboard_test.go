package onboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeJSON helper for tests.
func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	buf, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Fresh laptop, no sentinel.yaml, Claude config with 2 servers
// ---------------------------------------------------------------------------

func TestRun_FreshLaptopMigratesAllServers(t *testing.T) {
	dir := t.TempDir()
	clientPath := filepath.Join(dir, "claude_desktop_config.json")
	agentPath := filepath.Join(dir, "sentinel.yaml")

	writeJSON(t, clientPath, map[string]any{
		"theme": "dark", // unrelated top-level key — should survive
		"mcpServers": map[string]any{
			"filesystem": map[string]any{
				"command": "npx",
				"args":    []string{"-y", "@modelcontextprotocol/server-filesystem", "/Users/me/projects"},
			},
			"github": map[string]any{
				"command": "npx",
				"args":    []string{"-y", "@modelcontextprotocol/server-github"},
				"env": map[string]string{
					"GITHUB_TOKEN": "ghp_xxx",
				},
			},
		},
	})

	rpt, err := Run(Options{
		ClientConfigPath: clientPath,
		AgentConfigPath:  agentPath,
		SentinelCmd:      "sentinel",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Both servers were migrated.
	want := []string{"filesystem", "github"}
	if got := rpt.ServersMigrated; !strSliceEq(got, want) {
		t.Errorf("ServersMigrated: want %v, got %v", want, got)
	}
	if rpt.ClaudeConfigBackup == "" {
		t.Errorf("expected backup of claude config")
	}

	// Claude config now points at sentinel for both servers AND preserves theme.
	var rewritten map[string]any
	if err := json.Unmarshal([]byte(readFile(t, clientPath)), &rewritten); err != nil {
		t.Fatal(err)
	}
	if rewritten["theme"] != "dark" {
		t.Errorf("unrelated key not preserved: %v", rewritten["theme"])
	}
	servers := rewritten["mcpServers"].(map[string]any)
	for _, name := range want {
		entry := servers[name].(map[string]any)
		if entry["command"] != "sentinel" {
			t.Errorf("server %q command: %v", name, entry["command"])
		}
		args := entry["args"].([]any)
		if len(args) != 3 || args[0] != "run" || args[1] != "--server" || args[2] != name {
			t.Errorf("server %q args: %v", name, args)
		}
	}

	// sentinel.yaml has both servers under servers: with the original commands.
	body := readFile(t, agentPath)
	for _, name := range want {
		if !strings.Contains(body, name+":") {
			t.Errorf("agent yaml missing %q: %s", name, body)
		}
	}
	if !strings.Contains(body, "npx") {
		t.Errorf("agent yaml missing the original command 'npx': %s", body)
	}
	if !strings.Contains(body, "/Users/me/projects") {
		t.Errorf("agent yaml missing filesystem path: %s", body)
	}
}

// ---------------------------------------------------------------------------
// Idempotency — running twice should produce no further migrations
// ---------------------------------------------------------------------------

func TestRun_TwiceIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	clientPath := filepath.Join(dir, "claude_desktop_config.json")
	agentPath := filepath.Join(dir, "sentinel.yaml")

	writeJSON(t, clientPath, map[string]any{
		"mcpServers": map[string]any{
			"filesystem": map[string]any{
				"command": "npx",
				"args":    []string{"-y", "@modelcontextprotocol/server-filesystem"},
			},
		},
	})

	first, err := Run(Options{
		ClientConfigPath: clientPath,
		AgentConfigPath:  agentPath,
		SentinelCmd:      "sentinel",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.ServersMigrated) != 1 {
		t.Fatalf("first run migrated %d", len(first.ServersMigrated))
	}

	second, err := Run(Options{
		ClientConfigPath: clientPath,
		AgentConfigPath:  agentPath,
		SentinelCmd:      "sentinel",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.ServersMigrated) != 0 {
		t.Errorf("second run should be no-op; migrated %v", second.ServersMigrated)
	}
	if len(second.ServersAlreadyWrapped) != 1 {
		t.Errorf("second run should report already-wrapped; got %v", second.ServersAlreadyWrapped)
	}
}

// ---------------------------------------------------------------------------
// Central block injection
// ---------------------------------------------------------------------------

func TestRun_WritesCentralBlock(t *testing.T) {
	dir := t.TempDir()
	clientPath := filepath.Join(dir, "claude_desktop_config.json")
	agentPath := filepath.Join(dir, "sentinel.yaml")
	writeJSON(t, clientPath, map[string]any{"mcpServers": map[string]any{}})

	_, err := Run(Options{
		ClientConfigPath: clientPath,
		AgentConfigPath:  agentPath,
		Central: &CentralBlock{
			URL:       "https://sentinel.acme.internal",
			Token:     "mcpg_test_token",
			AgentName: "alice-laptop",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := readFile(t, agentPath)
	if !strings.Contains(body, "central:") {
		t.Errorf("missing central block: %s", body)
	}
	if !strings.Contains(body, "sentinel.acme.internal") {
		t.Errorf("missing url: %s", body)
	}
	if !strings.Contains(body, "mcpg_test_token") {
		t.Errorf("missing token: %s", body)
	}
	if !strings.Contains(body, "alice-laptop") {
		t.Errorf("missing agent_name: %s", body)
	}
}

// ---------------------------------------------------------------------------
// No Claude config present — solo case, central only
// ---------------------------------------------------------------------------

func TestRun_NoClaudeConfigStillWritesAgentYaml(t *testing.T) {
	dir := t.TempDir()
	agentPath := filepath.Join(dir, "sentinel.yaml")

	rpt, err := Run(Options{
		ClientConfigPath: filepath.Join(dir, "does-not-exist.json"),
		AgentConfigPath:  agentPath,
		Central: &CentralBlock{
			URL:       "https://central.example",
			Token:     "mcpg_x",
			AgentName: "x",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rpt.ClaudeConfigMissing {
		t.Errorf("expected ClaudeConfigMissing")
	}
	if _, err := os.Stat(agentPath); err != nil {
		t.Errorf("agent yaml not written: %v", err)
	}
	body := readFile(t, agentPath)
	if !strings.Contains(body, "central:") {
		t.Errorf("expected central block: %s", body)
	}
}

// ---------------------------------------------------------------------------
// Preserves existing sentinel.yaml server entries
// ---------------------------------------------------------------------------

func TestRun_DoesNotOverwriteExistingAgentServer(t *testing.T) {
	dir := t.TempDir()
	clientPath := filepath.Join(dir, "claude_desktop_config.json")
	agentPath := filepath.Join(dir, "sentinel.yaml")

	// Pre-existing agent yaml with a customized filesystem server.
	if err := os.WriteFile(agentPath, []byte(`version: "1"
servers:
  filesystem:
    command: my-custom-filesystem-binary
    args: ["--readonly"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Claude has the same server name but a default npm-based entry.
	writeJSON(t, clientPath, map[string]any{
		"mcpServers": map[string]any{
			"filesystem": map[string]any{
				"command": "npx",
				"args":    []string{"-y", "@modelcontextprotocol/server-filesystem"},
			},
		},
	})

	_, err := Run(Options{
		ClientConfigPath: clientPath,
		AgentConfigPath:  agentPath,
		SentinelCmd:      "sentinel",
	})
	if err != nil {
		t.Fatal(err)
	}

	body := readFile(t, agentPath)
	if !strings.Contains(body, "my-custom-filesystem-binary") {
		t.Errorf("existing server config got overwritten: %s", body)
	}
	if strings.Contains(body, "npx") {
		t.Errorf("Claude's command leaked into existing entry: %s", body)
	}
	// Claude config should still have been rewritten to point at sentinel.
	if !strings.Contains(readFile(t, clientPath), `"sentinel"`) {
		t.Errorf("claude config not rewritten")
	}
}

// ---------------------------------------------------------------------------
// Dry-run leaves files untouched
// ---------------------------------------------------------------------------

func TestRun_DryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	clientPath := filepath.Join(dir, "claude_desktop_config.json")
	agentPath := filepath.Join(dir, "sentinel.yaml")

	original := map[string]any{
		"mcpServers": map[string]any{
			"filesystem": map[string]any{
				"command": "npx",
				"args":    []string{"-y", "fs-server"},
			},
		},
	}
	writeJSON(t, clientPath, original)

	rpt, err := Run(Options{
		ClientConfigPath: clientPath,
		AgentConfigPath:  agentPath,
		SentinelCmd:      "sentinel",
		DryRun:           true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rpt.ServersMigrated) != 1 {
		t.Errorf("dry-run report should still list planned migrations: %v", rpt.ServersMigrated)
	}
	if _, err := os.Stat(agentPath); !os.IsNotExist(err) {
		t.Errorf("dry-run created agent yaml: %v", err)
	}
	// Claude config still the original (unchanged).
	if !strings.Contains(readFile(t, clientPath), "fs-server") {
		t.Errorf("dry-run mutated client config")
	}
}

// ---------------------------------------------------------------------------
// Multi-client (slice 0.2.5.1)
// ---------------------------------------------------------------------------

func TestRun_MigratesMultipleClients(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "claude_desktop_config.json")
	cursorPath := filepath.Join(dir, "mcp.json") // simulated Cursor config
	agentPath := filepath.Join(dir, "sentinel.yaml")

	// Claude has filesystem; Cursor has github.
	writeJSON(t, claudePath, map[string]any{
		"theme": "dark",
		"mcpServers": map[string]any{
			"filesystem": map[string]any{
				"command": "npx",
				"args":    []string{"-y", "@modelcontextprotocol/server-filesystem", "/Users/me"},
			},
		},
	})
	writeJSON(t, cursorPath, map[string]any{
		"mcpServers": map[string]any{
			"github": map[string]any{
				"command": "npx",
				"args":    []string{"-y", "@modelcontextprotocol/server-github"},
			},
		},
	})

	rpt, err := Run(Options{
		ClientPaths:     []string{claudePath, cursorPath},
		AgentConfigPath: agentPath,
		SentinelCmd:     "sentinel",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rpt.Clients) != 2 {
		t.Fatalf("want 2 client reports, got %d", len(rpt.Clients))
	}

	// Each client should have wrapped its own server.
	byPath := map[string]onboard_clientReportLite{}
	for _, c := range rpt.Clients {
		byPath[c.ConfigPath] = onboard_clientReportLite{
			DisplayName:     c.DisplayName,
			ServersMigrated: c.ServersMigrated,
			BackupPath:      c.BackupPath,
		}
	}
	if got := byPath[claudePath]; !strSliceEq(got.ServersMigrated, []string{"filesystem"}) {
		t.Errorf("claude migrated: want [filesystem], got %v", got.ServersMigrated)
	}
	if got := byPath[cursorPath]; !strSliceEq(got.ServersMigrated, []string{"github"}) {
		t.Errorf("cursor migrated: want [github], got %v", got.ServersMigrated)
	}

	// Both files should have backups.
	if byPath[claudePath].BackupPath == "" {
		t.Errorf("claude config not backed up")
	}
	if byPath[cursorPath].BackupPath == "" {
		t.Errorf("cursor config not backed up")
	}

	// Both servers should be in sentinel.yaml.
	body := readFile(t, agentPath)
	for _, name := range []string{"filesystem", "github"} {
		if !strings.Contains(body, name+":") {
			t.Errorf("agent yaml missing %q: %s", name, body)
		}
	}

	// Both client configs should now launch through sentinel.
	for _, p := range []string{claudePath, cursorPath} {
		var raw map[string]any
		_ = json.Unmarshal([]byte(readFile(t, p)), &raw)
		servers := raw["mcpServers"].(map[string]any)
		for _, e := range servers {
			ent := e.(map[string]any)
			if ent["command"] != "sentinel" {
				t.Errorf("%s: command not wrapped: %v", p, ent["command"])
			}
		}
	}

	// Claude's theme key should survive the rewrite.
	var claude map[string]any
	_ = json.Unmarshal([]byte(readFile(t, claudePath)), &claude)
	if claude["theme"] != "dark" {
		t.Errorf("claude theme key lost: %v", claude["theme"])
	}
}

func TestRun_NoClientsFoundFlag(t *testing.T) {
	dir := t.TempDir()
	agentPath := filepath.Join(dir, "sentinel.yaml")

	rpt, err := Run(Options{
		ClientPaths:     []string{},
		ClientConfigPath: filepath.Join(dir, "does-not-exist.json"),
		AgentConfigPath: agentPath,
		// No Central — solo dev case where they have no clients yet.
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rpt.NoClientsFound {
		t.Errorf("expected NoClientsFound = true")
	}
	if _, err := os.Stat(agentPath); err != nil {
		t.Errorf("agent yaml still written: %v", err)
	}
}

func TestClientNameFromPath(t *testing.T) {
	cases := map[string]string{
		"/Users/me/Library/Application Support/Claude/claude_desktop_config.json": "Claude Desktop",
		"C:\\Users\\me\\AppData\\Roaming\\Claude\\claude_desktop_config.json":     "Claude Desktop",
		"/Users/me/.cursor/mcp.json":                                              "Cursor",
		"/Users/me/something/random.json":                                         "random.json",
	}
	for in, want := range cases {
		if got := clientNameFromPath(in); got != want {
			t.Errorf("clientNameFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}

type onboard_clientReportLite struct {
	DisplayName     string
	ServersMigrated []string
	BackupPath      string
}

func strSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
