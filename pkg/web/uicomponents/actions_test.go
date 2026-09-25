package uicomponents_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// declWithActions is the shared fixture: one granted mutating tool, one
// granted readonly tool, a button naming the mutating action, and a form
// naming the other.
const declWithActions = `{
  "actions":[
    {"name":"advance","tool":"crm_advance_stage","args":{"id":{"$param":"lead"},"note":{"$param":"why"}},"inputs":["why"]},
    {"name":"refresh","tool":"crm_list_leads"}
  ],
  "slots":[{"name":"root","default":{"component":"ap:stack","children":[
    {"component":"ap:select","props":{"param":"lead","value":"l-1"}},
    {"component":"ap:button","props":{"label":"Advance","action":"advance"}},
    {"component":"ap:form","props":{"action":"advance","fields":[{"name":"why"}]}}
  ]}}]}`

func opts(t *testing.T) uicomponents.Options {
	t.Helper()
	o := uicomponents.DefaultOptions()
	o.GrantedTools = map[string]bool{"crm_advance_stage": true, "crm_list_leads": true}
	// Deliberately narrower than GrantedTools: an ACTION must validate against
	// the grant alone. If validateActions ever consults ReadonlyTools, this
	// fixture turns red.
	o.ReadonlyTools = map[string]bool{"crm_list_leads": true}
	return o
}

func TestActionsValidate(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string // "" means must validate clean
	}{
		{
			name: "a side-effecting tool IS a legal action — an action is never readonly-gated",
			raw:  declWithActions,
		},
		{
			name:    "an action naming an ungranted tool is rejected",
			raw:     `{"actions":[{"name":"a","tool":"not_granted"}],"slots":[]}`,
			wantErr: "not granted",
		},
		{
			name:    "a duplicate action name is rejected",
			raw:     `{"actions":[{"name":"a","tool":"crm_list_leads"},{"name":"a","tool":"crm_advance_stage"}],"slots":[]}`,
			wantErr: "duplicate action name",
		},
		{
			name:    "an action with no name is rejected",
			raw:     `{"actions":[{"name":"","tool":"crm_list_leads"}],"slots":[]}`,
			wantErr: "missing action name",
		},
		{
			// An action that neither calls nor says anything is a control that
			// does nothing when pressed — worse than a document that refuses
			// to load, because it looks like it works.
			name:    "an action that neither calls a tool nor sends a prompt is rejected",
			raw:     `{"actions":[{"name":"a","tool":""}],"slots":[]}`,
			wantErr: "either a tool to call or a prompt to send",
		},
		{
			// Both would give one control two outcomes and two failure modes,
			// with no answer for what its pending state means.
			name:    "an action declaring BOTH a tool and a prompt is rejected",
			raw:     `{"actions":[{"name":"a","tool":"crm_list_leads","prompt":"summarize this"}],"slots":[]}`,
			wantErr: "must declare exactly one",
		},
		{
			// A prompt action reaches no tool, so there is no grant to check —
			// its message is one the viewer could have typed themselves.
			// Requiring a grant would be asking permission for something it
			// does not do.
			name:    "a prompt action needs no tool grant",
			raw:     `{"actions":[{"name":"a","prompt":"summarize what is on screen"}],"slots":[]}`,
			wantErr: "",
		},
		{
			// Unfilled, a placeholder reaches the agent as the literal text
			// "{nope}" — a question about nothing, answered confidently. The
			// document is the last place it is still obviously a mistake.
			name:    "a prompt naming a value nothing supplies is rejected",
			raw:     `{"actions":[{"name":"a","prompt":"tell me about {nope}"}],"slots":[]}`,
			wantErr: "prompt references a value nothing supplies",
		},
		{
			name:    "a prompt may name one of the action's own declared inputs",
			raw:     `{"actions":[{"name":"a","prompt":"tell me about {company}","inputs":["company"]}],"slots":[]}`,
			wantErr: "",
		},
		{
			name: "a control naming an undeclared action is rejected",
			raw: `{"actions":[],"slots":[{"name":"root","default":
			       {"component":"ap:button","props":{"action":"nope"}}}]}`,
			wantErr: "undeclared action",
		},
		{
			name: "an input colliding with a declared binding parameter is rejected",
			raw: `{"actions":[{"name":"a","tool":"crm_list_leads","inputs":["span"]}],
			       "slots":[{"name":"root","default":{"component":"ap:select","props":{"param":"span"}}}]}`,
			wantErr: "collides with a binding parameter",
		},
		// The disjointness domain is ParamKEYS, not ParamNAMES. An
		// ap:daterange declaring "window" drives "window.from"/"window.to" and
		// never "window" itself, and the actions route merges the filtered
		// params (keyed by ParamKeys) with the filtered inputs into ONE values
		// map — so these two cases are exactly inverted from what validating
		// against ParamNames produced.
		{
			name: "an input colliding with an EXPANDED parameter key is rejected",
			raw: `{"actions":[{"name":"a","tool":"crm_list_leads","inputs":["window.from"]}],
			       "slots":[{"name":"root","default":{"component":"ap:daterange","props":{"param":"window"}}}]}`,
			wantErr: "collides with a binding parameter",
		},
		{
			name: "an input named for a parameter that expands away is LEGAL — that key never exists",
			raw: `{"actions":[{"name":"a","tool":"crm_list_leads","inputs":["window"]}],
			       "slots":[{"name":"root","default":{"component":"ap:daterange","props":{"param":"window"}}}]}`,
		},
		// A control's own inputs must be a subset of the action's Inputs
		// allowlist. Without this the mismatch is invisible until a viewer
		// submits, at which point the actions route 400s the whole request and
		// tells them to reload the page — which can never help.
		{
			name: "a form field the named action does not declare is rejected",
			raw: `{"actions":[{"name":"a","tool":"crm_list_leads","inputs":["title"]}],
			       "slots":[{"name":"root","default":{"component":"ap:form","props":{"action":"a",
			         "fields":[{"name":"title"},{"name":"body"}]}}}]}`,
			wantErr: `control supplies the input "body", which action "a" does not declare (declared: title)`,
		},
		{
			name: "a form against an action declaring NO inputs names the empty set in its error",
			raw: `{"actions":[{"name":"a","tool":"crm_list_leads"}],
			       "slots":[{"name":"root","default":{"component":"ap:form","props":{"action":"a",
			         "fields":[{"name":"title"}]}}}]}`,
			wantErr: "(declared: none)",
		},
		{
			name: "a form whose fields are all declared validates clean",
			raw: `{"actions":[{"name":"a","tool":"crm_list_leads","inputs":["title","body"]}],
			       "slots":[{"name":"root","default":{"component":"ap:form","props":{"action":"a",
			         "fields":[{"name":"title"},{"name":"body"}]}}}]}`,
		},
		{
			name: "a form field with no name is rejected",
			raw: `{"actions":[{"name":"a","tool":"crm_list_leads","inputs":["title"]}],
			       "slots":[{"name":"root","default":{"component":"ap:form","props":{"action":"a",
			         "fields":[{"name":""}]}}}]}`,
			wantErr: "control declares an input with no name",
		},
		{
			name: "an ap:button supplies no inputs of its own, whatever the action declares",
			raw: `{"actions":[{"name":"a","tool":"crm_list_leads","inputs":["title"]}],
			       "slots":[{"name":"root","default":{"component":"ap:button","props":{"action":"a"}}}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decl, err := uicomponents.ParseDeclaration([]byte(tc.raw))
			require.NoError(t, err, "fixture must parse")
			err = uicomponents.Validate(decl, opts(t))
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestActionsFailClosedOnNilGrant(t *testing.T) {
	decl, err := uicomponents.ParseDeclaration([]byte(declWithActions))
	require.NoError(t, err)
	o := uicomponents.DefaultOptions() // GrantedTools nil
	assert.Error(t, uicomponents.Validate(decl, o),
		"a nil GrantedTools map must grant zero actions, never all of them")
}

func TestActionRefsResolveThroughTheRegistry(t *testing.T) {
	decl, err := uicomponents.ParseDeclaration([]byte(declWithActions))
	require.NoError(t, err)
	refs := uicomponents.ActionRefs(decl)
	require.Len(t, refs, 2, "the button and the form each name one action")
	assert.Equal(t, "advance", refs[0].Action)
	// "root" is not agentWritable, so its content sits outside any hook: region "".
	assert.Equal(t, "", refs[0].Region)
	assert.Equal(t, "advance", refs[1].Action)

	// Inputs are resolved through the component's registered InputNamesProp,
	// so ap:button (which declares none) contributes nothing and ap:form
	// contributes its field names.
	assert.Empty(t, refs[0].Inputs, "an ap:button supplies no values of its own")
	assert.Equal(t, []string{"why"}, refs[1].Inputs, "an ap:form's field names ARE its inputs")
}

// TestActionRefsRegionAppliesAtTheViewRoot is ActionRefs' half of the same
// root-hook gap TestWalkBindingsRegionAppliesAtTheViewRoot pins for bindings:
// a view whose OWN root is the hook must attribute a control several levels
// under it to that hook's region, not "".
func TestActionRefsRegionAppliesAtTheViewRoot(t *testing.T) {
	raw := []byte(`{"actions":[{"name":"go","tool":"crm_list_leads"}],
	  "view":{"component":"oap:generative","props":{"name":"page","allowedComponents":["*"]},"children":[
	    {"component":"ap:button","props":{"label":"Go","action":"go"}}
	  ]}}`)
	decl, err := uicomponents.ParseDeclaration(raw)
	require.NoError(t, err)

	refs := uicomponents.ActionRefs(decl)
	require.Len(t, refs, 1)
	assert.Equal(t, "go", refs[0].Action)
	assert.Equal(t, "page", refs[0].Region)
	assert.Equal(t, "page/0", refs[0].Path)
}

// TestActionRefsIncludeANoticesButtons pins the ActionListProp seam: a
// component whose actions live in a LIST prop contributes one ref per entry,
// at the node's own path, so validateActions refuses an undeclared button the
// same way it refuses an undeclared ap:button.
func TestActionRefsIncludeANoticesButtons(t *testing.T) {
	raw := []byte(`{"actions":[{"name":"test_done","prompt":"Done."},{"name":"stop","prompt":"Stop."}],
	  "view":{"component":"oap:generative","props":{"name":"testRun","allowedComponents":["*"]},"children":[
	    {"component":"ap:notice","props":{"body":"Started.","buttons":[{"label":"Done","action":"test_done"},{"label":"Stop","action":"stop"}]}}
	  ]}}`)
	decl, err := uicomponents.ParseDeclaration(raw)
	require.NoError(t, err)

	refs := uicomponents.ActionRefs(decl)
	require.Len(t, refs, 2, "one ref per button")
	assert.Equal(t, "test_done", refs[0].Action)
	assert.Equal(t, "stop", refs[1].Action)
	assert.Equal(t, "testRun/0", refs[0].Path)
	assert.Equal(t, "testRun/0", refs[1].Path, "both buttons sit on the same node")
	assert.Empty(t, refs[0].Inputs, "a notice button supplies no inputs")
}

func TestActionLookup(t *testing.T) {
	decl, err := uicomponents.ParseDeclaration([]byte(declWithActions))
	require.NoError(t, err)

	a, ok := decl.Action("advance")
	require.True(t, ok)
	assert.Equal(t, "crm_advance_stage", a.Tool)
	assert.Equal(t, []string{"why"}, a.Inputs)
	assert.JSONEq(t, `{"id":{"$param":"lead"},"note":{"$param":"why"}}`, string(a.Args))

	_, ok = decl.Action("nope")
	assert.False(t, ok, "an unknown action name must fail closed, never fall back")

	_, ok = decl.Action("")
	assert.False(t, ok, "the empty action name must never resolve")
}

// TestActionsGrantCheckNormalizesToolName pins that the SAME rule a data
// binding's grant check follows (Options.NormalizeToolName's doc comment,
// validateBindings) applies to an action's tool grant check too: the ref is
// normalized before comparison, never compared raw. Without this test, a
// validateActions that indexed o.GrantedTools[a.Tool] directly (skipping
// NormalizeToolName) would still pass every case in TestActionsValidate,
// because that table's tool names already match their grant keys verbatim.
func TestActionsGrantCheckNormalizesToolName(t *testing.T) {
	raw := `{"actions":[{"name":"a","tool":"crm_advanceStage"}],"slots":[]}`
	decl, err := uicomponents.ParseDeclaration([]byte(raw))
	require.NoError(t, err)

	o := uicomponents.DefaultOptions()
	o.GrantedTools = map[string]bool{"crm_advancestage": true}
	o.NormalizeToolName = strings.ToLower
	assert.NoError(t, uicomponents.Validate(decl, o),
		"the grant check must normalize the action's tool ref before comparing, exactly like a data binding's")

	o.NormalizeToolName = nil
	err = uicomponents.Validate(decl, o)
	require.Error(t, err, "without normalization the camelCase ref must miss the lowercase grant")
	assert.Contains(t, err.Error(), "not granted")
}

// actionArgsFixture builds a one-action declaration whose ap:form's fields
// mirror inputs exactly — the existing I2 rule (a control's declared inputs
// must be a SUBSET of the action's own Inputs, enforced by the ActionRefs
// loop in validateActions) rejects any row whose form doesn't match, before
// the row ever reaches the placeholder check under test. The shared shape
// under every row: an ap:daterange named "window" (keys "window.from",
// "window.to") and an ap:select named "stage" (key "stage"), so
// TestActionArgsPlaceholdersMustResolve's rows can reference either.
func actionArgsFixture(args string, inputs []string) string {
	fields := make([]string, len(inputs))
	for i, in := range inputs {
		fields[i] = fmt.Sprintf(`{"name":%q}`, in)
	}
	inputsJSON, err := json.Marshal(inputs)
	if err != nil {
		panic(err) // inputs is a []string literal from the test table; this cannot fail
	}
	return fmt.Sprintf(`{
	  "actions":[{"name":"advance","tool":"crm_advance_stage","args":%s,"inputs":%s}],
	  "slots":[{"name":"root","default":{"component":"ap:stack","children":[
	    {"component":"ap:daterange","props":{"param":"window"}},
	    {"component":"ap:select","props":{"param":"stage"}},
	    {"component":"ap:form","props":{"action":"advance","fields":[%s]}}
	  ]}}]}`, args, string(inputsJSON), strings.Join(fields, ","))
}

// TestActionArgsFixtureHelperProducesAnOtherwiseValidDeclaration pins the
// fixture helper's OWN validity, independent of Gate B: the shape most
// likely to sink a table row before it ever reaches the placeholder check is
// the form/Inputs mismatch (I2), not the check under test. Isolating it here
// means a table row failure is never misattributed to a broken fixture.
func TestActionArgsFixtureHelperProducesAnOtherwiseValidDeclaration(t *testing.T) {
	raw := actionArgsFixture(`{"stage":{"$param":"stage"}}`, nil)
	decl, err := uicomponents.ParseDeclaration([]byte(raw))
	require.NoError(t, err, "fixture must parse")
	o := opts(t)
	o.GrantedTools = map[string]bool{"crm_advance_stage": true}
	assert.NoError(t, uicomponents.Validate(decl, o),
		"the fixture helper itself must produce an otherwise-valid declaration")
}

// TestActionArgsPlaceholdersMustResolve is Gate B: an action's args
// placeholders must resolve against the declaration's ParamKeys union that
// action's own Inputs — the SAME union pkg/web/webui/agentui's actionsHandler
// merges (its filtered params plus its filtered inputs) into one values map
// before uibindings.SubstituteParams fills the template. Before this gate,
// {"leadId":{"$param":"leed"}} validated clean, rendered live, and 400'd on
// every submit with advice aimed at a viewer who cannot fix an author's typo.
func TestActionArgsPlaceholdersMustResolve(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		inputs  []string
		wantErr string // "" means must validate clean
	}{
		{name: "a param key satisfies a placeholder", args: `{"stage":{"$param":"stage"}}`},
		{name: "an expanded daterange key satisfies a placeholder", args: `{"from":{"$param":"window.from"}}`},
		{name: "the action's own input satisfies a placeholder", args: `{"note":{"$param":"note"}}`, inputs: []string{"note"}},
		{name: "a typo'd param name is rejected at the author", args: `{"stage":{"$param":"stag"}}`, wantErr: `"stag"`},
		{name: "a daterange's DECLARED name is not a key and is rejected", args: `{"from":{"$param":"window"}}`, wantErr: `"window"`},
		{name: "an input another action declares does not satisfy this one", args: `{"note":{"$param":"note"}}`, wantErr: `"note"`},
		{name: "a placeholder nested inside an array is still checked", args: `{"ids":[{"$param":"nope"}]}`, wantErr: `"nope"`},
		{name: "literal args with no placeholder validate", args: `{"limit":"25"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := actionArgsFixture(tc.args, tc.inputs)
			decl, err := uicomponents.ParseDeclaration([]byte(raw))
			require.NoError(t, err, "fixture must parse")
			err = uicomponents.Validate(decl, opts(t))
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestActionSourceBinding covers Node.Bindings' "action" source: a prop MAY
// reference a declared action to read something ABOUT it, but this is not an
// invocation path — the action's tool is never called by resolving a
// binding, only by a viewer clicking the control that NAMES it (ActionProp),
// re-authorized per click in the runner. args is always empty because there
// is nothing to parameterize about "what state is this action in".
func TestActionSourceBinding(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{
			name: "a data binding may reference a declared action with no args",
			raw: `{"actions":[{"name":"advance","tool":"crm_advance_stage"}],
			       "slots":[{"name":"root","default":{"component":"ap:button",
			       "props":{"label":"Advance"},
			       "bindings":{"disabled":{"source":"action","ref":"advance"}}}}]}`,
		},
		{
			name: "an action binding naming an undeclared action is rejected",
			raw: `{"actions":[],"slots":[{"name":"root","default":{"component":"ap:button",
			       "props":{"label":"Advance"},
			       "bindings":{"disabled":{"source":"action","ref":"nope"}}}}]}`,
			wantErr: "undeclared action",
		},
		{
			name: "an action binding carrying args is rejected",
			raw: `{"actions":[{"name":"advance","tool":"crm_advance_stage"}],
			       "slots":[{"name":"root","default":{"component":"ap:button",
			       "props":{"label":"Advance"},
			       "bindings":{"disabled":{"source":"action","ref":"advance","args":{"x":1}}}}}]}`,
			wantErr: "must not carry args",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decl, err := uicomponents.ParseDeclaration([]byte(tc.raw))
			require.NoError(t, err, "fixture must parse")
			o := uicomponents.DefaultOptions()
			o.GrantedTools = map[string]bool{"crm_advance_stage": true}
			err = uicomponents.Validate(decl, o)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
