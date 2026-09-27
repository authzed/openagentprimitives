# `pkg/agent/session/lifecycle`

The session state machine, as a pure function. Events go in, a `State` and a
list of `Effect`s come out; the caller performs the effects.

| File                                   | Holds                                                                                                                 |
| -------------------------------------- | --------------------------------------------------------------------------------------------------------------------- |
| [`state.go`](./state.go)               | `State`, `Phase`, `Region`, `DecisionKind`, `PendingDecision`.                                                        |
| [`event.go`](./event.go)               | The `Event` interface and every event type (`PodReady`, `RunnerClaimed`, `DecisionAsked`, `Revoked`, `Expired`, …).   |
| [`effect.go`](./effect.go)             | The `Effect` interface and every effect (`AppendLog`, `ArmTimer`, `Notify`, `ProjectStatus`, …).                      |
| [`transition.go`](./transition.go)     | `Transition(state, event) → (State, []Effect)` — the single-step rule.                                                |
| [`fold.go`](./fold.go)                 | `Fold` / `FoldWithReissue` — replay a log of events into the current state, reissuing effects that must still happen. |
| [`decision.go`](./decision.go)         | Per-decision-kind timeout policy: `FailsClosedOnTimeout`, and the pending-decision add/remove helpers.                |
| [`continuation.go`](./continuation.go) | `Disposition` / `ContinuationDisposition` — what a resumed runner should do next; `IsPolicyHalt`.                     |
| [`project.go`](./project.go)           | `Project` → `StatusView` / `ConditionView`: state as CRD status conditions.                                           |

## Constraints

- **No I/O, and no clock.** The package imports no k8s, NATS, SpiceDB, memory,
  or `time` package; time-dependent transitions arrive as events from a caller
  that owns a clock. `imports_test.go` enforces this — keep it that way, because
  it is what makes the machine exhaustively testable.
- **Timeouts fail closed** for decision kinds where `FailsClosedOnTimeout`
  reports true. A decision that expires unanswered is not an approval.

## Callers

The runner folds the log and emits events in
[`../../runner/sequencer.go`](../../runner/sequencer.go); the AgentSession
reconciler projects state onto status.

## See also

- [`pkg/agent/session`](../) — the session group.
