# `pkg/authz/scope`

The structured per-session permission policy document — Layer 2 of dynamic
session scope. A `Scope` says which tools this session may call and which
resources it may touch: narrowed at cold start, adjusted mid-session through the
metaagent gate.

**It is a document plus the functions that evaluate and evolve it.** Nothing
here talks to SpiceDB, memory, or the network, and
`go list -deps ./pkg/authz/scope` names no other package in this repo (only
stdlib and CEL). The dispatch-time enforcer is the `Scope` hook in
[`../hooks/scope.go`](../hooks/scope.go), which calls `CheckScope` here; a
second caller is the tool-call pre-pass in
[`../spicedb/toolcheck`](../spicedb/toolcheck/).

| File                                                       | Owns                                                                                                                                                                                                                        |
| ---------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`scope.go`](scope.go)                                     | The `Scope` document: `ScopeTools`, `ScopeResource`, `ResourcePattern`, and the `Source` tag recording where each entry came from (`default`, `initial-ask`, `metaagent-approved`, `extracted`, `approver-deny-converted`). |
| [`check_scope.go`](check_scope.go)                         | `CheckScope` (tool + args) and `CheckScopeWithRefs` (tool + args + resolved resource refs), returning `Result{OK, Reason, Message}`. **An empty `Scope` is the identity — it allows.**                                      |
| [`delta.go`](delta.go), [`apply_delta.go`](apply_delta.go) | `ScopeDelta` and `ApplyDelta` — the only way a `Scope` changes.                                                                                                                                                             |
| [`classify.go`](classify.go)                               | `ClassifySkipped`: which fragments of a requested delta authzd refused to apply, each tagged with a deterministic `SkippedReason` (`out_of_envelope_tool`, `requester_lacks_perm`, `conflicts_with_existing_disallow`, …).  |
| [`caveats.go`](caveats.go)                                 | `DetectCaveats`: fragments that _were_ applied but are known-incomplete — a search tool that can still return the denied thing, a pattern that is not enumerable, an alias for a denied ID.                                 |
| [`cold_start.go`](cold_start.go)                           | The cold-start extraction shape.                                                                                                                                                                                            |
| [`output.go`](output.go)                                   | The metaagent's structured output and approval-payload shapes.                                                                                                                                                              |

## Constraints

- **`AgentClassEnvelope` is the ceiling.** It is the immutable static view of an
  AgentClass's `spec.authz` block, populated before the extractor runs. Anything
  a delta requests outside it is dropped by `ClassifySkipped` with an
  `out_of_envelope_*` reason — new delta paths must go through that check, not
  around it.
- **`Reason` and `Category` are always set deterministically here.** The prose
  `Explanation` on a `SkippedItem` / `CaveatItem` is LLM-composed downstream and
  is empty until the composer fills it. Never switch on the prose.
- **`Source` is audit-bearing.** Every `ScopeResource` records how it got there,
  so an approver-granted entry stays distinguishable from a class default.
- **Keep the repo-dependency count at zero.** The value of this package is that
  the policy document can be evaluated in the runner, in a test, or in a CLI
  preview without standing up a backend.
