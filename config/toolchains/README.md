# `config/toolchains/` — built-in developer toolchains

These are `SpiceboxToolchain` custom resources, not Deployments. Each one
declares an OCI image whose contents get overlaid into an agent's sandbox at a
fixed prefix, so language runtimes live outside the base sandbox image and are
mounted only when a session needs them. Installing them is what makes `go`,
`node`, and `claude` available to sandboxed agents.

## Files

| File | Toolchain |
| --- | --- |
| `go.yaml` | Go 1.26 (compiler, stdlib) plus the `gopls` language server. Detected by `go.mod`. Pins `GOTOOLCHAIN=local` so a go.mod naming a newer Go fails fast instead of hanging on a blocked download. |
| `node.yaml` | Node.js 22 with npm, pnpm, yarn, and the TypeScript language server. |
| `claude.yaml` | The Claude Code CLI, relocated out of the base sandbox image. No `detect` patterns — it is selected explicitly. |

## Editing notes

- `spec.source.image` is written as `ap-toolchain-<name>:dev` and rewritten to
  the registry/digest-pinned reference by `manifests.Substitute` on a registry
  install.
- `spec.sizeBytes` must be **on-disk usage** (`du -sk` × 1024), not apparent
  size — the kubelet enforces the emptyDir `SizeLimit` against allocated
  blocks, and under-estimating evicts the pod mid-copy. Each file records the
  measurement command and the headroom it holds.

Nothing here is generated.

[← config/](../README.md)
