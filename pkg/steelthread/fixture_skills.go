package steelthread

import (
	"fmt"
	"slices"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// fixtureSkillOwnerUID is the metadata.ownerReferences[].uid every emitted
// Skill carries.
//
// The field is REQUIRED by the API server and a static fixture cannot know the
// uid the replay's own SkillSource will be assigned — the object does not exist
// until the harness creates it. A placeholder is safe here for exactly one
// reason, and it is a property of envtest rather than of this package: envtest
// runs no garbage collector, so nothing ever resolves an owner-ref to an object
// and nothing acts on the mismatch. The provenance gate this owner-ref exists
// to satisfy (pkg/tools/skills/materialize) reads Kind, APIVersion, Controller
// and Name — never the uid — and then Gets the named SkillSource for real.
//
// blockOwnerDeletion is deliberately NOT set: it engages the API server's
// ownerReferencesPermissionEnforcement admission plugin where that is enabled,
// which would make the fixture's admissibility depend on a cluster setting that
// has nothing to do with what the bundle is testing.
const fixtureSkillOwnerUID = "00000000-0000-0000-0000-000000000000"

// rewriteSkills emits the Skill and SkillSource CRs a replayed AgentClass needs
// in order to go Valid=True at all.
//
// # Why both objects, and why the owner-ref
//
// A Skill alone is not enough, and a Skill whose owner-ref dangles is WORSE
// than a missing one. The AgentClass reconciler parks at
// Valid=False/AgentClassSkillMissing until every spec.skills[].ref resolves to
// a Skill (or ClusterSkill) that is itself Valid=True, and the Skill
// reconciler will only set Valid=True when validate.CheckProvenance passes.
// For a git-authority canonical name that check demands an UNFORGEABLE signal
// (pkg/tools/skills/materialize): a controller owner-ref of Kind SkillSource,
// to a same-namespace SkillSource that EXISTS, whose normalized spec.repoURL
// equals the canonical name's authority. A bare Skill would go
// Valid=False/InvalidSpec instead — a different park, at the same barrier.
//
// So the fixture emits the source beside the skill and owner-refs across, which
// makes the emitted pair satisfy the real gate rather than sidestep it. The
// replay does not stamp these Valid: test/e2e/harness.go registers the actual
// Skill controller, which is a pure spec-to-status reconcile with no external
// I/O, so the gate genuinely runs against these manifests.
//
// # Ordering inside the file
//
// Every SkillSource doc is written BEFORE every Skill doc, and that is
// load-bearing rather than tidy. The harness applies a multi-doc file in
// document order, and the Skill controller has no watch on SkillSource — it is
// driven by `For(&Skill{})` alone, because in production the SkillSource
// controller CREATES the Skill and the source therefore always exists first. A
// Skill created before its source would reconcile once, find nothing, and stay
// Valid=False forever with nothing to re-trigger it.
//
// # spec.bundle is DROPPED
//
// The bundle ref names a content digest in the operator's skillbundle.Store —
// bytes that live in no CR and in no durable record this capture reads, so
// there is nothing to emit alongside it. Carried into a fixture the digest
// would dangle: the AgentSession reconciler would ask a store that has never
// heard of it, log a miss and stage the skill instruction-only anyway. Emitting
// the ref would therefore change nothing about the replay except to make it
// depend on a store lookup that must fail. The names of the skills this
// affected are returned so the bundle can carry a finding saying the scripts
// and assets those skills ship were not captured; see CodeSkillBundleNotStaged.
//
// # spec.auth is DROPPED from the SkillSource
//
// It names an AgentIdentity credential used to clone a private repo. Nothing
// clones at replay — the harness runs no SkillSource controller and the Skill's
// content is already inline in its own spec — so the reference is inert, and a
// live credential reference is precisely what every other rewrite here exists
// to keep out of a file that gets written into a repo.
//
// Everything else rides through verbatim, including spec.body,
// spec.description, spec.frontmatter, spec.source and spec.repoInstructions.
// Those are the skill's CONTENT, and the capture's job is to reproduce what the
// session ran against: for a target=agent skill the description and
// repoInstructions land in the composed system prompt, so substituting them
// would replay a different prompt than the one recorded.
//
// Returns the emitted docs, the canonical names emitted (so the self-check
// compares against what was actually written rather than re-deriving it), and
// the canonical names whose bundle ref was dropped.
func rewriteSkills(
	skills []*spiceboxv1alpha1.Skill, sources []*spiceboxv1alpha1.SkillSource,
) (docs []any, emitted []string, droppedBundles []string, err error) {
	sortedSources, err := sortedByName(sources,
		func(s *spiceboxv1alpha1.SkillSource) string { return s.Name }, "SkillSource")
	if err != nil {
		return nil, nil, nil, err
	}
	sortedSkills, err := sortedByName(skills,
		func(s *spiceboxv1alpha1.Skill) string { return s.Name }, "Skill")
	if err != nil {
		return nil, nil, nil, err
	}

	// Sources first — see the doc comment's ordering note.
	for _, src := range sortedSources {
		spec := src.Spec.DeepCopy()
		spec.Auth = nil // see the doc comment above
		if spec.RepoURL == "" {
			return nil, nil, nil, fmt.Errorf("steelthread: RewriteFixture: SkillSource %q has no spec.repoURL, "+
				"so no owned Skill's provenance gate could match its canonical name's authority", src.Name)
		}
		docs = append(docs, &spiceboxv1alpha1.SkillSource{
			TypeMeta:   typeMeta("SkillSource"),
			ObjectMeta: fixtureMeta(src.Name),
			Spec:       *spec,
		})
	}

	for _, sk := range sortedSkills {
		spec := sk.Spec.DeepCopy()
		if spec.CanonicalName == "" {
			return nil, nil, nil, fmt.Errorf("steelthread: RewriteFixture: Skill %q has no spec.canonicalName, "+
				"so the AgentClass's skills[].ref cannot resolve to an emitted CR", sk.Name)
		}
		if spec.Bundle != nil {
			droppedBundles = append(droppedBundles, spec.CanonicalName)
			spec.Bundle = nil // see the doc comment above
		}
		meta := fixtureMeta(sk.Name)
		// The ONE widening of fixtureMeta's name+namespace allowlist, and it is
		// not incidental metadata: this owner-ref IS the provenance signal the
		// Skill's own validity gate reads. It is rebuilt from the live ref's
		// Name alone, never copied, so nothing else a live object accumulated
		// rides through with it.
		if ref := controllerSkillSourceRef(sk); ref != "" {
			meta.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeGroupVersion.String(),
				Kind:       "SkillSource",
				Name:       ref,
				UID:        fixtureSkillOwnerUID,
				Controller: ptr.To(true),
			}}
		}
		docs = append(docs, &spiceboxv1alpha1.Skill{
			TypeMeta:   typeMeta("Skill"),
			ObjectMeta: meta,
			Spec:       *spec,
		})
		emitted = append(emitted, spec.CanonicalName)
	}

	slices.Sort(emitted)
	emitted = slices.Compact(emitted)
	sort.Strings(droppedBundles)
	return docs, emitted, slices.Compact(droppedBundles), nil
}

// controllerSkillSourceRef is the NAME of the SkillSource that controls this
// Skill, or "" when it has no such owner — a hand-authored local// skill, which
// needs none and whose provenance gate passes without one.
//
// Matched exactly as pkg/tools/skills/materialize matches it (Kind, APIVersion,
// Controller), so a ref this function reports is a ref that gate will find.
func controllerSkillSourceRef(sk *spiceboxv1alpha1.Skill) string {
	for _, ref := range sk.OwnerReferences {
		if ref.Kind == "SkillSource" &&
			ref.APIVersion == spiceboxv1alpha1.SchemeGroupVersion.String() &&
			ptr.Deref(ref.Controller, false) {
			return ref.Name
		}
	}
	return ""
}
