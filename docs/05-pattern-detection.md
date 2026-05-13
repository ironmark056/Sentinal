# 05 — Pattern Detection (Slice 3)

> Status: Slice 3 implemented. Regex- and heuristic-based detection wired into the proxy hot path. Tools/call requests that trigger high-severity rules get a JSON-RPC error response instead of being forwarded upstream.

## What this is, and what it is not

This is the deterministic, code-owned detection layer. Every rule is a Go function or regular expression in `internal/policy/patterns.go`. There are no third-party model integrations, no remote API calls, no embeddings. Detection runs entirely in-process with predictable latency (sub-millisecond for the full rule set on a typical tools/call payload).

This is intentionally narrow. ML-based detection — trained on the telemetry Sentinel collects from opt-in installs — arrives in v0.3. The current rules catch the obvious 80%; the ML layer will catch the rest. Shipping the dumb version first means we collect the data that makes the smart version possible.

## How the engine fits

```
Client → Proxy → JSON-RPC parse → audit log → POLICY ENGINE → forward / block → Upstream
                                                    │
                                                    ▼
                                   walks every string in params
                                   runs every rule against each
                                   if any finding has severity ≥ High → block
```

Concretely, `policy.Engine.Evaluate(method, toolName, params)` returns a `Result{Decision, Findings, BlockReason}`. `proxy.shouldBlock` calls into this for every `tools/call` and either forwards the message or synthesizes a JSON-RPC error response (`code: -32000, message: "blocked by sentinel policy: ..."`) back to the client.

## Rule categories

Six categories ship in slice 3. Every category contributes one or more rules.

### 1. Sensitive paths (`sensitive-path`)

The largest category. Substring matching against the lowercased, forward-slash-normalized argument value. Catches both POSIX (`/home/x/.ssh/id_rsa`) and Windows (`C:\Users\x\.ssh\id_rsa`) forms.

| Rule ID | Severity | What it blocks |
|---------|----------|----------------|
| `ssh-secrets` | High | `/.ssh/`, `id_rsa`, `authorized_keys`, `known_hosts` |
| `aws-credentials` | High | `~/.aws/credentials`, `~/.aws/config` |
| `cloud-credentials` | High | `gcloud`, `.azure`, `.kube/config` |
| `system-secrets` | High | `/etc/passwd`, `/etc/shadow`, `SAM` registry, etc. |
| `package-credentials` | High | `.npmrc`, `.pypirc`, `.netrc`, Docker config |
| `shell-history` | High | `.bash_history`, `.zsh_history`, `psql_history` (often contains pasted secrets) |
| `browser-storage` | High | Chrome `Login Data`, Firefox `cookies.sqlite`, `key3.db` |
| `git-credentials` | High | `.git-credentials` |

These rules favor recall over precision — they will sometimes fire on legitimate calls. The user has two escape hatches: explicit per-server allowlists (slice 4) and the audit log so a blocked-by-mistake call is at least visible. The cost of an over-eager block is one tool call retry; the cost of a missed exfiltration is your credentials.

### 2. Path traversal (`path-traversal`)

| Rule ID | Severity | Pattern |
|---------|----------|---------|
| `dot-slash` | High | `../` or `..\` as a path component |
| `encoded` | High | `%2e%2e`, `%252e%252e`, `..%2f`, `..%5c` |
| `triple-dot` | Medium | `.../`, `....`, exploits in some parsers |

The encoded variants matter because some MCP servers pass arguments through URL decoders that an attacker may exploit to bypass naive path checks.

### 3. Command injection (`command-injection`)

| Rule ID | Severity | Pattern |
|---------|----------|---------|
| `shell-metachars` | High | Backticks, `$()`, command separators (`;`, `&&`, `\|\|`) followed by a word, redirects to/from `/dev/` |
| `dangerous-command` | Critical | `rm -rf`, `dd if=`, `mkfs`, fork bombs `:(){`, `chmod 777`, `curl ... \| sh`, `wget ... \| bash` |

Note these fire on *argument values* not on the tool name. A tool called `shell.exec` receiving `command: "ls /tmp"` is fine; receiving `command: "rm -rf /"` is critical.

### 4. Unicode smuggling (`unicode-smuggling`)

This category is the one most security tooling misses. Attackers embed instructions in Unicode code points that render invisibly to humans but are passed to the model.

| Rule ID | Severity | What it catches |
|---------|----------|-----------------|
| `tag-chars` | Critical | `U+E0000`–`U+E007F` — the Unicode "tag" block, used in published prompt-injection PoCs to encode hidden instructions |
| `rtl-override` | High | Bidirectional override chars (`U+202A`–`U+202E`, `U+2066`–`U+2069`) used to reverse displayed text |
| `zero-width` | Medium | Three or more zero-width spaces (`U+200B`, `U+200C`, `U+200D`, `U+2060`, `U+FEFF`) — single ones are common, runs are suspicious |
| `unusual-format-chars` | Low | More than 10% of the string is Unicode category `Cf` (format) — generic catch-all |

Tag chars in particular are nearly impossible to encounter in legitimate text. Their presence is a near-certain signal of intent to smuggle.

### 5. Prompt injection (`prompt-injection`)

A single regex covering the most common phrases:

- "ignore all previous instructions"
- "disregard the system prompt"
- "forget everything above"
- "new system prompt:"
- "you are now an [X] without restrictions"
- "reveal your system prompt"
- "developer mode enabled"
- "DAN mode" / "jailbreak mode"

Severity: High.

This is the weakest category because the attack surface is genuinely natural-language and a regex cannot enumerate it. It catches lazy attackers and gives a baseline. The ML detector in v0.3 owns this category properly.

### 6. Secret-like strings (`secret-like`)

Recognizes recognizable token shapes appearing in arguments — a sign that the agent is about to *exfiltrate* a secret, not access one.

| Rule ID | Severity | Pattern |
|---------|----------|---------|
| `aws-access-key` | Critical | `AKIA[0-9A-Z]{16}` |
| `github-token` | Critical | `ghp_`, `gho_`, `ghs_`, `ghu_`, `ghr_` prefix + 36+ chars |
| `slack-token` | High | `xox[abpsr]-` prefix |
| `jwt` | Medium | Three base64url segments separated by `.` starting with `eyJ` |
| `openai-key` | Critical | `sk-` followed by 20+ chars |
| `anthropic-key` | Critical | `sk-ant-` followed by api/sid digits |

When one of these appears in a tool call argument, the agent is likely about to POST a secret somewhere it should not go. That is almost always a finding worth blocking.

## Decision logic in slice 3

```go
if any finding has severity >= High:
    Decision = Block
else:
    Decision = Allow
```

Findings below High are still recorded and logged — they show up in the audit DB and in the proxy's stderr log as `FINDING(medium) ...`. They just do not block. Slice 4 replaces this binary cutoff with risk scoring and approval thresholds.

## The walker

Rules operate on *string values*. The engine walks the entire `params` JSON tree recursively and runs every rule against every string it finds.

```go
walkStrings(params, "params", func(jsonPath, value string) {
    for _, rule := range registry.Rules() {
        if rule.Match(value) {
            findings = append(findings, ...)
        }
    }
})
```

Important properties:

- **Walks arrays and nested objects.** Attacks hidden three levels deep in arguments are caught.
- **Skips numbers, bools, nulls.** Rules apply to strings only.
- **Records `JSONPath`.** Every finding remembers where in the tree it came from: `params.arguments.path`, `params.arguments.commands[2]`, etc. This is how the audit log explains *why* a call was blocked.
- **Truncates values at 256 chars** before storing in findings, so a 10 MB blob does not bloat the audit DB.

The tool *name* (`params.name`) is also evaluated, because a malicious upstream could expose tools with attack patterns literally in their names.

## What it does not yet do

- **No semantic similarity / embedding matching.** A rephrased prompt injection ("disregard whatever has been said before") slips past the regex. The ML layer in v0.3 owns this.
- **No cross-call correlation.** Each call is evaluated in isolation. "Read invoice X, then read invoice Y, then read all invoices" is harmless per-call; the *pattern* is suspicious. Behavioral detection in v0.3 owns this.
- **No per-server rule overrides.** All rules apply to all servers in slice 3. Slice 4 adds per-server policy blocks for nuance ("the shell server is allowed to use `rm -rf`, but only on `/tmp`").
- **No false-positive suppression.** A user cannot disable a specific rule yet. They can override decisions globally by setting `policy.enabled: false`, which turns the engine off entirely. Granular suppression is slice 4.
- **No tool-output scanning.** We inspect requests, not responses. Slice 4+ may add response scanning — important for catching exfiltration where the *server* returns secrets to the agent.

## Performance

For a benign `tools/call` with a typical payload (≤ 1 KB), the full rule set runs in well under a millisecond. The dominant cost is JSON parsing (already done for logging) and regex matching. Compiled regexes are package-level vars so there's no per-call compilation overhead.

We do not benchmark adversarial payloads designed to be slow; if a pathological input is ever found, the right fix is bounding the walker to a max-string-length and max-depth rather than rule-by-rule timeout handling.

## How to read findings in the audit log

Every blocked call writes two rows to the audit DB:

1. The original client request (`direction=c2s`, `msg_type=request`, full payload).
2. The synthesized error response (`direction=s2c`, `msg_type=error`, with `message="blocked by sentinel policy: [...]"`).

The proxy log on stderr also includes one line per finding above informational:

```
[sentinel] FINDING(critical) command-injection/dangerous-command: Known-dangerous shell command pattern at params.arguments.command
[sentinel] BLOCKED tools/call id=42 tool=shell.exec: [command-injection/dangerous-command] Known-dangerous shell command pattern
```

This is enough for forensic reconstruction in v0.1. The dashboard (slice 5) will turn this into a UI.

## Testing

| Test | What it pins |
|------|--------------|
| `TestEngine_AllowsBenignCall` | A clean call is allowed |
| `TestEngine_BlocksSshAccess` | Reading `~/.ssh/id_rsa` is blocked |
| `TestEngine_BlocksPathTraversal` | `../../../etc/passwd` is blocked |
| `TestEngine_BlocksEncodedTraversal` | `%2e%2e` form is blocked |
| `TestEngine_BlocksDangerousCommand` | `rm -rf` is blocked |
| `TestEngine_BlocksUnicodeTagChars` | U+E0000-range chars are blocked |
| `TestEngine_BlocksPromptInjection` | Common override phrases are blocked |
| `TestEngine_BlocksLeakingSecrets` | AWS access keys in arguments are blocked |
| `TestEngine_BlocksGitHubToken` | GitHub PATs in arguments are blocked |
| `TestEngine_UserDenyPath` | User-configured `deny_paths` work |
| `TestEngine_FindingIncludesJSONPath` | Findings record where in the tree they matched |
| `TestEngine_WalksNullSafely` | nil params do not crash |
| `TestEngine_TruncatesLongValues` | Long matched values are truncated |
| `TestProxy_BlocksSshAccess` (integration) | The proxy actually returns a JSON-RPC error to the client and does not forward |
| `TestProxy_AllowsBenignCall` (integration) | Benign calls still reach the upstream |

Total tests across the project: 40+. All pass on every commit.

## Related docs

- [[07-allowlist-denylist]] — `deny_paths` configuration and how to scope policy
- [[03-proxy-design]] — where in the proxy pipeline this fits
- [[06-risk-scoring]] — coming in slice 4, replaces the binary allow/block cutoff
- [[08-approval-flow]] — coming in slice 4, adds the "ask the human" path
