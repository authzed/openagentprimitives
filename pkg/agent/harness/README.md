# `pkg/agent/harness`

The agent-harness seam: the swappable component that runs an AgentSession's
outer loop. A harness answers one question — *what does the runner container
look like?* — by contributing its own image, command, args, env and mounts.
Everything else (memory token, NATS credentials, SpiceDB endpoint, signing keys)
is platform wiring computed by
[`pkg/controllers/agentsession`](../../controllers/agentsession/) and merged
around the harness contribution, so no harness restates it.

`harness.go` holds the `Harness` interface, `ContainerSpec`, `HarnessOpts`,
`ModelAccessMode`, and `DefaultName` (`"ap-native"`).

## Subpackages

- [`apnative`](./apnative/) — the built-in harness: the `runner` binary running
  `runner.Loop`. Its `ContainerSpec` is deliberately empty; the runner image's
  entrypoint and env are already platform wiring.
- [`registry`](./registry/) — global name→harness lookup (`Register`, `Get`,
  `Resolve`, `All`, `Reset`).
- [`fake`](./fake/) — configurable test harness.

## One registered backend

**`ap-native` is the only harness registered today.** `fake` exists but has no
`init()` registration on purpose — tests register it explicitly so the global
registry stays deterministic. Treat this as a seam with one implementation, not
a menu.

To add one: new package, `func init() { registry.Register(&Harness{}) }`, and a
blank import in the binaries that need it (the AgentSession controller resolves
`AgentClass.spec.harness` by name). Nothing else changes.

## See also

- [`pkg/agent`](../) — group overview.
- [`AGENTS.md` § Pluggability](../../../AGENTS.md#pluggability-is-a-primary-design-goal)
