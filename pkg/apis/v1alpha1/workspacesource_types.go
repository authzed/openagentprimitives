package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,spicebox},shortName=wsrc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Kind",type="string",JSONPath=".spec.source.kind"
// +kubebuilder:printcolumn:name="Valid",type="string",JSONPath=".status.conditions[?(@.type=='Valid')].status"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient

// WorkspaceSource declares a named, pluggable origin that per-session
// workspaces are cut from. The origin kind is registered
// (pkg/platform/workspacekinds); git is the backend that exists today.
//
// Namespaced. Reconciled by pkg/controllers/workspacesource, which validates
// the spec against the named kind, provisions a shared base PVC, and runs a
// Job to materialize the checkout into it. Sessions never clone the origin
// themselves: the AgentSession reconciler cuts a copy-on-write overlay off
// that shared base once, then freezes it for the session lifetime, so
// re-pointing this object mid-session does not move a running workspace.
type WorkspaceSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkspaceSourceSpec   `json:"spec,omitempty"`
	Status WorkspaceSourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type WorkspaceSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkspaceSource `json:"items"`
}

type WorkspaceSourceSpec struct {
	// Source is the driver and the location it points at.
	Source WorkspaceSourceRef `json:"source"`
	// Scope narrows which subpaths a session may see; empty exposes everything.
	// +optional
	Scope WorkspaceScope `json:"scope,omitempty"`
	// Base configures the shared read-cache every session overlays off.
	// +optional
	Base WorkspaceBase `json:"base,omitempty"`
}

// WorkspaceSourceRef identifies the driver and what it points at.
type WorkspaceSourceRef struct {
	// Kind selects the registered driver (e.g. "git").
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+$`
	Kind string `json:"kind"`
	// Locator is the driver-parsed source location (repo URL, file:// path, …).
	Locator string `json:"locator"`
	// Ref is the driver-specific revision (a git branch, tag or SHA); empty
	// takes the driver's default.
	// +optional
	Ref string `json:"ref,omitempty"`
	// Config is driver-specific extra configuration, opaque here.
	// +optional
	Config map[string]string `json:"config,omitempty"`
}

// WorkspaceScope is the exposure boundary — the allowlist of subpaths.
type WorkspaceScope struct {
	// Paths is the allowlist; empty exposes the whole checkout.
	// +optional
	Paths []WorkspaceScopePath `json:"paths,omitempty"`
}

type WorkspaceScopePath struct {
	// Path is a subpath relative to the checkout root.
	Path string `json:"path"`
	// Writable permits the session to modify this subtree; default is read-only.
	// +optional
	Writable bool `json:"writable,omitempty"`
}

// WorkspaceBase configures the shared read-cache base.
type WorkspaceBase struct {
	// Size is the base PVC request (default 2Gi).
	// +optional
	Size string `json:"size,omitempty"`
	// Refresh controls whether/how often the shared base is re-pulled from the
	// origin. "" or "onDemand" (default) never auto-refreshes. A Go duration
	// (e.g. "10m") re-pulls the base on that interval so new sessions overlay
	// off a near-current base and pull only the delta.
	// +optional
	Refresh string `json:"refresh,omitempty"`
}

type WorkspaceSourceStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// BaseClaimName is the shared base PVC sessions cut their overlay from.
	// +optional
	BaseClaimName string `json:"baseClaimName,omitempty"`
	// LastMaterializedAt is when the base checkout was first populated.
	// +optional
	LastMaterializedAt *metav1.Time `json:"lastMaterializedAt,omitempty"`
	// LastRefreshedAt is when a scheduled base refresh last completed.
	// +optional
	LastRefreshedAt *metav1.Time `json:"lastRefreshedAt,omitempty"`
	// Conditions carries Valid and Ready.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

const (
	WorkspaceSourceConditionValid = "Valid"
	WorkspaceSourceConditionReady = "Ready"
)

func init() {
	SchemeBuilder.Register(&WorkspaceSource{}, &WorkspaceSourceList{})
}
