package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories={authzed,spicebox},shortName=uid
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Subject",type="string",JSONPath=".spec.subject"
// +kubebuilder:printcolumn:name="Valid",type="string",JSONPath=".status.conditions[?(@.type=='Valid')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
// +genclient:nonNamespaced
//
// UserIdentity is a human's credential catalog: the credentials one person has
// linked, keyed by their canonical SpiceDB subject, used by
// identityMode=userPassthrough agents through a per-session
// SessionUserIdentity projection.
//
// Cluster-scoped. Reconciled by pkg/controllers/useridentity -- one reconciler
// for validity, one for proactive OAuth refresh. metadata.name is a
// deterministic hash of spec.subject rather than the subject itself, and the
// referenced Secrets live in the agentprimitives-identities namespace.
type UserIdentity struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UserIdentitySpec   `json:"spec,omitempty"`
	Status UserIdentityStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type UserIdentityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UserIdentity `json:"items"`
}

type UserIdentitySpec struct {
	// Subject is the canonical SpiceDB subject of the user this catalog
	// belongs to, e.g. "user:YWxpY2VAZXhhbXBsZS5jb20=". metadata.name is a
	// deterministic hash of this value (see pkg/platform/identity/useridentity).
	// +kubebuilder:validation:MinLength=1
	Subject string `json:"subject"`

	// DisplayName is a human-friendly label (e.g. an email address) surfaced in
	// CLI output and the web UI.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Credentials are the user's reusable named credentials. Same shape as
	// AgentIdentity.Spec.Credentials. The secretRefs name Secrets in the
	// agentprimitives-identities namespace (IdentitiesNamespace).
	// Credential names must match what tools declare (or the runtime
	// default — see MCPServerAuth.Credential / ToolkitEnvVar.Credential).
	// +optional
	Credentials []AgentCredential `json:"credentials,omitempty"`

	// RefreshThreshold overrides the operator-wide default OAuth refresh
	// threshold for this identity. No effect on type=static credentials.
	// +optional
	RefreshThreshold *metav1.Duration `json:"refreshThreshold,omitempty"`
}

type UserIdentityStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ResolvedCredentials lists credential names whose Secret + key resolved.
	// +optional
	ResolvedCredentials []string `json:"resolvedCredentials,omitempty"`

	// AvailableCredentials is the set of credential names this identity
	// successfully provides — a convenience for the operator's passthrough
	// gate and CLI output.
	// +optional
	AvailableCredentials []string `json:"availableCredentials,omitempty"`

	// LastRefreshAt is the most recent successful OAuth refresh.
	// +optional
	LastRefreshAt *metav1.Time `json:"lastRefreshAt,omitempty"`

	// ObservedCredentials is the durable trigger state for in-flight
	// credential revocation — see AgentIdentityStatus.ObservedCredentials for
	// the full contract. SecretNames is unused here: a UserIdentity
	// credential's master Secret name is derived from (identity, credential)
	// rather than referenced by the spec.
	// +optional
	ObservedCredentials []ObservedCredential `json:"observedCredentials,omitempty"`

	// Conditions carries Valid and Refresh; the reason constants are shared
	// with AgentIdentity.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ChannelIdentities is the durable directory of the channel-native
	// identities (Slack user in a workspace, …) that map to this canonical
	// user. One canonical user fans out to many entries — the same human has
	// distinct external IDs across unrelated Slack workspaces/orgs. Written by
	// channelsd (best-effort) via a status merge-patch disjoint from the
	// credential fields the useridentity controller owns.
	// +optional
	// +listType=map
	// +listMapKey=kind
	// +listMapKey=domain
	// +listMapKey=externalID
	ChannelIdentities []ChannelIdentity `json:"channelIdentities,omitempty"`
}

// ChannelIdentity is one channel-native identity linked to a canonical user.
// (Kind, Domain, ExternalID) is the unique key: Domain is the workspace/org
// scope (Slack team_id) that distinguishes the same person across workspaces.
type ChannelIdentity struct {
	// Kind is the channel kind, e.g. "slack".
	Kind string `json:"kind"`
	// Domain is the workspace/org scope (Slack team_id); "" for kinds without one.
	Domain string `json:"domain"`
	// ExternalID is the channel-native user id, e.g. a Slack user_id.
	ExternalID string `json:"externalID"`
	// DisplayName is the channel-native display/real name, when known.
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// Email is the trusted email the channel vouched for, when known.
	// +optional
	Email string `json:"email,omitempty"`
}

func init() {
	SchemeBuilder.Register(&UserIdentity{}, &UserIdentityList{})
}
