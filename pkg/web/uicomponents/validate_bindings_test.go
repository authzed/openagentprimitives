package uicomponents_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uiselect"
)

func bindingDecl(prop string, b uicomponents.Binding) uicomponents.Declaration {
	return decl(uicomponents.Node{
		Component: "ap:leaf",
		Bindings:  map[string]uicomponents.Binding{prop: b},
	})
}

// grantingOptions builds Options where every named tool is both granted AND
// readonly. The tests in this file exercise the GRANT check, not the
// readonly-vs-mutating distinction (that gets its own table in
// TestValidateBindings_DataBindingReadonlyAndNormalization), so marking every
// granted tool readonly here keeps those tests exercising exactly one thing.
func grantingOptions(tools ...string) uicomponents.Options {
	o := uicomponents.DefaultOptions()
	o.GrantedTools = map[string]bool{}
	o.ReadonlyTools = map[string]bool{}
	for _, t := range tools {
		o.GrantedTools[t] = true
		o.ReadonlyTools[t] = true
	}
	return o
}

func TestValidateBindingFailures(t *testing.T) {
	installFixtureVocabulary(t)

	cases := []struct {
		name   string
		prop   string
		bind   uicomponents.Binding
		opts   uicomponents.Options
		reason string
	}{
		{
			name:   "binding on a non-bindable prop: rejected",
			prop:   "notBindable",
			bind:   uicomponents.Binding{Source: "tool", Ref: "list_things"},
			opts:   grantingOptions("list_things"),
			reason: "prop is not bindable",
		},
		{
			name:   "unknown binding source: rejected",
			prop:   "text",
			bind:   uicomponents.Binding{Source: "http", Ref: "https://example.invalid"},
			opts:   grantingOptions(),
			reason: "unknown binding source",
		},
		{
			name:   "empty ref: rejected",
			prop:   "text",
			bind:   uicomponents.Binding{Source: "tool", Ref: ""},
			opts:   grantingOptions(),
			reason: "missing binding ref",
		},
		{
			name:   "tool binding naming an ungranted tool: rejected",
			prop:   "text",
			bind:   uicomponents.Binding{Source: "tool", Ref: "restart_everything"},
			opts:   grantingOptions("list_things"),
			reason: "tool is not granted to this UI",
		},
		{
			name:   "tool binding with a nil grant map: rejected, fail closed",
			prop:   "text",
			bind:   uicomponents.Binding{Source: "tool", Ref: "list_things"},
			opts:   uicomponents.DefaultOptions(),
			reason: "tool is not granted to this UI",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := uicomponents.Validate(bindingDecl(tc.prop, tc.bind), tc.opts)
			require.Error(t, err)

			var ve *uicomponents.ValidationError
			require.ErrorAs(t, err, &ve)
			assert.Equal(t, "view.children[0]", ve.Path)
			assert.Contains(t, ve.Reason, tc.reason)
		})
	}
}

func TestValidateAcceptsAGrantedToolBinding(t *testing.T) {
	installFixtureVocabulary(t)
	err := uicomponents.Validate(
		bindingDecl("text", uicomponents.Binding{Source: "tool", Ref: "list_things"}),
		grantingOptions("list_things"),
	)
	assert.NoError(t, err)
}

func TestValidateAcceptsMemoryAndArtifactBindingsWithoutAToolGrant(t *testing.T) {
	installFixtureVocabulary(t)
	for _, src := range []string{"memory", "artifact"} {
		t.Run(src+" binding needs no tool grant", func(t *testing.T) {
			err := uicomponents.Validate(
				bindingDecl("text", uicomponents.Binding{Source: src, Ref: "some_ref"}),
				uicomponents.DefaultOptions(),
			)
			assert.NoError(t, err)
		})
	}
}

// TestValidateGrantChecksEveryNodeNotJustTheSlotRoot guards against the check
// being hoisted out of the recursive walk. Every other binding test in this
// file plants its binding at the slot root (slots[0].default), which the
// grant check would still catch even if it only ran once per declaration
// instead of once per node — this test plants the ungranted binding on a
// CHILD instead, so it only passes if validateBindings runs at every depth
// validateNode visits.
func TestValidateGrantChecksEveryNodeNotJustTheSlotRoot(t *testing.T) {
	installFixtureVocabulary(t)
	in := decl(uicomponents.Node{
		Component: "ap:box",
		Children: []uicomponents.Node{{
			Component: "ap:leaf",
			Bindings: map[string]uicomponents.Binding{
				"text": {Source: "tool", Ref: "restart_everything"},
			},
		}},
	})
	err := uicomponents.Validate(in, grantingOptions("list_things"))
	var ve *uicomponents.ValidationError
	require.ErrorAs(t, err, &ve, "a child node's binding must be grant-checked too")
	assert.Equal(t, "view.children[0].children[0]", ve.Path)
	assert.Contains(t, ve.Reason, "tool is not granted to this UI")
}

func TestValidateBindings_DataBindingReadonlyAndNormalization(t *testing.T) {
	decl := func(ref string) uicomponents.Declaration {
		raw := []byte(`{"slots":[{"name":"root","default":{"component":"ap:table",
          "bindings":{"rows":{"source":"tool","ref":"` + ref + `"}}}}]}`)
		d, err := uicomponents.ParseDeclaration(raw)
		require.NoError(t, err)
		return d
	}
	norm := func(s string) string { return strings.ToLower(s) }

	cases := []struct {
		name      string
		ref       string
		granted   map[string]bool
		readonly  map[string]bool
		normalize func(string) string
		wantErr   string
	}{
		{
			name:      "granted and readonly: accepted",
			ref:       "crm_list_leads",
			granted:   map[string]bool{"crm_list_leads": true},
			readonly:  map[string]bool{"crm_list_leads": true},
			normalize: norm,
		},
		{
			name:      "granted but side-effecting: rejected — a write is never a data binding",
			ref:       "crm_advance_stage",
			granted:   map[string]bool{"crm_advance_stage": true},
			readonly:  map[string]bool{},
			normalize: norm,
			wantErr:   "may only read",
		},
		{
			name:      "nil ReadonlyTools rejects every tool data binding",
			ref:       "crm_list_leads",
			granted:   map[string]bool{"crm_list_leads": true},
			readonly:  nil,
			normalize: norm,
			wantErr:   "may only read",
		},
		{
			name:      "a camelCase ref matches a normalized grant through NormalizeToolName",
			ref:       "crm_listLeads",
			granted:   map[string]bool{"crm_listleads": true},
			readonly:  map[string]bool{"crm_listleads": true},
			normalize: norm,
		},
		{
			name:     "with a nil NormalizeToolName a camelCase ref misses the normalized grant (fail-closed)",
			ref:      "crm_listLeads",
			granted:  map[string]bool{"crm_listleads": true},
			readonly: map[string]bool{"crm_listleads": true},
			wantErr:  "is not granted to this UI",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := uicomponents.DefaultOptions()
			opts.GrantedTools = tc.granted
			opts.ReadonlyTools = tc.readonly
			opts.NormalizeToolName = tc.normalize
			err := uicomponents.Validate(decl(tc.ref), opts)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestValidateBindings_NonToolSourcesIgnoreToolGrants(t *testing.T) {
	raw := []byte(`{"slots":[{"name":"root","default":{"component":"ap:markdown",
      "bindings":{"body":{"source":"memory","ref":"note"}}}}]}`)
	d, err := uicomponents.ParseDeclaration(raw)
	require.NoError(t, err)
	opts := uicomponents.DefaultOptions() // no grants at all
	assert.NoError(t, uicomponents.Validate(d, opts),
		"a memory binding is gated by the session scope at resolve time, not by the tool grant")
}

// paramRefOptions grants+marks-readonly the one tool the $param cases bind to.
func paramRefOptions() uicomponents.Options {
	o := uicomponents.DefaultOptions()
	o.GrantedTools = map[string]bool{"list_rows": true}
	o.ReadonlyTools = map[string]bool{"list_rows": true}
	return o
}

// paramRefDecl wraps a control node and a bound ap:table whose args template
// carries argsJSON, using the REAL vocabulary — the fixture vocabulary declares
// no ParamProp, and the registry-driven expansion is exactly what is under test.
func paramRefDecl(t *testing.T, controlJSON, argsJSON string) uicomponents.Declaration {
	t.Helper()
	raw := []byte(`{"slots":[{"name":"root","default":{"component":"ap:stack","children":[
      ` + controlJSON + `,
      {"component":"ap:table","bindings":{"rows":{"source":"tool","ref":"list_rows","args":` + argsJSON + `}}}
    ]}}]}`)
	d, err := uicomponents.ParseDeclaration(raw)
	require.NoError(t, err, "fixture must parse")
	return d
}

// A $param reference no control can ever satisfy is an AUTHOR bug. Until it is
// rejected here it surfaces only as a permanent error card on the viewer's
// page — and no reload and no interaction can produce the missing key, so the
// viewer has no move to make.
func TestValidateRejectsAnUnsatisfiableParamReference(t *testing.T) {
	const selectSpan = `{"component":"ap:select","props":{"param":"span","value":"7d"}}`
	const dateWindow = `{"component":"ap:daterange","props":{"param":"window","from":"2026-01-01","to":"2026-01-31"}}`

	cases := []struct {
		name    string
		control string
		args    string
		wantErr string
	}{
		{
			name:    "a name no control declares: rejected",
			control: selectSpan,
			args:    `{"since":{"$param":"typo"}}`,
			wantErr: `no control declares the binding parameter "typo"`,
		},
		{
			// ap:daterange declaring "window" drives window.from/window.to and
			// NEVER "window": the reference passes the request layer's filter
			// and can still never be satisfied.
			name:    "a compound parameter referenced by its bare declared name: rejected",
			control: dateWindow,
			args:    `{"since":{"$param":"window"}}`,
			wantErr: `no control declares the binding parameter "window"`,
		},
		{
			name:    "a control's own compound keys: accepted",
			control: dateWindow,
			args:    `{"from":{"$param":"window.from"},"to":{"$param":"window.to"}}`,
		},
		{
			name:    "a simple control's key: accepted",
			control: selectSpan,
			args:    `{"since":{"$param":"span"}}`,
		},
		{
			name:    "a reference nested inside an array element: still checked",
			control: selectSpan,
			args:    `{"filters":[{"op":"eq","value":{"$param":"nope"}}]}`,
			wantErr: `no control declares the binding parameter "nope"`,
		},
		{
			name:    "an object carrying $param alongside other keys is ordinary data, not a placeholder",
			control: selectSpan,
			args:    `{"note":{"$param":"span","extra":1}}`,
		},
		{
			name:    "no args at all: accepted",
			control: selectSpan,
			args:    `{}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := uicomponents.Validate(paramRefDecl(t, tc.control, tc.args), paramRefOptions())
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Contains(t, err.Error(), "declared:",
				"the message must name the keys that DO exist, so the author can fix it without reading the registry")
		})
	}
}

func TestValidateAcceptsAWellFormedSelector(t *testing.T) {
	installFixtureVocabulary(t)
	err := uicomponents.Validate(
		bindingDecl("text", uicomponents.Binding{Source: "tool", Ref: "list_things", Select: "results[].properties"}),
		grantingOptions("list_things"),
	)
	assert.NoError(t, err)
}

// TestValidateRejectsAMalformedSelector pins that a rejection is attributable
// to the SELECTOR specifically — not merely "an error was returned", which
// this fixture (an otherwise-legal granted, readonly tool binding) could
// satisfy via five other checks validateBindings performs on the same
// binding. Reason must name both the prop and the word "selector", and
// errors.Is must reach the exact pkg/web/uiselect sentinel the malformed input
// produces.
func TestValidateRejectsAMalformedSelector(t *testing.T) {
	installFixtureVocabulary(t)
	// Fixed, not derived from uiselect.MaxSelectorLen — see select_test.go's
	// TestParseRejects for why a derived count would hide a mutation to the
	// bound itself.
	overLong := strings.Repeat("a", 300)

	cases := []struct {
		name    string
		sel     string
		wantErr error
	}{
		{name: "a doubled dot", sel: "a..b", wantErr: uiselect.ErrSyntax},
		{name: "an index", sel: "a[0]", wantErr: uiselect.ErrSyntax},
		{name: "trailing garbage after []", sel: "a[]b", wantErr: uiselect.ErrSyntax},
		{name: "over MaxSelectorLen", sel: overLong, wantErr: uiselect.ErrTooLong},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := uicomponents.Validate(
				bindingDecl("text", uicomponents.Binding{Source: "tool", Ref: "list_things", Select: tc.sel}),
				grantingOptions("list_things"),
			)
			require.Error(t, err)

			var ve *uicomponents.ValidationError
			require.ErrorAs(t, err, &ve)
			assert.Contains(t, ve.Reason, "selector",
				"the reason must be attributable to the SELECTOR check, not any of the other five checks this fixture could otherwise fail")
			assert.Contains(t, ve.Reason, `"text"`, "the reason must name the offending prop")
			assert.True(t, errors.Is(err, tc.wantErr), "got %v, want it to wrap %v", err, tc.wantErr)
		})
	}
}

// TestValidateAllowsASelectorOnEverySource pins that a selector is legal
// regardless of source — unlike Args, which is a source-specific invocation
// input and is rejected on an action binding. The action row is the one that
// matters: it asserts BOTH facts (selector accepted, args still rejected) in
// the same test, because either alone is consistent with having the rule
// backwards.
func TestValidateAllowsASelectorOnEverySource(t *testing.T) {
	installFixtureVocabulary(t)

	cases := []struct {
		name string
		bind uicomponents.Binding
		opts uicomponents.Options
	}{
		{name: "tool", bind: uicomponents.Binding{Source: "tool", Ref: "list_things", Select: "a"}, opts: grantingOptions("list_things")},
		{name: "memory", bind: uicomponents.Binding{Source: "memory", Ref: "note", Select: "a"}, opts: uicomponents.DefaultOptions()},
		{name: "artifact", bind: uicomponents.Binding{Source: "artifact", Ref: "handle", Select: "a"}, opts: uicomponents.DefaultOptions()},
	}
	for _, tc := range cases {
		t.Run(tc.name+" source: a selector is accepted", func(t *testing.T) {
			assert.NoError(t, uicomponents.Validate(bindingDecl("text", tc.bind), tc.opts))
		})
	}

	// The action source needs a real action table — Options.DeclaredActions
	// is derived by Validate itself from the declaration's own Actions, and
	// cannot be set directly by a caller (see Options.DeclaredActions).
	t.Run("action source: a selector is accepted, and args on the SAME binding is still rejected", func(t *testing.T) {
		selectOnly := []byte(`{"actions":[{"name":"advance","tool":"crm_advance"}],
		  "slots":[{"name":"root","default":{"component":"ap:leaf",
		    "bindings":{"text":{"source":"action","ref":"advance","select":"a"}}}}]}`)
		d, err := uicomponents.ParseDeclaration(selectOnly)
		require.NoError(t, err)
		assert.NoError(t, uicomponents.Validate(d, grantingOptions("crm_advance")),
			"a selector is a projection of the resolved value, not an invocation input — legal on an action binding")

		selectAndArgs := []byte(`{"actions":[{"name":"advance","tool":"crm_advance"}],
		  "slots":[{"name":"root","default":{"component":"ap:leaf",
		    "bindings":{"text":{"source":"action","ref":"advance","args":{"x":1},"select":"a"}}}}]}`)
		d2, err := uicomponents.ParseDeclaration(selectAndArgs)
		require.NoError(t, err)
		err = uicomponents.Validate(d2, grantingOptions("crm_advance"))
		require.Error(t, err, "args on an action binding must still be rejected even though its selector is legal — a selector is not an arg")
		assert.Contains(t, err.Error(), "must not carry args")
	})
}

// TestValidateChecksTheSelectorBeforeTheParamRefs pins the documented
// ordering: a binding wrong in BOTH ways (a malformed selector and an args
// template referencing a $param no control drives) must report the
// SELECTOR — never the param reference. A reordering refactor that ran
// validateParamRefs first would silently change which author-facing error a
// document with both defects produces.
func TestValidateChecksTheSelectorBeforeTheParamRefs(t *testing.T) {
	installFixtureVocabulary(t)
	bind := uicomponents.Binding{
		Source: "tool",
		Ref:    "list_things",
		Select: "a[0]",                                     // malformed: an index, forbidden by the grammar
		Args:   json.RawMessage(`{"x":{"$param":"nope"}}`), // unsatisfiable: no control drives "nope"
	}
	err := uicomponents.Validate(bindingDecl("text", bind), grantingOptions("list_things"))
	require.Error(t, err)

	var ve *uicomponents.ValidationError
	require.ErrorAs(t, err, &ve)
	assert.Contains(t, ve.Reason, "selector", "the selector check must run before validateParamRefs")
	assert.NotContains(t, ve.Reason, "nope", "the param-ref error must never fire — the selector check fails first")
	assert.True(t, errors.Is(err, uiselect.ErrSyntax))
}
