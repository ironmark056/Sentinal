# 11 — Central Server (Slice 0.2.1)

> Status: Slice 0.2.1 implemented. `sentinel-server` is a second binary — a self-hosted central service that ingests audit events from a fleet of `sentinel` agents and exposes a multi-agent API at `http://0.0.0.0:7843`. The matching dashboard UI is shipped piecemeal: the API is live now, the SPA lands in the next slice.

## Why a separate binary

The v0.1 product is a single laptop installing one Go binary in front of its local MCP servers. The v0.2 milestone makes Sentinel usable inside a team or company — multiple laptops or automation hosts running the agent, one shared place to see everything together. That shared place needs to:

- Run somewhere reachable by every agent.
- Persist events independently of any single agent's lifetime.
- Authenticate agents so a leaked one machine doesn't let strangers inject events.

That's a different deployment model from the laptop agent. Bundling it into `sentinel` would force every laptop install to embed a server. Splitting it into `sentinel-server` keeps each binary single-purpose, lets us ship laptop installs without changes, and matches the natural operator boundary (one server, many agents).

There is no SaaS component. The customer's IT team runs `sentinel-server` inside their own VPC / VPN / on-prem network. All data lives on disk in the data directory they pass with `--data`.

## What's in this slice

- `cmd/sentinel-server/main.go` — new binary entrypoint with four subcommands.
- `internal/server` — HTTP server, storage, embedded static landing page.
- `internal/telemetry` — the agent-side pump that ships local audit events to central.
- `internal/audit` additions:
  - `ReadEventsAfter(afterID, limit)` for the pump's batched read.
  - `GetCursor` / `SetCursor` on a new `telemetry_state` table for resumable shipping.
- `internal/config.CentralConfig` — opt-in `central:` block in `sentinel.yaml`.
- `cmd/sentinel/main.go` — wires the pump into `sentinel run` when the config block is present.

```
┌─────────────────┐ Bearer mcpg_…    ┌─────────────────────────┐
│ sentinel agent  │ ─────────────────►│ sentinel-server         │
│  (laptop A)     │ POST /events      │   ┌────────────────┐   │
└─────────────────┘                   │   │ SQLite (events │   │
                                      │   │   + agents)    │   │
┌─────────────────┐                   │   └────────────────┘   │
│ sentinel agent  │ ─────────────────►│   /api/{stats,         │
│  (laptop B)     │                   │     agents, events}    │
└─────────────────┘                   └─────────────────────────┘
```

Agents never delete from their local audit log. If central is unreachable, the pump pauses; when it comes back, it resumes from the persisted cursor. No event loss, no double-shipping (the cursor only advances after a successful `POST`).

## CLI surface

```
sentinel-server serve [--addr ADDR] [--data DIR]
sentinel-server agent create <name> [--meta key=value,...]
sentinel-server agent list [--json]
sentinel-server agent delete <id>
sentinel-server version
sentinel-server help
```

| Flag        | Default          | Notes |
|-------------|------------------|-------|
| `--addr`    | `0.0.0.0:7843`   | Listen address. Bind to `127.0.0.1:7843` for local-only; otherwise expose only inside the trust boundary. |
| `--data`    | `./data`         | Holds `sentinel-server.db`. Created if missing. |
| `--meta`    | (empty)          | Comma-separated `key=value` annotations for the agent (os, owner, etc.). |
| `--json`    | off              | Machine-readable agent list. |

Admin operations on the dashboard (create / delete agents over HTTP) are gated by the `SENTINEL_ADMIN_TOKEN` env var. When unset, all dashboard endpoints are open — fine inside a VPN with a single operator, dangerous on a wider network. The server logs a `WARNING` on startup if it's running without one.

## HTTP surface

Two distinct API groups behind different auth.

### Agent endpoints — `Authorization: Bearer mcpg_<32-byte-hex>`

| Method | Path                 | Body                       | Returns |
|--------|----------------------|----------------------------|---------|
| POST   | `/agent/v1/events`   | `{"events": [IngestEvent]}` | `{"accepted": N}` |
| GET    | `/agent/v1/health`   | —                          | `{"ok": true, "agent_id": N, "agent_name": "..."}` |

An `IngestEvent` mirrors `internal/audit.Event` with int64-nanosecond timestamps for JSON friendliness:

```json
{
  "agent_ts": 1747164923000000000,
  "session_id": "91a4...",
  "upstream": "echo",
  "direction": "c2s",
  "msg_type": "request",
  "msg_id": "7",
  "method": "tools/call",
  "payload": {"jsonrpc":"2.0","id":7,"method":"tools/call","params":{}},
  "bytes": 84
}
```

The server stamps a `server_ts` of its own and rejects events missing any of `{agent_ts, session_id, direction, msg_type}` — silently drops bad rows from the batch instead of failing the whole `POST`, so one malformed event from one agent never blocks the fleet.

### Dashboard endpoints — optional `Authorization: Bearer <admin>` for writes

| Method | Path                       | Notes |
|--------|----------------------------|-------|
| GET    | `/healthz`                 | Open. |
| GET    | `/version`                 | Open. |
| GET    | `/api/stats`               | Fleet aggregates: total / 24h / blocked / agent count. |
| GET    | `/api/agents`              | List of registered agents. |
| POST   | `/api/agents`              | Create agent. **Admin token required**; response is the only time the bearer is shown in cleartext. |
| DELETE | `/api/agents/{id}`         | Revoke and cascade-delete events. **Admin token required**. |
| GET    | `/api/events`              | Recent events; query params `agent_id`, `limit`, `with_payload=true`. |
| GET    | `/`                        | Static landing page (SPA lands next slice). |

## Agent tokens

Tokens are 32 random bytes hex-encoded with an `mcpg_` prefix — i.e. `mcpg_a1b2…`. Only the SHA-256 of the token is persisted (`agents.token_hash`); the raw token is shown to the admin exactly once at create time:

```
$ sentinel-server agent create alice-laptop --meta os=macos,owner=alice
Agent created: id=1 name=alice-laptop

Bearer token (save this — it will not be shown again):

  mcpg_5f4a1d8c…

Put this in the agent's sentinel.yaml under central.token.
```

Revoking an agent (`agent delete <id>`) drops the row and cascades to its events. We delete events because in this product the agent **is** the unit of trust: keeping events from a revoked agent in the dashboard would confuse future analysis ("whose events are these?"). v1.0 may add a soft-revoke that anonymizes instead.

## Agent config

The agent picks up its central credentials from `sentinel.yaml`:

```yaml
central:
  url: https://sentinel.acme.internal:7843
  token: mcpg_5f4a1d8c...           # from `sentinel-server agent create`
  agent_name: alice-laptop          # default: os.Hostname()
  flush_interval_seconds: 5         # default: 5
  batch_size: 100                   # default: 100
  # enabled: false                  # explicit kill switch; leave unset to enable
```

If the `central:` block is missing or `enabled: false`, the agent behaves identically to v0.1 — no network traffic, no pump goroutine started. The proxy hot path is untouched.

When the block is set, on `sentinel run`:

1. The pump issues a `GET /agent/v1/health` with the configured token. If it 401s or the server is unreachable, a warning prints but the proxy still starts — local audit is the source of truth, central is a downstream consumer.
2. The pump loads its persisted cursor from `telemetry_state` (keyed by canonical URL — same agent talking to two central servers keeps two separate cursors).
3. Every `flush_interval_seconds`, the pump reads up to `batch_size` events with `id > cursor`, marshals them, and POSTs. Only on `2xx` does it advance the cursor.

The pump runs on its own goroutine and never blocks the proxy. POST failures are logged and retried on the next tick.

## SQLite schema

```sql
CREATE TABLE agents (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    token_hash  TEXT NOT NULL UNIQUE,
    created_at  INTEGER NOT NULL,        -- unix nano
    last_seen   INTEGER,
    metadata    TEXT                     -- JSON
);

CREATE TABLE events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_id    INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    agent_ts    INTEGER NOT NULL,        -- agent's clock
    server_ts   INTEGER NOT NULL,        -- central's clock
    session_id  TEXT NOT NULL,
    upstream    TEXT NOT NULL,
    direction   TEXT NOT NULL,
    msg_type    TEXT NOT NULL,
    msg_id      TEXT,
    method      TEXT,
    payload     BLOB NOT NULL,
    bytes       INTEGER NOT NULL
);

CREATE INDEX idx_events_agent     ON events(agent_id);
CREATE INDEX idx_events_server_ts ON events(server_ts);
CREATE INDEX idx_events_method    ON events(method);
CREATE INDEX idx_events_session   ON events(session_id);
```

Both `agent_ts` and `server_ts` are persisted because clock skew between agents and central is real; the dashboard primarily sorts by `server_ts` (defendable ordering for an operator) but lets queries that care about the agent's view use `agent_ts`.

## What's deferred to later slices

Landed in [slice 0.2.2](12-central-dashboard.md):

- ~~The multi-agent SPA at `/`.~~ Now serves a real dashboard, not a placeholder.
- ~~Per-agent drill-down view.~~ Click an agent row; URL hash `#/agent/<id>` is the restore signal.

Still pending:

- Slack / webhook alerting on policy blocks (0.2.5).
- Full-text search across payloads (0.2.4).
- Per-session grouping (0.2.3).
- TLS termination guidance (today: terminate at a reverse proxy in front of the listener).

## Threat model — what this slice does and doesn't defend

| Threat                                                          | Defence today                                                                 |
|-----------------------------------------------------------------|-------------------------------------------------------------------------------|
| Stranger pushing fake events into the dataset                    | Bearer tokens, hashed at rest, 32-byte cryptographic.                          |
| Operator accidentally exposing dashboard to public internet      | Admin token required for write endpoints. `WARNING` logged when unset.        |
| One leaked agent token poisoning the whole dataset               | Per-agent quotas / rate-limits — **not yet**. Revocation works (drops events).|
| Replay of captured events                                        | Not addressed. Token + TLS-at-proxy is the assumed mitigation.                |
| Eavesdropper reading payloads on the wire                        | Run behind TLS (operator's reverse proxy). Server does not terminate TLS.     |
| Compromised central server reading sensitive payloads            | Out of scope — central is in the customer's trust boundary. Payloads can be redacted at the agent (v0.2.3 plan). |

## Tests

`internal/server` and `internal/telemetry` between them cover:

- Agent management: create/list/delete, duplicate-name rejection, token hashing (raw token never stored, lookup-by-token round-trips).
- Auth: missing token → 401, bad token → 401, valid token → 200 + identity echoed.
- Ingest: round-trip with two events, malformed-row drop without failing the batch, fleet-wide stats aggregating across agents, per-agent filter on `/api/events`.
- Admin gating: POST `/api/agents` without admin token → 401, with admin token → 200 + token in body.
- Pump: ships events and advances cursor, resumes from persisted cursor across restart without duplicates, survives sustained server 401s without advancing the cursor, rejects bad options, `CheckHealth` round-trips.

Full project test suite passes (~95 tests across 8 packages) on Windows with `go test ./... -count=1`.
