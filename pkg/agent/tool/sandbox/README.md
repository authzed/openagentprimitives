# `pkg/agent/tool/sandbox`

Runner-side dispatch of sandbox tools: synthesize `Tool` impls from
SpiceboxToolspecs, execute each as a ToolCall CR the operator picks up, and
fetch the resulting stdout/stderr.

| File | Holds |
| ---- | ----- |
| [`synthesize.go`](./synthesize.go) | `Synthesize(bundle, specs, …)` — toolspec entries → `[]tool.Tool`, over [`../synthesize`](../synthesize/). |
| [`sandbox_tool.go`](./sandbox_tool.go) | `SandboxTool`, `SandboxOpts`, `IDs` and the context carriers for them; `Execute` / `ExecuteWithIDs`; post-call relationship writes. |
| [`interactive_tool.go`](./interactive_tool.go) | Streaming execution: `InteractiveHooks`, the streaming path, and the tail buffer that bounds retained output. |
| [`bridge.go`](./bridge.go) | `Bridge` — the stdin/stdout pump for an interactive call, with idle detection. |
| [`tool_progress.go`](./tool_progress.go) | `ToolProgressHook` / `ToolProgressUpdate`, carried on context, for the live caption on a long call. |
| [`artifact.go`](./artifact.go) | `ArtifactClient` and the HTTP implementation over the operator's artifact endpoint. |
| [`describe.go`](./describe.go) | Human-readable description of a call. |

## Constraints

- **The tool never runs code in the runner.** It creates a ToolCall CR; the
  operator dispatches it into the sandbox backend (see
  [`pkg/tools/sandboxkinds`](../../../tools/sandboxkinds/)).
- **Per-call wiring travels on `context`**, not on the tool value: `WithIDs`,
  `WithInteractiveHooks`, `WithToolProgressHook`. A missing carrier is a
  not-present, not a nil deref — read them back with the matching `…FromCtx`.

## See also

- [`pkg/agent/tool`](../) — the `Tool` interface and the three kinds.
- [`pkg/tools/toolspec`](../../../tools/toolspec/) — the spec these are built from.
