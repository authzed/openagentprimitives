# `pkg/platform/oap/oci`

Moves `.oap` agent containers to and from OCI-compliant registries. The `.oap`
file produced by [`oap.Pack`](../) *is* an OCI-layout tar, so this package is
mostly a content-addressed copy in each direction, using `oras-go/v2` for the
registry protocol.

## Files

| File | What it does |
| ---- | ------------ |
| `ref.go` | Reference parsing and the authenticated `remote.Repository` construction — the package doc lives here. |
| `push.go` | `Push` — loads the layout tar as a read-only source store and copies the manifest graph to the registry. Returns the manifest digest, which equals `oap.Digest(oapBytes)`. |
| `pull.go` | `Pull` — copies the graph into a temporary layout store and re-serializes it as a `.oap` tar. |
| `resolve.go` | `Resolve` — HEAD-resolves a ref to its digest **without pulling any blob**. The lightweight path for pinning. |
| `sign.go` | Cosign-format signature creation and verification, matched byte-for-byte against cosign v2's shapes. |

## Constraints

- **`Pull` output need not be byte-identical to what was pushed.** The outer tar
  packaging is not content-addressed; only the OCI content inside is. Compare
  digests, never bytes.
- **`Resolve` is the right call when you only need to learn a digest** — pinning
  in particular. Using `Pull` for that fetches every blob for no reason.
- Signing is deliberately cosign-compatible rather than a bespoke scheme, so
  existing verification tooling works against a pushed `.oap`.
