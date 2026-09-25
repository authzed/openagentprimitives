// Package skillspec holds type-agnostic leaf helpers shared by the SkillSource
// and ClusterSkillSource controllers, which materialize Skill / ClusterSkill
// CRs that embed the SAME SkillSpec / SkillStatus. These helpers operate on
// SkillSpec, []metav1.OwnerReference, and the parsed skillmd.Frontmatter — never
// on the parent CR type — so a single copy serves both mirror controllers.
package skillspec

import (
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/skillmd"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// ToV1Frontmatter maps the parsed skillmd.Frontmatter onto the CRD's
// SkillFrontmatter. Note Description lives on SkillSpec, not the CRD frontmatter.
func ToV1Frontmatter(fm skillmd.Frontmatter) v1.SkillFrontmatter {
	return v1.SkillFrontmatter{
		Name:          fm.Name,
		License:       fm.License,
		Compatibility: fm.Compatibility,
		Metadata:      fm.Metadata,
		AllowedTools:  fm.AllowedTools,
	}
}

// Equal reports whether two SkillSpecs are equivalent for upsert purposes.
func Equal(a, b v1.SkillSpec) bool {
	if a.CanonicalName != b.CanonicalName ||
		a.DisplayName != b.DisplayName ||
		a.Description != b.Description ||
		a.Body != b.Body {
		return false
	}
	if !frontmatterEqual(a.Frontmatter, b.Frontmatter) {
		return false
	}
	if !provenanceEqual(a.Source, b.Source) {
		return false
	}
	if !repoInstructionsEqual(a.RepoInstructions, b.RepoInstructions) {
		return false
	}
	return bundleEqual(a.Bundle, b.Bundle)
}

func frontmatterEqual(a, b v1.SkillFrontmatter) bool {
	if a.Name != b.Name || a.License != b.License ||
		a.Compatibility != b.Compatibility || a.AllowedTools != b.AllowedTools {
		return false
	}
	if len(a.Metadata) != len(b.Metadata) {
		return false
	}
	for k, v := range a.Metadata {
		if b.Metadata[k] != v {
			return false
		}
	}
	return true
}

func provenanceEqual(a, b *v1.SkillProvenance) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func repoInstructionsEqual(a, b *v1.SkillRepoInstructions) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.SourceFile == b.SourceFile && a.Content == b.Content && a.Truncated == b.Truncated
}

func bundleEqual(a, b *v1.SkillBundleRef) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// OwnerRefPresent reports whether refs already contains an owner reference that
// matches want on every field the controllers care about: APIVersion, Kind,
// Name, UID, AND the Controller / BlockOwnerDeletion flags (nil compared as
// false).
//
// The flags are part of the comparison, not ignored, because this is the
// skip-the-write predicate: the mirror controllers call it to decide whether an
// existing Skill / ClusterSkill already carries the ownership they want. A ref
// that matches on identity but is NOT flagged as the controller is weaker than
// want, so it must report ABSENT — that is what drives AdoptOwnerRef to upgrade
// it. Reporting it present would skip the write and leave the weaker ref in
// place forever, so the child would never be garbage-collected with its parent.
func OwnerRefPresent(refs []metav1.OwnerReference, want metav1.OwnerReference) bool {
	for _, r := range refs {
		if r.APIVersion == want.APIVersion &&
			r.Kind == want.Kind &&
			r.Name == want.Name &&
			r.UID == want.UID &&
			ptr.Deref(r.Controller, false) == ptr.Deref(want.Controller, false) &&
			ptr.Deref(r.BlockOwnerDeletion, false) == ptr.Deref(want.BlockOwnerDeletion, false) {
			return true
		}
	}
	return false
}

// AdoptOwnerRef returns a new slice that contains want as a controller owner ref,
// replacing any pre-existing ref with the same Kind+Name (so we don't accumulate
// duplicates across runs).
func AdoptOwnerRef(refs []metav1.OwnerReference, want metav1.OwnerReference) []metav1.OwnerReference {
	out := make([]metav1.OwnerReference, 0, len(refs)+1)
	for _, r := range refs {
		if r.Kind == want.Kind && r.Name == want.Name {
			continue // drop stale ref; we'll add the canonical one below
		}
		out = append(out, r)
	}
	return append(out, want)
}
