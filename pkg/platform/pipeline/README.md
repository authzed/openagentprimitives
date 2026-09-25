# `pkg/platform/pipeline`

The generic session-lifecycle interceptor framework. Lifecycle **points** mark
moments in an agent session; **hooks** attach to points with a code-owned order;
hooks return **decisions as data**; an **executor** runs them and applies their
effects through a `Host` port that each component implements.

The package imports no domain packages. `pkg/authz/hooks` is one consumer;
non-authz concerns (output secret scanning, budget gating) register the same
way.

## Files

| File            | What it holds                                                                                                                                                                                                      |
| --------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `doc.go`        | The package doc.                                                                                                                                                                                                   |
| `point.go`      | `Point` — the data-plane points (`session_start`, `inbound_turn`, `pre_tool_call`, `post_tool_call`, `pre_response`, `session_end`), the four metaagent control-plane points, and `session_fork`.                  |
| `hook.go`       | The `Hook` and `Host` interfaces.                                                                                                                                                                                  |
| `registry.go`   | `Registry`, `NewRegistry`, and the process-global `Default`.                                                                                                                                                       |
| `executor.go`   | `Executor.Run` — ordering, Deny/Halt short-circuit, approval publish/await/timeout, notice and status delivery, audit, fail-closed-on-panic.                                                                       |
| `timeout.go`    | `IsTimeout` — the shared classifier every `Host` uses so none can disagree about what "timed out" means.                                                                                                           |
| `types.go`      | `SessionRef`, the per-point payloads, `Decision`, `Outcome`, `ApprovalAsk`.                                                                                                                                        |
| `definition.go` | `DefinitionError` — plain-string vocabulary for "a piece of the agent's own definition could not be evaluated", so a hook can report one and a consumer can render it without either importing the other's domain. |

## Constraints

- **Point string values are stable.** They appear in audit records and rendered
  views; renaming one rewrites history.
- **The executor owns _all_ orchestration.** A hook returns a `Decision` and
  mutates only through handles it already holds. It does not publish, await, or
  short-circuit.
- **A timeout is not a Halt.** When `AwaitDecision` returns `timedOut == true`,
  `err` MUST be nil and the executor derives the verdict from
  `ApprovalAsk.OnTimeout`. Hosts classify their deadline with `IsTimeout`; they
  do **not** decide the verdict.
- **Failure is fail-closed.** A hook that panics, and a genuine host-primitive
  error, both become `Halt`.
- **Both lifecycles share one machinery.** The metaagent points are four more
  string constants run in sequence by the authzd worker rather than by the
  runner — not a second framework.
- Use `NewRegistry` in tests; `Default` is for production `init()` registration.
