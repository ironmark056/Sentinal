# 16 — Zero-Touch Enrollment (Slice 0.2.5)

> Status: Slice 0.2.5 implemented. Employee onboarding into a company Sentinel deployment is now **one command**. Admin runs `sentinel-server enroll create alice-laptop`, sends Alice the printed `sentinel enroll <url>`. Alice runs it. Her laptop is fully wired in 30 seconds — bearer token exchanged, `sentinel.yaml` written, Claude Desktop config rewritten with backup.

## Why this slice exists

Before this slice, onboarding an employee was:

1. Admin generates a bearer token (`sentinel-server agent create`).
2. Admin sends Alice **both** the server URL and the token over a secure channel.
3. Alice writes `sentinel.yaml` by hand, getting the `central:` block format right.
4. Alice finds her Claude Desktop config (which is in a different path on each OS).
5. Alice edits the config to launch every server through `sentinel run --server X`.
6. Alice restarts Claude.

That's five places to make a typo and one place to leak a bearer token. The goal of zero-touch is to collapse all of it into a single command that any employee can paste, while keeping the security boundary at least as tight as before.

The shape borrowed from Tailscale / 1Password / WireGuard managers: a **single-use, short-lived enrollment URL** that exchanges for the real bearer token on first use. If the enrollment URL leaks, it's worthless after first use or 24h, whichever comes first.

## Wire-level flow

```
Admin                                 Server                                 Alice
────                                  ──────                                 ─────
sentinel-server enroll create
  alice-laptop                        ↦ POST  /api/enroll
                                      (admin token gates this)
                                      ↤ ott=ott_5f4a...
                                      ↤ url=https://central/e/ott_5f4a...

(admin sends URL via 1Password / chat)
                                                                              sentinel enroll
                                                                                <url>
                                                                              ↦ POST  /e/ott_5f4a...
                                                                              (no auth; the OTT IS the credential)
                                      ↤ token=mcpg_a1b2...
                                      ↤ agent_name=alice-laptop
                                      ↤ central_url=https://central
                                                                              ✓ write sentinel.yaml
                                                                              ✓ detect Claude Desktop config
                                                                              ✓ migrate each mcpServer entry
                                                                              ✓ write .bak files
                                                                              "Restart Claude Desktop."
```

The OTT-consume operation is **atomic**: in one DB transaction the server validates the OTT, creates the agent, generates the bearer token, and marks the OTT consumed. There is no window where two parallel `sentinel enroll` calls could both succeed against the same OTT.

## Security model

| Threat | Mitigation today |
|---|---|
| OTT leaks in transit | Single-use + 24h TTL. After first consume, the token is permanently dead. |
| Replay of a consumed OTT | DB transaction marks `consumed_at`; second consume returns HTTP 409 Conflict. |
| OTT survives past employee leaving | Admin runs `sentinel-server enroll revoke <id>`. Consume after revoke returns 404. |
| OTT IDs are guessable | 32 bytes of `crypto/rand` hex → 256-bit entropy, `ott_` prefix. |
| OTT logged in plaintext on the server | Only the SHA-256 hash is persisted, same pattern as the bearer token in slice 0.2.1. |
| Stranger registers as "alice-laptop" | Enrollment locks the name at creation. If an agent named `alice-laptop` already exists when consumed, the OTT returns 409 (admin must revoke first). |
| Admin sends URL on a compromised channel | Re-issue. The OTT is single-use; one extra `sentinel-server enroll create` + the old one is dead the moment the real Alice uses the new one. |

What this **doesn't** defend against (out of scope for v0.2):

- Compromised admin machine. If an attacker has the admin token, they can mint enrollment URLs for any name they want.
- Compromised employee laptop. The bearer token lives at rest in `sentinel.yaml`; same threat model as any local secrets file.

## Storage

New table:

```sql
CREATE TABLE enrollments (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    name              TEXT NOT NULL,
    ott_hash          TEXT NOT NULL UNIQUE,
    created_at        INTEGER NOT NULL,
    expires_at        INTEGER NOT NULL,
    consumed_at       INTEGER,
    resolved_agent_id INTEGER,
    metadata          TEXT
);
CREATE INDEX idx_enrollments_hash ON enrollments(ott_hash);
```

`name` is **not** unique on the enrollments table itself — an admin can have multiple outstanding OTTs for the same intended name (e.g. retry after sending the URL on a flaky channel). The uniqueness constraint is on the resulting **agent**: only one can hold a given name at any time.

## API

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/api/enroll` | admin token | Mint a new OTT, return the URL + the full command to send. |
| GET | `/api/enroll` | none | List all enrollments (admin endpoint surface, no PII risk). |
| DELETE | `/api/enroll/{id}` | admin token | Revoke an outstanding enrollment. |
| POST | `/e/{ott}` | the OTT itself | Public consume. Returns bearer token + agent identity. |
| GET | `/e/{ott}` | the OTT itself | Same as POST; tolerated for `curl` debugging. |

Status codes for consume:

| Code | Meaning |
|---|---|
| 200 | OK; body contains `{token, agent_id, agent_name, central_url}` |
| 400 | Malformed token (wrong prefix etc.) |
| 404 | Unknown OTT (either bogus or revoked) |
| 409 | Already consumed, **or** an agent with that name already exists |
| 410 | Expired |

`POST /api/enroll` accepts `{name, ttl_seconds?, metadata?}` and returns:

```json
{
  "enrollment": { "id": 7, "name": "alice-laptop", ... },
  "ott":     "ott_5f4a1d8c...",
  "url":     "https://central.acme.internal/e/ott_5f4a1d8c...",
  "command": "sentinel enroll https://central.acme.internal/e/ott_5f4a1d8c...",
  "note":    "Single-use; expires at the time shown."
}
```

The `url` field is built from the **incoming request's** scheme + host (honoring `X-Forwarded-Host` / `X-Forwarded-Proto` so a reverse proxy gets it right). If the admin uses the CLI from outside HTTP, they can pass `--base-url` to override.

## The `internal/onboard` package

The CLI side is a thin orchestrator over `onboard.Run(Options)`. The package:

1. Detects the Claude Desktop config path (`os.UserConfigDir()`-based, OS-aware).
2. Parses it as `map[string]json.RawMessage` so unknown top-level keys (`theme`, `prompts`, anything Claude adds) survive verbatim.
3. For each entry in `mcpServers`:
   - **Skip** if `command` already ends in `sentinel` and `args[0] == "run"` (idempotent re-run).
   - Otherwise: copy the original `{command, args, env}` into `sentinel.yaml` under `servers.<name>` — **unless** that name already exists in the yaml (skip-and-warn so a hand-tuned entry is never clobbered).
   - Rewrite the Claude entry to `{command: "sentinel", args: ["run", "--server", "<name>"]}`.
4. Optionally writes a `central:` block.
5. Saves `.bak.<UTC-timestamp>` of every file it overwrites.
6. Writes both files **atomically** (temp + rename), so a crash mid-write never leaves the user with a truncated config.

YAML is read/written via `yaml.Node` so existing comments, indentation, and key ordering in `sentinel.yaml` are preserved on round-trip.

## CLI

### Admin side

```
sentinel-server enroll create <name> [--meta k=v,...] [--ttl 24h] [--base-url URL]
sentinel-server enroll list [--json]
sentinel-server enroll revoke <id>
```

`--ttl` is a Go duration (`24h`, `15m`, `7d` — though for OTTs you almost always want the default 24h).

`--base-url` is what gets prefixed to `/e/<ott>` when building the URL to print. If unset, the CLI uses `https://<hostname>` as a placeholder; the admin can fix the host before sending. (In production you'd pass `--base-url https://sentinel.acme.internal`.)

### Employee side

```
sentinel enroll <enrollment-url>
sentinel enroll <enrollment-url> --no-wrap        # don't touch Claude config
sentinel enroll <enrollment-url> --config /path   # write yaml somewhere non-default
```

The default success trace:

```
✓ exchanged enrollment token (agent: alice-laptop)
✓ wrote /Users/alice/.sentinel/sentinel.yaml
✓ /Users/alice/Library/Application Support/Claude/claude_desktop_config.json
  — migrated: [filesystem github] (backup: claude_desktop_config.json.bak.2026-05-14T19-30-21Z)

Done. Restart Claude Desktop to begin reporting events.
```

If the exchange succeeds but the local config write fails for any reason (permission, disk, etc.), the bearer token is **still printed** so the user can paste it into their yaml by hand — no scenario where a successful network round trip leaves them in an unrecoverable state.

## Solo-developer `init`

The same plumbing powers `sentinel init --wrap-claude` for the solo case. No central server, just:

```
sentinel init --wrap-claude
```

Detects the local Claude Desktop config, transplants every MCP server entry into a fresh `sentinel.yaml`, and rewrites Claude to launch each through `sentinel run`. Idempotent on re-run.

## Tests

`internal/server` (slice 0.2.5 additions):

- `TestEnrollment_CreateThenConsume` — full round trip including using the resulting bearer token against `/agent/v1/health`.
- `TestEnrollment_ConsumeTwiceFails` — second consume returns 409.
- `TestEnrollment_ExpiredFails` — past `expires_at` returns 410.
- `TestEnrollment_UnknownTokenIs404`.
- `TestEnrollment_NameCollisionIs409` — agent with the same name already exists.
- `TestEnrollment_AdminCreateAndRevoke` — admin endpoint requires admin token, revoke + subsequent consume = 404.

`internal/onboard`:

- `TestRun_FreshLaptopMigratesAllServers` — preserves unrelated top-level keys in the Claude config (`theme: dark`), transplants the original commands into `sentinel.yaml`, rewrites Claude to launch `sentinel run --server X` for each entry.
- `TestRun_TwiceIsIdempotent` — second run reports 0 migrations + 1 already-wrapped.
- `TestRun_WritesCentralBlock` — `central:` block lands in `sentinel.yaml` with url/token/agent_name.
- `TestRun_NoClaudeConfigStillWritesAgentYaml` — missing client config is a warn, not an error.
- `TestRun_DoesNotOverwriteExistingAgentServer` — hand-tuned `sentinel.yaml` entries are preserved.
- `TestRun_DryRunWritesNothing` — `--dry-run` produces a plan without touching disk.

Full project: 9 packages green (8 + new `internal/onboard`), vet clean.

## Updates in slice 0.2.5.1

Two friction points from 0.2.5 are now closed:

- **Cursor auto-detection.** `sentinel enroll` and `sentinel init --wrap-claude` now probe Cursor's MCP config locations as well as Claude Desktop's. Both files (when present) get migrated in a single pass; both get backups. Cline remains manual-only because its MCP config is workspace-scoped (per `.vscode/`) and any global "migrate Cline" would silently miss most of a developer's projects.
- **`+ Enroll agent` button in the dashboard.** The Agents section gains an Enroll button next to Add agent. The form takes name + metadata + TTL hours; on submit the SPA POSTs to `/api/enroll` and shows a copy-friendly modal with the `sentinel enroll <url>` command pre-formatted. An "Outstanding enrollments" section appears above Agents whenever any unconsumed-and-unexpired enrollment exists; each row has a Revoke button.

## What's deferred

- **Re-enrollment after key rotation.** Today an employee whose token is compromised has to be revoked + re-issued. A single-flight "rotate-my-token" flow can come once we hear it asked for.
