package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,spicebox},shortName=apch
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Kind",type="string",JSONPath=".spec.kind"
// +kubebuilder:printcolumn:name="AgentClass",type="string",JSONPath=".spec.agentClass"
// +kubebuilder:printcolumn:name="Scope",type="string",JSONPath=".spec.sessionScope"
// +kubebuilder:printcolumn:name="Connected",type="string",JSONPath=".status.conditions[?(@.type=='Connected')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
//
// Channel binds one transport conversation -- a Slack channel, a scheduled
// Bento feed, a terminal, a browser view -- to one AgentClass, and declares
// how inbound messages correlate to AgentSessions via spec.sessionScope.
//
// Namespaced. Reconciled by pkg/controllers/channel, which validates the
// referenced Secret, AgentClass and AgentIdentity and owns the Valid
// condition. Transport is deliberately not the operator's: channelsd owns the
// Connected condition and patches it through the status subresource.
type Channel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ChannelSpec   `json:"spec,omitempty"`
	Status ChannelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ChannelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Channel `json:"items"`
}

type ChannelSpec struct {
	// Kind selects the kind impl. "fake" is an in-process test kind;
	// "slack" and "bento" are channelsd-hosted; "local" is the
	// client-hosted TUI kind driven by `oap agent chat`; "browser" is the
	// client-hosted kind for a session a browser is looking at; "github" is
	// received by webhook at webd rather than relayed by channelsd; "agent"
	// is the channelsd-hosted kind whose counterparty is another
	// AgentSession in this cluster, not an external surface.
	// +kubebuilder:validation:Enum=fake;slack;bento;local;browser;github;agent
	Kind string `json:"kind"`

	// Role declares whether this Channel handles inbound messages, outbound
	// responses, or both; `monitoring` is a framework-event sink bound to no
	// agent.
	//
	// `output` names this Channel's DELIVERY obligation, not deafness. On a kind
	// that has a listener (slack, bento, …) a role=output Channel still runs an
	// inbound listener — only `monitoring` is listener-less — and an inbound
	// message on it routes to a same-channel-reply session (it is not treated as
	// a split-channel input, which only role=input triggers). reviewbot relies
	// on this: its slack Channel is role=output — the webhook flow's delivery
	// target, resolved by outputbind, which refuses role=both — AND the surface a
	// human uses to REQUEST a review. See pkg/channels/channelsd listener
	// reconcile (monitoring is the only skip) and the inbound pipeline's
	// role=input special-case.
	// +kubebuilder:validation:Enum=input;output;both;monitoring
	// +kubebuilder:default=both
	// +optional
	Role string `json:"role,omitempty"`

	// AuthzSubject is the non-human SpiceDB subject this Channel carries.
	// Format: "service:<id>" or "agentsession:<namespace>/<name>". A
	// "user:"/"group:" value is forbidden: on the kinds that assert it
	// the value becomes the identity the session ACTS AS, so an
	// unconstrained one would let channel config impersonate a person or
	// a group.
	//
	// What it MEANS is per-kind, and the two are not variations of one
	// thing:
	//
	// "service:" is the ACTING SUBJECT asserted for an inbound with no
	// human to attribute to. Required for any kind that cannot attribute
	// an inbound message to a human — bento (cron-spawned) and github (a
	// pull request's author has no AP identity) today; the Channel
	// controller refuses such a Channel without it, off the kind's own
	// UserAttributable() declaration rather than a per-kind list. Optional
	// for slack, which falls back to its existing per-user subject
	// resolution.
	//
	// "agentsession:" names the COUNTERPARTY END of a session-to-session
	// conversation. It is a pair-resolution input and never the acting
	// subject: channelsd resolves an agent message's Channel by requiring
	// this to name the other end of the (target, sender) pair, and
	// attributes the message to the session that SENT it — which on a
	// child-to-parent message is the end this field does not name.
	// Required when kind=agent; an inbound asserting an agentsession
	// subject over a Channel of any other kind is refused by the
	// channelsd pipeline.
	//
	// It does NOT become the session's started_by, on either kind. That
	// relation is user-typed in the SpiceDB schema, so a non-human acting
	// subject suppresses both the started_by write and the started-by
	// annotation rather than writing this value into them.
	//
	// Enforced by the Pattern below (apiserver) AND re-checked
	// fail-closed, per-kind, in the channelsd pipeline via
	// authz.ValidateSubject. Examples: "service:hubspot-digest-bot",
	// "agentsession:demo-ns/lead-1".
	// +kubebuilder:validation:Pattern=`^((service|agentsession):[A-Za-z0-9._@/-]+)?$`
	// +optional
	AuthzSubject string `json:"authzSubject,omitempty"`

	// AgentClass names the AgentClass in the same namespace this Channel
	// is bound to. 1:1 invariant — multi-agent routing is the future
	// meta-agent's job, not Channel's. Immutable post-bind. Required for
	// every role except "monitoring" (a monitoring Channel is a
	// framework-event sink, not bound to an agent); the Channel
	// controller enforces presence for non-monitoring roles.
	// +optional
	AgentClass string `json:"agentClass,omitempty"`

	// AgentIdentity overrides AgentClass.spec.agentIdentity at session
	// creation. Frozen onto the AgentSession at bind time.
	// +optional
	AgentIdentity string `json:"agentIdentity,omitempty"`

	// SessionScope governs how channelsd correlates inbound messages to
	// AgentSessions.
	// +kubebuilder:validation:Enum=auto;thread;user;singleton
	// +kubebuilder:default=auto
	SessionScope string `json:"sessionScope,omitempty"`

	// CredentialsRef points at a Secret in the same namespace holding the
	// kind-specific tokens. Required keys depend on kind.
	CredentialsRef ChannelCredentialsRef `json:"credentialsRef"`

	// Slack carries kind-specific config for kind="slack": the connection
	// mode, the resolved bot user, and the app this Channel's credentials
	// belong to. Empty/nil for other kinds.
	// +optional
	Slack *SlackChannelConfig `json:"slack,omitempty"`

	// Fake carries kind-specific config for kind="fake". Empty/nil for other
	// kinds; test-only, never appropriate in production.
	// +optional
	Fake *FakeChannelConfig `json:"fake,omitempty"`

	// Bento carries kind-specific config for kind="bento". Empty/nil
	// for other kinds.
	// +optional
	Bento *BentoChannelConfig `json:"bento,omitempty"`

	// Metaagent overrides scope-management behavior for this channel.
	// When the bound AgentClass has spec.authz.scope.enabled=true, the
	// AgentSession controller auto-invites the metaagent bot to this
	// channel unless this override sets Enabled=false. Pointer-bool so
	// the absence of the field is distinguishable from explicit false.
	// +optional
	Metaagent *ChannelMetaagentSpec `json:"metaagent,omitempty"`

	// Owner declares how sessions spawned via this Channel get their
	// agentsession#owner. Optional. Absent ⇒ owner is the channel kind's
	// starting user (slack/local/cli); a kind that provides no starter
	// (bento) then requires Owner.Ownerless or it is rejected at validation.
	// +optional
	Owner *ChannelOwnerPolicy `json:"owner,omitempty"`

	// ChannelHistory opts this Channel's bound agent into reading the whole
	// channel's prior history (not just its own thread) via the
	// read_channel_history tool. Requires the channel kind to implement
	// ChannelHistoryReader (slack does; the channel controller rejects
	// enabled=true on kinds that don't). Absent/nil ⇒ disabled.
	// +optional
	ChannelHistory *ChannelHistorySpec `json:"channelHistory,omitempty"`

	// Attachments opts this Channel's bound agent into ingesting files users
	// attach to messages. Requires the channel kind to implement
	// AttachmentFetcher and the bound AgentClass to grant the "attachments"
	// capability; all three must agree before any byte is fetched. Absent/nil
	// means disabled — the agent still learns a file was attached and says it
	// cannot read it.
	// +optional
	Attachments *ChannelAttachmentsSpec `json:"attachments,omitempty"`

	// GitHub carries kind-specific config for kind="github".
	// +optional
	GitHub *GitHubChannelConfig `json:"github,omitempty"`
}

type ChannelCredentialsRef struct {
	// SecretName is the Secret name in the same namespace as the Channel.
	SecretName string `json:"secretName"`
}

// SlackChannelConfig is the kind-specific config for a Channel of
// kind="slack": how channelsd connects, which bot user the credentials
// resolved to, which Slack app they came from, and where an unsolicited
// message lands. Its presence is also what the "exactly one kind block
// matches kind" validation checks for.
type SlackChannelConfig struct {
	// Mode is how channelsd connects to Slack.
	// +kubebuilder:validation:Enum=socket
	// +kubebuilder:default=socket
	Mode string `json:"mode,omitempty"`

	// BotUserID is auto-resolved by channelsd from auth.test on first
	// connect when empty.
	// +optional
	BotUserID string `json:"botUserId,omitempty"`

	// AppID is the Slack app this Channel's credentials belong to, recorded
	// when `oap channel create` provisioned the app itself. Empty for a Channel
	// whose tokens were pasted: nothing in the run knew which app they came
	// from, and no Slack API can be asked.
	//
	// Stable desired state, not an observation: it is written once from what
	// the run created, nothing recomputes it, and a byte-identical re-apply is
	// a no-op.
	// +optional
	AppID string `json:"appId,omitempty"`

	// OutputDefaults govern where the FIRST outbound message of a
	// session lands when the session was not triggered by an
	// inbound Slack message.
	// +optional
	OutputDefaults *SlackOutputDefaults `json:"outputDefaults,omitempty"`
}

// FakeChannelConfig configures the in-process test kind.
type FakeChannelConfig struct {
	// Echo makes the fake re-deliver each outbound message as an inbound one,
	// giving tests a pure round-trip.
	// +optional
	Echo bool `json:"echo,omitempty"`
	// OrgScoped makes the fake kind attribute org membership on this channel,
	// so a scenario can exercise the session-start gate: injected identities
	// carry their OrgMembership stamp verbatim, and an unstamped one reads as
	// guest at the pipeline (fail-closed).
	// +optional
	OrgScoped bool `json:"orgScoped,omitempty"`
}

// BentoChannelConfig carries kind-specific config for kind="bento".
type BentoChannelConfig struct {
	// Generate is a Bento `generate` input config. Exactly one
	// input-shape sub-block is allowed per Channel.
	// +optional
	Generate *BentoGenerateConfig `json:"generate,omitempty"`
}

// BentoGenerateConfig is the per-Channel projection of Bento's
// `generate` input. The bloblang `mapping` runs at trigger time;
// assign a string to `root` (e.g. `root = "send the weekly digest"`)
// and that string becomes the inbound user prompt; optional
// `root.routing_key` lets the mapping force a specific channelKey
// (default is `cron:<channel-name>:<ts-nanos>`, one new session per
// firing).
type BentoGenerateConfig struct {
	// Mapping is the bloblang script Bento evaluates per tick.
	// Assign a string to `root` (e.g. `root = "send the weekly
	// digest"`); that string becomes the inbound user prompt.
	// Avoid `root.message = "..."` — the forward output passes the
	// whole `root` value through as bytes, which means a struct-
	// shaped root becomes JSON and confuses the LLM. Optionally set
	// metadata key `routing_key` to force a specific channelKey
	// (default is `cron:<channel-name>:<ts-nanos>`, one new session
	// per firing).
	Mapping string `json:"mapping"`
	// Interval is a Bento interval string: a duration ("168h") or
	// a cron expression ("@every 24h", "0 9 * * MON").
	Interval string `json:"interval"`
	// Count caps total emissions; 0 (default) = unbounded.
	// +kubebuilder:default=0
	// +optional
	Count int64 `json:"count,omitempty"`
}

// GitHubChannelConfig carries kind-specific config for kind="github".
// It is deliberately INGRESS-ONLY. The Check Run name is not here because the
// agent writes the Check Run; Slack settings are not here because the output
// surface is a separate Channel CR.
type GitHubChannelConfig struct {
	// AppSlug is the GitHub App's slug, recorded so the wizard and the
	// drift check can build install and settings URLs. The App's identity
	// and keys live in the credentialsRef Secret, never in spec.
	AppSlug string `json:"appSlug"`

	// Repositories optionally narrows which repos produce reviews.
	// Empty means every repository the App installation grants.
	// +optional
	Repositories []string `json:"repositories,omitempty"`

	// Events are the pull_request actions that become a review.
	// +kubebuilder:default={"opened","synchronize","reopened","ready_for_review"}
	// +optional
	Events []string `json:"events,omitempty"`

	// SkipDrafts drops draft pull requests. Nil means true: drafts are
	// skipped unless the Channel opts in with an explicit `skipDrafts: false`.
	//
	// A *bool with no default marker, NOT a bool with +kubebuilder:default=true.
	// That combination is unsettable: `false` is the zero value, omitempty
	// elides it, the API server sees an absent field and the defaulter restores
	// true - so the one value a user reaches for when they DO want draft PRs
	// reviewed is the one value the field cannot hold. Same shape as
	// ChannelMetaagentSpec.Enabled in this file.
	// +optional
	SkipDrafts *bool `json:"skipDrafts,omitempty"`
}

// SlackOutputDefaults governs where the FIRST outbound message of
// a session lands when the session was not triggered by an inbound
// Slack message. Ignored for human-initiated sessions (which carry
// channel + thread_ts on the inbound).
type SlackOutputDefaults struct {
	// ChannelID is the Slack channel to post into (e.g. C0123ABC).
	ChannelID string `json:"channelId"`
	// ThreadStrategy controls whether each session gets its own
	// thread, appends to a fixed digest thread, or posts directly
	// without a thread anchor.
	// +kubebuilder:validation:Enum=new-thread-per-session;direct;static-thread
	// +kubebuilder:default=new-thread-per-session
	// +optional
	ThreadStrategy string `json:"threadStrategy,omitempty"`
	// StaticThreadTS is required when ThreadStrategy=static-thread.
	// +optional
	StaticThreadTS string `json:"staticThreadTs,omitempty"`
}

// ChannelMetaagentSpec lets an operator override the AgentSession
// controller's default behavior of auto-inviting the metaagent bot to
// this channel when the AgentClass has scope-management enabled.
type ChannelMetaagentSpec struct {
	// Enabled forces metaagent membership on (true) or off (false).
	// When nil, the AgentSession controller follows the AgentClass's
	// scope.enabled flag.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

// ChannelOwnerPolicy is the per-channel owner-resolution policy.
type ChannelOwnerPolicy struct {
	// Explicit overrides the starting user for every session of this channel
	// (agent identityMode only; forbidden under userPassthrough). A SpiceDB
	// subject or subject-set ref, e.g. "user:abc" or "group:sec#member".
	// +optional
	Explicit string `json:"explicit,omitempty"`

	// Ownerless is consulted only when the channel kind provides no starting
	// user (e.g. bento).
	// +optional
	Ownerless *ChannelOwnerlessSource `json:"ownerless,omitempty"`
}

// ChannelOwnerlessSource names the owner source for ownerless input kinds.
// Exactly one of the fields should be set.
type ChannelOwnerlessSource struct {
	// Permission is a SpiceDB subject-set ref whose members become the
	// owners, e.g. "resourcetype:id#relation".
	// +optional
	Permission string `json:"permission,omitempty"`

	// FromOutputChannel links the owner to the output channel's membership
	// group (e.g. slack_channel:<id>#member), resolved via the output
	// channel kind's OwnerGroupRef capability.
	// +optional
	FromOutputChannel bool `json:"fromOutputChannel,omitempty"`
}

// ChannelHistorySpec configures whole-channel history reads for this Channel's
// agent. The kind clamps MaxLookback/MaxMessages to its own hard maximum
// (Slack ~30d); a value exceeding the kind max is clamped, not rejected.
type ChannelHistorySpec struct {
	// Enabled turns the capability on. Must be true to inject the tool.
	Enabled bool `json:"enabled"`

	// MaxLookback optionally caps how far back reads may reach. Zero/nil ⇒ the
	// kind's default window.
	// +optional
	MaxLookback *metav1.Duration `json:"maxLookback,omitempty"`

	// MaxMessages optionally caps messages returned per read. Zero/nil ⇒ the
	// kind's default.
	// +optional
	MaxMessages *int32 `json:"maxMessages,omitempty"`
}

// ChannelAttachmentsSpec bounds attachment ingestion for this Channel. The kind
// clamps MaxSizeBytes to its own hard maximum; a larger value is clamped, not
// rejected.
type ChannelAttachmentsSpec struct {
	// Enabled turns ingestion on. Must be true for bytes to move.
	Enabled bool `json:"enabled"`

	// MaxSizeBytes caps a single attachment. Zero means the kind's default.
	// +optional
	MaxSizeBytes *int64 `json:"maxSizeBytes,omitempty"`

	// MaxPerMessage caps how many attachments are ingested from one message.
	// Zero means the kind's default. Files past the cap get the oversize-style
	// notice rather than silent omission.
	// +optional
	MaxPerMessage *int32 `json:"maxPerMessage,omitempty"`
}

type ChannelStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ResolvedAgentClassUID is frozen at first successful Valid=True
	// reconcile. Protects against same-name AgentClass re-create silently
	// re-routing in-flight sessions.
	// +optional
	ResolvedAgentClassUID types.UID `json:"resolvedAgentClassUID,omitempty"`

	// Conditions carries Valid (operator-owned) and Connected/ScopesValid
	// (channelsd-owned).
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// WebhookURLDriftCheckedAt is when the channel controller last actually
	// called out to a WebhookURLDriftChecker kind's provider (github today)
	// to compare its registered webhook URL against this cluster's — set
	// regardless of whether that call found drift, matched, or errored.
	// A controller-owned observation, not a per-check trigger: the
	// controller throttles the outbound call to at most once per interval
	// per Channel using this timestamp, so an unbounded per-reconcile call
	// against a rate limit this cluster does not control (and whose
	// exhaustion would also break the token-minting path the drift check is
	// meant to be monitoring) cannot happen. Absent means "never checked".
	// +optional
	WebhookURLDriftCheckedAt *metav1.Time `json:"webhookURLDriftCheckedAt,omitempty"`

	// RepointedWebhookURL is the webhook URL the channel controller last
	// SUCCESSFULLY wrote to this Channel's third-party provider (a GitHub
	// App's hook_attributes.url), for a Channel carrying the provenance
	// marker that says this tool registered that application. Absent means
	// no such write has ever landed — including for every Channel whose
	// application a human registered by hand, which this controller never
	// writes to at all.
	//
	// It is a controller-owned OBSERVATION, and it is what makes the repoint
	// event-driven instead of a poll: the controller compares this against
	// the URL it derives LOCALLY (the PublicEndpoint's status.url plus
	// channelevents.WebhookPathFor) and calls the provider only when the two
	// disagree. Without it, "is the registration already correct?" could be
	// answered only by reading the provider on every reconcile — against a
	// rate limit this cluster does not control, and whose exhaustion also
	// breaks the token-minting path.
	//
	// A write that FAILS is deliberately not recorded, so the next reconcile
	// retries rather than believing a URL landed that never did. Losing this
	// value (a status wipe, a restore from an older object) costs one
	// redundant write of the value the provider most likely already has, not
	// correctness.
	// +optional
	RepointedWebhookURL string `json:"repointedWebhookURL,omitempty"`

	// DerivedSessionOwner is the subject-set that will own sessions inbound on
	// this Channel because nothing else supplies an owner: the kind provides no
	// starting user, spec.owner declares no source, and the Channel this
	// agent's output lands in yields a membership subject-set
	// (slack_channel:<id>#member for a Slack destination).
	//
	// Present only when the derivation actually fired. A Channel that declared
	// spec.owner.explicit or spec.owner.ownerless keeps what it declared and
	// leaves this empty, as does one whose kind names a starting user — so a
	// non-empty value always reads as "nobody wrote this owner; it was derived,
	// and here is from what".
	//
	// It is published because of what an owner CARRIES. `owner` is not merely
	// interact: manage_scope, fork, approve, manage_budget and manage_model all
	// resolve through it, so a derived channel-membership owner hands every
	// member of that Slack channel the approve button on this agent's tool
	// calls. That is the same population the agent posts every one of its
	// messages to, and it is what an operator configuring this by hand chose —
	// but it must be visible on the object rather than inferable only from the
	// absence of a field.
	// +optional
	DerivedSessionOwner string `json:"derivedSessionOwner,omitempty"`
}

// ChannelRole values for ChannelSpec.Role. These match the
// kubebuilder enum.
const (
	ChannelRoleInput      = "input"
	ChannelRoleOutput     = "output"
	ChannelRoleBoth       = "both"
	ChannelRoleMonitoring = "monitoring"
)

// AllChannelRoles returns every legal ChannelSpec.Role value, in the order the
// field's own +kubebuilder:validation:Enum lists them. A new role is added
// HERE and to that marker, together — a caller enumerating roles (a channel
// kind declaring it serves all of them, a lint checking a declared role is a
// real one) must never carry its own copy of the list.
//
// A fresh slice per call, so a caller that sorts or truncates the answer
// cannot corrupt the next caller's.
func AllChannelRoles() []string {
	return []string{
		ChannelRoleInput,
		ChannelRoleOutput,
		ChannelRoleBoth,
		ChannelRoleMonitoring,
	}
}

// AgentChannelRoles is every role a Channel BOUND TO AN AGENT may have: the
// legal set minus monitoring.
//
// The distinction is the CRD's own, stated on AgentClass above and enforced by
// the Channel controller: a monitoring Channel is a framework-event sink that
// binds to no AgentClass, so it is never a role something choosing a channel
// FOR an agent may pick. Every caller that offers a role — a bundle's
// requires.channels declaration, `oap channel create --role` — offers exactly
// this set, and reaches monitoring through its own separate route
// (`--monitoring`, channelkinds.WizardInput.Monitoring) rather than by naming
// the role.
//
// Derived by subtraction from AllChannelRoles rather than written out, so a
// role added to the CRD becomes offerable without an edit here — the same
// reason AllChannelRoles exists at all.
func AgentChannelRoles() []string {
	all := AllChannelRoles()
	out := make([]string, 0, len(all))
	for _, r := range all {
		if r != ChannelRoleMonitoring {
			out = append(out, r)
		}
	}
	return out
}

func init() {
	SchemeBuilder.Register(&Channel{}, &ChannelList{})
}
