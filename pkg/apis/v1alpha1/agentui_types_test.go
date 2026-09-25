package v1alpha1_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestAgentUISpecRoundTripsThroughJSON(t *testing.T) {
	in := spiceboxv1alpha1.AgentUI{
		Spec: spiceboxv1alpha1.AgentUISpec{
			DisplayName: "Demo Console",
			Chrome:      &spiceboxv1alpha1.AgentUIChrome{InitialState: "expanded"},
			Slots: []spiceboxv1alpha1.AgentUISlot{{
				Name:          "root",
				AgentWritable: true,
				Default:       &apiextensionsv1.JSON{Raw: []byte(`{"component":"ap:stack"}`)},
			}},
			Tools: []string{"list_leads"},
		},
	}

	raw, err := json.Marshal(in)
	require.NoError(t, err, "marshal AgentUI")

	var out spiceboxv1alpha1.AgentUI
	require.NoError(t, json.Unmarshal(raw, &out), "unmarshal AgentUI")

	assert.Equal(t, "Demo Console", out.Spec.DisplayName)
	assert.Equal(t, "root", out.Spec.Slots[0].Name)
	assert.True(t, out.Spec.Slots[0].AgentWritable)
	assert.JSONEq(t, `{"component":"ap:stack"}`, string(out.Spec.Slots[0].Default.Raw))
	assert.Equal(t, []string{"list_leads"}, out.Spec.Tools)
}

func TestAgentUISpecViewRoundTripsAsRawJSON(t *testing.T) {
	in := spiceboxv1alpha1.AgentUISpec{View: &apiextensionsv1.JSON{Raw: []byte(`{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"brief","allowedComponents":["*"]}}]}`)}}
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"view":{"component":"ap:stack"`)
	assert.NotContains(t, string(raw), `"slots"`, "slots is omitted when unset — a view-only page carries no empty slot list")
	var out spiceboxv1alpha1.AgentUISpec
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.JSONEq(t, string(in.View.Raw), string(out.View.Raw))
}

func TestAgentUIStatusHooksIsAnObservation(t *testing.T) {
	st := spiceboxv1alpha1.AgentUIStatus{Hooks: []spiceboxv1alpha1.AgentUIHook{{Name: "brief", Intent: "the brief", AllowedComponents: []string{"*"}}}}
	raw, err := json.Marshal(st)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"hooks":[{"name":"brief","intent":"the brief","allowedComponents":["*"]}]`)
}

func TestAgentUIStatusEligibleToolsIsObservationOnly(t *testing.T) {
	// status.eligibleTools is controller-owned. Assert it is absent from a
	// freshly-authored spec's JSON so a client that applies a bundle cannot
	// smuggle a value through the spec payload.
	in := spiceboxv1alpha1.AgentUI{Spec: spiceboxv1alpha1.AgentUISpec{Tools: []string{"list_leads"}}}
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "eligibleTools",
		"an authored AgentUI must not carry eligibleTools; it is a controller observation")
}
