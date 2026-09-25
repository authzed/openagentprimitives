# `pkg/platform/workspacekinds`

The pluggable driver surface for **workspace sources** — the origin a per-session
workspace is cut from. The root declares the Kubernetes-free `Spec`, `Scope` and
`Kind` types plus the shared path-safety check; each backend is a subpackage.

Drivers are **command builders, not executors**. A `Kind` returns the argv and
env to run; the framework decides where to run it — a Job against the shared base
PVC, or a ToolCall in the sandbox pod. Keeping this package free of any
`apis/v1alpha1` import is what lets the driver layer be unit-tested without a
cluster.

## Subpackages

| Package | What it is |
| ------- | ---------- |
| [`git`](git/) | The `git` driver: materialize a shared read-cache base, sync an overlay to the latest revision, and apply overlay commits back to the origin. Importing it side-effects registration. |
| [`registry`](registry/) | The global lookup, over `pkg/x/kindregistry`. Mirrors `pkg/channels/channelkinds/registry` and `pkg/tools/kinds/registry`. |

## Constraints

- **Register from `init()`, resolve by name.** `registry.Register` panics on an
  empty or duplicate name. Consumers call `registry.Get(spec.Kind)` — never a
  `switch`.
- **Every driver's `Validate` funnels its scope paths through
  `ValidateScopePath`.** The rules (relative, no `..`, no `~`, no backslash, no
  leading/trailing whitespace) are a property of the overlay-mount boundary every
  driver shares, not of any one backend, so a new driver reuses the check rather
  than re-deriving and subtly diverging from it.
- **An empty `Scope.Paths` means "the whole source, read-only."** Writability is
  per-`PathRule`, opt-in.
- **This is not `pkg/tools/sandboxkinds`.** Workspace sources decide *what
  content* a session sees; sandbox backends decide *where the pod runs*.
- The `git` driver's `Ref` is a branch or tag (it becomes `git clone --branch`);
  pinning to a raw commit SHA is not supported yet.
