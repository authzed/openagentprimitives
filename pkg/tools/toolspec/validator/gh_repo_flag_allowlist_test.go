package validator

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

func loadGhToolkit(t *testing.T) *toolkit.Toolkit {
	t.Helper()
	tk, err := toolkit.Load(filepath.Join("..", "..", "..", "..", "toolkits", "gh.yaml"))
	require.NoError(t, err, "load gh toolkit")
	return tk
}

// ghAllowlistCEL mirrors the gh-review / git-ro repo-allowlist constraint shipped
// at examples/reviewbot/manifests/toolspecs.yaml (kept in sync by hand, like the
// other parallel copies in this repo — see reviewbot_constraints_test.go).
const ghAllowlistCEL = `call.resourceId == '' || config.allowedRepos.exists(r, glob.match('https://github.com/' + r, call.resourceId))`

// Reproduces the live reviewbot failure (session demo-reviewbot-gh-4fbe4037):
// `gh pr view <n> --repo owner/repo` was denied by
// the config-driven repo allowlist even under allowedRepos:["acme/*"], while
// the SAME allowlist matched a git clone of the full URL
// (TestCheck_ConfigDrivenRepoAllowlist). Root cause: the gh toolkit's repo
// checks use resourceIDTransforms:[github_repo_url_id], which base64url-encodes
// the URL for a SpiceDB object id; celTransforms must expose it to CEL as the
// plain https URL (via github_repo_id) instead. This full-path test is the
// vehicle because bronzethread runs no ToolCall reconciler and stubs tool
// results — it cannot reach the config-driven constraint.
func TestCheck_GhRepoFlagAllowlist(t *testing.T) {
	tk := loadGhToolkit(t)

	// The gh --repo form must resolve call.resourceId to the PLAIN canonical
	// URL, identical to what the git toolkit's normalize_url'd clone URL yields
	// — not the base64url object key. This is the regression the fix guards.
	p, err := pickParser(tk)
	require.NoError(t, err)
	call, err := p.Parse(tk, []string{"pr", "view", "12055", "--repo", "acme/internal"})
	require.NoError(t, err, "parse gh pr view")
	assert.Equal(t, "https://github.com/acme/internal", resolveResourceID(tk, call),
		"gh --repo resourceId must be the plain canonical URL for a readable allowlist")

	sp := &spec.Spec{
		Name:             "gh-review",
		Version:          "1",
		Toolkit:          spec.ToolkitRef{Name: "gh", Revision: "2026-05-01"},
		AllowSubcommands: []string{"pr view"},
		Constraints: []spec.Constraint{{
			CEL:     ghAllowlistCEL,
			Message: "This repository is not in reviewbot's allowlist (spec.config.allowedRepos).",
		}},
	}

	cases := []struct {
		name      string
		repo      string
		allowed   []any
		wantAllow bool
	}{
		{"org glob allows a repo in the org", "acme/internal", []any{"acme/*"}, true},
		{"exact repo allowed", "acme/internal", []any{"acme/internal"}, true},
		{"off-list org denied", "other/thing", []any{"acme/*"}, false},
		{"empty allowlist denies (fail-closed)", "acme/internal", []any{}, false},
		{"all-repos sentinel */* allows any repo", "anyorg/anyrepo", []any{"*/*"}, true},
		{"all-repos sentinel */* allows acme too", "acme/internal", []any{"*/*"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := Check(tk, sp, Invocation{
				Argv:   []string{"pr", "view", "12055", "--repo", tc.repo},
				Config: map[string]any{"allowedRepos": tc.allowed},
			})
			require.NoError(t, err, "Check must not return an internal error")
			if dec.FailedOn != nil {
				t.Logf("denied: path=%q msg=%q", dec.FailedOn.Path, dec.FailedOn.Message)
			}
			assert.Equal(t, tc.wantAllow, dec.Allow,
				"gh pr view --repo %s under allowedRepos=%v", tc.repo, tc.allowed)
		})
	}
}
