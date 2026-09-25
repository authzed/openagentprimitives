package validator

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
)

// This is the allowlist's real enforcement path, end to end through the pure
// validator: Check parses the argv, resolveResourceID mints call.resourceId from
// the git toolkit's own transforms, and the config-driven constraint evaluates
// against Invocation.Config (the `config` root). It is the closest a unit test
// gets to what the toolcall controller does in production; bronzethread cannot
// cover it because it runs no ToolCall reconciler.
func TestCheck_ConfigDrivenRepoAllowlist(t *testing.T) {
	tk := loadGitToolkit(t)
	sp := &spec.Spec{
		Name:             "git-ro",
		Version:          "1",
		Toolkit:          spec.ToolkitRef{Name: "git", Revision: "2026-04-24"},
		AllowSubcommands: []string{"clone", "status"},
		Constraints: []spec.Constraint{{
			CEL:     `call.resourceId == '' || config.allowedRepos.exists(r, glob.match('https://github.com/' + r, call.resourceId))`,
			Message: "repository not in allowlist",
		}},
	}
	allow := func(repos ...any) map[string]any {
		return map[string]any{"allowedRepos": repos}
	}

	cases := []struct {
		name      string
		argv      []string
		config    map[string]any
		wantAllow bool
	}{
		{"clone of an allowlisted org is allowed", []string{"clone", "https://github.com/demo-org/x"}, allow("demo-org/*"), true},
		{"clone of an exact allowlisted repo is allowed", []string{"clone", "https://github.com/demo-org/repo"}, allow("demo-org/repo"), true},
		{"clone off the allowlist is denied", []string{"clone", "https://github.com/evil/x"}, allow("demo-org/*"), false},
		{"clone with an empty allowlist is denied (fail-closed)", []string{"clone", "https://github.com/demo-org/x"}, allow(), false},
		{"local status op is not gated", []string{"status"}, allow("demo-org/*"), true},
		{"clone of a non-github remote is denied", []string{"clone", "https://gitlab.com/demo-org/x"}, allow("demo-org/*"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := Check(tk, sp, Invocation{Argv: tc.argv, Config: tc.config})
			require.NoError(t, err, "Check must not return an internal error for a well-formed call")
			assert.Equal(t, tc.wantAllow, dec.Allow, "decision for %v under %v", tc.argv, tc.config)
			if !tc.wantAllow {
				require.NotNil(t, dec.FailedOn)
				assert.Contains(t, dec.FailedOn.Message, "allowlist")
			}
		})
	}
}
