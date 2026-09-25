package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,agentprimitives},shortName=arender
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Kind",type="string",JSONPath=".spec.kind"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="MIME",type="string",JSONPath=".status.outputMIME"
// +kubebuilder:printcolumn:name="Size",type="integer",JSONPath=".status.outputSize"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
//
// ArtifactRender is one request to turn agent-supplied bytes into a
// deliverable artifact -- an HTML page, an SVG, an image -- through a named
// renderer plug-in. The agent creates it; the rendered output is persisted to
// the artifactstore and referenced from status.
//
// Namespaced. Reconciled by pkg/controllers/artifactrender, which drives the
// phase machine Pending -> Rendering -> Ready/Failed by dispatching to the
// renderer registered under spec.kind
// (pkg/channels/channelassets/registry).
type ArtifactRender struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ArtifactRenderSpec   `json:"spec,omitempty"`
	Status ArtifactRenderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ArtifactRenderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ArtifactRender `json:"items"`
}

type ArtifactRenderSpec struct {
	// Kind names the renderer plug-in (matches the registered Kind() value).
	// Pattern: lowercase, dash-separated. Example: "html".
	Kind string `json:"kind"`

	// Filename is a delivery hint for the channel's file-upload primitive.
	// Renderers may rewrite (e.g. force ".html" extension).
	// +optional
	Filename string `json:"filename,omitempty"`

	// AltText is the accessibility / channel-side preview text shown when
	// the artifact can't render. Required for image-MIME renderer outputs;
	// optional otherwise (the runner enforces in prepare_artifact).
	// +optional
	AltText string `json:"altText,omitempty"`

	// Payload is the raw input bytes. Exactly one of Payload (inline,
	// ≤256 KiB) or PayloadRef (artifactstore) MUST be set.
	// +optional
	Payload []byte `json:"payload,omitempty"`

	// PayloadRef is an artifactstore.Ref the controller fetches the input bytes
	// from, for payloads too large to inline. Deleted with the CR.
	// +optional
	PayloadRef string `json:"payloadRef,omitempty"`

	// TimeoutSeconds is the agent-requested per-render budget. Operator
	// caps at 300; default 30.
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=300
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// CSP is a widget's declared `_meta.ui.csp` (mcp-ui / MCP Apps protocol),
	// carried from the MCP tool result so the session-view page's per-widget CSP
	// can honor domains the widget says it needs instead of falling back to the
	// restrictive same-origin default. nil when the widget declared none, and
	// always nil for non-mcpui renders. MCP-server-authored and therefore
	// UNTRUSTED: every consumer sanitizes each domain before use.
	// +optional
	CSP *WidgetCSP `json:"csp,omitempty"`
}

// WidgetCSP mirrors a widget's `_meta.ui.csp` (mcp-ui / MCP Apps protocol):
// the domains it declares it needs beyond the same-origin defaults. See
// pkg/web/webui/sessionview/widgets.go's buildWidgetCSP for the exact directive
// mapping and pkg/web/webui/sessionview.WidgetCSPMeta for the package-local DTO
// mirror consumed by the session-view page.
type WidgetCSP struct {
	// ConnectDomains lists the origins the widget needs XHR/fetch/WebSocket
	// access to (maps to the CSP connect-src directive).
	// +optional
	ConnectDomains []string `json:"connectDomains,omitempty"`
	// ResourceDomains lists the origins the widget loads scripts, styles,
	// and images from (maps to script-src/style-src/img-src).
	// +optional
	ResourceDomains []string `json:"resourceDomains,omitempty"`
	// FrameDomains lists the origins the widget may embed as a nested
	// frame (maps to frame-src).
	// +optional
	FrameDomains []string `json:"frameDomains,omitempty"`
}

// ArtifactRenderPhase is the closed-enum lifecycle state.
// +kubebuilder:validation:Enum=Pending;Rendering;Ready;Failed
type ArtifactRenderPhase string

const (
	ArtifactRenderPhasePending   ArtifactRenderPhase = "Pending"
	ArtifactRenderPhaseRendering ArtifactRenderPhase = "Rendering"
	ArtifactRenderPhaseReady     ArtifactRenderPhase = "Ready"
	ArtifactRenderPhaseFailed    ArtifactRenderPhase = "Failed"
)

// SanitizerWarning is a structured finding from the artifact renderer's
// sanitizer, surfaced to the agent so it can adjust the source. Mirrors
// channelassets.Warning (the apis package cannot import channelassets).
type SanitizerWarning struct {
	// kind is "tag", "attr", or "css".
	Kind string `json:"kind"`
	// name is the element, attribute, or CSS construct affected.
	Name string `json:"name"`
	// action is "unwrapped", "removed", "stripped", or "kept".
	Action string `json:"action"`
	// count is the number of occurrences.
	Count int `json:"count"`
	// note is an optional actionable hint.
	// +optional
	Note string `json:"note,omitempty"`
}

type ArtifactRenderStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is the render lifecycle position.
	// +optional
	Phase ArtifactRenderPhase `json:"phase,omitempty"`

	// OutputRef is the artifactstore ref the rendered bytes landed under.
	// Set when phase=Ready.
	// +optional
	OutputRef string `json:"outputRef,omitempty"`
	// OutputMIME is the rendered artifact's content type.
	// +optional
	OutputMIME string `json:"outputMIME,omitempty"`
	// OutputSize is the rendered artifact's size in bytes.
	// +optional
	OutputSize int64 `json:"outputSize,omitempty"`
	// OutputFilename is the delivery filename, after any renderer rewrite.
	// +optional
	OutputFilename string `json:"outputFilename,omitempty"`
	// Warnings are the sanitizer's findings, surfaced so the agent can fix its
	// source; a non-empty list does not mean the render failed.
	// +optional
	// +listType=atomic
	Warnings []SanitizerWarning `json:"warnings,omitempty"`

	// FailureReason is the machine-readable failure code. Set when phase=Failed.
	// +optional
	FailureReason string `json:"failureReason,omitempty"`
	// FailureMessage is the human-readable detail behind FailureReason.
	// +optional
	FailureMessage string `json:"failureMessage,omitempty"`

	// StartedAt is when rendering began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// FinishedAt is when rendering reached Ready or Failed.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// Conditions carries Ready; see ArtifactRenderConditionReady.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

const (
	ArtifactRenderConditionReady = "Ready"
)

func init() {
	SchemeBuilder.Register(&ArtifactRender{}, &ArtifactRenderList{})
}
