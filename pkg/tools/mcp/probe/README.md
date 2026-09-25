# probe

The **MCP client**. Backed by the official `modelcontextprotocol/go-sdk`, so it
speaks the full session protocol rather than a hand-rolled stateless JSON-RPC
POST — which spec-compliant servers reject with "invalid during session
initialization". Used by the runner (sidecar and `MCPServer` tool synthesis and
dispatch), by the operator's reachability reconciles, and by the `oap` CLI.

| File | Holds |
| --- | --- |
| [`client.go`](client.go) | `Client` — a server URL plus an HTTP client and a timeout. |
| [`session.go`](session.go) | The go-sdk-backed session: `ListTools`, `CallTool`, SSE unwrapping, reconnects. Auth lives in a `RoundTripper` on the injected HTTP client, so the dispatcher's CEL / audit / toolguard middleware stays auth-agnostic. |
| [`session_cache.go`](session_cache.go) | One persistent session per (server URL, credential), for an AgentSession's lifetime. |
| [`types.go`](types.go) | `Tool`, `ServerInfo`, `Annotations`. Input and output schemas are kept as raw JSON rather than committing to a JSON Schema parser. |
| [`errors.go`](errors.go) | `HTTPError`, with `IsAuth()` so a controller can distinguish 401/403 from other server errors when writing a condition. |

## Constraints

- **SSRF.** The URL originates from `MCPServer.spec.server.url` — an
  agent-influenceable value. A nil `Client.HTTP` means the SSRF-guarded
  [`safehttp.Client()`](../../../x/safehttp/); only tests, the e2e harness, and
  operator-controlled reach paths (a sidecar pod IP or loopback, which the guard
  would correctly refuse) set it explicitly.
- **Why `SessionCache` exists.** A stateful MCP server keys per-request state by
  the MCP session id. Plain `CallTool` opens a fresh session per call and closes
  it, so a selection made on one call is invisible to the next. One session per
  AgentSession fixes that at the source. It is safe for concurrent use — the
  runner dispatches tool calls in parallel — and is closed when the session ends.
- A tool's `Description` is the server's own prose and reaches the model
  verbatim unless a spec overrides it: **untrusted prompt text**.
- `tools/list` ordering is not guaranteed by the SDK; assertions over it must be
  membership, never positional.

Part of [`pkg/tools/mcp`](../README.md). See the root
[`README.md`](../../../../README.md) and [`AGENTS.md`](../../../../AGENTS.md).
