# `config/` — the kustomize install tree

This is the source of truth for everything `oap install` applies as its base
region. `mage manifests` runs `kubectl kustomize config/` and writes the result
to `pkg/platform/manifests/install.yaml`, which is `go:embed`ed into the `oap`
binary.

**Never hand-edit `pkg/platform/manifests/install.yaml`.** Edit here, then
regenerate.

## Regeneration

```
+kubebuilder:rbac markers in pkg/controllers/*/controller.go
+ pkg/apis/v1alpha1/*_types.go
          │  mage gen:api      (controller-gen)
          ▼
config/crds/*.yaml  +  config/manager/role.yaml       ← GENERATED
          │
config/**  (everything else — hand-written)
          │  mage manifests    (kubectl kustomize config/)
          ▼
pkg/platform/manifests/install.yaml                    ← GENERATED, embedded
```

- Changed a `+kubebuilder:rbac:` marker or a CRD-shaping type? Run
  `mage gen:api`, then `mage manifests`.
- Changed anything under `config/` by hand? `mage manifests` alone.

`TestInstallYAMLMatchesKustomize` (in `pkg/platform/manifests`) fails in CI when
`install.yaml` drifts from this tree.

## Directories

| Directory | Contributes |
| --- | --- |
| [`authzd/`](authzd/README.md) | The async authorization worker: entity extraction and metaagent scope orchestration. |
| [`crds/`](crds/README.md) | All `agentprimitives.authzed.com` CustomResourceDefinitions. **Generated.** |
| [`extractord/`](extractord/README.md) | The powerless attachment text-extraction service (no egress, no SA token). |
| [`identities/`](identities/README.md) | The `agentprimitives-identities` Namespace, where per-user credential Secrets live. |
| [`manager/`](manager/README.md) | The operator itself: namespace, RBAC, Deployment, memory PVC, Services, admission webhooks. |
| [`networkpolicy/`](networkpolicy/README.md) | Default-deny + allowlist NetworkPolicies for `agentprimitives-system`. |
| [`spicedb-operator/`](spicedb-operator/README.md) | The vendored upstream authzed spicedb-operator release bundle. |
| [`toolchains/`](toolchains/README.md) | Built-in `SpiceboxToolchain` CRs (go, node, claude) mounted into sandboxes. |
| [`webd/`](webd/README.md) | The browser-UI host and its four-namespace RBAC split. |
| [`workspace-provisioner/`](workspace-provisioner/README.md) | Optional RWX workspace storage tier, applied separately by `oap install`. |

## Notes

- **`config/` is not everything `oap install` applies.** NATS, Postgres, Neo4j,
  Graphiti, cert-manager, Envoy Gateway, and channelsd ship as separate embedded
  manifest sets under `pkg/platform/manifests/<name>/`, applied as their own
  install regions. Only the base region is rendered from this tree.
- **Image tags are placeholders.** Every first-party image is written as
  `<name>:dev` here; `manifests.Substitute` rewrites it to the registry-,
  version-, and digest-resolved reference at install time
  (`pkg/platform/apimage` is the image list of record).
- **Optional tiers are labelled, not conditionally rendered.** Resources
  carrying `agentprimitives.authzed.com/install-tier` are split out of the base
  region by `manifests.FilterByInstallTier` and applied only when the install
  asks for them.
