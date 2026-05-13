# 01 — Architecture

## High-level diagram

```
┌────────────────────────────────────────────────────────────────────────┐
│                              AI Client                                 │
│              (Claude Desktop, Cursor, Cline, Zed, custom)              │
└────────────────────────────────┬───────────────────────────────────────┘
                                 │ MCP (JSON-RPC over stdio or HTTP/SSE)
                                 ▼
┌────────────────────────────────────────────────────────────────────────┐
│                            Sentinel Proxy                              │
│  ┌──────────────────────────────────────────────────────────────────┐  │
│  │                       Transport Layer                            │  │
│  │      stdio listener   │   HTTP/SSE listener   │   future...      │  │
│  └──────────────────────────┬───────────────────────────────────────┘  │
│                             ▼                                          │
│  ┌──────────────────────────────────────────────────────────────────┐  │
│  │                      JSON-RPC Router                             │  │
│  │   parses MCP messages, identifies tool calls, dispatches         │  │
│  └──────────────────────────┬───────────────────────────────────────┘  │
│                             ▼                                          │
│  ┌──────────────────────────────────────────────────────────────────┐  │
│  │                    Security Pipeline                             │  │
│  │  ┌────────┐  ┌─────────┐  ┌─────────┐  ┌─────────┐  ┌─────────┐  │  │
│  │  │ Schema │─►│ Pattern │─►│ Allow/  │─►│  Risk   │─►│Approval │  │  │
│  │  │ Valid. │  │ Detect. │  │ Denylist│  │  Score  │  │  Gate   │  │  │
│  │  └────────┘  └─────────┘  └─────────┘  └─────────┘  └─────────┘  │  │
│  └──────────────────────────┬───────────────────────────────────────┘  │
│                             ▼                                          │
│  ┌──────────────────────────────────────────────────────────────────┐  │
│  │                  Upstream Server Manager                         │  │
│  │   process supervision, env stripping, transport multiplexing     │  │
│  └──────────────────────────┬───────────────────────────────────────┘  │
│                             ▼                                          │
│  ┌──────────────────────────────────────────────────────────────────┐  │
│  │              Audit Log + Telemetry Pipeline                      │  │
│  │   SQLite (local)  │  Optional opt-in cloud sink (v0.2+)          │  │
│  └──────────────────────────────────────────────────────────────────┘  │
└────────────────────────────────┬───────────────────────────────────────┘
                                 │ MCP (unchanged wire format)
                                 ▼
┌────────────────────────────────────────────────────────────────────────┐
│                      Upstream MCP Servers                              │
│   filesystem  │  github  │  postgres  │  slack  │  shell  │  custom   │
└────────────────────────────────────────────────────────────────────────┘
                                 ▲
                                 │ HTTP (local only)
                                 │
┌────────────────────────────────┴───────────────────────────────────────┐
│                  Local Dashboard (localhost:7842)                      │
│   tool call timeline │ risk-scored events │ approval queue │ config    │
└────────────────────────────────────────────────────────────────────────┘
```

## Component responsibilities

### Transport Layer

The proxy must speak two MCP transports:

- **stdio** — the dominant transport. Used by Claude Desktop, Cursor, Cline, and most local MCP servers. The proxy registers itself as a stdio server to the client (reading from stdin, writing to stdout) and as a stdio client to each upstream (spawning the upstream as a subprocess and communicating over its stdin/stdout).
- **HTTP/SSE** (and the newer **Streamable HTTP**) — used by remote MCP servers and increasingly by cloud-deployed servers. The proxy exposes an HTTP endpoint and forwards to the upstream HTTP endpoint.

The transport layer is intentionally a thin shim. Once a message is decoded into a JSON-RPC envelope, the rest of the pipeline is transport-agnostic.

See [[03-proxy-design]] for the precise wire protocol handling.

### JSON-RPC Router

Every MCP message is a JSON-RPC 2.0 envelope. The router:

1. Validates the envelope structure (`jsonrpc: "2.0"`, `method`, `id` or `params`).
2. Categorizes the message: lifecycle (`initialize`, `initialized`, `ping`), discovery (`tools/list`, `resources/list`, `prompts/list`), or execution (`tools/call`, `resources/read`, `prompts/get`).
3. Routes execution messages through the security pipeline. Lifecycle and discovery messages bypass the security pipeline but are still logged.

The router is also responsible for tracking request/response correlation by `id`, since JSON-RPC is bidirectional and the proxy must match responses to the requests it forwarded.

### Security Pipeline

Five stages, in strict order:

1. **Schema validation** — does the tool call match the schema the upstream advertised? Reject malformed calls before they reach the upstream.
2. **Pattern detection** — run the tool name and all arguments through the regex/heuristic library (see [[05-pattern-detection]]). Tag any matches.
3. **Allowlist / denylist** — check tool name, argument values, filesystem paths, network destinations against the user's policy (see [[07-allowlist-denylist]]).
4. **Risk scoring** — combine signals from the previous stages into a numeric risk score (see [[06-risk-scoring]]).
5. **Approval gate** — if risk exceeds the configured threshold, suspend the call and route it to the approval queue (see [[08-approval-flow]]).

A call that fails any stage is either blocked (reply with a JSON-RPC error) or held pending approval. A call that passes all stages is forwarded to the upstream.

### Upstream Server Manager

For each configured MCP server, the manager:

- Spawns the upstream subprocess (stdio) or maintains an HTTP client (HTTP/SSE).
- Strips disallowed environment variables before spawn (e.g. `AWS_SECRET_ACCESS_KEY`, `GITHUB_TOKEN`) unless the server is explicitly granted them.
- Supervises the subprocess: restarts on crash with exponential backoff, kills on shutdown, reports health to the dashboard.
- Multiplexes requests if the client opens multiple MCP sessions to the same upstream.

### Audit Log + Telemetry Pipeline

Every request and response — passed, blocked, approved, denied — is written to a local SQLite database (`~/.sentinel/audit.db` on macOS/Linux, `%APPDATA%\sentinel\audit.db` on Windows).

The schema (full version in [[09-telemetry-pipeline]]) captures:

- timestamp, session id, upstream server name
- tool name, arguments (with secrets redacted)
- detection results, risk score, decision (pass/block/approve/deny)
- response status, latency, response size
- approval metadata if applicable

In v0.2 an optional cloud sink can be configured. The local DB is always the source of truth; the cloud is downstream.

### Local Dashboard

A small embedded HTTP server on `localhost:7842` serving a single-page app that reads from the audit DB. Purpose:

- **Visibility first.** The thing a v0.1 user actually wants is "show me what my agents did today."
- **Approval queue.** Pending high-risk calls appear here for human review.
- **Config inspection.** Show the active config, which servers are running, which detections are armed.
- **No write endpoints in v0.1.** The dashboard is read-only and approval-only. Config edits happen by editing `sentinel.yaml` and restarting the proxy. This is deliberate — write endpoints invite a class of bugs we do not want in the security-critical path.

## Data flow for a single tool call

```
1.  Client → Proxy:  tools/call { name: "filesystem.read", path: "~/.ssh/id_rsa" }
2.  Proxy: log request received (audit row, status=received)
3.  Proxy: schema validate against advertised filesystem.read schema → pass
4.  Proxy: pattern scan → match on "~/.ssh/" (sensitive path)
5.  Proxy: denylist check → ~/.ssh is in default denied paths → BLOCK
6.  Proxy: write audit row (status=blocked, reason=denylist:~/.ssh)
7.  Proxy → Client:  JSON-RPC error -32000 "blocked by sentinel policy"
8.  Dashboard: real-time update via SSE shows the blocked event
```

For a passing call:

```
1.  Client → Proxy:  tools/call { name: "github.list_repos" }
2.  Proxy: schema validate → pass
3.  Proxy: pattern scan → no matches
4.  Proxy: allow/denylist → tool allowed, no path/network constraints triggered
5.  Proxy: risk score → 5 (below threshold)
6.  Proxy: log request forwarded
7.  Proxy → Upstream(github):  forwards original message
8.  Upstream → Proxy:  result payload
9.  Proxy: log response (status=ok, latency=234ms)
10. Proxy → Client:  forwards original response
```

## Process model

Sentinel runs as a single long-lived process. Inside that process:

- **Main goroutine** — supervises subsystems, handles signals.
- **Per-client-session goroutine** — owns the stdin/stdout pair for one AI client connection. There is typically only one (Claude Desktop) but the architecture allows many.
- **Per-upstream-server goroutine** — owns the lifecycle of one upstream MCP server subprocess.
- **Dashboard HTTP goroutine** — serves the local web UI.
- **Audit writer goroutine** — buffered writes to SQLite to keep the hot path fast.

Concurrency is bounded and predictable. There is no shared mutable state on the request path — every request is processed by a single goroutine end to end, with the security pipeline as a synchronous in-memory chain.

## Storage

| Path (macOS/Linux) | Purpose |
|--------------------|---------|
| `~/.sentinel/config.yaml` | User configuration |
| `~/.sentinel/audit.db` | SQLite audit database |
| `~/.sentinel/patterns.yaml` | Detection pattern library (bundled, user-overridable) |
| `~/.sentinel/cache/` | Compiled regex cache, tool schemas |
| `~/.sentinel/logs/` | Proxy process logs (rotated) |

On Windows everything lives under `%APPDATA%\sentinel\` with equivalent layout.

## Non-goals of the architecture

- **No persistent state besides the audit DB and config.** No hidden state, no implicit caches that change behavior, no machine-specific tuning that survives reinstall.
- **No background daemon.** The proxy runs as a foreground process (or as a launchd/systemd unit if the user installs it that way). No auto-update daemon, no telemetry daemon, no separate "agent" process.
- **No mutation of upstream servers.** The proxy is transparent on the wire. Upstream MCP servers behave identically whether or not the proxy is present.
- **No coupling to a specific AI client.** The proxy must work identically whether the client is Claude Desktop, Cursor, a Python script, or a curl command speaking JSON-RPC.

## Related docs

- [[02-mcp-protocol]] — protocol details that justify "universal" claims
- [[03-proxy-design]] — proxy internals and JSON-RPC handling
- [[04-config-schema]] — config file format
- [[05-pattern-detection]] — what the pattern stage actually detects
- [[09-telemetry-pipeline]] — audit DB schema and telemetry boundaries
