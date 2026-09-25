package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,spicebox},shortName=agid
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Valid",type="string",JSONPath=".status.conditions[?(@.type=='Valid')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
//
// AgentIdentity is the named credential set an agent acts as: reusable static
// and OAuth credential descriptors that toolkits and MCP servers request by
// name. It carries references to Secrets, never token bytes.
//
// Namespaced. Reconciled by pkg/controllers/agentidentity -- one reconciler
// for validity (every referenced Secret + key must exist), one for proactive
// OAuth refresh. type=federated is rejected here: those credentials are minted
// on demand from a human enterprise identity, so they only ever live on a
// UserIdentity or SessionUserIdentity.
type AgentIdentity struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentIdentitySpec   `json:"spec,omitempty"`
	Status AgentIdentityStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentIdentityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentIdentity `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.credentials) || !self.credentials.exists(c, c.type == 'federated')",message="spec.credentials: type=federated is passthrough-only and not permitted on an AgentIdentity"
type AgentIdentitySpec struct {
	// Description is a human-readable summary surfaced in events / status.
	// +optional
	Description string `json:"description,omitempty"`

	// Credentials are reusable named credential descriptors. Each has a `type`
	// discriminator and exactly one type-specific block populated.
	// +optional
	Credentials []AgentCredential `json:"credentials,omitempty"`

	// RefreshThreshold overrides the operator-wide default refresh
	// threshold for this AgentIdentity. type=oauth credentials whose
	// expires_at is within this duration are refreshed proactively.
	// No effect on type=static credentials.
	// +optional
	RefreshThreshold *metav1.Duration `json:"refreshThreshold,omitempty"`
}

type AgentCredential struct {
	// Name is the credential's unique identifier. It is matched against the
	// credential names a toolkit/MCPServer declares to inject the resolved
	// value at runtime.
	Name string `json:"name"`

	// Type discriminates which of the blocks below is populated.
	// +kubebuilder:validation:Enum=static;oauth;federated;githubApp
	Type string `json:"type"`

	// Static is populated when type=static.
	// +optional
	Static *StaticCredentialSource `json:"static,omitempty"`

	// OAuth is populated when type=oauth. The referenced Secret holds
	// access_token, refresh_token, expires_at, token_endpoint, client_id,
	// scope as separate keys. Key names are fixed; not configurable.
	// +optional
	OAuth *OAuthCredentialSource `json:"oauth,omitempty"`

	// Federated is populated when type=federated. The credential is MINTED
	// on demand via ID-JAG from the user's enterprise identity — there is
	// no stored access token. Valid ONLY on UserIdentity /
	// SessionUserIdentity; rejected on AgentIdentity (bot identities have no
	// human subject to assert).
	// +optional
	Federated *FederatedCredentialSource `json:"federated,omitempty"`

	// GitHubApp is populated when type=githubApp.
	// +optional
	GitHubApp *GitHubAppCredentialSource `json:"githubApp,omitempty"`

	// AllowedHosts scopes where this credential may be sent, as host patterns
	// ("github.com", "*.internal.example"). Matching is on the URL authority
	// (host and port), case-insensitively; a leading "*." matches one or more
	// leading labels but never the apex, so an author who wants both lists
	// both.
	//
	// It exists because destination and credential were independent fields on
	// the same tenant-writable object with nothing tying them together: a
	// SkillSource naming spec.repoURL: "https://attacker.example/x.git" and
	// spec.auth pointing at a production PAT made the operator send that PAT to
	// that host as an HTTP Basic password, reading the Secret with its own
	// cluster-wide credentials so the actor never needed `get secrets`.
	//
	// EMPTY MEANS UNSCOPED, which is the only reading that does not break every
	// existing credential on upgrade. That makes the field opt-in, so a caller
	// with a stricter default enforces it itself rather than relying on this:
	// the SkillSource controller refuses an unscoped credential for a clone
	// outright, because there is no safe default destination for one.
	//
	// +optional
	AllowedHosts []string `json:"allowedHosts,omitempty"`
}

type StaticCredentialSource struct {
	// SecretRef points at a Secret in the same namespace as the AgentIdentity.
	SecretRef SecretKeyRef `json:"secretRef"`
}

type SecretKeyRef struct {
	// Name is the Secret's name, in the referring object's own namespace.
	Name string `json:"name"`
	// Key is the data key within the Secret holding the value.
	Key string `json:"key"`
}

type OAuthCredentialSource struct {
	// SecretRef points at a Secret whose keys hold the OAuth state.
	// Required keys (read at runtime / refresh time):
	//   access_token   — current access token
	//   refresh_token  — long-lived refresh token (optional)
	//   expires_at     — RFC 3339 timestamp; absent → never expires
	//   token_endpoint — auth-server URL for refresh
	//   client_id      — OAuth client ID used at registration
	//   scope          — granted scopes (informational)
	SecretRef SecretRef `json:"secretRef"`
}

// SecretRef references a Secret by name (no key — see OAuthCredentialSource
// for the fixed multi-key shape it implies).
type SecretRef struct {
	// Name is the Secret's name, in the referring object's own namespace.
	Name string `json:"name"`
}

// FederatedCredentialSource declares an ID-JAG-minted credential. No Secret
// holds the upstream token; it is derived per resolve from the user's
// IdP-identity Secret (the refresh token captured at login).
type FederatedCredentialSource struct {
	// Resource is the identifier the IdP knows the MCP server by — the
	// ID-JAG audience.
	Resource string `json:"resource"`
	// ResourceServerURL is the MCP server URL whose authorization server
	// accepts the ID-JAG (leg 2 discovery target).
	ResourceServerURL string `json:"resourceServerURL"`
	// IdPSecretRef points at the subject-keyed IdP-identity Secret in
	// IdentitiesNamespace (the user's refresh token).
	IdPSecretRef SecretRef `json:"idpSecretRef"`
	// Scopes are optional scopes requested at the resource AS.
	// +optional
	Scopes []string `json:"scopes,omitempty"`
}

// GitHubAppCredentialSource declares a credential MINTED on demand from a
// stored GitHub App private key. The referenced Secret holds app-id,
// private-key (PEM), installation-id and webhook-secret; the resolved value is
// a ~1h installation access token, never the private key itself.
//
// Valid ONLY on AgentIdentity. This is the exact mirror of type=federated,
// which is rejected on AgentIdentity because a bot has no human subject to
// assert: a GitHub App has no human subject AT ALL, so an AgentIdentity is the
// only place it belongs.
type GitHubAppCredentialSource struct {
	// SecretRef points at the Secret the channel wizard wrote.
	SecretRef SecretRef `json:"secretRef"`
}

type AgentIdentityStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ResolvedCredentials lists credential names whose Secret + key resolved.
	// +optional
	ResolvedCredentials []string `json:"resolvedCredentials,omitempty"`

	// LastSetupAt is the most recent timestamp at which any credential
	// on this identity was created or updated by the setup CLI.
	// +optional
	LastSetupAt *metav1.Time `json:"lastSetupAt,omitempty"`

	// LastRefreshAt is the most recent successful OAuth refresh
	// performed by the refresh controller.
	// +optional
	LastRefreshAt *metav1.Time `json:"lastRefreshAt,omitempty"`

	// ObservedCredentials is the durable trigger state for in-flight credential
	// revocation: the credential set the operator has already published revokes
	// for. The reconciler diffs spec.credentials against it, so a removal or
	// replacement committed while the operator was down — or one whose publish
	// failed — is still emitted on the next reconcile, rather than living only
	// in a process-scoped map that dies with the pod.
	//
	// Operator-owned and derived: a pure function of spec.credentials, sorted
	// by name, so an unchanged spec re-writes nothing. An entry whose revoke
	// failed to publish deliberately keeps its PREVIOUS value, which is what
	// makes the next reconcile re-derive and re-emit it.
	// +optional
	ObservedCredentials []ObservedCredential `json:"observedCredentials,omitempty"`

	// Conditions carries Valid, Refresh and PlatformLinked.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// ObservedCredential is one entry of an identity's status.observedCredentials
// — the operator's record of a credential it has already accounted for in the
// revocation diff. Shared by AgentIdentity and UserIdentity.
type ObservedCredential struct {
	// Name is the credential name from spec.credentials[].name.
	Name string `json:"name"`

	// Fingerprint is "<type>:<secretRefName>": stable across reconciles and
	// sensitive to both a type change (static↔oauth) and a Secret-ref rename,
	// which is how an OAuth refresh surfaces a replaced credential.
	Fingerprint string `json:"fingerprint"`

	// SecretNames are the backing Secret names for this credential, recorded
	// so a REMOVED credential's revoke key can be reconstructed without
	// re-reading a spec that no longer mentions it.
	//
	// Empty for UserIdentity, whose backing Secret name is derived from
	// (identity name, credential name) rather than referenced by the spec.
	// +optional
	SecretNames []string `json:"secretNames,omitempty"`
}

const (
	AgentIdentityConditionValid   = "Valid"
	AgentIdentityConditionRefresh = "Refresh"

	// AgentIdentityConditionPlatformLinked reports whether this identity is
	// linked to the singleton SpiceDB platform object
	// (agentidentity:<ns>/<name>#platform@platform:platform).
	//
	// That ONE relationship is the only live arm of
	// agentidentity#update_credential — the permission gating "a human replaces
	// this agent's own dead shared credential" — so while it is absent every
	// such check fails closed, otherwise SILENTLY: the schema compiles, the card
	// publishes, the button renders, the click is refused.
	//
	// Deliberately NOT folded into Valid: an identity whose credentials all
	// resolve is usable by every session, and Valid=False over a SpiceDB-side
	// problem would stop agents that work fine. What is broken is only the
	// operator's ability to REPAIR it later.
	AgentIdentityConditionPlatformLinked = "PlatformLinked"
)

func init() {
	SchemeBuilder.Register(&AgentIdentity{}, &AgentIdentityList{})
}
