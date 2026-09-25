# plangate-call-budget

The focus mechanism, proven end to end.

## What it pins

A phase declares `budget: {calls: 1}` and then makes two governed calls. The
second is refused — and the reason matters: the handle is squarely inside the
approved ceiling, and the FIRST call proved it, so this is not the membership
test firing. The phase has simply spent what it asked for.

That is the only failure a permission model cannot see. An agent grinding forty
calls out of a perfectly legal readonly ceiling breaks no rule at all; a budget
is what catches it.

## Why the assertions are shaped this way

The denial is asserted on the **tool result** — `lastToolResultIsError: true`
plus `lastToolResultContains: "update_plan"` — not on the agent's reply. A
scripted transcript says whatever the bundle tells it to, so a reply mentioning
"budget" would prove nothing about whether the call actually ran.

The message must name `update_plan` and must NOT send the agent to
`select_phase`: selecting another phase cannot restore a spent budget, and a
denial that suggests it would send the agent round the exact loop the budget
exists to break.
