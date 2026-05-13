<p align="center">
  <img src="sentinel-banner.png" alt="Sentinel Banner" width="100%"/>
</p>

# Sentinel

**Zero-trust runtime security for Model Context Protocol (MCP) servers.**

A single Go binary that sits between your AI client and your MCP servers. It logs every tool call, blocks the obvious attacks, asks for your approval on the high-risk ones, and gives you a local dashboard to see what your agents actually did.

```
┌─────────────────┐      ┌──────────────┐      ┌──────────────────┐
│   AI Client     │ ───▶ │   Sentinel   │ ───▶ │   MCP Server(s)  │
│ Claude / Cursor │      │    Proxy     │      │  fs / github /   │
│ Cline / web app │ ◀─── │              │ ◀─── │  postgres / ...  │
└─────────────────┘      └──────┬───────┘      └──────────────────┘
                                │
                                ▼
                         ┌──────────────┐
                         │  Audit DB +  │
                         │  Dashboard   │
                         └──────────────┘
```

No accounts. No cloud. No third-party APIs. Drop in, point your AI client at `sentinel` instead of the underlying server, get visibility and policy enforcement for free.

For teams, the same binary plus a companion `sentinel-server` gives you a self-hosted fleet view, central policy distribution, and one-command employee onboarding — all running on hardware you own. See [`docs/18-use-cases-and-benefits.md`](docs/18-use-cases-and-benefits.md) for the diagrammatic overview.

---

## What it stops, by default

| Attack | Caught by |
|---|---|
| Reading `~/.ssh/`, `~/.aws/credentials`, `/etc/passwd`, browser cookies, shell history | `sensitive-path/*` rules |
| `../../etc` and URL-encoded `%2e%2e` variants | `path-traversal/*` rules |
| `rm -rf`, `dd if=`, `curl … \| sh`, fork bombs | `command-injection/*` rules |
| Hidden Unicode tag chars (`U+E0000`–`U+E007F`), RTL overrides, zero-width runs | `unicode-smuggling/*` rules |
| "Ignore all previous instructions", "reveal system prompt", "DAN mode" | `prompt-injection/*` rules |
| Outbound `AKIA…` / `ghp_…` / `sk-…` / `xox…` tokens in arguments | `secret-like/*` rules |

A 0–100 risk score is computed from the findings. Score `< 30` → allow silently. Score `30–79` → suspend the call and ask the human via the dashboard. Score `≥ 80` → block with a JSON-RPC error sent back to the client. The thresholds are configurable.

---

## Quick start — solo developer

```bash
# 1. Get the binary (see "Install" below for downloads).
# 2. One command — auto-detects Claude Desktop and Cursor configs,
#    transplants their MCP servers into sentinel.yaml, rewrites the
#    client configs to launch through sentinel, saves backups.
sentinel init --wrap-claude

# 3. Restart Claude Desktop / Cursor. Then watch what they do:
sentinel dashboard
# → http://127.0.0.1:7842
```

For testing without Claude Desktop, the MCP Inspector works equally well — see [`TESTING.md`](TESTING.md).

If you prefer the manual flow (you don't use Claude Desktop / Cursor, or you want to hand-edit the config), drop `--wrap-claude` and follow [`docs/15-onboarding.md`](docs/15-onboarding.md).

## Quick start — company / team

```bash
# On a server inside your network (one binary, no SaaS):
export SENTINEL_ADMIN_TOKEN="$(openssl rand -hex 32)"
sentinel-server serve --addr 0.0.0.0:7843 --data /var/lib/sentinel
# Put nginx / Caddy in front for TLS.

# For each employee, generate a single-use enrollment URL:
sentinel-server enroll create alice-laptop \
  --base-url https://sentinel.acme.internal
# → prints: "sentinel enroll https://sentinel.acme.internal/e/ott_..."

# Alice runs that one command on her laptop. Done.
# Visit the dashboard for fleet-wide visibility:
# → https://sentinel.acme.internal/
```

Full fleet walkthrough: [`docs/15-onboarding.md`](docs/15-onboarding.md). Enrollment design: [`docs/16-enrollment.md`](docs/16-enrollment.md). Centrally-managed policy: [`docs/17-central-policy.md`](docs/17-central-policy.md).

---

## Install

### Pre-built binaries

Download from the [Releases](https://github.com/your-org/sentinel/releases) page. The release publishes two binaries: `sentinel` (the agent / laptop binary) and `sentinel-server` (the optional fleet binary). Put the one(s) you need on `PATH`.

| OS | Architecture | Agent binary | Fleet binary |
|---|---|---|---|
| Linux | x86_64 | `sentinel-linux-amd64` | `sentinel-server-linux-amd64` |
| Linux | ARM64 | `sentinel-linux-arm64` | `sentinel-server-linux-arm64` |
| macOS | Intel | `sentinel-darwin-amd64` | `sentinel-server-darwin-amd64` |
| macOS | Apple Silicon | `sentinel-darwin-arm64` | `sentinel-server-darwin-arm64` |
| Windows | x86_64 | `sentinel-windows-amd64.exe` | `sentinel-server-windows-amd64.exe` |
| Windows | ARM64 | `sentinel-windows-arm64.exe` | `sentinel-server-windows-arm64.exe` |

### Build from source

Requires Go 1.25 or newer.

```bash
git clone https://github.com/your-org/sentinel
cd sentinel
go build -o bin/sentinel ./cmd/sentinel
go build -o bin/sentinel-server ./cmd/sentinel-server
# or, for every supported OS/arch at once:
./scripts/build-release.sh   # Unix
./scripts/build-release.ps1  # Windows
```

Each result is a single static binary (~11 MB, no CGO, no runtime deps).

---

## Commands

### `sentinel` (the agent / laptop binary)

```
sentinel init [--path PATH] [--force] [--wrap-claude]
    Write a starter sentinel.yaml. With --wrap-claude, also auto-detect
    the local Claude Desktop and Cursor configs and rewrite each MCP
    server entry to launch through sentinel. Timestamped .bak saved.

sentinel enroll <enrollment-url>
    Onboard this machine into a company Sentinel deployment in one
    command. Exchanges the one-time URL for a bearer token, writes
    sentinel.yaml with the central: block, and (unless --no-wrap)
    auto-wraps detected AI client configs.

sentinel run --server NAME [--config PATH] [--audit PATH]
    Run a configured upstream server.

sentinel run <upstream-command> [args...]
    Run an upstream by inline command, bypassing the config file.

sentinel dashboard [--addr ADDR] [--audit PATH]
    Open the local dashboard (default http://127.0.0.1:7842).

sentinel version
sentinel help
```

### `sentinel-server` (the optional fleet binary)

```
sentinel-server serve [--addr ADDR] [--data DIR]
    Run the HTTP service. Default addr: 0.0.0.0:7843.
    Admin token: env SENTINEL_ADMIN_TOKEN (gates write endpoints).

sentinel-server enroll create <name> [--meta k=v,...] [--ttl 24h]
                                     [--base-url URL]
    Generate a single-use enrollment URL for an employee laptop.

sentinel-server enroll list [--json]
sentinel-server enroll revoke <id>

sentinel-server agent create <name>     # legacy bare-token issuance
sentinel-server agent list
sentinel-server agent delete <id>

sentinel-server version
sentinel-server help
```

---

## Configuration

Full schema in [`docs/04-config-schema.md`](docs/04-config-schema.md). The short version:

```yaml
version: "1"

audit:
  # path: defaults to ~/.sentinel/audit.db (or %APPDATA%\sentinel\audit.db)

defaults:
  env:
    allow_system: true   # PATH, HOME, USERPROFILE, etc. pass through
    # Anything else (GITHUB_TOKEN, AWS_*, etc.) is stripped by default.

policy:
  # Built-in sensitive-path rules always apply. Add your own substring or
  # glob patterns here.
  deny_paths:
    - ~/Documents/work-secrets
    - ~/projects/**/secrets.yml
  scoring:
    approve_threshold: 30
    block_threshold: 80
    approval_timeout_seconds: 60

servers:
  # stdio (local subprocess)
  filesystem:
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "~/Documents"]

  # stdio with an explicit secret allowed through
  github:
    command: npx
    args: ["-y", "@modelcontextprotocol/server-github"]
    env:
      allow: [GITHUB_TOKEN]

  # HTTP (remote server, Streamable HTTP transport)
  remote_search:
    url: https://api.example.com/mcp
    headers:
      Authorization: "Bearer eyJ..."
```

Exactly one of `command:` or `url:` per server.

---

## How it works

Ten Go packages:

| Package | What it does |
|---|---|
| `internal/proxy` | The proxy itself. stdio + HTTP transport, JSON-RPC framing, audit logging, policy invocation, approval wait. |
| `internal/policy` | Pattern detection (regex + heuristics, no ML), risk scoring, three-way decision. |
| `internal/approval` | SQLite-backed approval queue and auto-decisions. |
| `internal/audit` | Append-only audit log with buffered writes. |
| `internal/config` | YAML parser + env filtering. |
| `internal/dashboard` | Embedded HTTP server + vanilla-JS SPA for the local laptop UI. |
| `internal/server` | The self-hosted fleet central server: storage, HTTP API, multi-agent SPA. |
| `internal/telemetry` | Agent-side pump that ships local audit events to the central server. |
| `internal/centralpolicy` | Agent-side fetcher + stricter-wins merge for company-distributed policy. |
| `internal/onboard` | Cross-platform AI-client config detection + safe rewrite (for `init --wrap-claude` and `enroll`). |

Deeper dives:

**Start here:**
- [`docs/18-use-cases-and-benefits.md`](docs/18-use-cases-and-benefits.md) — diagrammatic overview, who uses it, where it helps

**Foundations (v0.1):**
- [`docs/00-vision.md`](docs/00-vision.md) — what this is for and why
- [`docs/01-architecture.md`](docs/01-architecture.md) — system shape
- [`docs/02-mcp-protocol.md`](docs/02-mcp-protocol.md) — protocol surface we sit on
- [`docs/03-proxy-design.md`](docs/03-proxy-design.md) — proxy internals (stdio + HTTP)
- [`docs/04-config-schema.md`](docs/04-config-schema.md) — `sentinel.yaml` reference
- [`docs/05-pattern-detection.md`](docs/05-pattern-detection.md) — every built-in rule, by category
- [`docs/06-risk-scoring.md`](docs/06-risk-scoring.md) — how the score is computed
- [`docs/07-allowlist-denylist.md`](docs/07-allowlist-denylist.md) — `deny_paths` and env policy
- [`docs/08-approval-flow.md`](docs/08-approval-flow.md) — the approval queue + auto-decisions
- [`docs/10-dashboard.md`](docs/10-dashboard.md) — local UI

**Fleet (v0.2):**
- [`docs/11-central-server.md`](docs/11-central-server.md) — `sentinel-server` design
- [`docs/12-central-dashboard.md`](docs/12-central-dashboard.md) — fleet dashboard SPA
- [`docs/13-sessions.md`](docs/13-sessions.md) — per-session grouping
- [`docs/14-payload-search.md`](docs/14-payload-search.md) — `q=` search across the fleet
- [`docs/15-onboarding.md`](docs/15-onboarding.md) — individual + company onboarding playbook
- [`docs/16-enrollment.md`](docs/16-enrollment.md) — zero-touch enrollment design
- [`docs/17-central-policy.md`](docs/17-central-policy.md) — centrally-distributed policy, stricter-wins merge

---

## Status

**v0.2.7** (pending tag). Single static binary, no CGO. 153 tests across 10 internal packages, all green on Windows / macOS / Linux.

Shipped:

- v0.1 — local proxy, audit log, denylist, risk scoring, approval gate, local dashboard, stdio + HTTP transport.
- v0.2 — self-hosted fleet server (`sentinel-server`), multi-agent dashboard, sessions view, payload search, zero-touch enrollment with single-use URLs, auto-detect Claude Desktop + Cursor configs, centrally-distributed policy with stricter-wins merge.

Not yet:

- Slack / webhook alerts on policy blocks (v0.2.6).
- Dynamic policy reload without restart (v0.2.7.1).
- Per-agent / per-group policies (v0.2.8).
- ML-based behavioral anomaly detection (v0.3, trained on telemetry collected via v0.2's central server).
- SSO, RBAC, on-prem hardened distribution, sandboxed execution (v1.0).

See [`CHANGELOG.md`](CHANGELOG.md) for the running list.

---

## Testing it yourself

[`TESTING.md`](TESTING.md) walks through wiring `sentinel` between MCP Inspector and the official `@modelcontextprotocol/server-everything` reference server, firing both benign and attack tool calls, and inspecting the audit log. About 10 minutes.

---

## Security & threat model

Sentinel sits **at the protocol layer**, in the path of every tool call. That is the position. What it is good for:

- Catching protocol-shaped attacks: path traversal, command injection, secret-shaped exfiltration, sensitive-path access, prompt-injection phrases inside arguments.
- Making the implicit explicit: every tool call your agent makes is now visible, logged, and rule-checked.
- Giving you a kill switch: a human approval gate for risky calls.

What it is **not** good for:

- Defending against a malicious MCP server that simply lies about what it does in its tool schemas. (Sandboxing the upstream is a v1.0 enterprise concern.)
- Stopping attacks that don't appear at the JSON-RPC layer (in-process exploits, network attacks on the upstream itself, social engineering through tool *output*).
- Catching every prompt-injection phrase ever written — the regex rules catch the lazy 80%; the ML rules in v0.3 will handle the rest.

This is **defense in depth, not defense in one**.

---

## Contributing

Bug reports and small PRs welcome. For anything larger, open a discussion first so we can agree on shape.

Run the test suite:

```bash
go test ./...
```

Build for every supported platform:

```bash
./scripts/build-release.sh   # or build-release.ps1 on Windows
```

---

## License

Apache 2.0 — see [`LICENSE`](LICENSE). Commercial use is fine, modifications are fine, redistribution is fine. The future cloud / managed tier (v0.2+) will be source-available under a different license; the proxy core in this repo is and will remain Apache 2.0.
