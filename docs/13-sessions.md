# 13 — Per-Session Grouping (Slice 0.2.3)

> Status: Slice 0.2.3 implemented. The central dashboard gains a Sessions table aggregating events by `session_id`, plus composable filters (agent + session) carried in the URL hash as `#?agent=X&session=Y`. Clicking a session row narrows the activity feed; clicking an agent narrows the sessions list.

## Why sessions

An MCP "session" in this product is one client ↔ one proxy ↔ one upstream conversation: it starts when an MCP client like Claude Desktop spawns `sentinel run` (or opens a Streamable HTTP connection), and ends when that process exits. Inside one session you typically see:

- a few `initialize` / `tools/list` requests,
- many `tools/call` requests as the model uses the tools,
- responses and (occasionally) policy errors.

For an operator scanning the dashboard, the most useful unit is "what did agent X do during session Y?" — not the raw 2,000-row event stream. The Sessions section gives that summary at a glance: who, when, how long, how many events, did anything get blocked.

```
┌────────────────────────────────────────────────────────────────────────────┐
│ SESSIONS                                                                   │
│  ID            AGENT          STARTED                 DUR  EVENTS  BLKD ...│
│  91a4c8d2e0    alice-laptop   2026-05-14 19:14:01    3m   42      2       │
│  4f7b2a18c1    alice-laptop   2026-05-14 18:02:33    1h   210     0       │
│  c7e2d0a9f8    bob-laptop     2026-05-14 14:10:11    8m   89      0       │
├────────────────────────────────────────────────────────────────────────────┤
│ ACTIVITY  agent: alice-laptop ×  session: 91a4c8d2 ×   clear all          │
│  TIME       AGENT       DIR  TYPE     METHOD       SESSION  UPSTREAM ...   │
│  19:15:23   alice-…     s2c  error    —            91a4...  echo          │
│  19:15:23   alice-…     c2s  request  tools/call   91a4...  echo          │
│  ...                                                                       │
└────────────────────────────────────────────────────────────────────────────┘
```

A session is unique **per agent**: the same `session_id` reported by two different agents shows as two rows. (This effectively can't happen with the proxy's UUID generator, but the schema enforces it via `GROUP BY session_id, agent_id` so we don't silently merge identical IDs across machines.)

## Storage

`Storage.ListSessions(ctx, agentID, limit)` runs:

```sql
SELECT e.session_id,
       e.agent_id,
       a.name,
       MIN(e.server_ts), MAX(e.server_ts),
       COUNT(*) AS event_count,
       SUM(CASE WHEN e.direction='s2c' AND e.msg_type='error'
                 AND INSTR(e.payload, 'blocked by sentinel policy') > 0
                THEN 1 ELSE 0 END) AS blocked_count,
       COALESCE(GROUP_CONCAT(DISTINCT e.upstream), '')
FROM events e JOIN agents a ON a.id = e.agent_id
[WHERE e.agent_id = ?]
GROUP BY e.session_id, e.agent_id, a.name
ORDER BY MAX(e.server_ts) DESC
LIMIT ?;
```

The blocked-row pattern (`INSTR(payload, 'blocked by sentinel policy') > 0`) is the same one slice 0.2.1 settled on for top-level stats — `LIKE` against BLOB columns gave surprising results in pure-Go SQLite, and `INSTR` treats bytes as bytes. Re-using the same predicate means the section-level blocked count and the per-session blocked count always agree.

No new index is needed. The two indexes added in 0.2.1 (`idx_events_agent`, `idx_events_session`) already cover both the filter and the grouping.

## API

| Method | Path                                  | Notes                                              |
|--------|---------------------------------------|----------------------------------------------------|
| GET    | `/api/sessions`                       | Fleet-wide session aggregate.                      |
| GET    | `/api/sessions?agent_id=N`            | Narrow to one agent.                               |
| GET    | `/api/sessions?limit=N`               | Caps at 1000; default 200.                         |
| GET    | `/api/events?session_id=...`          | Narrow events to one session. Composes with `agent_id`. |

Wire shape returned by `/api/sessions`:

```json
{
  "session_id": "91a4c8d2e0...",
  "agent_id": 1,
  "agent_name": "alice-laptop",
  "first_ts": 1747164923000000000,
  "last_ts":  1747164943000000000,
  "event_count": 42,
  "blocked_count": 2,
  "upstreams": "echo,filesystem"
}
```

`upstreams` is the raw `GROUP_CONCAT(DISTINCT)` string; the SPA splits on `,` for display.

`Storage.ListEvents` was refactored to take an `EventFilter` struct (agent / session / query / limit) so all three filters AND together without callers chaining `if` branches. The handler reads them from query params; everything zero-valued is a no-op.

## URL hash format

The hash carries dashboard state so reload, deep-link, and tab-share all work:

```
#?agent=1&session=91a4c8d2e0...&q=tools/call
```

`history.replaceState` is used when filters change so typing in the search box doesn't pollute the browser back stack. The legacy `#/agent/<id>` form from slice 0.2.2 still parses as a fallback in `readHashFilter()` — kept for one slice to make hot reloads between dev sessions painless; will be removed by 0.3.

## UX flow

- **Click an agent row** → `state.filter.agentID` set, sessions list filtered to that agent, events filtered to that agent. Sessions table no longer shows other agents' sessions.
- **Click a session row** → `state.filter.sessionID` set (and `agentID` set to the session's agent so the chip strip stays consistent). Events table narrows to that session.
- **Click the same row again** → toggles the filter off.
- **Filter chip ×** → clears just that filter. A "clear all" appears when ≥ 2 chips are active.
- **Browser back/forward** → `hashchange` handler re-reads the URL and re-renders.

## Tests

`internal/server`:

- `TestSessions_AggregatesAcrossEvents` — three sessions across two agents; verifies fleet-wide grouping, blocked-count for the session with a policy error, upstreams string, event counts, and agent-id filter (alice's two sessions vs bob's one).
- `TestEvents_FilterBySession` — two sessions on one agent, `session_id=` query param returns only that session's events.
- Existing `TestEvents_FilterByAgent` (slice 0.2.1) was updated as part of the `ListEvents → EventFilter` refactor; still passes.

Full suite: 8 packages green, vet clean.

## Out of scope for 0.2.3

- A per-session timeline view (left-to-right of events inside a session). The current detail-modal-per-event is enough for the common "what tool got blocked" question; we can build a real timeline when there's user demand for it.
- Per-session export. Sessions are queryable via `/api/events?session_id=...&limit=1000&with_payload=true` already; a one-click export button can wait.

Next slice (0.2.4) is payload search.
