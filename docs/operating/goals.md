# Private goals

Goals describe outcomes that survive agent sessions. This first slice manages
private goals for a verified human and an AgentClass. Activating a goal or
setting a due timestamp records intent; it does not launch work, send reminders,
or approve tool calls. Employee-bot scheduling will build on this store in a
later change.

Enable goals on a class alongside enforcing information-flow protection:

```yaml
spec:
  capabilities:
    goals: {}
  authz:
    informationLeakage:
      mode: enforcing
```

The agent receives `list_goals`, `get_goal`, `create_goal`, and `update_goal`.
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

## Desktop validation before publishing the PR

Build from the `feat/goals-support` worktree, including its PR #16 slot-pinning
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
`executionAvailable: false`. Repeat the identical create command: the ID and
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
durability. Confirm due timestamps never create sessions or send reminders.

Finally test refusal with goals disabled, information-flow policy set to
logging, a delegated child, and another employee entering the original shared
conversation. Revoking access to a source used in a goal must hide that goal
from later reads; listing can consequently return an empty page with a `next`
cursor that should still be followed.

## Follow-up slices

The remaining implementation plan covers standing execution consent,
transactional scheduling claims, bounded goal activations, and reminder delivery.
Those need fresh checks for offboarding, identity health, class authority, tool
permissions, budgets, concurrency, and notification destinations. No execution
consent is written by the management API in this change.
