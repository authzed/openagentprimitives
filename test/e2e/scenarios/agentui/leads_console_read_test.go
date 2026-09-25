//go:build e2e

// This file does NOT assert wall-clock latency anywhere. "Sub-second" is a
// browser-side property (useBindings.ts's 250ms debounce plus one direct
// tool round-trip with no LLM in the path) and a wall-clock assertion in an
// e2e suite that already contends for a shared envtest/SpiceDB container
// would be a flake generator, not evidence. The substantive claim — that
// painting the console and re-querying it never invokes the model — is
// asserted as a request-count delta against h.LLM.Requests() instead. See
// TestLeadsConsole_TierZeroPaintsAndReQueries's own doc comment for exactly
// what that delta does and does not establish, and do not "improve" this
// into a time.Since assertion later.
package agentui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uidemo/leadflow"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// wireDeclarationMirror decodes the SERVED bootstrap declaration
// (pkg/web/webui/agentui/viewmodel.go's declarationWire): the page tree plus
// the browser-only agentComposed list nothing in uicomponents.Declaration
// itself carries — a hook's CURRENT content looks identical whether it is the
// author's Tier-0 default or a fragment the agent wrote, so the wire has to
// say which names were actually composed. Actions and View reuse
// uicomponents.Action/Node directly — the wire fields are byte-for-byte those
// types — so only AgentComposed needs a mirror field at all. Duplicated
// rather than imported because declarationWire is unexported in a package
// this test must not depend on for its import graph.
type wireDeclarationMirror struct {
	Actions       []uicomponents.Action `json:"actions,omitempty"`
	View          *uicomponents.Node    `json:"view"`
	AgentComposed []string              `json:"agentComposed,omitempty"`
}

// pageProps mirrors pkg/web/webui/agentui.ViewProps — the agent-defined view's own
// bootstrap, which the session shell nests under its selected view — carrying
// only the fields this test reads.
type pageProps struct {
	Ns          string                `json:"ns"`
	Name        string                `json:"name"`
	Declaration wireDeclarationMirror `json:"declaration"`
}

// decodePageProps decodes b.Page's raw props and additionally rebuilds a
// plain uicomponents.Declaration (dropping the wire-only agentComposed list)
// so uicomponents.WalkBindings/ParamStates/Hooks can walk it — those
// functions are the declaration's own vocabulary, not this test's, and
// re-deriving the expected binding-key set through them (rather than
// hand-listing keys) is what catches a resolver answering under the wrong
// key.
func decodePageProps(t *testing.T, raw json.RawMessage) (pageProps, uicomponents.Declaration) {
	t.Helper()
	var props pageProps
	require.NoError(t, json.Unmarshal(raw, &props), "decode page props")
	decl := uicomponents.Declaration{Actions: props.Declaration.Actions, View: props.Declaration.View}
	return props, decl
}

func pathsOf(bound []uicomponents.BoundProp) []string {
	out := make([]string, len(bound))
	for i, bp := range bound {
		out[i] = bp.Path
	}
	return out
}

func keysOf(m map[string]e2e.BindingResult) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func paramsFrom(states []uicomponents.ParamState) map[string]string {
	out := make(map[string]string, len(states))
	for _, s := range states {
		out[s.Key] = s.Value
	}
	return out
}

// TestLeadsConsole_TierZeroPaintsAndReQueries is spec E2E 13, corrected
// against shipped behaviour per the task brief: "a session starts" does NOT
// mean this test drives a wake — ResolveSession (pkg/web/webui/agentui/session.go)
// is pure and never stamps a wake annotation or creates an AgentSession, so
// this scenario asserts the ladder's Attached branch for a session the
// harness starts by the ordinary channel path (SendUserMessage), not a
// wake it triggers itself.
//
// What "no agent turn" means here, precisely: AgentSessionSpec.Prompt is a
// required field and the runner's cold-start always places it as turn 0 and
// answers it with the LLM BEFORE the session can reach a phase this page
// will serve — there is no such thing as a "live" AgentSession that has
// made zero LLM calls ever. So this test does not assert h.LLM.Requests()
// is empty; it takes a BASELINE count once the session reaches Idle (the
// cold-start turn's own unavoidable call(s)) and asserts that count is
// UNCHANGED after painting the console and after every re-query. That is
// the strongest claim the harness can observe: viewing and re-querying the
// console add ZERO model calls beyond the session's own bootstrap — not
// that the session's lifetime ever had zero LLM calls in it. Every binding
// this test resolves is "tool"-sourced and dispatches through
// runner.Loop.HandleUIDataBinding, which is a sibling entry point to the
// conversational turn loop, never routed through the LLM — see
// pkg/agent/runner/apptoolcall.go's own doc comment on why HandleUIDataBinding
// is "a thin shell... not a second implementation" of the app-tool dispatch
// the mcp-ui widget path already proved LLM-free.
func TestLeadsConsole_TierZeroPaintsAndReQueries(t *testing.T) {
	crmServer, crm := newLeadflowServer(t)
	h := e2e.Start(t, e2e.Options{})
	applyLeadsConsoleFixture(t, h, crm.URL)
	stampAgentUIValidity(t, h, "default", "pipeline-desk-ui", pipelineDeskGrantedTools)
	h.WaitForAgentClassValid("pipeline-desk", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	const viewerEmail = "viewer@demo.invalid"

	// Repeating: the harness cold-start can drive the seed message through
	// the LLM more than once (spec.Prompt replay + an injected inbound —
	// see chat_resume_test.go's identical note), so a non-repeating rule
	// would "no rule matched"-fatal on a replay. agent_work_complete is a
	// reliable single-shot terminal for this fixture: no respond_to_user
	// round-trip needed to reach Idle.
	h.LLM.On(func(llm.Request) bool { return true }).
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "opened the pipeline desk"})).
		Repeating()

	h.SendUserMessage("open the pipeline desk", e2e.AsUser(viewerEmail))
	e2e.WaitForSessionIdle(t, h)

	sess := e2e.FindOnlyAgentSession(t, h, h.Namespace())
	ns := h.Namespace()

	// Baseline: whatever the cold-start turn cost, taken AFTER the session
	// is Idle and BEFORE any browser call — see the doc comment above for
	// why this is a delta, not an absolute-zero assertion.
	baseline := len(h.LLM.Requests())
	require.NotZero(t, baseline, "the cold-start turn must have made at least one LLM call before Idle")

	viewer := e2e.CanonicalForFakeEmail(viewerEmail)
	b := h.AgentUIBrowserFor("user:" + viewer.String())
	ctx := context.Background()

	// --- 1: first paint is complete with no agent turn. -------------------

	propsRaw, err := b.Page(ctx, ns, sess.Name)
	require.NoError(t, err, "the doors ladder must resolve for a live session the viewer may interact with")
	props, decl := decodePageProps(t, propsRaw)
	assert.Equal(t, ns, props.Ns)
	assert.Equal(t, sess.Name, props.Name)
	// The fixture (02-agentui.yaml) declares 5 slots and marks exactly ONE
	// (summary) agentWritable: true; CompileSlots turns only that one into a
	// hook and places the other four directly as root-stack children — so
	// the hook count and the root's child count are two different claims
	// about the same compiled page.
	require.Len(t, uicomponents.Hooks(decl), 1, "exactly one agentWritable slot (summary) compiles to a hook")
	require.NotNil(t, decl.View, "the served declaration must always carry a view")
	assert.Len(t, decl.View.Children, 5, "summary, filters, pipeline, breakdown, advance — one root-stack child per slot")

	// --- 2: response keys are the SERVED declaration's own binding paths. -

	want := pathsOf(uicomponents.WalkBindings(decl))
	defaultParams := paramsFrom(uicomponents.ParamStates(decl))
	require.Len(t, defaultParams, 4, "window.from, window.to, stage, lead")

	got, status, err := b.Bindings(ctx, ns, sess.Name, defaultParams)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	assert.ElementsMatch(t, want, keysOf(got),
		"every result must be keyed by the SERVED declaration's own binding path")
	assert.Contains(t, got, "/2.0#rows",
		"the literal is the second half of the pin — catches a coordinated format change ElementsMatch alone would miss: "+
			"pipeline is not agentWritable (region \"\"), the root stack's 3rd child (index 2), whose rows binding sits "+
			"one level inside the compiled ap:card at child 0")
	for k, r := range got {
		assert.Equal(t, "ok", r.Status, "binding %s did not resolve: %s", k, r.Message)
	}

	assert.Equal(t, baseline, len(h.LLM.Requests()), "Tier 0 must paint with no ADDITIONAL agent turn")

	// --- 3: the chart binding is a chart's worth of data. ------------------

	var stages []leadflow.StageCount
	require.NoError(t, json.Unmarshal(got["/3.0#data"].Value, &stages), "decode /3.0#data")
	assert.Len(t, stages, 4, "one entry per pipeline stage, always — see leadflow.stageBreakdown's own doc comment")

	var rowsWide []leadflow.Lead
	require.NoError(t, json.Unmarshal(got["/2.0#rows"].Value, &rowsWide))
	require.NotEmpty(t, rowsWide, "the default window must show at least one lead for the narrower re-query below to shrink from something")

	// --- 4: a parameter change re-queries with the new window, still with
	// no LLM turn, and the CRM (not just the response) observed the change.

	const narrowerFrom = "2026-07-01T00:00:00Z"
	narrower := make(map[string]string, len(defaultParams))
	for k, v := range defaultParams {
		narrower[k] = v
	}
	narrower["window.from"] = narrowerFrom

	got2, status2, err := b.Bindings(ctx, ns, sess.Name, narrower)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status2)
	for k, r := range got2 {
		assert.Equal(t, "ok", r.Status, "re-query binding %s did not resolve: %s", k, r.Message)
	}

	assert.Equal(t, baseline, len(h.LLM.Requests()), "the re-query must ALSO add no agent turn")

	var rowsNarrow []leadflow.Lead
	require.NoError(t, json.Unmarshal(got2["/2.0#rows"].Value, &rowsNarrow))
	assert.Less(t, len(rowsNarrow), len(rowsWide), "the narrower window's row count must shrink")

	// The half that cannot be faked by a stub returning a canned narrower
	// list: read the CRM's OWN call log, not the response.
	sawNarrowerArgs := false
	for _, c := range crmServer.Calls() {
		if c.Tool != leadflow.ToolListLeads {
			continue
		}
		if from, _ := c.Args["from"].(string); from == narrowerFrom {
			sawNarrowerArgs = true
			break
		}
	}
	assert.True(t, sawNarrowerArgs, "the CRM must have observed a list_leads call carrying the re-query's narrower window.from")

	// --- 5: an undeclared parameter key is a 400, not a silent drop. ------

	_, status3, err := b.Bindings(ctx, ns, sess.Name, map[string]string{"windo.from": "2026-01-01T00:00:00Z"})
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, status3, "an undeclared parameter key must reject the whole request")

	// --- 6: no hook is agentComposed before any update_view. --------------

	assert.Empty(t, props.Declaration.AgentComposed, "no hook must be agentComposed before any update_view write")
}
