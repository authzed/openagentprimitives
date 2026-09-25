# plangate-phase-approval

The feature's namesake, end to end: a human clears a declared phase **before**
it runs, and the clearance is **durable**.

## What it pins

The phase declares TWO handles against a `maxAutoApproveHandles: 1` budget, and
that is deliberate rather than incidental. An all-readonly phase inside the
session budget auto-approves at tier 0 — the feature working as designed — so a
bundle about human approval has to describe a phase that genuinely needs one, or
it asserts against a card the gradient was never going to raise. Breadth is what
promotes this one: two handles under a budget of one, which is the
"breadth escalates independently of severity" rule doing its job.

The agent declares a phase whose ceiling does hold the reach it needs, so the
membership test passes — and the call is still refused until somebody says yes.
Ceiling and approval are independent gates; this bundle is the one that shows
the second one exists.

The phase is then used **twice**, and that is the load-bearing part. The gate
holds no approval state in memory by design: it folds its append-only log on
every permissioned call. So a decision that never reaches that log leaves the
phase looking unapproved forever, and the second call asks the same person the
same question again. `approvalPrompts: 1` is what distinguishes "approved" from
"re-approved on every call" — a scenario that only ever makes one call passes
either way, which is exactly how the missing decision-writer went unnoticed.

## History

Parked from the day it was written until the interaction-resume registry
landed: the product path worked (the prompt published), but the harness's
decision → resume routing had no case for a newly-registered category, so
`AwaitDecision` waited out its timeout. That was test infrastructure, not gate
behavior. Resume shape now lives on `channelinteractions.Category`, so a
category is wired by declaring it and nowhere else.

## What else covers this behavior

- `pkg/agent/runner/host_approval_plangate_test.go` — the ask publishes rather
  than erroring (the bug that made enforcing HALT the session), the computed
  summary reaches the card, severity styling survives to the channel, and the
  decision is recorded carrying the subset it granted.
- `pkg/authz/hooks/plangate_enforce_test.go` — an unapproved phase blocks on an
  ask; an approved one runs; a denied one is never re-asked; an attached
  Approval carries Allow so a YES actually grants; approval never bypasses the
  ceiling; and the ask carries what the decision is recorded against.
- `pkg/authz/plangate/carryover_test.go` — a partial approval grants only its
  approved subset, in all three widening dimensions.
