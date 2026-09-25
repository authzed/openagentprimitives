# `lifecycle` — the session/scope lifecycle timeline

`KindName = "lifecycle"`. **Append-only.**

The canonical Kind for session and scope lifecycle signals. Its `ScopeHooks`
listens to _every_ signal in its scope and writes a lifecycle Entry — so the
audit timeline falls out for free, with no per-producer wiring.

| File             | Role                                                               |
| ---------------- | ------------------------------------------------------------------ |
| `kind.go`        | The `memory.Kind` implementation and registration                  |
| `signals.go`     | The `SigSession*` / `SigTurn*` / `SigScope*` signal-kind constants |
| `hooks.go`       | The `ScopeHooks` that turns any in-scope signal into an Entry      |
| `accessor.go`    | Typed append + list over the Kind                                  |
| `marshal.go`     | The on-disk envelope for typed lifecycle events                    |
| `order.go`       | Ordering the timeline for readers                                  |
| `incremental.go` | Incremental timeline reads                                         |

## Non-obvious constraints

- **The signal constants here are the vocabulary other packages publish
  against.** `kg_ingestion`'s hooks react to `lifecycle/turn.completed`; the
  runner and channelsd raise session and turn signals. Renaming one is a wire
  change.
- **Append-only means order matters at write time.** A content-changing `Put`
  returns `ErrAppendOnlyConflict` — `Put` is not last-writer-wins. A copied
  scope must be seeded before anything else can write a divergent entry at the
  same deterministic ID.
- **The typed event nests inside an envelope** (`marshal.go`) rather than being
  stored flat, so a reader that does not know a newer event type can still walk
  the timeline.
