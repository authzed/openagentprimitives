# plangate-plan-scoped-approval

**The plan is the approval surface.** One card shows every phase with the
concrete resource it will touch, the user answers once, and the run proceeds.

## What it pins

Two phases, each naming a **different** company. That difference is the whole
fixture: a phase's authority key covers the instances it names, so two phases
naming the same target would share a key and *one* approval would already cover
both — the scenario would pass without the feature. Different targets means
different authority, which is exactly the case per-phase cards charged twice
for.

`approvalPrompts: 1` is the assertion. Before the decision boundary moved, this
run raised two cards: the second phase was unapproved when it became active, so
the human answered the same question again — and the first card said nothing
about the second phase's target, so at the moment of the only decision they were
actually shown, they could not see what they were agreeing to.

`cardWhatContains` covers the other half, which a prompt count cannot see. One
card is worthless if it names one phase: the What must carry **both** phases and
**both** company ids, or the approval is broader than the thing the approver
read.

`cardWhatOmits` holds the trust boundary at the seam. The whole-plan renderer is
the first thing with reason to print several agent-authored labels at once, and
the computed half is the part the approver is told to trust — so the labels stay
out of it and appear, attributed, in the agent's own section.

## Why each phase needs a human at all

Both phases are a single readonly handle, which auto-approves at tier 0 and
would raise no card. Each also **requests a slot**, and approving a slot request
writes a SpiceDB grant on somebody's resource — no amount of readonly-ness makes
that free. That is what promotes both phases past tier 0, and it is the same
mechanism `plangate-slot-request-tier` covers on its own.

## What else covers this behavior

- `pkg/authz/hooks/plangate_planscoped_test.go` — the card shows every phase,
  the ask carries each covered phase's own grant, severity is priced at the
  worst phase, coalescing is per plan, and the ask always clears the phase that
  raised it.
- `pkg/agent/runner/host_approval_plangate_test.go` — one yes writes one record
  per covered phase, slot delegation is bounded per phase, and a no is still a
  single denial.
- `pkg/authz/plangate/card_wholeplan_test.go` — the renderer itself, including
  the 200-iteration trust-boundary property test.
