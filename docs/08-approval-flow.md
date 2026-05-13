# 08 — Approval Flow (Slice 4)

> Status: Slice 4 implemented. Calls that score in the "approve" range suspend in the proxy, surface in the dashboard as pending approvals with approve/deny buttons, and resume (or terminate) based on the human's answer or a configurable timeout.

## The flow end to end

```
┌──────────┐        ┌──────────────┐         ┌──────────────────┐
│ AI Agent │  call  │  sentinel    │  insert │ approvals table  │
│ (client) ├───────▶│   proxy      ├────────▶│  (SQLite, shared)│
└──────────┘        └──────┬───────┘         └────────┬─────────┘
                           │   poll (500ms)           │
                           ▼                          │
                    (suspended goroutine)             │
                                                      │
                    ┌──────────────────────────────┐  │
                    │   sentinel dashboard         │◀─┘
                    │   GET /api/approvals → list  │
                    │   POST /api/approvals/N/approve│
                    │   POST /api/approvals/N/deny   │
                    └──────────────────────────────┘
                              │
                              ▼
                       row.status changes
                              │
                              ▼
                    Proxy resumes:
                      - approved → forward original call to upstream
                      - denied   → synth JSON-RPC error to client
                      - timeout  → synth error, default-deny
```

## What lives where

| Component | What it does |
|---|---|
| **`internal/policy/`** | Returns `DecisionApprove` when 30 ≤ score < 80 (defaults). No knowledge of the queue. |
| **`internal/approval/`** | Owns the `approvals` SQLite table. Insert / Get / ListPending / Resolve / WaitForDecision. |
| **`internal/proxy/proxy.go`** | On `DecisionApprove`: Insert a row, call `WaitForDecision`, act on the returned status. |
| **`internal/dashboard/`** | Exposes the queue over HTTP. List + Approve + Deny endpoints. Vanilla-JS UI cards with risk score and finding details. |

## The data model

```sql
CREATE TABLE approvals (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at     INTEGER NOT NULL,        -- unix nanos
    session_id     TEXT NOT NULL,           -- proxy session uuid
    upstream       TEXT NOT NULL,           -- logical server name
    msg_id         TEXT NOT NULL,           -- original JSON-RPC id
    method         TEXT NOT NULL,
    tool_name      TEXT,                    -- inner name from params.name
    risk_score     INTEGER NOT NULL,        -- 0..100
    findings_json  BLOB NOT NULL,           -- JSON array of policy findings
    payload        BLOB NOT NULL,           -- raw original request envelope
    status         TEXT NOT NULL DEFAULT 'pending',
    resolved_at    INTEGER,                 -- unix nanos, null while pending
    resolved_by    TEXT                     -- "dashboard" by default
);
```

The table lives in the same SQLite file as the audit log. Two reasons:

1. **One source of truth.** Cross-process coordination between the proxy and the dashboard works because they both read/write this single file.
2. **Forensic persistence.** The row survives even after the suspended call is gone (e.g. on proxy crash). The dashboard still shows what was pending and what was decided.

## Why polling instead of channels / signals

The proxy and dashboard are different processes. Channels and Go's `context.Context` can't bridge them. Options were:

- **SQLite polling** (chosen) — each suspended call polls its own row every 500ms. Simple, no inter-process plumbing, latency under a second.
- **Unix socket / named pipe** — would need cross-platform abstraction and reconnect logic. Bigger surface for v0.1.
- **SQLite update hooks** — modernc.org/sqlite doesn't expose these portably. Dead end.

500ms polling is unmeasurable load (one query against an indexed table). For human-scale UX (the user clicks Approve, expects "soon" not "instant"), this is fine. A future remote dashboard tier may use SSE or WebSockets to push approval requests faster, but the local case doesn't need it.

## Timeouts and default-deny

If the suspended call has been pending for `approval_timeout_seconds` (default 60), the proxy:

1. Stops waiting.
2. Calls `Resolve(id, StatusTimeout, "(timeout)")` to mark the row as timed-out.
3. Sends a JSON-RPC error to the client with message `approval #N timed out after Xs (default deny)`.

Default-deny is the correct posture: an un-decided request is not approved by inaction. Users who want a longer window can set `approval_timeout_seconds: 300` (5 min) or similar.

The dashboard does not currently filter out timed-out rows — they remain visible in the pending-approvals API (though the proxy will refuse to re-resolve them, returning HTTP 409). Slice 7 may add a "history" tab for resolved approvals.

## HTTP surface

| Endpoint | Verb | Auth | Notes |
|---|---|---|---|
| `/api/approvals` | GET | none (localhost) | Returns pending only, oldest-first. |
| `/api/approvals/{id}/approve` | POST | none (localhost) | Resolves to `approved`. Idempotent: second call returns HTTP 409. |
| `/api/approvals/{id}/deny` | POST | none (localhost) | Resolves to `denied`. |

`POST /api/approvals/{id}/{approve\|deny}?by=username` records `username` in `resolved_by`. Default is `dashboard`.

**No CSRF protection.** The dashboard binds to `127.0.0.1` only. A page on a different origin cannot POST to it (browsers block cross-origin POST by default, and the dashboard sets no CORS-permissive headers). When we ship a remote dashboard tier, CSRF protection becomes mandatory; for v0.1 local-only, it's a non-issue.

## What the client sees during a suspension

The MCP client's `tools/call` request just blocks for up to `approval_timeout_seconds`. From the client's perspective:

- It sent a JSON-RPC request.
- It got back either a JSON-RPC `result` (approved → upstream's actual response) or a JSON-RPC `error` (denied / timed out).
- It did not see any progress notifications or signals.

This is by design. The MCP spec does not have a "still thinking" intermediate state for tool calls. Adding one would require client-side support that doesn't exist yet (no current MCP client interprets such a state). The honest cost is that during a long suspension, the client's UI may show a stalled tool call. Compared to the alternative — silent block on every Approve-tier call — this is preferable.

## What gets blocked vs. what gets sent for approval

| Tool call | Findings | Score | Decision |
|---|---|---|---|
| Read `~/Documents/notes.md` | none | 0 | allow (no question asked) |
| Read `~/.ssh/id_rsa` | ssh-secrets (High) | 50 | **approve** |
| Read `/etc/passwd` | system-secrets (High) | 50 | **approve** |
| Read `../../etc/passwd` | path-traversal (High) + system-secrets (High) | 100 | **block** (never asks) |
| `rm -rf /tmp` | dangerous-command (Critical) | 90 | **block** |
| Echo `"Ignore all previous instructions"` | prompt-injection (High) | 50 | **approve** |
| Echo `"AKIAIOSFODNN7EXAMPLE"` | aws-access-key (Critical) | 90 | **block** |

Note the SSH and prompt-injection cases that *blocked* in slice 3 now *ask*. This is the intended slice 4 design — single-finding Highs are real signals, but they are also where most false positives live, so asking is more useful than refusing.

## Testing

| Test | Pins |
|---|---|
| `TestInsertAndGet` | Approval row persists with all fields |
| `TestListPending` | Only pending rows surface |
| `TestResolve_Approve` | Status transitions to approved with resolved_by recorded |
| `TestResolve_DoubleResolveRejected` | Second resolution attempt errors |
| `TestResolve_BadStatus` | Cannot Resolve to `pending` |
| `TestWaitForDecision_Approved` | Wait returns within poll interval after concurrent approve |
| `TestWaitForDecision_Timeout` | Wait returns `StatusTimeout` when ctx deadline hits |
| `TestProxy_BlocksDangerousCommand` | Critical findings still block without going through approval |
| `TestProxy_ApprovalApproved` | Proxy suspends, dashboard approves, upstream sees the call |
| `TestProxy_ApprovalDenied` | Proxy suspends, dashboard denies, client gets JSON-RPC error |
| `TestApprovals_ListAndResolve` | Dashboard endpoints work end-to-end |
| `TestApprovals_BadAction` | Bad action verb returns 400 |

That's 12 tests covering this slice specifically. Full project is at ~60 tests, all green.

## Failure modes worth knowing

- **Proxy crashes while approval is pending.** The row stays in the DB. The client connection is gone, so even if a human later approves, the upstream is not contacted (nothing to contact for). The approval row records the human's decision for audit purposes only. (Slice 4.x might add an explicit "stale" status.)
- **Dashboard not running while approval is pending.** Nothing approves it. The call times out and is default-denied. This is the same as if a human was online but said no.
- **Two dashboards open and both approve simultaneously.** First one wins via the SQL `WHERE status = 'pending'` guard. Second one gets HTTP 409.
- **Approval store DB locked.** SQLite WAL mode + 5s busy_timeout means this resolves itself in practice. If a write fails after retry exhausts, the proxy default-denies.

## Auto-decisions: "remember this rule" (slice 4.1)

A common workflow problem after slice 4: an agent that legitimately needs to touch a particular path or send a particular string keeps hitting the same rule, and the user keeps clicking Approve. Slice 4.1 fixes this with **auto-decisions** — persistent allow/deny rulings on a given rule_id.

### How the UI flows

Every pending-approval card now has a checkbox: **"Remember this decision for `<rule-id>`"**. When ticked, the action the user clicks (Approve or Deny) is also persisted as an auto-decision for that rule. From that point forward:

- Auto-allow rules: future calls whose findings are *all* covered by auto-allow rules are forwarded immediately (no approval queue, no human prompt).
- Auto-deny rules: future calls in which *any* finding matches an auto-deny rule are blocked immediately.

The asymmetry is the safety property: auto-allow requires every triggering rule to be covered, so a novel finding will still prompt the human. Auto-deny only needs one match because "deny means deny."

The dashboard renders a "Standing rules" section listing active auto-decisions with a **Remove** button each. Removing an entry restores the normal approval flow for that rule.

### Where they live

The `auto_decisions` table in the same SQLite file as audit + approvals:

```sql
CREATE TABLE auto_decisions (
    rule_id     TEXT PRIMARY KEY,   -- "category/rule", e.g. "sensitive-path/ssh-secrets"
    decision    TEXT NOT NULL,      -- 'approved' or 'denied'
    created_at  INTEGER NOT NULL,
    created_by  TEXT NOT NULL,      -- "dashboard" by default
    note        TEXT
);
```

### Auto-allow does *not* override hard blocks

A persistent auto-allow only short-circuits the **Approve** tier. A Critical finding that puts the score at ≥ block_threshold still produces a Block, regardless of any auto-allow on a related rule.

Concretely: if you've auto-allowed `path-traversal/triple-dot` and a later call combines it with `command-injection/dangerous-command` (Critical), the call is blocked. The user only said yes to the lesser rule.

The other direction is also asymmetric: there is no UI to auto-allow a rule that *only ever produces Block-tier outcomes* (the user never sees an approval card for it). To pre-allow a Critical-tier rule, the user would have to lower thresholds in config — a deliberate change, not a one-click oversight.

### HTTP surface (additions)

| Endpoint | Verb | Notes |
|---|---|---|
| `/api/approvals/{id}/approve?remember=true` | POST | Resolve as approved AND set auto-allow for every rule_id in this approval's findings. |
| `/api/approvals/{id}/deny?remember=true` | POST | Same, for auto-deny. |
| `/api/auto-decisions` | GET | List active auto-decisions. |
| `/api/auto-decisions/{rule_id}` | DELETE | Remove (idempotent). |

### Tests

- `TestAutoDecision_SetGetList` — store CRUD basics
- `TestAutoDecision_UpsertReplaces` — re-setting the same rule overwrites cleanly
- `TestAutoDecision_RejectsBadDecision` — only approved/denied allowed
- `TestAutoDecision_RejectsEmptyRule` — empty rule_id rejected
- `TestAutoDecision_DeleteIsIdempotent` — delete-missing is a no-op
- `TestProxy_AutoAllow_ShortCircuitsApprove` — proxy bypass on auto-allow
- `TestProxy_AutoDeny_HardBlocks` — proxy hard-block on auto-deny
- `TestProxy_AutoAllow_DoesNotOverrideBlock` — auto-allow can never bypass a Block

## What this slice does *not* do

- **Bulk approve / deny.** One row at a time. UI would be nice; functionality not blocking.
- **Approval rules beyond rule_id ("auto-approve filesystem.read under ~/Documents").** A future "policy override" feature could let the user write per-tool / per-argument-shape rules. Slice 7 or later.
- **Approval expiry beyond timeout.** No way to say "approve this for the next 5 minutes" yet.
- **Push notifications.** No system tray, no Slack ping. Slice 7 hook point.

## Related docs

- [[06-risk-scoring]] — how the score that drives this is computed
- [[05-pattern-detection]] — what generates the findings that drive the score
- [[10-dashboard]] — the UI half of this slice (approval cards, buttons, modal)
- [[03-proxy-design]] — `evaluate()` and `handleApproval()` live in `internal/proxy/proxy.go`
- [[04-config-schema]] — `policy.scoring.*` config fields
