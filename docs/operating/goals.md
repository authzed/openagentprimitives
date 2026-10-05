# Private goals

Goals describe outcomes that survive agent sessions. The management tools maintain
private goals for a verified human and an AgentClass. Activating a goal or
setting a due timestamp records intent. A separate human consent can authorize
one bounded future session to deliver a private report or reminder.

Enable goals on a class alongside enforcing information-flow protection:

```yaml
spec:
  capabilities:
    goals: {}
  authz:
    informationLeakage:
      mode: enforcing
```

The agent receives `list_goals`, `get_goal`, `create_goal`, `update_goal`, and
`request_goal_execution`.
It lists first to obtain its private resource reference, then supplies that
reference for writes. Every mutation requires a stable request ID, and updates
also require the expected revision. An identical retry returns the original
accepted result; conflicting reuse or a stale revision returns a conflict.

The lifecycle is draft → active ⇄ paused, followed by completed or cancelled.
A draft may also be completed or cancelled. Terminal goals cannot be reopened.
Completing a goal requires a summary and at least one evidence reference; that
records reported completion, not independently verified external success.
`revise` edits the title, desired outcome, due timestamp, or timezone.

## Ownership and storage

channelsd signs an actor attestation only after accepting authenticated human
input. The browser routes the opening prompt's author through this same trusted
component, without adding a duplicate turn or waking the runner again.
Goal access requires a primary session token, the current attested
human's session permission, a live class with goals enabled, and current goal
permissions. A framework-owned, immutable session pin binds the private domain.
Delegated children, machine-triggered sessions, and sessions without that proof
cannot use the goal API. Another employee entering an existing conversation
cannot switch its private goal domain; they must start their own session.
A class deleted and recreated with the same name has a different goal domain.
During the operator's bounded startup window, an unregistered session bearer
receives a retryable 503 while the token registry is restored. Missing credentials
and registered tokens for another session remain unauthorized.

Goal state, idempotency receipts, and audit intent commit together in the
operator's existing SQLite or PostgreSQL database. The in-memory operator mode
is ephemeral. Goal storage is independent of the memory shadow read-source
switch. Session cleanup does not delete goal rows. There is no goal CRD and no
per-goal Kubernetes object.

Goal content remains untrusted tool output. Resource dependencies from prior
reads are retained with the goal, and later reads and updates recheck those
permissions. Goal results flow through the runner's existing requester and
audience guards; writes also declare their destination to the information-flow
gate. Durable actor proofs and event snapshots are platform-written memory
kinds, hidden from session memory queries. The operator publishes committed
goal events, including a copy of the verified actor envelope, through a signed audit outbox,
preserving the exact signed envelope across retries and restarts. Publication failures are logged and retried.
Goals are not currently indexed in general memory search, so recall goes through
the goal API's source checks.

## Desktop validation

Build from the local `feat/goal-execution` branch in the `goals-support` worktree, including its PR #16 slot-pinning
base. Use the normal desktop image workflow to load the updated operator,
channelsd, and runner, and build the CLI with `mage build:oap`. Existing desktop
data should be kept: the database migration is additive. The new scaffold schema
must be applied by the updated operator before testing.

Enable the class configuration above. Start a fresh conversation through the
browser or another human channel; a session created directly with kubectl has
no channelsd actor proof. Substitute the real session name below:

```sh
bin/oap --context oap-desktop goals list <session>
bin/oap --context oap-desktop goals create <session> \
  --request-id desktop-agenda-1 --title 'Meeting agenda' \
  --outcome 'Prepare the agenda for the next meeting'
```

The response contains a draft goal ID and revision 1, with
`executionAvailable: true` on durable installs with authorization and NATS
configured (false on the in-memory backend). Repeat the identical create command: the ID and
revision must remain unchanged. Reusing the request ID with different content
must fail.

```sh
bin/oap --context oap-desktop goals update <session> <goal-id> \
  --request-id desktop-activate-1 --revision 1 --action activate
bin/oap --context oap-desktop goals get <session> <goal-id>
```

Activation produces revision 2. A different mutation still using revision 1
must fail without changing the goal. Exercise pause, resume, revise, and cancel
with the newly returned revision each time. For a separate goal, complete with
`--action complete --summary 'Agenda prepared' --evidence artifact:agenda`.
A completed or cancelled goal must reject further changes.

Ask the agent to create and recall another goal in chat. Verify tool results,
including any existing plan approvals, rather than relying on the wording of its
reply. Start a new conversation with the same human and class: it must list the
same goals. Restart the operator and repeat that check to verify desktop SQLite
durability. Confirm a due timestamp without execution consent never launches a session.

Finally test refusal with goals disabled, information-flow policy set to
logging, a delegated child, and another employee entering the original shared
conversation. Revoking access to a source used in a goal must hide that goal
from later reads; listing can consequently return an empty page with a `next`
cursor that should still be followed.

## One-time private execution

`request_goal_execution` proposes one execution of an active goal. Its terms
include a UTC due time and expiry, finite duration/turn/token ceilings, approval
timeout, evidence requirements, and `allowedOperations: [respond_to_user]`.
The server pins the current class specification, skill commits, source session
UID, owner catalog UID, channel UID, routing binding and recipient. A signed
operator card presents timing, limit, action and private-recipient rows, with
goal text and evidence in an inert excerpt. Internal version and routing pins
remain in its signed details. Only the verified human addressee can authorize it; channelsd
retains the signed request inside its signed decision. Human client-hosted clicks
travel on a component-only NATS subject outside both current and legacy runner
grants; bounded sessions reject the session-scoped decision path. An agent-written card,
conversation message or approval record cannot replace that decision.

Approval creates an expiring, revocable SpiceDB grant for the exact consent
digest. Both the durable decision and current grant are required. Every goal
management mutation clears consent, including pause, resume and edits. Current
start permission, source access, class version, private channel route and owner
catalog are checked again before activation and immediately before reporting.
`UserIdentity.spec.suspended: true` prevents future activation and stops current
goal work. Deleting the catalog also refuses execution. A class may expose a
boolean `goal_execution_enabled` user preference; when declared, its resolved
value must be true, including any locked namespace policy.

SQLite and PostgreSQL schema version 2 provide deterministic occurrence and
session identity, fair due batches, fenced worker leases and atomic owner/class
capacity reservations (one per owner, four per class). Worker takeover preserves
the session UID. A missing known session is an unknown outcome and is never
recreated. Cancellation releases capacity only after the session and its runner
pods disappear. Ledger transitions use the signed durable audit outbox.

Execution creates an operator-owned root session with reviewed finite budgets,
user passthrough, the reviewed private route on both channel bindings, and a
fresh enforcing plan. Its report tool has an explicit
permission and cannot run outside an approved phase. Only that report tool and
local plan/completion controls are exposed. The admission gate prevents forged
execution references, forks, identity overrides, changed specifications and
owner takeover; the controller validates the durable claim before credentials
are materialized.

This slice supports private single-human channel kinds through their registry
contracts. For browser channels, the approval and report appear in the new
`goal-*` session; open that session from the sessions page when it starts.
A session finishing does not complete the durable goal. Completion remains a
separate evidence-bearing management update. External tools, recurring triggers,
structured result proposals and delivery receipts are subsequent slices.

### Desktop execution test

Build in `.claude/worktrees/goals-support` with `mage desktop:devapp` and retain
the existing desktop data. For an existing test VM, leave the current desktop
running after the build: launching the rebuilt app can re-stage its VM disk.
Import the rebuilt images into the running VM, refresh operator, channelsd,
webd and runner, and apply the updated install bundle through the normal install
path. This refreshes the CRDs, admission gate, certificate bundle and webd NATS
grants without replacing the current cluster. A binary-only replacement is
insufficient.

Use a private browser class with goals enabled, `identityMode: userPassthrough`,
no tool bundles, MCP servers or sidecar toolboxes, no widened session interact
policy, and the following authorization settings:

The human's `UserIdentity` catalog must exist before requesting execution. An
empty catalog is sufficient for a report-only class; it supplies the immutable
owner UID and administrator-controlled suspension state.

```yaml
authz:
  informationLeakage:
    mode: enforcing
  toolCalls:
    mode: enforcing
  planGate:
    mode: enforcing
    requirePlan: true
    rendering:
      maxAutoApproveHandles: 0
```

1. In a fresh human chat, create and activate a goal to remind you to stretch.
   Ask for one execution five minutes from now, expiry ten minutes from now,
   duration 180 seconds, ten turns, 10,000 tokens, and approval timeout 90 seconds.
   Permit only `respond_to_user`, with a private reminder as the required evidence.
2. Check the operator consent card and authorize it. Approval should survive an
   operator restart and create exactly one new `goal-*` session when due.
3. Open that session promptly (within its 90-second approval window) and approve
   its fresh plan. Confirm the private reminder and
   inspect its finite budget. The original goal should remain active.
4. For a second goal, authorize execution and then pause or cancel before it is
   due. No runner should start. Resume requires a new execution request.
5. Deny another request and confirm it never launches. Separately, suspend the
   owner catalog or revoke the execution grant before due time and confirm
   refusal. Restore the catalog afterward. Never substitute a fabricated click
   for the real browser approval.

The next slice adds evidence-bearing result proposals and delivery receipts,
then external-operation idempotency and recurring employee-bot work.
