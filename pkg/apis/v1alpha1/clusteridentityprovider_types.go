package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterIdentityProviderName is the enforced singleton object name.
const ClusterIdentityProviderName = "default"

// ClusterIdentityProvider condition type + reasons.
const (
	// ConditionIdPValid is the readiness condition identityd consults
	// before serving IdP login.
	ConditionIdPValid = "Valid"

	ReasonIdPReady           = "Ready"
	ReasonIdPSecretMissing   = "SecretMissing"
	ReasonIdPDiscoveryFailed = "DiscoveryFailed"
	ReasonIdPConfigInvalid   = "ConfigInvalid"
)

// ClusterSecretKeyRef points at a single key of a Secret in an
// explicit namespace — needed because the referring CRD is
// cluster-scoped and has no namespace of its own.
type ClusterSecretKeyRef struct {
	// Namespace is the Secret's namespace; required, since the referring CRD is
	// cluster-scoped.
	Namespace string `json:"namespace"`
	// Name is the Secret's name.
	Name string `json:"name"`
	// Key is the data key within the Secret holding the value.
	Key string `json:"key"`
}

// ClusterIdentityProviderSpec configures the cluster's human-login
// identity provider. The kind is dispatched through
// pkg/platform/identity/idp/registry.
type ClusterIdentityProviderSpec struct {
	// Kind names the registered idp kind: "oidc" or "google".
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Issuer is the OIDC issuer URL. Required for kind=oidc; must be
	// empty for kind=google (the kind pins it). Enforced by webhook.
	// +optional
	Issuer string `json:"issuer,omitempty"`

	// ClientID is the OAuth client id (not secret; inline).
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientID"`

	// ClientSecretRef names the Secret key holding the client secret.
	ClientSecretRef ClusterSecretKeyRef `json:"clientSecretRef"`

	// Scopes are extra scopes beyond openid/email/profile.
	// +optional
	Scopes []string `json:"scopes,omitempty"`

	// AllowedEmailDomains gates who counts as logged in. Empty with
	// AllowAnyEmail=false is invalid (fail closed) — enforced by webhook.
	// +optional
	AllowedEmailDomains []string `json:"allowedEmailDomains,omitempty"`

	// AllowAnyEmail must be set explicitly to run with no domain gate.
	// +optional
	AllowAnyEmail bool `json:"allowAnyEmail,omitempty"`

	// SessionTTL is the idd_session cookie lifetime for IdP-verified
	// logins (and the CLI assertion lifetime). Default 12h.
	// +optional
	SessionTTL *metav1.Duration `json:"sessionTTL,omitempty"`

	// Federation, when Enabled, turns this IdP into an EMA federation
	// source: login requests offline_access so the user's refresh token is
	// captured, and the broker mints upstream tokens via ID-JAG. Requires
	// kind=oidc and a confidential client (see the webhook).
	// +optional
	Federation *FederationConfig `json:"federation,omitempty"`
}

// FederationConfig toggles ID-JAG federation for the cluster IdP.
type FederationConfig struct {
	// Enabled turns this IdP into an EMA federation source.
	Enabled bool `json:"enabled,omitempty"`
}

// ClusterIdentityProviderStatus reports validity via conditions.
type ClusterIdentityProviderStatus struct {
	// Conditions carries Valid; identityd consults it before serving login.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ObservedGeneration is the generation the conditions describe.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories={authzed,spicebox},shortName=cidp
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Valid",type="string",JSONPath=".status.conditions[?(@.type=='Valid')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
// +genclient:nonNamespaced
//
// ClusterIdentityProvider configures cluster-wide human login: which
// registered idp.Kind authenticates people (pkg/platform/identity/idp), its
// OIDC client details, which email domains are admitted, and how long a
// browser session lives.
//
// Cluster-scoped singleton -- the only permitted name is "default"
// (ClusterIdentityProviderName). Reconciled by
// pkg/controllers/clusteridentityprovider, which shares its spec judgment with
// the admission webhook so the two can never disagree, and refuses a
// local-only kind on a non-local cluster.
type ClusterIdentityProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClusterIdentityProviderSpec   `json:"spec,omitempty"`
	Status            ClusterIdentityProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterIdentityProviderList contains a list of ClusterIdentityProvider.
type ClusterIdentityProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterIdentityProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterIdentityProvider{}, &ClusterIdentityProviderList{})
}
