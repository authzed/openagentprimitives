package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// wantSkills builds the AgentSkill entries resolveSkills expects for its
// "want" argument. The local Name is unused by resolveSkills (it matches on
// Ref only), so it's just set to the ref itself.
func wantSkills(refs ...string) []spiceboxv1alpha1.AgentSkill {
	out := make([]spiceboxv1alpha1.AgentSkill, len(refs))
	for i, ref := range refs {
		out[i] = spiceboxv1alpha1.AgentSkill{Name: ref, Ref: ref}
	}
	return out
}

func nsSkill(canonical, desc, body string) spiceboxv1alpha1.Skill {
	return spiceboxv1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-" + canonical, Namespace: "test-ns"},
		Spec: spiceboxv1alpha1.SkillSpec{
			CanonicalName: canonical,
			Description:   desc,
			Body:          body,
		},
	}
}

func clusterSkill(canonical, desc, body string) spiceboxv1alpha1.ClusterSkill {
	return spiceboxv1alpha1.ClusterSkill{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-" + canonical},
		Spec: spiceboxv1alpha1.SkillSpec{
			CanonicalName: canonical,
			Description:   desc,
			Body:          body,
		},
	}
}

func TestMergeSkillSources(t *testing.T) {
	const (
		canonA = "github.com/org/repo//skills/alpha@v1"
		canonB = "github.com/org/repo//skills/beta@v1"
		canonC = "github.com/org/repo//skills/gamma@v1"
	)

	cases := []struct {
		name     string
		nsSkills []spiceboxv1alpha1.Skill
		csSkills []spiceboxv1alpha1.ClusterSkill
		// wantKeys is the set of canonical names expected in the merged map.
		wantKeys []string
		// wantNSWins is the canonical name that must carry the namespace description
		// (used for the shadow-precedence sub-case).
		wantNSWins      string
		wantNSDesc      string
		wantClusterDesc string
	}{
		{
			name:     "namespace-only: only ns skills present",
			nsSkills: []spiceboxv1alpha1.Skill{nsSkill(canonA, "ns-desc-A", "ns-body-A")},
			csSkills: nil,
			wantKeys: []string{canonA},
		},
		{
			name:     "cluster-only: only cluster skills present",
			nsSkills: nil,
			csSkills: []spiceboxv1alpha1.ClusterSkill{clusterSkill(canonB, "cs-desc-B", "cs-body-B")},
			wantKeys: []string{canonB},
		},
		{
			name:     "both without overlap: all canonical names present",
			nsSkills: []spiceboxv1alpha1.Skill{nsSkill(canonA, "ns-desc-A", "ns-body-A")},
			csSkills: []spiceboxv1alpha1.ClusterSkill{clusterSkill(canonB, "cs-desc-B", "cs-body-B")},
			wantKeys: []string{canonA, canonB},
		},
		{
			name: "namespace shadows cluster: ns wins on same canonical name",
			nsSkills: []spiceboxv1alpha1.Skill{
				nsSkill(canonA, "ns-desc-A", "ns-body-A"),
				nsSkill(canonC, "ns-desc-C", "ns-body-C"),
			},
			csSkills: []spiceboxv1alpha1.ClusterSkill{
				// canonA exists in both — cluster must be shadowed.
				clusterSkill(canonA, "cs-desc-A-SHADOWED", "cs-body-A-SHADOWED"),
				clusterSkill(canonB, "cs-desc-B", "cs-body-B"),
			},
			// All three canonical names appear exactly once.
			wantKeys:        []string{canonA, canonB, canonC},
			wantNSWins:      canonA,
			wantNSDesc:      "ns-desc-A",
			wantClusterDesc: "cs-desc-A-SHADOWED",
		},
		{
			name:     "empty inputs: empty map",
			nsSkills: nil,
			csSkills: nil,
			wantKeys: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeSkillSources(tc.nsSkills, tc.csSkills)

			assert.Len(t, got, len(tc.wantKeys), "merged map key count")
			for _, k := range tc.wantKeys {
				_, ok := got[k]
				assert.True(t, ok, "canonical name %q should be in merged map", k)
			}

			// Shadow-precedence check: the namespace description wins.
			if tc.wantNSWins != "" {
				d, ok := got[tc.wantNSWins]
				if assert.True(t, ok, "shadowed canonical %q must be in merged map", tc.wantNSWins) {
					assert.Equal(t, tc.wantNSDesc, d.Description,
						"namespace skill description must win over cluster for %q", tc.wantNSWins)
					assert.NotEqual(t, tc.wantClusterDesc, d.Description,
						"cluster description must NOT appear for shadowed canonical %q", tc.wantNSWins)
				}
			}
		})
	}
}

// TestResolveSkillsFailClosed pins the fail-closed contract: an opted-in skill
// the runner cannot resolve (because the List was forbidden, or the Skill is
// simply absent) must abort the session with a diagnosable error — NOT be
// logged-and-skipped, which silently strips load_skill + the Agent Skills
// prompt section from a session the AgentClass gate already declared Valid.
// The forbidden-List case is the exact production bug: the per-session runner
// ServiceAccount lacked read access to skills/clusterskills.
func TestResolveSkillsFailClosed(t *testing.T) {
	const (
		canonA = "github.com/org/repo//skills/alpha@v1"
		canonB = "github.com/org/repo//skills/beta@v1"
	)
	scheme := refreshScheme(t) // shared helper (sidecar_refresh_test.go), package main
	nsA := nsSkill(canonA, "Alpha skill.", "ALPHA BODY")

	// newClient builds a fake client seeded with objs; when forbidExpand is set,
	// every List call returns a forbidden error (models the runner SA missing
	// skills/clusterskills RBAC).
	newClient := func(forbidList bool, objs ...client.Object) client.Client {
		b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...)
		if forbidList {
			b = b.WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					return errors.New(`skills.agentprimitives.authzed.com is forbidden: cannot list resource "skills"`)
				},
			})
		}
		return b.Build()
	}

	t.Run("all opted-in skills resolve: metadata + body returned, no error", func(t *testing.T) {
		meta, bodies, _, err := resolveSkills(context.Background(), newClient(false, &nsA), "test-ns", wantSkills(canonA))
		require.NoError(t, err)
		require.Len(t, meta, 1)
		assert.Equal(t, canonA, meta[0].CanonicalName)
		assert.Equal(t, "ALPHA BODY", bodies[canonA], "load_skill body must be carried for the resolved skill")
	})

	t.Run("no skills opted in: no error, nothing resolved", func(t *testing.T) {
		meta, bodies, _, err := resolveSkills(context.Background(), newClient(false), "test-ns", nil)
		require.NoError(t, err)
		assert.Empty(t, meta)
		assert.Empty(t, bodies)
	})

	t.Run("opted-in skill absent, lists clean: fail-closed error names the missing skill", func(t *testing.T) {
		_, _, _, err := resolveSkills(context.Background(), newClient(false, &nsA), "test-ns", wantSkills(canonA, canonB))
		require.Error(t, err, "an unresolved opted-in skill must fail the session, not be skipped")
		assert.Contains(t, err.Error(), canonB, "error must name the missing skill")
		assert.NotContains(t, err.Error(), canonA, "a skill that DID resolve must not be reported missing")
	})

	t.Run("List forbidden (runner SA missing RBAC): fail-closed error blames skills RBAC", func(t *testing.T) {
		_, _, _, err := resolveSkills(context.Background(), newClient(true, &nsA), "test-ns", wantSkills(canonA))
		require.Error(t, err, "the production bug: a forbidden List must fail the session, not silently skip the skill")
		assert.Contains(t, err.Error(), canonA, "error must name the unresolved skill")
		assert.Contains(t, err.Error(), "skills/clusterskills", "error must point at the runner SA RBAC as the likely cause")
	})
}

// wantSkillTargeted builds a single AgentSkill with an explicit Target, for
// tests that need to exercise resolveSkills' Target-based filtering (which
// wantSkills' all-implicit-agent-target entries never do).
func wantSkillTargeted(ref string, target spiceboxv1alpha1.SkillTarget) spiceboxv1alpha1.AgentSkill {
	return spiceboxv1alpha1.AgentSkill{Name: ref, Ref: ref, Target: target}
}

// TestResolveSkillsHonoursTarget pins the fix for the finding that
// resolveSkills read sk.Target zero times: a skill declared
// target:sandbox must be staged to disk by the AgentSession controller
// ONLY, never surfaced into the outer agent's prompt (skillMeta) or its
// load_skill body map (bodies) — the whole point of "sandbox" is to keep
// the skill's cost and content out of the agent's own context. Before the
// fix, every target value (including "sandbox") was appended identically.
func TestResolveSkillsHonoursTarget(t *testing.T) {
	const (
		canonAgent   = "github.com/org/repo//skills/agent-only@v1"
		canonSandbox = "github.com/org/repo//skills/sandbox-only@v1"
		canonBoth    = "github.com/org/repo//skills/both@v1"
	)
	scheme := refreshScheme(t)

	nsAgent := nsSkill(canonAgent, "Agent-targeted skill.", "AGENT BODY")
	nsSandbox := nsSkill(canonSandbox, "Sandbox-targeted skill.", "SANDBOX BODY")
	nsBoth := nsSkill(canonBoth, "Both-targeted skill.", "BOTH BODY")

	t.Run("sandbox target excluded from prompt metadata and load_skill bodies", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&nsAgent, &nsSandbox, &nsBoth).Build()
		want := []spiceboxv1alpha1.AgentSkill{
			wantSkillTargeted(canonAgent, spiceboxv1alpha1.SkillTargetAgent),
			wantSkillTargeted(canonSandbox, spiceboxv1alpha1.SkillTargetSandbox),
			wantSkillTargeted(canonBoth, spiceboxv1alpha1.SkillTargetBoth),
		}

		meta, bodies, _, err := resolveSkills(context.Background(), c, "test-ns", want)
		require.NoError(t, err)

		var gotNames []string
		for _, m := range meta {
			gotNames = append(gotNames, m.CanonicalName)
		}
		assert.ElementsMatch(t, []string{canonAgent, canonBoth}, gotNames,
			"sandbox-targeted skill must not appear in the agent's prompt metadata")

		assert.Contains(t, bodies, canonAgent, "agent-targeted skill must remain available via load_skill")
		assert.Contains(t, bodies, canonBoth, "both-targeted skill must remain available via load_skill")
		assert.NotContains(t, bodies, canonSandbox, "sandbox-targeted skill body must never reach the outer agent")
	})

	t.Run("sandbox target that fails to resolve does not fail the session", func(t *testing.T) {
		// canonSandbox is opted-in but deliberately NOT seeded into the client:
		// resolveAndStageSkillBundles (operator side) already handles a missing
		// sandbox-only skill non-fatally, so resolveSkills must not re-apply its
		// fail-closed check to a skill it never surfaces to the agent anyway.
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&nsAgent).Build()
		want := []spiceboxv1alpha1.AgentSkill{
			wantSkillTargeted(canonAgent, spiceboxv1alpha1.SkillTargetAgent),
			wantSkillTargeted(canonSandbox, spiceboxv1alpha1.SkillTargetSandbox),
		}

		meta, _, _, err := resolveSkills(context.Background(), c, "test-ns", want)
		require.NoError(t, err, "an unresolved sandbox-only skill must not fail the session")
		require.Len(t, meta, 1)
		assert.Equal(t, canonAgent, meta[0].CanonicalName)
	})
}

func TestResolveSkillsRepoInstructions(t *testing.T) {
	const (
		canonA = "github.com/org/repo//skills/alpha@v1"
		canonB = "github.com/org/repo//skills/beta@v1"
	)
	scheme := refreshScheme(t)
	mk := func(canonical string) *spiceboxv1alpha1.Skill {
		s := nsSkill(canonical, "d", "b")
		s.Spec.Source = &spiceboxv1alpha1.SkillProvenance{RepoLocator: "github.com/org/repo"}
		s.Spec.RepoInstructions = &spiceboxv1alpha1.SkillRepoInstructions{SourceFile: "AGENTS.md", Content: "REPO CONVENTIONS"}
		return &s
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mk(canonA), mk(canonB)).Build()

	_, _, instrs, err := resolveSkills(context.Background(), c, "test-ns", wantSkills(canonA, canonB))
	require.NoError(t, err)
	require.Len(t, instrs, 1, "two skills, same repo content → one deduped block")
	assert.Equal(t, "github.com/org/repo", instrs[0].SourceRepo)
	assert.Equal(t, "AGENTS.md", instrs[0].SourceFile)
	assert.Equal(t, "REPO CONVENTIONS", instrs[0].Content)
}

func TestResolveSkillsRepoInstructionsMultipleRepos(t *testing.T) {
	const (
		canonAAA = "github.com/org/aaa//skills/one@v1"
		canonZZZ = "github.com/org/zzz//skills/one@v1"
	)
	scheme := refreshScheme(t)

	skillAAA := nsSkill(canonAAA, "d", "b")
	skillAAA.Spec.Source = &spiceboxv1alpha1.SkillProvenance{RepoLocator: "github.com/org/aaa"}
	skillAAA.Spec.RepoInstructions = &spiceboxv1alpha1.SkillRepoInstructions{SourceFile: "AGENTS.md", Content: "AAA"}

	skillZZZ := nsSkill(canonZZZ, "d", "b")
	skillZZZ.Spec.Source = &spiceboxv1alpha1.SkillProvenance{RepoLocator: "github.com/org/zzz"}
	skillZZZ.Spec.RepoInstructions = &spiceboxv1alpha1.SkillRepoInstructions{SourceFile: "CLAUDE.md", Content: "ZZZ"}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&skillAAA, &skillZZZ).Build()

	_, _, instrs, err := resolveSkills(context.Background(), c, "test-ns", wantSkills(canonAAA, canonZZZ))
	require.NoError(t, err)
	require.Len(t, instrs, 2, "two skills from different repos with different content → two distinct blocks")

	// instrs are sorted by SourceRepo; "github.com/org/aaa" < "github.com/org/zzz".
	assert.Equal(t, "github.com/org/aaa", instrs[0].SourceRepo)
	assert.Equal(t, "AGENTS.md", instrs[0].SourceFile)
	assert.Equal(t, "AAA", instrs[0].Content)

	assert.Equal(t, "github.com/org/zzz", instrs[1].SourceRepo)
	assert.Equal(t, "CLAUDE.md", instrs[1].SourceFile)
	assert.Equal(t, "ZZZ", instrs[1].Content)
}
