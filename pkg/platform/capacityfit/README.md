# `pkg/platform/capacityfit`

Answers one install-time question: _this `SpiceboxClass` requests more of some
resource than the cluster's largest node has — what question should ask the
operator to lower it?_ It synthesizes [`oap.Question`](../oap/) values whose
`Binding` overlays the chosen answer back onto the CR, so clamping a class is an
ordinary answered question and the same path serves the CLI, the macOS desktop
installer, and the admin UI.

The three clampable dimensions (cpu, memory, ephemeral-storage) are a table, not
three parallel switches — a fourth checked resource is a new row.

## Subpackages

| Package         | What it is                                                                                                                                                                                            |
| --------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`hook`](hook/) | Builds the `install.InstallOpts.ExtraQuestions` closure every `.oap` install surface wires in: resolve the cluster's scheduling ceiling once via `cloud.Strategy`, then call `capacityfit.Questions`. |

## Constraints

- **The root package is pure**: no I/O, no prompting, no CR mutation. That is
  why `hook` is a sibling rather than a file here — its whole job is the I/O
  this package refuses to own. Keep the boundary.
- **Questions synthesized here are namespaced** under
  `oap.ReservedQuestionPrefix` (`capacity.`), so a bundle-declared question can
  never collide with one in the answer map.
- **`hook` reads an already-installed class as `*unstructured.Unstructured`,
  never as the typed CR.** A typed `Get` round-trips a quantity like `1.8Gi`
  through `resource.Quantity` and renders it back as `1932735283200m`, which
  would rewrite the CR on every subsequent install — a byte-identical-re-apply
  violation.
- A clamp leaves `memoryFloorMargin` above the memory-backed scratch: `/tmp` and
  `/work` are tmpfs charged against the same limit, so a class clamped to
  exactly their sum is OOM-killed as soon as a tool allocates.
