# `pkg/apis`

The CRD types. **Everything depends on this group; it depends on nothing else
in the repo.** Keep it that way — an import from `pkg/apis` into any other
`pkg/` group would make the dependency graph cyclic at the root.

| Package | Contents |
| ------- | -------- |
| [`v1alpha1`](v1alpha1/) | The single API version: 27 CRD kinds in group `agentprimitives.authzed.com`, plus the shared sub-types, condition/reason constants, and the generated deepcopy methods. |

## These types are generator input, not just Go structs

`config/crds/*.yaml` and `config/manager/role.yaml` are **generated from this
tree**, and `pkg/platform/manifests/install.yaml` is generated from those. Three
consequences that catch people:

- **`v1alpha1/zz_generated.deepcopy.go` is generated** by `mage gen:api`
  (controller-gen `object`). Never hand-edit it; add or change the field on the
  `*_types.go` file and regenerate.
- **Doc comments become the CRD `description`.** Prose you write above an
  exported field is what `kubectl explain` prints and what ships inside
  `config/crds/*.yaml`. Write it for a cluster operator, not for a Go reader.
- **`+kubebuilder:` markers are the schema.** Validation, defaults, printer
  columns, and the `status` subresource all come from markers here.

### Regeneration chain

```
pkg/apis/v1alpha1/*_types.go
+ kubebuilder:rbac markers in pkg/controllers/*/controller.go
                  │  mage gen:api      (controller-gen: CRDs + role.yaml + deepcopy)
                  ▼
config/**
                  │  mage manifests    (kubectl kustomize config/)
                  ▼
pkg/platform/manifests/install.yaml    ← embedded into oap, applied by oap install
```

Run `mage gen:api` after any CRD-shaping change here, then **always**
`mage manifests`. Skipping the second stage ships an `oap` binary whose embedded
bundle disagrees with `config/` — `TestInstallYAMLMatchesKustomize` in
`pkg/platform/manifests/` is what catches that in CI.

## `agentclass_types.go` is deliberately not gofmt-clean

`gofmt -l pkg/apis/v1alpha1/` reports `agentclass_types.go`, and that is
**intentional. Do not "fix" it.**

The file carries two `+kubebuilder:validation:XValidation` CEL rules whose
expressions contain paired single quotes — `self.identityMode == 'ask'` and the
empty-string literal `self.agentIdentity != ''`. gofmt applies its smart-quote
transformation to comment text and rewrites `''` into a single `”`
(U+201D RIGHT DOUBLE QUOTATION MARK). controller-gen then emits that character
into the CRD's `x-kubernetes-validations` rule, and the CRD **fails to install**:
the API server cannot compile the CEL expression.

The breakage is invisible locally — the Go code still builds and unit tests still
pass; it surfaces only when the regenerated CRD is applied to a cluster. If you
must reformat the file, re-run `mage gen:api` and confirm the rule text in
`config/crds/agentprimitives.authzed.com_agentclasses.yaml` still contains plain
ASCII apostrophes before committing.
