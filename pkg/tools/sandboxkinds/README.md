# sandboxkinds

The pluggability seam for **sandbox backends** — the substrate a
`SpiceboxSession`'s tools actually execute in. The seam is lifecycle verbs over
an _opaque_ `Handle`, deliberately not a pod constructor: a backend that is not
Kubernetes could not implement one. Pod construction stays a helper
([`pkg/platform/podspec`](../../platform/podspec/)) that pod-shaped kinds
import.

| Package                         | Purpose                                                                                                                                                                                                                                                    |
| ------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`registry`](registry/)         | The process-wide registry, over `pkg/x/kindregistry`. Fail-closed: an unknown _or empty_ name misses, so a typo'd `spec.sandbox.kind` surfaces as a validation error rather than silently installing the built-in backend.                                 |
| [`pod`](pod/)                   | The built-in backend: a `corev1.Pod`. The kind every `SpiceboxClass` gets when `spec.sandbox.kind` is unset.                                                                                                                                               |
| [`agentsandbox`](agentsandbox/) | The `agents.x-k8s.io` `Sandbox` CR backend. Bring-your-own — AP installs neither the controller nor its CRDs, so `NewRuntime` fails closed when they are absent, which is what keeps its owned-object watch unregistered on clusters that cannot serve it. |
| [`conformance`](conformance/)   | The assertions every backend must satisfy, in executable form. A new backend proves itself by passing `conformance.Run`.                                                                                                                                   |
| [`internal`](internal/)         | Test-support only — see its own README.                                                                                                                                                                                                                    |

Files at the root:

| File                           | Holds                                                                                                                                                           |
| ------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`kind.go`](kind.go)           | The `Kind` and `Runtime` interfaces and the `Feature` set.                                                                                                      |
| [`kubeshape.go`](kubeshape.go) | Handle-ref helpers for backends whose sandbox happens to be a namespaced Kubernetes object. A convenience for the kinds that agree, not a widening of the seam. |
| [`status.go`](status.go)       | `ErrPreconditionPending` and the shared `Reason` vocabulary. Not an enum — nothing switches on it; consumers that must branch use `Status.Phase`.               |
| [`validate.go`](validate.go)   | `ValidateClassAgainstKind` — rejects a class asking for a capability its resolved backend does not support.                                                     |
| [`workspace.go`](workspace.go) | `CheckWorkspaceDomains`.                                                                                                                                        |

## Constraints

- **`Supports(FeatureSharedWorkspace)` does not answer whether two backends can
  share with _each other_.** `WorkspaceDomain()` names the storage world, and
  sharing requires agreement on it. `CheckWorkspaceDomains` enforces that — and
  today is tested with no production caller, latent rather than wrong: both
  shipped kinds return `DomainKubernetesPVC`, so no reachable input can make it
  error. It goes live when a backend reporting a different domain registers.
- **`pod` reports `FeatureHostEgressAllowlist` as false, deliberately.**
  Per-session NetworkPolicies enforce L3/L4 only; the merged hostname allowlist
  is recorded on status for a DNS-aware policy controller to consume and is not
  enforced by AP, so the backend must not claim it.
- Both `ValidateClassAgainstKind` and `CheckWorkspaceDomains` take the resolved
  `Kind` as a parameter rather than looking it up, so this package never imports
  its own registry and a consumer links only the backends it blank-imports.

Part of [`pkg/tools`](../README.md). See the root
[`README.md`](../../../README.md) and [`AGENTS.md`](../../../AGENTS.md).
