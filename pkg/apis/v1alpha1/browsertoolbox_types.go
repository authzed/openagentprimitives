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
// status.resolvedBrowserToolboxes (see ResolvedBrowserToolbox). UNLIKE a
// SidecarToolbox's snapshot, this one is NOT frozen for the session's
// lifetime: the reconciler re-Gets this CR's spec on every reconcile, not
// just at session start, so a mid-session edit to Access/Interaction/State
// propagates on the next reconcile -- into EffectiveSites, the gate's policy
// ConfigMap, and (per C-R5) the resolved State. A security reader relying on
// the spec at session start remaining in force for the rest of the session
// would be wrong; only the per-session pod/token/gate-port allocation and the
// identity login catalog (barring a wake or a token rotation) are pinned.
//
// Phase A shipped a narrow field subset: Access covered only an explicit
// allowlist/blocklist of sites (Task 1's pkg/tools/browserpolicy decides
// admission from it) and Interaction was act/readonly. Phase B added
// identity-sourced sites (Access.Identity, IdentitySites) and Phase C added
// persistent state (State, BrowserSite.Unattended) -- both as additive
// fields, not a breaking change to this spec. State is no longer pinned to
// "ephemeral": an unset value still defaults in code (persistent iff an
// identity is bound, see BrowserToolboxSpec.State), but an explicit value in
// either direction is honored as written.
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
	// A Phase C rule, same reasoning as the two above: an `unattended` site's
	// web_host#act grant is written unconditionally at session start (C8, no
	// human card) for a bound identity's browsing session, so it needs both an
	// allowlist (a blocklist's "every site but these" shape has no per-site
	// standing to grant against) and a bound identity (there is no session
	// approver to ask if the binding is missing, and the grant is meant to
	// stand in for that identity's own sign-in).
	//
	// The leading `!has(self.access.sites)` guard is load-bearing, not
	// decorative: Sites is `+optional`, and CEL's list access on an absent
	// optional field ERRORS rather than reading empty -- so
	// `self.access.sites.exists(...)` alone throws on every sites-less
	// BrowserToolbox, and that error is absorbed only when the right `||`
	// operand happens to be true. A sites-less blocklist (the common
	// general-access shape) can never satisfy that operand, so without the
	// guard every sites-less blocklist create was rejected with this rule's own
	// message -- an admission/CLI-validator disagreement, since
	// pkg/tools/kinds/browsertoolbox/kind.go accepts a sites-less shape. The
	// guard short-circuits before the field access is ever attempted.
	// +kubebuilder:validation:XValidation:rule="!has(self.access.sites) || !self.access.sites.exists(s, s.unattended) || (self.access.mode == 'allowlist' && has(self.access.identity))",message="an unattended site requires access.mode=allowlist and access.identity"
	// A Phase C (Task C9) rule, same field-scoped reasoning as the four above: an
	// explicit state=persistent captures cookies + localStorage into a per-session
	// Secret that survives reap, which is only legitimate for a credentialed
	// (identity-bound) browser -- a general browser has nothing to keep and a
	// long-lived profile of arbitrary-site cookies is only a tracking surface
	// (spec §1.2, §1.4). An UNSET state is left to the AgentSession snapshot to
	// default (persistent iff an identity is bound, else ephemeral), so the rule
	// short-circuits on absence; it fires only on an EXPLICIT persistent.
	// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state != 'persistent' || has(self.access.identity)",message="a persistent browser requires access.identity"
	Spec   BrowserToolboxSpec   `json:"spec,omitempty"`
	Status BrowserToolboxStatus `json:"status,omitempty"`
}

// Browser session-state kinds (BrowserToolboxSpec.State). The CRD states the
// legal set as a kubebuilder Enum marker on that field; these constants are the
// Go-side source of truth the AgentSession snapshot defaulting (controller.go),
// the state-Secret lifecycle (browserpod.go), and the runner's capture/inject
// wiring compare against, so a literal "persistent"/"ephemeral" never drifts.
//
// The field is INTENTIONALLY not defaulted at admission (no kubebuilder:default):
// the effective value is identity-driven and resolved in code at the snapshot
// (spec §1.4), so an unset field must stay distinguishably empty at the API --
// a static default would mask the identity-driven one.
const (
	// BrowserStateEphemeral: a clean profile every pod boot; nothing captured.
	BrowserStateEphemeral = "ephemeral"
	// BrowserStatePersistent: cookies + localStorage for the effective sites are
	// captured into a per-session Secret and re-injected across reap/wake.
	BrowserStatePersistent = "persistent"
)

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
	// retained across turns: "persistent" captures cookies + localStorage for the
	// effective sites into a per-session Secret and re-injects them across reap and
	// wake (spec §1.4); "ephemeral" keeps a clean profile every pod boot and
	// captures nothing. An explicit "persistent" requires access.identity (the
	// field-scoped XValidation rule on BrowserToolbox.Spec).
	//
	// DELIBERATELY NOT DEFAULTED at admission. An unset value is resolved in code
	// at the AgentSession snapshot (controller.go) to "persistent" when an identity
	// is bound and "ephemeral" otherwise -- a static kubebuilder default would mask
	// that identity-driven default, so the field is left omitempty and
	// enum-but-not-defaulted so an unset field stays distinguishably empty.
	// +kubebuilder:validation:Enum=ephemeral;persistent
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
	//
	// MinLength=1 makes an explicit `identity: ""` unrepresentable at
	// admission. Without it, CEL's `has(self.access.identity)` reads TRUE for
	// an explicit empty string -- has() asks only whether the field was set,
	// not whether it is non-empty -- so `{mode: allowlist, identity: "",
	// state: persistent}` satisfied the persistent-requires-identity rule
	// (BrowserToolbox.Spec's XValidation above) while every Go-side read
	// (`!= ""`) and the CLI mirror treat "" as "no identity bound". That gap
	// had no runtime backstop: the session controller's ensureBrowserStateSecret
	// and resolveBrowserIdentityPins honor an explicit persistent state and
	// grant the runner a reap-surviving cookie jar for a browser with no bound
	// identity -- the exact tracking surface this field's persistence rule
	// exists to refuse. MinLength=1 aligns has() with every other reader.
	// +optional
	// +kubebuilder:validation:MinLength=1
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

	// Unattended marks this site's web_host#act grant as operator-approved: the
	// AgentSession reconciler's runner writes the slot_grant_act relationship
	// for this host at session cold start, unconditionally, with no human
	// plan-gate card -- the operator's declaration of Unattended IS the
	// authorization, exactly as an approved card would be. A Phase C addition;
	// the third field-scoped XValidation rule on BrowserToolbox.Spec requires
	// access.mode=allowlist and access.identity whenever any site sets this, so
	// an unattended grant is always for a specific, bound-identity session and
	// never for a general-access (blocklist) browser.
	//
	// The default marker below lands a structural default on every site, even
	// one that omits the field entirely -- without it, the third XValidation
	// rule's `s.unattended` on a plain site with no explicit value errors
	// evaluation ("no such key: unattended") instead of reading false, and
	// CEL's fail-closed-on-error semantics then refuse EVERY BrowserToolbox
	// create, not just an unattended one.
	// +optional
	// +kubebuilder:default=false
	Unattended bool `json:"unattended,omitempty"`
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
