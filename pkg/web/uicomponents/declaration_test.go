package uicomponents_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// TestParseDeclarationRejectsUnknownWireFields pins the reason ParseDeclaration
// exists at all: json.Unmarshal accepts every declaration below and yields a
// node with no props and no children, which then VALIDATES CLEAN. The author is
// told success and gets an empty block.
func TestParseDeclarationRejectsUnknownWireFields(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{name: "misspelled Node.props: rejected", in: `{"slots":[{"name":"root","default":{"component":"ap:text","propz":{"text":"hi"}}}]}`},
		{name: "misspelled Node.children: rejected", in: `{"slots":[{"name":"root","default":{"component":"ap:stack","childs":[]}}]}`},
		{name: "misspelled Slot.agentWritable: rejected", in: `{"slots":[{"name":"root","agentWriteable":true}]}`},
		{name: "unknown Declaration field: rejected", in: `{"slots":[],"version":2}`},
		{name: "unknown Binding field: rejected", in: `{"slots":[{"name":"root","default":{"component":"ap:text","bindings":{"text":{"source":"tool","ref":"t","argz":{}}}}}]}`},
		{name: "misspelled Binding.select: rejected", in: `{"slots":[{"name":"root","default":{"component":"ap:text","bindings":{"text":{"source":"tool","ref":"t","selct":"a"}}}}]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := uicomponents.ParseDeclaration([]byte(tc.in))
			require.Error(t, err, "an unknown wire field must be a rejection, not a silently-empty node")
			assert.Contains(t, err.Error(), "parse declaration")
		})
	}
}

func TestParseDeclarationAcceptsAndPreservesEveryWireField(t *testing.T) {
	in := `{"slots":[{"name":"root","agentWritable":true,"default":{
		"component":"ap:stack",
		"props":{"gap":"lg"},
		"bindings":{"text":{"source":"tool","ref":"list_leads","args":{"limit":5}}},
		"children":[{"component":"ap:text","props":{"text":"hi"}}]
	}}]}`

	d, err := uicomponents.ParseDeclaration([]byte(in))
	require.NoError(t, err)
	assert.Nil(t, d.Slots, "slots are compiled away by Normalize, never carried alongside the view")

	// "root" is agentWritable, so CompileSlots turned it into a hook whose
	// children are the slot's original default.
	require.Len(t, d.View.Children, 1)
	hook := d.View.Children[0]
	assert.Equal(t, uicomponents.GenerativeType, hook.Component)
	require.Len(t, hook.Children, 1)
	s := hook.Children[0]
	assert.Equal(t, "ap:stack", s.Component)
	assert.JSONEq(t, `"lg"`, string(s.Props["gap"]))
	require.Len(t, s.Children, 1)
	assert.Equal(t, "ap:text", s.Children[0].Component)

	// Props and Args stay RawMessage on purpose: strictness is a property of
	// the four wire structs, not of the payloads they carry. A prop map with an
	// arbitrary key is Validate's business (against the component's own struct);
	// a binding's args are the source's business.
	b := s.Bindings["text"]
	assert.Equal(t, "tool", b.Source)
	assert.Equal(t, "list_leads", b.Ref)
	assert.JSONEq(t, `{"limit":5}`, string(b.Args))
}

func TestParseDeclarationNormalizesLegacySlotsIntoAView(t *testing.T) {
	d, err := uicomponents.ParseDeclaration([]byte(`{"slots":[
	  {"name":"a","agentWritable":true,"default":{"component":"ap:text","props":{"text":"A"}}},
	  {"name":"b","default":{"component":"ap:text","props":{"text":"B"}}},
	  {"name":"c","agentWritable":true}
	]}`))
	require.NoError(t, err)
	require.NotNil(t, d.View, "a parsed declaration always has a view")
	assert.Nil(t, d.Slots, "slots are compiled away, never carried alongside the view")
	assert.Equal(t, "ap:stack", d.View.Component)
	require.Len(t, d.View.Children, 3)

	a := d.View.Children[0]
	assert.Equal(t, uicomponents.GenerativeType, a.Component, "a writable slot becomes a hook")
	assert.JSONEq(t, `"a"`, string(a.Props["name"]))
	assert.JSONEq(t, `["*"]`, string(a.Props["allowedComponents"]), "the shim admits the whole vocabulary")
	_, hasIntent := a.Props["intent"]
	assert.False(t, hasIntent, "the shim writes no intent; the runner logs the absence")
	require.Len(t, a.Children, 1)
	assert.Equal(t, "ap:text", a.Children[0].Component, "the slot's default becomes the hook's children")

	assert.Equal(t, "ap:text", d.View.Children[1].Component, "a non-writable slot is placed directly: no hook, no region")

	c := d.View.Children[2]
	assert.Equal(t, uicomponents.GenerativeType, c.Component)
	assert.Empty(t, c.Children, "a writable slot with no default is an empty hook")
}

func TestParseDeclarationKeepsAnAuthoredView(t *testing.T) {
	d, err := uicomponents.ParseDeclaration([]byte(`{"view":{"component":"ap:card","props":{"title":"t"},"children":[
	  {"component":"oap:generative","props":{"name":"brief","intent":"the brief","allowedComponents":["ap:markdown"]}}
	]}}`))
	require.NoError(t, err)
	assert.Nil(t, d.Slots)
	assert.Equal(t, "ap:card", d.View.Component)
}

func TestNormalizeRejectsViewAndSlotsTogether(t *testing.T) {
	n := uicomponents.Node{Component: "ap:text"}
	_, err := uicomponents.Normalize(uicomponents.Declaration{View: &n, Slots: []uicomponents.Slot{{Name: "x"}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "either view or slots")
}

func TestNormalizeOfNothingIsAnEmptyStack(t *testing.T) {
	d, err := uicomponents.Normalize(uicomponents.Declaration{})
	require.NoError(t, err)
	require.NotNil(t, d.View)
	assert.Equal(t, "ap:stack", d.View.Component)
	assert.Empty(t, d.View.Children)
}

// TestParseDeclarationDoesNotConstrainPropNames guards the boundary between the
// two strictness layers: an unknown PROP name must reach Validate (which knows
// the component's struct and rejects it there, with the offending node's path),
// not die at the wire layer with an error that names no node.
func TestParseDeclarationDoesNotConstrainPropNames(t *testing.T) {
	d, err := uicomponents.ParseDeclaration(
		[]byte(`{"slots":[{"name":"root","default":{"component":"ap:leaf","props":{"nope":"x"}}}]}`))
	require.NoError(t, err, "prop names are Validate's business, not the wire parser's")

	installFixtureVocabulary(t)
	err = uicomponents.Validate(d, uicomponents.DefaultOptions())
	require.Error(t, err)
	var ve *uicomponents.ValidationError
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "view.children[0]", ve.Path)
	assert.Contains(t, ve.Reason, `unknown prop "nope"`)
}

// TestParseDeclarationAcceptsASelector pins the wire shape of Binding.Select:
// a declaration carrying "select" on a binding parses, and the exact string
// survives untouched (ParseDeclaration does not parse it — that is
// pkg/web/uiselect.Parse's job, invoked by Validate).
func TestParseDeclarationAcceptsASelector(t *testing.T) {
	in := `{"slots":[{"name":"root","default":{"component":"ap:text",
	  "bindings":{"text":{"source":"tool","ref":"list_leads","select":"results[].properties"}}}}]}`

	d, err := uicomponents.ParseDeclaration([]byte(in))
	require.NoError(t, err)

	// "root" is not agentWritable, so it is placed directly at view.children[0].
	require.Len(t, d.View.Children, 1)
	b := d.View.Children[0].Bindings["text"]
	assert.Equal(t, "results[].properties", b.Select)
}
