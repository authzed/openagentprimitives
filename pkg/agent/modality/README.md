# `pkg/agent/modality`

A _modality_ is a capability-aware surface that plugs into a turn: given what
the resolved model can do and what the session opted into, it contributes meta
tools and prompt instructions. The runner asks the registry for all of them
rather than branching on which one is active.

[`modality.go`](./modality.go) holds the contract: `Modality`
(`Name`/`MetaTools`/`Instructions`), `Env` (the per-turn capability + opt-in
snapshot, with `NativeActive`), and the two byte-transport interfaces
`ArtifactReader` and `Bridge`.

## Subpackages

- [`registry`](./registry/) — process-local registry. Modalities `Register` in
  `init()`; consumers call `All()`.
- [`files`](./files/) — the only modality today: artifact bytes in and out of a
  model that advertises native file support.

## Constraints

- **A modality's tools are offered, not assumed.** `MetaTools(env)` returns
  nothing when the model lacks the capability or the session did not opt in.
- **Register, don't switch.** Adding a modality is a new package plus an
  `init()`; no consumer changes.

## See also

- [`pkg/agent`](../) — group overview.
