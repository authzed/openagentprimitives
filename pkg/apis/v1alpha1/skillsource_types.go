package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:categories={authzed,spicebox},shortName=sksrc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Repo",type="string",JSONPath=".spec.repoURL"
// +kubebuilder:printcolumn:name="SHA",type="string",JSONPath=".status.resolvedSHA"
// +kubebuilder:printcolumn:name="Skills",type="integer",JSONPath=".status.discoveredSkills"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
//
// SkillSource is a git repo that yields one or more Skills.
//
// Namespaced. Reconciled by pkg/controllers/skillsource, which clones at
// spec.ref, discovers SKILL.md files under spec.subpath, caches each skill's
// bundle by content digest, and materializes owned Skill CRs. Fetch and
// discovery failures surface on the Ready condition and requeue on the sync
// interval, so a bad credential or ref degrades this source rather than
// crash-looping the operator.
type SkillSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SkillSourceSpec   `json:"spec,omitempty"`
	Status SkillSourceStatus `json:"status,omitempty"`
}

// SkillSourceSpec is the desired state of a SkillSource.
type SkillSourceSpec struct {
	// RepoURL is the git repo to clone (e.g. "https://github.com/org/repo").
	RepoURL string `json:"repoURL"`

	// Ref is the branch, tag, or commit SHA to fetch. Empty = default branch
	// (a rolling source — warned downstream).
	// +optional
	Ref string `json:"ref,omitempty"`

	// Subpath restricts SKILL.md discovery to this subtree. Empty = whole repo.
	// +optional
	Subpath string `json:"subpath,omitempty"`

	// Auth references the credential used to clone a private repo.
	// +optional
	Auth *SkillSourceAuth `json:"auth,omitempty"`

	// Sync controls re-polling cadence.
	// +optional
	Sync SkillSourceSync `json:"sync,omitempty"`

	// DisableRepoInstructions, when true, stops this source from discovering and
	// attaching the repo-root agent-instructions file (AGENTS.md/CLAUDE.md) to
	// the skills it materializes. Default (false) = enabled.
	// +optional
	DisableRepoInstructions bool `json:"disableRepoInstructions,omitempty"`
}

// SkillSourceAuth references a credential on an AgentIdentity for cloning.
type SkillSourceAuth struct {
	// AgentIdentity is the AgentIdentity CR (same namespace) holding the credential.
	AgentIdentity string `json:"agentIdentity"`
	// Credential is the AgentCredential name on that identity (e.g. a github_pat).
	Credential string `json:"credential"`
}

// SkillSourceSync controls the re-poll cadence.
type SkillSourceSync struct {
	// Interval between re-polls. Zero → controller default (1h).
	// +optional
	Interval metav1.Duration `json:"interval,omitempty"`
}

// SkillSourceStatus is the observed state.
type SkillSourceStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ResolvedSHA is the commit the last successful sync pulled.
	// +optional
	ResolvedSHA string `json:"resolvedSHA,omitempty"`
	// DiscoveredSkills is the number of Skills materialized on the last sync.
	// +optional
	DiscoveredSkills int32 `json:"discoveredSkills,omitempty"`
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
	// DiscoveryProblems lists SKILL.md files found but skipped on the last sync
	// (invalid frontmatter, name/dir mismatch, parse/bundle error) WITH the
	// reason, so an operator can see why a skill did not materialize without
	// reading operator logs. Capped (excess summarized).
	// +optional
	DiscoveryProblems []string `json:"discoveryProblems,omitempty"`
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
type SkillSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SkillSource `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SkillSource{}, &SkillSourceList{})
}
