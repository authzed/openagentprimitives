package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories={authzed,spicebox},shortName=csksrc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Repo",type="string",JSONPath=".spec.repoURL"
// +kubebuilder:printcolumn:name="SHA",type="string",JSONPath=".status.resolvedSHA"
// +kubebuilder:printcolumn:name="Skills",type="integer",JSONPath=".status.discoveredSkills"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +genclient
// +genclient:nonNamespaced
//
// ClusterSkillSource is a git repo that materializes cluster-scoped
// ClusterSkills. Auth is a direct Secret reference, because AgentIdentity is
// namespaced and so unavailable at cluster scope.
//
// Cluster-scoped. Reconciled by pkg/controllers/clusterskillsource, which
// clones at spec.ref, discovers SKILL.md files under spec.subpath, caches each
// bundle by content digest, and owns the ClusterSkills it creates. It is the
// cluster-scoped mirror of pkg/controllers/skillsource.
type ClusterSkillSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClusterSkillSourceSpec `json:"spec,omitempty"`
	Status            SkillSourceStatus      `json:"status,omitempty"`
}

type ClusterSkillSourceSpec struct {
	RepoURL string `json:"repoURL"`
	// +optional
	Ref string `json:"ref,omitempty"`
	// +optional
	Subpath string `json:"subpath,omitempty"`
	// Auth references a Secret holding the clone token (e.g. a github PAT).
	// +optional
	Auth *ClusterSkillSourceAuth `json:"auth,omitempty"`
	// +optional
	Sync SkillSourceSync `json:"sync,omitempty"`
	// DisableRepoInstructions, when true, stops this source from discovering and
	// attaching the repo-root agent-instructions file (AGENTS.md/CLAUDE.md) to
	// the skills it materializes. Default (false) = enabled.
	// +optional
	DisableRepoInstructions bool `json:"disableRepoInstructions,omitempty"`
}

type ClusterSkillSourceAuth struct {
	SecretRef SecretKeyRef `json:"secretRef"` // {Name, Key}
	// Namespace of the Secret (required — cluster scope has no implicit namespace).
	Namespace string `json:"namespace"`
}

// +kubebuilder:object:root=true
type ClusterSkillSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterSkillSource `json:"items"`
}

func init() { SchemeBuilder.Register(&ClusterSkillSource{}, &ClusterSkillSourceList{}) }
