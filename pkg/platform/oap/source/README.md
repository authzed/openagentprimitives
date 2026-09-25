# `pkg/platform/oap/source`

Builds an [`oap.Bundle`](../) from a place that holds an agent. One tiny
interface, `Source`, with two implementations today: a folder on disk and a live
cluster. A registry-backed source would be a third.

## Files

| File | What it does |
| ---- | ------------ |
| `source.go` | The `Source` interface. |
| `folder.go` | `OpenFolder` — delegates to `oap.FromFolder`. |
| `cluster.go` | `OpenCluster` — walks a live AgentClass and its dependency graph out of the cluster into a bundle. |
| `cluster_refs.go` | `refDescriptors` — the **data-driven** table the cluster walk runs off. A new AgentClass ref field is one row here, not a change to the walk. |
| `sanitize.go` | `Sanitize` — strips server-managed fields so an exported object can be re-applied. |

## Constraints

- **A `Source` must not read secret *values*, only references.** The cluster
  source collects `SecretKeyRef`s; it never resolves them.
- **`Sanitize` strips finalizers on purpose.** Re-applying a CR carrying a
  foreign finalizer into a cluster whose controller is absent wedges the object
  in `Terminating` on delete. Each controller re-adds its own on the next
  reconcile.
- **It also strips `kubectl.kubernetes.io/last-applied-configuration`** (dropping
  the annotations map entirely if that was its only entry), plus the whole
  `status` subtree and the server-populated metadata fields.
- Add a new dependent-CR type by adding a `refDescriptor` row — `Kind`, `Names`,
  `NewObj` — rather than by extending the walk.
