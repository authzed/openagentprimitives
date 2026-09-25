# `channelinteractions` — the interaction-category registry

The single place a prompt type is _described_: tool approval, credential link,
identity choice, info-leakage, a one-way notice. The relay, the resurface
machinery, the pipeline's decision pipe and every channel kind dispatch
generically over this registry.

**Adding a category requires zero per-channel-kind code.**

| File            | Role                                                                        |
| --------------- | --------------------------------------------------------------------------- |
| `category.go`   | The `Category` row: name, tone, park phase, deciders, resurface policy      |
| `registry.go`   | `Register` / `Get` / `All` over the process-wide registry                   |
| `decision.go`   | The `Bind`-able decision handlers a category's Approve/Deny click routes to |
| `regenerate.go` | The `ResurfaceRegenerate` seam — rebuilds a prompt from live durable state  |
| `textrender.go` | Kind-agnostic text rendering of a category's copy                           |
| `tone.go`       | The closed tone vocabulary                                                  |

| Package                        | What it holds                                                         |
| ------------------------------ | --------------------------------------------------------------------- |
| [`categories/`](categories/)   | The production rows — 10 prompts + 23 notices                         |
| [`testsupport/`](testsupport/) | Snapshot/restore of the registry and its decision bindings, for tests |

## Non-obvious constraints

- **Rows are declarative; handlers are bound separately.** A `Category` row
  carries no behavior. Decision handlers are `Bind`ed by the binary that runs
  the pipeline (`internal/cmd/channelsd`), so a binary that only _renders_
  interactions never needs them.
- **`Resurface` is a per-category policy, and it decides durability.** Only
  `ResurfaceCached` categories are stored in the `parked_prompt` memory kind.
  `ResurfaceRegenerate` (credential_link) stores nothing on purpose — its
  actionable part is a freshly-minted signed link that must never be persisted
  in replayable form.
- **Registration panics on a duplicate name.** Production code must never call
  `categories.RegisterAll()`; the package's `init` already has. It is exported
  only so a test that calls `Reset` can put the registry back.
- **The name is `channelinteractions`, not `interactions`,** to avoid colliding
  with [`pkg/channels/interact`](../interact/), which is the view-surface
  _inbound_-kind registry — a different seam entirely.
