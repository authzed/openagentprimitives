# `reconcile` — the phase-based reconciler skeleton

Every controller in `pkg/controllers/*` builds a list of `Phase`s, hands them to
`RunPhases`, and lets the skeleton handle status persistence and early-stop
semantics — so reconcilers stay focused on their phase logic.

| File           | Exports                                                                                           |
| -------------- | ------------------------------------------------------------------------------------------------- |
| `skeleton.go`  | `RunPhases`, and the three outcomes a phase returns: `Continue()`, `StopAfter()`, `FailWith(err)` |
| `load.go`      | `LoadInto` — Get + NotFound handling in one call                                                  |
| `finalizer.go` | `EnsureFinalizer`                                                                                 |
| `invalid.go`   | `SetInvalid` — the standard Valid=False path                                                      |
| `observed.go`  | `StampIfMoved` — set a `*metav1.Time` only when the value it observes actually changed            |

## Non-obvious constraints

- **`StampIfMoved` is the SSA-idempotency guard in helper form.** An observation
  timestamp restamped on every pass turns a converged reconcile into a
  self-sustaining storm — the guardian controller shipped exactly that bug. Use
  it rather than assigning `metav1.Now()` directly;
  [`../../testenv/idempotency`](../../testenv/idempotency/) is the CI gate that
  catches the alternative.
- **A phase's outcome is not an error convention.** `StopAfter()` ends the pass
  successfully (status is still persisted); `FailWith(err)` ends it as a
  failure. Returning a bare `nil` error from a phase means "continue", which is
  not the same thing as "done".
