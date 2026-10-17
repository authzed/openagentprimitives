package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,spicebox},shortName=btb
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.access.mode`
// +kubebuilder:printcolumn:name="Interaction",type=string,JSONPath=`.spec.interaction`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +genclient
//
// BrowserToolbox declares a per-session browser the agent can drive: which
// sites it may reach and whether the LLM may act on the page or only read it.
//
// Namespaced. An AgentClass references it via AgentClassBrowserToolboxRef,
// and the AgentSession reconciler snapshots the referenced spec into
// status.resolvedBrowserToolboxes at session start (see
// ResolvedBrowserToolbox), frozen for the session's lifetime exactly like a
// SidecarToolbox.
//
// Phase A deliberately ships a narrow field subset: Access covers only an
// explicit allowlist/blocklist of sites (Task 1's pkg/tools/browserpolicy
// decides admission from it), Interaction is act/readonly, and State is
// pinned to "ephemeral" -- there is no persistent browser profile/identity
// yet. Those arrive in later phases (identity-sourced sites in Phase B,
// persistent state in Phase C) as additive fields, not a breaking change to
// this spec.
type BrowserToolbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Two Phase B rules live here rather than on BrowserToolboxSpec/BrowserAccess
	// (see that type's doc comment for why): this field is the ONE place a CRD
	// validation marker is legal, because BrowserToolboxSpec is also embedded
	// by value into AgentSession.status.resolvedBrowserToolboxes[].Spec, and a
	// rule reachable from a status path trips both the status-CEL guard and the
	// apiserver's rule-cost budget across that unbounded array. Precedent:
	// workshop_types.go:44-66 attaches its XValidation rules the same way, on
	// the field, not the type.
	// +kubebuilder:validation:XValidation:rule="self.access.mode != 'blocklist' || (!has(self.access.identity) && !self.access.identitySites)",message="a blocklist (general-access) browser cannot carry an identity or identitySites"
	// +kubebuilder:validation:XValidation:rule="!self.access.identitySites || has(self.access.identity)",message="identitySites requires identity"
	Spec   BrowserToolboxSpec   `json:"spec,omitempty"`
	Status BrowserToolboxStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
//
// BrowserToolboxList is a list of BrowserToolbox.
type BrowserToolboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BrowserToolbox `json:"items"`
}

// BrowserToolboxSpec is deliberately not annotated with a CEL XValidation
// rule: this type is embedded by value into AgentSession's
// status.resolvedBrowserToolboxes snapshot (see ResolvedBrowserToolbox.Spec),
// and a CEL rule reachable from a status path both trips the status-CEL guard
// and risks the apiserver's CRD rule-cost budget across that unbounded status
// array -- the same reasoning SidecarToolboxSpec documents. Field-level
// markers (Enum, Pattern, default) are the validation surface here.
type BrowserToolboxSpec struct {
	// Access is the site allowlist/blocklist the browser's egress is
	// evaluated against.
	Access BrowserAccess `json:"access"`

	// Interaction gates whether the LLM may act on the page (click, type,
	// navigate) or only read its rendered content.
	//   act      -- full interaction.
	//   readonly -- observation only; no mutating actions are dispatched.
	// +kubebuilder:validation:Enum=act;readonly
	// +kubebuilder:default=act
	Interaction string `json:"interaction,omitempty"`

	// State selects how the browser's session state (cookies, storage) is
	// retained across turns.
	// Phase A: ephemeral is the only state; persistent arrives with identities (Phase C).
	// +kubebuilder:validation:Enum=ephemeral
	// +kubebuilder:default=ephemeral
	State string `json:"state,omitempty"`
}

// BrowserAccess is the egress policy a BrowserToolbox's browser is evaluated
// against. Mode selects allowlist or blocklist semantics (literal values
// agree with pkg/tools/browserpolicy.ModeAllowlist/ModeBlocklist); Sites is
// the list Mode interprets.
type BrowserAccess struct {
	// Mode is "allowlist" (only Sites are reachable) or "blocklist" (every
	// site except Sites is reachable).
	// +kubebuilder:validation:Enum=allowlist;blocklist
	Mode string `json:"mode"`

	// Sites is the set of hosts Mode interprets. Matching is suffix-by-label
	// unless Exact is set: a Site with Host "example.com" also matches
	// "www.example.com" and "a.b.example.com", but never a lookalike like
	// "notexample.com" -- pkg/tools/browserpolicy.Match does label-boundary
	// comparison, not a raw string suffix check. Exact pins the entry to the
	// literal host only.
	// +optional
	// +listType=map
	// +listMapKey=host
	Sites []BrowserSite `json:"sites,omitempty"`

	// Identity is the name of an AgentIdentity (in this BrowserToolbox's
	// namespace) carrying a directCredentials credential this browser binds
	// to -- a Phase B addition. Allowlist-only: the two XValidation rules on
	// BrowserToolbox.Spec refuse it under Mode=blocklist, because a
	// general-access browser has no legitimate use for a credential
	// reference. Identity name syntax and existence are not checked here --
	// that is the Task 7 BrowserToolbox reconciler's job (Valid=False when
	// the reference is missing or has no directCredentials credential).
	// +optional
	Identity string `json:"identity,omitempty"`

	// IdentitySites, when true, folds every host Identity's directCredentials
	// catalog lists into this allowlist, alongside any explicit Sites -- a
	// Phase B addition. Requires Identity to be set (enforced by the second
	// XValidation rule on BrowserToolbox.Spec). This field only records the
	// bit; deriving the actual host names from the identity's source happens
	// in the AgentSession reconciler (DeriveBrowserEffectiveSites, Task 7) and
	// is recorded onto status, not here.
	// +optional
	// +kubebuilder:default=false
	IdentitySites bool `json:"identitySites,omitempty"`
}

// BrowserSite is one host entry in a BrowserAccess list.
type BrowserSite struct {
	// Host is the DNS hostname this entry covers, matched suffix-by-label
	// (see BrowserAccess.Sites) unless Exact is set.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?)*$`
	// +kubebuilder:validation:MaxLength=253
	Host string `json:"host"`

	// Exact pins this entry to the literal Host only, disabling the default
	// suffix-by-label subdomain match.
	// +optional
	Exact bool `json:"exact,omitempty"`
}

// BrowserToolboxStatus is intentionally minimal in Phase A: a condition set
// and the generation it reflects. There is no admission-time probe the way
// SidecarToolbox has one -- a BrowserToolbox has no image to reach; its own
// validity is purely a function of its spec.
type BrowserToolboxStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries this BrowserToolbox's Valid condition.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

func init() {
	SchemeBuilder.Register(&BrowserToolbox{}, &BrowserToolboxList{})
}
