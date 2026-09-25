package authz_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

func TestResolveTemplate(t *testing.T) {
	cases := []struct {
		name       string
		template   string
		args       map[string]any
		transforms []string
		want       string
		wantErr    string // substring; empty means expect no error
	}{
		{
			name:     "single arg substitutes value",
			template: "{repo}",
			args:     map[string]any{"repo": "spicedb"},
			want:     "spicedb",
		},
		{
			name:     "multi-arg composes both refs",
			template: "{owner}/{repo}",
			args:     map[string]any{"owner": "Authzed", "repo": "SpiceDB"},
			want:     "Authzed/SpiceDB",
		},
		{
			name:       "lowercase transform pipeline applies after substitution",
			template:   "{owner}/{repo}",
			args:       map[string]any{"owner": "Authzed", "repo": "SpiceDB"},
			transforms: []string{"lowercase"},
			want:       "authzed/spicedb",
		},
		{
			name:     "literal {{ escape produces literal brace",
			template: "{{literal}",
			args:     map[string]any{},
			want:     "{literal}",
		},
		{
			name:     "numeric arg coerces to string",
			template: "{n}",
			args:     map[string]any{"n": 42},
			want:     "42",
		},
		{
			name:     "missing arg returns error naming the ref",
			template: "{nope}",
			args:     map[string]any{},
			wantErr:  "nope",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := authz.ResolveTemplate(tc.template, tc.args, tc.transforms)
			if tc.wantErr != "" {
				require.Error(t, err, "expected error")
				assert.Contains(t, err.Error(), tc.wantErr, "error should mention the missing/invalid token")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestExtractTemplateRefs(t *testing.T) {
	got := authz.ExtractTemplateRefs("{owner}/{repo}/{owner}")
	assert.Equal(t, []string{"owner", "repo"}, got)
}
