package steelthread_test

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/materialize"
)

const (
	reviewSkillName = "github.com/demo-org/demo-skills//plugins/review/skills/review-pr@master"
	auditSkillName  = "github.com/other-org/audit-skills//skills/audit@v1.0.0"
)

// withSkills gives a live fixture the shape a skills-using class has: two
// skills from two DIFFERENT repos, so the per-skill owner-ref cannot be
// satisfied by emitting one source and pointing everything at it.
func withSkills(f *steelthread.FixtureInput) {
	f.Class.Spec.Skills = []spiceboxv1alpha1.AgentSkill{
		{Name: "review-pr", Ref: reviewSkillName, Target: spiceboxv1alpha1.SkillTargetSandbox},
		{Name: "audit", Ref: auditSkillName, Target: spiceboxv1alpha1.SkillTargetAgent},
	}
	f.SkillSources = []*spiceboxv1alpha1.SkillSource{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-skills", Namespace: "acme-prod"},
			Spec: spiceboxv1alpha1.SkillSourceSpec{
				RepoURL: "https://github.com/demo-org/demo-skills",
				Ref:     "master",
				Subpath: "plugins/review/skills/review-pr",
				Auth: &spiceboxv1alpha1.SkillSourceAuth{
					AgentIdentity: "demoforge-identity", Credential: "demoforge-bearer",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "audit-skills", Namespace: "acme-prod"},
			Spec: spiceboxv1alpha1.SkillSourceSpec{
				RepoURL: "https://github.com/other-org/audit-skills.git",
				Ref:     "v1.0.0",
			},
		},
	}
	f.Skills = []*spiceboxv1alpha1.Skill{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "review-pr-aaaaaaaa", Namespace: "acme-prod",
				Annotations:     map[string]string{"operator-note": "do not carry me forward"},
				OwnerReferences: []metav1.OwnerReference{ownerRef("SkillSource", "demo-skills")},
			},
			Spec: spiceboxv1alpha1.SkillSpec{
				CanonicalName: reviewSkillName,
				DisplayName:   "review-pr",
				Description:   "Review a pull request.",
				Body:          "# review-pr\n\nDo the review.\n",
				Frontmatter:   spiceboxv1alpha1.SkillFrontmatter{Name: "review-pr"},
				Source: &spiceboxv1alpha1.SkillProvenance{
					RepoLocator: "github.com/demo-org/demo-skills",
					Subpath:     "plugins/review/skills/review-pr",
					Ref:         "master",
					SourceName:  "demo-skills",
				},
				Bundle: &spiceboxv1alpha1.SkillBundleRef{Digest: "deadbeef", CacheKey: "deadbeef"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "audit-bbbbbbbb", Namespace: "acme-prod",
				OwnerReferences: []metav1.OwnerReference{ownerRef("SkillSource", "audit-skills")},
			},
			Spec: spiceboxv1alpha1.SkillSpec{
				CanonicalName: auditSkillName,
				Description:   "Audit the diff.",
				Body:          "# audit\n",
				Frontmatter:   spiceboxv1alpha1.SkillFrontmatter{Name: "audit"},
				RepoInstructions: &spiceboxv1alpha1.SkillRepoInstructions{
					SourceFile: "AGENTS.md", Content: "repo-wide instructions",
				},
			},
		},
	}
}

func ownerRef(kind, name string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: spiceboxv1alpha1.SchemeGroupVersion.String(),
		Kind:       kind,
		Name:       name,
		UID:        "live-uid-that-must-not-ride-through",
		Controller: ptr(true),
	}
}

func TestRewriteFixture_Skills(t *testing.T) {
	in := liveFixture(t, withSkills)
	rewritten, err := steelthread.RewriteFixture(in)
	require.NoError(t, err, "RewriteFixture")
	files := rewritten.Files

	t.Run("the Skill and its SkillSource are both emitted", func(t *testing.T) {
		var sk spiceboxv1alpha1.Skill
		findDoc(t, files, "Skill", "review-pr-aaaaaaaa", &sk)
		assert.Equal(t, reviewSkillName, sk.Spec.CanonicalName,
			"the class's skills[].ref resolves by canonical name, not by metadata.name")

		var src spiceboxv1alpha1.SkillSource
		findDoc(t, files, "SkillSource", "demo-skills", &src)
		assert.Equal(t, "https://github.com/demo-org/demo-skills", src.Spec.RepoURL,
			"the provenance gate compares the NORMALIZED repoURL to the canonical name's authority; "+
				"rewriting it would park the replayed Skill at Valid=False/InvalidSpec")
	})

	t.Run("the pair satisfies the REAL provenance gate", func(t *testing.T) {
		// Asserted through the production predicate rather than by re-listing
		// the fields it happens to read: this is the whole reason the fixture
		// emits a source at all, and a hand-rolled restatement here would keep
		// passing after the gate started asking a different question.
		for _, name := range []string{reviewSkillName, auditSkillName} {
			n, err := canonical.Parse(name)
			require.NoError(t, err)
			var sk spiceboxv1alpha1.Skill
			findDocByCanonicalName(t, files, name, &sk)

			ok, err := materialize.Skill(t.Context(), fixtureReader(t, files),
				sk.Namespace, sk.OwnerReferences, name)
			require.NoError(t, err)
			assert.True(t, ok, "skill %q is not materialized by its emitted SkillSource", n)
		}
	})

	t.Run("every SkillSource sorts before every Skill", func(t *testing.T) {
		// The Skill controller is driven by For(&Skill{}) with no watch on
		// SkillSource, so a Skill created first reconciles once against a
		// source that does not exist and stays Valid=False with nothing to
		// re-trigger it.
		var blob []byte
		for _, f := range files {
			blob = append(blob, f.YAML...)
		}
		// Anchored at column 0: "kind: SkillSource" also appears INDENTED, as
		// each Skill's own owner-ref, and matching that would compare a Skill
		// against itself.
		lastSource := bytes.LastIndex(blob, []byte("\nkind: SkillSource"))
		firstSkill := bytes.Index(blob, []byte("canonicalName:"))
		require.NotEqual(t, -1, lastSource)
		require.NotEqual(t, -1, firstSkill)
		assert.Less(t, lastSource, firstSkill, "a Skill applied before its SkillSource never becomes Valid")
	})

	t.Run("they sort before the AgentClass that opts into them", func(t *testing.T) {
		assert.Less(t, applyOrderOfKind(t, files, "Skill"), applyOrderOfKind(t, files, "AgentClass"),
			"the class parks at AgentClassSkillMissing when applied before the Skill it names")
	})

	t.Run("the owner-ref is REBUILT, never copied", func(t *testing.T) {
		var sk spiceboxv1alpha1.Skill
		findDoc(t, files, "Skill", "review-pr-aaaaaaaa", &sk)
		require.Len(t, sk.OwnerReferences, 1)
		assert.Equal(t, "demo-skills", sk.OwnerReferences[0].Name)
		require.NotNil(t, sk.OwnerReferences[0].Controller)
		assert.True(t, *sk.OwnerReferences[0].Controller,
			"the gate requires Controller:true; a plain owner-ref is not the unforgeable signal")
		assert.NotEqual(t, "live-uid-that-must-not-ride-through", string(sk.OwnerReferences[0].UID),
			"a live uid is a fact about the cluster the capture read, and it addresses nothing here")
		assert.Empty(t, sk.Annotations,
			"metadata is rebuilt from the fixture allowlist; whatever an operator annotated a live "+
				"Skill with must not ride into a repo")
	})

	t.Run("spec.bundle is dropped and reported", func(t *testing.T) {
		var sk spiceboxv1alpha1.Skill
		findDoc(t, files, "Skill", "review-pr-aaaaaaaa", &sk)
		assert.Nil(t, sk.Spec.Bundle,
			"the archive lives in the operator's store keyed by digest; a ref the fixture cannot "+
				"back would make the replay depend on a lookup that must fail")
		assert.Equal(t, []string{reviewSkillName}, rewritten.SkillsWithoutBundle,
			"the function that dropped it is the function that says whose it was")
	})

	t.Run("spec.auth is dropped from the SkillSource", func(t *testing.T) {
		var src spiceboxv1alpha1.SkillSource
		findDoc(t, files, "SkillSource", "demo-skills", &src)
		assert.Nil(t, src.Spec.Auth,
			"nothing clones at replay, so the credential reference is inert — and a live credential "+
				"reference is what every other rewrite here exists to keep out of a repo")
		assert.Equal(t, "master", src.Spec.Ref, "dropping auth must not disturb the rest of the spec")
	})

	t.Run("the CONTENT rides through verbatim", func(t *testing.T) {
		var sk spiceboxv1alpha1.Skill
		findDoc(t, files, "Skill", "audit-bbbbbbbb", &sk)
		assert.Equal(t, "Audit the diff.", sk.Spec.Description)
		assert.Equal(t, "# audit\n", sk.Spec.Body)
		assert.Equal(t, "audit", sk.Spec.Frontmatter.Name,
			"the staging path FAILS the session when the frontmatter name and the AgentSkill name disagree")
		require.NotNil(t, sk.Spec.RepoInstructions)
		assert.Equal(t, "repo-wide instructions", sk.Spec.RepoInstructions.Content,
			"for a target=agent skill this lands in the composed system prompt; substituting it "+
				"would replay a different prompt than the one recorded")
	})

	t.Run("EmittedSkills names exactly what was written", func(t *testing.T) {
		assert.Equal(t, []string{reviewSkillName, auditSkillName}, rewritten.EmittedSkills,
			"sorted and deduplicated, so a re-capture of one session is byte-identical — "+
				"and NOT in the order the caller gathered them, which a List does not fix")
	})
}

// TestRewriteFixture_SkillsDoesNotMutateTheCallersSkills is the counterpart to
// the sandbox version: Capture hands one FixtureInput to the rewrite AND to the
// self-check, so a mutation would make them disagree about what ran.
func TestRewriteFixture_SkillsDoesNotMutateTheCallersSkills(t *testing.T) {
	in := liveFixture(t, withSkills)
	_, err := steelthread.RewriteFixture(in)
	require.NoError(t, err)

	assert.NotNil(t, in.Skills[0].Spec.Bundle, "the caller's Skill lost its bundle ref")
	assert.NotNil(t, in.SkillSources[0].Spec.Auth, "the caller's SkillSource lost its auth")
	require.Len(t, in.Skills[0].OwnerReferences, 1)
	assert.Equal(t, "live-uid-that-must-not-ride-through", string(in.Skills[0].OwnerReferences[0].UID),
		"the caller's owner-ref was rewritten in place")
}

// TestRewriteFixture_ASkillWithNoOwnerGetsNoOwnerRef covers the hand-authored
// local// skill: it needs no SkillSource and its provenance gate passes without
// one, so inventing an owner-ref would point at an object nothing emits.
func TestRewriteFixture_ASkillWithNoOwnerGetsNoOwnerRef(t *testing.T) {
	in := liveFixture(t, func(f *steelthread.FixtureInput) {
		f.Class.Spec.Skills = []spiceboxv1alpha1.AgentSkill{{Name: "local-one", Ref: "local//local-one"}}
		f.Skills = []*spiceboxv1alpha1.Skill{{
			ObjectMeta: metav1.ObjectMeta{Name: "local-one-cccccccc", Namespace: "acme-prod"},
			Spec: spiceboxv1alpha1.SkillSpec{
				CanonicalName: "local//local-one",
				Description:   "A hand-authored skill.",
				Body:          "# local-one\n",
				Frontmatter:   spiceboxv1alpha1.SkillFrontmatter{Name: "local-one"},
			},
		}}
	})
	rewritten, err := steelthread.RewriteFixture(in)
	require.NoError(t, err)

	var sk spiceboxv1alpha1.Skill
	findDoc(t, rewritten.Files, "Skill", "local-one-cccccccc", &sk)
	assert.Empty(t, sk.OwnerReferences)
	assert.Equal(t, []string{"local//local-one"}, rewritten.EmittedSkills)
	assert.Empty(t, rewritten.SkillsWithoutBundle)
}

// TestRewriteFixture_SkillsRefusesAnUnusableCR pins the two shapes that would
// emit a file the replay cannot act on, rather than letting either through to
// surface minutes later as a class parked on a ref that resolves to nothing.
func TestRewriteFixture_SkillsRefusesAnUnusableCR(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*steelthread.FixtureInput)
		wantErr string
	}{
		{
			name: "a Skill with no canonicalName: the class's ref could never resolve",
			mutate: func(f *steelthread.FixtureInput) {
				withSkills(f)
				f.Skills[0].Spec.CanonicalName = ""
			},
			wantErr: "no spec.canonicalName",
		},
		{
			name: "a SkillSource with no repoURL: no owned Skill's gate could match",
			mutate: func(f *steelthread.FixtureInput) {
				withSkills(f)
				f.SkillSources[0].Spec.RepoURL = ""
			},
			wantErr: "no spec.repoURL",
		},
		{
			name: "a nameless Skill: nothing to address it by",
			mutate: func(f *steelthread.FixtureInput) {
				withSkills(f)
				f.Skills[0].Name = ""
			},
			wantErr: "no metadata.name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := steelthread.RewriteFixture(liveFixture(t, tc.mutate))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// findDocByCanonicalName locates an emitted Skill by the identity the class
// references it under, which is NOT its metadata.name.
func findDocByCanonicalName(t *testing.T, files []steelthread.FixtureFile, canonicalName string, out *spiceboxv1alpha1.Skill) {
	t.Helper()
	for _, f := range files {
		for _, doc := range splitYAMLDocs(f.YAML) {
			if !bytes.Contains(doc, []byte("kind: Skill\n")) ||
				!bytes.Contains(doc, []byte(canonicalName)) {
				continue
			}
			var sk spiceboxv1alpha1.Skill
			if err := yamlUnmarshalSkill(doc, &sk); err != nil || sk.Spec.CanonicalName != canonicalName {
				continue
			}
			*out = sk
			return
		}
	}
	t.Fatalf("no Skill with canonicalName %q among %d emitted files", canonicalName, len(files))
}

func TestRewriteFixture_ADerivedInteractPermissionIsCarriedOntoTheSpec(t *testing.T) {
	const derived = "slack_channel:C0DEMO123#member"

	cases := []struct {
		name     string
		declared string
		derived  string
		want     string
	}{
		{
			name:    "derived only: carried, because the rewrite destroys the channel it came from",
			derived: derived,
			want:    derived,
		},
		{
			name:     "declared wins: EffectiveSessionInteractPermission prefers it, and so must this",
			declared: "group:reviewers#member",
			derived:  derived,
			want:     "group:reviewers#member",
		},
		{
			name: "neither: nothing invented — a class with a human input needs none",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := liveFixture(t, func(f *steelthread.FixtureInput) {
				if tc.declared != "" {
					f.Class.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
						Session: &spiceboxv1alpha1.SessionAuthz{InteractPermission: tc.declared},
					}
				}
				f.Class.Status.DerivedSessionInteractPermission = tc.derived
			})
			rewritten, err := steelthread.RewriteFixture(in)
			require.NoError(t, err)

			var class spiceboxv1alpha1.AgentClass
			findDoc(t, rewritten.Files, "AgentClass", in.Class.Name, &class)
			assert.Equal(t, tc.want, class.Spec.GetAuthz().GetSession().InteractPermission)
		})
	}
}

// TestRewriteFixture_NoSkillsEmitsNoSkillFile is the negative control every
// capture taken before skills existed depends on.
func TestRewriteFixture_NoSkillsEmitsNoSkillFile(t *testing.T) {
	rewritten, err := steelthread.RewriteFixture(liveFixture(t, nil))
	require.NoError(t, err)
	for _, f := range rewritten.Files {
		assert.False(t, strings.Contains(f.Name, "skill"), "unexpected skill file %q", f.Name)
		assert.False(t, bytes.Contains(f.YAML, []byte("kind: Skill")), "unexpected Skill doc in %q", f.Name)
	}
	assert.Empty(t, rewritten.EmittedSkills)
	assert.Empty(t, rewritten.SkillsWithoutBundle)
}

// fixtureReader turns the emitted manifests into a client.Reader the real
// materialize.Skill gate can be run against, so the test proves the FIXTURE
// satisfies the gate rather than proving this package's idea of it.
func fixtureReader(t *testing.T, files []steelthread.FixtureFile) *skillSourceReader {
	t.Helper()
	r := &skillSourceReader{sources: map[string]*spiceboxv1alpha1.SkillSource{}}
	for _, f := range files {
		for _, doc := range splitYAMLDocs(f.YAML) {
			if !bytes.Contains(doc, []byte("kind: SkillSource")) {
				continue
			}
			var src spiceboxv1alpha1.SkillSource
			require.NoError(t, yamlUnmarshalSource(doc, &src))
			r.sources[src.Namespace+"/"+src.Name] = &src
		}
	}
	require.NotEmpty(t, r.sources, "no SkillSource was emitted, so this reader would prove nothing")
	return r
}

func TestClassSkillRefsStillNamesEveryRef(t *testing.T) {
	in := liveFixture(t, withSkills)
	assert.Equal(t, []string{reviewSkillName, auditSkillName},
		steelthread.ClassSkillRefsForTest(in.Class),
		"in spec order, so the refusal reads the way the class does")
	assert.True(t, slices.Contains(steelthread.ClassSkillRefsForTest(in.Class), auditSkillName))
}

// TestCapture_SkillsFindingComesFromWhatWasEMITTED spans the join the two
// halves cannot see on their own, and it exists because a mutation survived
// without it.
//
// checkSkills is tested against a hand-built SelfCheckInput, and rewriteSkills
// against a hand-built FixtureInput; neither notices when the WIRING between
// them re-derives CapturedSkills from the class instead of taking it from the
// rewrite. That mutation makes the check compare the class to itself, so it can
// never fire — every unit test still passed, and every skills capture would
// have emitted a bundle that parks at the readiness barrier.
//
// Driven through Capture, which is the only place both halves meet.
func TestCapture_SkillsFindingComesFromWhatWasEMITTED(t *testing.T) {
	cases := []struct {
		name string
		// gather models the CLI having found the Skill CRs behind the class's
		// refs (true) or having found none (false).
		gather            bool
		wantRefusal       bool
		wantBundleWarning bool
	}{
		{
			name:        "the class opts in and nothing was gathered: refused",
			wantRefusal: true,
			// No Skill was emitted, so no bundle ref was dropped: the warning
			// must not fire alongside the refusal and send a reader after a
			// missing archive when the whole Skill is missing.
		},
		{
			name:              "the class opts in and the CRs were gathered: emits, with the archive warning",
			gather:            true,
			wantBundleWarning: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := captureInput(t)
			in.Fixture = liveFixture(t, withSkills)
			require.NotNil(t, in.Fixture.Skills[0].Spec.Bundle,
				"the bundle-warning half of this test needs a gathered skill that ships an archive")
			if !tc.gather {
				in.Fixture.Skills, in.Fixture.SkillSources = nil, nil
			}

			_, findings, err := steelthread.Capture(syntheticRecords(t), in)
			require.NoError(t, err)

			var refused, warned bool
			for _, f := range findings {
				switch f.Code {
				case steelthread.CodeSkillsNotCaptured:
					refused = true
					assert.Equal(t, steelthread.SeverityHard, f.Severity)
				case steelthread.CodeSkillBundleNotStaged:
					warned = true
					assert.Equal(t, steelthread.SeverityWarn, f.Severity)
					assert.Contains(t, f.Message, reviewSkillName,
						"the warning has to name the skill whose scripts the replay will not stage")
				}
			}
			assert.Equal(t, tc.wantRefusal, refused)
			assert.Equal(t, tc.wantBundleWarning, warned)
		})
	}
}

// skillSourceReader is the narrowest client.Reader materialize.Skill needs: it
// answers Get for the SkillSources the fixture emitted and nothing else, so a
// gate that started reading some OTHER object fails loudly here instead of
// quietly passing against a permissive fake.
type skillSourceReader struct {
	sources map[string]*spiceboxv1alpha1.SkillSource
}

func (r *skillSourceReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	src, ok := r.sources[key.Namespace+"/"+key.Name]
	if !ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "skillsources"}, key.Name)
	}
	out, ok := obj.(*spiceboxv1alpha1.SkillSource)
	if !ok {
		return fmt.Errorf("skillSourceReader: asked for %T, which this fixture reader does not hold", obj)
	}
	src.DeepCopyInto(out)
	return nil
}

func (r *skillSourceReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return fmt.Errorf("skillSourceReader: List is not implemented; the provenance gate must not need one")
}

func yamlUnmarshalSkill(doc []byte, out *spiceboxv1alpha1.Skill) error {
	return yaml.Unmarshal(doc, out)
}

func yamlUnmarshalSource(doc []byte, out *spiceboxv1alpha1.SkillSource) error {
	return yaml.Unmarshal(doc, out)
}
