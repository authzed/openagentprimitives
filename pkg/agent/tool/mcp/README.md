# `pkg/agent/tool/mcp`

Turns an MCPServer CR into runner-side tools. Each allowlisted entry becomes one
`tool.Tool` whose `Execute` issues a JSON-RPC `tools/call` against the server.

| File | Holds |
| ---- | ----- |
| [`synthesize.go`](./synthesize.go) | `Synthesize(cr, live, opts…)` — CR + live probe results → `SynthesizeResult`. Options for HTTP client, origin name, session cache. |
| [`mcp_tool.go`](./mcp_tool.go) | The `MCPTool` struct and its whole method set: identity, schema, permission variants, auth set/invalidate/reauth, label and relationship-write sinks, `UseTokenGate`. |
| [`dispatch.go`](./dispatch.go) | `Execute` — the JSON-RPC round trip, result and annotation decoding. |
| [`redact.go`](./redact.go) | `RedactSensitive` — strips declared sensitive arg paths before anything is logged or shown. |
| [`uiresource.go`](./uiresource.go) | Recognizes MCP-UI resource blocks in a response and converts them to `tool.UIResourceSpec`. |

## Subpackages

- [`labelextract`](./labelextract/) — evaluates the per-tool `labels` CEL blocks
  that pull `(resourceType, id, label)` tuples out of a response.

## Constraints

- **Extracted labels are attacker-controllable** — they originate in MCP tool
  responses. `labelextract` is pure and emits to no LLM; callers stamp results
  into the runner's `LabelStore`, which only deterministic channel renderers
  read.
- **`labelextract` is decoupled from the CRD types** so fixtures need no API
  import; the dispatcher adapts CRD blocks at call time.
- **Auth is refreshed, not cached forever.** `SetReauth` supplies the callback
  used when a credential is found revoked mid-session.

## See also

- [`pkg/agent/tool`](../) — the `Tool` interface and the three kinds.
- [`pkg/tools/mcp`](../../../tools/mcp/) — MCP spec, probe, and rendering.
