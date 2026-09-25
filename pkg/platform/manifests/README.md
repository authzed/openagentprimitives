# `pkg/platform/manifests`

The install bundle `oap` embeds and applies, plus the small helpers that shape
it at install time: split a multi-doc stream, filter by install tier, rewrite
image references, inject a storage class, and read CRD scopes back out.

Most of this directory is **generated or vendored YAML**. See below before
editing anything.

## Generated and vendored content

| Path                                                      | Origin                                           | Rule                                                                                                                                                                              |
| --------------------------------------------------------- | ------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `install.yaml`                                            | `kubectl kustomize config/` via `mage manifests` | **Generated. Never hand-edit.** Change `config/**` (or a `+kubebuilder:rbac:` marker, then `mage gen:api`) and regenerate. `TestInstallYAMLMatchesKustomize` catches drift in CI. |
| `cert-manager/`                                           | Pinned cert-manager v1.16.2 upstream release     | Vendored. Replace wholesale on a version bump.                                                                                                                                    |
| `envoy-gateway/`                                          | Pinned Envoy Gateway v1.2.4 upstream release     | Vendored. Note it ships **no** `GatewayClass` named `eg` — callers apply one.                                                                                                     |
| `nats/`, `channelsd/`, `postgres/`, `neo4j/`, `graphiti/` | Hand-written component manifests                 | Editable. Each is exposed by an accessor in `embed.go` that returns the files **in apply order**; keep the order list correct when adding a file.                                 |

## Go files

| File              | What it does                                                                                                                                                                            |
| ----------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `embed.go`        | The `//go:embed` declarations and the per-component accessors.                                                                                                                          |
| `split.go`        | `Split` — thin forwarder to [`../kubeyaml`](../kubeyaml/), which is a leaf so the operator and webd can pull the splitter without pulling the ~1MB bundle.                              |
| `filter.go`       | `FilterByInstallTier` — separates optional regions (e.g. the workspace provisioner) from the always-apply base.                                                                         |
| `substitute.go`   | Image-reference rewriting for `--image-registry` and digest pinning. The image list itself lives in [`../apimage`](../apimage/).                                                        |
| `storageclass.go` | `InjectStorageClass` — sets `storageClassName` on a PVC or every `volumeClaimTemplate` of a StatefulSet, so a cloud-resolved RWO class is applied without editing the embedded files.   |
| `crdscopes.go`    | `CRDScopes` — parses the bundle for every CRD's group/plural/scope. The single source of truth for namespaced-vs-cluster classification, so apply clients do not keep a parallel table. |

## Constraints

- **Secrets are never embedded.** Postgres, Neo4j and Graphiti each require a
  companion Secret generated at install time and applied separately by the
  caller.
- **The test files here are the drift harness**, not incidental coverage: they
  assert install-vs-kustomize equality, RBAC sufficiency, NetworkPolicy
  allowlists, NATS URL consistency, PVC sizes, storage classes, and the SpiceDB
  operator bundle. A change to `config/**` that breaks one of them is a real
  break.
