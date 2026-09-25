# `pkg/platform/cloud/gke`

The GKE implementation of [`cloud.Strategy`](../) and its sub-interfaces. It is
the only cloud kind that carries real per-cloud behavior beyond storage-class
selection: a managed Gateway, a Google-managed certificate flow, Hyperdisk
pinning, an auto-provisioned GCS artifact bucket, and a namespace-unwedge path
for the NEG controller's dangling finalizers.

Registered under `cloud.KeyGKE`; claims the `gce://` `providerID` prefix, which
is how `cloud.Detect` selects it.

## Files

| File                   | Aspect                                                                                                                                              |
| ---------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| `gke.go`               | The `Strategy` value: keys, display name, DNS/Gateway CIDRs, address-wait budget, and the sub-interface accessors.                                  |
| `tls.go`               | `GoogleManagedTLS` — Certificate Manager provisioning; `Complete` is a no-op because the cert attaches at Gateway program time.                     |
| `gatewaycontroller.go` | Offers to enable the control-plane Gateway API on the connected cluster and waits until `gke-l7-global-external-managed` is served.                 |
| `stateful.go`          | RWO block-storage selection. Steers the bundled Postgres/Neo4j PVCs onto a Hyperdisk class on Hyperdisk-only node pools.                            |
| `artifactstorage.go`   | Consented GCS bucket create + workload-identity binding, and its teardown.                                                                          |
| `clusteridentity.go`   | Best-effort cluster display name and console deep link, from the GCE metadata server with a node-name heuristic fallback.                           |
| `ceiling.go`           | Reports an **unknown** scheduling ceiling — GKE node pools autoscale, so no present node bounds what can be scheduled.                              |
| `unwedge.go`           | Clears `networking.gke.io` `ServiceNetworkEndpointGroup` finalizers (and the orphaned NEGs behind them) that leave a namespace stuck `Terminating`. |
| `registry.go`          | Artifact Registry prep for image push: `gcloud auth configure-docker` plus repository create.                                                       |

## Constraints

- **Ownership labels gate every destructive path.** The artifact bucket is
  labeled `agentprimitives-owned`; `Ensure` never adopts and `Teardown` never
  deletes a bucket without it.
- **Unwedge never force-clears a finalizer whose backing resource is still
  referenced by a live load balancer.** When `gcloud` is unavailable it reports
  manual commands instead of mutating.
- **`SchedulingCeiling` returning `Known=false` means "skip the check".** A
  caller must not treat it as a zero ceiling — the autoscaler can add a node
  larger than any node present today.
- Long `gcloud` operations use the streaming helper in
  [`../gcloudexec.go`](../gcloudexec.go) so their output is not buffered and
  eaten, and so a stuck tool stays Ctrl-C-able.
