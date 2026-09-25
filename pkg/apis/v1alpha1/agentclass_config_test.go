package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestAgentClassConfigRoundTrips(t *testing.T) {
	in := AgentClassSpec{
		Config: map[string]apiextensionsv1.JSON{
			"allowedRepos": {Raw: []byte(`["demo-org/*","owner/repo"]`)},
		},
		ConfigSchema: []ConfigKeySchema{
			{Name: "allowedRepos", Type: "stringList", Pattern: `^([a-z0-9_.-]+|\*)/([a-z0-9_.-]+|\*)$`},
		},
	}
	b, err := json.Marshal(in)
	require.NoError(t, err)

	var out AgentClassSpec
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, in.ConfigSchema, out.ConfigSchema)
	assert.JSONEq(t, `["demo-org/*","owner/repo"]`, string(out.Config["allowedRepos"].Raw))
}
