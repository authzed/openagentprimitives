package uicomponents_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

func TestHooksFindsEveryHookAtAnyDepthInDocumentOrder(t *testing.T) {
	d, err := uicomponents.ParseDeclaration([]byte(`{"view":{"component":"ap:stack","children":[
	  {"component":"oap:generative","props":{"name":"phase","intent":"the timeline","allowedComponents":["ap:steps"]}},
	  {"component":"ap:card","children":[
	    {"component":"oap:generative","props":{"name":"brief","allowedComponents":["*"]},
	     "children":[{"component":"ap:text","props":{"text":"default"}}]}
	  ]},
	  {"component":"ap:text","props":{"text":"not a hook"}}
	]}}`))
	require.NoError(t, err)

	got := uicomponents.Hooks(d)
	require.Len(t, got, 2)
	assert.Equal(t, uicomponents.Hook{Name: "phase", Intent: "the timeline", AllowedComponents: []string{"ap:steps"}, Path: []int{0}}, got[0])
	assert.Equal(t, uicomponents.Hook{Name: "brief", AllowedComponents: []string{"*"}, Path: []int{1, 0}}, got[1])
}

func TestHooksOfAShimmedDeclarationAreTheWritableSlots(t *testing.T) {
	d, err := uicomponents.ParseDeclaration([]byte(`{"slots":[{"name":"a","agentWritable":true},{"name":"b"},{"name":"c","agentWritable":true}]}`))
	require.NoError(t, err)
	names := []string{}
	for _, h := range uicomponents.Hooks(d) {
		names = append(names, h.Name)
	}
	assert.Equal(t, []string{"a", "c"}, names)
}

func TestHooksCarryTheirStep(t *testing.T) {
	d := parse(t, `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"brief","allowedComponents":["*"],"step":"assess","title":"Brief"}}]}`)
	hooks := uicomponents.Hooks(d)
	require.Len(t, hooks, 1)
	assert.Equal(t, "assess", hooks[0].Step)
	assert.Equal(t, "Brief", hooks[0].Title)
}

func TestHooksDoesNotValidate(t *testing.T) {
	// Two hooks with the same name, one with no name at all: Hooks reports
	// what is there; Validate (Task 3) is what rejects it.
	d, err := uicomponents.ParseDeclaration([]byte(`{"view":{"component":"ap:stack","children":[
	  {"component":"oap:generative","props":{"name":"x"}},
	  {"component":"oap:generative","props":{"name":"x"}},
	  {"component":"oap:generative"}
	]}}`))
	require.NoError(t, err)
	assert.Len(t, uicomponents.Hooks(d), 3)
}
