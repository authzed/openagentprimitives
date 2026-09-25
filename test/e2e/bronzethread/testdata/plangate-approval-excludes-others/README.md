# plangate-approval-excludes-others

An approved plan phase reaches exactly the instance it named, and the
approval it writes is bounded by what the person who clicked can personally
speak for.

## What it pins

The plan declares one phase with one slot: `crm_company` id `4210`
(Northwind). Nothing else is ever named. The human clears the phase once —
`autoApprove: ["plan_phase", "plan_amendment"]`, no `tool_approval` — and the
agent then calls the SAME gated tool
(`centerdot_list_contacts_for_company`) against a SECOND company of the same
type, `4299` (Fabrikam), that the plan never mentioned. That second call is
refused.

The approver's standing is seeded, on `4210` and nothing else, by
`manifests/approver-standing.yaml`. That is not scenery: a plan-gate approval
binds only what its approver holds (`approverCanDelegateSlots`), and the
approver of a `plan_phase` card is the session approve-set — i.e. the
requester, `user@example.com`, who owns nothing in the shared fixture seed.
Without the seed the delegable set is empty, nothing is bound, and the run
diverges on **`4210`'s** call rather than on `4299`'s (verified by deleting
the entry from `extraManifests`: `crm_company:4210#contact_access` comes back
`denied`, a second approval card appears, and the tool result is a
`SYSTEM_TIMEOUT`). So the green here is a statement about the production
wiring, not about a filter that happened to be switched off.

Nothing else in this fixture would refuse it for an uninteresting reason:

- `list` is wildcard-open (`crm_company:any#any_user`) for every user, so
  breadth of company knowledge isn't the gate.
- `4299` has its own real, resolvable owner (`owner-2`, seeded the same way
  `04-spicedbbootstrap.yaml` seeds `owner-1`) — an owner who genuinely
  *could* grant it, given a click. This bundle deliberately never gives them
  one: `tool_approval` is left out of `autoApprove`, so the JIT ask raised
  for `4299` is left to expire against the class's `approvalTimeout: 3s`
  override (mirrors `contacts_owner_timeout`'s `approvalTimeoutOverride`).
- The plan gate's own ceiling check is a TYPE-level membership test
  (`permsurface.Handle`, i.e. `perm:contact_access:crm_company`) — it does
  not look at the resource id at all, so it would happily wave a call to
  `4299` through once `contact_access:crm_company` is on the approved
  ceiling. It is not what stops this call.

## What is actually refusing `4299` — read this before trusting the name "narrows"

`4299` is refused because **nothing in this run ever gave the session access
to it**, not because a session-scope document excluded it. `contact_access`
on this fixture's schema has no wildcard leaf
(`pkg/e2e/testdata/agent-centerdot-companies/02-mcpserver.yaml`): it is
`slot_grant_contact_access->interact + owner`. Neither leg resolves for
`4299` — the phase never named it, so no grant was written, and it belongs
to `owner-2`, so the owner leg is somebody else's.

## What this bundle can NOT isolate: the grant, on `4210`

`4210` now satisfies `contact_access` **two** ways: the grant the approval
wrote, and the `owner` leg the seeded standing gives the requester. So a
green run here is not evidence that the grant alone would have carried the
call.

That is structural, not an oversight. A `plan_phase` card's decider policy is
`DecideApprovers` (`pkg/channelinteractions/categories`), which resolves to
the session approve-set — the requester. The approver must hold the slot's
permission on the named instance for anything to be delegated at all. So in
any single-user plan-gate scenario the approver's standing IS the requester's
standing, and a grant conferring it is necessarily redundant for that one
user. (A plan-gate grant earns its keep for the session's OTHER members, a
shape this bundle does not have; seeding an `agentsession#owner` co-owner to
manufacture one is explicitly forbidden — see the note above
`Harness.SessionRef` in `pkg/e2e/conversation.go`.)

What isolates the grant instead:

- `pkg/agent/runner/host_approval_plangate_test.go` —
  `...standingOnOneInstanceDoesNotGrantAnother` and
  `...planScopedApprovalBindsOnlyTheInstancesTheApproverHolds` assert the
  binding directly, including the case where type-level standing would have
  bound an instance the approver has none of.
- `slot-approval-binds-one-company` / `slot-approval-by-second-user` — the
  JIT path, where requester and approver ARE different people, so the grant
  is the only thing that can explain the requester's access.

## What `narrowToApproved` does on every approve

`narrowToApproved` (`pkg/agent/runner/host_approval.go`, via
`authz.BindApproved` in `pkg/authz/bind.go`) does two things:

1. Writes the instance-scoped SpiceDB grant described above. Before this
   slice, `recordPlanGateDecision` wrote only an audit record and called no
   grant path at all — a plan-gate approval was **authorization-inert**, and
   any permissioned call after "approval" still depended entirely on some
   other path (an owner tuple, a class default) to ever succeed.
2. Records a matching `scope.Resources` entry in `session_scope` (Layer 2).
   **This half is written but not yet read.** `scope.CheckScopeWithRefs` —
   the function that would check a resolved resource ref against
   `scope.Resources` — has **zero non-test callers**. Both live enforcement
   points call the ref-less form instead:
   - `pkg/authz/check_tool_call.go:60` — `scope.CheckScope(in.SessionScope,
     p.ToolName, jsonArgs(in.Args))`
   - `pkg/authz/hooks/scope.go:89` — `scope.CheckScope(sc, in.Tool.Name,
     jsonArgs(argsMap))`

   Neither ever passes a `[]scope.ResourceRef`, so the per-instance narrowing
   `scope.CheckScopeWithRefs` implements is simply never consulted at
   dispatch. The write is real and durable (see
   `pkg/authz/bind_approved_test.go` and
   `host_approval_plangate_test.go`'s `...NarrowsScope*` tests, which assert
   the `session_scope` document directly) — it just has no reader yet.

**This bundle cannot and does not isolate Layer-2 scope narrowing** as a
distinct enforcement mechanism. Doing that would require a fixture where
`4299` is reachable through some OTHER grant (an owner tuple for `4299`
pointing at the same subject, say) so that only a live `scope.Resources`
check — not "no grant exists" — could explain a denial. On current code,
adding that owner tuple would make `4299` **succeed** (since nothing reads
`scope.Resources` to override it), so that fixture is parked, not built
here — it belongs with whatever slice wires `CheckScopeWithRefs` into
`check_tool_call.go` / `hooks/scope.go`, which is its own design decision
(new enforcement behavior for every session and every existing binding
source, not a one-line call) and out of scope for this task.

## `neverAllowed` is still the load-bearing assertion

`authz.decisions` alone would pass on "denied, then approved, then allowed"
just as readily as on "never allowed" — the two only coincide by accident.
`neverAllowed: ["crm_company:4299#contact_access"]` is the strong form: at
no point in this run did SpiceDB ever say yes to that pair. That claim is
true and useful regardless of which mechanism (grant-scoping today,
scope-narrowing once wired) is doing the excluding — it just is not, by
itself, evidence of which one.

## Why the earlier plan-gate bundles don't already cover this

`plangate-phase-approval` and `plangate-plan-scoped-approval` both prove
the CARD mechanics (ceiling, breadth-forces-approval, one-ask-covers-many-
phases) — but neither ever calls a `contact_access`-gated tool. Both only
ever call `centerdot_list_companies`, which is wildcard-open and would pass
identically whether or not any approval, or any grant, had ever happened.
This bundle is the first plan-gate scenario that drives `narrowToApproved`
into an actual SpiceDB `Check` on a gated tool.

## The revert this bundle does detect, verified by hand

Deleting `"approver-standing"` from `extraManifests` — leaving the approver
with standing on nothing, which is what every run looked like while the e2e
factory left the standing hooks unwired — fails the bundle hard:

```
final outcome for "crm_company:4210#contact_access" (sequence: [denied];
  messages: [permission denied: user:dXNlckBleGFtcGxlLmNvbQ does not have
  contact_access on crm_company:4210])
a cleared phase must be asked about ONCE; a second card means ...
llm step 3: the model was handed a different tool result than the transcript
  describes  (SYSTEM_TIMEOUT, not contacts)
```

So the delegable filter is live in this run and the approval is bounded by
it. Note what this does NOT detect: reverting the
`h.narrowToApproved(ctx, rec, requested, granted)` call itself no longer
changes the outcome here, because the seeded `owner` leg carries `4210` on
its own — see "What this bundle can NOT isolate" above.

## What else covers this behavior

- `pkg/authz/bind_approved_test.go` — `BindApproved` writes the
  `scope.Resources` entry and records `scope.SourceApproved`. Asserts the
  write directly, not through a live `Check`.
- `pkg/agent/runner/host_approval_plangate_test.go` —
  `TestAwaitDecision_planGateApprovalNarrowsScope` and
  `...NarrowsScopeAcrossDifferingCoveredInstances` — same, the
  `session_scope` write in isolation.
- `pkg/agent/runner/host_approval_plangate_test.go` —
  `...standingOnOneInstanceDoesNotGrantAnother` and
  `...planScopedApprovalBindsOnlyTheInstancesTheApproverHolds` — the
  per-instance standing question, including the case where the type-level
  answer disagrees with it.
- `pkg/e2e/bronzethread/testdata/slot-approval-binds-one-company` — the
  same grant-scoping property for the JIT tool-approval path (Task 3)
  instead of the plan-gate path (Task 2); same caveat about Layer-2
  narrowing applies there too.
