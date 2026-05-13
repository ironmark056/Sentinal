# 06 — Risk Scoring (Slice 4)

> Status: Slice 4 implemented. The policy engine now returns a 0-100 risk score and a three-way decision: allow / require approval / block.

## Why a score instead of a binary

Slice 3 was binary: any High-severity finding → block, anything below → allow. That collapses two distinct user intents into one outcome:

- "This is definitely bad; never let it through." → **block**
- "This is risky enough that I want to look at it before it runs." → **approve manually**

Real production traffic has both. A single SSH path access is suspicious but not always malicious (the user might genuinely be debugging their own keys). A `rm -rf /tmp/foo` from a shell tool is suspicious in a different way. The risk-score model gives the user a knob: at what severity should I be asked vs. just stopped?

## The numbers

```go
SeverityWeight:
    SevCritical = 90
    SevHigh     = 50
    SevMedium   = 25
    SevLow      = 10
    SevInfo     = 0
```

A call's risk score is the sum of finding weights, capped at 100.

```go
DefaultThresholds:
    ApproveThreshold = 30   //  < 30  → allow
    BlockThreshold   = 80   //  >= 80 → block
                             //  30..79 → require approval
```

So under defaults:

| Findings | Score | Decision |
|---|---|---|
| Nothing | 0 | allow |
| One Low (e.g. unusual-format-chars) | 10 | allow |
| One Medium (e.g. triple-dot) | 25 | allow |
| One High (e.g. SSH path access) | 50 | **approve** |
| Two Highs (path traversal + sensitive path) | 100 | **block** |
| One Critical (e.g. `rm -rf`) | 90 | **block** |
| Critical + High | 100 | **block** |

This matches intuition. Critical findings always block (they alone exceed the threshold). Single High findings ask the human. Stacked findings escalate to block.

## Configuration

```yaml
policy:
  scoring:
    approve_threshold: 30
    block_threshold: 80
    approval_timeout_seconds: 60
```

All three are optional. Zero / missing → defaults.

A few useful overrides:

```yaml
# Paranoid mode: ask about anything above noise.
scoring:
  approve_threshold: 1
  block_threshold: 60

# Permissive dev mode: only block obvious badness.
scoring:
  approve_threshold: 80
  block_threshold: 100
```

`sentinel run --server foo` reads these from `sentinel.yaml`. The CLI does not (yet) expose thresholds as flags — config-driven is the right surface because thresholds are policy, not invocation parameters.

## Why these specific weights

The weights are not arbitrary, but they are calibrated against the goal of "what should happen given the *typical* attack patterns in the slice 3 rule catalog." A different rule catalog (or a different deployment context) may need a different scale.

- **Critical = 90** because a single Critical finding should *always* block under any reasonable threshold. 90 ≥ default block threshold (80).
- **High = 50** because two Highs stack to 100 (always block), one High alone needs approval, and one High plus one Low (60) crosses into approve.
- **Medium = 25** because four Mediums (100) block; two Mediums (50) require approval; one Medium (25) is right at the approve threshold.
- **Low = 10** is below the approve threshold individually; three Lows (30) cross into approve.
- **Info = 0** never moves the needle; included for finding metadata only.

The geometry: severity steps are not equal, they roughly double. That's because the relationship between severity classes in this catalog is qualitative — Critical isn't 4x worse than Low, it's "absolutely yes" vs. "barely worth mentioning."

## What's not yet weighted

- **Per-rule overrides.** A given rule cannot say "weight me at 70 even though I'm tagged High." All weighting goes through the severity tier. This is intentional for v0.1 simplicity; per-rule weights become valuable once we have telemetry showing which rules are too eager / too quiet.
- **Per-tool context.** A `read` of `/etc/passwd` and a `write` to `/etc/passwd` would both score the same right now (the rule fires on the path, not the operation). The right place to add tool-level context is per-server policy in slice 4.x or slice 7 polish.
- **Per-finding location context.** A finding in `params.arguments.path` and a finding in `params.arguments.body` count the same. A path-shaped string in a body field is probably more suspicious than the user typing it as a path. Could be a future Multiplier.

These are all "good ideas for v0.3+ once we have the dataset" rather than "missing features for v0.1."

## The Decision type

```go
type Decision int

const (
    DecisionAllow   Decision = iota
    DecisionApprove                // suspend for human approval
    DecisionBlock
)
```

The proxy's `evaluate` method translates this into a `verdict` (`verdictAllow | verdictBlock`) after running the approval flow. `DecisionApprove` is not a final outcome — it's a request to ask a human and then act on their answer. See [[08-approval-flow]] for what happens next.

## Tested

| Test | Pins |
|---|---|
| `TestEngine_RiskScoreFromFindings` | A single High = 50 = Approve |
| `TestEngine_TwoHighFindingsTriggersBlock` | Stacked Highs reach Block |
| `TestEngine_CriticalAlwaysBlocks` | Critical alone exceeds default Block |
| `TestEngine_CustomThresholds` | `WithThresholds` overrides defaults |
| `TestEngine_ApprovesSshAccess` | SSH path now Approve, not Block |
| `TestEngine_FlagsEncodedTraversal` | Encoded traversal alone is Approve |
| `TestEngine_FlagsPromptInjection` | Single prompt-injection High is Approve |
| `TestEngine_UserDenyPath_RequiresApproval` | User-deny-paths land in Approve under defaults |

The full suite is at 16 engine tests, all green.

## Related docs

- [[05-pattern-detection]] — what the findings are and what severity each rule carries
- [[08-approval-flow]] — what happens when Decision is `Approve`
- [[04-config-schema]] — `policy.scoring.*` config fields
- [[03-proxy-design]] — `evaluate()` lives in `internal/proxy/proxy.go`
