package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,spicebox},shortName=agrants
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Included",type="string",JSONPath=".status.conditions[?(@.type=='SchemaIncluded')].status"
// +kubebuilder:printcolumn:name="Pairs",type="integer",JSONPath=".status.observedPairCount"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient

// AgentSessionGrants is the per-AgentClass declaration of the (resourceType,
// permission) pairs that must exist as grant_<perm>_<resType> relations on the
// SpiceDB "agentsession" definition.
//
// Namespaced. Reconciled by pkg/controllers/guardian, which watches every
// AgentSessionGrants cluster-wide and composes their union -- together with
// the schema fragments contributed by MCPServer and SpiceDBBootstrap -- into a
// single agentsession schema block. No individual CR owns that block; the
// composition of all of them does, so a lone CR read in isolation does not
// tell you what SpiceDB actually enforces.
type AgentSessionGrants struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSessionGrantsSpec   `json:"spec,omitempty"`
	Status AgentSessionGrantsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentSessionGrantsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentSessionGrants `json:"items"`
}

type AgentSessionGrantsSpec struct {
	// Pairs is the unique set of (resourceType, permission) tuples this
	// class needs as grant relations. Order is normalized at write time.
	// +listType=map
	// +listMapKey=resourceType
	// +listMapKey=permission
	Pairs []GrantPair `json:"pairs,omitempty"`

	// Slots is the set of (resourceType, permission) tuples this class declares
	// through authz.slots — the INSTANCE axis.
	//
	// Shape-identical to Pairs and semantically its mirror image, which is why
	// it is a separate field rather than more entries in the same list. A Pair
	// makes the composer put a grant relation on the SESSION pointing at the
	// resource; a Slot makes it put one on the RESOURCE pointing at the session.
	// Collapsing them would lose exactly the distinction that decides whether
	// the resulting check is per-requester.
	// +listType=map
	// +listMapKey=resourceType
	// +listMapKey=permission
	// +optional
	Slots []GrantPair `json:"slots,omitempty"`
}

type GrantPair struct {
	// ResourceType is the SpiceDB definition name (e.g., "github_repo").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*(/[a-z][a-z0-9_]*)*$`
	ResourceType string `json:"resourceType"`

	// Permission is the SpiceDB permission name on ResourceType
	// (e.g., "read", "admin").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*$`
	Permission string `json:"permission"`
}

type AgentSessionGrantsStatus struct {
	// Conditions carries SchemaIncluded.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ObservedPairCount echoes len(spec.pairs) at the last observation.
	// +optional
	ObservedPairCount int32 `json:"observedPairCount,omitempty"`

	// ObservedSchemaWrittenAt records when the guardian controller last
	// confirmed (or wrote) a SpiceDB schema that includes these pairs.
	// +optional
	ObservedSchemaWrittenAt *metav1.Time `json:"observedSchemaWrittenAt,omitempty"`
}

// AgentSessionGrants condition types.
const (
	// AgentSessionGrantsConditionSchemaIncluded reports whether every pair
	// in spec.pairs is present in the live SpiceDB agentsession definition.
	AgentSessionGrantsConditionSchemaIncluded = "SchemaIncluded"
)

func init() {
	SchemeBuilder.Register(&AgentSessionGrants{}, &AgentSessionGrantsList{})
}
