# `pkg/agent/session/state`

The per-session in-memory state framework. A `Kind` registers at `init()`; the
runner builds one `Registry` per AgentSession, which materializes one `Store`
per registered kind. [`registry.go`](./registry.go) holds all of it.

## Subpackages

- [`plans`](./plans/) — the one registered kind: agent-declared multi-step
  plans, mutated only by the `update_plan` meta tool.

## Replay

State survives a runner restart by riding on `system_note` turns in memory,
under a self-describing wrapper:

```json
{"kind": "<name>", "v": <int>, "data": <kind-defined>}
```

`DispatchSystemNote` routes each wrapped note to the matching kind's
`Store.ReplayNote`. **Unwrapped notes fall through** to the caller's own
`system_note` handling — `respond_to_user`'s delivered-set is one — so a kind
must always write the wrapper, or its state silently vanishes on resume.

## See also

- [`pkg/agent/session`](../) — the session group.
