package validator

import (
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

// Comparisons use three dots; refusing traversal must not refuse valid SHAs.
func TestScheduledCoverageAPI(t *testing.T) {
	tk := loadGhToolkit(t)
	sp, err := spec.Load("testdata/scheduled-coverage-gh.yaml")
	require.NoError(t, err)
	for _, tt := range []struct {
		name  string
		argv  []string
		allow bool
	}{
		{"metadata", []string{"api", "repos/acme/widget"}, true},
		{"commit", []string{"api", "repos/acme/widget/commits/abc123"}, true},
		{"compare SHAs", []string{"api", "repos/acme/widget/compare/abc123...def456"}, true},
		{"traversal", []string{"api", "repos/acme/widget/compare/../../issues"}, false},
		{"comment write", []string{"api", "repos/acme/widget/issues/1/comments"}, false},
		{"post method", []string{"api", "repos/acme/widget", "--method", "POST"}, false},
		{"implicit post", []string{"api", "repos/acme/widget", "--raw-field", "x=y"}, false},
		{"host override", []string{"api", "repos/acme/widget", "--hostname", "evil.test"}, false},
		{"different repo", []string{"api", "repos/acme/other"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dec, err := Check(tk, sp, Invocation{Argv: tt.argv, Config: map[string]any{"allowedRepos": []any{"acme/widget"}}})
			require.NoError(t, err)
			assert.Equal(t, tt.allow, dec.Allow, "%v: %+v", tt.argv, dec.FailedOn)
		})
	}
}
