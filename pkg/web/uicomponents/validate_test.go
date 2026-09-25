package uicomponents_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents/registry"
)

type boxProps struct {
	Gap int `json:"gap,omitempty"`
}

type leafProps struct {
	Text string `json:"text,omitempty"`
}

// installFixtureVocabulary swaps the registry for a three-component fixture
// set (two containers, one leaf) and restores the real vocabulary afterwards.
// The validator's behavior must not depend on WHICH components exist, so the
// unit tests deliberately do not lean on the platform vocabulary.
//
// "ap:stack" is registered here (not just "ap:box") because CompileSlots — the
// shim decl() goes through — always wraps its output in a literal "ap:stack"
// root node, whatever vocabulary is installed at the time; without it, every
// decl()-built fixture would fail one level up from the node actually under
// test, on the wrapper the test never meant to exercise.
func installFixtureVocabulary(t *testing.T) {
	t.Helper()
	prior := registry.All()
	registry.Reset()
	t.Cleanup(func() {
		registry.Reset()
		for _, p := range prior {
			registry.Register(p)
		}
	})
	registry.Register(uicomponents.Component{
		Type: "ap:stack", Props: boxProps{}, AcceptsChildren: true,
	})
	registry.Register(uicomponents.Component{
		Type: "ap:box", Props: boxProps{}, AcceptsChildren: true,
	})
	registry.Register(uicomponents.Component{
		Type: "ap:leaf", Props: leafProps{}, Bindable: []string{"text"},
	})
}

func node(component string, children ...uicomponents.Node) uicomponents.Node {
	return uicomponents.Node{Component: component, Children: children}
}

// decl wraps n as a non-writable "root" slot, compiled through the shim —
// so the node under test sits at view.children[0], the same place every
// existing structural-failure test in this file expects it.
func decl(n uicomponents.Node) uicomponents.Declaration {
	return uicomponents.Declaration{View: uicomponents.CompileSlots([]uicomponents.Slot{{Name: "root", Default: &n}})}
}

func TestValidateAcceptsAWellFormedDeclaration(t *testing.T) {
	installFixtureVocabulary(t)
	err := uicomponents.Validate(decl(node("ap:box", node("ap:leaf"))), uicomponents.DefaultOptions())
	assert.NoError(t, err)
}

// TestValidateStructuralFailures no longer has duplicate-slot-name/empty-slot-name
// cases: Validate rejects an un-normalized Slots list outright (see
// TestValidateRejectsUnnormalizedSlots) rather than walking it, so slot-name
// uniqueness is no longer a Validate-time check at all. Its successor — hook
// NAME uniqueness on the compiled view — is Task 3's, per Hooks' own doc
// comment ("Hooks VALIDATES NOTHING").
func TestValidateStructuralFailures(t *testing.T) {
	installFixtureVocabulary(t)

	deep := uicomponents.Node{Component: "ap:leaf"}
	for i := 0; i < 40; i++ {
		deep = uicomponents.Node{Component: "ap:box", Children: []uicomponents.Node{deep}}
	}

	wide := uicomponents.Node{Component: "ap:box"}
	for i := 0; i < 600; i++ {
		wide.Children = append(wide.Children, uicomponents.Node{Component: "ap:leaf"})
	}

	// pathPrefix, not an exact path: the depth and node-count cases fail at a
	// node several levels down, and pinning their full path would encode the
	// bound's arithmetic into the assertion. Every other case's prefix IS its
	// full path, so one assertion covers both shapes.
	cases := []struct {
		name       string
		in         uicomponents.Declaration
		pathPrefix string
		reason     string
	}{
		{
			name:       "unknown component type: rejected with the offending path",
			in:         decl(node("ap:nope")),
			pathPrefix: "view.children[0]",
			reason:     "unknown component type",
		},
		{
			name:       "children on a leaf component: rejected, not silently ignored",
			in:         decl(node("ap:leaf", node("ap:leaf"))),
			pathPrefix: "view.children[0]",
			reason:     "does not accept children",
		},
		{
			name:       "empty component type: rejected",
			in:         decl(node("")),
			pathPrefix: "view.children[0]",
			reason:     "missing component type",
		},
		{
			name:       "depth beyond MaxDepth: rejected",
			in:         decl(deep),
			reason:     "maximum nesting depth exceeded",
			pathPrefix: "view.children[0].children[",
		},
		{
			name:       "node count beyond MaxNodes: rejected",
			in:         decl(wide),
			reason:     "maximum node count exceeded",
			pathPrefix: "view.children[0].children[",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := uicomponents.Validate(tc.in, uicomponents.DefaultOptions())
			require.Error(t, err, "this declaration must be rejected")

			var ve *uicomponents.ValidationError
			require.ErrorAs(t, err, &ve, "the error must be structured so the agent can correct it")
			assert.Contains(t, ve.Reason, tc.reason)
			assert.True(t, strings.HasPrefix(ve.Path, tc.pathPrefix),
				"path %q must locate the offending node under %q", ve.Path, tc.pathPrefix)
		})
	}
}

// TestValidateAcceptsAnEmptyHook does NOT swap in the fixture vocabulary: an
// empty hook is a real oap:generative node (CompileSlots), and that type is
// part of the PLATFORM vocabulary (registered in components.go's own init()),
// not a stand-in the fixture set needs to reproduce.
func TestValidateAcceptsAnEmptyHook(t *testing.T) {
	in := uicomponents.Declaration{View: uicomponents.CompileSlots(
		[]uicomponents.Slot{{Name: "root", AgentWritable: true}})}
	assert.NoError(t, uicomponents.Validate(in, uicomponents.DefaultOptions()),
		"an agent-writable slot may legitimately ship with no author default")
}

// TestValidateRejectsUnnormalizedSlots pins the fail-closed guard against a
// hand-built Declaration that skipped ParseDeclaration/Normalize: Validate
// must refuse it rather than silently walking a nil View (or worse, a stale
// View left over from a previous normalization alongside a Slots list a
// caller forgot to clear).
func TestValidateRejectsUnnormalizedSlots(t *testing.T) {
	in := uicomponents.Declaration{Slots: []uicomponents.Slot{{Name: "root"}}}
	err := uicomponents.Validate(in, uicomponents.DefaultOptions())
	require.Error(t, err)
	var ve *uicomponents.ValidationError
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "slots", ve.Path)
	assert.Contains(t, ve.Reason, "not normalized")
}

// TestValidateRejectsMissingView pins the OTHER half of the same guard: a
// Declaration with neither View nor Slots is not "an empty page" at Validate
// time — Normalize is what turns "neither" into an empty root stack, and a
// caller that bypassed it gets a named rejection instead of a nil-pointer
// panic on *d.View.
func TestValidateRejectsMissingView(t *testing.T) {
	err := uicomponents.Validate(uicomponents.Declaration{}, uicomponents.DefaultOptions())
	require.Error(t, err)
	var ve *uicomponents.ValidationError
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "view", ve.Path)
	assert.Contains(t, ve.Reason, "missing view")
}

func TestValidationErrorMessageNamesBothPathAndReason(t *testing.T) {
	err := &uicomponents.ValidationError{Path: "slots[0].default", Reason: "unknown component type \"ap:nope\""}
	assert.Equal(t, `slots[0].default: unknown component type "ap:nope"`, err.Error())
}
