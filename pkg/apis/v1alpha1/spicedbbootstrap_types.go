package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,spicebox},shortName=spicedbstrap
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Valid",type="string",JSONPath=".status.conditions[?(@.type=='Valid')].status"
// +kubebuilder:printcolumn:name="SchemaIncluded",type="string",JSONPath=".status.conditions[?(@.type=='SchemaIncluded')].status"
// +kubebuilder:printcolumn:name="RelationshipsApplied",type="string",JSONPath=".status.conditions[?(@.type=='RelationshipsApplied')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient

// SpiceDBBootstrap declaratively seeds SpiceDB state: an optional schema
// fragment and/or a set of relationships TOUCHed into SpiceDB. At least one of
// spec.spicedbSchema.resources and spec.relationships MUST be non-empty.
//
// Namespaced. It has no reconciler of its own -- pkg/controllers/guardian
// watches it from the AgentSessionGrants loop, composes its fragment into the
// unified schema alongside MCPServer fragments, and refcounts its
// relationships across every CR that claims them, so deleting one CR removes
// only the tuples no other CR still owns.
type SpiceDBBootstrap struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SpiceDBBootstrapSpec   `json:"spec,omitempty"`
	Status SpiceDBBootstrapStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SpiceDBBootstrapList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpiceDBBootstrap `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.spicedbSchema) && has(self.spicedbSchema.resources) && size(self.spicedbSchema.resources) > 0) || (has(self.relationships) && size(self.relationships) > 0)",message="spec must declare at least one of spicedbSchema.resources or relationships"
type SpiceDBBootstrapSpec struct {
	// SpiceDBSchema is an optional schema fragment composed into the
	// unified SpiceDB schema by the guardian reconciler. Same shape and
	// rules as MCPServer.spec.spicedbSchema. Must not redeclare 'user'.
	// +optional
	SpiceDBSchema *SpiceDBSchemaFragment `json:"spicedbSchema,omitempty"`

	// Relationships is an optional list of tuples to TOUCH into SpiceDB.
	// With reclaimPolicy=Delete (default), the controller also DELETEs
	// tuples that were previously TOUCHed but have since dropped out
	// of the cluster-wide desired set (refcounted across all
	// SpiceDBBootstrap CRs).
	// +optional
	// +listType=atomic
	Relationships []SpiceDBBootstrapRelationship `json:"relationships,omitempty"`

	// ReclaimPolicy controls cleanup of tuples this CR previously
	// wrote. Delete (default) reconciles to desired state via
	// cluster-wide refcounting; Retain TOUCHes on each reconcile but
	// never DELETEs and does not participate in refcount.
	// +kubebuilder:validation:Enum=Delete;Retain
	// +kubebuilder:default=Delete
	// +optional
	ReclaimPolicy string `json:"reclaimPolicy,omitempty"`
}

type SpiceDBBootstrapRelationship struct {
	// Resource is the tuple's left-hand object.
	Resource SpiceDBObjectRef `json:"resource"`
	// Relation is the relation name declared on Resource's type.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*$`
	Relation string `json:"relation"`
	// Subject is the tuple's right-hand subject.
	Subject SpiceDBSubjectRef `json:"subject"`
}

type SpiceDBObjectRef struct {
	// Type is the SpiceDB definition name.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*(/[a-z][a-z0-9_]*)*$`
	Type string `json:"type"`
	// SpiceDB's object_id regex is `^(([a-zA-Z0-9/_|\-=+]{1,})|\*)$` — letters,
	// digits, and _ / | - = + . The pattern below admits all of those, plus '@'
	// and '.' so that an email can be carried verbatim when
	// subject.canonicalize=true; the controller's L2 validation rejects an '@'
	// in a user subject when canonicalize is false.
	//
	// '=' is load-bearing beyond email. It is the escape character
	// authz.spicedb_escape uses to fit a free-form value — a repository URL —
	// into an object id INJECTIVELY, which is what lets an approval name one
	// repository without also covering another. A bootstrap that seeds a grant
	// on such a value has to be able to express the id the check will compute,
	// so a pattern narrower than SpiceDB's own rejects legal ids.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_/.@=|+-]+$`
	// +kubebuilder:validation:MaxLength=1024
	ID string `json:"id"`
}

type SpiceDBSubjectRef struct {
	// Type is the SpiceDB definition name of the subject.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*(/[a-z][a-z0-9_]*)*$`
	Type string `json:"type"`
	// ID is required UNLESS Wildcard=true (which forces the wire-form
	// SpiceDB subject id to "*", the wildcard match-any value).
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_/.@=|+-]+$`
	// +kubebuilder:validation:MaxLength=1024
	ID string `json:"id,omitempty"`
	// Relation, when set, makes this a subject-set reference like
	// "group:eng#member". Mutually exclusive with canonicalize=true
	// and wildcard=true.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*$`
	Relation string `json:"relation,omitempty"`
	// Canonicalize, when true, runs ID through identity.EmailReference(id).Canonical()
	// before writing. Only valid when Type=="user" and ID is an email.
	// Mutually exclusive with wildcard=true.
	// +optional
	Canonicalize bool `json:"canonicalize,omitempty"`
	// Wildcard, when true, writes the SpiceDB wildcard subject
	// (`<type>:*`) — every subject of Type matches. Requires the
	// corresponding relation on the target resource to itself be
	// wildcard-typed in the schema (see
	// SpiceDBRelation.Wildcard). Mutually exclusive with
	// Relation and Canonicalize. When true, ID is ignored.
	// +optional
	Wildcard bool `json:"wildcard,omitempty"`
}

type SpiceDBBootstrapStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ObservedRelationships is the count of tuples in spec.relationships
	// that the controller successfully TOUCHed on the last reconcile.
	// +optional
	ObservedRelationships int32 `json:"observedRelationships,omitempty"`
	// Conditions carries Valid, SchemaIncluded and RelationshipsApplied.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// SpiceDBBootstrap condition types.
const (
	SpiceDBBootstrapConditionValid                = "Valid"
	SpiceDBBootstrapConditionSchemaIncluded       = "SchemaIncluded"
	SpiceDBBootstrapConditionRelationshipsApplied = "RelationshipsApplied"
)

// SpiceDBBootstrap reclaim policies.
const (
	SpiceDBBootstrapReclaimDelete = "Delete"
	SpiceDBBootstrapReclaimRetain = "Retain"
)

// SpiceDBBootstrap condition reasons.
const (
	ReasonCanonicalizeRequiresUserEmail = "CanonicalizeRequiresUserEmail"
	ReasonUserSubjectNotCanonical       = "UserSubjectNotCanonical"
	ReasonSubjectSetCannotCanonicalize  = "SubjectSetCannotCanonicalize"
	ReasonSubjectWildcardConflict       = "SubjectWildcardConflict"
	ReasonCannotRedeclareUser           = "CannotRedeclareUser"
	// ReasonRelationOwnedByAnotherSource marks a relationship whose
	// resourceType#relation is claimed by a different in-tree writer (see
	// pkg/authz/spicedb/relsource). The write-time guard
	// (spicedb.TouchBootstrapRelationshipVia) refuses the same relationship;
	// this reason surfaces that refusal on the CR itself instead of only in a
	// reconcile log.
	ReasonRelationOwnedByAnotherSource = "RelationOwnedByAnotherSource"
	// ReasonSpicedbSchemaFragmentInvalid is the SchemaIncluded=False reason
	// when this CR's own spec.spicedbSchema fragment is invalid ON ITS OWN
	// (guardianschema.ValidateFragment) — a RawZed syntax error, or it
	// redeclares a reserved scaffold definition. Distinct from
	// ReasonSpicedbSchemaConflict below, which is for a fragment that is
	// valid alone but collides with an already-accepted one.
	ReasonSpicedbSchemaFragmentInvalid = "SpicedbSchemaFragmentInvalid"
	// ReasonSpicedbSchemaConflict is the SchemaIncluded=False reason when
	// this CR's fragment is valid alone but collides with an already-accepted
	// fragment (guardianschema.PartitionCompatibleFragments) — see
	// ReasonSpicedbSchemaFragmentInvalid above for the invalid-alone case.
	ReasonSpicedbSchemaConflict = "SpicedbSchemaConflict"
	ReasonAllTouched            = "AllTouched"
	ReasonPartialApply          = "PartialApply"
	ReasonSpiceDBUnavailable    = "SpiceDBUnavailable"
	ReasonSpiceDBAuthFailed     = "SpiceDBAuthFailed"
	ReasonSpiceDBRejected       = "SpiceDBRejected"
	ReasonSchemaNotReady        = "SchemaNotReady"
	ReasonSchemaWriteFailed     = "SchemaWriteFailed"
)

func init() {
	SchemeBuilder.Register(&SpiceDBBootstrap{}, &SpiceDBBootstrapList{})
}
