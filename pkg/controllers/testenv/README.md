# `testenv` — the shared controller test harness

A controller-runtime **envtest** harness shared across every controller test
package, plus the CI gates for the repo's merge/apply conventions.

| File | Role |
| ---- | ---- |
| `testenv.go` | Starting and stopping the envtest control plane |
| `shared.go` | The process-wide shared environment, so N test packages do not each stand up an apiserver |
| `sandbox.go` | An `exec.Executor` stand-in for tests that reach into a sandbox |

| Package | What it holds |
| ------- | ------------- |
| [`idempotency/`](idempotency/) | `CheckApplyIdempotent` and `CheckReconcileConverges` |

## The two idempotency gates

Both guard a real production bug, and both come straight from `AGENTS.md`'s
"Server-side apply: keep applied fields idempotent; put observations in status".

| Check | Catches |
| ----- | ------- |
| `CheckApplyIdempotent` | A volatile value — `time.Now()`, a random ID, a recomputed digest — leaking into a field a client server-side-applies. A byte-identical re-apply must not bump `resourceVersion` |
| `CheckReconcileConverges` | A reconciler that unconditionally restamps an observed-at or status field on every pass, producing a reconcile storm |

Each splits into an error-returning core (`Check*`) and a `require`-based
wrapper (`Require*`), so the package's own tests can assert the core **detects**
a violation — something a `t.FailNow`-calling helper could never do.

Neither has a build tag or imports envtest; they need only a `client.Client`
(and a `reconcile.Reconciler`). They are nonetheless meaningful **only** against
a real apiserver.

## Non-obvious constraints

- **The envtest control plane is machine-wide.** `mage test:integration` and
  `mage test:e2e` serialize via an advisory lock for that reason; concurrent
  runs from different worktrees starve each other rather than taking turns.
