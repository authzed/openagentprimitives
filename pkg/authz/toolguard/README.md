# `pkg/authz/toolguard`

Per-tool circuit breakers (with exponential backoff as the recovery schedule),
per-tool rate limits, and per-tool data-volume budgets — enforced as
`PreToolCall` / `PostToolCall` pipeline hooks. It sits at order 15 in
[`../hooks/order.go`](../hooks/order.go): after MCP trust, before the SpiceDB
check, so a breaker-open denial never burns a permission check or an approval
ask.

| File | Owns |
| ---- | ---- |
| [`state.go`](state.go) | `Registry` — the breaker/limit state keyed per tool and per origin — and `Action` (`off` < `warn` < `deny` < `halt`), ordered by severity so a ceiling can floor it. |
| [`policy.go`](policy.go) | `Tiers` → `ResolvePolicy` → `ResolvedPolicy`. `Builtin` is the hardcoded bottom tier (breaker on, rate limits off), overridable by any `ToolGuardPolicy` tier; every resolved rule carries a `Provenance` string naming the tier it came from. |
| [`hook_guard.go`](hook_guard.go) | The `PreToolCall` admission decision. |
| [`hook_record.go`](hook_record.go) | The `PostToolCall` recorder. Reuses order 15 so it observes the raw `Execute` outcome, not the post-gated one. `PatchStatus` pushes the open-breaker snapshot to `AgentSession` status **only on a transition**, not per call. |
| [`probe.go`](probe.go) | The half-open probe ledger — see below. |
| [`events.go`](events.go) | `Event`, the enforcement-relevant occurrence fed to the `toolguard_audit` memory kind and to structured logs. |

## The half-open probe slot is a claim, not a flag

`Admit` grants exactly one in-flight call the right to test a recovering
breaker, and `Record` hands that claim back with an outcome. Half-open consults
no cool-off timer (only `open` does), so **a claim that is never handed back
denies every later call on that key for the life of the session** — with
`deniedBy: "probe"`. When the key is an origin key, that is every tool of that
MCP server.

Several exits in the runner's contained-call sequence return before
`PostToolCall`, and therefore before `Record`, ever runs: a `Pre` Deny (any
downstream gate, including a human-denied or timed-out approval), a `Pre` Halt,
and a user interrupt.

`WithProbeLedger(ctx)` closes that hole structurally rather than asking each
exit to remember a release call: the caller opens exactly one ledger per
contained call and defers its release, and `Admit` files every claim it grants
into the ledger it finds on the context. **A new call path must open a ledger —
claiming without filing is what wedges a breaker permanently.**
