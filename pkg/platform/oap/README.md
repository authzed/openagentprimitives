# `pkg/platform/oap`

The `.oap` agent-container format: a single-file, OCI-native bundle describing
one AgentClass and its dependency graph. The root package owns the format
itself — the manifest schema, the packed layout, the questions a bundle asks and
how their answers bind onto CRs, and the validity rules every install path runs.

The subpackages cover the lifecycle around it: build a bundle from somewhere,
gate and apply it to a cluster, push and pull it from a registry.

## Files

| File | What it holds |
| ---- | ------------- |
| `manifest.go` | The manifest schema, `ParseManifest`, and `SupportedFormatMajor`. |
| `bundle.go` | `Bundle`, the **closed set** of allowed `(group, Kind)` pairs, `Validate`, and `FromFolder`. |
| `layout.go` | `Pack` / `Unpack` / `Digest` over the OCI-layout tar, with hardened extraction caps on the attacker-influenceable layers. |
| `question.go` | The question vocabulary, `ReservedQuestionPrefix` (`capacity.`) and `SplitReserved`. |
| `target.go` | The `<Kind>/<name>#<segment>…` binding target syntax. |
| `overlay.go` | `Answers` and `Apply` — overlays non-secret answers onto CRs per each question's bindings. |
| `skills.go` | `SkillClone` — the external git repos a bundled `SkillSource` would fetch, surfaced before install so the operator consents. |
| `agentui_lint.go` | Static checks on a bundle's declared agent UI against the tools its class actually grants. |

## Subpackages

| Package | What it is |
| ------- | ---------- |
| [`source`](source/) | Builds a `Bundle` from a place that holds an agent — a folder on disk, or a live cluster. |
| [`install`](install/) | The fail-closed checks and answer resolution that gate an install, plus apply and uninstall. |
| [`oci`](oci/) | Push, pull, resolve and cosign-format signing against OCI registries via `oras-go/v2`. |
| [`instance`](instance/) | Stamps install-identifying labels, and on `--name`/collision prefixes CR names while rewriting the cross-references between them so the graph stays consistent. |

## Constraints

- **`allowedBundleKinds` is a security invariant, not a convenience list.**
  `Bundle.Validate` enforces it, and every install path calls `Validate` — most
  importantly the admin console's install endpoint, which runs under the elevated
  operator ServiceAccount and force-applies. Widening the set widens what a
  bundle can make that ServiceAccount write.
- **A binding whose target CR is absent is an error, not a skip.** `Apply` is
  fail-closed; an unanswered question leaves its CR sentinel default alone.
- **Secret-typed questions are skipped by `Apply`** — they drive Secret creation
  in `install`, not a spec overlay.
- **Extraction is capped.** The manifests and assets layer tars may be
  attacker-influenced; the caps in `layout.go` are what bound a decompression
  bomb.
- **Tests must not read anything under `examples/`.** Use the shared fixture in
  [`test/oaptest`](../../../test/oaptest/) or a package-local `testdata/`.

> Note: `FromFolder` treats a `README.md` at a **bundle folder's** root as that
> bundle's long-form docs (`Bundle.Readme`). That has nothing to do with this
> file — `pkg/platform/oap/` is a Go package, not a bundle.
