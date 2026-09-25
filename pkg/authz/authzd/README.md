# `pkg/authz/authzd`

Code scoped to the **authzd daemon alone**. Everything else under
[`pkg/authz`](../) is shared by the runner, channelsd and the operator; the
`authzd/` path level is what says "this one is not".

| Package                         | What it does                                                                                                        |
| ------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| [`pipelinehost`](pipelinehost/) | authzd's implementation of `pipeline.Host` — the approval round-trip for the two `ApprovalAsk` kinds authzd raises. |

## The two asks

- **`cold_start`** — publishes the `metaagent_scope_approval` envelope, awaits,
  maps the decision through `coldstart.ActionForDecision` (five actions), then
  applies the scope and writes the `cold_start_task`.
- **`metaagent_scope`** — the mid-session `@metaagent` gate. Same envelope minus
  the `coldStart` key, and a plain two-way approve/deny. It owns no effect: the
  `MetaagentApply` hook mutates scope and converts a deny into a sticky
  `HardDeny`.

## Constraints

- **An await error is a sticky deny, not a halt.** A context timeout or an
  orchestrator failure is returned as a deny with `err == nil`, so the executor
  `Deny`s rather than `Halt`s the turn.
- **The envelope's field names are load-bearing.** channelsd renders the
  approval buttons off them — and branches on the presence of the `coldStart`
  key to pick the 5-button render over the 3-button one. Renaming a field
  silently changes what a user is shown.
- **No `cmd/` imports.** The apply / write / notice closures are injected from
  `internal/cmd/authzd/main.go`, which is why this package can live under `pkg/`
  without forming a cycle with [`../hooks`](../hooks/). Keep new behaviour
  arriving as a closure, not an import.
