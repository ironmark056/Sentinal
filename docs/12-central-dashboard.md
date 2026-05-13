# 12 — Central Dashboard SPA (Slice 0.2.2)

> Status: Slice 0.2.2 implemented. The central server's root `/` now serves a single-page web UI that renders the multi-agent fleet view: stats, agent list with per-agent drill-down, fleet-wide or filtered activity table, click-to-detail modal, and admin-token-gated agent create / revoke flows. No build step.

## What it is

A small HTML + vanilla JS + CSS app embedded directly into `sentinel-server` via `//go:embed`. The SPA polls the same `/api/...` endpoints the binary already exposes, so the dashboard is just one more consumer of the API — not a separate service.

```
┌────────────────────────────────────────────────────────────────────────┐
│ Sentinel [central]  multi-agent fleet dashboard    admin: set  • live  │
├────────────────────────────────────────────────────────────────────────┤
│  ┌──────┐ ┌──────┐ ┌──────────┐ ┌──────────┐                           │
│  │ 8124 │ │  213 │ │  14 BLKD │ │  4 AGNTS │                           │
│  └──────┘ └──────┘ └──────────┘ └──────────┘                           │
├────────────────────────────────────────────────────────────────────────┤
│ AGENTS                                                  [+ Add agent]  │
│  ID  NAME           LAST SEEN   CREATED              METADATA          │
│  1   alice-laptop   12s ago     2026-05-09 09:14     os=macos          │
│  2   bob-laptop     2h ago      2026-05-10 14:00     os=linux          │
│  3   ci-runner      (never)     2026-05-11 11:33     —                 │
├────────────────────────────────────────────────────────────────────────┤
│ ACTIVITY     filtered: alice-laptop ×              [✓] auto-refresh    │
│  TIME       AGENT       DIR  TYPE     METHOD       SESSION  UPSTREAM   │
│  19:15:23   alice-…     s2c  error    —            91a4...  echo      │
│  19:15:23   alice-…     c2s  request  tools/call   91a4...  echo      │
│  ...                                                                  │
└────────────────────────────────────────────────────────────────────────┘
```

Click any row in the events table → modal with the full JSON-RPC payload, the joined agent name, both clocks (server and agent), and message metadata. Click any row in the agents table → filter the activity feed to just that agent and pin the filter to the URL hash as `#/agent/<id>` so it survives reload and can be shared.

## Why it ships inside `sentinel-server`, not a separate process

The local-laptop story (slice 5) split `sentinel run` and `sentinel dashboard` into separate processes because `run` is one-per-MCP-session while the dashboard's value is surviving any single session. The same problem doesn't exist for the central server: `sentinel-server serve` is already long-running, single-instance, and listens on a port. Embedding the SPA into that same process keeps the deployment story to "one binary, one port."

There is no auth on the read endpoints by design (same as `/api/agents`, `/api/events`, `/api/stats`). Write endpoints (`POST /api/agents`, `DELETE /api/agents/{id}`) are gated by `SENTINEL_ADMIN_TOKEN` when it is set on the server. The SPA picks up the operator's token from a small "admin token" config modal and stashes it in **sessionStorage** — wiped on tab close, never persisted to disk. Deployments inside a trusted VPN with no admin token configured continue to work without one; the WARNING already printed at server start is the loud signal.

## URLs the SPA depends on

| Method | Path                          | What for | Added in |
|--------|-------------------------------|----------|----------|
| GET    | `/api/stats`                  | top stats bar (total / last 24h / blocked / agent count) | 0.2.1 |
| GET    | `/api/agents`                 | agents table | 0.2.1 |
| POST   | `/api/agents`                 | + Add agent → token reveal modal | 0.2.1 |
| DELETE | `/api/agents/{id}`            | Revoke button per row | 0.2.1 |
| GET    | `/api/events?agent_id=&limit=`| activity table; filtered when `agent_id` is set | 0.2.1 |
| **GET**| **`/api/events/{id}`**        | **event-detail modal — new in 0.2.2** | **0.2.2** |
| GET    | `/healthz`, `/version`        | not used by the SPA, kept for ops | 0.2.1 |

`/api/events/{id}` returns the same `EventDTO` shape as `/api/events`, with `payload` always included (the list endpoint requires `with_payload=true` to include it). 404 when the id doesn't exist; 400 when the id isn't a positive integer.

## Per-agent drill-down

Click an agent row → state changes to `filterAgentID = <id>`, the URL hash becomes `#/agent/<id>`, the activity title gets `(<name>)` appended, the filter chip appears, and `refreshEvents()` re-runs with `?agent_id=<id>`. Clicking the same row again, the chip's `×`, or hitting back in the browser clears the filter.

Reloading the page while on `#/agent/<id>` shows `#<id>` in the filter chip first (just the numeric id), then after the agents list comes back from the API the chip swaps in the human name. This avoids a sync-load before the first paint while keeping the URL hash a complete restore signal.

## Agent create flow

`+ Add agent` opens a small form. Name is required; metadata is a free-text `key=value, key=value` field that the SPA parses client-side and POSTs as `{ name, metadata }`. On success the server returns:

```json
{
  "agent": { "id": 1, "name": "alice-laptop", "created": 1747... },
  "token": "mcpg_5f4a1d8c...",
  "note":  "Save this token; it will not be shown again."
}
```

The SPA pops a second modal that displays the token in a copy-friendly box plus the YAML snippet the operator can drop into their agent's `sentinel.yaml`:

```yaml
central:
  url: https://central.acme.internal:7843
  token: mcpg_5f4a1d8c...
  agent_name: alice-laptop
```

`mcpg_…` is never re-displayed; only the SHA-256 hash is persisted on the server (slice 0.2.1).

## Revoke flow

A "Revoke" button next to each agent row asks for confirmation, then `DELETE /api/agents/<id>`. The cascade in slice 0.2.1's schema drops the agent's events with it; the SPA refreshes stats, agents, and events to reflect the deletion. If the current filter happens to point at the revoked agent the filter is cleared.

## Admin token UX

A small button in the header opens a modal with a single password-masked field; the value goes into `sessionStorage` under `sentinel.adminToken`. The header chip ("admin token: set" / "admin token: unset") gives an at-a-glance signal of whether the SPA is currently authorized to make writes.

If a write returns 401, the SPA assumes the token is wrong or missing and surfaces a targeted error message pointing the operator at the admin-token button rather than a generic "request failed."

## What this slice does *not* do

- Per-session view (group events by `session_id`). Today the table shows raw events; sessions are visible by sorting / filtering. Belongs in 0.2.3.
- Search / full-text filter on payload contents. Belongs in 0.2.4.
- Alerting (Slack / webhook on policy blocks). Belongs in 0.2.5.
- TLS termination — operator runs a reverse proxy in front. Same as 0.2.1.
- Time-bucketed charts. The single-laptop UI doesn't have them either; we'll add them once the data is big enough to make them useful.

## Tests

`internal/server` gained these cases in this slice:

- `TestEventDetail_ReturnsPayloadAndJoinedAgent` — single-event endpoint joins the agent name and includes the raw payload.
- `TestEventDetail_NotFound` — 404 for unknown ids.
- `TestEventDetail_BadID` — 400 for non-numeric ids.
- `TestStaticAssets_Served` — `/`, `/index.html`, `/app.js`, `/style.css` all 200 with the right content-types and the expected substrings inside.

Full suite: 8 packages green on Windows, no skip / no flake, vet clean.

## File layout

```
internal/server/
├── api.go              # routes + handlers (added handleEventDetail in 0.2.2)
├── server.go           # mux + middleware + embed of static/
├── storage.go          # added GetEvent in 0.2.2
└── static/
    ├── index.html      # SPA shell — rewritten from placeholder in 0.2.2
    ├── app.js          # vanilla JS — added in 0.2.2
    └── style.css       # palette + table + modal — added in 0.2.2
```

Embedded asset total: ~26 KB. Build size after this slice: unchanged at ~10.9 MB for `sentinel-server` on Windows (the static files compress well into the binary's read-only data section).
