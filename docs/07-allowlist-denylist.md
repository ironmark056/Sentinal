# 07 — Allow/Deny Lists (Slice 3)

> Status: Slice 3 implemented. `policy.deny_paths` is live as a global setting. Per-server policy overrides and `allow_paths` (root-restricted access) land in slice 4.

## Two different "lists" in Sentinel

Don't confuse these. They protect different things and they live in different config sections.

| What | Config block | What it controls |
|------|--------------|------------------|
| **Env allowlist / denylist** | `defaults.env`, `servers.X.env` | Which environment variables reach the upstream subprocess (PATH, GITHUB_TOKEN, etc.). See [[04-config-schema]]. |
| **Path denylist** | `policy.deny_paths` | Which path-shaped arguments inside *tool calls* get blocked by the policy engine. This doc. |

The env lists run at subprocess spawn time, exactly once per session. The path denylist runs per-`tools/call`, on every request, inside the policy engine.

## What `policy.deny_paths` does

A list of substring or glob patterns. Any string argument inside a `tools/call` whose value contains a match is treated as a `user-deny-path` finding at High severity, which triggers a block.

```yaml
policy:
  enabled: true               # default true; set false to disable engine
  deny_paths:
    - ~/secrets
    - ~/Downloads/private/**
    - /etc/internal-only
```

These are added on top of the built-in sensitive-path rules (`~/.ssh`, `~/.aws`, `/etc/passwd`, etc. — see [[05-pattern-detection]]). You cannot disable the built-ins from this list. The built-ins are intentionally a fixed floor: any user who finds them too aggressive should be running `policy.enabled: false` entirely, not silently weakening the security baseline.

## Matching rules

For each pattern:

1. **Tilde expansion.** Leading `~/` or `~\` is replaced with the user's home directory at engine construction time using `os.UserHomeDir()`. So `~/secrets` becomes `/Users/alice/secrets` on macOS, `C:\Users\Alice\secrets` on Windows.
2. **Normalization.** Backslashes become forward slashes; the result is lowercased. Both the pattern and the candidate are normalized the same way.
3. **Substring or glob.** If the pattern contains `*`, `?`, or `[`, it is matched as a glob (using `filepath.Match`) against the candidate full path and basename. Otherwise it is matched as a substring.

```yaml
deny_paths:
  - ~/.config/myapp          # substring → matches any path containing ".config/myapp"
  - ~/Documents/*.key        # glob → matches "Documents/foo.key" by basename
  - /var/log/secret.log      # substring → exact-ish match
```

## What patterns are checked

The policy engine walks the entire `params` JSON tree of every `tools/call` and evaluates every string value. So a deny path matches whether the suspect path appears in:

- `params.arguments.path` (filesystem-style servers)
- `params.arguments.url` (HTTP-style servers)
- `params.arguments.files[3].path` (deeply nested arguments)
- `params.arguments.body` (request bodies that contain the path as text)

That last one matters: if an agent is about to POST `/etc/passwd` as the body of an HTTP request, we catch it even though the body field is just a string of arbitrary text.

## What `deny_paths` does *not* check

- **It does not check tool *outputs*.** A response from the upstream containing a sensitive path is not blocked. This is an intentional v0.1 limitation. Output scanning is a feature for slice 4+ once we have a clearer picture of legitimate large-response patterns.
- **It does not check upstream-server filesystem access directly.** Sentinel sits at the protocol layer. If a server internally `open()`s a sensitive file without exposing the path through the tool call interface, Sentinel cannot see it. The mitigation is OS-level sandboxing of the upstream, which is a v1.0 enterprise feature.
- **It does not implicitly cover patterns *similar* to deny entries.** `deny_paths: [~/.ssh]` blocks `~/.ssh/id_rsa` but not `~/MySshBackup/id_rsa`. The built-in `ssh-secrets` rule catches `id_rsa` regardless of directory, but a user-written `deny_paths` list is taken literally.

## Why no `allow_paths` yet

A natural complement to `deny_paths` is `allow_paths`: an allowlist of directory roots, where any path argument not under one of those roots is blocked. This is the right model for a filesystem server scoped to a project directory.

It is **deferred to slice 4** because to be useful, `allow_paths` needs to be per-server, not global. A global allow path list either blocks everything (because the github server's "path" arguments are repo paths, not filesystem paths) or allows everything (because the union of every server's allowed roots is enormous).

Per-server policy blocks are the slice 4 unlock. After that, the schema will look something like:

```yaml
# slice 4 — not yet implemented
servers:
  filesystem:
    command: npx
    args: ...
    policy:
      allow_paths:
        - ~/Documents
        - ~/Projects
      deny_paths:
        - ~/Documents/financial
```

And the engine will resolve per-server policy with the global `policy` as a fallback.

## Disabling the engine

If for some reason the policy engine is causing more pain than value (debugging, an MCP server that legitimately needs sensitive access and you accept the risk), it can be turned off entirely:

```yaml
policy:
  enabled: false
```

This disables *all* rules, not just `deny_paths`. With the engine disabled the proxy degrades to slice-1 behavior: pass-through with audit logging only. The audit log still records everything; the visibility benefit is preserved.

The CLI flag `--policy=off` (slice 4) will offer a more granular override per invocation.

## Recommended starter `deny_paths`

The starter config written by `sentinel init` does not seed `deny_paths` — the built-in sensitive-path rules already cover most of what people would put there. Add custom entries only when:

- You have project-specific secret directories the built-ins do not know about (`~/Documents/work-secrets`, `~/projects/foo/.env-prod`).
- You want to forbid an agent from touching specific dotfiles or app data even though they are not credentials per se.
- You are operating a hardened environment with a known list of off-limits paths.

A representative slice 3 config:

```yaml
version: "1"

policy:
  deny_paths:
    - ~/Documents/work-secrets
    - ~/projects/**/secrets.yml
    - ~/projects/**/*.pem
    - ~/.env*

servers:
  filesystem:
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "~/Documents"]
    env:
      allow: [NODE_PATH]
```

## Testing

The slice 3 test suite for this feature lives in `internal/policy/engine_test.go`:

- `TestEngine_UserDenyPath` — confirms a configured deny path triggers a block with the `user-deny-path` rule ID.

The pattern-detection doc covers the built-in path rules — see [[05-pattern-detection]] for the full sensitive-path list.

## Related docs

- [[04-config-schema]] — the `defaults.env` / `servers.X.env` allowlist (different feature, same word)
- [[05-pattern-detection]] — built-in sensitive-path rules (the floor that `deny_paths` extends)
- [[03-proxy-design]] — where the block decision is enforced (`proxy.shouldBlock` in `internal/proxy/proxy.go`)
- [[06-risk-scoring]] — slice 4, will let you set `allow_paths` per server and replace the binary block with risk thresholds
