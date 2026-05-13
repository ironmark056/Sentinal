# 15 — Onboarding (Individual & Company)

> Status: as of slice **0.2.5**, both individual setup and company employee enrollment are one-command operations. The detailed mechanism is in [docs/16-enrollment.md](16-enrollment.md). This page is the operator-facing guide that walks through both flows and includes the manual fallback for non-Claude-Desktop clients.

There are two stories: a solo developer setting Sentinel up on their own laptop with no server, and an employee being onboarded into a company-wide Sentinel deployment. Both share the same binary; the difference is whether a `central:` block lives in the config.

---

## Part A — Solo developer (no central server)

You want to see and gate the tool calls your AI agent makes on your own machine. ~30 seconds.

### 1. Install the binary

macOS / Linux:

```bash
curl -fsSL https://example.com/install.sh | sh
sentinel version
```

Windows (PowerShell):

```powershell
iwr https://example.com/install.ps1 -useb | iex
sentinel version
```

### 2. Run one command

```bash
sentinel init --wrap-claude
```

That's it. The command will:

- Write a starter `~/.sentinel/sentinel.yaml` (or `%APPDATA%\sentinel\sentinel.yaml` on Windows).
- Detect your Claude Desktop config.
- Transplant every MCP server entry into `sentinel.yaml` and rewrite the Claude config to launch each through `sentinel run`.
- Save `.bak.<timestamp>` of the original Claude config before touching it.

Sample output:

```
✓ wrote /Users/me/.sentinel/sentinel.yaml
✓ /Users/me/Library/Application Support/Claude/claude_desktop_config.json
  — migrated: [filesystem github] (backup: claude_desktop_config.json.bak.2026-05-14T19-30-21Z)

Done. Restart Claude Desktop to begin reporting events.
```

### 3. Open the dashboard

```bash
sentinel dashboard
# → http://127.0.0.1:7842
```

Leave it running in a terminal. Every tool call your agent makes now shows up live.

### Solo manual fallback (Cline / other clients)

Cursor is auto-detected as of slice 0.2.5.1 — running `sentinel init --wrap-claude` (or `sentinel enroll`) will migrate both Claude Desktop and Cursor configs in one pass, with a separate `.bak` for each. The flag name remains `--wrap-claude` for back-compat; it really means "wrap every detected client."

For Cline (workspace-scoped) or other clients we don't auto-detect, the manual path still works:

| Client | Config path | Notes |
|---|---|---|
| Cline | inside `.vscode/settings.json` under `cline.mcpServers` | Per-workspace; you edit each project's settings. |

Open the config file. Each entry like:

```json
"filesystem": {
  "command": "npx",
  "args": ["-y", "@modelcontextprotocol/server-filesystem", "/Users/me/projects"]
}
```

should be edited to:

```json
"filesystem": {
  "command": "sentinel",
  "args": ["run", "--server", "filesystem"]
}
```

And the original `command`/`args` go into `~/.sentinel/sentinel.yaml`:

```yaml
servers:
  filesystem:
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/Users/me/projects"]
```

---

## Part B — Employee being onboarded to a company deployment

Your IT admin sends you **one URL**. You paste **one command**. Done in 30 seconds.

### What the admin sends you

A single command, like:

```
sentinel enroll https://sentinel.acme.internal/e/ott_5f4a1d8c9e2b...
```

That URL is single-use and expires in 24 hours. There is no bearer token to handle manually; the URL exchanges for one when you run the command.

### 1. Install the binary

macOS / Linux:

```bash
curl -fsSL https://example.com/install.sh | sh
```

Windows:

```powershell
iwr https://example.com/install.ps1 -useb | iex
```

### 2. Run the command your admin sent

```
sentinel enroll https://sentinel.acme.internal/e/ott_5f4a1d8c9e2b...
```

Sample output:

```
✓ exchanged enrollment token (agent: alice-laptop)
✓ wrote /Users/alice/.sentinel/sentinel.yaml
✓ /Users/alice/Library/Application Support/Claude/claude_desktop_config.json
  — migrated: [filesystem github] (backup: claude_desktop_config.json.bak.2026-05-14T19-30-21Z)

Done. Restart Claude Desktop to begin reporting events.
```

The command does all of:

- Contacts the central server, exchanges the URL for a bearer token.
- Writes your `sentinel.yaml` with the `central:` block and your existing MCP servers.
- Detects Claude Desktop's config and rewrites each server entry to launch through `sentinel run`.
- Saves a timestamped `.bak` of anything it overwrites.

### 3. Restart Claude Desktop

The next tool call your agent makes streams up to the central server. You appear on the company dashboard under your agent name, and your "Last Seen" flips to "just now."

### Troubleshooting

- **`server returned 409: already used`** — your admin's link was already redeemed. Ask for a new one.
- **`server returned 410: enrollment token expired`** — past 24 hours. Ask for a new one.
- **`server returned 409: agent name already exists`** — you (or a previous laptop with the same name) is already registered. Ask the admin to revoke the old one or pick a different name.
- **Don't use Claude Desktop?** Run with `--no-wrap`. The bearer token and `sentinel.yaml` are written; you edit your client config (Cursor / Cline) manually.

### Manual fallback (rare cases)

If you must paste credentials by hand (no central enrollment available, restricted network), the admin can fall back to `sentinel-server agent create <name>` which prints a bare bearer token. The manual `sentinel.yaml` looks like:

```yaml
central:
  url: https://sentinel.acme.internal
  token: mcpg_5f4a1d8c9e2b...
  agent_name: alice-laptop

servers:
  filesystem:
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/Users/alice/projects"]
```

Plus the same client-config edit as Part A's manual fallback.

---

## Part C — Company owner / admin setup

What you do **once** to stand up the server. ~15 minutes.

### 1. Pick a host

Any small Linux VM inside the network. Needs to be reachable from every employee laptop (corporate VPN, Tailscale, internal DNS — whatever you already use).

### 2. Install and start

```bash
curl -fsSL https://example.com/install.sh | sh

# Strong admin token gates write endpoints (add/revoke agents):
export SENTINEL_ADMIN_TOKEN="$(openssl rand -hex 32)"

sentinel-server serve --addr 127.0.0.1:7843 --data /var/lib/sentinel
```

### 3. TLS in front

`sentinel-server` itself speaks plain HTTP. Put **nginx** or **Caddy** in front of it, terminating TLS on 443 and forwarding to `127.0.0.1:7843`. Standard reverse-proxy work, nothing Sentinel-specific.

Caddy example:

```
sentinel.acme.internal {
    reverse_proxy 127.0.0.1:7843
}
```

### 4. Enroll each employee (the easy way)

For each laptop you want to onboard:

```bash
sentinel-server enroll create alice-laptop --meta owner=alice,team=eng \
  --base-url https://sentinel.acme.internal
```

The output:

```
Enrollment created: id=12 name=alice-laptop
Expires:            2026-05-15 19:30:00

Send the employee this single command:

  sentinel enroll https://sentinel.acme.internal/e/ott_5f4a1d8c9e2b...
```

Send that **one line** to Alice. She runs it, she's done. No bearer-token sharing, no YAML editing on her side.

The OTT is single-use and expires in 24h (override with `--ttl`). If something goes wrong before she redeems it, `sentinel-server enroll revoke <id>` invalidates it; if she missed the window, just create a new one.

### Legacy / fallback: bare bearer tokens

For cases where the employee can't reach the server during onboarding (air-gapped, restricted contractor laptops), the older flow still works:

```bash
sentinel-server agent create alice-laptop --meta owner=alice,team=eng
```

Prints the bearer token **once**. Send it via 1Password / Signal — never Slack DM in plaintext — along with the manual Part B fallback instructions.

### 5. Open the dashboard

`https://sentinel.acme.internal/` — empty Sessions, empty Activity, two agents in the Agents table both showing `(never)` in Last Seen. As soon as Alice or Bob does anything in Claude, their row's Last Seen flips and events start appearing.

---

## Common policy recipes

The `policy:` block in `sentinel.yaml` shapes how Sentinel handles risky calls. Drop these into either an individual's or an employee's config.

### Recipe 1 — Block destructive filesystem operations outright

```yaml
policy:
  enabled: true
  denylist:
    - "filesystem/delete_file"
    - "filesystem/delete_directory"
    - "filesystem/move_file"            # if it leaves the allowed root
```

Anything in `denylist` gets a fail-closed response without ever reaching the underlying MCP server. The error shows up as a `BLOCKED` row in the dashboard.

### Recipe 2 — "Block writes outside the project dir unless I approve"

```yaml
policy:
  enabled: true
  scoring:
    enabled: true
    approval_timeout_seconds: 120
  rules:
    # Anything path-y outside ~/projects raises the risk score.
    - id: writes/outside-project
      match:
        method: ["filesystem/write_file", "filesystem/create_directory"]
        param_jsonpath: "$.path"
        param_not_starts_with: "/Users/me/projects/"
      score: +60
      action: require_approval

    # tools/call to shell at all → require approval, even inside the project.
    - id: shell/any
      match:
        method: ["shell/exec"]
      score: +50
      action: require_approval
```

When the score crosses the approval threshold, the request **pauses** at the proxy, the dashboard's "Pending approvals" section shows it with the matched findings, and the agent waits up to `approval_timeout_seconds` for you to click Approve or Deny. If you tick "Remember this decision" the dashboard records an auto-decision for that `category/rule` pair so future calls that match the same rule don't ask again.

### Recipe 3 — Strip secret-looking env vars before forwarding

```yaml
defaults:
  env_strip:
    - "AWS_SECRET_ACCESS_KEY"
    - "OPENAI_API_KEY"
    - "ANTHROPIC_API_KEY"
    - "GITHUB_TOKEN"
```

`env_strip` applies to **stdio** upstreams only (HTTP upstreams don't inherit a subprocess environment). Each listed name is removed from the environment passed to the MCP server. Use this when you don't fully trust the server but still want to run it locally.

### Recipe 4 — Multiple servers, central shipping, mixed policy

```yaml
central:
  url: https://sentinel.acme.internal
  token: mcpg_5f4a1d8c...
  agent_name: alice-laptop

policy:
  enabled: true
  denylist:
    - "filesystem/delete_directory"
  rules:
    - id: shell/any
      match: { method: ["shell/exec"] }
      score: +50
      action: require_approval

servers:
  filesystem:
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/Users/alice/projects"]
  github:
    command: ["npx", "-y", "@modelcontextprotocol/server-github"]
    env_strip: ["GITHUB_TOKEN"]              # forward via sentinel's own creds instead
  internal-api:
    url: https://mcp.acme.internal/v1
    headers:
      Authorization: "Bearer ${INTERNAL_API_TOKEN}"
```

---

## Where things go on disk

| File / dir | What | Who reads it |
|---|---|---|
| `~/.sentinel/sentinel.yaml` | Agent config | `sentinel run` |
| `~/.sentinel/audit.db` | Local audit log (SQLite) | `sentinel dashboard`, telemetry pump |
| `/var/lib/sentinel/sentinel-server.db` | Central DB (agents + fleet events) | `sentinel-server serve` |

Back up the central DB the same way you back up any SQLite-based service — it's a single file plus a WAL sidecar.

---

## Coming next

The current friction points and where they'll land:

- **Auto-migrating Cursor / Cline configs.** Today `--wrap-claude` only handles Claude Desktop. Cursor + Cline follow in 0.2.5.1.
- **Centrally-pushed policy.** Today each laptop owns its own denylist / approval rules; central just collects. Policy distribution lands in 0.2.7.
- **`+ Enroll` button in the dashboard.** Today the admin uses the CLI to mint enrollment URLs. A button on the agents page is ~30 lines; 0.2.5.1.
- **Slack / webhook alerts** on policy blocks. 0.2.6.

For the technical details of the enrollment flow (wire shapes, security model, error codes), see [docs/16-enrollment.md](16-enrollment.md).
