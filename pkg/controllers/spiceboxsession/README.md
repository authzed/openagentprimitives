# `spiceboxsession` — the SpiceboxSession reconciler

Owns a session's **sandbox**: it resolves the class's sandbox backend and
toolchains, creates the sandbox (a Pod, or an agent-sandbox `Sandbox`), tracks
its readiness as conditions, and tears it down on TTL expiry or deletion.

| File | Role |
| ---- | ---- |
| `controller.go` | `Reconcile` + `SetupWithManager`, and the `+kubebuilder:rbac` markers |
| `sandbox.go` | Folds a session-level sandbox override onto the class, and dispatches to the registered `sandboxkinds.Kind` |
| `toolchains.go` | Resolves the bound toolchains and writes the frozen image digests to the `toolchain_audit` memory kind |
| `conditions.go` | The condition vocabulary |
| `ttl.go` | Idle-TTL teardown |

## Non-obvious constraints

- **The sandbox backend is a registry, not a branch.** `pod` and
  `agent-sandbox` both register into `pkg/tools/sandboxkinds/registry`; this
  controller dispatches through the interface.
- **The `+kubebuilder:rbac` markers here cover a peer CRD you may not be
  running.** The agent-sandbox backend's availability check is RESTMapper
  *discovery*, which needs no RBAC — so on a cluster where the CRD is installed,
  the runtime registers its `Owns(&Sandbox{})` informer regardless of whether any
  class opted in. Without `list;watch` on `sandboxes`, that informer is
  Forbidden, the cache never syncs, `mgr.Start` returns, and **the operator
  crash-loops — taking the built-in pod backend down with it.** Read the marker
  block in `controller.go` before narrowing anything there.
- **`list;watch` is required even where you only `Get`.** The operator's client
  reads through the cache, and the first `Get` starts an informer.
- **`toolchain_audit` is append-only.** Only the `resolved` phase is written
  today, carrying what actually ran; `requested`/`rejected` are declared but
  unwritten because a session-side publisher for them would need
  `WriteAuthority` widened past `ComponentWritten`.
