# 10 — Local Dashboard (Slice 5)

> Status: Slice 5 implemented. `sentinel dashboard` serves a single-page web UI at `http://127.0.0.1:7842` that reads the audit DB. Read-only, no auth, localhost-bound. Approval queue UI is slice 4's concern; this slice ships only the visibility half.

## What it is

A small Go HTTP server, embedded inside `sentinel.exe`, that serves:

- A static SPA (HTML + vanilla JS + CSS) at `/`.
- Three JSON endpoints under `/api/`.

The SPA polls the API every ~2.5 seconds and renders a live view of recent audit events, top-level stats, and a "recent blocks" callout. Click any row → modal with the full JSON-RPC payload.

```
┌────────────────────────────────────────────────────────────────────┐
│ Sentinel   runtime security for AI tool calls          audit: …   │
├────────────────────────────────────────────────────────────────────┤
│  ┌──────┐ ┌──────┐ ┌──────────┐ ┌──────────┐                       │
│  │ 1247 │ │  89  │ │ 14 BLKD  │ │  6 SESS  │                       │
│  └──────┘ └──────┘ └──────────┘ └──────────┘                       │
├────────────────────────────────────────────────────────────────────┤
│ RECENT BLOCKS                                                      │
│  • 19:15:23  echo  tools/call  (id 7)                              │
├────────────────────────────────────────────────────────────────────┤
│ ACTIVITY                              [✓] auto-refresh             │
│  TIME      DIR  TYPE     METHOD       SESSION  UPSTREAM  STATUS    │
│  19:15:23  s2c  error    —            91a4...  echo      BLOCKED   │
│  19:15:23  c2s  request  tools/call   91a4...  echo      req       │
│  19:15:20  s2c  response —            91a4...  echo      ok        │
│  ...                                                               │
└────────────────────────────────────────────────────────────────────┘
```

## Why this is its own subcommand, not part of `run`

It would be tempting to embed the dashboard in every `sentinel run` invocation. Reality kills that:

- `sentinel run` is spawned **once per MCP client session**. With one-server-per-process semantics (slices 2-3) and a typical Claude Desktop config wiring multiple servers, you'd have 3-5 `sentinel run` processes running concurrently, each fighting for port 7842.
- The dashboard's value is precisely that it survives any single session. You want to look at "what happened last Tuesday" without needing a running proxy.

So the dashboard is a separate command. It reads the shared SQLite DB. It works whether the proxy is currently running or not.

## CLI surface

```
sentinel dashboard [--addr ADDR] [--audit PATH]
```

| Flag | Default | Notes |
|------|---------|-------|
| `--addr` | `127.0.0.1:7842` | Listen address. Always localhost. |
| `--audit` | OS config dir | Override audit DB path (matches the run subcommand). |

The server is always bound to `127.0.0.1` — it does not listen on `0.0.0.0`, by design. The audit log contains tool-call payloads which may include sensitive context; exposing this on a LAN interface would defeat the purpose of Sentinel. A future enterprise cloud tier (v0.2+) is the right place for remote/multi-user dashboards.

Open `http://127.0.0.1:7842/` in your browser. Press Ctrl+C in the terminal to stop.

## API surface

| Endpoint | Returns |
|----------|---------|
| `GET /api/stats` | Aggregates: total events, last-24h count, blocked count, distinct sessions, top methods, top upstreams, last 5 blocked calls, audit DB path. |
| `GET /api/events?limit=N&before_id=ID` | Paginated event list, ordered by id desc (most recent first). `limit` capped at 1000. `before_id` is exclusive — pass the last id from the previous page to get the next. |
| `GET /api/event/{id}` | Single event including the full raw payload. |

All endpoints return JSON. None accept writes. None require auth.

Open the JSON directly in a browser if you don't want the SPA — useful for ad-hoc inspection.

## SPA implementation choices

- **No framework, no build step.** `internal/dashboard/static/` holds three files: `index.html`, `app.js`, `style.css`. The Go binary embeds them via `//go:embed`. Total payload: ~12 KB.
- **Polling, not SSE.** Auto-refresh on a 2.5-second interval. SSE was tempting but adds a moving part (long-lived connections, reconnect logic) for marginal benefit at this scale. Polling 22 messages every 2.5 seconds is ~9 req/s peak, which is nothing.
- **Click-to-detail modal.** Avoids juggling routes/state in vanilla JS. The list is the entry point; the modal is the only secondary view.
- **Tabular monospace font.** This is a developer tool. Readability of JSON-RPC IDs, paths, and timestamps trumps marketing aesthetic.

## Why two real bugs surfaced building this

Worth recording so they don't bite future code:

### 1. `LIKE` on SQLite `BLOB` columns returns NULL

The audit DB stores JSON payloads in a `BLOB` column (chosen so binary content like image bytes doesn't get mangled). SQLite's `LIKE` operator on BLOB returns NULL — neither true nor false — which means a `WHERE … LIKE …` clause silently matches nothing.

`CAST(payload AS TEXT) LIKE …` *seems* like it should work but is also subtly broken in some SQLite drivers (type affinity propagates back).

The fix that actually works on bytes: `INSTR(payload, 'needle') > 0`. INSTR treats both arguments as byte sequences and returns the 1-based position of the first occurrence (or 0). No type drama.

Now used in `internal/dashboard/api.go` for the blocked-count query and the recent-blocks query.

### 2. Read-only `*sql.DB` keeps the file open on Windows

`sql.Open` returns a handle even in read-only mode. If you don't `Close()` it, Windows refuses to delete the underlying file. `t.TempDir()`'s cleanup then fails with "the process cannot access the file because it is being used by another process".

Resolution: `Server.Close()` releases the handle; tests register `t.Cleanup` to call both `ts.Close()` and `s.Close()`. POSIX silently tolerates this (file is unlinked when last fd closes); Windows surfaces it.

## What is *not* in this slice

| Feature | Where it lives |
|---------|----------------|
| Approval queue UI | Slice 4 — needs the approval gate mechanic in the proxy first |
| Per-session timeline view | Slice 5.1 polish, or merged into slice 7 |
| Filters (by direction, method, status, severity) | Slice 5.1 polish — basic table is enough for v0.1 |
| Real-time push (SSE / WebSocket) | Future — polling is fine for v0.1 |
| Authentication | Out of scope. Localhost-only + read-only is the security boundary. |
| Multi-instance / fleet view | Cloud tier (v0.2+) |
| Search by payload contents | Slice 5.1 polish, low priority |

## Testing

`internal/dashboard/dashboard_test.go` seeds a temporary audit DB with four representative rows (request, response, request, error) and runs every endpoint via `httptest.NewServer`:

| Test | What it pins |
|------|--------------|
| `TestStats_ReportsCounts` | Totals match the seed; blocked count uses the right query; recent_blocked is populated |
| `TestEvents_ListsRecentFirst` | `/api/events` returns most recent first |
| `TestEvents_Pagination` | `before_id` gives non-overlapping pages |
| `TestEventDetail_IncludesPayload` | The full payload is returned |
| `TestEventDetail_BadID` | Bad id returns 400 |
| `TestIndexPage_Served` | `/` serves index.html |
| `TestStaticAssets_Served` | `/app.js`, `/style.css` reachable |
| `TestAuditDBMissing_ReturnsError` | Constructor fails clearly if audit DB does not exist |

8 dashboard tests; full project is at ~50 tests, all green.

## Smoke test against your real audit DB

After `sentinel dashboard` is running (default `127.0.0.1:7842`):

```powershell
# Stats
Invoke-RestMethod -Uri "http://127.0.0.1:7842/api/stats" | ConvertTo-Json -Depth 4

# Last 3 events
Invoke-RestMethod -Uri "http://127.0.0.1:7842/api/events?limit=3"
```

Or just open `http://127.0.0.1:7842/` in a browser.

If you've done the `TESTING.md` walkthrough already, you should see your real test traffic in here: the `add` and `echo` round trips, plus error rows for every attack you fired against the everything server. The "recent blocks" callout near the top is the fastest sanity check that the policy engine is actually doing work.

## Related docs

- [[03-proxy-design]] — the proxy writes; this slice reads.
- [[09-telemetry-pipeline]] — coming with slice 8+, will define how this same data flows to a cloud backend.
- [[08-approval-flow]] — coming with slice 4, will add an approval queue UI to this dashboard.
