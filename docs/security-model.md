# The OAP security model

Open Agent Primitives is built so that an agent's authority is decided by the
platform, not by the model. This document is the full inventory of the controls
that make that true: what each one does, why it exists, and where it lives in
the code.

It is organized as six pillars, roughly in order of how much they distinguish
OAP from other ways of running agents. The first four are the reason the project
exists. The last two are the substrate that makes the first four worth trusting.

Each control names the package that implements it, so this document can be
checked against the tree rather than taken on faith. It is hand-maintained:
update it when the code it describes moves. It is a description of the system's
design, not a security certification or an audit result. For a mapping of this
codebase against an external standard, see
[`owasp-agentic-top10-coverage.html`](owasp-agentic-top10-coverage.html).

**How to read the contrast tables.** The left column describes the common
default across agent frameworks generally, not any specific product. It is there
to make the design decision legible, not to score competitors.

---

## 1. Every action is authorized

_Agents follow your organization's real permissions on every call, not a single
shared level of trust._

A permission check at the edge, deciding who may start an agent, stops helping
the moment the agent is running and touching data with many different owners.
Every control in this section is about moving authorization inward, to the
individual action.

### Permission checks on every action

**Every tool call, interaction, memory access, lookup, and preference change
goes through an authorization check.**

Authorization is evaluated against a SpiceDB relationship graph that can mirror
real-world ownership: who owns an account, who is on which team, who shared what
with whom. Relationships can be written from source systems as the agent runs,
so a decision reflects current reality rather than a copy that went stale. When
a check fails, the platform can convert the denial into an approval request
routed to whoever actually holds the authority to grant it, rather than simply
returning an error to the model.

| Typical agent platform                               | OAP                                         |
| ---------------------------------------------------- | ------------------------------------------- |
| Permissions checked when a session starts, if at all | Every action checked against the graph      |
| One trust level for the whole session                | Authority resolved per action, per resource |

Implemented in: `pkg/authz`, `pkg/authz/spicedb`, `pkg/authz/authzd`

### Operation-level access rules

**The same agent can grant different powers to different people.**

Different people legitimately need different things from one agent. Rather than
standing up a separate agent per audience, permissions attach to individual
operations. Low-risk operations can be open to everyone while riskier ones
require a role, a team membership, or an approval. The agent definition stays
the same, and what each person can do with it follows their own authorization.
An operations agent can be read-only for one team and read-write for another.

| Typical agent platform                                   | OAP                                               |
| -------------------------------------------------------- | ------------------------------------------------- |
| One agent, one permission level for all its users        | Per-operation checks that follow each user's role |
| Separate agents per audience to get separate permissions | One agent, many audiences                         |

Implemented in: `pkg/authz/permsurface`, `pkg/tools/toolspec`

### Directory sync

**Group memberships sync from the systems where they are already managed.**

Authorization rules are only as good as the data behind them, and a separately
maintained list of who belongs to which team goes stale quickly. OAP syncs
groups from Slack, GitHub, and 1Password into SpiceDB, so permissions follow
your organization automatically. When someone joins or leaves a group, their
access changes without anyone editing the agent platform. The same data tells
the platform who can see a given channel, which the data-leakage controls in
pillar 3 depend on.

| Typical agent platform                                         | OAP                                        |
| -------------------------------------------------------------- | ------------------------------------------ |
| Permissions maintained by hand, separately from your directory | Existing groups become authorization facts |

Implemented in: `cmd/oap/internal/directorycmd`,
`pkg/channels/channelkinds/onepassword`, `pkg/platform/identity`

### Agent identity modes

**Four modes decide whose permissions an agent acts under.**

The identity an agent acts under determines what it can reach, whose permissions
apply, and who appears as responsible in downstream audit trails. Different
tasks call for different answers, so `AgentClass.spec.identityMode` offers four:

- `agent` — the agent acts as itself. Suits automated work. This is the default.
- `userPassthrough` — the agent acts as the session's user, carrying that
  person's own permissions and attribution. The platform manages per-user
  credentials, so this reflects the actual person in the session rather than one
  shared service token.
- `ask` — the user chooses at session start. The session parks until answered.
- `dynamic` — an isolated recommender model proposes `agent` or
  `userPassthrough` for the session, and the initiating user confirms. The
  recommendation is advisory only; the agent cannot select the more privileged
  identity on its own.

| Typical agent platform                             | OAP                                                                   |
| -------------------------------------------------- | --------------------------------------------------------------------- |
| One hard-coded service token the framework runs as | Real per-user identity, four modes, user confirms the escalating ones |

Implemented in: `pkg/apis/v1alpha1/agentclass_types.go` (`IdentityMode`),
`pkg/platform/identity`

### Multiplayer sessions with approvals

**Joining a session, directing it, and running sensitive tools can each require
approval.**

Once several people share an agent session, who may do what becomes a real
question. OAP treats participation itself as something to authorize. Teams can
work in one session without everyone effectively borrowing the access of the
most privileged person in the room.

| Typical agent platform                                     | OAP                                                          |
| ---------------------------------------------------------- | ------------------------------------------------------------ |
| Sessions are single-user, or anyone in one can do anything | Each participant's access is checked; approvals are built in |

Implemented in: `pkg/authz/check_session.go`, `pkg/channels/channelinteractions`

### Per-user preferences

**Agent settings belong to each user and are gated like any other data.**

Personalization is data, and one user's data should not be readable or editable
by another. Preferences are stored per user as their own memory kind, behind the
same authorization checks as everything else, so one person's settings can never
change how the agent behaves for someone else. Each agent declares which
settings it offers.

| Typical agent platform           | OAP                                                    |
| -------------------------------- | ------------------------------------------------------ |
| Settings are global to the agent | Settings belong to each user and are access-controlled |

Implemented in: `pkg/agent/tool/meta/capability/preferences.go`,
`cmd/oap/internal/preferencescmd`

---

## 2. Prompt injection cannot take the wheel

_Whatever an agent reads, it can only do what a person approved and what the
platform enforces in code._

A model cannot reliably tell instructions apart from data, so any external
content it reads can attempt to redirect it. Every control in this section
accepts that premise and removes the model from the decision instead of trying
to make the model more discerning.

### Plan gating

**The agent must write a plan, approved by a human, before it takes tool
actions.**

Plan gating separates deciding from doing. The agent states up front what it
intends, a person approves that scope, and from then on the platform checks each
action against the approved plan rather than trusting the model's judgment in
the moment. Actions outside the plan fail or require an amendment that a person
approves. Injected text can still change what the model _wants_ to do. It cannot
change what the model is _allowed_ to do.

| Typical agent platform                                            | OAP                                                             |
| ----------------------------------------------------------------- | --------------------------------------------------------------- |
| An injected instruction mid-session can redirect the next actions | The approved plan is the mandate; deviation needs a human's yes |

Implemented in: `pkg/authz/plangate`, `pkg/agent/runner/plangate_*.go`,
`pkg/agent/session/state/plans`

### Slots

**A slot binds a session to a specific resource. A single-occupancy slot cannot
be silently reopened to a different one — a different instance needs a human's
approval, and a slot configured `rebind: never` needs a new session.**

Credentials are usually broader than any single task. A GitHub token might reach
dozens of repositories when the request at hand concerns one. A slot is a named
placeholder for a resource, filled at plan time or on first encounter. By
default a slot is single-occupancy: the first bind writes a non-expiring
`slot_pin` relationship recording the one instance that occupies it, guarded by
an atomic precondition that the slot was still empty. A bind to a different
instance is refused rather than silently added alongside it, so the intended
scope of a task becomes an enforced boundary rather than a hint the agent can
wander past. (A slot can instead be declared `occupancy: multi`, which keeps the
unpinned behavior: each addition is gated on its own and no pin is written.)

A class whose tools bind a constant, class-fixed instance (a literal
`resourceIDTemplate`, not a per-call `resourceIDExpr`) alongside the chosen
instance under the same resource type needs `occupancy: multi` on that slot — or
a split into two resource types — because the pin treats the constant and the
chosen instance as two distinct instances of one type, and the default
single-occupancy slot refuses the second as drift away from the first.

The refusal names a way out, and which way depends on the slot's `rebind`
setting. The default, `rebind: approval`, routes to a plan amendment: the agent
proposes a plan naming the new instance, and if a human approves it, the
approval moves the pin and revokes the old instance's grants in the same atomic
write — the approval card shows the move as current → proposed, with a line
stating plainly that approving it revokes the session's access to the current
instance. `rebind: never` admits no such move; the refusal is unconditional and
retargeting the slot means starting a new session. Neither mode is tripped by a
second permission check on the already-pinned instance, or by a re-grant of that
same instance after its prior grant expired — those are not drift, and they bind
without needing approval.

One honest limitation: the plan-amendment route exists only for a class that
runs behind the plan gate. A class without it has nowhere to propose the
amendment, so for such a class `rebind: approval` behaves like a refusal whose
only remedy is the same as `rebind: never`'s — a new session for the different
instance.

A grant and a pin carry different things and have different lifetimes. The grant
is the actual authority to call tools against the instance, and it expires and
can be revoked by anyone who could have approved it. The pin is only the record
of which instance was committed to, and it does not expire on its own: it is
deleted when the session ends, swept again at admission for any session that is
not a fork (so a reused session name can never inherit a dead predecessor's
commitment), and copied verbatim onto a forked or restarted session rather than
re-asked. A tool can never write a `slot_pin` or `slot_grant_*` relation
directly — those are platform mechanism relations, and the write is refused
before it reaches SpiceDB.

Two limits are worth stating plainly, because the enforcement is real but
narrower than it can sound. A pin is scoped to a resource **type**, not to a
real-world resource: a class that exposes one underlying resource through two
different SpiceDB types — say a URL-keyed git-repo type and an identity-keyed
GitHub-repo type for the same repository — pins each independently, and nothing
stops the two from drifting to different targets at once. And pin equality is
derived-ID string equality: a type keyed by a transform that does not fully
canonicalize spelling (a trailing slash, a `.git` suffix, inconsistent casing)
treats two spellings of the same resource as two different instances, each
eligible to be "first" into the slot. Where an identity-keyed transform is
available — an account or repository ID rather than a URL — prefer it for a
single-occupancy slot.

`AgentSession.status.slotPins` mirrors the current pin for display, and
`oap session show` renders it, but the status field is an observation, not the
enforcement point: SpiceDB's relationship is what every check reads, and the
mirror can lag behind it.

| Typical agent platform                           | OAP                                                                                      |
| ------------------------------------------------ | ---------------------------------------------------------------------------------------- |
| A session can act anywhere its credentials reach | A session commits to one resource; changing it takes a human's approval or a new session |

Implemented in: `pkg/authz/slotspec`, `pkg/authz/slot_grant.go`,
`pkg/authz/slot_pin.go`, `pkg/authz/data_slot.go`,
`pkg/agent/runner/dataslot_*.go`

### Deterministic approval descriptions

**Every approval carries a "what" written by the platform and a "why" written by
the model.**

An approval is only as trustworthy as the description the approver reads. If the
model writes that description, a confused or manipulated model can describe a
dangerous action in harmless terms, and the approval becomes theater. OAP splits
the request: the platform generates the factual description directly from the
call being made, and the model contributes only its reasoning. An approver can
weigh the model's justification while knowing the description of the action
itself is accurate.

| Typical agent platform                                                | OAP                                                              |
| --------------------------------------------------------------------- | ---------------------------------------------------------------- |
| The model writes the whole approval prompt, including what it will do | The action is described by code; the model supplies only reasons |

Implemented in: `pkg/authz/approve.go`, `pkg/agent/runner/approval/summarizer`

### Tool specs, which lens an MCP server or CLI

**Expose only the operations an agent needs, even when the upstream exposes far
more.**

MCP servers and CLIs are built to expose everything a service can do, but a
given agent usually needs a small slice. Narrowing through upstream tokens works
only when the upstream offers fine-grained tokens, and many do not. A tool spec
defines the narrower interface yourself, which operations and which resources,
and the platform enforces it in code at the moment each call runs, where the
model has no say. Because the restriction is independent of the token, it also
acts as a second layer when scoped tokens _are_ available. A read-write
integration can be exposed to an agent as read-only without a read-only
credential existing.

| Typical agent platform                                          | OAP                                                       |
| --------------------------------------------------------------- | --------------------------------------------------------- |
| Whatever the MCP server exposes is what the agent can do        | You declare the subset; the platform enforces it per call |
| Restricting access requires the upstream to mint a narrow token | The restriction holds regardless of the token's scope     |

Implemented in: `pkg/tools/toolspec`, `pkg/authz/toolguard`

---

## 3. Business data stays where it belongs

_Customer context, internal information, and secrets cannot drift to the wrong
people._

### Information-leakage tracking

**Data entering and leaving the system is tagged, and flows are checked before
anything is sent.**

An agent that reads from one place and writes to another can move information to
people who should not see it without anyone intending it, for example by
summarizing a restricted source into a widely shared channel. Tool responses are
tagged with their origin, and the tags travel with the data. When the control is
enabled, the platform compares where data came from against who will receive it
before anything leaves through an output channel, and blocks the flow if it
would cross a boundary. Because the tagging is structural, the check does not
depend on the model remembering where something came from.

The read-side gate is fail-closed by design: every non-meta tool kind is gated
by default, so a future tool kind inherits gating until it is proven safe rather
than escaping it by omission.

| Typical agent platform                                           | OAP                                                    |
| ---------------------------------------------------------------- | ------------------------------------------------------ |
| Once data is in the context window, nothing tracks where it goes | Provenance follows the data and is checked at the exit |

Implemented in: `pkg/authz/untrusted/tags.go`, `pkg/agent/runner/leakage*.go`,
`pkg/authz/contentguard`

### Slot-scoped memory

**Memory is shared only across sessions working on the same resource, and by
default is not shared at all.**

Memory is a leakage channel: anything an agent remembers from one conversation
can resurface in another. In a business setting the right boundary is rarely the
agent or the user, it is the thing being worked on, such as a customer, a
repository, or an account. Scoping memory to a slot makes that boundary
explicit, so context accumulates where it is useful and cannot spill into
someone else's work. The options are no sharing (the default), sharing across
all sessions of an agent, or sharing across sessions bound to the same resource
slot.

| Typical agent platform                                   | OAP                                                                      |
| -------------------------------------------------------- | ------------------------------------------------------------------------ |
| Memory is a broad pool any session of the agent can read | Memory follows the resource, so customer context never crosses customers |

Implemented in: `pkg/memory`, `pkg/authz/scope`

### Secret scrubbing

**Mark a tool output as a secret and the model receives only a handle.**

Anything in a model's context can end up in its output, in a log, or in the
hands of a prompt injection. The safest secret is one the model never sees. A
tool output declared as a secret is captured into a per-session secret store and
the model is handed an opaque handle instead. The model can still direct the
workflow by passing that handle to the next tool, and the platform substitutes
the real value outside the model. Handles are write-once per session. The
default store is Kubernetes Secrets, and the store is pluggable.

| Typical agent platform                                | OAP                                                  |
| ----------------------------------------------------- | ---------------------------------------------------- |
| Secrets returned by tools land in the model's context | The model holds a reference and never sees the value |

Implemented in: `pkg/web/secretoutsrv`,
`pkg/apis/v1alpha1/sidecartoolbox_types.go` (`SecretInputs`),
`pkg/apis/v1alpha1/agentsession_types.go` (`SatisfiedSecretOutputs`)

---

## 4. Least privilege by default

_Agents start with nothing, and every tool holds only the access it needs._

### Opt-in capabilities

**A newly defined agent has no toolkits, no tool specs, and no capabilities.**

Every capability an agent has can be misused, whether through a bug, a careless
prompt, or an injected instruction. Opt-in design inverts the review burden:
instead of auditing an agent to find what should be turned off, you review only
what someone deliberately turned on. A new agent can run an LLM loop with
session history and audit logging, and nothing else. Each capability added
carries its own gating.

| Typical agent platform                                       | OAP                                                   |
| ------------------------------------------------------------ | ----------------------------------------------------- |
| Agents start broad and teams try to remember what to disable | Agents start with nothing and gain only what is added |

Implemented in: `pkg/agent/tool/meta/capability`

### A separate sandbox for every tool

**Each tool runs in its own sandbox and holds only its own credentials.**

Credentials are the most valuable thing an attacker can take from an agent, and
the surest protection is to never put one where it is not needed. Giving each
tool its own sandbox means a credential lives only beside the code that uses it.
If one tool is tricked or compromised, the damage stops at what that tool could
already do; it cannot read another tool's token or reach another system with it.
The orchestrating agent directs its tools without ever holding their
credentials.

Sessions also get network policies. Enforcement is L3/L4: a network mode of
`none` denies all egress, and `allowlist` permits DNS plus coarse TCP. A
hostname-level allowlist is recorded on session status for a DNS-aware policy
controller to consume, and is deliberately not claimed as enforced by OAP
itself.

| Typical agent platform                                             | OAP                                                        |
| ------------------------------------------------------------------ | ---------------------------------------------------------- |
| Tools share one sandbox, so every tool can reach every token in it | Each tool is walled off and cannot borrow another's access |

Implemented in: `pkg/tools/sandboxkinds`,
`pkg/controllers/agentsession/netpol.go`

### Sub-agents request their own permissions

**Sub-agents do not inherit the parent's access.**

Delegation is a common way for agent systems to escalate privilege by accident:
a narrowly scoped task spawns helpers that inherit the parent's full authority.
OAP treats each sub-agent as its own principal. Its permissions are granted
explicitly, within what the plan allows, and each grant is recorded as a SpiceDB
relationship like any other. Splitting work across sub-agents narrows access
rather than copying it.

| Typical agent platform                          | OAP                                             |
| ----------------------------------------------- | ----------------------------------------------- |
| Sub-agents inherit everything the parent can do | Sub-agents get exactly what was granted to them |

Implemented in: `pkg/controllers/subagentrequest`

### Sidecar toolboxes

**Run arbitrary code as an MCP server in its own sandbox, started and stopped
with the session.**

Not every capability fits an existing MCP server or CLI, and custom code is
where security shortcuts tend to creep in. A sidecar toolbox packages any code
as a tool without giving up isolation: it receives only the credential it is
handed and exists only for the life of the session, so no long-running service
quietly accumulates access or state. A toolbox gated on a secret input runs as
its own per-session pod with its own network policy, and a toolbox can request
that isolation explicitly. Concurrent sessions each get their own.

Combining sidecars with separate tools also allows separation of duties, so the
tool that obtains a credential is not the tool that uses it.

| Typical agent platform                                     | OAP                                                                        |
| ---------------------------------------------------------- | -------------------------------------------------------------------------- |
| Custom tools are ad hoc shell-outs in a shared environment | MCP, CLI, and sidecar tools are all first-class, isolated, and short-lived |

Implemented in: `pkg/apis/v1alpha1/sidecartoolbox_types.go`,
`pkg/agent/tool/sidecartoolbox`, `pkg/tools/kinds/sidecartoolbox`

---

## 5. Control and oversight for administrators

_Set policy once, prove what happened, and pull access immediately._

### Inherited controls

**Requirements and defaults are set at the cluster, namespace, or agent-class
level, and flow downward.**

Security settings configured agent by agent drift apart, and one misconfigured
agent is enough. Layered controls let an administrator set policy once, at the
level where it belongs, and have it apply to everything beneath.
Organization-wide rules live at the cluster, team-specific limits such as
allowed models or budgets live at the Kubernetes namespace, and individual
agents inherit both. Agent authors work within those bounds and cannot opt out
of them.

| Typical agent platform                                      | OAP                                                     |
| ----------------------------------------------------------- | ------------------------------------------------------- |
| Each agent is configured on its own and drift is inevitable | Policy flows downhill and is enforced deterministically |

Implemented in: `pkg/apis/v1alpha1`, `cmd/oap/internal/settingscmd`

### Revocation

**Revoke tokens, agent access, tool calls, or any other permission through
SpiceDB.**

When something goes wrong, access has to be removed quickly and everywhere.
Because every grant is a SpiceDB relationship, revocation uses the same
mechanism: remove the relationship and every subsequent check fails. There is no
hunting down copies of a credential and no redeploying agents. A tool call
already in flight is not interrupted, but nothing new starts.

| Typical agent platform                                    | OAP                                             |
| --------------------------------------------------------- | ----------------------------------------------- |
| Pulling access means rotating credentials and redeploying | One revocation takes effect across the platform |

Implemented in: `pkg/authz/revocation`,
`internal/cmd/operator/revocation_subscribe.go`

### Tamper-evident audit log

**Security-relevant history is append-only, signed, and independently
verifiable.**

An audit log is useful for investigation only if you can trust it has not been
altered, including by an attacker covering their tracks. The transcript,
authorization-decision, tool-session, tool-catalog, trigger-delivery, and audit
record kinds are append-only: a write that changes existing content is rejected,
per-entry deletion is refused, and a byte-identical rewrite is idempotent.

Each entry carries a provenance envelope with the publisher, a key ID, a
monotonic sequence number per publisher and scope, the previous entry's hash,
and an Ed25519 signature over a canonical digest. The sequence and hash chain
make modification, fabrication, gaps, and reordering detectable, and with the
tail anchor, truncation too. Session signing keys are minted per session with
the public half anchored on the session's Kubernetes status, which serves as the
witnessed trust root. Verification runs offline:

```bash
oap audit verify <session>
```

| Typical agent platform                          | OAP                                                         |
| ----------------------------------------------- | ----------------------------------------------------------- |
| Logs can be edited or deleted without detection | Modification, reordering, and truncation are all detectable |

Implemented in: `pkg/memory/provenance`, `pkg/memory/publisherkeys`

### Pinning

**Pin which skills, containers, and MCP servers are allowed, down to versions.**

Skills, containers, and MCP servers are supply-chain dependencies. A changed or
swapped dependency can change what an agent does without anyone editing the
agent. Pinning restricts agents to approved components at approved identities.
It can be enforced strictly, so a non-compliant agent refuses to run, or set to
warn while a policy is rolled out. `--pinning-mode` accepts `off`, `warn`,
`approve`, or `block`.

| Typical agent platform                  | OAP                                                     |
| --------------------------------------- | ------------------------------------------------------- |
| Any skill or server version can slip in | Only approved components, at approved versions, can run |

Implemented in: `pkg/authz/pinning`

### Circuit breakers and budgets

**Cap turns, spend, and run time, and stop sessions automatically when checks
fail.**

Agents run in loops, and a loop that goes wrong keeps burning money, time, and
API calls until someone notices. Budgets put hard limits on turns, spend, and
duration, and a circuit breaker denies tool calls after repeated failures.
Monitoring hooks can trip a breaker on a policy violation, so a misbehaving
session stops itself rather than waiting to be caught.

| Typical agent platform                                  | OAP                                             |
| ------------------------------------------------------- | ----------------------------------------------- |
| A looping agent keeps spending until a human intervenes | Hard limits stop runaway sessions automatically |

Implemented in: `pkg/apis/v1alpha1/agentclass_types.go` (`BudgetConfig`),
`cmd/oap/internal/settingscmd/wizard.go`

### Pre- and post-call hooks

**Inspect and enforce your own rules before and after every tool call and agent
call.**

No platform can anticipate every organization's policies. Hooks are a place to
enforce yours deterministically on the way into and out of every call. A common
use is restricting links in agent output to trusted domains, to blunt phishing.
Because a hook can trip a circuit breaker, a violation can stop the session
rather than only leaving a log entry.

| Typical agent platform            | OAP                                                  |
| --------------------------------- | ---------------------------------------------------- |
| Output goes to the user unchecked | Custom policy runs on every call, in both directions |

Implemented in: `pkg/authz/hooks`, `pkg/authz/toolguard/hook_guard.go`

---

## 6. A hardened platform underneath

_Defense in depth, so a failure in one component stays in that component._

### Typed input and output sanitization

**Every content type passes through a handler written for that type's risks.**

Agent output is untrusted content. It can contain whatever the model was
manipulated into producing, and rendering it in someone's browser is a classic
route to session theft. Each content type is handled by a dedicated sanitizer
that understands that format rather than by one generic filter: HTML has its
own, CSS has its own, and archive extraction is bounded against inputs built to
exhaust resources. Rendered HTML runs inside an iframe under a Content Security
Policy and without cookie access, so material that slipped through a sanitizer
still cannot reach the viewer's session.

| Typical agent platform                                                 | OAP                                                              |
| ---------------------------------------------------------------------- | ---------------------------------------------------------------- |
| Generated HTML opens in your browser, where it can steal session state | Nothing reaches a user without passing a type-specific sanitizer |

Implemented in: `pkg/channels/channelassets/html`,
`pkg/channels/channelassets/css`, `pkg/channels/channelassets/svg`,
`internal/cmd/extractord`

### Pod-per-session and component isolation

**The agent session, the operator, and each sanitizer run in separate pods.**

Isolating tools does not help much if the platform itself is one trust zone. OAP
separates the parts that handle untrusted content, such as the session runner
and the sanitizers processing model output, from the parts that control the
system, such as the operator, which runs no untrusted code. Anything touching
untrusted input is assumed breakable and runs in its own disposable environment
scoped to one session or one artifact. Cluster RBAC keeps signing key material
away from components that do not need it.

Where the cluster provides a stronger sandbox, OAP uses it: the `agent-sandbox`
backend targets the upstream `sigs.k8s.io/agent-sandbox` API, which on a
supporting platform such as GKE backs workloads with micro-VMs. The isolation
strength there is a property of that backend, not a claim OAP makes on its own.

| Typical agent platform                                          | OAP                                 |
| --------------------------------------------------------------- | ----------------------------------- |
| The agent runtime and the control plane share a process or host | Every layer is its own blast radius |

Implemented in: `pkg/controllers/agentsession`,
`pkg/tools/sandboxkinds/agentsandbox`

### Authenticated, session-scoped in-cluster messaging

**Each component dials the control-plane bus with its own minted credential,
granting only the subjects it may use.**

Most internal systems assume anything on the cluster network is friendly, which
turns one compromised component into a foothold for impersonating the others.
The OAP control plane runs over an authenticated, TLS NATS bus using
decentralized JWT identity generated once per install. Every client is minted
its own user JWT carrying explicit subject grants, so a session's components can
publish only on that session's subjects. A compromised pod cannot publish onto
another session's subjects, because the server rejects it against the grant
rather than trusting the sender.

The subject grammar is written in one place so a subscription pattern and the
subject it must match cannot drift apart, and grants are asserted in tests by
observation against a real server rather than by string matching, because an
empty allow list means publish-anywhere rather than deny-all.

| Typical agent platform                                  | OAP                                                                |
| ------------------------------------------------------- | ------------------------------------------------------------------ |
| Internal services trust anything on the cluster network | Every client proves identity and holds only its own subject grants |

Implemented in: `pkg/platform/nats`, `pkg/platform/nats/subjects`

### OCI-compliant agent packages

**An agent and its dependencies ship as one identifiable package.**

You cannot govern what you cannot identify. When an agent is a scattered set of
prompts, configuration, and dependencies, it is hard to say exactly what is
running or to control what gets deployed. Bundling each agent into a single
OCI-compliant package gives it a definite identity that works with existing
container tooling, so installation can be gated by an administrator and reviewed
like any other artifact.

| Typical agent platform                                              | OAP                                                        |
| ------------------------------------------------------------------- | ---------------------------------------------------------- |
| An agent is a loose collection of config, prompts, and dependencies | One package per agent, with installation gated by an admin |

Implemented in: `pkg/platform/oap`, `cmd/oap/internal/agentcmd`

---

## What an attacker has to get through

To misuse an OAP agent, an attacker has to get past a plan a human approved, a
slot that cannot be silently reopened — a different instance needs a human's
approval, and `rebind: never` needs a new session — a tool lens that cannot be
widened, an authorization check on every call, and a sandbox that never held the
credential in the first place. No single one of these is sufficient on its own,
which is the point: each assumes the others may fail.

## Related reading

- [`README.md`](../README.md) — what OAP is and how to run it.
- [`PRIMITIVES.md`](../PRIMITIVES.md) — the six primitives and where each one's
  code lives.
- [`owasp-agentic-top10-coverage.html`](owasp-agentic-top10-coverage.html) —
  this codebase mapped against the OWASP Top 10 for Agentic Applications.
- [`AGENTS.md`](../AGENTS.md) — repository conventions, including the
  authorization and audit invariants contributors must preserve.
