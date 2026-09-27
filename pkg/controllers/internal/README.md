# `controllers/internal`

Machinery shared across the reconcilers. Not importable outside
`pkg/controllers`.

| Package                                | What it holds                                                                                      |
| -------------------------------------- | -------------------------------------------------------------------------------------------------- |
| [`reconcile/`](reconcile/)             | The phase-based reconciler skeleton every controller here builds on                                |
| [`backoff/`](backoff/)                 | Per-key exponential-backoff tracker for the JIT credential-refresh loops                           |
| [`identityrefresh/`](identityrefresh/) | The RFC 6749 refresh-token policy shared by the AgentIdentity and UserIdentity refresh reconcilers |
| [`skillspec/`](skillspec/)             | Type-agnostic helpers shared by the SkillSource and ClusterSkillSource controllers                 |
| [`skillpin/`](skillpin/)               | Pin-record construction — `Baseline` for the skill controllers, `Declared` for SpiceboxToolkit     |

## Why these exist as shared bodies

Three of them (`identityrefresh`, `skillspec`, `skillpin`) exist because a
namespaced controller and its cluster-scoped mirror were maintained as two
near-identical files, with wildly unequal test coverage — so a change made in
one copy and forgotten in the other was caught by nothing. The shared body
operates on the _embedded_ type (`SkillSpec`, `[]metav1.OwnerReference`, the
parsed frontmatter), never on the parent CR type, which is what lets one copy
serve both.

The per-controller half that is genuinely **not** shared stays in the controller
packages: `SetupWithManager` (distinct controller names, so leader election and
metrics scope independently) and the watch mappings (a namespaced `List` on one
side, a cluster-wide `List` behind a namespace pre-filter on the other).
