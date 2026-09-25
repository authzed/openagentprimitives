package builderbundle_test

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/builderbundle"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
)

// workshopAgentUI extracts the typed agent-builder-workshop AgentUI from the
// assembled builder bundle.
func workshopAgentUI(t *testing.T) *v1alpha1.AgentUI {
	t.Helper()
	b, err := builderbundle.Bundle()
	require.NoError(t, err)
	crs, err := b.CRs()
	require.NoError(t, err)
	for _, u := range crs {
		if u.GetKind() != "AgentUI" {
			continue
		}
		var ui v1alpha1.AgentUI
		require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &ui))
		return &ui
	}
	t.Fatal("no AgentUI CR in the builder bundle")
	return nil
}

// builderAgentClass extracts the typed agent-builder AgentClass from the bundle.
func builderAgentClass(t *testing.T) *v1alpha1.AgentClass {
	t.Helper()
	b, err := builderbundle.Bundle()
	require.NoError(t, err)
	crs, err := b.CRs()
	require.NoError(t, err)
	for _, u := range crs {
		if u.GetKind() != "AgentClass" {
			continue
		}
		var ac v1alpha1.AgentClass
		require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &ac))
		return &ac
	}
	t.Fatal("no AgentClass CR in the builder bundle")
	return nil
}

// builderPageView decodes the workshop AgentUI's spec.view into the root
// uicomponents.Node, the shape the tests below walk directly.
func builderPageView(t *testing.T) *uicomponents.Node {
	t.Helper()
	ui := workshopAgentUI(t)
	decl, err := uiview.DeclarationFromSpec(ui)
	require.NoError(t, err)
	require.NotNil(t, decl.View)
	return decl.View
}

// collectComponent appends every node of the given component type reachable
// from n, in document order.
func collectComponent(n *uicomponents.Node, component string, out *[]uicomponents.Node) {
	if n == nil {
		return
	}
	if n.Component == component {
		*out = append(*out, *n)
	}
	for i := range n.Children {
		collectComponent(&n.Children[i], component, out)
	}
}

// TestBuilderPage_RailRootAndSummaries pins the layout the builder page
// declares and that every step carries the one-line summary the rail shows
// before the builder has said anything.
func TestBuilderPage_RailRootAndSummaries(t *testing.T) {
	view := builderPageView(t)
	require.Equal(t, uicomponents.PageType, view.Component)
	var layout string
	require.NoError(t, json.Unmarshal(view.Props["layout"], &layout))
	assert.Equal(t, uicomponents.PageLayoutRail, layout)

	var timelines []uicomponents.Node
	collectComponent(view, "ap:steps", &timelines)
	require.Len(t, timelines, 1)
	var steps []uicomponents.Step
	require.NoError(t, json.Unmarshal(timelines[0].Props["steps"], &steps))
	require.Len(t, steps, 6)
	for _, st := range steps {
		assert.NotEmpty(t, st.Summary, "step %q carries a default summary", st.ID)
		assert.LessOrEqual(t, utf8.RuneCountInString(st.Summary), uicomponents.MaxStepSummaryLen)
	}
}

// The load-bearing prompt-backed invariant: the whole page validates with NO
// granted tools. DefaultOptions() grants none, and validateActions fail-closes
// on an empty GrantedTools for any tool action — so a green here PROVES the page
// declares no tool action and no tool binding. Adding a tool-backed element
// later turns this red.
func TestWorkshopPage_ValidatesWithZeroGrantedTools(t *testing.T) {
	ui := workshopAgentUI(t)
	decl, err := uiview.DeclarationFromSpec(ui)
	require.NoError(t, err, "the workshop page spec must convert to a declaration")
	require.NoError(t, uicomponents.Validate(decl, uicomponents.DefaultOptions()),
		"the workshop page must validate with NO granted tools — it is fully prompt-backed")
}

func TestWorkshopPage_ActionsAreAllPromptBacked(t *testing.T) {
	ui := workshopAgentUI(t)
	assert.Empty(t, ui.Spec.Tools, "prompt-backed: the page requests no tools")
	names := map[string]bool{}
	for _, a := range ui.Spec.Actions {
		assert.NotEmpty(t, a.Prompt, "action %q must be a Prompt action", a.Name)
		assert.Empty(t, a.Tool, "action %q must declare no tool (prompt-backed)", a.Name)
		names[a.Name] = true
	}
	for _, want := range []string{"describe_agent", "edit_description", "test_done", "stop_test", "thats_not_right", "keep_watching", "start_fresh_test", "install_for_real", "hand_off", "test_again"} {
		assert.True(t, names[want], "missing declared action %q", want)
	}
}

// TestBuilderActions_DescribeAndEdit pins the two actions that drive the
// merged agent hook: describe_agent seeds it from the person's first
// description, and edit_description brings the same form back — prefilled,
// via Task 1's ap:form values — so amending never means retyping. Neither
// prompt may still speak of the hooks this page retired.
func TestBuilderActions_DescribeAndEdit(t *testing.T) {
	ui := workshopAgentUI(t)
	byName := map[string]v1alpha1.AgentUIAction{}
	for _, a := range ui.Spec.Actions {
		byName[a.Name] = a
	}
	d, ok := byName["describe_agent"]
	require.True(t, ok)
	assert.Equal(t, []string{"description"}, d.Inputs)
	assert.Contains(t, d.Prompt, "`agent` hook")
	assert.Contains(t, d.Prompt, "`questions` hook")
	assert.NotContains(t, d.Prompt, "`intake`")
	assert.NotContains(t, d.Prompt, "`brief`")

	e, ok := byName["edit_description"]
	require.True(t, ok)
	assert.Empty(t, e.Inputs)
	assert.Contains(t, e.Prompt, "values")
}

// hookIntents returns each of the page's oap:generative hooks' Intent field,
// keyed by hook name. Used to pin what the person or the model could read
// off the page, independent of what any default child paints.
func hookIntents(t *testing.T, ui *v1alpha1.AgentUI) map[string]string {
	t.Helper()
	decl, err := uiview.DeclarationFromSpec(ui)
	require.NoError(t, err)
	out := map[string]string{}
	for _, h := range uicomponents.Hooks(decl) {
		out[h.Name] = h.Intent
	}
	return out
}

// No declared action, action prompt, or hook intent may invite the person to
// hand over a credential: agents never ask for one, on the page or in chat.
func TestWorkshopPage_NothingAsksForACredential(t *testing.T) {
	ui := workshopAgentUI(t)
	for _, a := range ui.Spec.Actions {
		assert.NotContains(t, strings.ToLower(a.Name), "connect")
		for _, word := range []string{"connect", "api key", "token", "password", "paste"} {
			assert.NotContains(t, strings.ToLower(a.Prompt), word, "action %s", a.Name)
		}
	}
	intents := hookIntents(t, ui)
	require.NotEmpty(t, intents, "the page must declare at least one hook, or this loop checks nothing")
	for name, intent := range intents {
		for _, word := range []string{"connect", "api key", "token", "password"} {
			assert.NotContains(t, strings.ToLower(intent), word, "hook %s", name)
		}
	}
}

// TestWorkshopPage_AgentIntentNamesTheEditControls pins the standing
// instruction the builder repaints the merged hook from: the card's two
// headings, the Edit button's action, and — because an AgentUI prompt action
// is delivered as a viewer message, so Edit is a turn like any other — the
// form state the intent must admit, `values`. Without that last word the
// intent describes only the settled card, and a builder obeying it repaints
// over a form the person is still typing in.
func TestWorkshopPage_AgentIntentNamesTheEditControls(t *testing.T) {
	ui := workshopAgentUI(t)
	intent := hookIntents(t, ui)["agent"]
	for _, want := range []string{"edit_description", "In your words", "What we've settled", "values", "no title"} {
		assert.Contains(t, intent, want)
	}
	declared := map[string]bool{}
	for _, a := range ui.Spec.Actions {
		declared[a.Name] = true
	}
	assert.True(t, declared["edit_description"], "the intent names edit_description; the page must declare it or the Edit button 400s")
}

func TestWorkshopPage_TestRunIntentNamesTheTryControls(t *testing.T) {
	intent := hookIntents(t, workshopAgentUI(t))["testRun"]
	for _, want := range []string{
		"ap:agentlink", "test_link", "test_done", "thats_not_right", "stop_test", "ap:notice",
		"ap:status", "ap:chat", "keep_watching", "start_fresh_test", "`since`",
	} {
		assert.Contains(t, intent, want)
	}
}

func TestWorkshopPage_DeliverIntentNamesTheAttachment(t *testing.T) {
	intent := hookIntents(t, workshopAgentUI(t))["deliver"]
	for _, want := range []string{"ap:attachment", "artifactId", "install_for_real", "hand_off", "test_again"} {
		assert.Contains(t, intent, want)
	}
}

// TestWorkshopPage_DeclaresTheSevenHooksInOrder pins the page's contract with
// the 6b skills: the hook names each skill repaints, in page order, each with
// an intent (the agent's standing instruction — plan 3 prints it beside the
// hook) and the allowlist the spec's §11 gives it. The four late hooks carry
// NO default, so they do not exist on screen until the builder reaches them.
func TestWorkshopPage_DeclaresTheSevenHooksInOrder(t *testing.T) {
	ui := workshopAgentUI(t)
	assert.Empty(t, ui.Spec.Slots, "the page is a tree (spec.view), not the legacy slots list")
	decl, err := uiview.DeclarationFromSpec(ui)
	require.NoError(t, err)
	hooks := uicomponents.Hooks(decl)

	var names []string
	for _, h := range hooks {
		names = append(names, h.Name)
		assert.NotEmpty(t, h.Intent, "hook %q needs an intent: it is the instruction the agent is given for the region", h.Name)
		assert.Len(t, h.Path, 1, "hook %q is a direct child of the page: the stage filters only the page's direct children", h.Name)
	}
	assert.Equal(t, []string{"phase", "questions", "agent", "tools", "permissions", "testRun", "deliver"}, names)

	allowed := map[string][]string{}
	hasDefault := map[string]bool{}
	paths := map[string][]int{}
	for _, h := range hooks {
		allowed[h.Name] = h.AllowedComponents
		paths[h.Name] = h.Path
		node := decl.View
		for _, i := range h.Path {
			node = &node.Children[i]
		}
		hasDefault[h.Name] = len(node.Children) > 0
	}
	assert.Len(t, paths["questions"], 1, "questions is not wrapped: an empty card would show before the agent asks")
	assert.Equal(t, []string{"ap:steps"}, allowed["phase"])
	assert.Equal(t, []string{"*"}, allowed["agent"])
	assert.Equal(t, []string{"ap:question", "ap:markdown"}, allowed["questions"])
	for _, late := range []string{"tools", "permissions", "testRun", "deliver"} {
		assert.Equal(t, []string{"*"}, allowed[late])
		assert.False(t, hasDefault[late], "hook %q must have no default: it does not exist on screen until reached", late)
	}
	assert.True(t, hasDefault["phase"], "the timeline paints before the builder speaks")
	assert.True(t, hasDefault["agent"], "the description form paints before the builder speaks")
	assert.False(t, hasDefault["questions"], "nothing to ask until the builder asks")
}

// TestWorkshopPage_HooksBindToTimelineSteps pins the page's step-binding
// contract: the timeline is pinned and names its steps, and every phase hook
// is bound to the step whose end takes it off the stage — agent with
// assess, so it shows only while Intake is selected, and so on for the
// later hooks. questions carries no step, so it always sits at the top of
// the stage, whichever step is selected.
func TestWorkshopPage_HooksBindToTimelineSteps(t *testing.T) {
	ui := workshopAgentUI(t)
	decl, err := uiview.DeclarationFromSpec(ui)
	require.NoError(t, err)
	hooks := uicomponents.Hooks(decl)

	bound := map[string]string{}
	for _, h := range hooks {
		bound[h.Name] = h.Step
	}
	assert.Equal(t, map[string]string{
		"phase": "", "questions": "", "agent": "assess", "tools": "tools",
		"permissions": "permissions", "testRun": "test", "deliver": "deliver",
	}, bound)

	titles := map[string]string{}
	for _, h := range hooks {
		titles[h.Name] = h.Title
	}
	assert.Equal(t, map[string]string{"phase": "", "questions": "", "agent": "Your agent", "tools": "", "permissions": "", "testRun": "", "deliver": ""}, titles,
		"one hook shows under the assess step; the questions region carries no title")

	// The timeline sits inside the phase hook as its author default.
	var phase *uicomponents.Node
	for _, h := range hooks {
		if h.Name == "phase" {
			n := decl.View
			for _, i := range h.Path {
				n = &n.Children[i]
			}
			phase = n
		}
	}
	require.NotNil(t, phase)
	require.Len(t, phase.Children, 1)
	tl := phase.Children[0]
	require.Equal(t, "ap:steps", tl.Component)
	var props uicomponents.StepsProps
	require.NoError(t, json.Unmarshal(tl.Props["steps"], &props.Steps))
	require.NoError(t, json.Unmarshal(tl.Props["pinned"], &props.Pinned))
	assert.True(t, props.Pinned, "the timeline stays in view while the page scrolls")
	assert.Contains(t, hookIntents(t, ui)["phase"], "keeping it pinned", "a repaint may not drop the pin: the intent names it")
	var ids []string
	for _, st := range props.Steps {
		ids = append(ids, st.ID)
	}
	assert.Equal(t, []string{"assess", "tools", "permissions", "build", "test", "deliver"}, ids)
}

// TestWorkshopPage_QuestionsHookAdmitsOnlyQuestions pins the runtime asymmetry
// between the two merged-away hooks' successor and the page's other hooks
// (spec §12): questions' allowlist admits only ap:question and ap:markdown,
// so the builder may put one question there or clear it, but may never
// repaint it as an ap:card — that latitude belongs to the agent hook, whose
// allowlist is "*" precisely because it must eventually become a card.
func TestWorkshopPage_QuestionsHookAdmitsOnlyQuestions(t *testing.T) {
	ui := workshopAgentUI(t)
	base, err := uiview.DeclarationFromSpec(ui)
	require.NoError(t, err)

	card := uicomponents.Node{Component: "ap:card", Props: map[string]json.RawMessage{"title": json.RawMessage(`"x"`)}}
	refused := uicomponents.ResolveView(base, []uicomponents.Fragment{{Hook: "questions", Node: &card}}, uicomponents.DefaultOptions())
	require.Len(t, refused.Rejected, 1, "an ap:card fill is outside questions' allowlist")
	assert.Equal(t, "questions", refused.Rejected[0].Hook)
	assert.Contains(t, refused.Rejected[0].Reason, `"ap:card" is not allowed in hook "questions"`)

	question := uicomponents.Node{Component: "ap:question", Props: map[string]json.RawMessage{"prompt": json.RawMessage(`"where does it run?"`)}}
	filled := uicomponents.ResolveView(base, []uicomponents.Fragment{{Hook: "questions", Node: &question}}, uicomponents.DefaultOptions())
	assert.Empty(t, filled.Rejected, "an ap:question fill is inside questions' allowlist")

	cleared := uicomponents.ResolveView(base, []uicomponents.Fragment{{Hook: "questions", Node: nil}}, uicomponents.DefaultOptions())
	assert.Empty(t, cleared.Rejected, "clearing questions once it is answered is always accepted")
}

// TestWorkshopPage_AgentHookAcceptsTheShapeItsIntentInstructs resolves the
// exact card the `agent` hook's intent tells the builder to paint —
// headings, the Edit button, the person's words, the settled facts, and the
// prefilled describe form the intent says to leave there until they submit.
// The intent is prose the builder acts on; this is the proof the runtime
// accepts what it instructs, so a later narrowing of the hook's allowlist,
// of ap:form's own validation, or of a prop name turns red here rather than
// in a live session where the fill is simply refused and nothing changes.
func TestWorkshopPage_AgentHookAcceptsTheShapeItsIntentInstructs(t *testing.T) {
	ui := workshopAgentUI(t)
	base, err := uiview.DeclarationFromSpec(ui)
	require.NoError(t, err)

	props := func(kv map[string]string) map[string]json.RawMessage {
		out := map[string]json.RawMessage{}
		for k, v := range kv {
			out[k] = json.RawMessage(v)
		}
		return out
	}
	card := uicomponents.Node{
		Component: "ap:card",
		Children: []uicomponents.Node{
			{Component: "ap:heading", Props: props(map[string]string{"text": `"In your words"`, "level": `4`})},
			{Component: "ap:button", Props: props(map[string]string{"label": `"Edit"`, "action": `"edit_description"`})},
			{Component: "ap:markdown", Props: props(map[string]string{"body": `"> a haiku agent"`})},
			{Component: "ap:heading", Props: props(map[string]string{"text": `"What we've settled"`, "level": `4`})},
			{Component: "ap:markdown", Props: props(map[string]string{"body": `"- what it does: writes haiku\n- where it runs: — asking you now"`})},
			{Component: "ap:form", Props: props(map[string]string{
				"action": `"describe_agent"`,
				"fields": `[{"name":"description","kind":"textarea","label":"What should this agent do?"}]`,
				"values": `{"description":"a haiku agent"}`,
			})},
		},
	}
	view := uicomponents.ResolveView(base, []uicomponents.Fragment{{Hook: "agent", Node: &card}}, uicomponents.DefaultOptions())
	assert.Empty(t, view.Rejected, "the card the agent hook's intent instructs must resolve")
	assert.Contains(t, view.AgentComposed, "agent", "the fill must land in the agent hook")
}

// The first real consumer of ap:session_view: an agent-written session_view
// fragment must be accepted into testRun (proving the hook's allowlist admits
// the node the test skill writes at test time).
func TestWorkshopPage_TestRunAcceptsSessionView(t *testing.T) {
	ui := workshopAgentUI(t)
	base, err := uiview.DeclarationFromSpec(ui)
	require.NoError(t, err)
	node := uicomponents.Node{
		Component: "ap:session_view",
		Props:     map[string]json.RawMessage{"sessionRef": json.RawMessage(`"workshop-ns/test-child"`)},
	}
	view := uicomponents.ResolveView(base, []uicomponents.Fragment{{Hook: "testRun", Node: &node}}, uicomponents.DefaultOptions())
	require.Empty(t, view.Rejected, "an ap:session_view fragment must be accepted into the testRun hook")
	assert.Contains(t, view.AgentComposed, "testRun", "testRun content must come from the fragment")
}

func TestWorkshopPage_ClassRefMatchesAndGrantsNoTools(t *testing.T) {
	ui := workshopAgentUI(t)
	ac := builderAgentClass(t)
	require.NotNil(t, ac.Spec.AgentUI, "the builder class references an AgentUI")
	assert.Equal(t, ui.Name, ac.Spec.AgentUI.Ref, "class.agentUI.ref names the workshop AgentUI CR")
	assert.Empty(t, ac.Spec.AgentUI.GrantedTools, "prompt-backed: the class grants no tools to the page")
}

// TestBuilderClass_GrantsTheViewTools pins that the builder can actually call
// the tool its skills tell it to call. update_view and read_view are OPT-IN
// capabilities (agentui_views.go: DefaultOn false), so a class that grants
// only agent_builder is never offered them — every "call update_view" in the
// 6b skill bodies would then name a tool the model does not have.
func TestBuilderClass_GrantsTheViewTools(t *testing.T) {
	ac := builderAgentClass(t)
	for _, name := range []string{"update_view", "read_view"} {
		_, ok := ac.Spec.Capabilities[name]
		assert.True(t, ok, "the builder class must grant %q or the workshop page can never be repainted", name)
	}
}

// TestBuilderClassGrantsWhatItsSkillsCall pins the grant the Deliver skill
// depends on: artifact_await, respond_to_user's attached and ap:attachment
// all need the exported draft finalized, and only the artifacts capability
// offers the tool that does that. Opt-in capabilities are asserted through
// the same fold the runner uses, so a default-off capability cannot pass by
// merely being mentioned.
func TestBuilderClassGrantsWhatItsSkillsCall(t *testing.T) {
	ac := builderAgentClass(t)
	for _, name := range []string{"artifacts", "update_view", "read_view"} {
		g, err := agentcaps.GrantOf(ac, name)
		require.NoError(t, err, name)
		assert.True(t, agentcaps.Active(false, g), "the builder must grant %s: its skills call the tools it offers", name)
	}
}

// TestBuilderClassPromptForbidsPausingBetweenPhases pins the class-level
// rule behind TestSkillBodiesRunPhasesBackToBack: the runner's own protocol
// text says agent_work_complete "means this round is done… use it whenever
// you'd naturally pause", which is exactly what a phased builder must not do
// between phases. The class prompt is where that builder-specific rule lives
// (universal rules stay in the runner).
func TestBuilderClassPromptForbidsPausingBetweenPhases(t *testing.T) {
	ac := builderAgentClass(t)
	prompt := strings.Join(strings.Fields(ac.Spec.SystemPrompt.Inline), " ")
	for _, want := range []string{"do not pause between phases", "in the same turn", "agent_work_complete", "Deliver"} {
		assert.Contains(t, prompt, want)
	}
}
