# kinds

Toolchain **delivery mechanisms**, dispatched by
`SpiceboxToolchain.spec.source.kind`.

| Package | Kind | Mechanism |
| --- | --- | --- |
| [`image`](image/) | `image` | Runs the toolchain's own OCI image as an init container that copies the payload into a shared `emptyDir`, which the sandbox container then mounts read-only. |
| [`registry`](registry/) | — | The process-local registry. Kinds self-register from `init()`; binaries blank-import the ones they support. |

## Constraints

- **`image` is the portable mechanism**: no feature gate, no minimum Kubernetes
  version, no particular container runtime — unlike the native image-volume
  source (KEP-4639), which is GA only in 1.36 and needs containerd ≥ 2.1. The
  cost is one copy and transient disk.
- `image.Validate` re-checks that the mount name is a DNS-1123 label even though
  CR admission checks the same invariant: the Kind builds `"toolchain-"+name`
  into a container name and is reachable independently, so it cannot rely on
  the admission caller having run first.
- `registry.Register` **panics on a name collision**, so an init-ordering bug is
  loud at startup rather than a silently-shadowed delivery mechanism at
  pod-create time. `Names()` exists so an unknown-kind error can list the known
  ones.

Part of [`pkg/tools/toolchain`](../README.md). See the root
[`README.md`](../../../../README.md) and [`AGENTS.md`](../../../../AGENTS.md).
