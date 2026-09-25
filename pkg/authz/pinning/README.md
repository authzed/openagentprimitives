# `pkg/authz/pinning`

The pluggable dependency-pinning abstraction. Every kind of swappable dependency
an agent opts into — skills, sidecar images, MCP servers, CLI toolkits, `.oap`
agent containers — has a user-facing ref with a **measurable pin strength**, a
**frozen identity** it resolves to, and a way to **detect drift** between a
recorded baseline and live state.

```go
type Kind interface {
    Name() string                                            // registry key
    ParseRef(spec string) (Ref, error)                        // pure, no I/O
    Resolve(ctx, Ref) (Frozen, error)                         // may do I/O
    Verify(ctx, Ref, baseline Frozen) (DriftReport, error)
}
```

| Package | What it does |
| ------- | ------------ |
| [`kinds`](kinds/) | The per-kind implementations. |
| [`registry`](registry/) | The process-wide registry. Kinds self-register from `init()`; binaries opt in with a blank import. Storage and the panic-on-duplicate/empty semantics come from `pkg/x/kindregistry`. |

## Constraints

- **`ParseRef` is pure.** It classifies a ref string and assigns a `Strength`
  without touching the network. Only `Resolve` and `Verify` do I/O. A kind that
  needs a lookup to *classify* a ref has the seam in the wrong place.
- **New kinds are registered, never branched on.** A consumer that switches on
  the kind name is missing a method on `Kind`.

## Strength

`Strength` ([`pinning.go`](pinning.go)) is a syntactic classification of the ref
alone, with an ordered `Level()` so tiers can compare them:

| Strength | Meaning |
| -------- | ------- |
| `frozen` | Immutable identity — a sha, a digest, a manifest hash. |
| `named` | A movable name — a tag, a branch, a version range. |
| `unpinned` | Rolling; no ref at all. |
