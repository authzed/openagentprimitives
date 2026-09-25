package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,spicebox},shortName=relsrc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Kind",type="string",JSONPath=".spec.kind"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
//
// RelationshipSource declares one upstream directory (Slack first) to poll
// and sync into SpiceDB. The upstream kind is registered
// (pkg/platform/relsync); each poll pass writes the membership relationships
// that upstream currently reports and prunes ones it no longer reports.
//
// Namespaced. Reconciled by pkg/controllers/relationshipsource, which
// resolves spec.auth's credential, enumerates upstream scopes through the
// named kind, and writes/prunes relationships in SpiceDB one pass at a time.
// status.sync tracks progress within and across passes so a large directory
// is synced incrementally rather than in one reconcile.
type RelationshipSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RelationshipSourceSpec   `json:"spec,omitempty"`
	Status RelationshipSourceStatus `json:"status,omitempty"`
}

// RelationshipSourceSpec declares one upstream directory to sync.
type RelationshipSourceSpec struct {
	// Kind selects the registered relsync kind (e.g. "slack").
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Auth references the credential used to read the upstream.
	Auth RelationshipSourceAuth `json:"auth"`

	// Sync controls re-poll cadence.
	// +optional
	Sync RelationshipSourceSync `json:"sync,omitempty"`

	// BaseURL is the upstream endpoint for kinds whose service is
	// customer-hosted — the 1Password SCIM Bridge, for instance, which has no
	// constant address. Kinds that talk to a fixed vendor endpoint (Slack)
	// ignore it.
	//
	// Scheme and destination are deliberately unconstrained in the SCHEMA — no
	// kubebuilder Pattern, no host allowlist here. In-cluster (a Kubernetes
	// Service DNS name over http://, e.g.
	// http://scim.1password.svc.cluster.local) and loopback (a test/dev bridge
	// on http://127.0.0.1:<port>) are legitimate destinations — in fact the
	// primary deployment shape for a customer-hosted service — mirroring
	// MCPServerServer.URL's own doc (pkg/apis/v1alpha1/mcpserver_types.go).
	// For the same reason a kind dereferencing this field must NOT wrap its
	// client in pkg/x/safehttp: that guarded dialer refuses every
	// private/loopback IP, which is exactly what this field exists to carry.
	//
	// It is nonetheless a TENANT-WRITABLE DESTINATION, and the credential sent
	// to it must be scoped to it. This field and spec.auth sit on the same CR,
	// and anyone who can write the CR chooses both; the operator then reads the
	// named Secret with its OWN cluster-wide credentials and sends the value
	// here as a bearer token. An unconstrained pairing is credential
	// exfiltration with no `get secrets` needed — the SkillSource incident
	// (pkg/platform/identity/credhost's package doc) on an identical CR shape.
	// So when this field is set, the RelationshipSource controller requires the
	// referenced credential to declare spec.credentials[].allowedHosts and to
	// admit this host; it refuses the sync otherwise, on the Ready condition.
	// An empty value is checked against nothing: a kind with a constant vendor
	// endpoint has no tenant-writable destination to guard.
	//
	// Required by any kind that declares it needs one; such a kind refuses to
	// run rather than guessing when it is empty.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	BaseURL string `json:"baseURL,omitempty"`

	// Config is this kind's own configuration, opaque to the CRD: the
	// registered kind parses and validates it, so adding a kind never adds a
	// field here. Shape and required keys are the kind's to document (see
	// docs/relationshipsource.md).
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Config *apiextensionsv1.JSON `json:"config,omitempty"`
}

// RelationshipSourceAuth references a credential on an AgentIdentity used to
// read the upstream directory.
type RelationshipSourceAuth struct {
	// AgentIdentity is the AgentIdentity CR (same namespace) holding the credential.
	AgentIdentity string `json:"agentIdentity"`
	// Credential is the AgentCredential name on that identity (e.g. a slack bot token).
	Credential string `json:"credential"`
}

// RelationshipSourceSync controls cadence and per-pass cost.
type RelationshipSourceSync struct {
	// Interval between full passes. Zero → controller default (15m).
	// +optional
	Interval metav1.Duration `json:"interval,omitempty"`

	// MaxScopesPerPass bounds how many scopes one reconcile processes, so a
	// single enormous source cannot starve every other RelationshipSource
	// the operator manages. Zero → UNBOUNDED (the controller passes this
	// value straight through to relsync.Pass with no substituted default),
	// which is also why unbounded is the right choice unless this source's
	// scope count genuinely needs the budget: setting ANY bound below the
	// source's total scope count defers cross-resource reaping (an
	// identity/workspace-membership edge no scope still asserts) — that
	// reap only ever runs on a pass whose fetch coverage reaches every
	// enumerated scope, which a persistent bound may prevent from ever
	// happening.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxScopesPerPass int32 `json:"maxScopesPerPass,omitempty"`
}

// RelationshipSourceStatus is the observed state. Controller-owned.
type RelationshipSourceStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Sync is the resumption marker for the in-progress or most recently
	// completed enumeration cycle.
	// +optional
	Sync RelationshipSourceSyncStatus `json:"sync,omitempty"`

	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// RelationshipSourceSyncStatus is the resumption marker. Controller-owned.
type RelationshipSourceSyncStatus struct {
	// ResumeAfter is the ScopeID of the last scope completed this cycle.
	// Empty means the cycle starts from the beginning. The next pass takes
	// the first scope sorting AFTER this one — a comparison, not a lookup,
	// so a scope deleted between passes does not strand the cursor.
	//
	// This is deliberately a ScopeID and never an upstream pagination
	// token: upstream cursors expire, ours does not.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	ResumeAfter string `json:"resumeAfter,omitempty"`

	// EnumComplete reports whether scope enumeration finished this cycle.
	// Orphan reaping is gated on it: a truncated enumeration is non-empty
	// and well-formed and short, and reaping against it would delete every
	// scope it never reached.
	// +optional
	EnumComplete bool `json:"enumComplete,omitempty"`

	// CycleStartedAt is when the current cycle began.
	// +optional
	CycleStartedAt *metav1.Time `json:"cycleStartedAt,omitempty"`

	// LastSyncTime is the last completed full cycle.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// LastPass summarizes the most recently recorded pass. Only the most
	// recent is kept: pass history belongs in the audit plane, not in a CR
	// status that would grow without bound.
	// +optional
	LastPass *RelationshipSourcePassStats `json:"lastPass,omitempty"`
}

// RelationshipSourcePassStats summarizes what ONE sync pass did. A pass is
// not always a whole cycle: with spec.sync.maxScopesPerPass set a cycle is
// several passes, and an event-driven wake processes a single scope. Scoped
// records that, so a one-scope wake cannot be misread as a full-directory
// result.
//
// Controller-owned observation. FinishedAt is stamped ONLY when the status is
// already being written for another reason — see applySyncResult, and the
// identical treatment CycleStartedAt/LastSyncTime get there. Restamping it on
// every pass would make every reconcile a status write, and the controller's
// self-watch has no predicate, so that is an infinite reconcile loop that
// re-runs the upstream Pass forever.
type RelationshipSourcePassStats struct {
	// ScopesProcessed is how many scopes the pass actually visited.
	// +optional
	ScopesProcessed int32 `json:"scopesProcessed,omitempty"`
	// Written is relationship tuples touched as additions.
	// +optional
	Written int32 `json:"written,omitempty"`
	// Pruned is tuples deleted by the per-scope diff or the cross-resource reap.
	// +optional
	Pruned int32 `json:"pruned,omitempty"`
	// ReapedScopes is scopes whose whole resource object was swept.
	// +optional
	ReapedScopes int32 `json:"reapedScopes,omitempty"`
	// JoinMisses is upstream members whose identity resolved to no platform
	// user, dropped rather than written. A nonzero, growing value is the
	// signature of a broken identity join.
	// +optional
	JoinMisses int32 `json:"joinMisses,omitempty"`
	// ScopeErrors is how many scopes failed this pass. Non-fatal by design
	// (relsync.Pass's own doc: "one scope failing is not a pass failure"), so
	// Ready stays True — this count, and
	// RelationshipSourceConditionPartialFailure alongside it, are the only
	// things that distinguish a partially-failed pass from a clean one.
	//
	// A count, never a length: ScopeErrorSamples below is capped, so
	// len(samples) says how many failures were SAMPLED, not how many there
	// were. 156 failing scopes report 156 here and a handful there.
	// +optional
	ScopeErrors int32 `json:"scopeErrors,omitempty"`
	// ScopeErrorSamples is a small, capped sample of this pass's scope
	// errors, sorted deterministically by (scope, message) and truncated to
	// the first few. Deliberately NOT the whole list: a status subresource
	// must not grow with the size of a failing directory, and an entire arm
	// of a sync failing is exactly when the list would be longest.
	//
	// Each message is scrubbed before it lands here — a URL's query string or
	// fragment is stripped (a scope error quotes an upstream request URL, and
	// both halves are where a credential would ride) and the text is bounded.
	// The sample set is also preserved verbatim across passes whose failure
	// count and sampled scopes are unchanged: error text that varies run to
	// run would otherwise make every reconcile a status write, and the
	// controller's self-watch has no predicate. The live example is a rate
	// limit, whose backoff the kind computes fresh from one upstream reset
	// timestamp on every pass — see the relationshipsource controller's
	// preserveStableSamples for that and for the two trades it accepts, and
	// FinishedAt's own note above for the same hazard in its first form.
	// +optional
	// +kubebuilder:validation:MaxItems=5
	ScopeErrorSamples []RelationshipSourceScopeError `json:"scopeErrorSamples,omitempty"`
	// Scoped reports that this pass processed an explicit scope subset.
	// +optional
	Scoped bool `json:"scoped,omitempty"`
	// FinishedAt is when the recorded pass completed.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
}

// RelationshipSourceScopeError is one sampled per-scope failure from a sync
// pass. Controller-owned observation.
type RelationshipSourceScopeError struct {
	// Scope is the upstream scope id that failed. EMPTY when the failure is
	// not attributable to a single scope — enumeration itself failing, or the
	// reap scan (see relsync.ScopeError's own doc).
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Scope string `json:"scope,omitempty"`
	// Message is the failure text, scrubbed and truncated. Never raw error
	// text: see ScopeErrorSamples above for what is removed and why.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
type RelationshipSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RelationshipSource `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RelationshipSource{}, &RelationshipSourceList{})
}
