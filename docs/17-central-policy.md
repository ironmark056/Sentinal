# 17 — Central Policy Distribution (Slice 0.2.7)

> Status: Slice 0.2.7 implemented. Security teams can now edit one policy in the central dashboard and have it apply to every connected agent automatically. The merge with each agent's local `sentinel.yaml` policy uses **stricter-wins** semantics, so an employee can add restrictions but cannot weaken what the company set.

## What ships

- **Central** stores versioned policy revisions in SQLite. Each `PUT /api/policy` appends a new row; the latest one is what agents see.
- **Agents** fetch the policy on startup, cache it to disk (`central-policy.json` next to `audit.db`), and merge it with their local `sentinel.yaml` before constructing the policy engine. A background poll (every 5 min) keeps the cache fresh; cache survives central outages.
- **Dashboard** has a Policy section with a JSON editor + Save button (admin-token gated, same as `+ Add agent` / `+ Enroll agent`).

```
                              ┌─────────────────────────┐
                              │  sentinel-server         │
                              │   policy_revisions      │
                              │     ├ rev 1 (Friday)    │
                              │     ├ rev 2 (Monday)    │
                              │     └ rev 3 (current) ◄─┼─── dashboard editor PUT
                              └────────────┬────────────┘
                                           │ GET /agent/v1/policy
                              ┌────────────┴────────────┐
              ┌───────────────┘                         └─────────────────┐
              ▼                                                            ▼
  ┌────────────────────────┐                                  ┌────────────────────────┐
  │ Alice's laptop          │                                  │ Bob's laptop            │
  │                         │                                  │                         │
  │  local sentinel.yaml    │                                  │  local sentinel.yaml    │
  │   deny: ~/Downloads     │   union ──► engine deny:         │   deny: (none)          │
  │                         │   • ~/Downloads (local)          │                         │
  │   approve_t: 30         │   • ~/.ssh (central)             │   approve_t: (default)  │
  │   block_t:   80         │   • ~/.aws (central)             │   block_t:   (default)  │
  │                         │                                  │                         │
  │  central cache:          │   min ──► thresholds:            │  central cache:          │
  │   deny: ~/.ssh ~/.aws    │   • approve_t: 20 (central)      │   deny: ~/.ssh ~/.aws    │
  │   approve_t: 20          │   • block_t:   70 (central)      │   approve_t: 20          │
  │   block_t:   70          │                                  │   block_t:   70          │
  └────────────────────────┘                                  └────────────────────────┘
```

## Merge semantics — "stricter wins, central never weakens local"

| Field | Rule |
|---|---|
| **DenyPaths** | **Union** of local and central, deduplicated, local first. Anything denied by either side is denied. |
| **ApproveThreshold** | **min** of the two non-zero values. Stricter (lower) wins; zero is treated as "not set." |
| **BlockThreshold** | Same — `min` of non-zero. |
| **Enabled** | Local only. Central's enabled toggle is **intentionally ignored**; only the laptop's operator decides whether enforcement runs at all. |

Local can ADD restrictions on top of central. Local cannot REMOVE a central restriction. The merge is pure, deterministic, and pure-function tested.

## Wire format

The policy body is JSON, matching the local YAML PolicyConfig with `_` instead of camelCase:

```json
{
  "deny_paths": ["~/.ssh", "~/.aws", "~/.kube"],
  "scoring": {
    "approve_threshold": 20,
    "block_threshold": 70
  }
}
```

The body is **stored verbatim** at central — there is no server-side schema validation beyond "is it valid JSON". A future agent that understands extra fields submits a body that older agents harmlessly ignore.

## API

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/api/policy` | none | Read the latest policy. Includes `ETag` header. |
| PUT | `/api/policy` | admin token | Append a new revision. Body is the JSON above. Returns the new revision metadata. |
| GET | `/api/policy/revisions` | none | List recent revisions (audit history). |
| GET | `/agent/v1/policy` | agent bearer | What agents poll. Honors `If-None-Match` → returns **304** when the cached ETag matches. |

ETag is the hex SHA-256 of the body, recomputed at `PUT` time. Stable across server restarts. The agent uses it for cheap polling: 304 has zero body and tells the agent "your cache is still current — just re-stamp `LastFetch`."

### Status codes

| Code | Meaning |
|---|---|
| 200 | OK; body in the response. (Empty `{}` if no policy has ever been set.) |
| 304 | Not modified (agent's `If-None-Match` matched). |
| 400 | Body is not valid JSON. |
| 401 | Auth required or wrong (`PUT` without admin token, `/agent/v1/policy` without bearer). |

## Storage

```sql
CREATE TABLE policy_revisions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    body        BLOB NOT NULL,
    etag        TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    created_by  TEXT
);
CREATE INDEX idx_policy_created_at ON policy_revisions(created_at);
```

Every PUT appends a row; the latest is what gets served. The history is preserved for audit; the dashboard's "View history" lists them (slice 0.2.7.1 will add a diff view).

`created_by` is whatever the editor put in the `X-Sentinel-Editor` header — the dashboard sets this to `"dashboard"`. CLI tools can set it to a user identifier.

## Agent lifecycle

1. On `sentinel run` startup, if `central:` is configured:
   - Construct a `centralpolicy.Fetcher`.
   - Call `Refresh(ctx)` with a 5s timeout.
     - Success → snapshot in memory + persisted to `~/.sentinel/central-policy.json`.
     - HTTP/network failure → load the disk cache. Run with whatever we have.
     - No fetch + no cache → run with **local-only** policy. Documented in stderr.
2. Build the policy engine from `centralpolicy.Merge(centralEffective, localDenyPaths, localApproveT, localBlockT)`.
3. The engine is **snapshotted at startup**. Dynamic engine reload when central pushes a new policy mid-session lands in slice 0.2.7.1; for now the background poll keeps the disk cache warm so the next restart picks up the latest.
4. Background poll on `pumpCtx` (5min). Each iteration sends the cached ETag in `If-None-Match`; central returns 304 on no-change and the loop is essentially free.

## Failure modes

| Scenario | Behavior |
|---|---|
| Central is reachable, returns valid policy | Snapshot in memory, persisted to disk, engine built from merged result. |
| Central is unreachable (no DNS, refused) | Disk cache loaded; engine built from cached + local. stderr logs the failure. |
| Central reachable but returns 401 (bad token) | Same as unreachable — fall back to cache + local. The proxy still runs. |
| Central reachable but returns malformed JSON | Logged; cache used. |
| First-ever run, central unreachable, no cache | `centralpolicy.Effective{Empty: true}` installed. Engine built from local-only. The proxy still runs. Local audit is the source of truth; central is downstream. |

The principle: **the agent never fails closed because central is unavailable.** Telemetry and policy distribution are downstream of the local protect-the-laptop story; central outages must not block AI tool calls from working.

## SPA

The "Policy" section sits between the stats grid and the existing Sessions / Agents tables. A monospace textarea pre-formats the current revision as JSON; a Save button enables only when the text differs from what we last fetched.

- Refresh polls `/api/policy` every 2.5s like every other dashboard data source. If the textarea is dirty (user is mid-edit), the refresh doesn't overwrite — `policyLastSaved` is compared to the current value before updating.
- ETag is tracked client-side too. Future work: optimistic-concurrency `If-Match` on PUT so two admins editing simultaneously get a 409 instead of silent last-write-wins.
- A small `policy-meta` chip in the header shows the current revision id + timestamp.

## Tests

`internal/server` (slice 0.2.7 additions):

- `TestPolicy_EmptyServerReturnsEmptyBody` — empty `{}` from `/api/policy` when no policy has ever been set.
- `TestPolicy_PutRequiresAdmin` — 401 without admin token.
- `TestPolicy_PutThenGet` — full round-trip, ETag matches between PUT and GET.
- `TestPolicy_PutRejectsInvalidJSON` — 400 on garbage body.
- `TestPolicy_AgentEndpointRequiresBearer` — 401 without agent token.
- `TestPolicy_AgentEndpointReturnsBodyAndHonorsIfNoneMatch` — first fetch 200; second with `If-None-Match` is 304.
- `TestPolicy_RevisionsList` — three PUTs produce three revisions, newest-first.

`internal/centralpolicy`:

- `TestMerge_Nil` — nil central passes local through.
- `TestMerge_UnionsDenyPaths` — local first, then central, dedup.
- `TestMerge_StricterThresholdWins` — 4 table-driven cases including the "each side fills the other's missing" case.
- `TestFetcher_Refresh_Success` — round-trip through an `httptest.NewServer` stub; parses deny paths + thresholds; sets ETag.
- `TestFetcher_Refresh_FailoverToCache` — primary stub closes, second fetcher pointed at the dead URL successfully loads from the disk cache.
- `TestFetcher_Refresh_FailNoCache` — no server + no cache → `Effective{Empty: true}` so the engine still constructs.
- `TestFetcher_HonorsIfNoneMatch_OnRefresh` — second refresh sends `If-None-Match`; server responds 304; deny paths preserved across the 304 path.
- `TestFetcher_RejectsBadOptions`.

Full project: 10 packages green, vet clean.

## What's deferred

- **Dynamic engine reload.** Today the agent snapshots policy at startup; a mid-session policy change is picked up at next restart. Reload-without-restart needs the policy engine to accept `UpdateDenyPaths` + `UpdateThresholds` calls and the proxy to swap the engine atomically. Targeted for 0.2.7.1.
- **Per-agent / per-group policies.** Single global policy in 0.2.7. Per-agent overrides + group-based policies (e.g. "engineering laptops have these rules, contractor laptops have those") are a v0.3 thing once we have telemetry showing it's a real ask.
- **Schema validation on the server.** The body is stored verbatim. We could validate against a server-side JSON Schema, but that locks the schema version on the central side; better to let the agent be the schema authority for now.
- **Diff view between revisions.** The audit list is there; a side-by-side diff is a small follow-up.
- **`If-Match` on PUT** so concurrent admin edits get a 409 instead of last-write-wins.
