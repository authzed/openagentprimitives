package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:categories={authzed,spicebox},shortName=skl
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Valid",type="string",JSONPath=".status.conditions[?(@.type=='Valid')].status"
// +kubebuilder:printcolumn:name="Pinned",type="string",JSONPath=".status.conditions[?(@.type=='Pinned')].status"
// +kubebuilder:printcolumn:name="Canonical",type="string",JSONPath=".spec.canonicalName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
//
// Skill is one agentskills.io skill: a SKILL.md (frontmatter + body) plus
// optional bundled files. It is the addressable unit an AgentClass opts into.
// A Skill may be hand-authored (empty Source) or materialized by a SkillSource.
//
// Namespaced. Reconciled by pkg/controllers/skill, which only keeps the Valid
// and Pinned conditions honest -- it never pulls or stages content. Identity
// is spec.canonicalName (pkg/tools/skills/canonical), not metadata.name, and a
// namespaced Skill shadows a ClusterSkill sharing that canonical name.
type Skill struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SkillSpec   `json:"spec,omitempty"`
	Status SkillStatus `json:"status,omitempty"`
}

// SkillSpec returns a pointer to the embedded spec. It lets the shared skill
// validity reconciler (pkg/controllers/internal/skillspec) drive Skill and
// ClusterSkill, which carry the same SkillSpec, through one interface.
func (s *Skill) SkillSpec() *SkillSpec { return &s.Spec }

// SkillStatus returns a pointer to the embedded status. See SkillSpec.
func (s *Skill) SkillStatus() *SkillStatus { return &s.Status }

// SkillSpec is the desired state of a Skill.
type SkillSpec struct {
	// CanonicalName is the stable, collision-free identity, e.g.
	// "github.com/someorg/somerepo//skills/skillone@v1.2.0". All references
	// (AgentClass, settings ceilings) use this string.
	CanonicalName string `json:"canonicalName"`

	// DisplayName is an optional human label; defaults to the frontmatter name.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Description is the frontmatter description — the always-on metadata that
	// progressive disclosure injects into the agent prompt (~100 tokens).
	Description string `json:"description"`

	// Body is the SKILL.md markdown body, loaded on demand by the load_skill
	// tool. Stored inline, well within the etcd object budget.
	Body string `json:"body"`

	// Frontmatter carries the remaining SKILL.md frontmatter fields.
	// +optional
	Frontmatter SkillFrontmatter `json:"frontmatter,omitempty"`

	// Source records git provenance, written by the SkillSource controller.
	// Empty for hand-authored skills.
	// +optional
	Source *SkillProvenance `json:"source,omitempty"`

	// Bundle references the cached scripts/assets archive; nil means the skill
	// is instruction-only and nothing is staged into the sandbox for it.
	// +optional
	Bundle *SkillBundleRef `json:"bundle,omitempty"`

	// RepoInstructions is the repo-root agent-instructions file (AGENTS.md or
	// CLAUDE.md) discovered in the skill's source repo, injected into the agent's
	// system prompt when this skill is linked to an AgentClass. nil for
	// hand-authored skills (no repo) or when the source set
	// disableRepoInstructions.
	// +optional
	RepoInstructions *SkillRepoInstructions `json:"repoInstructions,omitempty"`
}

// SkillFrontmatter holds the optional agentskills.io frontmatter fields.
type SkillFrontmatter struct {
	// Name is the frontmatter "name" (kebab-case); must match the canonical
	// skill directory basename.
	Name string `json:"name"`
	// License is the skill author's declared license identifier.
	// +optional
	License string `json:"license,omitempty"`
	// Compatibility is the author's declared compatibility statement.
	// +optional
	Compatibility string `json:"compatibility,omitempty"`
	// Metadata is the author's arbitrary key/value frontmatter, carried
	// verbatim and interpreted by nothing here.
	// +optional
	Metadata map[string]string `json:"metadata,omitempty"`
	// AllowedTools is the experimental space-separated pre-approved tool list.
	// RECORDED, not enforced: nothing gates a tool call on it.
	// +optional
	AllowedTools string `json:"allowedTools,omitempty"`
}

// SkillProvenance records where a materialized skill came from.
type SkillProvenance struct {
	// RepoLocator is the normalized host/org/repo (no scheme, no .git).
	RepoLocator string `json:"repoLocator"`
	// Subpath is the skill's directory within the repo.
	Subpath string `json:"subpath"`
	// Ref is the requested ref (tag/branch/sha) as written in the source.
	// +optional
	Ref string `json:"ref,omitempty"`
	// ResolvedSHA is the commit the controller actually pulled.
	// +optional
	ResolvedSHA string `json:"resolvedSHA,omitempty"`
	// SourceName is the owning SkillSource CR name (empty for hand-authored).
	// +optional
	SourceName string `json:"sourceName,omitempty"`
}

// SkillBundleRef points at the cached scripts/assets archive.
type SkillBundleRef struct {
	// Digest is the content digest of the bundle archive.
	Digest string `json:"digest"`
	// CacheKey is the skillbundle.Store lookup key.
	CacheKey string `json:"cacheKey"`
}

// SkillRepoInstructions is the repo-root agent-instructions file discovered
// alongside a skill's SKILL.md and injected into the agent prompt for any
// AgentClass that links the skill.
type SkillRepoInstructions struct {
	// SourceFile is the repo-root filename the content came from
	// ("AGENTS.md" or "CLAUDE.md"), shown as provenance in the prompt.
	SourceFile string `json:"sourceFile"`
	// Content is the (possibly truncated) file contents.
	Content string `json:"content"`
	// Truncated is true when Content was capped at the 64 KiB injection limit.
	// +optional
	Truncated bool `json:"truncated,omitempty"`
}

// SkillStatus is the observed state of a Skill.
type SkillStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Pin is the recorded pin identity in the common PinRecord shape. Its
	// Strength ("frozen" | "named" | "unpinned") is the single source of the
	// skill's syntactic pin strength. nil when the canonical name did not
	// parse -- the Valid and Pinned conditions carry the reason.
	// +optional
	Pin *PinRecord `json:"pin,omitempty"`

	// Conditions carries Valid and Pinned.
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
type SkillList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Skill `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Skill{}, &SkillList{})
}
