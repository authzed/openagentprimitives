package uicomponents_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// roundtripPage is the fixture both halves of the round trip share: a rail
// page root, every attribute form the grammar admits (string, number,
// boolean, array, object; the null form is covered by the unit tests on each
// side), a bound prop, a pinned timeline with step ids and a step summary, a
// hook bound to a step, a hook with an author default, an empty hook, a hook
// whose default uses a component its allowlist excludes, and a collapsed fold
// holding a notice. The actions table exists only so the button's action
// validates; the JSX carries the view alone.
const roundtripPage = `{"actions":[{"name":"demo_go","prompt":"Go."}],"view":{"component":"oap:page","props":{"layout":"rail"},"children":[
  {"component":"ap:stack","props":{"direction":"vertical","gap":"sm"},"children":[
    {"component":"ap:heading","props":{"text":"Round trip","level":2}},
    {"component":"ap:table","props":{"columns":[{"key":"name","header":"Name"}],"empty":"Nothing yet."},
     "bindings":{"rows":{"source":"memory","ref":"demo_list","args":{"limit":5},"select":"results[]"}}},
    {"component":"ap:button","props":{"label":"Go","action":"demo_go","disabled":true}},
    {"component":"ap:agentlink","props":{"namespace":"ws-demo","agentClass":"demo-agent","label":"Try it as yourself","prompt":"linear issues for PR #1234"}},
    {"component":"ap:collapsible","props":{"title":"Details","collapsed":true},"children":[
      {"component":"ap:notice","props":{"body":"Test session started.","tone":"info","buttons":[{"label":"Done","action":"demo_go"}]}}]},
    {"component":"oap:generative","props":{"name":"phase","intent":"The stage timeline.","allowedComponents":["ap:steps"]},
     "children":[{"component":"ap:steps","props":{"pinned":true,"steps":[{"id":"intake","label":"Intake","state":"active","summary":"in progress"},{"id":"tools","label":"Tools","state":"upcoming"}]}}]},
    {"component":"ap:card","props":{"title":"Your agent so far"},"children":[
      {"component":"oap:generative","props":{"name":"brief","intent":"The running summary.","allowedComponents":["*"],"step":"intake"}}]},
    {"component":"oap:generative","props":{"name":"intake","intent":"Where the person first describes the agent.","allowedComponents":["ap:markdown","ap:form"]},
     "children":[{"component":"ap:card","props":{"title":"Describe it"},"children":[
       {"component":"ap:markdown","props":{"body":"Tell me what it should do — in a sentence or two."}}]}]}
  ]}
]}}`

// TestJSXRoundTripFixture is the Go half of the JSX ⇄ nodes round trip
// (spec §12): the committed testdata/roundtrip.page.tsx is exactly what the
// printer emits for roundtripPage, wrapped as a page, and
// testdata/roundtrip.view.json is exactly its nodes. The TypeScript half
// (web/packages/agentui/src/compile.roundtrip.test.ts) compiles the page
// and requires the same nodes back. Regenerate both with REGEN_GOLDEN=1 —
// and read the diff: a change here is a change to the grammar the compiler
// must accept.
func TestJSXRoundTripFixture(t *testing.T) {
	d, err := uicomponents.ParseDeclaration([]byte(roundtripPage))
	require.NoError(t, err)
	require.NoError(t, uicomponents.Validate(d, uicomponents.DefaultOptions()), "the fixture must be a valid page")

	page := "export default (\n" + indent(uicomponents.JSX(d, nil), "  ") + "\n);\n"
	nodes, err := json.MarshalIndent(d.View, "", "  ")
	require.NoError(t, err)
	nodes = append(nodes, '\n')

	pagePath := filepath.Join("testdata", "roundtrip.page.tsx")
	viewPath := filepath.Join("testdata", "roundtrip.view.json")
	if os.Getenv("REGEN_GOLDEN") == "1" {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(pagePath, []byte(page), 0o644))
		require.NoError(t, os.WriteFile(viewPath, nodes, 0o644))
		t.Logf("regenerated %s and %s", pagePath, viewPath)
	}
	gotPage, err := os.ReadFile(pagePath)
	require.NoError(t, err, "run with REGEN_GOLDEN=1 once to create the fixture pair")
	gotView, err := os.ReadFile(viewPath)
	require.NoError(t, err)
	assert.Equal(t, page, string(gotPage), "the printer's output for the fixture changed; regenerate with REGEN_GOLDEN=1 and read the diff")
	assert.JSONEq(t, string(nodes), string(gotView))
}

// indent prefixes every line of s with prefix — the page wraps the printed
// tree inside `export default ( … );` one level in.
func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}
