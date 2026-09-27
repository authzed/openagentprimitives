# `pkg/authz/revocation`

In-flight revocation: making a credential or a tool origin stop working **now**,
without waiting for a session to end or a cache to expire.

The shape is publish/subscribe over NATS:

1. A CR's controller notices a revocation and calls `Publisher.Publish`, which
   emits a `KindRevoked` envelope on the `ap.revocation` subject.
2. Each runner runs **one** subscriber (`RegisterSubscriber`). It scope-filters
   the envelope (`Applies(scope, myNamespace)`, where `AllNamespaces` is `"*"`)
   and dispatches to the `Invalidator` registered for the envelope's `Kind`.

Adding a revocable kind = register one `Invalidator` + add a publisher trigger
in that CR's controller. **No consumer edits.**

| Package           | What it does                 |
| ----------------- | ---------------------------- |
| [`kinds`](kinds/) | The registered invalidators. |

## Fan-out, not cache-drop

An `Invalidator` must reach **every** holder of the revoked thing, not just the
obvious cache. See [`kinds/credential`](kinds/credential/): the broker's
resolution cache is one holder, but a credential resolved once at session start
may also have been frozen into a live object — the runner's `MCPTool` copies
`(header, value)` into struct fields and never re-reads the broker. Dropping the
cache alone would leave that copy live.

When you add an invalidator, enumerate the holders first.
