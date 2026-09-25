# `pkg/platform/cloud`

The cloud-aware install layer. It exposes a `Strategy` registry keyed on cluster
kind and a set of shared sub-interfaces (`TLSStrategy`, `WorkspaceStorage`,
`StatefulStorage`, `ArtifactStorage`, `InstallProfile`) so `cmd/oap`,
`internal/cmd/operator` and `internal/cmd/webd` contain no `if cloud == "gke"`
branches. It depends only on client-go, controller-runtime and stdlib, and must
not import anything under `cmd/oap/internal/`.

Six kinds are registered: `local`, `desktop`, `default`, `gke`, `eks`, `aks`.

## Subpackages

| Package | What it is |
| ------- | ---------- |
| [`aks`](aks/) | `cloud.Strategy` for Azure Kubernetes Service. |
| [`certmanager`](certmanager/) | The cert-manager + Let's Encrypt `TLSStrategy` (HTTP-01 through the Gateway). Wired by returning it from a Strategy's `TLS()`; it has no `init()` registration of its own. |
| [`desktop`](desktop/) | `oap desktop`'s guest-VM kind. Embeds `local.Strategy` and overrides only `Key`, `DisplayName`, `InstallProfile`. |
| [`eks`](eks/) | `cloud.Strategy` for Amazon EKS. |
| [`gke`](gke/) | `cloud.Strategy` for GKE — the largest kind, with its own README. |
| [`local`](local/) | The lightweight developer kind: sqlite memory, in-memory SpiceDB datastore, `file://` artifact PVC, local `:dev` images, no digest pinning. |
| [`unmanaged`](unmanaged/) | The `default` fallback for any cluster with no recognized `providerID`. Inherits `local`'s cloud behavior but carries the **production** `InstallProfile`. |

## Constraints

- **Registration is by blank import.** A kind calls `cloud.Register(s, keys…)`
  from its `init()`; binaries pull them in via a `cloudimports.go`. `Register`
  panics on a duplicate key or a keyless call.
- **Lookup is fail-closed.** `For(key)` errors on both an unknown *and* an empty
  key — there is no silent downgrade. `Default()` is the only route to the
  `default` kind, so every fallback is spelled out at its call site. `MustFor`
  panics and is for `init()` and test literals only.
- **`local` and `desktop` are opt-in only.** Neither reports a
  `ProviderIDPrefix()`, so `Detect` and `ForProviderID` can never return them.
  Auto-selecting one on a real cluster would install an ephemeral SpiceDB
  datastore onto durable infrastructure. `invariants_test.go` asserts this as a
  *property* — any kind whose `SpiceDBDatastore()` is memory must be opt-in — not
  as a key list.
- **`Validate` runs before any mutation**, on explicitly chosen and detected
  kinds alike, so a wrong `--cluster-kind` fails fast rather than halfway
  through a bring-up.
- **Backends never write to stdout.** User-facing output goes through
  `Reporter` (`report.go`); side-effecting cloud-CLI calls go through
  `gcloudexec.go`, which detaches from the parent deadline while still honoring
  cancellation.
- `AP_CLUSTER_KIND` carries the resolved kind to the operator and webd, which
  re-resolve this registry at their own startup, fail-closed.

## Layout

The root splits by *aspect*, one file per sub-interface — `tls.go`,
`workspace.go`, `stateful.go`, `artifactstorage.go`, `gateway.go`,
`gatewaycontroller.go`, `profile.go`, `ceiling.go`, `clusteridentity.go` — with
`cloud.go` holding the `Strategy` interface and the registry, and `keys.go` the
wire-value kind constants. Each cloud package implements the same aspects under
its own name.
