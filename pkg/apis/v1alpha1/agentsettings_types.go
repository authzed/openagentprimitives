package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,spicebox},shortName=agset
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Consistent",type="string",JSONPath=".status.conditions[?(@.type=='SelfConsistent')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
//
// AgentSettings is the namespace tier of agent governance settings: limits
// that narrow what agents in this namespace may do, and defaults that fill in
// fields a lower tier omits. Per-namespace singleton -- the only permitted
// name is "default" (AgentSettingsName).
//
// Namespaced. Reconciled by pkg/controllers/settings, which reports
// SelfConsistent. Tier resolution itself lives in pkg/platform/settings: a
// limit here can only narrow the ClusterAgentSettings ceiling, never widen it,
// and the resolved snapshot is stamped onto AgentClass/AgentSession status.
type AgentSettings struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SettingsSpec   `json:"spec,omitempty"`
	Status SettingsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentSettingsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentSettings `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentSettings{}, &AgentSettingsList{})
}
