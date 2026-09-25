package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkshopProbe is the tool-discovery probe object for an agent-builder
// workshop: the operator runs one pod against spec.image (or spec.script's
// baseImage, or spec.cliHelp's image) and records what it discovered — the
// probed tool set, help text, or a failure — on status. Namespaced to the
// workshop's own namespace; the sidecar SA creates it, the operator (never
// the sidecar) runs the pod and writes status.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=wprobe
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type WorkshopProbe struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workshop probe spec is immutable after creation"
	// +kubebuilder:validation:XValidation:rule="(has(self.image)?1:0)+(has(self.script)?1:0)+(has(self.cliHelp)?1:0)==1",message="exactly one of image, script, cliHelp must be set"
	Spec   WorkshopProbeSpec   `json:"spec,omitempty"`
	Status WorkshopProbeStatus `json:"status,omitempty"`
}

// WorkshopProbeSpec: exactly one of Image, Script, CliHelp, enforced at
// admission by the CEL rule on WorkshopProbe.Spec above (no webhook).
// TimeoutSeconds bounds the pod.
type WorkshopProbeSpec struct {
	Image   string                `json:"image,omitempty"`
	Script  *WorkshopProbeScript  `json:"script,omitempty"`
	CliHelp *WorkshopProbeCliHelp `json:"cliHelp,omitempty"`
	// +kubebuilder:default=120
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:validation:Maximum=600
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

type WorkshopProbeScript struct {
	BaseImage string `json:"baseImage"`
	Script    string `json:"script"`
}

type WorkshopProbeCliHelp struct {
	Image  string   `json:"image"`
	Binary string   `json:"binary"`
	Args   []string `json:"args,omitempty"`
}

type WorkshopProbedTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// InputSchema is JSON text (not json.RawMessage: a CRD status field
	// serializes cleanly as a plain string).
	InputSchema string `json:"inputSchema,omitempty"`
}

type WorkshopProbeStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Tools []WorkshopProbedTool `json:"tools,omitempty"`
	// +optional
	HelpText string `json:"helpText,omitempty"`
	// +optional
	PodFailure string `json:"podFailure,omitempty"`
	// +optional
	ResolvedDigest string `json:"resolvedDigest,omitempty"`
	// +optional
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
type WorkshopProbeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkshopProbe `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WorkshopProbe{}, &WorkshopProbeList{})
}
