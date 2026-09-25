package validator

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// loadExample pairs a shipped toolkit with a spec fixture this package owns.
//
// The toolkit comes from the real toolkits/ tree on purpose: these cases exist
// to prove the validator handles the full production definitions, which carry
// permission blocks and subcommands the trimmed testdata/<tool>/toolkit.yaml
// fixtures used by TestGolden deliberately omit. The spec, by contrast, is a
// local copy — tests must not read from examples/, which is demo material users
// are meant to edit freely.
func loadExample(t *testing.T, toolkitRel, specRel string) (*toolkit.Toolkit, *spec.Spec) {
	t.Helper()
	repoRoot := filepath.Join("..", "..", "..", "..")
	tk, err := toolkit.Load(filepath.Join(repoRoot, "toolkits", toolkitRel))
	require.NoError(t, err, "load toolkit %s", toolkitRel)
	sp, err := spec.Load(filepath.Join("testdata", specRel))
	require.NoError(t, err, "load spec %s", specRel)
	return tk, sp
}

func TestExamples_AllowDenyMatrix(t *testing.T) {
	cases := []struct {
		name      string
		toolkit   string
		spec      string
		inv       Invocation
		wantAllow bool
	}{
		{
			name:    "gh-readonly: pr view in allowed repo allows",
			toolkit: "gh.yaml",
			spec:    "gh-readonly-spec.yaml",
			inv: Invocation{
				Command:       "gh",
				Argv:          []string{"pr", "view", "123", "-R", "authzed/openagentprimitives"},
				BinaryVersion: "2.53.1",
			},
			wantAllow: true,
		},
		{
			name:    "gh-readonly: pr merge denies (mutating subcommand)",
			toolkit: "gh.yaml",
			spec:    "gh-readonly-spec.yaml",
			inv: Invocation{
				Command:       "gh",
				Argv:          []string{"pr", "merge", "45", "-R", "authzed/openagentprimitives"},
				BinaryVersion: "2.53.1",
			},
			wantAllow: false,
		},
		{
			name:    "gh-readonly: pr view in wrong repo denies",
			toolkit: "gh.yaml",
			spec:    "gh-readonly-spec.yaml",
			inv: Invocation{
				Command:       "gh",
				Argv:          []string{"pr", "view", "1", "-R", "some/other"},
				BinaryVersion: "2.53.1",
			},
			wantAllow: false,
		},
		{
			name:    "kubectl-readonly: get in allowed namespace allows",
			toolkit: "kubectl.yaml",
			spec:    "kubectl-readonly-spec.yaml",
			inv: Invocation{
				Command:       "kubectl",
				Argv:          []string{"get", "pods", "-n", "tenant-foo"},
				BinaryVersion: "1.30.0",
			},
			wantAllow: true,
		},
		{
			name:    "kubectl-readonly: apply denies (mutating subcommand)",
			toolkit: "kubectl.yaml",
			spec:    "kubectl-readonly-spec.yaml",
			inv: Invocation{
				Command:       "kubectl",
				Argv:          []string{"apply", "-f", "x.yaml", "-n", "tenant-foo"},
				BinaryVersion: "1.30.0",
			},
			wantAllow: false,
		},
		{
			name:    "kubectl-readonly: get in wrong namespace denies",
			toolkit: "kubectl.yaml",
			spec:    "kubectl-readonly-spec.yaml",
			inv: Invocation{
				Command:       "kubectl",
				Argv:          []string{"get", "pods", "-n", "other-ns"},
				BinaryVersion: "1.30.0",
			},
			wantAllow: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tk, sp := loadExample(t, tc.toolkit, tc.spec)
			d, err := Check(tk, sp, tc.inv)
			require.NoError(t, err, "Check")
			assert.Equal(t, tc.wantAllow, d.Allow, "decision allow; failedOn=%+v", d.FailedOn)
		})
	}
}
