# `agent` — the session-to-session channel kind

Every other channel kind connects an AgentSession to something outside the
cluster: Slack, a Bento scheduler, a browser tab, a terminal. `agent` connects
it to **another AgentSession** — the counterparty is named directly on
`Channel.spec.authzSubject`, as an `agentsession:<namespace>/<name>` value, not
resolved through any external directory. `agentsession:` is admitted only on a
Channel of this kind; every other kind's `authzSubject` still accepts only
`service:`. This is what a `task` or `chat` mode subagent
(`v1alpha1.SubagentRequestSpec.Mode`) is bound to so it has somewhere to talk; a
`single_turn` child stays headless and is never given one.

The counterparty is **required**: `ValidateSpec` refuses a Channel of this kind
with an empty or malformed `spec.authzSubject`, because both halves of the kind
are defined by it — the Sender publishes onto the counterparty's inbound
subject, and the field is what says whose traffic this Channel may carry. It is
the only check that demands it; the AgentClass controller's
`userLessChannelMissingAuthz` rule skips every kind whose
`SpawnsSessionOnInbound()` is `false`, which includes this one.

Because the counterparty lives in the same cluster, delivery is a write onto the
AP bus rather than a call to a third-party API — but the write still happens
inside a channelsd pod, not inside the runner or the CLI, so
`RelayedByChannelsd()` is `true` here just like every other channelsd-hosted
kind. Do not read "the destination is internal" as "the transport is
client-hosted" — those are independent questions, and this kind answers them
differently from `local`/`browser`, which answer both `false` for the same
underlying reason (their surface is a terminal or a browser tab, which really
can't run inside channelsd).

`NewSender` is a real implementation, and **nothing in tree drives its message
arm today.** It publishes a `KindAgentMessageSend` envelope
(`pkg/channels/channelevents`) onto the **sending** session's own inbound bus
subject (`ap.session.<sender-ns>.<sender-name>.in.agent_message_send`) via
`Deps.NATSPublish`, carrying the message text and naming the counterparty in the
payload. See `sender.go`'s file comment for why it uses `NATSPublish` rather
than `Deps.Inbound` (the short version: channelsd wires `Deps.Inbound` to nil
for every Sender it builds, and a raw NATS publish is fire-and-forget, so this
Sender's delivery step cannot block on, or be blocked by, anything downstream).

**Why the sender's subject, when this Sender could use either.** It runs inside
channelsd, which holds a cluster-wide NATS credential, so it is the one
publisher that COULD address the counterparty's subject directly. Publishing on
the sender's is what makes the sender _authenticated_: channelsd's
`envelopeHandler` cross-checks `Envelope.Session` against the subject the
publisher was authorized on, so whichever end the subject names is the end the
bus proved. Naming the counterparty instead would leave the SENDER as a payload
claim with nothing but this Sender's own good behaviour behind it — the one
direction of traffic in the system not authenticated by its subject. It is also
the only subject a runner can publish on, so `reply_to_subagent` and this Sender
now emit the same envelope onto the same subscription.

The reason that arm has no producer is a security decision one layer up, not

`Send` accepts NOTHING. The dropped progress kinds are dropped silently; every
other envelope kind, `KindUserMessage` included, is an error.

That is a security decision one layer up, not neglect. `Send` used to accept
`KindUserMessage` — `respond_to_user`'s envelope — and republish it as a
`KindAgentMessageSend`. Nothing produced one: a message arriving that way lands
in the other agent's transcript with no content inspection anywhere, so the tool
is withheld from exactly the sessions bound to this kind (`respondToUserSkip`,
`pkg/agent/tool/meta/capability/channelinteraction.go`, fail-closed — an
unregistered kind withholds too).

The three live directions of a conversational delegation each go elsewhere, and
each is TYPED and separately authorized: the child's question through
`ask_parent` → the request's status, its answer through `return_result` →
`status.result`, and the parent's reply through `reply_to_subagent` →
`KindAgentMessageSend` published by the runner. All three arrive as INSPECTED
tool results. A free-form session-to-session message is the untyped channel that
design replaced, which is why the arm is gone rather than dormant: unreachable
code that would reactivate if a withholding rule changed is a latent re-opening,
and relaxing `respondToUserSkip` later would have brought it live carrying a
path nobody re-reviewed. If the need returns it should arrive as a deliberately
designed direction. Git history keeps the code.

The Sender itself is NOT idle: it is constructed and invoked every turn for a
conversational child, and `progressKindsWithNoAgentSurface` is what keeps five
envelope kinds per turn from becoming per-turn error logs.

**What this means for the relay's delivery-failure fallback.** That fallback
(`deliveryFailureNotice` / `surfaceDeliveryFailure` in
`pkg/channels/channelsd/outbound/relay.go`) retries a failed `Send` through the
SAME sender with a hardcoded, human-addressed English sentence, and gates that
retry on `env.Kind == KindUserMessage`. Both it and the removed arm were dormant
for the same reason, so they were decided together.

Deciding them together is what happened, and the answer for the relay half is
LEAVE IT ALONE: that gate is generic, and slack/browser depend on it. Its
consequence here is now benign and worth knowing when reading logs — a
`KindUserMessage` that somehow reaches an agent-bound session produces TWO
errors, the refusal from `Send` and then "delivery-failure notice also failed to
send; channel unreachable". That is correct rather than a bug: there is no
person on this channel for a failure notice to reach, the counterparty is
another agent. It does not recurse.

`NewListener` returns a no-op, permanently: `channelkinds.Deps` grants a kind
only `NATSPublish` (one-off) and `NATSRequest` (request-reply), never a
subscribe capability or a raw `*nats.Conn`, so a channel-kind package has no
seam through which to subscribe to anything. A published `KindAgentMessageSend`
DOES have a consumer, just not one that lives in this package: channelsd
subscribes centrally — the "agent_message_send" row in
`internal/cmd/channelsd/main.go`, same wildcard-across-every-session shape as
every other inbound kind — and delivers through
`pkg/channels/channelsd/pipeline.Pipeline.HandleAgentMessageSend`, which
resolves the Channel for the `(target, sender)` PAIR and reaches `Deliver`
through it. The subject-authorized sender becomes `InboundEvent.AuthzSubject`,
carried through with no per-user identity attached — the monotonic-identity
property the whole delegation design rests on: a child must never be able to act
as its parent's human.

**One Channel, two ends.** A delegation edge gets exactly one `agent` Channel:
it is bound to the CHILD as `spec.inputChannel`, and its `spec.authzSubject`
names the PARENT. Both directions of the conversation ride it, and which end is
the bound one is the only thing that differs between them — parent→child
resolves the Channel from the target's own binding, child→parent from the
SENDER's. Resolving from the target's own binding in both directions is what the
handler used to do, and it is why a child could never answer a root parent: a
root's binding is `slack` or the local TUI, which admits no `agentsession:`
subject at all.

**This Sender publishes one of those two directions, not both.** It is built
from a session's own `spec.inputChannel`
(`internal/cmd/channelsd/ sender_resolver.go`), and only the CHILD is bound to
the pair Channel, so child→parent is the only direction it can originate. The
other one comes from the parent's runner instead — `reply_to_subagent`
(`pkg/agent/tool/meta/delegate_reply.go`), which publishes the same
`KindAgentMessageSend` envelope on the parent's own subject, because a runner's
per-session NATS grant authorizes publishing on its own prefix and nowhere else
(`runnerNATSUserGrant`, `pkg/controllers/agentsession/controller.go`). Both
directions therefore arrive as the same kind, on the same subscription, at the
same handler; only which session's subject carries them differs.

Because the Channel is bound to the child, its correlation labels
(`LabelChannelName` + `LabelChannelKey`) are on the child too. A child→parent
message therefore cannot be routed by those labels, and `HandleAgentMessageSend`
names the session explicitly via `InboundEvent.TargetSession` — sound because
the destination is corroborated against the pair Channel before anything is
delivered.

## Authorization

**These gates are not this Sender's.** They live below `HandleAgentMessageSend`,
in `deliverAgentMessage` (`pkg/channels/channelsd/pipeline/agent_message.go`),
and run once for every session-to-session message whichever producer published
it. Read this as the gate on session-to-session delivery, not as a gate on a
publish path nothing drives.

One end is proved and the other is claimed, always the same way round: the
SUBJECT names the sender (only that session's JWT may publish there, and
`envelopeHandler` cross-checks the envelope against it), and the payload's `to`
is the claim the pair lookup corroborates.

0. **Is this claimed sender really the other end?** `resolvePairChannel`
   resolves a real, K8s-witnessed Channel whose two ends are exactly the target
   and the claimed sender — one end named on `spec.authzSubject`, the other
   witnessed by that session's own `spec.inputChannel`. Nothing is synthesized
   and no pair is inferred from the payload, so a session holding an `agent`
   Channel to somebody else reaches nobody through this.
1. **May this Channel carry a session subject at all?** `Deliver` asks the
   RESOLVED Channel's own kind, through `channelkinds.SessionCounterparty` — a
   K8s-witnessed fact, never the payload's word. A Channel whose kind never
   declared itself a session counterparty refuses an `agentsession:` subject
   outright. Pair resolution deliberately does not ask this itself: keeping it
   in `Deliver` keeps one choke point rather than two that can disagree.
2. **May THIS session message THAT one?** `agentsession#converse` in
   `pkg/authz/spicedb/schema/schema.zed`, checked on every inbound like any
   other:

   ```zed
   permission converse = parent + child
   ```

   It is exactly one hop in each direction, matching the transport: one Channel
   exists per parent/child pair, so a grandparent, a sibling and a stranger are
   all refused. The two relations are written together by
   `spicedb.Client.TouchLineage` when the delegation is created.

`converse` is a separate permission from `interact` on purpose, and separate in
subject TYPE as well: `interact` admits `user | group#member` only, so no
session can satisfy it, and no human grant reaches `converse`. Nothing on this
path lets an agent act with a person's standing.

A refusal here is a plain refusal — never a join request. `handlePermissionDeny`
is the human flow (blocklist, pending requester, approval card); a sender that
is a process has no identity to put in it and nothing that could click it.

## Human-directed vs conversational traffic

A child talking to its **parent** must reach one hop up the lineage. A child
asking a **human** a question must reach the person who owns the work, which is
further up still — collapsing the two sends either a clarification into a thread
nobody reads, or a permission prompt to an agent that is then positioned to
answer it.

**That split is not implemented in this package, and cannot be.**
`Kind.SubChannelSender(name string, deps Deps) Sender` is handed a _Channel_,
never a session — so a kind has nothing to resolve a lineage against, no matter
which sub-channel it is asked for. It lives in the outbound relay
(`pkg/channels/channelsd/outbound`), which loads the AgentSession before
choosing a Sender:

- **Conversational** traffic — the agent's replies, status, everything not in
  `humanDirectedKinds` — routes to the session's own binding. For a
  conversational child that binding IS this Channel, so its output reaches its
  parent with no walk at all. This is the whole "conversational" half; it needs
  no code here.
- **Human-directed** traffic — the interaction family — resolves through
  `v1alpha1.ResolveHumanDirectedBinding` to the nearest ancestor whose channel
  kind answers `DeliversToHuman() == true`. This kind answers `false`, so a
  conversational child's own Channel is skipped and the card lands on the human
  surface further up. Nothing human-readable anywhere is a loud drop, never a
  fallback onto an agent surface.

`SubChannelSender` therefore returns `nil` for every name, which is the
interface's own documented "this kind doesn't implement that sub-channel"
answer: every sub-channel is a bespoke rendering for a person, and the
counterparty here has no rendering surface at all.
