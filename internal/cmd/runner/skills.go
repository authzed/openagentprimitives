package main

import spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

// skillData holds the description + body + bundle ref for one resolved skill,
// extracted from either a namespace Skill or a cluster-scoped ClusterSkill.
type skillData struct {
	Description      string
	Body             string
	Bundle           *spiceboxv1alpha1.SkillBundleRef
	RepoInstructions *spiceboxv1alpha1.SkillRepoInstructions
	SourceRepo       string // sk.Spec.Source.RepoLocator, "" if no provenance
}

// mergeSkillSources builds a single canonical-name → skillData map from the
// namespace and cluster sources. Namespace Skills take precedence: if both a
// namespace Skill and a ClusterSkill share a canonical name, the namespace
// entry wins and the ClusterSkill is silently discarded.
//
// This is a pure function (no I/O) so it is straightforwardly unit-testable
// and is separated from the k8s List calls in resolveSkills.
func mergeSkillSources(nsSkills []spiceboxv1alpha1.Skill, clusterSkills []spiceboxv1alpha1.ClusterSkill) map[string]skillData {
	merged := make(map[string]skillData, len(nsSkills)+len(clusterSkills))

	// Namespace Skills first — these win on any conflict.
	for i := range nsSkills {
		sk := &nsSkills[i]
		merged[sk.Spec.CanonicalName] = skillData{
			Description:      sk.Spec.Description,
			Body:             sk.Spec.Body,
			Bundle:           sk.Spec.Bundle,
			RepoInstructions: sk.Spec.RepoInstructions,
			SourceRepo:       repoLocatorOf(sk.Spec.Source),
		}
	}

	// ClusterSkills fill in canonical names not already covered by the namespace.
	for i := range clusterSkills {
		csk := &clusterSkills[i]
		if _, present := merged[csk.Spec.CanonicalName]; !present {
			merged[csk.Spec.CanonicalName] = skillData{
				Description:      csk.Spec.Description,
				Body:             csk.Spec.Body,
				Bundle:           csk.Spec.Bundle,
				RepoInstructions: csk.Spec.RepoInstructions,
				SourceRepo:       repoLocatorOf(csk.Spec.Source),
			}
		}
	}

	return merged
}

// repoLocatorOf returns the provenance repo locator, or "" when absent
// (hand-authored skills carry no Source).
func repoLocatorOf(src *spiceboxv1alpha1.SkillProvenance) string {
	if src == nil {
		return ""
	}
	return src.RepoLocator
}
