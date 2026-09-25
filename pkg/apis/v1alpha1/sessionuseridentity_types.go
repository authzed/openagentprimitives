package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,spicebox},shortName=suid
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Session",type="string",JSONPath=".spec.agentSession"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
//
// SessionUserIdentity is one session's narrowing of a user's UserIdentity
// catalog down to just the credentials that session's agent asked for, for
// identityMode=userPassthrough. It is 1:1 with its AgentSession: metadata.name
// equals the session name, and an owner-ref ties their lifetimes together.
//
// Namespaced. It has no controller of its own -- pkg/controllers/agentsession
// builds it and server-side-applies it. status.missingCredentials is what
// parks the session in AwaitingCredentials, so an empty spec.credentials here
// means "still waiting for the user to link", not "no credentials needed".
type SessionUserIdentity struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SessionUserIdentitySpec   `json:"spec,omitempty"`
	Status SessionUserIdentityStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SessionUserIdentityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SessionUserIdentity `json:"items"`
}

type SessionUserIdentitySpec struct {
	// AgentSession is the name of the owning AgentSession (same namespace).
	// metadata.name equals this value (1:1).
	AgentSession string `json:"agentSession"`

	// UserIdentity is the name of the cluster-scoped UserIdentity this
	// projection was narrowed from.
	UserIdentity string `json:"userIdentity"`

	// Subject is the starter's canonical SpiceDB subject.
	Subject string `json:"subject"`

	// Credentials is the subset of the UserIdentity's catalog the agent
	// needs. Same shape as AgentIdentity.Spec.Credentials. SecretRefs name
	// Secrets in the agentprimitives-identities namespace. Empty while
	// the session is parked with missing credentials.
	// +optional
	Credentials []AgentCredential `json:"credentials,omitempty"`
}

type SessionUserIdentityStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// MissingCredentials lists credential names the agent needs but the
	// user has not linked. Empty → ready to proceed.
	// +optional
	MissingCredentials []string `json:"missingCredentials,omitempty"`

	// ParkedAt is when the owning session first entered AwaitingCredentials.
	// The credential-link timeout is measured from
	// max(ParkedAt, LastInteractionAt).
	// +optional
	ParkedAt *metav1.Time `json:"parkedAt,omitempty"`

	// LastInteractionAt is bumped by identityd's heartbeat while the user is
	// actively linking, so an in-progress link does not time out.
	// +optional
	LastInteractionAt *metav1.Time `json:"lastInteractionAt,omitempty"`

	// Explanation is the structured per-credential description rendered to
	// the user (one Item per missing credential). Populated by the
	// operator's BuildExplanation; rendered by channelsd (Slack DM) and
	// identityd (portal menu).
	// +optional
	Explanation *CredentialExplanation `json:"explanation,omitempty"`

	// Conditions carries Ready; see SessionUserIdentityConditionReady.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// CredentialExplanation is the user-facing description of a credential
// request, one Item per missing credential. Populated by the operator's
// BuildExplanation; rendered by channelsd (Slack DM) and identityd
// (portal menu).
type CredentialExplanation struct {
	// Items is one entry per missing credential, in stable order.
	// +optional
	Items []CredentialExplanationItem `json:"items,omitempty"`
}

// CredentialExplanationItem is one credential row: a service Title, a
// Description of what the token is, and an agent-specific Why.
type CredentialExplanationItem struct {
	// Credential is the final (post-remap) credential name this row is for.
	Credential string `json:"credential"`
	// Title is the user-facing service/token display name ("GitHub").
	// +optional
	Title string `json:"title,omitempty"`
	// Description is the "what is this token" sentence.
	// +optional
	Description string `json:"description,omitempty"`
	// Why is the agent-specific reason. EMPTY is meaningful: it signals
	// "no explicit reason declared" so the render surface applies its own
	// fallback (channelsd: LLM then static; identityd: static). A non-empty
	// Why is rendered verbatim and never sent to the LLM.
	// +optional
	Why string `json:"why,omitempty"`
}

// PassthroughCredentialSecretName is the deterministic name of the per-session
// Secret into which the operator projects a userPassthrough session's
// type=static credential VALUES, keyed by credential name. It lives in the
// session's own namespace (owned by the AgentSession), so a sandbox ToolCall's
// credential source resolves WITHIN the ToolCall's namespace — satisfying
// ToolCall.ValidateCredentialSourceNamespaces, which refuses a cross-namespace
// (confused-deputy) Secret reference. The session namespace holds only this
// session's projected credentials, so that boundary stays closed.
//
// type=oauth / type=federated credentials are NOT projected here: their JIT
// refresh / ID-JAG mint must stay anchored on the master / IdP-identity Secret
// in the identities namespace (projecting a refresh-token copy would risk a
// double-spend against the useridentity refresh controller).
func PassthroughCredentialSecretName(sessionName string) string {
	return sessionName + PassthroughCredentialSecretSuffix
}

// PassthroughCredentialSecretSuffix is the suffix of every per-session
// passthrough-credential Secret name. Used to recognize such a source so it can
// be bound to its owning session (see ToolCall.ValidateCredentialSourceOwnership).
const PassthroughCredentialSecretSuffix = "-passthrough-creds"

func init() {
	SchemeBuilder.Register(&SessionUserIdentity{}, &SessionUserIdentityList{})
}
