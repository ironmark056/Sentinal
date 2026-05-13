# 18 — Use Cases & Benefits

> A non-architectural read-me-first doc. Two flavors of Sentinel ship from the same binary: a **personal** install for one developer, and a **fleet** install for a company. This page explains what each looks like, where it fits in real life, and what you get from running it.

## In one sentence

**Sentinel is a security proxy that sits between AI tools (Claude Desktop, Cursor, Cline) and the MCP servers they call — so every tool call gets logged, dangerous calls get blocked, and a human can be asked before risky ones go through.**

---

## Where it sits

```
                ┌────────────────────────────────────────────────┐
                │  Without Sentinel:                             │
                │                                                │
                │    AI agent  ───────────────►   MCP server     │
                │    (Claude)                     (filesystem,   │
                │                                  github,       │
                │                                  shell, ...)   │
                │                                                │
                │    Nobody knows what just happened.            │
                └────────────────────────────────────────────────┘

                ┌────────────────────────────────────────────────────────────┐
                │  With Sentinel:                                            │
                │                                                            │
                │                ┌──────────────────┐                        │
                │   AI agent ───►│    sentinel      │───► MCP server         │
                │                │                  │                        │
                │                │  • audit log     │                        │
                │                │  • denylist      │                        │
                │                │  • risk score    │                        │
                │                │  • approval gate │                        │
                │                │  • policy engine │                        │
                │                └────────┬─────────┘                        │
                │                         │                                  │
                │                         ▼                                  │
                │                ┌──────────────────┐                        │
                │                │  audit DB        │  ← dashboard reads     │
                │                │  (SQLite)        │                        │
                │                └──────────────────┘                        │
                └────────────────────────────────────────────────────────────┘
```

Every tool call your AI agent makes is now visible, blockable, and logged. The proxy is a single static Go binary; it runs locally and starts in milliseconds. No cloud. No signup.

---

## Who uses it

| Audience                | What they install              | Where data lives                | Why they care                                                                  |
|-------------------------|--------------------------------|---------------------------------|--------------------------------------------------------------------------------|
| **Solo developer**      | `sentinel` on their laptop      | SQLite file in their home dir   | "Show me what my AI just did. Stop it before it does something dumb."          |
| **Engineering team**    | `sentinel` on every dev laptop  | Each laptop + central server    | One audit trail for the team, one policy for everyone, see what tools get used |
| **Security / compliance** | `sentinel-server` on a VPC host | Customer-owned host             | Immutable record, fleet visibility, exportable for SOC2 / ISO27001             |
| **Engineering leader**  | Same fleet install              | Same                            | "Are my engineers using AI safely? What blocks have triggered? Any incidents?" |

---

# Part 1 — Individual developer

## Architecture (personal install)

```
┌─────────────────────────────────────────────────────────────────────┐
│  Your laptop                                                        │
│                                                                     │
│   ┌────────────┐     ┌────────────────────┐     ┌────────────────┐  │
│   │   Claude   │────►│   sentinel run     │────►│  MCP server     │ │
│   │  Desktop   │     │   (one per server) │     │  (filesystem)   │ │
│   │            │     │                    │     └────────────────┘  │
│   │   Cursor   │────►│  • policy.engine   │                         │
│   │            │     │  • risk scoring    │     ┌────────────────┐  │
│   │   Cline    │────►│  • approval queue  │────►│  MCP server     │ │
│   └────────────┘     └─────────┬──────────┘     │  (github)       │ │
│                                │                └────────────────┘  │
│                                ▼                                    │
│                       ┌────────────────────┐                        │
│                       │  ~/.sentinel/      │                        │
│                       │    audit.db        │   ◄── sentinel         │
│                       │    sentinel.yaml   │       dashboard         │
│                       └────────────────────┘     (http://127.0.0.1:7842)│
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

Everything is **local**. No outbound network, no telemetry, no signup. The dashboard runs on localhost only.

## Onboarding (under 30 seconds)

```
                   ╭───────────────────────────────────────╮
                   │  sentinel init --wrap-claude          │
                   ╰───────────────────┬───────────────────╯
                                       │
                                       ▼
            ┌────────────────────────────────────────────────────────┐
            │  ✓ wrote ~/.sentinel/sentinel.yaml                     │
            │                                                        │
            │  ✓ detected Claude Desktop config                      │
            │    → wrapped 2 servers (filesystem, github)            │
            │    → .bak.2026-05-14T19-30-21Z saved                  │
            │                                                        │
            │  Done. Restart Claude Desktop to begin reporting.      │
            └────────────────────────────────────────────────────────┘
```

**That's it.** No YAML to write, no Claude config to find. Both Claude Desktop and Cursor are auto-detected; their MCP server entries are transplanted into your `sentinel.yaml` and the client config is rewritten to launch through `sentinel`. Timestamped backups of every file touched, idempotent on re-run.

## What a tool call looks like

```
       Claude wants to read a file
                │
                ▼
   ┌────────────────────────────────────────────────────────────┐
   │  sentinel receives:                                        │
   │    method = tools/call                                     │
   │    params = { name: "read_file", arguments: { ... } }      │
   │                                                            │
   │  Policy engine runs:                                       │
   │    1. Check denylist  ──► not denied? continue.            │
   │    2. Score the call  ──► risk = 18 (well below threshold) │
   │    3. Log it          ──► row added to audit.db            │
   │                                                            │
   │  Forward to MCP server.                                    │
   │  Forward the response back to Claude.                      │
   │                                                            │
   │  Log the response too.                                     │
   └────────────────────────────────────────────────────────────┘
                                │
                                ▼
                       Claude gets the file content
```

Risky calls take a different path:

```
       Claude wants to delete a directory
                │
                ▼
   ┌────────────────────────────────────────────────────────────┐
   │  Policy engine:                                            │
   │    1. Denylist?       not exactly, but...                  │
   │    2. Score:          risk = 75   (writes outside project) │
   │    3. Threshold:      approve_threshold=30, block=80       │
   │    4. Decision:       REQUIRE_APPROVAL                     │
   │                                                            │
   │  Pause this call. Add to /api/approvals queue.             │
   └─────────────────────┬──────────────────────────────────────┘
                         │
                         ▼
                ┌────────────────────┐
                │  Dashboard shows:  │
                │  "Pending approval"│
                │  [Approve] [Deny]  │
                └──────────┬─────────┘
                           │
            you click Approve (or "Remember this decision")
                           │
                           ▼
                  Call proceeds (or doesn't)
```

If you don't respond within the configured timeout (default 60s), the call gets denied. Safe-by-default.

## Day in the life

```
9:14am  Open Claude Desktop. Ask it to refactor a service.
        │
        ▼  20-30 tool calls fly by. Every one is logged.
        │
9:21am  Glance at the dashboard. 22 calls. 0 blocks.
        │
10:03am Ask Claude to clean up an old branch's files.
        │
        ▼  Claude tries to delete a file outside the project.
        │  Dashboard pops "Pending approval" — you see the
        │  exact path and click Deny.
        │
        ▼  Claude gets a clean error, picks a different approach.
        │
10:08am Right-click an entry from a week ago, see the full
        JSON-RPC payload. Search "secret_key" across all events
        — nothing. Good.
```

## Benefits — individual

| You get…                                  | Today's alternative                                 |
|-------------------------------------------|------------------------------------------------------|
| Every AI tool call recorded in SQLite      | Nothing — vanished into the void.                    |
| Dashboard you can grep through             | Reading scrollback in your AI client (if you can).  |
| Approval gate for risky operations         | Crossing fingers.                                   |
| Default denylist for sensitive paths       | Trusting the model's training data.                 |
| One YAML file owns policy for all clients  | Per-client config sprawl.                           |
| **Reversibility** when AI does something weird | "What just happened?"                            |

---

# Part 2 — Company / team

## Architecture (fleet install)

```
   ┌──────────────────────┐   ┌──────────────────────┐   ┌──────────────────────┐
   │ Alice's laptop       │   │ Bob's laptop         │   │ Ci-runner box        │
   │                      │   │                      │   │                      │
   │  Claude → sentinel   │   │  Cursor → sentinel   │   │  script → sentinel   │
   │            │         │   │            │         │   │            │         │
   │            ▼         │   │            ▼         │   │            ▼         │
   │       audit.db       │   │       audit.db       │   │       audit.db       │
   │            │         │   │            │         │   │            │         │
   │     telemetry pump   │   │     telemetry pump   │   │     telemetry pump   │
   │            │         │   │            │         │   │            │         │
   └────────────┼─────────┘   └────────────┼─────────┘   └────────────┼─────────┘
                │                          │                          │
                │       HTTPS + Bearer token (per-agent)               │
                └──────────────────────────┼──────────────────────────┘
                                           ▼
                                ┌─────────────────────────────┐
                                │  sentinel-server            │  ← one binary
                                │                             │     in your VPC
                                │   POST /agent/v1/events     │
                                │   GET  /agent/v1/policy     │
                                │   GET  /api/{stats,...}     │
                                │   GET  /                    │
                                │                             │
                                │   ┌─────────────────────┐   │
                                │   │  SQLite             │   │
                                │   │   agents            │   │
                                │   │   events            │   │
                                │   │   sessions          │   │
                                │   │   enrollments       │   │
                                │   │   policy_revisions  │   │
                                │   └─────────────────────┘   │
                                └──────────┬──────────────────┘
                                           ▲
                                           │ browser
                                ┌──────────┴──────────┐
                                │  Security team /    │
                                │   IT admin          │
                                │   (dashboard SPA)   │
                                └─────────────────────┘
```

There is **no cloud and no SaaS**. `sentinel-server` is one binary you run in your own VPC, VPN, or on-prem network. All telemetry stays inside your trust boundary.

## Employee onboarding flow

```
   ADMIN                          CENTRAL SERVER                   EMPLOYEE
   ─────                          ──────────────                   ────────

   $ sentinel-server enroll
     create alice-laptop          ──► POST /api/enroll
                                      (admin token required)
                                  ◄── { ott, url, command }

   Send Alice this one line:
   "sentinel enroll https://central/e/ott_5f4a..."

   ─ via 1Password Share ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─►

                                                                  $ sentinel enroll
                                                                    https://central/e/ott_5f4a...
                                                                          │
                                  POST /e/ott_5f4a...           ◄─────────┘
                                  (OTT is the credential —
                                   single-use, 24h max)
                                  ──► { bearer token,
                                        agent_id, agent_name,
                                        central_url }
                                                                          │
                                                                          ▼
                                                                  ✓ exchanged token
                                                                  ✓ wrote sentinel.yaml
                                                                  ✓ wrapped Claude Desktop
                                                                  ✓ wrapped Cursor
                                                                    (.bak saved)
                                                                  "Restart your AI clients."
                                                                          │
                                                                          │
                                                                   first tool call ─┐
                                                                                    │
                                  POST /agent/v1/events           ◄─────────────────┘
                                  (bearer token authed)
                                  ──► { accepted: N }

                                  Alice's row in the Agents
                                  table flips "Last seen → 3s ago"
```

Three commands total: one on the admin's side, one URL sent over a secure channel, one command on the employee's machine. **No bearer-token-handling by hand**, **no YAML editing by the employee**, **no "find your Claude Desktop config" treasure hunt**.

For air-gapped or contractor laptops where the employee can't reach central during onboarding, the legacy bare-token flow (`sentinel-server agent create`) still works.

## Centrally-managed policy

The company sets a policy once. Every connected agent picks it up and merges it with whatever the local laptop has.

```
   ┌─────────────────────────────────────────────────────────────┐
   │  Security team in the dashboard:                            │
   │                                                             │
   │     {                                                       │
   │       "deny_paths": [                                       │
   │         "~/.ssh", "~/.aws", "/etc/", "~/.kube"              │
   │       ],                                                    │
   │       "scoring": {                                          │
   │         "approve_threshold": 20,                            │
   │         "block_threshold": 70                               │
   │       }                                                     │
   │     }                                                       │
   │                                  [ Save ]                   │
   └────────────────────┬────────────────────────────────────────┘
                        │
                        ▼ PUT /api/policy  (admin token gates this)
                ┌───────────────────┐
                │ policy_revisions   │
                │  rev #3 ◄── newest │
                │  rev #2            │
                │  rev #1            │
                └─────────┬──────────┘
                          │
                          │ GET /agent/v1/policy   (each agent polls every 5 min)
                          │ If-None-Match: <cached etag>
                          │   200 OK + body, OR
                          │   304 Not Modified
                          ▼
              ┌─────────────────────────────────────────────────┐
              │ On the agent:                                   │
              │                                                 │
              │   central deny:       ~/.ssh ~/.aws /etc/      │
              │   local deny:         ~/Downloads/temp          │
              │   merged (union):     ~/Downloads/temp,         │
              │                       ~/.ssh, ~/.aws, /etc/    │
              │                                                 │
              │   central approve_t:  20                        │
              │   local approve_t:    30                        │
              │   merged (stricter):  20                        │
              │                                                 │
              │   ▶ engine built with merged policy             │
              └─────────────────────────────────────────────────┘
```

**Stricter-wins rules:**

| Field              | Merge rule                            | Why                                                    |
|--------------------|----------------------------------------|--------------------------------------------------------|
| `deny_paths`        | **Union** (dedup, local order first)   | Company can add, employee can add. Neither can remove. |
| `approve_threshold` | **min** of non-zero values             | Stricter (lower) wins. Required-approval expands.      |
| `block_threshold`   | **min** of non-zero values             | Stricter (lower) wins. Hard block expands.             |
| `enabled`           | **Local only**                          | Only the laptop owner decides whether enforcement runs at all. Central can never silently disable a laptop. |

If central is unreachable, the agent uses its on-disk cache. If there's no cache either (first run, no internet), the agent runs with **local-only** policy. **The proxy never fails closed because central is down.**

## A day in the life of the security team

```
9:00am   Open the dashboard. Stats: 8,124 events / 213 last 24h / 14 blocked / 4 agents.
         │
         ▼
9:01am   The "14 BLOCKED" tile is red. Click → events filter to blocked.
         │  Top of the list: bob-laptop tried `shell/exec` with `rm -rf node_modules` at 8:47am.
         │  Click the row → full payload. Decision: legitimate. No action.
         │
9:14am   Slack: "alice's agent is asking about credentials." Open dashboard.
         │  Type "GITHUB_TOKEN" in the search box.
         │  Three events on alice-laptop in the last hour mention it.
         │  Click into the session → it was alice asking Claude to debug auth, not a leak.
         │  Reassure the reporter.
         │
10:30am  New hire onboarding for Dave.
         │  Dashboard → "+ Enroll agent" → name=dave-laptop, ttl=24h.
         │  Copy the printed `sentinel enroll ...` command, paste in chat.
         │  Done. Dave's row appears in the Agents table within 5 minutes.
         │
2:00pm   Quarterly policy review. Open the Policy section.
         │  Add `~/.config/gh/hosts.yml` to deny_paths. Save.
         │  Within 5 minutes, every connected agent's disk cache reflects it.
         │  At each laptop's next `sentinel run` restart, the rule is enforced.
         │
4:00pm   Compliance asks for "every tool call alice made on 2026-05-12."
         │  Filter by agent + payload search; export the rows.
         │  Done in 90 seconds.
```

## Benefits — company

| Concern                                             | What Sentinel gives you                                                                                |
|-----------------------------------------------------|--------------------------------------------------------------------------------------------------------|
| "We have no idea what our team's AI agents are doing." | Live fleet dashboard, every tool call recorded with payload, searchable across all agents.            |
| "We need an audit trail for SOC2 / ISO27001."          | Append-only SQLite log on a host you own. Easy to back up, easy to export.                              |
| "We have to assume a tool call could leak data."       | Per-laptop denylist + central denylist (union). Approval gate on risky calls.                         |
| "Different teams have different risk profiles."        | Centrally-managed policy distributed to every agent. Local can ADD restrictions; can't remove central. |
| "Onboarding a new engineer to AI tooling is a hassle." | One command (`sentinel enroll <url>`). Auto-detects Claude Desktop + Cursor.                          |
| "We don't want a SaaS in the loop."                    | None exists. Single binary in your VPC. SQLite on disk.                                                |
| "What if an employee leaves?"                          | One click in the dashboard revokes their token and drops their events.                                 |
| "We don't trust vendor models to gate vendor models."  | All detection, scoring, and policy logic is owned, open-source Go code. No third-party ML in the path. |

---

# Where Sentinel helps — situations matrix

| Scenario                                                                                                  | What Sentinel does                                                                                                                          |
|-----------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------|
| "My AI deleted files I didn't expect."                                                                    | Find the session in the dashboard → see the exact `delete_file` call and its arguments. Add `~/Documents/Important` to deny_paths.            |
| "Did my AI try to exfiltrate a secret?"                                                                   | Payload search: type `q=AWS_SECRET` in the activity table. Every event mentioning it across the fleet, instantly.                              |
| "Block dangerous tools fleet-wide."                                                                       | Add the tool name to central policy → every connected agent enforces it after the next restart.                                                |
| "Approve risky operations before they run."                                                               | Set `approve_threshold` lower; risky calls land in the approval queue with a "Approve / Deny" button.                                          |
| "Someone's agent is hitting the wrong internal API."                                                      | Filter the activity table by `upstream=internal-api` and `agent=name`. Drill down to that specific session.                                    |
| "We need to prove an employee's actions for compliance."                                                  | Filter by agent + date range. Every row is timestamped; the SQLite file is the source of truth.                                                |
| "A consultant's contract ends today."                                                                     | Revoke their agent. Their token stops working, their events drop. No lingering access.                                                         |
| "We want a policy where shell commands always require approval."                                          | Add `shell/exec` to a rule with score=+50, action=`require_approval`. Distribute via central. Every agent picks it up.                         |
| "Prompt-injection in a tool description tried to make Claude read my SSH key."                            | The built-in policy engine flags string arguments touching `~/.ssh`. The call gets a high risk score and is blocked or queued for approval.   |
| "Our AI agents call OpenAI's models too. We want them governed too."                                      | If they speak MCP, they go through `sentinel`. Same audit, same denylist, same dashboard.                                                     |
| "We're piloting AI agents and need to show leadership it's safe."                                         | The dashboard is the artifact. "Here are the 8,124 tool calls over the last month, with 14 blocks. Each one is recorded."                      |

---

# Benefits by audience

```
┌─────────────────────────────────────┐   ┌─────────────────────────────────────┐
│  👤 INDIVIDUAL DEVELOPER             │   │  👥 ENGINEERING TEAM                 │
├─────────────────────────────────────┤   ├─────────────────────────────────────┤
│ • See what your AI just did          │   │ • One audit log for the whole team   │
│ • Block tools you don't trust         │   │ • One policy applies to everyone     │
│ • Approval gate on risky operations  │   │ • Onboard new engineers in 30 secs   │
│ • Local. Free. OSS. No cloud.        │   │ • Replace "trust" with "verify"      │
│ • One YAML for all your AI clients   │   │ • Shared dashboard for incidents     │
└─────────────────────────────────────┘   └─────────────────────────────────────┘

┌─────────────────────────────────────┐   ┌─────────────────────────────────────┐
│  🛡 SECURITY / COMPLIANCE             │   │  💼 ENGINEERING LEADERSHIP           │
├─────────────────────────────────────┤   ├─────────────────────────────────────┤
│ • Immutable record of every action   │   │ • "Is our AI usage safe?" — answered │
│ • Centrally-managed denylist + rules │   │ • Fleet-wide visibility               │
│ • Token rotation / revocation         │   │ • Demonstrable due-diligence story    │
│ • Exportable for SOC2 / ISO27001     │   │ • Block-and-approve before incidents  │
│ • All data in YOUR network          │   │ • Zero SaaS in the loop                │
│ • IP-owned: no vendor ML in the path │   │ • One binary, one port, one SQLite   │
└─────────────────────────────────────┘   └─────────────────────────────────────┘
```

---

# What's shipped today vs roadmap

A non-aspirational maturity snapshot — every checked item is in `main` with tests.

## v0.1 — the local laptop story ✅ shipped

- [x] Stdio MCP proxy with per-server config
- [x] Immutable SQLite audit log
- [x] Default denylist + custom deny_paths
- [x] Risk scoring + approval gate (`require_approval`)
- [x] "Remember my choice" auto-decisions
- [x] Local dashboard (`http://127.0.0.1:7842`) — read-only SPA over the audit DB
- [x] HTTP/SSE upstream transport (hosted MCP servers)
- [x] Cross-compiled releases (macOS / Windows / Linux × amd64 / arm64)

## v0.2 — the fleet story ✅ shipped

- [x] Self-hosted central server (`sentinel-server`)
- [x] Agent telemetry pump (resumable, never loses events)
- [x] Multi-agent dashboard SPA (`https://central/...`)
- [x] Per-agent drill-down with shareable URL hashes
- [x] Per-session grouping
- [x] Payload search across the fleet
- [x] Zero-touch enrollment (`sentinel enroll <url>`)
- [x] Auto-detect Claude Desktop **and** Cursor configs
- [x] `+ Enroll agent` button in the dashboard
- [x] **Centrally-distributed policy with stricter-wins merge**

## v0.2.7+ — small follow-ups (planned)

- [ ] `0.2.7.1`: dynamic engine reload (drop the "restart to pick up policy" footnote)
- [ ] `0.2.6`: Slack / webhook alerts on policy blocks
- [ ] `0.2.8`: per-agent / per-group policies (engineering vs contractors)
- [ ] `0.2.9`: pagination on `/api/events` for huge fleets

## v0.3 — the moat 🟡 planned

- [ ] In-house behavioral anomaly detector trained on collected telemetry — the first paid tier. Detects deviations from each agent's normal tool-call patterns without rules.

## v1.0 — enterprise 🟡 planned

- [ ] SSO / SAML on the dashboard
- [ ] RBAC for the admin surface
- [ ] On-prem hardened distribution
- [ ] Sandboxed execution for the riskiest tool categories

---

## TL;DR for each audience

> **Developers:** One command (`sentinel init --wrap-claude`). Now you can see what Claude / Cursor / Cline are actually doing, and stop them when they try something dumb. Free, OSS, local.

> **Companies:** One server in your VPC, one bearer token per laptop, one policy distributed to all of them. Auditable, governable AI tool use without a SaaS in the loop. No vendor ML in the detection path — your data trains your model if you ever want one.

> **Security / compliance:** Append-only SQLite log of every AI tool call. Centrally-managed denylist. Single-use enrollment URLs (no bare-token-sharing). Revocable agents. All on hardware you own.
