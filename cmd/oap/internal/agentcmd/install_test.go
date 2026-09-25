package agentcmd

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// skillsShapeFixtureBundle builds a one-CR bundle (an AgentClass) whose
// spec.skills is exactly skills — a raw YAML list body, spliced in verbatim,
// so the caller can supply either the pre-migration bare-string shape or the
// current {name, ref, target} object shape. Fabricated names throughout
// (demo-org/demo-reviewbot); none shared with any bundled example.
func skillsShapeFixtureBundle(skills string) *oap.Bundle {
	return &oap.Bundle{Manifests: []byte(fmt.Sprintf(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: demo-reviewbot-class
spec:
  description: "Fixture AgentClass for the install-time skills-shape guard."
  systemPrompt:
    inline: "You are a fixture agent used only by this test."
  skills:
%s
`, skills))}
}

// TestCheckSkillsShape pins checkSkillsShape's own contract, independent of
// its call site in RunE: the old shape refuses with the {name, ref, target}
// rewrite, and the current shape passes silently. See
// TestAgentInstall_OldSkillsShapeRefusesBeforeAnyClusterWrite
// (install_e2e_test.go) for the companion test that drives the real `oap
// agent install` command end to end and confirms the guard is actually
// wired in ahead of any cluster write.
func TestCheckSkillsShape(t *testing.T) {
	cases := []struct {
		name    string
		skills  string
		wantErr bool
	}{
		{
			name:    "old bare-string shape: refused with the rewrite",
			skills:  "    - github.com/demo-org/demo-skills//skills/code-review@v1.0.0",
			wantErr: true,
		},
		{
			name: "current {name, ref, target} shape: passes",
			skills: "    - name: code-review\n" +
				"      ref: \"github.com/demo-org/demo-skills//skills/code-review@v1.0.0\"\n" +
				"      target: sandbox",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSkillsShape(skillsShapeFixtureBundle(tc.skills))
			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "name: code-review",
				"the rewrite must be shown, not merely a problem reported")
			assert.Contains(t, err.Error(), "ref: github.com/demo-org/demo-skills//skills/code-review@v1.0.0")
			assert.Contains(t, err.Error(), "install refused before touching the cluster")
		})
	}
}

func TestSkillCloneNotice(t *testing.T) {
	t.Run("empty when no clones", func(t *testing.T) {
		assert.Empty(t, skillCloneNotice(nil))
	})

	t.Run("lists each repo with its ref and source name", func(t *testing.T) {
		out := skillCloneNotice([]oap.SkillClone{
			{SkillSource: "alpha", RepoURL: "https://github.com/fakeorg/skills", Ref: "main"},
			{SkillSource: "beta", RepoURL: "https://github.com/fakeorg/playbooks"},
		})
		assert.Contains(t, out, "2 skill source(s)")
		assert.Contains(t, out, "https://github.com/fakeorg/skills @ main")
		assert.Contains(t, out, `SkillSource "alpha"`)
		// A SkillSource with no ref reports the default branch, not an empty ref.
		assert.Contains(t, out, "https://github.com/fakeorg/playbooks @ (default branch)")
	})
}
