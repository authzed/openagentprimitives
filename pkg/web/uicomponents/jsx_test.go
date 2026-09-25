package uicomponents_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// builderPage is the plan's builder-shaped page as nodes: a heading, a
// timeline hook with an author default, a card holding an empty hook, and
// an intake hook whose author default uses a component the AGENT may not
// (ap:card under an allowlist of markdown+form — the accepted asymmetry).
const builderPage = `{"view":{"component":"ap:stack","props":{"direction":"vertical","gap":"sm"},"children":[
  {"component":"ap:heading","props":{"text":"Agent Builder","level":2}},
  {"component":"oap:generative","props":{"name":"phase","intent":"The stage timeline. Advance the active step as you progress.","allowedComponents":["ap:steps"]},
   "children":[{"component":"ap:steps","props":{"steps":[{"label":"Intake","state":"active"},{"label":"Tools","state":"upcoming"}]}}]},
  {"component":"ap:card","props":{"title":"Your agent so far"},"children":[
    {"component":"oap:generative","props":{"name":"brief","intent":"The running summary.","allowedComponents":["*"]}}]},
  {"component":"oap:generative","props":{"name":"intake","intent":"Where the person first describes the agent.","allowedComponents":["ap:markdown","ap:form"]},
   "children":[{"component":"ap:card","props":{"title":"Describe the agent you want to build"},"children":[
     {"component":"ap:markdown","props":{"body":"Tell me what this agent should do."}}]}]}
]}}`

func parsePage(t *testing.T, src string) uicomponents.Declaration {
	t.Helper()
	d, err := uicomponents.ParseDeclaration([]byte(src))
	require.NoError(t, err)
	return d
}

func TestJSXPrintsTheBuilderShapedPage(t *testing.T) {
	want := strings.Join([]string{
		`<ap:stack direction="vertical" gap="sm">`,
		`  <ap:heading level={2} text="Agent Builder" />`,
		`  <oap:generative name="phase" intent="The stage timeline. Advance the active step as you progress." allowedComponents={["ap:steps"]}>`,
		`    <ap:steps steps={[{"label":"Intake","state":"active"},{"label":"Tools","state":"upcoming"}]} />`,
		`  </oap:generative>`,
		`  <ap:card title="Your agent so far">`,
		`    <oap:generative name="brief" intent="The running summary." allowedComponents={["*"]}>`,
		`      {/* empty */}`,
		`    </oap:generative>`,
		`  </ap:card>`,
		`  <oap:generative name="intake" intent="Where the person first describes the agent." allowedComponents={["ap:markdown","ap:form"]}>`,
		`    <ap:card title="Describe the agent you want to build">`,
		`      <ap:markdown body="Tell me what this agent should do." />`,
		`    </ap:card>`,
		`  </oap:generative>`,
		`</ap:stack>`,
	}, "\n")
	assert.Equal(t, want, uicomponents.JSX(parsePage(t, builderPage), nil))
}

func TestJSXMarksTheAgentsOwnFillsAndClears(t *testing.T) {
	d := parsePage(t, builderPage)
	// Simulate ResolveView's outcome by hand: brief filled by the agent,
	// intake cleared by the agent (children gone), phase left as authored.
	brief := &d.View.Children[2].Children[0]
	brief.Children = []uicomponents.Node{{Component: "ap:markdown", Props: map[string]json.RawMessage{"body": json.RawMessage(`"A PR digest bot."`)}}}
	d.View.Children[3].Children = nil

	got := uicomponents.JSX(d, []string{"brief", "intake"})

	assert.Contains(t, got, strings.Join([]string{
		`    <oap:generative name="brief" intent="The running summary." allowedComponents={["*"]}>`,
		`      {/* your fill */}`,
		`      <ap:markdown body="A PR digest bot." />`,
		`    </oap:generative>`,
	}, "\n"), "a hook the agent filled is marked, then shows its content:\n%s", got)
	assert.Contains(t, got, strings.Join([]string{
		`  <oap:generative name="intake" intent="Where the person first describes the agent." allowedComponents={["ap:markdown","ap:form"]}>`,
		`    {/* empty: you cleared this hook */}`,
		`  </oap:generative>`,
	}, "\n"), "a hook the agent cleared says so — distinct from one nobody touched:\n%s", got)
	// The plain "{/* empty */}" marker (an untouched, empty hook) is asserted
	// against builderPage's own unmutated form in
	// TestJSXPrintsTheBuilderShapedPage. This fixture's three hooks are all
	// accounted for above (phase stays authored with content, brief is
	// filled, intake is cleared) — none is both untouched and empty, so there
	// is nothing left here to additionally assert that marker against.
	assert.Equal(t, 1, strings.Count(got, "{/* your fill */}"), "only the agent's hooks are marked")
}

func TestJSXAttributeGrammar(t *testing.T) {
	cases := []struct {
		name string
		prop string // JSON value
		want string // the attribute as printed
	}{
		{"plain string quotes", `"hello world"`, `text="hello world"`},
		{"string with a double quote braces JSON", `"say \"hi\""`, `text={"say \"hi\""}`},
		{"string with an angle bracket braces JSON, unescaped", `"a < b"`, `text={"a < b"}`},
		{"string with a brace braces JSON", `"{x}"`, `text={"{x}"}`},
		{"string with a newline braces JSON", `"one\ntwo"`, `text={"one\ntwo"}`},
		// U+2028 and a C0 control are not in the punctuation set but must not
		// print raw inside the quotes; the JSON encoder escapes both.
		{"string with a line separator braces JSON", "\"a\u2028b\"", `text={"a\u2028b"}`},
		{"string with a C0 control braces JSON", `"a\u0001b"`, `text={"a\u0001b"}`},
		{"number", `2`, `text={2}`},
		{"boolean", `true`, `text={true}`},
		{"null", `null`, `text={null}`},
		{"array", `["a", "b"]`, `text={["a","b"]}`},
		{"object compacts", `{ "k" : [1, 2] }`, `text={{"k":[1,2]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := uicomponents.Declaration{View: &uicomponents.Node{Component: "ap:text", Props: map[string]json.RawMessage{"text": json.RawMessage(tc.prop)}}}
			assert.Equal(t, `<ap:text `+tc.want+` />`, uicomponents.JSX(d, nil))
		})
	}
}

func TestJSXPrintsPropsSortedAndBindingsLast(t *testing.T) {
	d := uicomponents.Declaration{View: &uicomponents.Node{
		Component: "ap:table",
		Props:     map[string]json.RawMessage{"title": json.RawMessage(`"Leads"`), "columns": json.RawMessage(`["name"]`)},
		Bindings: map[string]uicomponents.Binding{
			"rows": {Source: "tool", Ref: "crm_list", Args: json.RawMessage(`{"limit": 5}`), Select: "results[]"},
		},
	}}
	got := uicomponents.JSX(d, nil)
	assert.Equal(t, `<ap:table columns={["name"]} title="Leads" bindings={{"rows":{"source":"tool","ref":"crm_list","args":{"limit":5},"select":"results[]"}}} />`, got)
	// Twice, because map order is random and the output must not be.
	assert.Equal(t, got, uicomponents.JSX(d, nil))
}

func TestJSXOfAnEmptyDeclarationIsEmpty(t *testing.T) {
	assert.Equal(t, "", uicomponents.JSX(uicomponents.Declaration{}, nil))
}

// A hook's step and title are part of its contract, so they print with the
// contract — after allowedComponents, in that order — and the compiler
// reads them back as ordinary attributes.
func TestJSXPrintsAHookStepAndTitleAfterItsContract(t *testing.T) {
	d, err := uicomponents.ParseDeclaration([]byte(`{"view":{"component":"oap:generative","props":{"name":"brief","step":"assess","intent":"The summary.","allowedComponents":["*"],"title":"Brief"}}}`))
	require.NoError(t, err)
	assert.Equal(t, strings.Join([]string{
		`<oap:generative name="brief" intent="The summary." allowedComponents={["*"]} step="assess" title="Brief">`,
		`  {/* empty */}`,
		`</oap:generative>`,
	}, "\n"), uicomponents.JSX(d, nil))
}
