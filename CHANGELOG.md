# Changelog

All notable changes to Sentinel are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Planned

- **v0.2.6** — Slack / webhook alerts on policy blocks.
- **v0.2.7.1** — dynamic policy-engine reload (drop the "restart to pick up
  policy" footnote), policy revision diff view in the dashboard.
- **v0.2.8** — per-agent / per-group policies.
- **v0.3.0** — in-house behavioral anomaly detector trained on collected
  telemetry. First paid tier.
- **v1.0.0** — SSO, RBAC, on-prem hardened distribution, sandboxed
  execution for the riskiest tool categories.

## [0.2.7] — pending tag

Central policy distribution. A security team can now edit one policy in the
dashboard and have it apply fleet-wide, with stricter-wins merge semantics
that prevent a local laptop from weakening company-mandated rules.

### Added

- **`policy_revisions` table** on the central server with full audit
  history (id, body, etag, created_at, created_by). Latest revision is
  what gets served.
- **API**: `GET /api/policy`, `PUT /api/policy` (admin-gated),
  `GET /api/policy/revisions`, `GET /agent/v1/policy` (agent-bearer,
  honors `If-None-Match` → 304).
- **`internal/centralpolicy`** package — agent-side fetcher with disk
  caching, background poll, fail-soft chain (central → cache → empty).
  The proxy never fails closed because central is unreachable.
- **`Merge()`** pure function with stricter-wins semantics: DenyPaths
  union, ApproveThreshold/BlockThreshold min of non-zero values,
  central's `enabled` toggle deliberately ignored (local laptop owns
  the on/off switch).
- **Dashboard policy editor** — monospace JSON textarea with mid-edit-
  aware refresh, Save button enabled only when dirty.

Full design: [`docs/17-central-policy.md`](docs/17-central-policy.md).

## [0.2.5.1] — pending tag

Closes friction points from 0.2.5.

### Added

- **Cursor auto-detection** in `onboard`. `sentinel init --wrap-claude`
  and `sentinel enroll` now migrate Claude Desktop **and** Cursor in
  one pass, each with its own `.bak`.
- **`+ Enroll agent` button** in the dashboard SPA. Form takes name +
  metadata + TTL hours; on submit, a second modal shows the full
  `sentinel enroll <url>` command with a Copy button.
- **"Outstanding enrollments"** sub-section above the Agents table —
  per-row Revoke buttons for unconsumed-and-unexpired enrollments.

## [0.2.5] — pending tag

Zero-touch enrollment. Employee onboarding goes from "five steps and a
bearer token shared by hand" to one command.

### Added

- **`enrollments` table** with `crypto/rand` 256-bit OTTs, only the
  SHA-256 persisted, single-use + 24h default TTL.
- **API**: `POST /api/enroll` (admin), `POST /e/{ott}` (public — OTT
  is the credential), atomic consume in one DB transaction.
- **`sentinel-server enroll`** subcommands: `create`, `list`, `revoke`.
- **`sentinel enroll <url>`** on the agent side — exchanges OTT for a
  bearer token, writes `sentinel.yaml`, auto-wraps detected AI clients,
  saves timestamped `.bak`s.
- **`sentinel init --wrap-claude`** for the solo case — same wrap path,
  no central involved.
- **`internal/onboard`** package — cross-platform Claude Desktop config
  detection, JSON-preserving rewrite (unknown top-level keys survive),
  YAML-comment-preserving sentinel.yaml edits, atomic writes.

Full design: [`docs/16-enrollment.md`](docs/16-enrollment.md).

## [0.2.4] — pending tag

Payload search across the fleet.

### Added

- **`q=` filter** on `GET /api/events` — substring match via
  `INSTR(payload, ?)`, byte-safe against BLOB payloads.
- **Search input** above the activity table; 250 ms debounce, Escape
  clears, composes with agent + session filters via the URL hash.

Substring (not FTS or regex) chosen for predictable performance to
~100K rows without extra indexes. Case-sensitive by default — JSON
methods and tool names are lowercase by convention.

## [0.2.3] — pending tag

Per-session grouping + composable filter hash.

### Added

- **`Storage.ListSessions`** aggregating events by `(session_id,
  agent_id)`: first/last ts, event count, blocked count, distinct
  upstreams.
- **`GET /api/sessions`** with optional `agent_id` filter.
- **`session_id`** as a `/api/events` filter (composes with `agent_id`
  and `q`).
- **Sessions table** in the dashboard between Agents and Activity.
- **URL hash refactor**: `#?agent=X&session=Y&q=Z` form (was
  `#/agent/X`). Multi-chip filter strip; "clear all" affordance when
  ≥ 2 chips are active. Legacy single-form hash still parses as a
  fallback for one slice.

## [0.2.2] — pending tag

Central dashboard SPA + per-agent drill-down.

### Added

- **SPA**: HTML + vanilla JS + CSS embedded via `//go:embed`. Replaces
  the static placeholder at `/`. Top stats, agents table, activity
  table, four modals (event detail, add agent, token reveal, admin
  token).
- **`GET /api/events/{id}`** — single-event endpoint with payload
  always included. 404 / 400 paths.
- **Per-agent drill-down** — click an agent row, activity narrows;
  filter persists via URL hash.
- **Admin token UX** — sessionStorage-cached (per-tab, never on disk),
  Bearer-auth on `POST /api/agents` and `DELETE /api/agents/{id}`.

## [0.2.1] — pending tag

First slice of the v0.2 milestone. Self-hosted central server +
agent-side telemetry pump.

### Added

- **`sentinel-server` binary** — `serve`, `agent create/list/delete`,
  `version`, `help`.
- **`internal/server`** — `agents` and `events` tables with
  ON DELETE CASCADE; `Storage`, HTTP API (`/agent/v1/events`,
  `/agent/v1/health`, `/api/{stats,agents,events}`), embedded static
  landing page, admin-token gate for write endpoints.
- **`internal/telemetry`** — agent-side pump that ships events to
  central in batches, advances cursor only on 2xx success, resumable
  across restart, fails open (proxy hot path never blocked).
- **`internal/audit`** additions: `ReadEventsAfter`, `GetCursor`,
  `SetCursor`, `telemetry_state` table.
- **`config.CentralConfig`** — opt-in `central:` YAML block in the
  agent config.

Full design: [`docs/11-central-server.md`](docs/11-central-server.md).

## [0.1.0] — pending tag

First public release. Single Go binary that proxies MCP traffic between
an AI client (Claude Desktop, Cursor, Cline, MCP Inspector, …) and one
or more MCP servers, with a security pipeline and a local dashboard.

### Added

- **stdio transport** — proxy a local MCP server subprocess.
- **HTTP transport** — proxy a remote MCP server over Streamable HTTP
  (POST + one-shot JSON or SSE response, `Mcp-Session-Id` handling,
  custom auth headers).
- **Audit log** — every JSON-RPC envelope through the proxy persisted
  to a local SQLite DB (WAL mode, buffered writes).
- **Pattern detection** — code-owned regex/heuristic rules across six
  categories: sensitive paths, path traversal, command injection,
  unicode smuggling, prompt injection, secret-shaped strings.
- **Risk scoring** — 0-100 score per tool call, three-way decision
  (allow / require approval / block) with configurable thresholds.
- **Approval gate** — pending approvals queue with default-deny
  timeout; approve or deny from the local dashboard.
- **Auto-decisions ("remember my choice")** — persist allow/deny
  rulings per rule_id. Auto-allow only short-circuits the approval
  prompt; it never overrides a hard block. Auto-deny is absolute.
- **Local dashboard** — `sentinel dashboard` at
  `http://127.0.0.1:7842` with live event feed, stats, pending
  approvals UI, standing rules management. No framework, no build
  step.
- **Multi-server config** — `sentinel.yaml` with per-server transport,
  environment allowlist, and policy overrides. `sentinel init` writes
  a starter file.
- **Env stripping** — stdio upstreams see only OS-required system
  vars plus what is explicitly named under `env.allow`. Secrets like
  `AWS_*`, `GITHUB_TOKEN`, `OPENAI_API_KEY` are stripped by default.
- **Inline mode** — `sentinel run -- npx -y @some/server` for ad-hoc
  testing without writing a config file.

### Known limitations (v0.1)

- Localhost-only dashboard (by design — remote dashboards land in the
  v0.2 cloud-less central server).
- No automatic reconnect for HTTP upstreams on transient failures.
- No server-initiated SSE stream support (the GET half of Streamable
  HTTP).
- No environment-variable interpolation in YAML config (literal values
  only).
- Pattern detection is regex/heuristic only; ML-based detection ships
  in v0.3 once we have telemetry to train on.

### Internals

- Pure-Go SQLite (`modernc.org/sqlite`) — no CGO, simple cross-compile.
- Single static binary, ~11 MB.
- 153 tests across 10 internal packages as of v0.2.7, all green on
  Windows / macOS / Linux.
