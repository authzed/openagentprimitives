# plangate-slot-repoint-reasks

**An approval buys one instance.** Cleared for company 4210, the agent re-plans
the same phase pointing at 4299 — and must ask again.

## What it pins

The re-plan is a **pure re-point**: same handle, same entry budget, same
prerequisites, only the slot's instance moves. That isolation is the whole
fixture. Approval keys on the plan digest, which changes on any edit, so the
phase arrives unapproved and **carry-over** is consulted — the second route to
approval, which asks whether the human's earlier answer already covers what is
being asked and lets the phase run when nothing widened.

`diffSlots` compared slots by **type** alone. It found `crm_company` on both
sides, reported no addition, `Widens()` said nothing widened, and the second
company ran on the first company's approval. An approval obtained for one
target, spent on another — the exact escape the instance axis exists to close.

`approvalPrompts: 2` is the assertion, and it is the only one that can see this:
the run "works" either way, and with the bug the human is simply never asked
about Fabrikam.

## Why it asserts neither `rebuildableFromLog` nor `gatedAgainstFrozenPlan`

Both assume ONE plan. This scenario deliberately has two, and the log holds
both: `rebuildableFromLog` compares the FIRST `plan_approved` record's digest
against the plan rebuilt from the log, which is the LATEST — so a re-plan makes
it fail by construction rather than by defect. `plangate-multiphase` pins
single-plan reconstruction; nothing is lost by leaving it there.

## Why carry-over is reached at all

Worth stating because it looks like the digest should already stop this. It
does — for `PhaseApproved`, which fails on the new digest. Carry-over exists
precisely to soften that: an agent that narrows its own plan after recon should
not cost a human a second click for asking for *less*. The hole was that it
measured "less" without looking at the instance.

## History

Written after the plan-scoped approval work made naming targets up front the
normal case. Before that, plans rarely carried slot ids at all, so the type-only
comparison was almost never wrong in practice — and `Widens()`' own comment had
already predicted how it would break: "a dimension in one and not the other is a
hole in exactly one direction, and it is the direction nobody notices, because
the gate keeps working and simply stops asking."

## What else covers this behavior

- `pkg/authz/plangate/carryover_test.go` —
  `TestCarriesOver_ADifferentInstanceMustReachAHuman` covers the same property
  at unit level, including the deferred→named direction (a phase cleared while
  naming no target cannot pre-clear whichever target it later picks).
- `TestCarriesOver_AnyWideningBlocksIt` enumerates the other digest dimensions;
  the instance axis needs a granted slot to move, so it cannot share that
  fixture's baseline.
