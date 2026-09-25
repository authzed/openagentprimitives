# `pkg/platform/settings`

Folds the four-tier settings chain — cluster → namespace → AgentClass →
AgentSession — into one `EffectiveSettings` plus the `Violation`s that fold
produced. `Resolve` is **pure**: no I/O, no clock, no client. Callers build
`Inputs` from CRDs; the cluster-side loading half lives in
[`../settingswiring`](../settingswiring/).

Two kinds of tier field compose differently, and the distinction is the whole
design:

- a **default** is inherited — the nearest tier that sets one wins, and lower
  tiers may override freely;
- a **limit** is a ceiling that only ever tightens — the strictest across every
  tier wins, and a lower tier can never widen it.

Allowlists intersect, denylists union, pinning takes the strongest mode and the
highest strength floor.

## Files

| File | What it folds |
| ---- | ------------- |
| `types.go` | `Inputs`, `EffectiveSettings`, `Violation`, and the package doc. |
| `resolve.go` | `Resolve` — the top-level fold: budget, model + pricing + routing, toolkit and MCP allowlists, and the rest. |
| `pinning.go` | Pin requirements: strongest mode (`block` > `approve` > `warn` > `off`) and highest strength floor across tiers. |
| `routingmerge.go` | Merges an AgentClass's routing overlay over the catalog-resolved base with **narrowing-only** semantics. |
| `sandbox.go` | Sandbox-backend precedence, most specific tier first, recording provenance. |
| `tostatus.go` | `ToStatus` — converts the resolved struct into the CRD-serializable `v1alpha1.EffectiveSettings` stamped onto class and session status. |

## Constraints

- **Every disagreement comes back as a `Violation`, never as a silent
  adjustment.** A caller decides what is fatal; `Resolve` does not swallow.
- **`Provenance` records which tier supplied each value.** Populate it when you
  add a folded field, or the admin surfaces lose the "why is it this?" answer.
- **Narrowing-only means never falling open.** In `routingmerge`, a disjoint
  intersection falls back to the base rather than emitting empty — for `Only`, an
  empty wire value means "all providers allowed", so emitting empty would fail
  *open*.
- **`ToStatus` output must be accepted by the apiserver.** `status.effectiveSettings`
  mirrors tier ToolGuard policies, so every CEL rule on a type reachable from
  `spec.toolGuard` is also attached to a status path. The
  integration-tagged admission test proves the installed CRD accepts what the
  resolver produces — a rejected status write wedges the reconcile with an error
  naming a field on a different object.
- Keep `Resolve` pure. The moment it reads a clock or a client, the webhook and
  the CLI can no longer call it.
