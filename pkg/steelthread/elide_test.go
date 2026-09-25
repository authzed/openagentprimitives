package steelthread_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/materialize"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/validate"
)

// The authority withSkills' SANDBOX-targeted skill comes from, and the stand-in
// this file elides it onto. The agent-targeted one is on a different authority
// (auditSkillName) on purpose: eliding it must be refused, and a fixture where
// both skills shared one repo could not state both halves.
//
// Each stand-in is the SAME BYTE LENGTH as the authority it replaces, because
// that is now the rule the mechanism enforces and a fixture that broke it would
// only ever exercise the refusal. TestSkillElisionFixturesAreSameLength asserts
// the pairing here rather than leaving it to a comment nobody re-counts.
const (
	elidableAuthority = "github.com/demo-org/demo-skills"   // 31 bytes
	standInAuthority  = "github.com/exampleorg/demoskill"   // 31 bytes
	auditAuthority    = "github.com/other-org/audit-skills" // 33 bytes
	auditStandIn      = "github.com/exampleorg/auditskills" // 33 bytes

	// collisionAuthority is the second repo the collision cases elide. Spelled
	// to the SAME BYTE LENGTH as elidableAuthority, because a stand-in must
	// match its own original's length and so two authorities can share one
	// stand-in only when they are themselves the same length — which is now the
	// precondition of the collision the guards exist to catch.
	collisionAuthority = "github.com/other-org/auditskill" // 31 bytes
)

// TestSkillElisionFixturesAreSameLength pins the constants above against the
// rule they exist to exercise.
//
// Without it, someone editing a stand-in to read better turns every test in this
// file into a test of the length refusal — which they would all still pass,
// because they assert on errors as readily as on successes.
func TestSkillElisionFixturesAreSameLength(t *testing.T) {
	assert.Len(t, standInAuthority, len(elidableAuthority),
		"the stand-in must be the same byte length as the authority it replaces")
	assert.Len(t, auditStandIn, len(auditAuthority),
		"the stand-in must be the same byte length as the authority it replaces")
	assert.Len(t, collisionAuthority, len(elidableAuthority),
		"the collision cases need two originals of one length, or they exercise the length refusal instead")
}

// elideRule is the rule under test, spelled once.
func elideRule(t *testing.T) steelthread.SkillElision {
	t.Helper()
	e, err := steelthread.ParseSkillElision(elidableAuthority + "=" + standInAuthority)
	require.NoError(t, err, "the test's own elision must parse")
	return e
}

// sandboxOnly narrows withSkills to the one skill an elision is allowed to
// touch, for the cases that are about the REWRITE rather than about the gate.
// The agent-targeted skill and its source go entirely, class link included, so
// nothing here passes by accident on a fixture the gate would have refused.
func sandboxOnly(f *steelthread.FixtureInput) {
	withSkills(f)
	f.Class.Spec.Skills = f.Class.Spec.Skills[:1]
	f.Skills = f.Skills[:1]
	f.SkillSources = f.SkillSources[:1]
}

func TestParseSkillElision(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr string
		wantOld string
		wantNew string
	}{
		{
			name:    "two canonical locators of one length: parsed",
			in:      "github.com/someorg/somerepo=github.com/exampleorg/repos",
			wantOld: "github.com/someorg/somerepo",
			wantNew: "github.com/exampleorg/repos",
		},
		{
			name:    "a gitlab subgroup locator: parsed, subgroups are ordinary path segments",
			in:      "gitlab.com/org/sub/repo=gitlab.com/exorg/repo42",
			wantOld: "gitlab.com/org/sub/repo",
			wantNew: "gitlab.com/exorg/repo42",
		},
		{
			name:    "a replacement containing an '=': parsed, split is on the FIRST separator",
			in:      "github.com/someorg/somerepo=github.com/exampleorg/re=os",
			wantOld: "github.com/someorg/somerepo",
			wantNew: "github.com/exampleorg/re=os",
		},
		{
			// The generated form. The separator is a promise that a replacement
			// follows; no separator at all is the deliberate request for a
			// stand-in, and ResolveSkillElisions is what fills it in.
			name:    "no separator: the generated form, New left empty for resolution",
			in:      "github.com/someorg/somerepo",
			wantOld: "github.com/someorg/somerepo",
			wantNew: "",
		},
		{
			// The whole point of this change: a shorter stand-in desynchronises
			// every recorded value derived from a byte count, and the operator
			// is one edit away from a rule that works, so the error says how far.
			name:    "a shorter replacement: refused, naming the byte count required",
			in:      "github.com/someorg/somerepo=github.com/exorg/repo",
			wantErr: "Give a replacement of exactly 27 byte(s)",
		},
		{
			name:    "a longer replacement: refused, the rule is symmetric",
			in:      "github.com/someorg/somerepo=github.com/exampleorg/example-repository",
			wantErr: "Give a replacement of exactly 27 byte(s)",
		},
		{
			// Named "skill elision", not "redaction": one message, two flags,
			// and an operator must read about the flag they typed.
			name:    "a length refusal names the flag it came from",
			in:      "github.com/someorg/somerepo=github.com/exorg/repo",
			wantErr: "the skill elision replacing with",
		},
		{
			name:    "empty original: refused",
			in:      "=github.com/exampleorg/examplerepo",
			wantErr: "empty original authority",
		},
		{
			name:    "empty replacement: refused",
			in:      "github.com/someorg/somerepo=",
			wantErr: "empty replacement authority",
		},
		{
			// The comparison the rewrite makes is against canonical.Normalize's
			// output, so a URL-shaped rule would match nothing and leave the
			// name it was written to remove in the emitted manifests.
			name:    "a scheme on the original: refused as non-canonical",
			in:      "https://github.com/someorg/somerepo=github.com/exampleorg/examplerepo",
			wantErr: "non-canonical original authority",
		},
		{
			name:    "a .git suffix on the replacement: refused as non-canonical",
			in:      "github.com/someorg/somerepo=github.com/exampleorg/examplerepo.git",
			wantErr: "non-canonical replacement authority",
		},
		{
			name:    "a trailing slash: refused as non-canonical",
			in:      "github.com/someorg/somerepo/=github.com/exampleorg/examplerepo",
			wantErr: "non-canonical original authority",
		},
		{
			// A '//' would make the rule name a whole canonical skill rather
			// than an authority, and the rewrite moves authorities.
			name:    "a repo/subpath separator in the original: refused",
			in:      "github.com/someorg/somerepo//skills/x=github.com/exampleorg/examplerepo",
			wantErr: "does not parse as one",
		},
		{
			name:    "the reserved local authority as the original: refused",
			in:      "local=github.com/exampleorg/examplerepo",
			wantErr: "reserved local authority",
		},
		{
			name:    "the reserved local authority as the replacement: refused",
			in:      "github.com/someorg/somerepo=local/team-a",
			wantErr: "reserved local authority",
		},
		{
			name:    "an authority replaced with itself: refused",
			in:      "github.com/someorg/somerepo=github.com/someorg/somerepo",
			wantErr: "replaces an authority with itself",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := steelthread.ParseSkillElision(tc.in)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantOld, got.Old)
			assert.Equal(t, tc.wantNew, got.New)
		})
	}
}

// TestElideSkillSources_RefusesASkillTheAgentCouldSee is the safety gate, and it
// is the whole reason this mechanism is not a Redaction.
//
// A sandbox-targeted skill is staged to disk and excluded from both the system
// prompt and load_skill, so the recorded run depended on its EXISTENCE alone and
// emptying it changes nothing the model saw. An agent-targeted one's description
// is IN the composed prompt; emptying it replays a different prompt, and nothing
// downstream re-derives the prompt to notice — the divergence would be silent.
//
// The empty case is the one a human forgets: an AgentSkill read before the API
// server applies +kubebuilder:default=agent carries "", and reading that as
// "unset, so probably harmless" is exactly how a prompt-visible skill gets
// elided six months from now.
func TestElideSkillSources_RefusesASkillTheAgentCouldSee(t *testing.T) {
	cases := []struct {
		name   string
		target spiceboxv1alpha1.SkillTarget
		want   bool
	}{
		{name: "target sandbox: elided", target: spiceboxv1alpha1.SkillTargetSandbox, want: true},
		{name: "target agent: refused", target: spiceboxv1alpha1.SkillTargetAgent},
		{name: "target both: refused, the agent half still reaches the prompt", target: spiceboxv1alpha1.SkillTargetBoth},
		{name: "target unset: refused, the CRD default is agent", target: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := liveFixture(t, sandboxOnly)
			in.Class.Spec.Skills[0].Target = tc.target

			out, records, err := steelthread.ElideSkillSourcesForTest(in,
				[]steelthread.SkillElision{elideRule(t)})
			if !tc.want {
				require.Error(t, err, "eliding a skill whose target is %q must be refused", tc.target)
				assert.Contains(t, err.Error(), "refusing to elide")
				assert.Contains(t, err.Error(), string(cmpTarget(tc.target)),
					"the refusal must name the target it read, including the defaulted one")
				assert.Contains(t, err.Error(), "system prompt")
				return
			}
			require.NoError(t, err)
			require.Len(t, records, 1)
			assert.Equal(t, standInAuthority, records[0].Authority)
			assert.NotEqual(t, elidableAuthority, canonicalAuthority(t, out.Skills[0].Spec.CanonicalName))
		})
	}
}

// cmpTarget is what the refusal message is expected to name: the target as READ,
// with an empty one resolved to the CRD default the way every consumer in the
// tree resolves it.
func cmpTarget(t spiceboxv1alpha1.SkillTarget) spiceboxv1alpha1.SkillTarget {
	if t == "" {
		return spiceboxv1alpha1.SkillTargetAgent
	}
	return t
}

func canonicalAuthority(t *testing.T, name string) string {
	t.Helper()
	n, err := canonical.Parse(name)
	require.NoError(t, err, "emitted canonical name %q must parse", name)
	return n.Authority
}

// TestElideSkillSources_KeepsEveryProvenanceReferenceConsistent is the
// structural half, and it is asserted through the PRODUCTION gate rather than by
// re-listing the fields that gate happens to read today.
//
// A blind find-and-replace over the emitted bytes gets the authority right and
// the object names wrong: the SkillSource's metadata.name, the Skill's own name,
// the controller owner-ref that binds them and spec.source.sourceName all carry
// the third party's name in a form no substring rule rewrites consistently.
// Desynchronise any one and materialize.Skill goes false, the Skill parks
// Valid=False, and the AgentClass parks at AgentClassSkillInvalid — a replay
// that never reaches step one.
func TestElideSkillSources_KeepsEveryProvenanceReferenceConsistent(t *testing.T) {
	in := liveFixture(t, sandboxOnly)
	out, _, err := steelthread.ElideSkillSourcesForTest(in, []steelthread.SkillElision{elideRule(t)})
	require.NoError(t, err)

	rewritten, err := steelthread.RewriteFixture(out)
	require.NoError(t, err, "RewriteFixture over the elided fixture")
	files := rewritten.Files

	require.Len(t, rewritten.EmittedSkills, 1)
	elided := rewritten.EmittedSkills[0]

	t.Run("the emitted pair satisfies the REAL provenance gate", func(t *testing.T) {
		var sk spiceboxv1alpha1.Skill
		findDocByCanonicalName(t, files, elided, &sk)
		ok, err := materialize.Skill(t.Context(), fixtureReader(t, files),
			sk.Namespace, sk.OwnerReferences, elided)
		require.NoError(t, err)
		assert.True(t, ok,
			"the elided skill is not materialized by its emitted SkillSource; the replayed Skill would park "+
				"Valid=False and take the AgentClass with it")
	})

	t.Run("the class ref and the emitted canonical name still agree", func(t *testing.T) {
		require.Len(t, out.Class.Spec.Skills, 1)
		assert.Equal(t, elided, out.Class.Spec.Skills[0].Ref,
			"the class resolves its skills by canonical name; a ref left on the old authority resolves to nothing")
		assert.Equal(t, standInAuthority, canonicalAuthority(t, out.Class.Spec.Skills[0].Ref))
	})

	t.Run("spec.source follows the rename", func(t *testing.T) {
		src := out.Skills[0].Spec.Source
		require.NotNil(t, src)
		assert.Equal(t, standInAuthority, src.RepoLocator)
		assert.Equal(t, out.SkillSources[0].Name, src.SourceName,
			"sourceName names the SkillSource CR, which the rewrite renamed")
		assert.Empty(t, src.ResolvedSHA,
			"a commit in the removed repo does not exist in the stand-in and points back at what was elided")
	})

	t.Run("the emitted Skill is still VALID against the production rule set", func(t *testing.T) {
		// validate.Skill runs for real at replay: the harness registers the
		// Skill controller, which refuses an empty body, an empty description,
		// and a frontmatter name that does not match the canonical subpath's
		// last segment. An elision that emptied the fields outright would emit
		// a Skill that never goes Valid.
		spec := out.Skills[0].Spec
		assert.Empty(t, validate.Skill(spec.CanonicalName, spec.Frontmatter.Name, spec.Description, spec.Body))
	})

	t.Run("no emitted file mentions the elided authority", func(t *testing.T) {
		for _, f := range files {
			assert.NotContains(t, string(f.YAML), elidableAuthority, "%s still carries the elided authority", f.Name)
			assert.NotContains(t, string(f.YAML), "demo-org", "%s still carries the elided org", f.Name)
		}
	})
}

// TestElideSkillSources_DropsTheContent states what "elide" means, field by
// field, and pins the two fields that must NOT go with it.
func TestElideSkillSources_DropsTheContent(t *testing.T) {
	in := liveFixture(t, func(f *steelthread.FixtureInput) {
		sandboxOnly(f)
		sk := f.Skills[0]
		sk.Spec.Body = "# review-pr\n\nSecret third-party methodology.\n"
		sk.Spec.Description = "Third-party description."
		sk.Spec.RepoInstructions = &spiceboxv1alpha1.SkillRepoInstructions{
			SourceFile: "AGENTS.md", Content: "third-party repo instructions",
		}
		sk.Spec.Frontmatter = spiceboxv1alpha1.SkillFrontmatter{
			Name:          "review-pr",
			License:       "third-party-license",
			Compatibility: "third-party-compat",
			AllowedTools:  "Read Write",
			Metadata:      map[string]string{"vendor": "third-party-vendor"},
		}
	})
	before := in.Skills[0].Spec

	out, records, err := steelthread.ElideSkillSourcesForTest(in, []steelthread.SkillElision{elideRule(t)})
	require.NoError(t, err)
	spec := out.Skills[0].Spec

	t.Run("the content is gone", func(t *testing.T) {
		assert.NotContains(t, spec.Body, "methodology")
		assert.NotContains(t, spec.Description, "Third-party")
		assert.Nil(t, spec.RepoInstructions, "repo instructions reach the prompt for an agent-targeted skill")
		assert.Empty(t, spec.Frontmatter.License)
		assert.Empty(t, spec.Frontmatter.Compatibility)
		assert.Empty(t, spec.Frontmatter.AllowedTools)
		assert.Empty(t, spec.Frontmatter.Metadata)
	})

	t.Run("the identity fields stay", func(t *testing.T) {
		// frontmatter.name must equal the canonical name's last subpath
		// segment (validate.Skill) and the AgentClass link's own Name (the
		// sandbox staging path). It is a directory basename, not content.
		assert.Equal(t, "review-pr", spec.Frontmatter.Name)
		assert.Equal(t, before.DisplayName, spec.DisplayName)
	})

	t.Run("the placeholders satisfy the production validator", func(t *testing.T) {
		assert.NotEmpty(t, strings.TrimSpace(spec.Body))
		assert.Empty(t, validate.Skill(spec.CanonicalName, spec.Frontmatter.Name, spec.Description, spec.Body),
			"an elided skill must still go Valid=True; the replay runs validate.Skill for real")
	})

	t.Run("the record counts what went, and names only the stand-in", func(t *testing.T) {
		require.Len(t, records, 1)
		assert.Equal(t, standInAuthority, records[0].Authority)
		assert.Equal(t, 1, records[0].Sources)
		assert.Equal(t, 1, records[0].Skills)
		want := len(before.Body) + len(before.Description) + len(before.RepoInstructions.Content) +
			len(before.Frontmatter.License) + len(before.Frontmatter.Compatibility) +
			len(before.Frontmatter.AllowedTools) + len("vendor") + len("third-party-vendor")
		assert.Equal(t, want, records[0].ElidedBytes)
	})
}

// TestElideSkillSources_RefusesRatherThanDoingPartOfTheJob covers the three ways
// a rule can leave the fixture in a state nobody asked for.
func TestElideSkillSources_RefusesRatherThanDoingPartOfTheJob(t *testing.T) {
	t.Run("a rule that matched nothing", func(t *testing.T) {
		// Unlike a redaction — where a rule matching no prose is ordinary and
		// is recorded with a zero count — an elision names a structured object
		// the fixture either has or has not. The only way to reach zero is a
		// mistyped authority, and accepting it ships the name the operator
		// asked to remove.
		in := liveFixture(t, sandboxOnly)
		// Written in the GENERATED form, so the case stays about the typo
		// rather than about counting bytes: a mistyped authority has no
		// matching original to size a stand-in against, and demanding the
		// operator supply one of the typo's own length would be absurd.
		e, err := steelthread.ParseSkillElision("github.com/typo-org/typo-repo")
		require.NoError(t, err)

		_, _, err = steelthread.ElideSkillSourcesForTest(in, []steelthread.SkillElision{e})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "matched no SkillSource and no Skill")
		assert.Contains(t, err.Error(), elidableAuthority,
			"the refusal must offer the spellings the fixture carries, or a typo is a dead end")
	})

	t.Run("a gathered Skill no class link names", func(t *testing.T) {
		// Its target is unreadable, so it cannot be shown to be prompt-invisible.
		in := liveFixture(t, sandboxOnly)
		in.Class.Spec.Skills = nil

		_, _, err := steelthread.ElideSkillSourcesForTest(in, []steelthread.SkillElision{elideRule(t)})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no AgentClass")
		assert.Contains(t, err.Error(), "target")
	})

	t.Run("two repos sharing a subpath elided onto one stand-in", func(t *testing.T) {
		// Two DIFFERENT third-party repos that happen to ship the same skill
		// path collapse onto one canonical name once they share an authority.
		// Nothing later can see it: the rewrite is perfectly self-consistent,
		// so the harness applies the second manifest over the first and the
		// replay runs against a fixture silently missing a skill.
		const shared = "plugins/review/skills/review-pr"

		// Two guards, and which one fires depends on which pair collides. The
		// SkillSource guard catches two manifests the API server cannot both
		// hold; the Skill guard catches the same for skills, and because the
		// Skill's name is SafeSlug over its canonical name, it necessarily
		// fires before the canonical-name guard behind it. That last one is
		// kept as the invariant it states rather than as a reachable path: a
		// future change to how Skill names are derived would make it the only
		// thing standing between two merged skills and a silently short fixture.
		cases := []struct {
			name        string
			sameSubpath bool
			wantErr     string
		}{
			{
				name:        "the sources collide too",
				sameSubpath: true,
				wantErr:     "merges them",
			},
			{
				name:    "only the skills collide",
				wantErr: "two Skill manifests named",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				in := liveFixture(t, func(f *steelthread.FixtureInput) {
					withSkills(f)
					// The second repo's SKILL moves to the first's path and
					// ref, which is the only way one stand-in authority can
					// merge two canonical names.
					if tc.sameSubpath {
						f.SkillSources[1].Spec.Subpath = shared
						f.SkillSources[1].Spec.Ref = "master"
					}
					// And its AUTHORITY moves to one the same byte length as
					// elidableAuthority. Since the stand-in must match its
					// original's length, two authorities can only share a
					// stand-in when they are themselves the same length — so
					// that is now the precondition of the collision this guards.
					f.SkillSources[1].Spec.RepoURL = "https://" + collisionAuthority + ".git"
					f.Skills[1].Spec.CanonicalName = collisionAuthority + "//" + shared + "@master"
					f.Skills[1].Spec.Frontmatter.Name = "review-pr"
					f.Class.Spec.Skills[1].Ref = f.Skills[1].Spec.CanonicalName
					f.Class.Spec.Skills[1].Target = spiceboxv1alpha1.SkillTargetSandbox
				})
				second, err := steelthread.ParseSkillElision(collisionAuthority + "=" + standInAuthority)
				require.NoError(t, err)

				_, _, err = steelthread.ElideSkillSourcesForTest(in,
					[]steelthread.SkillElision{elideRule(t), second})
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			})
		}
	})

	t.Run("a fixture with no AgentClass", func(t *testing.T) {
		in := liveFixture(t, sandboxOnly)
		in.Class = nil

		_, _, err := steelthread.ElideSkillSourcesForTest(in, []steelthread.SkillElision{elideRule(t)})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "AgentClass")
	})
}

// TestElideSkillSources_LeavesTheCallersManifestsAlone. The capture reads these
// CRs off a live cluster and uses them elsewhere — the meta-tool prediction, the
// live-secret scan — so an in-place rewrite would change what those saw
// depending on call order, which is the least debuggable shape a bug can take.
func TestElideSkillSources_LeavesTheCallersManifestsAlone(t *testing.T) {
	in := liveFixture(t, sandboxOnly)
	wantCanonical := in.Skills[0].Spec.CanonicalName
	wantBody := in.Skills[0].Spec.Body
	wantSourceName := in.SkillSources[0].Name
	wantRef := in.Class.Spec.Skills[0].Ref

	_, _, err := steelthread.ElideSkillSourcesForTest(in, []steelthread.SkillElision{elideRule(t)})
	require.NoError(t, err)

	assert.Equal(t, wantCanonical, in.Skills[0].Spec.CanonicalName)
	assert.Equal(t, wantBody, in.Skills[0].Spec.Body)
	assert.Equal(t, wantSourceName, in.SkillSources[0].Name)
	assert.Equal(t, wantRef, in.Class.Spec.Skills[0].Ref)
}

// TestElideSkillSources_IsDeterministic. Two captures of one session must be
// byte-identical or the re-capture diff is noise nobody reads — the same
// property CaptureInput.Now exists to preserve. The derived object names are the
// only invented values here, so they are where it could break.
func TestElideSkillSources_IsDeterministic(t *testing.T) {
	var names []string
	for range 2 {
		out, _, err := steelthread.ElideSkillSourcesForTest(
			liveFixture(t, sandboxOnly), []steelthread.SkillElision{elideRule(t)})
		require.NoError(t, err)
		names = append(names, out.SkillSources[0].Name+" "+out.Skills[0].Name+" "+out.Skills[0].Spec.CanonicalName)
	}
	assert.Equal(t, names[0], names[1])

	t.Run("and the derived names are RFC1123-safe", func(t *testing.T) {
		out, _, err := steelthread.ElideSkillSourcesForTest(
			liveFixture(t, sandboxOnly), []steelthread.SkillElision{elideRule(t)})
		require.NoError(t, err)
		for _, n := range []string{out.SkillSources[0].Name, out.Skills[0].Name} {
			assert.Regexp(t, `^[a-z0-9][a-z0-9.-]*[a-z0-9]$`, n)
			assert.LessOrEqual(t, len(n), 253)
		}
	})
}

// TestElideSkillSources_TwoSourcesOnOneRepoDoNotCollide. The SkillSource name is
// derived rather than copied, so its uniqueness key has to be the triple that
// actually distinguishes two sources on one repo. Hashing the authority alone —
// the obvious thing, and what canonical.Name.SafeSlug would give — emits two
// manifests with one name.
func TestElideSkillSources_TwoSourcesOnOneRepoDoNotCollide(t *testing.T) {
	const secondSkill = elidableAuthority + "//plugins/audit/skills/audit@master"

	in := liveFixture(t, func(f *steelthread.FixtureInput) {
		sandboxOnly(f)
		src := f.SkillSources[0].DeepCopy()
		src.Name = "demo-skills-audit"
		src.Spec.Subpath = "plugins/audit/skills/audit"
		f.SkillSources = append(f.SkillSources, src)

		sk := f.Skills[0].DeepCopy()
		sk.Name = "audit-cccccccc"
		sk.Spec.CanonicalName = secondSkill
		sk.Spec.DisplayName = "audit"
		sk.Spec.Frontmatter = spiceboxv1alpha1.SkillFrontmatter{Name: "audit"}
		sk.OwnerReferences = []metav1.OwnerReference{ownerRef("SkillSource", "demo-skills-audit")}
		sk.Spec.Source.SourceName = "demo-skills-audit"
		sk.Spec.Source.Subpath = "plugins/audit/skills/audit"
		f.Skills = append(f.Skills, sk)

		f.Class.Spec.Skills = append(f.Class.Spec.Skills, spiceboxv1alpha1.AgentSkill{
			Name: "audit", Ref: secondSkill, Target: spiceboxv1alpha1.SkillTargetSandbox,
		})
	})

	out, records, err := steelthread.ElideSkillSourcesForTest(in, []steelthread.SkillElision{elideRule(t)})
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, 2, records[0].Sources)
	assert.Equal(t, 2, records[0].Skills)
	assert.NotEqual(t, out.SkillSources[0].Name, out.SkillSources[1].Name,
		"two SkillSources on one repo differ only by subpath and ref, which is what the derived name must hash")

	rewritten, err := steelthread.RewriteFixture(out)
	require.NoError(t, err)
	for _, name := range rewritten.EmittedSkills {
		var sk spiceboxv1alpha1.Skill
		findDocByCanonicalName(t, rewritten.Files, name, &sk)
		ok, err := materialize.Skill(t.Context(), fixtureReader(t, rewritten.Files),
			sk.Namespace, sk.OwnerReferences, name)
		require.NoError(t, err)
		assert.True(t, ok, "skill %q lost its provenance to the second source", name)
	}
}

// TestCapture_RecordsTheElisionAndNeverTheOriginal drives the mechanism through
// its real caller, which is the only place the record and the emitted bytes meet.
//
// The negative is the load-bearing half: a bundle recording "someorg became
// exampleorg" would carry the very name the elision existed to keep out of the
// repo, exactly as bt.Capture.Redactions documents for the textual half.
func TestCapture_RecordsTheElisionAndNeverTheOriginal(t *testing.T) {
	in := captureInput(t)
	in.Fixture = liveFixture(t, sandboxOnly)
	in.ElideSkills = []steelthread.SkillElision{elideRule(t)}

	res, findings, err := steelthread.Capture(syntheticRecords(t), in)
	require.NoError(t, err)

	require.NotNil(t, res.Bundle.Capture)
	require.Len(t, res.Bundle.Capture.SkillElisions, 1)
	assert.Equal(t, bt.SkillElision{
		Authority:   standInAuthority,
		Sources:     1,
		Skills:      1,
		ElidedBytes: res.Bundle.Capture.SkillElisions[0].ElidedBytes,
	}, res.Bundle.Capture.SkillElisions[0])
	assert.Positive(t, res.Bundle.Capture.SkillElisions[0].ElidedBytes)

	for _, f := range res.Emitted {
		assert.NotContains(t, string(f.Bytes), elidableAuthority,
			"%s still carries the elided authority", f.Name)
		assert.NotContains(t, string(f.Bytes), "demo-org", "%s still carries the elided org", f.Name)
	}

	f := findByCode(t, findings, steelthread.CodeSkillContentElided)
	assert.Equal(t, steelthread.SeverityWarn, f.Severity)
	assert.Contains(t, f.Message, standInAuthority)
	assert.NotContains(t, f.Message, elidableAuthority,
		"the finding is printed to a terminal and captured with it; it must not echo the original either")
}

// TestCapture_AnElisionRefusalStopsBeforeAnythingIsAssembled. The gate has to
// fire as an ERROR from Capture, not as a hard finding: a finding leaves a
// Result assembled from the un-elided manifests, and reportThenWrite's contract
// is that a hard finding writes nothing — which would be indistinguishable from
// this, until someone relaxed one of them.
func TestCapture_AnElisionRefusalStopsBeforeAnythingIsAssembled(t *testing.T) {
	in := captureInput(t)
	// withSkills' second skill is agent-targeted, on its own authority.
	in.Fixture = liveFixture(t, withSkills)
	e, err := steelthread.ParseSkillElision(auditAuthority + "=" + auditStandIn)
	require.NoError(t, err)
	in.ElideSkills = []steelthread.SkillElision{e}

	res, findings, err := steelthread.Capture(syntheticRecords(t), in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to elide")
	assert.Empty(t, res.Emitted, "nothing may be assembled from a fixture the gate refused")
	assert.Empty(t, findings)
}

// TestElideSkillSources_EveryAuthorityReferenceKeepsItsByteLength is the
// guarantee the length rule exists to buy, asserted where it has to hold rather
// than at the parser that enforces it.
//
// A rule accepted with equal-length halves is only useful if the REWRITE then
// preserves length everywhere it substitutes. Each field below carries the
// authority embedded in a longer string — a canonical name is authority +
// "//" + subpath + "@" + ref — so a length-preserving rule applied to a field
// that reassembles rather than substitutes would still move the total, and the
// bug this closes would be back with the refusal in place to hide it.
//
// The claim is per-FIELD and about bytes, not about the fixture's total size:
// the content elision changes the total deliberately (see elideSkillContent),
// and the SkillSource's metadata.name is a derived stand-in rather than a
// substitution. What must not move is any string a byte count could be taken
// over that contained the authority.
func TestElideSkillSources_EveryAuthorityReferenceKeepsItsByteLength(t *testing.T) {
	in := liveFixture(t, sandboxOnly)

	require.Len(t, in.Skills, 1)
	require.Len(t, in.SkillSources, 1)
	require.Len(t, in.Class.Spec.Skills, 1)
	before := map[string]string{
		"Skill.spec.canonicalName":      in.Skills[0].Spec.CanonicalName,
		"Skill.spec.source.repoLocator": in.Skills[0].Spec.Source.RepoLocator,
		"AgentClass.spec.skills[].ref":  in.Class.Spec.Skills[0].Ref,
		"SkillSource.spec.repoURL":      in.SkillSources[0].Spec.RepoURL,
		"Skill.metadata.name":           in.Skills[0].Name,
	}
	// Every one of them must actually carry the authority, or the assertions
	// below would pass over fields the rewrite never touches.
	for field, v := range before {
		if field == "Skill.metadata.name" {
			continue // a SafeSlug over the canonical name, not a copy of it
		}
		require.Contains(t, v, elidableAuthority,
			"%s does not carry the authority, so it cannot witness the length rule", field)
	}

	out, _, err := steelthread.ElideSkillSourcesForTest(in, []steelthread.SkillElision{elideRule(t)})
	require.NoError(t, err)

	after := map[string]string{
		"Skill.spec.canonicalName":      out.Skills[0].Spec.CanonicalName,
		"Skill.spec.source.repoLocator": out.Skills[0].Spec.Source.RepoLocator,
		"AgentClass.spec.skills[].ref":  out.Class.Spec.Skills[0].Ref,
		"SkillSource.spec.repoURL":      out.SkillSources[0].Spec.RepoURL,
		"Skill.metadata.name":           out.Skills[0].Name,
	}
	for field, was := range before {
		got := after[field]
		assert.Len(t, got, len(was),
			"%s changed byte length (%q -> %q); any recorded value derived from a count over it "+
				"now replays differently than it recorded", field, was, got)
		assert.NotContains(t, got, elidableAuthority,
			"%s still carries the elided authority", field)
	}
}

// TestSkillElision_LengthRefusalFiresExactlyOnALengthChange pins BOTH halves of
// "exactly": every entry point refuses a length change, and none of them refuses
// a rule whose halves agree.
//
// The second half is the one worth having. A refusal that fired on every rule
// would pass any test that only ever fed it a bad one, and the whole mechanism
// would be dead while every assertion about it stayed green.
func TestSkillElision_LengthRefusalFiresExactlyOnALengthChange(t *testing.T) {
	const shorter = "github.com/exorg/demoskill" // 26 bytes, against elidableAuthority's 31

	require.NotEqual(t, len(elidableAuthority), len(shorter), "the test's own short rule must be short")

	t.Run("ParseSkillElision refuses, naming the count required", func(t *testing.T) {
		_, err := steelthread.ParseSkillElision(elidableAuthority + "=" + shorter)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Give a replacement of exactly 31 byte(s)")
	})

	t.Run("ResolveSkillElisions refuses a rule built in code, skipping the parser", func(t *testing.T) {
		// Every test in this package builds SkillElision values directly, and
		// so could any programmatic caller. A rule that never met the parser
		// must still meet the rule.
		_, err := steelthread.ResolveSkillElisions([]steelthread.SkillElision{
			{Old: elidableAuthority, New: shorter},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Give a replacement of exactly 31 byte(s)")
	})

	t.Run("the rewrite itself refuses, so no caller can reach it unchecked", func(t *testing.T) {
		in := liveFixture(t, sandboxOnly)
		_, _, err := steelthread.ElideSkillSourcesForTest(in, []steelthread.SkillElision{
			{Old: elidableAuthority, New: shorter},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Give a replacement of exactly 31 byte(s)")
	})

	t.Run("an equal-length rule is NOT refused, at any entry point", func(t *testing.T) {
		require.Len(t, standInAuthority, len(elidableAuthority))

		_, err := steelthread.ParseSkillElision(elidableAuthority + "=" + standInAuthority)
		require.NoError(t, err, "ParseSkillElision")

		resolved, err := steelthread.ResolveSkillElisions([]steelthread.SkillElision{
			{Old: elidableAuthority, New: standInAuthority},
		})
		require.NoError(t, err, "ResolveSkillElisions")
		assert.Equal(t, standInAuthority, resolved[0].New, "a supplied stand-in is not replaced")

		in := liveFixture(t, sandboxOnly)
		_, records, err := steelthread.ElideSkillSourcesForTest(in,
			[]steelthread.SkillElision{{Old: elidableAuthority, New: standInAuthority}})
		require.NoError(t, err, "elideSkillSources")
		require.Len(t, records, 1)
		assert.Equal(t, standInAuthority, records[0].Authority)
	})
}

// TestResolveSkillElisions_GeneratesASameLengthStandIn covers the escape hatch
// the length rule needs in order to be livable, and the four properties a
// generated authority has to hold at once.
//
// Reuses --redact's engine rather than a second generator, so what is under test
// here is the LAYOUT: that an authority-shaped token comes out, admissible to
// the same validator an operator's typed rule meets, at every length.
func TestResolveSkillElisions_GeneratesASameLengthStandIn(t *testing.T) {
	// Lengths chosen to walk the layout's own transitions: comfortably longer
	// than the stem word, exactly its length, and shorter than it — where the
	// word must give way to digest characters rather than produce a token
	// ending mid-separator.
	cases := []struct {
		name string
		old  string
	}{
		{name: "a long authority: the whole stem word fits", old: "github.com/someorg/a-long-repository-name"},
		{name: "an ordinary authority", old: elidableAuthority},
		{name: "an authority the length of the stem word", old: "gitlab.com/o/rr"},
		{name: "an authority shorter than the stem word", old: "git.io/abc"},
		{name: "a very short authority", old: "a/b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := steelthread.ResolveSkillElisions([]steelthread.SkillElision{{Old: tc.old}})
			require.NoError(t, err)
			require.Len(t, got, 1)
			tok := got[0].New

			assert.Len(t, tok, len(tc.old),
				"a generated stand-in that changed length would reintroduce the bug the rule closes")
			assert.NotEqual(t, tc.old, tok, "a stand-in equal to the original rewrites nothing")

			// Admissible as an authority: asked of the parser rather than
			// eyeballed, because a token the parser would refuse is a token an
			// operator could not have typed and the fixture cannot carry.
			_, err = steelthread.ParseSkillElision(tc.old + "=" + tok)
			assert.NoError(t, err, "the generated stand-in %q is not one the parser accepts", tok)

			again, err := steelthread.ResolveSkillElisions([]steelthread.SkillElision{{Old: tc.old}})
			require.NoError(t, err)
			assert.Equal(t, tok, again[0].New,
				"generation must be deterministic, or every re-capture of one session is a diff nobody can read")
		})
	}

	t.Run("legible where the length allows", func(t *testing.T) {
		got, err := steelthread.ResolveSkillElisions([]steelthread.SkillElision{
			{Old: "github.com/someorg/a-long-repository-name"},
		})
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(got[0].New, "elided.example/"),
			"a reader of a committed fixture should see at a glance that the repo was removed, got %q", got[0].New)
	})

	t.Run("unique across the rule set, and never equal to another rule's original", func(t *testing.T) {
		// Two DIFFERENT authorities of one length. A generated token colliding
		// with the other rule's stand-in would merge two repos into one
		// canonical name; one colliding with the other rule's ORIGINAL would be
		// rewritten again by that rule, emitting an authority the bundle does
		// not record.
		require.Len(t, collisionAuthority, len(elidableAuthority))
		got, err := steelthread.ResolveSkillElisions([]steelthread.SkillElision{
			{Old: elidableAuthority},
			{Old: collisionAuthority},
		})
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.NotEqual(t, got[0].New, got[1].New, "two generated stand-ins collided")
		for _, r := range got {
			assert.NotContains(t, r.New, elidableAuthority)
			assert.NotContains(t, r.New, collisionAuthority)
		}
	})

	t.Run("a supplied stand-in is claimed before any is generated", func(t *testing.T) {
		// Order-independence: the generated rule sits FIRST, so a generator
		// that claimed as it went could take the token the second rule supplies.
		got, err := steelthread.ResolveSkillElisions([]steelthread.SkillElision{
			{Old: elidableAuthority},
			{Old: collisionAuthority, New: standInAuthority},
		})
		require.NoError(t, err)
		assert.Equal(t, standInAuthority, got[1].New)
		assert.NotEqual(t, standInAuthority, got[0].New,
			"a generated stand-in took a supplied one")
	})

	t.Run("idempotent, so resolving twice is a no-op", func(t *testing.T) {
		once, err := steelthread.ResolveSkillElisions([]steelthread.SkillElision{{Old: elidableAuthority}})
		require.NoError(t, err)
		twice, err := steelthread.ResolveSkillElisions(once)
		require.NoError(t, err)
		assert.Equal(t, once, twice,
			"the CLI resolves early and elideSkillSources resolves again; the second pass must change nothing")
	})

	t.Run("the layout's admissibility test is consulted, and widening recovers from a rejection", func(t *testing.T) {
		// Driven through a deliberately BAD stem word, because no word this
		// package ships can reach the branch: "elided.example/" truncates from
		// the right into a valid authority at every width. Without this, a
		// mutation deleting the accept hook survives every test — measured, not
		// assumed — leaving a guard that only matters after a future stem edit
		// as the one thing nothing checks.
		const badWord = "elided.example//" // truncates into a token carrying the repo/subpath separator
		const old = "github.com/someorg/a-long-repository-name"

		noSeparator := func(s string) bool { return !strings.Contains(s, "//") }

		got, err := steelthread.GenerateStandInWithWordForTest(old, badWord, noSeparator)
		require.NoError(t, err, "widening must find an admissible candidate, not give up")
		assert.Len(t, got, len(old), "recovering from a rejection must not cost the length guarantee")
		assert.NotContains(t, got, "//", "the generator emitted a candidate its own accept test rejects")

		// The control: with no accept test the same inputs DO produce the token
		// the predicate would have refused. Without this half, an accept hook
		// that was never consulted would still pass the assertion above.
		unchecked, err := steelthread.GenerateStandInWithWordForTest(old, badWord, nil)
		require.NoError(t, err)
		assert.Contains(t, unchecked, "//",
			"the bad stem no longer produces the rejectable token, so this case has stopped testing anything")
	})

	t.Run("admissible and same-length at EVERY length, not just the ones named above", func(t *testing.T) {
		// The layout truncates its stem word from the right as the digest
		// widens, so some lengths land mid-word — and one of them lands exactly
		// on the trailing "/" that would make the token an authority ending in a
		// separator. Sweeping the lengths is what makes the generator's accept
		// test a property rather than a defensive branch nobody exercises: edit
		// the stem word to something that truncates badly and this fails, while
		// a handful of hand-picked lengths would not.
		for n := 1; n <= 64; n++ {
			old := strings.Repeat("a", n)
			got, err := steelthread.ResolveSkillElisions([]steelthread.SkillElision{{Old: old}})
			require.NoError(t, err, "no stand-in generated for a %d-byte authority", n)
			tok := got[0].New
			assert.Len(t, tok, n, "a %d-byte authority got a %d-byte stand-in", n, len(tok))
			_, err = steelthread.ParseSkillElision(old + "=" + tok)
			assert.NoError(t, err, "the %d-byte stand-in %q is not an authority the parser accepts", n, tok)
		}
	})
}

// TestElideSkillSources_TheTargetGateSurvivesResolution is the guard on the
// change itself.
//
// elideSkillSources now resolves the rules BEFORE it reads the fixture, and a
// resolution step placed in front of a safety gate is exactly how a gate stops
// running. The gate refuses eliding a skill the agent could see — the fact the
// whole content elision rests on, since a target=sandbox skill's body and
// description reach neither the composed prompt nor load_skill.
func TestElideSkillSources_TheTargetGateSurvivesResolution(t *testing.T) {
	for _, target := range []spiceboxv1alpha1.SkillTarget{
		spiceboxv1alpha1.SkillTargetAgent,
		spiceboxv1alpha1.SkillTargetBoth,
		"",
	} {
		t.Run("target "+string(cmpTarget(target))+": refused through a GENERATED rule", func(t *testing.T) {
			in := liveFixture(t, sandboxOnly)
			in.Class.Spec.Skills[0].Target = target

			// Deliberately the generated form: it is the path that now runs
			// resolution first, and the one a gate could hide behind.
			_, _, err := steelthread.ElideSkillSourcesForTest(in,
				[]steelthread.SkillElision{{Old: elidableAuthority}})
			require.Error(t, err, "resolution must not consume the rule before the target gate reads it")
			assert.Contains(t, err.Error(), "refusing to elide")
			assert.Contains(t, err.Error(), "system prompt")
		})
	}
}
