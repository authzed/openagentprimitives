package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories={authzed,spicebox},shortName=clsagset
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Consistent",type="string",JSONPath=".status.conditions[?(@.type=='SelfConsistent')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
// +genclient:nonNamespaced
//
// ClusterAgentSettings is the cluster-wide top tier of agent governance
// settings: the limits and defaults every namespace inherits, plus the model
// catalog. Singleton -- the only permitted name is "cluster"
// (ClusterAgentSettingsName).
//
// Cluster-scoped. Reconciled by pkg/controllers/settings, which reports
// SelfConsistent. Tier resolution lives in pkg/platform/settings; limits set
// here are ceilings that lower tiers may narrow but never widen.
type ClusterAgentSettings struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SettingsSpec   `json:"spec,omitempty"`
	Status SettingsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ClusterAgentSettingsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterAgentSettings `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterAgentSettings{}, &ClusterAgentSettingsList{})
}
