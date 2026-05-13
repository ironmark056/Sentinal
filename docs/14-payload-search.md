# 14 — Payload Search (Slice 0.2.4)

> Status: Slice 0.2.4 implemented. The central dashboard gains a search box above the Activity table that filters events by substring match against the raw JSON payload. Composes with the agent and session filters from slice 0.2.3 via a unified URL hash.

## What it does

A small `<input type="search">` to the right of "Activity". Typing runs a 250 ms debounced fetch with `q=<value>` appended to `/api/events`. The server filters rows whose `payload` BLOB contains the substring. Composes with the existing `agent_id=` and `session_id=` filters.

```
ACTIVITY    [search payloads…    ssh        × ]   [✓] auto-refresh
agent: alice-laptop ×   search: "ssh" ×   clear all
─────────────────────────────────────────────────────────────────────
TIME       AGENT       DIR  TYPE     METHOD       SESSION  UPSTREAM …
19:15:23   alice-…     c2s  request  tools/call   91a4...  filesystem
19:15:18   alice-…     c2s  request  tools/call   91a4...  filesystem
```

A typical use: `q=tools/call` to see only invocations, `q=blocked` to see only policy errors, `q=write_file` to see all writes the fleet attempted.

## Why substring, not FTS or regex

Three reasonable choices were on the table:

1. **Substring** (`INSTR(payload, q) > 0`). Trivial implementation, exactly matches what an operator types, predictable performance to ~100K rows on a laptop's SQLite without any extra index.
2. **FTS5 virtual table**. Better at "find the row" but worse at "find a literal token in a JSON string." Schema migration cost to set up + keep in sync. Pure-Go `modernc.org/sqlite` supports FTS5 but the boilerplate (triggers to mirror payload into a content-less FTS5 shadow) is non-trivial for v0.2.
3. **Regex**. Powerful but the SPA UX (one search box) doesn't surface the difference well, and `LIKE`-with-regex via a custom function is a pain in modernc.org/sqlite.

Substring is the right primitive for the way operators actually search dashboards: "did anyone ever call `tool_x`?" / "is `secret-thing` ever in a payload?" When this stops scaling (millions of events on one server) we'll move to FTS5; the API shape doesn't need to change.

## Storage

Already in place from slice 0.2.3: `EventFilter.Query` runs `INSTR(e.payload, ?) > 0` in the same `WHERE` clause as `AgentID` and `SessionID`. Empty `Query` skips the predicate entirely so an empty search box is identical to no search.

`INSTR` (not `LIKE`) is the same byte-safe primitive slice 0.2.1 used for the blocked-count predicate. `LIKE` against BLOB columns gives surprising results in pure-Go SQLite (see docs/10 for the original write-up) — `INSTR(blob, str) > 0` works regardless of payload encoding.

## Case sensitivity

Searches are **case-sensitive** in 0.2.4. JSON method names, tool names, and most payload tokens are lowercase by convention (`tools/call`, `read_file`), so this matches operator expectations more often than not. The cost of switching to case-insensitive (`INSTR(LOWER(payload), LOWER(?)) > 0`) is one extra scan of the BLOB per row; we'll do it if real usage shows it's worth the extra I/O.

If you want a case-insensitive search today, lowercase your needle: `q=ssh` matches `"ssh"` but not `"SSH"`; type `q=ssh` plus another search for `q=SSH` if you need both.

## URL hash

Composes with the filters from slice 0.2.3:

```
#?agent=1&session=91a4c8d2e0...&q=tools%2Fcall
```

`q` is URL-encoded so slashes and special characters survive the round trip. `history.replaceState` (not `pushState`) is used as the SPA writes back to the hash on every keystroke — typing "tools/call" should not create eight back-stack entries.

## SPA behavior

- 250 ms debounce after the last keystroke; cancels in-flight timer on each key.
- Pressing **Escape** inside the input clears the search.
- The "search" chip shows `"<your text>"` in quotes so leading/trailing spaces are visible.
- The chip's `×` clears the search; the input clears too.
- "Clear all" (next to the chip strip when ≥ 2 are active) clears agent, session, **and** search at once.

## Tests

`internal/server`:

- `TestEvents_FilterByQuery` — three events with distinct tool names; `q=ssh` returns only the row that contains `"ssh"` in its payload.
- `TestEvents_QueryNoMatch` — searching for an absent string returns zero rows.
- `TestEvents_EmptyQuery_NoFilter` — `q=` (empty) returns all events; the empty case must be a no-op, not a "match nothing".
- `TestEvents_QueryComposesWithAgent` — both alice and bob have a matching payload; `q=ssh&agent_id=<alice>` returns only alice's row.
- `TestEvents_QueryComposesWithSession` — two sessions on one agent both contain the needle; `q=ssh&session_id=s-B` returns only s-B's row.

Full project: 8 packages green, vet clean.

## Limits

- Single substring only. No `OR`, no method prefix, no field selector (e.g., `params.name:ssh`). That's fine for 0.2 — the surface stays one input. Multi-term and field-targeted search land when there's a real ask.
- Max 1000 events per response (existing cap on `/api/events`). A search that would naturally return 5,000 rows returns the most recent 1,000 with no pagination. Pagination lands in 0.2.6 if needed.
- No saved searches. Bookmark the URL hash for now.

Next slice (0.2.5) is Slack / webhook alerting on policy blocks.
