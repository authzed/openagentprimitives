package oap

import (
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// SkillClone describes an external git repository a bundled SkillSource clones
// on install to materialize a Skill. It is surfaced BEFORE install so the
// operator consents to the fetch: installing a bundle that carries SkillSources
// reaches out to the network on the operator's behalf.
type SkillClone struct {
	SkillSource string `json:"skillSource"` // the SkillSource CR name
	RepoURL     string `json:"repoURL"`
	Ref         string `json:"ref,omitempty"`     // branch/tag/sha; empty = the repo's default branch
	Subpath     string `json:"subpath,omitempty"` // SKILL.md discovery subtree; empty = whole repo
}

// SkillClones returns the external repositories this bundle's SkillSource CRs
// will clone on install, sorted by SkillSource name for stable output. Empty
// when the bundle carries no SkillSource. Callers (the CLI install command, the
// admind install endpoint) surface these so the operator sees what the install
// will fetch.
func (b *Bundle) SkillClones() ([]SkillClone, error) {
	crs, err := b.CRs()
	if err != nil {
		return nil, err
	}
	var out []SkillClone
	for _, cr := range crs {
		if cr.GetKind() != "SkillSource" {
			continue
		}
		repoURL, _, _ := unstructured.NestedString(cr.Object, "spec", "repoURL")
		ref, _, _ := unstructured.NestedString(cr.Object, "spec", "ref")
		subpath, _, _ := unstructured.NestedString(cr.Object, "spec", "subpath")
		out = append(out, SkillClone{
			SkillSource: cr.GetName(),
			RepoURL:     repoURL,
			Ref:         ref,
			Subpath:     subpath,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SkillSource < out[j].SkillSource })
	return out, nil
}
