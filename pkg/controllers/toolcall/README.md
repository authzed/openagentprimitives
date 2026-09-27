# `toolcall` — the ToolCall reconciler

A `ToolCall` is the instruction the privileged operator follows to exec a tool
in the sandbox — and to inject resolved Secret values. The runner creates one
**after** its SpiceDB authz and approval check; this controller executes it and
records the result.

| File            | Role                                                                                |
| --------------- | ----------------------------------------------------------------------------------- |
| `controller.go` | `Reconcile` + `SetupWithManager`, and the `+kubebuilder:rbac` markers               |
| `toolspec.go`   | Runs the toolspec validator against every candidate spec for the requested tool     |
| `envcheck.go`   | Environment-variable validation before injection                                    |
| `executor.go`   | Selects the `exec.Executor` / `FileTransferer` for the session's sandbox backend    |
| `hydrate.go`    | Streams input artifacts into the sandbox (tar over exec, or a native file transfer) |
| `harvest.go`    | Streams captured output paths back out                                              |
| `stream.go`     | The streaming-tool path — long-running exec with live output                        |
| `snapshot.go`   | Pre-dispatch workspace snapshots for `readwrite` / `external` tools                 |

## Non-obvious constraints

- **The admission webhook pins this decision.** See
  [`../webhooks/toolcall/`](../webhooks/toolcall/): on CREATE the ToolCall must
  be owned by an AgentSession and every credential source must reference a
  Secret in the ToolCall's own namespace; on UPDATE the spec is **immutable**.
  The operator writes only the status subresource, so that breaks no legitimate
  flow — do not add a spec write here.
- **A pre-dispatch snapshot is a record, not just a side effect.** It is written
  to the append-only `tool_dispatch_snapshot` memory kind, linked to its turn,
  so the restart reconciler can map "cut at turn N" → "which snapshot to restore
  for which bundle PVC".
- **`toolspec.go` has a deliberate special case:** when the session has
  toolspecs configured for this tool but all of them reject it, that is a deny,
  not an absence — read the file's doc comment before changing the fallthrough.
