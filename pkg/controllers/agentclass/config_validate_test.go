package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func cfg(raw string) apiextensionsv1.JSON { return apiextensionsv1.JSON{Raw: []byte(raw)} }

func TestValidateConfig(t *testing.T) {
	schema := []v1alpha1.ConfigKeySchema{
		{Name: "allowedRepos", Type: "stringList", Pattern: `^([a-z0-9_.-]+|\*)/([a-z0-9_.-]+|\*)$`, Required: true},
	}
	cases := []struct {
		name    string
		config  map[string]apiextensionsv1.JSON
		celRefs []string
		wantErr string
	}{
		{
			name:   "valid list passes",
			config: map[string]apiextensionsv1.JSON{"allowedRepos": cfg(`["demo-org/*","owner/repo"]`)},
		},
		{
			name:   "all-repos sentinel */* passes (operator's deliberate widen)",
			config: map[string]apiextensionsv1.JSON{"allowedRepos": cfg(`["*/*"]`)},
		},
		{
			name:    "required key missing: rejected",
			config:  map[string]apiextensionsv1.JSON{},
			wantErr: "allowedRepos",
		},
		{
			name:    "wrong type (scalar for stringList): rejected",
			config:  map[string]apiextensionsv1.JSON{"allowedRepos": cfg(`"demo-org/x"`)},
			wantErr: "stringList",
		},
		{
			name:    "item fails pattern (uppercase): rejected",
			config:  map[string]apiextensionsv1.JSON{"allowedRepos": cfg(`["Demo-Org/*"]`)},
			wantErr: "pattern",
		},
		{
			name:    "config key not declared in schema: rejected",
			config:  map[string]apiextensionsv1.JSON{"allowedRepos": cfg(`["demo-org/*"]`), "stray": cfg(`"x"`)},
			wantErr: "stray",
		},
		{
			name:    "CEL references an undeclared config key: rejected",
			config:  map[string]apiextensionsv1.JSON{"allowedRepos": cfg(`["demo-org/*"]`)},
			celRefs: []string{"allowedRepos", "notDeclared"},
			wantErr: "notDeclared",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConfig(&v1alpha1.AgentClassSpec{Config: tc.config, ConfigSchema: schema}, tc.celRefs)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestConfigRefsFromToolspecs(t *testing.T) {
	specs := []v1alpha1.SpiceboxToolspec{
		{Spec: v1alpha1.SpiceboxToolspecSpec{Constraints: []v1alpha1.ToolspecConstraint{
			{CEL: "call.resourceId == '' || config.allowedRepos.exists(r, glob.match(r, call.resourceId))"},
			{CEL: "call.subcommand != 'x' || config.maxDepth > 0"},
		}}},
		{Spec: v1alpha1.SpiceboxToolspecSpec{Constraints: []v1alpha1.ToolspecConstraint{
			{CEL: "config.allowedRepos.size() > 0"}, // duplicate ref, must dedup
		}}},
	}
	got := configRefsFromToolspecs(specs)
	assert.ElementsMatch(t, []string{"allowedRepos", "maxDepth"}, got)
}
