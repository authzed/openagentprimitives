# toolchain

The seam by which a resolved **toolchain payload** is delivered into a sandbox
pod, plus the resolution that produces it.

| Package               | Purpose                                                                                 |
| --------------------- | --------------------------------------------------------------------------------------- |
| [`kinds`](kinds/)     | The delivery mechanisms and their registry.                                             |
| [`resolve`](resolve/) | Turns a class's toolchain _names_ into self-contained, frozen mounts plus a set digest. |

[`toolchain.go`](toolchain.go) holds the `Kind` interface, dispatched by
`SpiceboxToolchain.spec.source.kind`, and `ApplyParams`. The pod builder owns
the volume name and the init-container mount path, so a Kind never invents its
own layout, and the shared `SecurityContext` pointer must be treated as
read-only.

## Constraints

- **Adding a delivery mechanism is a new package plus a blank import.** An
  `imagevolume` kind (Kubernetes 1.36 + containerd 2.1) or a `nix` kind would
  leave [`pkg/platform/podspec`](../../platform/podspec/) and the controllers
  untouched.
- **`resolve.Resolve` is a pure function** of the class and the cluster-scoped
  `SpiceboxToolchain` CRs: the same names always yield the same mounts and the
  same digest. That purity is what lets more than one controller call it without
  either importing the other.
- `Resolve` **fails closed** on a missing or `Valid=False` toolchain, wrapping
  `ErrToolchainMissing` / `ErrToolchainNotValid` so the caller can pick the
  right `Ready=False` reason. Any other error — a transient apiserver failure —
  is returned wrapped by neither sentinel, so the caller propagates it instead
  of persisting a misleading condition.

Part of [`pkg/tools`](../README.md). See the root
[`README.md`](../../../README.md) and [`AGENTS.md`](../../../AGENTS.md).
