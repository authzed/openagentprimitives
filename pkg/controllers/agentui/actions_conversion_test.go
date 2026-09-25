package agentui

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// A field added to AgentUIAction has to be carried by EVERY conversion out of
// it, and there are two independent ones (this package's, and pkg/web/uiview's).
// Prompt was added and this one was missed, so an AgentUI that was valid as
// written reconciled Valid=False with a message describing a document nobody
// had authored: the validator saw an action declaring neither a tool nor a
// prompt, because the prompt had been dropped in transit.
//
// This asserts over the STRUCT'S FIELDS rather than a hand-written list, so
// the next field added fails here until it is carried — a transcribed list
// would have to be updated by the same person who forgot the conversion.
func TestActionsToDeclarationCarriesEveryAuthoredField(t *testing.T) {
	in := spiceboxv1alpha1.AgentUIAction{
		Name:   "company_detail",
		Tool:   "crm_lookup",
		Prompt: "Tell me about {name}.",
		Args:   &apiextensionsv1.JSON{Raw: []byte(`{"id":1}`)},
		Inputs: []string{"name"},
	}

	out := actionsToDeclaration([]spiceboxv1alpha1.AgentUIAction{in})
	require.Len(t, out, 1)
	got := out[0]

	assert.Equal(t, in.Name, got.Name)
	assert.Equal(t, in.Tool, got.Tool)
	assert.Equal(t, in.Prompt, got.Prompt, "a dropped prompt reconciles a valid AgentUI as invalid")
	assert.JSONEq(t, `{"id":1}`, string(got.Args))
	assert.Equal(t, in.Inputs, got.Inputs)

	// Every field on the CRD type was set to something non-zero above, so any
	// field left at its zero value on the way out was dropped. This is what
	// makes the next added field fail here rather than in a cluster.
	v := reflect.ValueOf(in)
	for i := range v.NumField() {
		assert.False(t, v.Field(i).IsZero(),
			"the fixture must set every AgentUIAction field, or this test cannot notice one being dropped: %s",
			v.Type().Field(i).Name)
	}
}

// The two kinds must survive conversion as themselves: a tool action with no
// prompt, and a prompt action with no tool. Round-tripping either into the
// other shape is what Validate then rejects.
func TestActionsToDeclarationKeepsTheTwoKindsDistinct(t *testing.T) {
	out := actionsToDeclaration([]spiceboxv1alpha1.AgentUIAction{
		{Name: "refresh", Tool: "crm_refresh"},
		{Name: "summarize", Prompt: "Summarize what's on screen."},
	})
	require.Len(t, out, 2)

	assert.Equal(t, "crm_refresh", out[0].Tool)
	assert.Empty(t, out[0].Prompt, "a tool action must not acquire a prompt")

	assert.Equal(t, "Summarize what's on screen.", out[1].Prompt)
	assert.Empty(t, out[1].Tool, "a prompt action must not acquire a tool")
}

// Gate A asks which tools an action table requires a grant for. A prompt
// action reaches no tool, so it contributes nothing — emitting its empty Tool
// made the gate report a missing grant for the tool "", which names nothing
// and cannot be fixed by adding anything to spec.tools.
func TestActionToolNamesIgnoresPromptActions(t *testing.T) {
	got := actionToolNames([]spiceboxv1alpha1.AgentUIAction{
		{Name: "summarize", Prompt: "Summarize what's on screen."},
		{Name: "refresh", Tool: "crm_refresh"},
	})
	assert.Equal(t, []string{"crm_refresh"}, got)

	assert.Empty(t, actionToolNames([]spiceboxv1alpha1.AgentUIAction{
		{Name: "summarize", Prompt: "Summarize what's on screen."},
	}), "a table of only prompt actions requires no tool grant at all")
}
