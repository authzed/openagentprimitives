package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestAgentClassCapabilitiesRoundTrip(t *testing.T) {
	spec := AgentClassSpec{
		Capabilities: map[string]apiextensionsv1.JSON{
			"memory":    {Raw: []byte(`{"maxResults":20}`)},
			"knowledge": {Raw: []byte(`{}`)},
		},
	}
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	var got AgentClassSpec
	require.NoError(t, json.Unmarshal(b, &got))
	assert.JSONEq(t, `{"maxResults":20}`, string(got.Capabilities["memory"].Raw))
	assert.Contains(t, got.Capabilities, "knowledge")
}
