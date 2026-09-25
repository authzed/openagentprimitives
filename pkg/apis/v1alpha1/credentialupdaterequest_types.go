package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,agentprimitives},shortName=credupdate
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Determination",type="string",JSONPath=".status.determination"
// +kubebuilder:printcolumn:name="Origin",type="string",JSONPath=".spec.origin"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
//
// CredentialUpdateRequest is an agent's REQUEST that a human replace a
// credential it believes has stopped authenticating. The spec names only the
// failing TOOL -- the agent has no vocabulary for naming an identity, which is
// what stops it pointing at a credential it was not already using.
//
// Namespaced. Reconciled by pkg/controllers/credentialupdaterequest, which
// independently re-verifies before any card is shown and refuses outright when
// the credential still authenticates. It is a request, never a decision: the
// verdict table lives in pkg/platform/identity/credupdate, and this CR only
// records what was asked and what was determined.
type CredentialUpdateRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CredentialUpdateRequestSpec   `json:"spec,omitempty"`
	Status CredentialUpdateRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type CredentialUpdateRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CredentialUpdateRequest `json:"items"`
}

type CredentialUpdateRequestSpec struct {
	// SessionRef is the AgentSession that parked on this request. The CR also
	// carries an owner-ref to it, so the request lives exactly as long as the
	// session does — which is what makes the ask budget derivable by listing
	// rather than storing a counter.
	SessionRef NamespacedRef `json:"sessionRef"`

	// Origin is the failing tool's tool.OriginTool.Origin() value, e.g.
	// "mcpserver/github". The reconciler resolves this to a credential.
	Origin string `json:"origin"`

	// ToolName is the failing tool's LLM-facing name, recorded for the card
	// and for audit. Not used for resolution.
	ToolName string `json:"toolName"`

	// Why is the agent's own explanation. UNTRUSTED and agent-authored: it is
	// the ONLY agent-written string that ever reaches the card, where it is
	// rendered in a visually distinct attributed block. Capped by the meta
	// tool before it is written here.
	// +optional
	// +kubebuilder:validation:MaxLength=280
	Why string `json:"why,omitempty"`

	// RequestedBy is the canonical subject of the turn's author.
	RequestedBy identity.Subject `json:"requestedBy"`
}

type CredentialUpdateRequestStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is the request's lifecycle position. Fulfilled, Refused and
	// Expired are terminal — see IsCredentialUpdateRequestTerminal.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Determination records WHY the reconciler opened or refused. Surfaced to
	// the agent verbatim, so a refusal is never silent.
	// +optional
	Determination string `json:"determination,omitempty"`

	// Reason is the human-readable determination text. It doubles as the
	// card's verdict line, and is platform-authored in every case.
	// +optional
	Reason string `json:"reason,omitempty"`

	// ResolvedCredential is what Origin resolved to. Empty when resolution
	// itself failed (NoCredential / AmbiguousCredential).
	// +optional
	ResolvedCredential *ResolvedCredentialRef `json:"resolvedCredential,omitempty"`

	// InteractionRef is the requestRef of the published interaction, set only
	// when the request opened a card.
	// +optional
	InteractionRef string `json:"interactionRef,omitempty"`

	// OpenedAt is when the reconciler transitioned this request to Open — the
	// instant a human's wait window actually starts. A controller-owned
	// observation, written ONCE on that transition and never rewritten.
	//
	// Deliberately not CreationTimestamp: creation and determination are
	// separated by however long the pipeline took (an operator restart, minutes
	// of probe backoff against an unreachable provider), and measuring from
	// creation let a deadline elapse before the card existed — Open written,
	// channelsd publishing, and the next reconcile expiring it, blaming a human
	// who had no window at all. Empty on requests opened before this field
	// existed, where CreationTimestamp remains the fallback.
	// +optional
	OpenedAt *metav1.Time `json:"openedAt,omitempty"`

	// CredentialSecretRef names the backing Secret the resolved credential's
	// value lives in, recorded once the request opens a card. It is what the
	// reconciler's Secret watch uses to find the CredentialUpdateRequest(s) a
	// changed Secret should unpark -- an OBSERVATION (set once, never
	// user-supplied), not part of the ask itself.
	// +optional
	CredentialSecretRef *NamespacedRef `json:"credentialSecretRef,omitempty"`

	// CredentialSecretKey is the Secret data key the resolved credential
	// occupies, written alongside CredentialSecretRef. Together they are the
	// credential's full backing coordinates — namespace, name, key — the only
	// handle that identifies a credential rather than the identity CR it was
	// reached through.
	//
	// Empty for a type=oauth credential, whose value is the WHOLE Secret rather
	// than one key of it; that emptiness is the discriminator, not a missing
	// value. A non-empty key scopes change-detection to that key alone, because
	// the operator projects EVERY static credential of a session into one
	// shared Secret and a whole-Secret digest cannot tell "this credential was
	// replaced" from "some other credential was linked".
	// +optional
	CredentialSecretKey string `json:"credentialSecretKey,omitempty"`

	// CollapsedInto names the CredentialUpdateRequest whose card this request is
	// waiting on, set when the reconciler resolved to a credential another
	// request ALREADY holds a live card for: five sessions sharing one dead bot
	// token must raise one card at one admin, not five. Empty on every request
	// that owns its own card, so "is this a follower?" is one nil check.
	//
	// Stored on the FOLLOWER, never mirrored as a list on the canonical: one
	// writer per field (a follower's own reconcile sets its own pointer, so no
	// two contend), and the canonical's follower set stays DERIVED by listing
	// rather than stored and kept in sync, because a mirrored list desyncs.
	// +optional
	CollapsedInto *NamespacedRef `json:"collapsedInto,omitempty"`

	// CredentialSecretObservedHash is a SHA-256 digest of the backing Secret's
	// Data as observed when the request opened. The Secret watch compares the
	// CURRENT hash against this recorded baseline -- a real content change
	// (not an unrelated metadata/label edit that only bumps resourceVersion)
	// is what marks the request Fulfilled.
	// +optional
	CredentialSecretObservedHash string `json:"credentialSecretObservedHash,omitempty"`

	// Conditions carries CardDelivered; see its type constant.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// ResolvedCredentialRef identifies the credential an Origin resolved to. It is
// the budget key: the ask budget counts SPENT asks per
// (session, resolved credential), NOT per origin, because two origins can
// share one token and asking twice for it is the same imposition on the human.
// "Spent" is credentialupdaterequest.askIsSpent -- every ask this reconciler has
// DETERMINED, which is broader than terminal: a Collapsed ask riding somebody
// else's live card has already cost a human attention and is charged then, not
// when it eventually settles.
type ResolvedCredentialRef struct {
	// IdentityKind is the identity CR's Kind spelling: "AgentIdentity",
	// "UserIdentity", or "SessionUserIdentity". SessionUserIdentity is the
	// per-session projection of a UserIdentity — both are user-owned
	// (passthrough) credentials, as distinct from an AgentIdentity's own.
	IdentityKind string `json:"identityKind"`
	// Namespace is the identity CR's namespace; empty for cluster-scoped kinds.
	Namespace string `json:"namespace"`
	// Name is the identity CR name.
	Name string `json:"name"`
	// Credential is the credential name within that identity.
	Credential string `json:"credential"`
	// ProviderID is the provider catalog id, recorded so the card can render a
	// title and icon without re-resolving.
	// +optional
	ProviderID string `json:"providerID,omitempty"`
}

// The three spellings ResolvedCredentialRef.IdentityKind may carry. They are
// constants because WHOSE credential a request names decides which humans see
// the card (pkg/channels/channelsd/pipeline), which gate authorizes the click, and which
// object the replacement is written to (pkg/platform/identityd) — three packages that
// must agree on the spelling exactly. A typo in any one of them fails OPEN in
// the worst direction: an agent's shared credential silently treated as a
// person's own.
const (
	// IdentityKindAgentIdentity is the AGENT's own shared credential.
	IdentityKindAgentIdentity = "AgentIdentity"
	// IdentityKindUserIdentity is a person's standing credential.
	IdentityKindUserIdentity = "UserIdentity"
	// IdentityKindSessionUserIdentity is the per-session projection of a
	// UserIdentity — same ownership (a person's), different CR.
	IdentityKindSessionUserIdentity = "SessionUserIdentity"
)

// AgentOwned reports whether rc names the AGENT's own shared credential rather
// than one belonging to a person. Nil-safe: unresolved is not agent-owned.
//
// The distinction drives the whole credential-update flow. A user-owned
// credential has one obvious human behind it, so the card DMs them and the
// click writes their own master Secret. An AgentIdentity's credential is a
// shared secret held on the agent's behalf: replacing it changes what every
// session of that agent authenticates as, the gate is
// `agentidentity#update_credential`, and the value belongs in the
// AgentIdentity's own Secret. Routing one as the other overwrites a bystander's
// personal credential while the shared one stays dead.
func (rc *ResolvedCredentialRef) AgentOwned() bool {
	return rc != nil && rc.IdentityKind == IdentityKindAgentIdentity
}

// NamespacedRef names a namespaced object.
type NamespacedRef struct {
	// Namespace is the object's namespace.
	Namespace string `json:"namespace"`
	// Name is the object's name.
	Name string `json:"name"`
}

// CredentialUpdateRequest phases.
const (
	CredentialUpdateRequestPhasePending   = "Pending"
	CredentialUpdateRequestPhaseOpen      = "Open"
	CredentialUpdateRequestPhaseFulfilled = "Fulfilled"
	CredentialUpdateRequestPhaseRefused   = "Refused"
	CredentialUpdateRequestPhaseExpired   = "Expired"

	// CredentialUpdateRequestPhaseCollapsed is a request that resolved to a
	// credential another request already holds a live card for. It is
	// deliberately neither of the two phases anything downstream acts on:
	//
	//   - NOT Open, and Open is the ONLY phase channelsd's
	//     CredentialUpdateWatcher publishes on, so "no second card" is
	//     structural here rather than a second check bolted onto the publisher.
	//   - NOT terminal (IsCredentialUpdateRequestTerminal), so the blocked
	//     agent's request_credential_update call keeps waiting instead of
	//     being told its ask was settled when nothing has been decided yet.
	//
	// It is a named phase rather than leaving the request in Pending so that
	// `kubectl get credupdate` answers "why has nothing happened for my
	// session" directly, next to status.collapsedInto naming what it waits on.
	CredentialUpdateRequestPhaseCollapsed = "Collapsed"
)

// CredentialUpdateRequest determinations. Every one of these is surfaced to
// the agent verbatim; none of them is a silent outcome.
const (
	// Opened a card.
	CredentialUpdateDeterminationRejectedVerified    = "RejectedVerified"
	CredentialUpdateDeterminationRejectedRefreshDead = "RejectedRefreshDead"
	// RejectedUnverified: opened on the platform's own observed auth failures
	// when the provider could not be reached to confirm.
	CredentialUpdateDeterminationRejectedUnverified = "RejectedUnverified"
	// Refused.
	CredentialUpdateDeterminationCredentialLive  = "CredentialLive"
	CredentialUpdateDeterminationUnverified      = "Unverified"
	CredentialUpdateDeterminationNotUpdatable    = "NotUpdatable"
	CredentialUpdateDeterminationSelfHealed      = "SelfHealed"
	CredentialUpdateDeterminationNoCredential    = "NoCredential"
	CredentialUpdateDeterminationAmbiguousCred   = "AmbiguousCredential"
	CredentialUpdateDeterminationBudgetExhausted = "BudgetExhausted"
	// CredentialUpdateDeterminationCollapsed is neither an open nor a refusal,
	// which is why it sits in its own group: nothing was asked of a human for
	// THIS request, and nothing was denied to the agent either. The identical
	// credential already has a card in front of a human, and this request is
	// waiting on that one. See CredentialUpdateRequestPhaseCollapsed.
	CredentialUpdateDeterminationCollapsed = "Collapsed"
)

// IsCredentialUpdateRequestTerminal reports whether phase is one the meta
// tool's poll loop may stop on. Unknown and empty phases are NOT terminal, so
// a typo can never strand a blocked agent on a phase that will never resolve —
// the idle TTL remains the backstop.
func IsCredentialUpdateRequestTerminal(phase string) bool {
	switch phase {
	case CredentialUpdateRequestPhaseFulfilled,
		CredentialUpdateRequestPhaseRefused,
		CredentialUpdateRequestPhaseExpired:
		return true
	default:
		return false
	}
}

// IsCredentialUpdateRequestAwaitingHuman reports whether a request in this
// phase is still waiting on a person: a card is live for it (Open), or one is
// live for the identical credential and this request is riding on that answer
// (Collapsed).
//
// It is the predicate the AgentSession controller parks on. Both phases park
// because the agent behind each is blocked identically — the meta tool waits on
// non-terminal, not on Open — so a collapsed request's session must read
// AwaitingCredentials too, not silently read Running while its runner is stuck.
//
// Pending is deliberately excluded: nothing has been determined yet, and a
// request about to be refused outright must not flip its session through a park
// it leaves microseconds later.
//
// It is deliberately NOT the negation of IsCredentialUpdateRequestTerminal:
// written that way, an unknown or typo'd phase would park a session forever
// with no card anywhere. As an allowlist it fails closed to "not waiting".
func IsCredentialUpdateRequestAwaitingHuman(phase string) bool {
	switch phase {
	case CredentialUpdateRequestPhaseOpen,
		CredentialUpdateRequestPhaseCollapsed:
		return true
	default:
		return false
	}
}

func init() {
	SchemeBuilder.Register(&CredentialUpdateRequest{}, &CredentialUpdateRequestList{})
}
