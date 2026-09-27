# internal

Test-support code for the sandbox seam, kept unimportable outside
[`pkg/tools/sandboxkinds`](../README.md).

- [`shapecheck`](shapecheck/) — a sandbox backend modelling a provider **outside
  the cluster**: an opaque ID handle, no Kubernetes client, no pod, a native
  file API and native snapshots.

It ships no production behavior and is **never registered**, so nobody can
select it from a `SpiceboxClass`. Its two jobs are to fail compilation if a
Kubernetes assumption leaks into the `sandboxkinds` interfaces while those
interfaces are still cheap to change, and to give the conformance suite a
second, deliberately unlike implementation to run against.

See the root [`README.md`](../../../../README.md) and
[`AGENTS.md`](../../../../AGENTS.md).
