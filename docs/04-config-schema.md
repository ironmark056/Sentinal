# 04 — Config Schema (Slice 2)

> Status: Slice 2 implemented. `sentinel.yaml` parser + multi-server config + env stripping shipped. `sentinel init` writes a starter template. Subprocess restart-on-crash deferred to a future daemon mode — explained below.

## Why a config file at all

Slice 1 took the upstream command on the CLI:

```bash
sentinel run -- npx -y @modelcontextprotocol/server-everything
```

That's fine for kicking the tires but not for the real use case. In production, every MCP server has:

- A specific command and args (`npx`, `uvx`, `python -m`, a local binary).
- A specific environment it's allowed to see (e.g. the GitHub server needs `GITHUB_TOKEN`; the filesystem server doesn't).
- A name the user remembers it by.

Putting that in YAML — once — and selecting by name on the CLI is much closer to how Claude Desktop's own `claude_desktop_config.json` works. The AI client points at `sentinel run --server github` and forgets about the underlying command.

## File location

| OS | Default path |
|----|--------------|
| Linux / macOS | `~/.sentinel/sentinel.yaml` |
| Windows | `%APPDATA%\sentinel\sentinel.yaml` |

Override with `--config <path>` on the CLI.

`sentinel init` writes a starter file at the default path (or to `--path <path>`). It refuses to overwrite an existing file unless `--force` is set.

## Schema

```yaml
version: "1"

audit:
  path: ~/.sentinel/audit.db          # optional; defaults are platform-specific

defaults:
  env:
    allow_system: true                # default true
    allow: []                         # extra var names (in addition to system)
    deny: []                          # var names to always strip

servers:
  filesystem:                         # name used by --server
    command: npx
    args:
      - "-y"
      - "@modelcontextprotocol/server-filesystem"
      - "~/Documents"
    env:
      allow:
        - NODE_PATH

  github:
    command: npx
    args:
      - "-y"
      - "@modelcontextprotocol/server-github"
    env:
      allow:
        - GITHUB_TOKEN
        - NODE_PATH
```

### Top-level fields

| Field | Required | Notes |
|-------|----------|-------|
| `version` | optional | If present must equal `"1"`. Configs from a future major version are rejected up front. |
| `audit.path` | optional | Overrides the audit DB location. |
| `defaults` | optional | Settings inherited by every server. |
| `servers` | **required** | Map of server name → server config. Names must match `[a-zA-Z0-9_-]+`. Empty `servers` is rejected. |

### Per-server fields

Exactly one transport must be set: `command` (local stdio subprocess) or `url` (remote Streamable HTTP server).

| Field | Required | Notes |
|-------|----------|-------|
| `command` | one of | Executable to spawn for a stdio upstream. |
| `args` | optional | List of arguments to `command`. |
| `env` | optional | Env policy; merges with `defaults.env`. **stdio-only** — has no effect on HTTP upstreams. |
| `url` | one of | `http://` or `https://` endpoint for a remote MCP server (Streamable HTTP). |
| `headers` | optional | Flat string-to-string map of headers to send on every request (typically auth tokens). HTTP-only. |

Set exactly one of `command` and `url`. The parser rejects configs that set both, or neither.

#### Example: remote HTTP server with bearer auth

```yaml
servers:
  remote_search:
    url: https://api.example.com/mcp
    headers:
      Authorization: "Bearer ${TOKEN}"
      X-Org-Id: "my-org"
```

The bearer token here is written literally; environment-variable interpolation in YAML is slice 7 polish, not slice 6. For now, paste the token (and add the file to `.gitignore`).

### Env policy

The env block is the security teeth of slice 2. Defaults are deliberately strict: every upstream sees only the OS-required system variables, unless you explicitly allow more.

```yaml
env:
  allow_system: true       # include the built-in system allowlist (default: true)
  allow:                   # extra var names to pass through
    - GITHUB_TOKEN
    - NODE_PATH
  deny:                    # always strip these, even if otherwise allowed
    - SUDO_ASKPASS
```

**Rule precedence** (highest to lowest):

1. `deny` — wins over everything.
2. `allow` — passes the named var.
3. System allowlist (when `allow_system: true`) — passes OS-required vars.
4. Default — strip.

**Merging `defaults.env` with a server's `env`:**

- `allow_system`: server value wins if set, otherwise the default's value, otherwise `true`.
- `allow` and `deny`: concatenated (server entries appended to defaults).

### System allowlist (built-in)

These are passed through automatically when `allow_system: true`. They're OS-required for almost any program to run.

| Linux / macOS | Windows |
|---------------|---------|
| `PATH` | `PATH`, `PATHEXT` |
| `HOME` | `USERPROFILE`, `HOMEDRIVE`, `HOMEPATH` |
| `USER`, `LOGNAME` | `USERNAME`, `USERDOMAIN`, `COMPUTERNAME` |
| `SHELL` | `COMSPEC` |
| `LANG`, `LC_ALL`, `LC_CTYPE`, `TZ` | (locale via system) |
| `TMPDIR`, `TERM`, `PWD` | `TEMP`, `TMP` |
|   | `APPDATA`, `LOCALAPPDATA`, `PROGRAMDATA` |
|   | `SYSTEMROOT`, `SYSTEMDRIVE`, `WINDIR` |
|   | `PROCESSOR_ARCHITECTURE`, `NUMBER_OF_PROCESSORS`, `OS` |

Intentionally **not** in this list (must be added with `env.allow`):

- Anything containing `TOKEN`, `KEY`, `SECRET`, `PASSWORD`, `PASSWD`.
- Tool-specific vars: `NODE_PATH`, `NODE_OPTIONS`, `NPM_*`, `PYTHONPATH`, `VIRTUAL_ENV`, etc.
- Cloud SDK vars: `AWS_*`, `GOOGLE_*`, `AZURE_*`, `GITHUB_TOKEN`, `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`.

If your MCP server needs one of these (most servers that authenticate to something do), list it under `env.allow` for that server only.

### Windows case-insensitive matching

Windows env var names are case-insensitive at the OS level (`Path` and `PATH` refer to the same variable). The proxy normalizes names to uppercase before comparing on Windows, so this works:

```yaml
env:
  allow:
    - github_token       # matches GITHUB_TOKEN
```

On POSIX, comparison is exact.

## CLI surface (slice 2)

```
sentinel init [--path PATH] [--force]
sentinel run --server NAME [--config PATH] [--audit PATH]
sentinel run -- <upstream-command> [args...]      # inline mode
sentinel version
sentinel help
```

### Inline mode

For ad-hoc testing without writing a config:

```bash
sentinel run -- npx -y @modelcontextprotocol/server-everything
```

**Inline mode does not apply env policy.** The upstream inherits the proxy's full environment, including secrets. This is by design — inline mode is for "I'm debugging a server right now," not for production. The mode is reported as "inline" in the audit log so you can tell at a glance.

## Validation

The parser uses strict-fields mode. Typos are rejected up front:

```yaml
servers:
  echo:
    command: /bin/echo
    bogus_field: oops      # ← error: unknown field
```

This is intentional. Silently ignoring unknown fields invites copy-paste errors that produce policy outcomes the user didn't ask for. We'd rather fail loudly.

## What is *not* in slice 2

### Subprocess restart-on-crash — deferred to daemon mode

The slice 2 task description mentioned "subprocess supervision with restarts." Building this **would not actually help any real user yet**, and here's why:

In the v0.1 usage pattern, the AI client (Claude Desktop, Cursor, etc.) spawns one `sentinel` process per upstream, and the proxy's lifecycle is bound to the client's session. If the upstream subprocess crashes:

- The client's MCP session is already in an unrecoverable state — it has session state (`initialize` results, tool schemas) tied to the now-dead upstream.
- Restarting the upstream silently would hand the client a server with no memory of `initialize`. Bad outcomes follow.
- Restarting requires the proxy to be a long-lived daemon that outlives any single client connection. That's a different deployment model.

The right home for restart logic is a future **daemon mode** where:
- `sentinel daemon` runs as a long-lived service.
- Multiple AI clients connect to it over a local Unix socket / named pipe.
- Upstream subprocesses are pooled and supervised independently of any single client.

That's a bigger architectural move and belongs in v0.2 or later. Adding half-of-it to slice 2 would be dead code.

The decision is documented here rather than silently dropped so future contributors don't re-add it without thinking through the lifecycle.

### Auto-import from Claude Desktop config

`sentinel init --import claude-desktop` (read Claude's config and auto-generate `sentinel.yaml`) is a great UX feature but lives in slice 7. It needs file-format-detection across Claude versions and OSes, which is not a slice 2 concern.

## Testing

| Test | What it pins |
|------|--------------|
| `TestParse_Minimal` | A minimum-viable config parses |
| `TestParse_RejectsUnknownFields` | Typos error out, do not silently pass |
| `TestParse_RejectsBadVersion` | Future-version configs are rejected |
| `TestParse_RequiresAtLeastOneServer` | Empty `servers` is an error |
| `TestParse_ServerNameValidation` | Bad names are rejected |
| `TestServer_MergesDefaultsAndOverrides` | `defaults.env.allow` merges with `servers.X.env.allow` |
| `TestFilterEnv_SystemDefaultsIncluded` | OS-required vars pass, `AWS_*` does not |
| `TestFilterEnv_AllowExtra` | `env.allow` adds named vars |
| `TestFilterEnv_DenyWinsOverAllow` | `env.deny` always wins |
| `TestFilterEnv_AllowSystemFalse` | Setting `allow_system: false` drops the OS defaults |
| `TestStarter_ParsesAsValidWhenServerAdded` | The `init` template plus one server parses cleanly |

The proxy's existing `TestProxy_RoundTrip` integration test continues to pass and now also exercises the new `Upstream.Env` field path (with `Env: nil`, which preserves slice 1 behavior).

## How slice 3 will build on this

Slice 3 introduces the pattern detection library and a default denylist of sensitive filesystem paths (`~/.ssh`, `~/.aws`, etc.). Nothing in slice 2's config schema changes — slice 3 just adds a new top-level `policy:` block that the parser will start accepting and that the proxy's hot path will start consulting between schema validation and forwarding to the upstream.

## Related docs

- [[03-proxy-design]] — proxy internals; `Upstream.Env` is now driven by the config
- [[01-architecture]] — where this layer sits
- [[05-pattern-detection]] — coming in slice 3, will read additional config blocks defined here
- [[07-allowlist-denylist]] — coming in slice 3, also extends the config schema
