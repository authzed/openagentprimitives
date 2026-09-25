// pkg/controllers/agentclass/stage_skills_validation_test.go
//
// Pure-function tests for validateSkillsSpec: no envtest, no cluster reader —
// the function only ever looks at the AgentClassSpec it's handed.
package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// classWithSkills builds a bare AgentClassSpec opting into the given skills
// and no toolBundles.
func classWithSkills(skills ...spiceboxv1alpha1.AgentSkill) spiceboxv1alpha1.AgentClassSpec {
	return spiceboxv1alpha1.AgentClassSpec{Skills: skills}
}

// classStaging builds an AgentClassSpec with the given skills and a single
// toolBundle ("demo-bundle") whose StageSkills is the given list.
func classStaging(skills []spiceboxv1alpha1.AgentSkill, stageSkills ...string) spiceboxv1alpha1.AgentClassSpec {
	return spiceboxv1alpha1.AgentClassSpec{
		Skills: skills,
		ToolBundles: []spiceboxv1alpha1.ToolBundle{
			{Name: "demo-bundle", Class: "demo-class", StageSkills: stageSkills},
		},
	}
}

func TestValidateSkillsSpec_RejectsTheThreeWays(t *testing.T) {
	cases := []struct {
		name    string
		spec    spiceboxv1alpha1.AgentClassSpec
		wantErr string
	}{
		{
			// wantErr is deliberately the whole rule-specific phrase, not just
			// the skill name: "code-review" alone also appears in the
			// well-formed cases' spec and (via %q) inside the rule-3 message
			// below, so a bare-name substring would pass even if this rule's
			// check were deleted and rule 3 coincidentally fired instead.
			name: "duplicate local name: rejected naming the name",
			spec: classWithSkills(
				spiceboxv1alpha1.AgentSkill{Name: "code-review", Ref: "demo-org/demo-repo//x@v1"},
				spiceboxv1alpha1.AgentSkill{Name: "code-review", Ref: "demo-org/demo-repo//y@v2"}),
			wantErr: `duplicate name "code-review"`,
		},
		{
			// wantErr names the rule-2 phrasing, not just "nosuch": the
			// zero-value AgentSkill a missing lookup would silently return
			// also defaults to Target agent, so a rule-3-shaped message
			// ("...names \"nosuch\", whose target is...") would ALSO contain
			// "nosuch" if rule 2's own not-found check were ever deleted —
			// this phrase is the one only rule 2 produces.
			name: "stageSkills names a skill that does not exist: rejected",
			spec: classStaging(
				[]spiceboxv1alpha1.AgentSkill{{Name: "code-review", Ref: "demo-org/demo-repo//x@v1", Target: spiceboxv1alpha1.SkillTargetSandbox}},
				"nosuch"),
			wantErr: `"nosuch", which is not in spec.skills`,
		},
		{
			name: "stageSkills names an agent-targeted skill: rejected naming both",
			spec: classStaging(
				[]spiceboxv1alpha1.AgentSkill{{Name: "code-review", Ref: "demo-org/demo-repo//x@v1", Target: spiceboxv1alpha1.SkillTargetAgent}},
				"code-review"),
			wantErr: `whose target is "agent"`,
		},
		{
			name: "stageSkills names a skill with the CRD-defaulted empty Target: rejected same as explicit agent",
			spec: classStaging(
				[]spiceboxv1alpha1.AgentSkill{{Name: "code-review", Ref: "demo-org/demo-repo//x@v1"}},
				"code-review"),
			wantErr: `whose target is "agent"`,
		},
		{
			name: "well-formed: accepted",
			spec: classStaging(
				[]spiceboxv1alpha1.AgentSkill{{Name: "code-review", Ref: "demo-org/demo-repo//x@v1", Target: spiceboxv1alpha1.SkillTargetSandbox}},
				"code-review"),
		},
		{
			name: "well-formed with both target: accepted",
			spec: classStaging(
				[]spiceboxv1alpha1.AgentSkill{{Name: "code-review", Ref: "demo-org/demo-repo//x@v1", Target: spiceboxv1alpha1.SkillTargetBoth}},
				"code-review"),
		},
		{
			name: "wildcard with no sandbox-targeted skill yet: accepted, not an error",
			spec: classStaging(
				[]spiceboxv1alpha1.AgentSkill{{Name: "code-review", Ref: "demo-org/demo-repo//x@v1", Target: spiceboxv1alpha1.SkillTargetAgent}},
				"*"),
		},
		{
			name: "wildcard with a sandbox-targeted skill: accepted",
			spec: classStaging(
				[]spiceboxv1alpha1.AgentSkill{{Name: "code-review", Ref: "demo-org/demo-repo//x@v1", Target: spiceboxv1alpha1.SkillTargetSandbox}},
				"*"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg := validateSkillsSpec(&tc.spec)
			if tc.wantErr == "" {
				require.Empty(t, reason, "unexpected reason=%q msg=%q", reason, msg)
				return
			}
			require.NotEmpty(t, reason, "expected a rejection but got none")
			assert.Contains(t, msg, tc.wantErr)
		})
	}
}

// TestValidateSkillsSpec_RejectsUnsafeCharacterSet is fix round 1's addition
// (Task 10): once BuildBundleSession started building the sandbox MountPath
// directly from AgentSkill.Name (ResolvedSkillBundle.LocalName), an
// unconstrained Name became a path-escape/injection surface, not just a
// cosmetic concern — see bundles.go's applyMounts, which interpolates the
// mount's Name into a shell script for the tarGz-unpack init container. This
// pins the fix at the pure-function level, independent of the
// Reconcile-level regression guard in skills_validation_test.go.
func TestValidateSkillsSpec_RejectsUnsafeCharacterSet(t *testing.T) {
	cases := []struct {
		name      string
		skillName string
		wantErr   string // "" = accepted
	}{
		{name: "path traversal: rejected", skillName: "../etc", wantErr: "must match [a-z0-9_-]{1,32}"},
		{name: "embedded slash: rejected", skillName: "a/b", wantErr: "must match [a-z0-9_-]{1,32}"},
		{name: "shell metacharacter: rejected", skillName: "code$(rm -rf /)", wantErr: "must match [a-z0-9_-]{1,32}"},
		{name: "uppercase: rejected", skillName: "Code-Review", wantErr: "must match [a-z0-9_-]{1,32}"},
		{name: "empty: rejected", skillName: "", wantErr: "must match [a-z0-9_-]{1,32}"},
		{name: "lowercase hyphenated: accepted", skillName: "code-review"},
		{name: "underscore: accepted", skillName: "code_review"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := classWithSkills(spiceboxv1alpha1.AgentSkill{Name: tc.skillName, Ref: "demo-org/demo-repo//x@v1"})
			reason, msg := validateSkillsSpec(&spec)
			if tc.wantErr == "" {
				require.Empty(t, reason, "unexpected reason=%q msg=%q", reason, msg)
				return
			}
			require.Equal(t, spiceboxv1alpha1.ReasonSpecInvalid, reason)
			assert.Contains(t, msg, tc.wantErr)
		})
	}
}
