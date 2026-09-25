package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories={authzed,spicebox},shortName=cskl
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Valid",type="string",JSONPath=".status.conditions[?(@.type=='Valid')].status"
// +kubebuilder:printcolumn:name="Canonical",type="string",JSONPath=".spec.canonicalName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +genclient
// +genclient:nonNamespaced
//
// ClusterSkill is a cluster-scoped Skill, visible to every namespace. A
// namespaced Skill with the same canonical name shadows it at resolution time.
//
// Cluster-scoped. Reconciled by pkg/controllers/clusterskill, which only keeps
// the Valid and Pinned conditions honest -- it never pulls or stages content,
// because a ClusterSkill is fully described by its spec. Discovery and
// materialization belong to the ClusterSkillSource controller.
type ClusterSkill struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SkillSpec   `json:"spec,omitempty"`
	Status            SkillStatus `json:"status,omitempty"`
}

// SkillSpec returns a pointer to the embedded spec, letting the shared skill
// validity reconciler (pkg/controllers/internal/skillspec) drive ClusterSkill
// and Skill — which carry the same SkillSpec — through one interface.
func (s *ClusterSkill) SkillSpec() *SkillSpec { return &s.Spec }

// SkillStatus returns a pointer to the embedded status. See SkillSpec.
func (s *ClusterSkill) SkillStatus() *SkillStatus { return &s.Status }

// +kubebuilder:object:root=true
type ClusterSkillList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterSkill `json:"items"`
}

func init() { SchemeBuilder.Register(&ClusterSkill{}, &ClusterSkillList{}) }
