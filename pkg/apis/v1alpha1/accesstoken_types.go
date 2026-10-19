package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AccessToken is the AUTHENTICATION record for one inbound delegated
// credential minted at OAuth consent: hash → token identity, plus display
// metadata and expiry. It deliberately carries NO role and NO scope — the
// authorization truth lives entirely in SpiceDB on accesstoken:<name>
// (see pkg/authz/spicedb/schema/schema.zed). Deleting this object is
// revocation: the finalizer removes the SpiceDB tuples.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,spicebox},shortName=atok
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Owner",type="string",JSONPath=".spec.owner"
// +kubebuilder:printcolumn:name="Client",type="string",JSONPath=".spec.clientName"
// +kubebuilder:printcolumn:name="Expires",type="string",JSONPath=".spec.expiresAt"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
type AccessToken struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AccessTokenSpec   `json:"spec,omitempty"`
	Status AccessTokenStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.tokenHash == oldSelf.tokenHash",message="tokenHash is immutable"
// +kubebuilder:validation:XValidation:rule="self.owner == oldSelf.owner",message="owner is immutable"
type AccessTokenSpec struct {
	// TokenHash is hex(sha256(bearer value)). The value itself is never
	// stored server-side.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	TokenHash string `json:"tokenHash"`

	// Owner is the canonical user id the token acts as.
	// +kubebuilder:validation:MinLength=1
	Owner string `json:"owner"`

	// ClientName is the DCR-supplied display name, sanitized at mint.
	// +optional
	ClientName string `json:"clientName,omitempty"`

	// ClientID is the OAuth client the token was issued to.
	// +optional
	ClientID string `json:"clientID,omitempty"`

	// ExpiresAt is when the token stops authenticating. The SpiceDB tuples
	// carry the same instant via the expiration trait; this copy gates the
	// authentication (hash-lookup) step without a SpiceDB round trip.
	ExpiresAt metav1.Time `json:"expiresAt"`
}

type AccessTokenStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// LastUsedAt is observation, controller-owned, updated on meaningful
	// change only (not every request).
	// +optional
	LastUsedAt *metav1.Time `json:"lastUsedAt,omitempty"`
}

// +kubebuilder:object:root=true
type AccessTokenList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AccessToken `json:"items"`
}

const (
	// AccessTokenConditionReady is True once the reconciler has observed the
	// object and its finalizer is in place.
	AccessTokenConditionReady = "Ready"
)

func init() {
	SchemeBuilder.Register(&AccessToken{}, &AccessTokenList{})
}
