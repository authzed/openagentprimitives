# `pkg/agent/tool`

Tools as the agent loop sees them. [`tool.go`](./tool.go) defines the single
`Tool` interface every callable thing implements, plus `Result`,
`SessionContext`, `Operation`, and the optional interfaces a tool may also
satisfy (`OriginTool`, `Cancellable`, `Introspectable`, …).

Three `Kind`s exist, all live:

| Kind | Where it runs | Synthesized from |
| ---- | ------------- | ---------------- |
| `KindMeta` | in-process in the runner | [`meta`](./meta/), assembled by [`meta/capability`](./meta/capability/) |
| `KindSandbox` | a ToolCall CR the operator dispatches into the sandbox | a SpiceboxToolspec, via [`sandbox`](./sandbox/) |
| `KindMCP` | JSON-RPC `tools/call` to an upstream server | an MCPServer CR, via [`mcp`](./mcp/) |

Other files: [`errors.go`](./errors.go) (`ParseArgs`, `ArgParseError`),
[`introspect.go`](./introspect.go), [`cancellable.go`](./cancellable.go).

## Subpackages

| Package | Purpose |
| ------- | ------- |
| [`meta`](./meta/) | The in-process meta tools (`respond_to_user`, `update_plan`, `query_memory`, …) and the capability layer that decides which a session gets. |
| [`sandbox`](./sandbox/) | Runner-side sandbox dispatch: SpiceboxToolspec → `Tool`, execution as a ToolCall CR, streaming/interactive mode, artifact fetch. |
| [`mcp`](./mcp/) | MCPServer CR → `Tool`; JSON-RPC dispatch, auth refresh, sensitive-arg redaction, label extraction. |
| [`sidecartoolbox`](./sidecartoolbox/) | Synthesizes tools from a `ResolvedSidecarToolbox`, reusing `mcp`'s dispatcher. |
| [`synthesize`](./synthesize/) | Shared "iterate spec → emit `Tool`" machinery: name normalization, collision detection, factory call. Used by every kind's `Synthesize`. |
| [`operations`](./operations/) | Per-session registry backing `new_operation` — every external call must be justified by a logical operation. In-process only; a restart loses mid-flight operations. |
| [`authfail`](./authfail/) | Records "this call failed the way this provider's credentials fail" onto session status, for the CredentialUpdateRequest reconciler. |
| [`originfmt`](./originfmt/) | The wire format of a tool's origin string (`mcpserver/…`, `toolkit/…`, `sidecartoolbox/…`). A leaf, so writer and reader cannot drift. |

## Boundary

This package is tool **invocation**. Tool **definition** — toolspec parsing,
toolkits, MCP spec/probe/render, sandbox backend kinds, skills — lives in
[`pkg/tools`](../../tools/), and the dependency points that way only.

### Two `sidecartoolbox` packages — do not conflate them

- **[`./sidecartoolbox`](./sidecartoolbox/) (here) is the execution
  synthesizer.** `Synthesize(rt, live, …)` turns a `ResolvedSidecarToolbox`
  snapshot into dispatchable `tool.Tool`s reusing [`mcp`](./mcp/)'s `MCPTool`,
  and tags each with its sidecar origin so toolguard's origin breaker treats one
  sidecar's tools as sharing health. `internal/cmd/runner` imports it directly.
  It touches no credential — the AgentSession reconciler resolves those.
- **[`pkg/tools/kinds/sidecartoolbox`](../../tools/kinds/sidecartoolbox/) is the
  CLI-presentation Kind** (`Row`, `Detail`, `ValidateFile`), blank-imported by
  `cmd/oap`. A new CLI Kind gets the blank import; this synthesizer does not.

## See also

- [`pkg/agent`](../) — group overview.
