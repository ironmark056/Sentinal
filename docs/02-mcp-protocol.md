# 02 — MCP Protocol and Universal Compatibility

## Why this document matters

The strongest claim Sentinel makes is that it works with **every MCP server, including ones we have never seen**. That claim only holds if the proxy operates strictly at the protocol layer and treats individual servers as opaque. This document is the contract that justifies the claim.

## What MCP is

The Model Context Protocol is a JSON-RPC 2.0 protocol published by Anthropic in November 2024. It defines:

- A small set of methods that clients and servers can call on each other.
- Two standard transports: **stdio** and **HTTP with Server-Sent Events** (the streaming variant is sometimes called **Streamable HTTP**).
- A lifecycle (initialize → operate → shutdown).
- A schema convention for tool/resource/prompt definitions using JSON Schema.

The protocol is intentionally small. The full method list fits on one page.

## The wire format

Every MCP message is a JSON-RPC 2.0 envelope.

**Request:**

```json
{
  "jsonrpc": "2.0",
  "id": 42,
  "method": "tools/call",
  "params": {
    "name": "filesystem.read_file",
    "arguments": { "path": "/tmp/notes.md" }
  }
}
```

**Response:**

```json
{
  "jsonrpc": "2.0",
  "id": 42,
  "result": {
    "content": [
      { "type": "text", "text": "...file contents..." }
    ]
  }
}
```

**Error response:**

```json
{
  "jsonrpc": "2.0",
  "id": 42,
  "error": {
    "code": -32000,
    "message": "file not found"
  }
}
```

**Notification (no `id`, no response expected):**

```json
{
  "jsonrpc": "2.0",
  "method": "notifications/tools/list_changed"
}
```

These four shapes — request, success response, error response, notification — are the only kinds of messages on the wire. Sentinel handles them uniformly regardless of what the `method` is or what the upstream server actually does with the call.

## The method namespace

MCP methods fall into five buckets. Sentinel treats them differently only in terms of which ones get full security inspection.

| Bucket | Methods | Security pipeline? |
|--------|---------|--------------------|
| **Lifecycle** | `initialize`, `initialized`, `ping`, `shutdown` | No — passthrough with logging |
| **Discovery** | `tools/list`, `resources/list`, `resources/templates/list`, `prompts/list` | No — passthrough, but the proxy caches the returned schemas |
| **Execution** | `tools/call`, `resources/read`, `prompts/get` | **Yes — full pipeline** |
| **Completion** | `completion/complete` | Yes — pipeline runs, but most servers do not implement this |
| **Notifications** | `notifications/*` | No — logged only |

This bucketing is stable across the spec. New methods can be added but they will fall into one of these categories or be transparent extensions. If a method is unknown to Sentinel, the default behavior is to pass it through with audit logging and not block.

## Why the proxy is universal

The proxy works for *any* MCP server because:

1. **The wire protocol is fixed.** Every MCP server speaks the same JSON-RPC envelope. The proxy only needs to parse and forward envelopes; it does not need to understand what a specific server's tools do.
2. **Tool schemas are advertised.** Servers expose their tools via `tools/list`, which returns JSON Schema descriptions of inputs. The proxy reads this and uses it for schema validation, with no per-server hardcoding.
3. **Security policies operate on tool names and arguments, not server semantics.** When the proxy sees `tools/call` with `name: "filesystem.write"`, it does not need to know what `filesystem.write` does — it just needs to know whether the configured policy allows that name with those arguments.
4. **Transports are standardized.** stdio and HTTP/SSE are the only two transports a v0.1 server can use. The proxy supports both.

A user can install a brand-new MCP server they wrote yesterday, point Sentinel at it via config, and the proxy will work — because the proxy only cares about the protocol, not the implementation.

## The two transports in detail

### stdio

The dominant transport for local MCP servers. The server is a subprocess that:

- Reads JSON-RPC messages from `stdin`, one per line (newline-delimited JSON).
- Writes JSON-RPC messages to `stdout`, one per line.
- Writes diagnostic logs to `stderr` (the proxy captures but does not interpret these).

For stdio servers, Sentinel:

1. Spawns the upstream server as a subprocess with a sanitized environment.
2. Acts as a stdio server *to the client* — reading from its own stdin, writing to its own stdout.
3. Pipes messages through the security pipeline, then forwards to/from the upstream's stdin/stdout.

There is one subtle correctness requirement: stdio MCP messages are framed by newlines, but message bodies can contain newlines inside strings. The proxy must parse JSON incrementally, not split on newlines naively. This is handled by a streaming JSON parser, never by `split('\n')`.

### HTTP / SSE / Streamable HTTP

Used by remote MCP servers and increasingly by cloud-hosted servers. The transport has evolved:

- **Legacy SSE transport** — separate POST endpoint for client→server messages, SSE endpoint for server→client. Deprecated but still seen in the wild.
- **Streamable HTTP** — single endpoint, requests sent as POST with a Streamable HTTP response that can either be a one-shot JSON response or an SSE stream. This is the current standard as of the late 2024 spec revision.

For HTTP transport, Sentinel:

1. Exposes an HTTP endpoint to the client (e.g. `http://localhost:7843/mcp/<server-name>`).
2. Maintains an HTTP client to the upstream URL.
3. Forwards requests and responses, intercepting at the JSON-RPC layer.
4. Handles SSE streams by parsing each `data:` event as a JSON-RPC message and running each through the pipeline independently.

Streamable HTTP introduces resumability via session IDs. The proxy is responsible for tracking sessions across reconnects and not for breaking that contract on the upstream side.

## OAuth and remote authentication

Remote MCP servers often require OAuth. The MCP spec defines an OAuth flow extension where the server returns a 401 with auth metadata and the client handles the OAuth dance.

Sentinel's posture on OAuth:

- **Passthrough by default.** OAuth challenges from the upstream are forwarded to the client unchanged. The proxy does not interpose on the OAuth flow.
- **Token redaction in audit logs.** Bearer tokens, authorization headers, and any field matching a known token shape are redacted before being written to the audit DB.
- **No token storage.** Sentinel never stores OAuth tokens or refresh tokens. Token lifetime is owned by the client and the upstream.

This means Sentinel works with any OAuth-protected remote MCP server without special configuration.

## Binary content

Tool results can include binary content (images, audio, files) via the `content` array with `type: "image"`, `type: "audio"`, or `type: "resource"`. Binary data is base64-encoded inside the JSON.

Sentinel handles binary content by:

- **Not decoding it.** The proxy treats binary content as opaque bytes.
- **Recording size only.** The audit log records the type and byte length, not the content itself.
- **Optional content hashing.** If telemetry is opted in, the proxy may hash binary content to enable downstream deduplication, but never uploads the content itself.

## Schema validation

When the proxy sees `tools/list` return from an upstream, it caches the schemas of every advertised tool. When a `tools/call` arrives for that tool, the arguments are validated against the cached schema before the security pipeline runs.

Schema validation rejects:

- Missing required fields.
- Type mismatches.
- Additional unknown fields when the schema declares `"additionalProperties": false`.

This catches an entire class of malformed tool calls without bothering the upstream.

If the schema is malformed or the cache is empty (e.g. the call arrives before `tools/list`), schema validation is skipped and a warning is logged.

## Notifications and bidirectional traffic

MCP is bidirectional. Servers can send notifications to clients (`notifications/tools/list_changed`, `notifications/resources/updated`, etc.) and clients can send notifications to servers. The proxy forwards both directions.

For request/response correlation:

- Each direction has its own `id` namespace. A request from client→server with `id: 1` is independent of a request from server→client with `id: 1`.
- The proxy maintains two separate pending-request maps, keyed by direction.
- Responses are matched and the pending entry is removed when delivered.
- Timeouts on pending entries are configurable; expired entries trigger an error response synthesized by the proxy.

## Edge cases the proxy must handle correctly

| Case | Handling |
|------|----------|
| Upstream crashes mid-request | Pending entries timeout, synthesize JSON-RPC error -32000 to client, attempt upstream restart |
| Client disconnects mid-request | Cancel pending upstream request if cancellation is supported, otherwise let it complete and discard |
| Upstream sends unsolicited notification | Forward to client, log |
| Upstream returns response with unknown `id` | Log warning, drop |
| Message exceeds size limit | Reject with -32600, log |
| Non-JSON line on stdio | Treat as stderr (log) and continue |
| Streamable HTTP session expires | Forward 401/410 to client, let it reconnect |
| Tool name contains characters that break the policy lookup | Match against tool name exactly as advertised; do not normalize |

## What is *not* in MCP that we sometimes get asked about

- **Authentication between client and server (stdio).** stdio servers trust whoever spawned them. There is no auth on the wire. Sentinel inherits this — anyone who can spawn the proxy can use it.
- **Encryption.** stdio is in-process pipes, not encrypted because there is nothing to encrypt to. HTTP transport should use TLS; Sentinel does not terminate TLS for upstream HTTPS, it passes through.
- **A tool capability negotiation richer than schema.** Capabilities are advertised in `initialize`, schemas are advertised in `tools/list`. There is no separate ACL system in the protocol. Sentinel provides that layer on top.

## Non-spec behavior we tolerate

In practice, MCP servers in the wild are not all perfectly spec-compliant. Common deviations Sentinel tolerates:

- Servers that omit `jsonrpc: "2.0"` — accepted with a warning.
- Servers that send `id` as a string instead of a number — accepted, IDs are kept as opaque strings internally.
- Servers that emit pretty-printed multi-line JSON over stdio — accepted via incremental parsing.
- Servers that mix log lines and JSON on stdout — accepted, non-JSON lines are routed to logs.

Servers that deviate in ways that break correlation or framing (e.g. unframed binary on stdio) are not supported and the proxy will log and disconnect.

## Implications for security

Because the proxy operates at the protocol layer:

- It cannot understand *intent* — only structure. The pattern detection layer ([[05-pattern-detection]]) is what bridges from "this is a valid tool call" to "this looks like an attack."
- It can enforce arbitrary policies on names and arguments without knowing what the tool does.
- It can be confident about correctness because the protocol surface is small and well-defined.

This is exactly the property that makes the proxy universal: it does not need to be updated when new MCP servers are released, only when the MCP protocol itself changes.

## Related docs

- [[01-architecture]] — where this protocol handling fits in the system
- [[03-proxy-design]] — concrete implementation of the protocol handling
- [[05-pattern-detection]] — how we go from "valid call" to "suspicious call"
- [[09-telemetry-pipeline]] — what we log about each protocol message
