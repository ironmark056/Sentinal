# 03 — Proxy Design

> Status: Slice 1 (stdio passthrough), slice 4 (policy + approval) and slice 6 (HTTP upstream) all wired in. This doc covers what exists today, with the slice 1 narrative kept inline where it still applies.

## What slice 1 actually delivers

A working bidirectional stdio proxy:

- Reads MCP JSON-RPC envelopes from `stdin` (the AI client side).
- Spawns a configured MCP server as a subprocess.
- Forwards every message client→upstream and upstream→client unchanged.
- Logs every message — direction, type, method, id, payload, size — to a SQLite audit DB.
- Surfaces upstream `stderr` through the proxy's own log so users can see what their MCP server is saying.
- Exits cleanly on SIGINT / SIGTERM, draining the audit queue first.

What it does *not* do (yet): security inspection, allowlists, approvals, multi-server config, HTTP transport, dashboard. Those land in later slices.

## Package layout

```
cmd/sentinel/main.go              CLI entry point
internal/proxy/jsonrpc.go         JSON-RPC envelope types + Decode/Encode
internal/proxy/stdio.go           FrameReader / FrameWriter (NDJSON framing)
internal/proxy/proxy.go           Proxy orchestration: spawn + pump + log
internal/audit/audit.go           SQLite audit log, buffered batch writer
testdata/echomcp/main.go          Trivial MCP-shaped upstream for tests
```

There is no `internal/config` or `internal/policy` yet — those arrive in slice 2 and slice 3.

## The JSON-RPC envelope

We defined a single permissive `Message` type:

```go
type Message struct {
    JSONRPC string          `json:"jsonrpc,omitempty"`
    ID      json.RawMessage `json:"id,omitempty"`
    Method  string          `json:"method,omitempty"`
    Params  json.RawMessage `json:"params,omitempty"`
    Result  json.RawMessage `json:"result,omitempty"`
    Error   *RPCError       `json:"error,omitempty"`
}
```

Three design choices worth calling out:

1. **`ID` is `json.RawMessage`, not `int` or `string`.** Servers in the wild use both number IDs and string IDs. RawMessage means we preserve whatever shape arrived without coercion. The `IDString()` helper produces a stable string for indexing.
2. **`Params`, `Result`, `Data` are all `RawMessage`.** We forward payloads bit-perfect. We never re-serialize a payload we did not need to inspect, so we cannot introduce field-ordering or whitespace differences that confuse downstream consumers.
3. **`json:"...,omitempty"` everywhere.** A request has no `Result`; a response has no `Method`. omitempty keeps the marshaled form clean if we ever do need to emit one ourselves.

`Classify()` returns one of `request | response | error | notification | unknown` based on which fields are populated. The classification rule is the only place in the proxy that interprets JSON-RPC semantics in slice 1.

## Framing: NDJSON over stdio

MCP stdio framing is newline-delimited JSON. One message per line, no embedded raw newlines in the JSON.

We use `json.Decoder` (from the standard library) instead of `bufio.Scanner` because:

- `json.Decoder` reads exactly one complete JSON value at a time. It does not care whether the value spans lines.
- This means we tolerate pretty-printed JSON from non-compliant upstreams, which we explicitly committed to in [[02-mcp-protocol]].
- A unit test (`TestFrameReader_PrettyPrintedJSON`) pins this behavior.

`FrameReader.Read()` returns the raw bytes of the next envelope plus the parsed `Message`. We forward the raw bytes verbatim and keep the parsed form only for logging and (in later slices) policy decisions.

`FrameWriter` is a thin wrapper around an `io.Writer` that serializes writes with a mutex and appends a newline after each message. Serialization matters because the upstream's stdin is shared between the client→upstream pump (most messages) and the future synth-error path (when we block a call we will need to write a JSON-RPC error to the client). Two goroutines writing concurrently must not interleave bytes mid-message.

## Recovery from broken framing

`FrameReader.Read()` handles two kinds of broken input:

- **Non-JSON garbage on the channel.** Some servers print plain log lines to `stdout` (against the spec). When `json.Decoder` produces a `*json.SyntaxError`, we advance past the next newline, surface the dropped bytes on the `UnparsedLines()` channel, and keep reading. The dropped line is logged but does not kill the session.
- **Valid JSON that is not a JSON-RPC envelope.** Anything that parses as JSON but has neither `method` nor `id` is also surfaced as `UnparsedLines` rather than crashing the pump.

This matters because v0.1 must work with messy real-world servers, not just compliant ones.

## The proxy lifecycle

```
proxy.New(opts)         construct, generate session ID, no IO yet
proxy.Run(ctx)          spawn upstream, wire pipes, run pumps until done
   ├─ spawnUpstream     exec.CommandContext, capture stdin/stdout/stderr
   ├─ goroutine: pumpClientToUpstream    reads client, logs, writes upstream
   ├─ goroutine: pumpUpstreamToClient    reads upstream, logs, writes client
   ├─ goroutine: drainUpstreamStderr     line-scans upstream stderr → log
   └─ goroutine: drainUnparsedLines      logs broken-framing recoveries
   wg.Wait()             both pumps must exit before Run returns
   killUpstream          SIGKILL the subprocess on the way out
```

**Why both pumps share one context and one cancel:** when either side closes (EOF on client stdin, upstream subprocess exits), we want the whole session to wind down. The pump that hit EOF calls `cancel()`. The other pump sees the context cancel and exits on its next iteration.

**Why we don't use io.Copy:** because we have to inspect every message for the audit log (and later for security policy). io.Copy would forward bytes without giving us a chance to parse them.

**Why we kill rather than `Wait` the subprocess on shutdown:** on Windows, an upstream that is blocked in `ReadFile` on its stdin will not exit just because we close its stdin pipe — it will sit there until killed. SIGKILL (`Process.Kill()`) is the only reliable way to shut down on this platform.

## Audit log

The audit DB is SQLite in WAL mode. Schema (in `internal/audit/audit.go`):

```sql
CREATE TABLE messages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          INTEGER NOT NULL,          -- unix nanoseconds
    session_id  TEXT NOT NULL,             -- uuid per Run() invocation
    upstream    TEXT NOT NULL,             -- logical server name
    direction   TEXT NOT NULL,             -- 'c2s' or 's2c'
    msg_type    TEXT NOT NULL,             -- request|response|error|notification|unknown
    msg_id      TEXT,                      -- nullable, the JSON-RPC id
    method      TEXT,                      -- nullable, present for requests/notifications
    payload     BLOB NOT NULL,             -- raw envelope bytes
    bytes       INTEGER NOT NULL           -- size of payload
);
```

Three indexes (`ts`, `session_id`, `method`, `upstream`) cover the queries the dashboard and CLI will want to make.

### Write path

The proxy hot path calls `audit.Append(Event)` which is **non-blocking**:

- Events go into a 1024-deep buffered channel.
- A dedicated writer goroutine batches up to 32 events or 500ms (whichever comes first) and commits them in one transaction.
- If the buffer is full (proxy is generating events faster than disk can absorb them) the event is dropped with a warning log line. We pick log loss over proxy stall.

This means there is a brief window where in-flight events can be lost on hard process kill. For v0.1 this is fine — the audit log is a forensic aid, not a transaction journal. v1.0 enterprise tier will add a synchronous-write mode.

### Storage location

`audit.DefaultPath()` returns:

| OS | Path |
|----|------|
| macOS, Linux | `~/.sentinel/audit.db` |
| Windows | `%APPDATA%\sentinel\audit.db` |

Override with `--audit <path>` on the CLI.

## CLI surface (slice 1)

```
sentinel run [--name NAME] [--audit PATH] -- <upstream-command> [args...]
sentinel version
sentinel help
```

The `--` separator is required. Anything after it is the upstream command, taken literally. This avoids flag parsing ambiguity when the upstream itself takes flags (which `npx`, `uvx`, `python -m`, etc. all do).

Example:

```bash
# Hypothetical real usage — wraps the official "everything" server.
sentinel run --name everything -- npx -y @modelcontextprotocol/server-everything
```

The proxy reads from its own stdin and writes to its own stdout, so the AI client points at `sentinel` exactly the way it would point at the underlying server.

## Testing

| Test | What it pins |
|------|--------------|
| `TestClassify_Request/Response/Notification/Error` | Each JSON-RPC shape classifies correctly |
| `TestClassify_StringID` | We accept string IDs (some servers use them) |
| `TestClassify_NullID` | `id: null` is treated as no-id (per spec) |
| `TestClassify_MissingJSONRPCField` | We accept envelopes without `"jsonrpc":"2.0"` |
| `TestEncodeRoundtrip` | Decode→Encode preserves request shape |
| `TestDecode_RawMessageParamsPreserved` | Nested params are not flattened |
| `TestFrameReader_NDJSON` | Standard one-line-per-message stream |
| `TestFrameReader_PrettyPrintedJSON` | Multi-line JSON value parses as one message |
| `TestFrameWriter_AppendsNewline` | NDJSON framing is correct |
| `TestFrameWriter_Concurrent` | Concurrent writes do not interleave bytes |
| `TestAppendAndRead` (audit) | Append → flush → query roundtrip works |
| `TestDefaultPathOSSpecific` | Default path resolves on every OS |
| `TestProxy_RoundTrip` | **End to end:** client → proxy → real upstream subprocess → response → audit DB |

The integration test (`internal/proxy/integration_test.go`) builds the `testdata/echomcp` server in `TestMain` and exercises the full pipeline with `io.Pipe` standing in for the client streams. This is the most important test in the slice — it proves the proxy actually works against a real subprocess.

```
PASS: TestProxy_RoundTrip (0.47s)
   audit rows: [c2s|request|tools/list s2c|response|]
```

## Slice 6: HTTP upstream

The proxy's upstream half is now a small interface, `upstreamConn`, with two implementations:

```go
type upstreamConn interface {
    Send(raw []byte) error
    NextFrame(ctx) (raw []byte, msg *Message, err error)
    Close() error
    Description() string
}
```

- `stdioUpstream` — wraps an `exec.Cmd` subprocess. This is what slices 1-5 used directly; it was extracted into a separate file (`internal/proxy/upstream.go`) when the HTTP transport landed.
- `httpUpstream` — speaks **Streamable HTTP**, the current MCP HTTP transport. New file (`internal/proxy/http_upstream.go`).

The proxy's hot path is now transport-agnostic. It calls `upstream.Send(raw)` to forward a client request and `upstream.NextFrame(ctx)` to read the next reply. The pumps, the policy engine, the approval flow, and the audit log don't change.

### Streamable HTTP, in 30 lines

The MCP spec defines a single endpoint per server. The wire flow:

1. **Client→Server.** POST the JSON-RPC envelope to the endpoint.
   - `Content-Type: application/json`
   - `Accept: application/json, text/event-stream`
   - `Mcp-Session-Id: <id>` after the first response assigns one
2. **Server→Client.** The HTTP response is one of:
   - `200 application/json` — single JSON-RPC envelope in the body
   - `200 text/event-stream` — an SSE stream of envelopes (one `data:` event each), used for tool calls that emit progress notifications before the final result
   - `202 Accepted` — acknowledged but no response (notifications)
   - Any 4xx/5xx — error; we log it and stop draining
3. **Session.** The first response with `Mcp-Session-Id` assigns the id; we echo it on every subsequent request from this `httpUpstream` instance.

What's out of scope for slice 6:

- **Server-initiated streams** (GET to the same URL for unsolicited server→client messages). Rare in real MCP traffic; we'll add when there's a server that needs it.
- **Resumability via `Last-Event-Id`.** Advanced reconnect logic.
- **Automatic reconnect on transient errors.** A failed POST is logged; the client sees an empty response and times out. v0.1.x polish.

### SSE parser

`httpUpstream.consumeSSE` is a small line scanner. The MCP-relevant rule:

```
data: <json>           ← single-line: deliver as one frame
data: <json prefix>    ← multi-line: join with "\n", deliver on blank line
data: <json suffix>

<blank line>           ← terminates the event
```

We ignore `event:`, `id:`, `retry:`, and `:` comment lines — MCP only uses default `message` events.

### Config

```yaml
servers:
  remote_mcp:
    url: https://example.com/mcp
    headers:
      Authorization: "Bearer ${SEARCH_TOKEN}"   # any custom headers
```

Validation rules (in `internal/config/config.go`):

- Exactly one of `command:` or `url:` per server.
- `url:` must be `http://` or `https://`.
- `headers:` is a flat string-to-string map; values are passed verbatim. **Environment-variable interpolation in YAML is not yet implemented** — for now, write the literal value (e.g. paste the token). Slice 7 polish.

### Env policy and HTTP upstreams

`env.allow_system` / `env.allow` / `env.deny` only apply to stdio subprocesses. HTTP upstreams don't have an environment to filter — they receive only what the proxy puts in HTTP headers. This is documented in `docs/04-config-schema.md`.

### Testing

`internal/proxy/http_upstream_test.go` covers the transport in isolation:

- One-shot JSON response round-trip
- SSE stream of multiple events
- Multi-line SSE `data:` lines join correctly
- 202 acknowledgements deliver no frame
- Custom headers forwarded
- Bad status codes don't surface garbage
- Empty URL rejected at construction
- Close doesn't hang

`internal/proxy/http_integration_test.go` covers the full pipeline:

- End-to-end client → proxy → HTTP upstream → reply → client
- Policy engine still blocks `rm -rf` over HTTP transport (proves the pipeline is transport-agnostic)

`testdata/httpmcp/main.go` is a standalone Streamable HTTP MCP echo server, useful for manual smoke testing (`go run ./testdata/httpmcp --addr :8801`).

## Known limitations of slice 1

| Limitation | Why we accept it for now | Resolved in |
|------------|--------------------------|-------------|
| Single hardcoded upstream | Multi-server needs config parsing | Slice 2 |
| No env stripping | Same (config-driven) | Slice 2 |
| No pattern detection | Need data flow first | Slice 3 |
| No HTTP/SSE transport | stdio covers ~all local servers | Slice 6 |
| No dashboard | Reads come from the same DB; UI can be added later | Slice 5 |
| Buffered audit writes can lose ~32 events on crash | Forensic, not transactional | v1.0 |

## How slice 2 will build on this

Slice 2 introduces `internal/config/config.go` with a YAML parser, replaces the single-upstream `--name` flag with a config file, supports multiple concurrent upstreams (one proxy session per upstream), and adds env stripping in `spawnUpstream`. Nothing in slice 1's `proxy.go` or `audit.go` needs to change — only `cmd/sentinel/main.go` grows a config-driven server selection.

## Related docs

- [[01-architecture]] — system context this slice fits into
- [[02-mcp-protocol]] — the wire protocol we implement
- [[04-config-schema]] — coming in slice 2
- [[09-telemetry-pipeline]] — coming when the telemetry/opt-in story matures
