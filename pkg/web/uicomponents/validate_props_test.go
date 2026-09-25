package uicomponents_test

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents/registry"
)

// propsNode wraps a node as a non-writable "root" slot, compiled through the
// shim — so it sits at view.children[0], same as decl (validate_test.go).
func propsNode(component string, props map[string]json.RawMessage) uicomponents.Declaration {
	return uicomponents.Declaration{View: uicomponents.CompileSlots([]uicomponents.Slot{{
		Name:    "root",
		Default: &uicomponents.Node{Component: component, Props: props},
	}})}
}

func TestValidatePropsFailures(t *testing.T) {
	installFixtureVocabulary(t)

	cases := []struct {
		name   string
		props  map[string]json.RawMessage
		reason string
	}{
		{
			name:   "unknown prop name: rejected rather than ignored",
			props:  map[string]json.RawMessage{"nope": json.RawMessage(`"x"`)},
			reason: "invalid props",
		},
		{
			name:   "wrong prop type: rejected",
			props:  map[string]json.RawMessage{"text": json.RawMessage(`123`)},
			reason: "invalid props",
		},
		{
			name:   "malformed prop JSON: rejected",
			props:  map[string]json.RawMessage{"text": json.RawMessage(`{`)},
			reason: "invalid props",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := uicomponents.Validate(propsNode("ap:leaf", tc.props), uicomponents.DefaultOptions())
			require.Error(t, err)

			var ve *uicomponents.ValidationError
			require.ErrorAs(t, err, &ve)
			assert.Equal(t, "view.children[0]", ve.Path)
			assert.Contains(t, ve.Reason, tc.reason)
		})
	}
}

// TestValidatePropsRequiresTheExactJSONFieldName pins the one spelling of a
// prop name the platform accepts. encoding/json matches struct fields
// CASE-INSENSITIVELY and DisallowUnknownFields does not change that, so without
// an explicit exact-name check "Text"/"TEXT"/"tExT" all decode cleanly into
// leafProps.Text — while the emitted JSON Schema (additionalProperties:false
// over the property "text") rejects them and the React renderer, which reads
// the exact key props["text"], renders nothing. The agent would be told SUCCESS
// and shown an empty block.
func TestValidatePropsRequiresTheExactJSONFieldName(t *testing.T) {
	installFixtureVocabulary(t)

	cases := []struct {
		name     string
		propName string
		accepted bool
	}{
		{name: "exact json tag name: accepted", propName: "text", accepted: true},
		{name: "all-caps variant: rejected, not case-folded onto the field", propName: "TEXT"},
		{name: "Go field name spelling: rejected, not case-folded onto the field", propName: "Text"},
		{name: "mixed-case variant: rejected, not case-folded onto the field", propName: "tExT"},
		{name: "invented name: rejected", propName: "nope"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := uicomponents.Validate(
				propsNode("ap:leaf", map[string]json.RawMessage{tc.propName: json.RawMessage(`"hello"`)}),
				uicomponents.DefaultOptions(),
			)
			if tc.accepted {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err, "only the exact json field name may be accepted")

			var ve *uicomponents.ValidationError
			require.ErrorAs(t, err, &ve, "the error must be structured so the agent can correct it")
			assert.Equal(t, "view.children[0]", ve.Path)
			assert.Contains(t, ve.Reason, "unknown prop "+strconv.Quote(tc.propName),
				"the agent must be told which prop name was wrong, in our wording — not Go's")
			assert.NotContains(t, ve.Reason, "json: unknown field",
				"encoding/json's internal wording must not leak into an agent-facing error")
		})
	}
}

func TestValidateAcceptsWellTypedProps(t *testing.T) {
	installFixtureVocabulary(t)
	err := uicomponents.Validate(
		propsNode("ap:leaf", map[string]json.RawMessage{"text": json.RawMessage(`"hello"`)}),
		uicomponents.DefaultOptions(),
	)
	assert.NoError(t, err)
}

func TestValidateAcceptsAbsentProps(t *testing.T) {
	installFixtureVocabulary(t)
	assert.NoError(t, uicomponents.Validate(propsNode("ap:leaf", nil), uicomponents.DefaultOptions()),
		"every prop is optional at the schema level; a component renders its own defaults")
}

// TestValidatePropsRejectsNilPropsSchemaInsteadOfPanicking guards a malformed
// registration: a component registered with Props: nil (a programmer error
// that should never reach production, but registry.Register has no way to
// catch it — nil is a legal `any`). Before the nil guard, validateProps called
// reflect.New(reflect.TypeOf(c.Props)) unconditionally; reflect.TypeOf(nil)
// returns nil, and reflect.New(nil) panics. Validate has no recover(), so a
// bad registration fed a non-empty props map would panic the whole validation
// call instead of returning the structured *ValidationError every other
// rejection in this file returns.
func TestValidatePropsRejectsNilPropsSchemaInsteadOfPanicking(t *testing.T) {
	installFixtureVocabulary(t)
	registry.Register(uicomponents.Component{Type: "ap:nilprops", Props: nil})

	var err error
	require.NotPanics(t, func() {
		err = uicomponents.Validate(
			propsNode("ap:nilprops", map[string]json.RawMessage{"anything": json.RawMessage(`"x"`)}),
			uicomponents.DefaultOptions(),
		)
	}, "a malformed registration must be reported as an error, not crash validation")

	require.Error(t, err)
	var ve *uicomponents.ValidationError
	require.ErrorAs(t, err, &ve, "the error must be structured so the agent can correct it")
	assert.Equal(t, "view.children[0]", ve.Path)
	assert.Contains(t, ve.Reason, "invalid props")
}

type ptrProps struct {
	Text string `json:"text,omitempty"`
}

// TestValidatePropsRejectsNonStructPropsSchemaInsteadOfPanicking is the
// nil-Props guard's sibling, for the other malformed registration this file's
// reflection can hit: a POINTER props schema (Props: &ptrProps{}). It is the
// plausible mistake — it reads as correct, and component.Schema() accepts it
// happily — so a bad registration installs cleanly and only fails later, on the
// first agent-supplied props map. jsonFieldNames calls NumField, which panics on
// any non-struct kind, and Validate has no recover() on this call path.
//
// Third-party callers of registry.Register are anticipated, which is exactly
// where a pointer Props would come from.
func TestValidatePropsRejectsNonStructPropsSchemaInsteadOfPanicking(t *testing.T) {
	installFixtureVocabulary(t)
	registry.Register(uicomponents.Component{Type: "ap:ptrprops", Props: &ptrProps{}})

	var err error
	require.NotPanics(t, func() {
		err = uicomponents.Validate(
			propsNode("ap:ptrprops", map[string]json.RawMessage{"text": json.RawMessage(`"x"`)}),
			uicomponents.DefaultOptions(),
		)
	}, "a malformed registration must be reported as an error, not crash validation")

	require.Error(t, err, "a pointer props schema cannot be reflected over and must be refused")
	var ve *uicomponents.ValidationError
	require.ErrorAs(t, err, &ve, "the error must be structured so the agent can correct it")
	assert.Equal(t, "view.children[0]", ve.Path)
	assert.Contains(t, ve.Reason, "invalid props")
	assert.Contains(t, ve.Reason, "non-struct props schema",
		"the error must name the actual defect, so the fix is to the registration and not the declaration")
}
