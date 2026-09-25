package capability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
)

// classGranting builds an AgentClass whose spec.capabilities grants exactly
// the named capabilities with an empty ({}) config each.
func classGranting(t *testing.T, names ...string) *spiceboxv1alpha1.AgentClass {
	t.Helper()
	if len(names) == 0 {
		return &spiceboxv1alpha1.AgentClass{}
	}
	caps := make(map[string]apiextensionsv1.JSON, len(names))
	for _, n := range names {
		caps[n] = apiextensionsv1.JSON{Raw: []byte(`{}`)}
	}
	return &spiceboxv1alpha1.AgentClass{Spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps}}
}

// sessionFixture is a minimal AgentSession, just enough for logSkip's
// namespace/name disclosure.
func sessionFixture(t *testing.T) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	return &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-session"}}
}

// testLogger is a discarding logr.Logger — these tests assert on the
// assembled tool set, not on log content.
func testLogger(t *testing.T) logr.Logger {
	t.Helper()
	return logr.Discard()
}

// newFakeAgentUIClient builds a controller-runtime fake client seeded with
// objs, scheme-registered for AgentUI — the same construction
// pkg/web/uiview/resolve_test.go's newFakeUIClient uses.
func newFakeAgentUIClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// runtimeFixture is a *uiview.Runtime backed by an AgentUI with one
// agent-writable slot ("panel") and one that is not ("notes") — enough to
// exercise update_view's writable-slot enum and read_view's happy path.
func runtimeFixture(t *testing.T) *uiview.Runtime {
	t.Helper()
	ui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-ui"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "panel", AgentWritable: true},
				{Name: "notes", AgentWritable: false},
			},
		},
	}
	return &uiview.Runtime{
		Namespace: "demo-ns", Session: "demo-session", UIName: "demo-ui",
		Client: newFakeAgentUIClient(t, ui),
		Mem:    memory.NewLocal(inmem.NewBackend()),
	}
}

// runtimeFixtureAllReadOnlySlots is a *uiview.Runtime whose AgentUI declares
// slots but none agentWritable — the shape TestUpdateViewSkipsWhenThePageHasNoHook
// needs: CompileSlots turns a non-writable slot into a plain node, never a
// hook, so this page compiles to zero hooks.
func runtimeFixtureAllReadOnlySlots(t *testing.T) *uiview.Runtime {
	t.Helper()
	ui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-ui"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "notes", AgentWritable: false},
			},
		},
	}
	return &uiview.Runtime{
		Namespace: "demo-ns", Session: "demo-session", UIName: "demo-ui",
		Client: newFakeAgentUIClient(t, ui),
		Mem:    memory.NewLocal(inmem.NewBackend()),
	}
}

// runtimeFixtureWithView is runtimeFixture's Spec.View counterpart: an
// AgentUI declaring the tree shape directly (view) rather than the
// Spec.Slots shim, so a test can name hooks, intents and allowlists exactly.
func runtimeFixtureWithView(t *testing.T, view string) *uiview.Runtime {
	t.Helper()
	ui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-ui"},
		Spec:       spiceboxv1alpha1.AgentUISpec{View: &apiextensionsv1.JSON{Raw: []byte(view)}},
	}
	return &uiview.Runtime{
		Namespace: "demo-ns", Session: "demo-session", UIName: "demo-ui",
		Client: newFakeAgentUIClient(t, ui),
		Mem:    memory.NewLocal(inmem.NewBackend()),
	}
}

// offerWith builds the OfferContext an Offer call needs, with rt as the
// wired UIView — the same shape every Offer call in this file constructs by
// hand, factored out once two tests want exactly this and nothing else.
func offerWith(t *testing.T, rt *uiview.Runtime) OfferContext {
	t.Helper()
	return OfferContext{Ctx: context.Background(), Session: sessionFixture(t), Env: RunnerEnv{UIView: rt}}
}

func TestViewCapabilitiesAreOptInAndDefaultOff(t *testing.T) {
	for _, name := range []string{"update_view", "read_view"} {
		c, ok := Lookup(name)
		require.True(t, ok, "%s must be registered or the AgentClass controller reports it unknown", name)
		assert.False(t, c.DefaultOn(), "%s must never be on without an explicit grant", name)
		assert.False(t, c.Infrastructural(), "%s must be disableable", name)
	}
}

// TestTheTwoGatesAreIndependent is the spec's integration case 10: "the two
// gates are independent: read_view alone grants reads and NOT writes."
func TestTheTwoGatesAreIndependent(t *testing.T) {
	cases := []struct {
		name    string
		granted []string
		want    []string // tools that MUST be present
		absent  []string // tools that MUST NOT be
	}{
		{name: "neither granted: Tier-0 only", granted: nil,
			absent: []string{"update_view", "read_view"}},
		{name: "read_view alone grants reads and NOT writes", granted: []string{"read_view"},
			want: []string{"read_view"}, absent: []string{"update_view"}},
		{name: "update_view alone grants writes and NOT reads", granted: []string{"update_view"},
			want: []string{"update_view"}, absent: []string{"read_view"}},
		{name: "both granted: both offered", granted: []string{"update_view", "read_view"},
			want: []string{"update_view", "read_view"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			names := toolNames(Assemble(context.Background(), AssembleDeps{
				Class:   classGranting(t, tc.granted...),
				Session: sessionFixture(t),
				Env:     RunnerEnv{UIView: runtimeFixture(t)},
				Logger:  testLogger(t),
			}))
			for _, w := range tc.want {
				assert.Contains(t, names, w)
			}
			for _, a := range tc.absent {
				assert.NotContains(t, names, a)
			}
		})
	}
}

func TestViewCapabilitiesSkipWithNoUI(t *testing.T) {
	names := toolNames(Assemble(context.Background(), AssembleDeps{
		Class:   classGranting(t, "update_view", "read_view"),
		Session: sessionFixture(t),
		Env:     RunnerEnv{}, // UIView nil: the class references no AgentUI
		Logger:  testLogger(t),
	}))
	assert.NotContains(t, names, "update_view")
	assert.NotContains(t, names, "read_view")
}

func TestUpdateViewSkipsWhenThePageHasNoHook(t *testing.T) {
	names := toolNames(Assemble(context.Background(), AssembleDeps{
		Class:   classGranting(t, "update_view"),
		Session: sessionFixture(t),
		Env:     RunnerEnv{UIView: runtimeFixtureAllReadOnlySlots(t)},
		Logger:  testLogger(t),
	}))
	assert.NotContains(t, names, "update_view",
		"a hook enum with no members is a tool that can never succeed; the skip is what tells the author their page has nothing agent-writable")
}

// TestUpdateViewOffersTheHookEnumInDocumentOrder asserts the schema's `hook`
// enum lists exactly the page's hooks, in the order Hooks(decl) walks the
// tree — never sorted, never grant-order — because document order is the
// order an agent reads the page description in, and a hook enum in a
// different order would be describing a page that is not this one.
func TestUpdateViewOffersTheHookEnumInDocumentOrder(t *testing.T) {
	rt := runtimeFixtureWithView(t, `{"component":"ap:stack","children":[
	  {"component":"oap:generative","props":{"name":"phase","intent":"timeline","allowedComponents":["ap:steps"]}},
	  {"component":"ap:card","children":[{"component":"oap:generative","props":{"name":"brief","allowedComponents":["*"]}}]}]}`)
	tools, skip := (updateViewCapability{}).Offer(offerWith(t, rt))
	require.Nil(t, skip)
	require.Len(t, tools, 1)
	var schema struct {
		Properties struct {
			Hook struct {
				Enum []string `json:"enum"`
			} `json:"hook"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(tools[0].InputSchema(), &schema))
	assert.Equal(t, []string{"phase", "brief"}, schema.Properties.Hook.Enum)
}

// TestUpdateViewLogsAHookWithoutIntentOnce covers the log the runner emits
// when a page's hook carries no author instruction for the agent: the shim
// (CompileSlots) never sets Intent, so every slots-authored page hits this
// on the ONE Offer call per session — hence "once", not once per turn.
func TestUpdateViewLogsAHookWithoutIntentOnce(t *testing.T) {
	// The shim writes no intent; the runner says so once, at assembly, with
	// enough context to find the page. Captured through slog, which is what
	// the meta tools themselves log through (OfferContext carries no logger).
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	rt := runtimeFixture(t) // slots fixture: "panel" writable → intent ""
	_, skip := (updateViewCapability{}).Offer(offerWith(t, rt))
	require.Nil(t, skip)
	assert.Contains(t, buf.String(), "hook without intent")
	assert.Contains(t, buf.String(), "hook=panel")
}

func TestViewCapabilitiesValidateAsGrants(t *testing.T) {
	for _, name := range []string{"update_view", "read_view"} {
		assert.NoError(t, ValidateGrant(name, json.RawMessage(`{}`)),
			"the AgentClass controller must not flag %q as an unknown capability", name)
		assert.Error(t, ValidateGrant(name, json.RawMessage(`{`)),
			"a malformed grant must fail closed for %q", name)
	}
}

// assembleWith runs AssembleAll for a class granting names, with rt wired as
// the session's UIView — the same fixture+deps shape every Gate test below
// wants, factored out once so each test differs only in its class/env.
func assembleWith(t *testing.T, rt *uiview.Runtime, names ...string) Assembly {
	t.Helper()
	return AssembleAll(context.Background(), AssembleDeps{
		Class:   classGranting(t, names...),
		Session: sessionFixture(t),
		Env:     RunnerEnv{UIView: rt},
		Logger:  testLogger(t),
	})
}

func sectionTitles(a Assembly) []string {
	out := make([]string, 0, len(a.Sections))
	for _, s := range a.Sections {
		out = append(out, s.Title)
	}
	return out
}

const twoHookPage = `{"component":"ap:stack","children":[
  {"component":"oap:generative","props":{"name":"phase","intent":"The stage timeline.","allowedComponents":["ap:steps"]}},
  {"component":"ap:card","children":[{"component":"oap:generative","props":{"name":"brief","allowedComponents":["*"]}}]}]}`

// TestAClassWithNoAgentUIGetsNoUIToolsAndNoPromptSection is the spec's Gate:
// the absence, asserted — through the same AssembleAll production calls —
// alongside its positive twin, so the test cannot pass by asserting nothing.
func TestAClassWithNoAgentUIGetsNoUIToolsAndNoPromptSection(t *testing.T) {
	without := assembleWith(t, nil, "update_view", "read_view", "set_view_params")
	names := toolNames(without.Tools)
	for _, n := range []string{"update_view", "read_view", "set_view_params"} {
		assert.NotContains(t, names, n, "no AgentUI wired → no %s", n)
	}
	assert.Empty(t, without.Sections, "no AgentUI wired → no prompt section either; the text is gated by the same decision as the tools")

	with := assembleWith(t, runtimeFixtureWithView(t, twoHookPage), "update_view", "read_view")
	assert.Contains(t, toolNames(with.Tools), "update_view")
	assert.Equal(t, []string{pageSectionTitle}, sectionTitles(with), "the same class with a page gets exactly one section")
}

func TestThePageSectionListsEveryHookInPageOrderWithItsLine(t *testing.T) {
	rt := runtimeFixtureWithView(t, twoHookPage)
	o := offerWith(t, rt)
	o.Class = classGranting(t, "update_view", "read_view")
	o.Binding = &spiceboxv1alpha1.ChannelBinding{} // channel-attached, so await_user_message is in the catalog
	tools, sections, skip := (updateViewCapability{}).OfferWithSections(o)
	require.Nil(t, skip)
	require.Len(t, tools, 1)
	require.Len(t, sections, 1)
	s := sections[0]
	assert.Equal(t, "Your page", s.Title)
	phase := "- phase: The stage timeline. (allowed: ap:steps)"
	brief := "- brief: no instruction from the author (allowed: any registered component)"
	assert.Contains(t, s.Body, phase)
	assert.Contains(t, s.Body, brief)
	assert.Less(t, strings.Index(s.Body, phase), strings.Index(s.Body, brief), "page order")
	assert.Contains(t, s.Body, "put an ap:notice in a hook", "the tell-without-asking rule rides beside the ask rule")
	assert.Contains(t, s.Body, "ap:question only when you need words back")
	assert.Contains(t, s.Body, "Replace or clear the notice once the thing you were waiting for happens")
	for _, rule := range []string{"update_view", "ap:question", "await_user_message", "clear: true", "Call read_view before your first update_view"} {
		assert.Contains(t, s.Body, rule, "the standing rules name %q", rule)
	}
	assert.NotContains(t, s.Body, "intake", "the rules are generic; a specific hook's guidance is its own intent line")
}

// TestThePageSectionNamesTheStepSummaryRule pins the rule line that tells the
// agent to keep a timeline step's summary current, one fact in a few words,
// never a sentence.
func TestThePageSectionNamesTheStepSummaryRule(t *testing.T) {
	rt := runtimeFixtureWithView(t, twoHookPage)
	o := offerWith(t, rt)
	o.Class = classGranting(t, "update_view", "read_view")
	o.Binding = &spiceboxv1alpha1.ChannelBinding{}
	_, sections, skip := (updateViewCapability{}).OfferWithSections(o)
	require.Nil(t, skip)
	require.Len(t, sections, 1)
	assert.Contains(t, sections[0].Body,
		"- If the page has a timeline, each step's summary is a fact in a few words, never a sentence; keep every step's summary current whenever you repaint the timeline.\n")
}

func TestThePageSectionOmitsTheReadViewRuleWhenReadViewIsNotActive(t *testing.T) {
	rt := runtimeFixtureWithView(t, twoHookPage)
	cases := []struct {
		name  string
		class *spiceboxv1alpha1.AgentClass
	}{
		{"read_view not granted", classGranting(t, "update_view")},
		{"read_view granted but disabled", &spiceboxv1alpha1.AgentClass{Spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: map[string]apiextensionsv1.JSON{
			"update_view": {Raw: []byte(`{}`)}, "read_view": {Raw: []byte(`{"enabled":false}`)}}}}},
		{"nil class", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := offerWith(t, rt)
			o.Class = tc.class
			_, sections, skip := (updateViewCapability{}).OfferWithSections(o)
			require.Nil(t, skip)
			require.Len(t, sections, 1)
			assert.NotContains(t, sections[0].Body, "read_view", "never tell the agent to call a tool it does not have")
		})
	}
}

// TestThePageSectionOmitsTheAwaitRuleWhenTheSessionIsNotChannelAttached is
// I-1's other half: await_user_message is contributed by
// channelInteractionCapability, which offers nothing when o.Binding is nil,
// so a kubectl-started session on a class with an AgentUI must be told to ask
// on the page WITHOUT being told to call a tool it does not have.
func TestThePageSectionOmitsTheAwaitRuleWhenTheSessionIsNotChannelAttached(t *testing.T) {
	o := offerWith(t, runtimeFixtureWithView(t, twoHookPage))
	o.Class = classGranting(t, "update_view", "read_view")
	require.Nil(t, o.Binding, "the fixture is not channel-attached")
	_, sections, skip := (updateViewCapability{}).OfferWithSections(o)
	require.Nil(t, skip)
	require.Len(t, sections, 1)
	assert.NotContains(t, sections[0].Body, "await_user_message", "never tell the agent to call a tool it does not have")
	assert.Contains(t, sections[0].Body, "ap:question", "it can still ask on the page; only the waiting half goes")
}

// TestThePageSectionSendsQuestionsToThePagesQuestionsHook pins the ask rule's
// wording: it points the agent at the hook the page itself sets aside for
// questions (named by that hook's own intent) WHEN the page has one — the
// rule ships to every page, and most declare no such hook — and names the
// conversation only as a fallback for what the page cannot carry, and only
// when there is a conversation to fall back to.
func TestThePageSectionSendsQuestionsToThePagesQuestionsHook(t *testing.T) {
	attached := "- Ask on the page: put an ap:question in the hook the page sets aside for questions when it has one (its intent says so), otherwise in the hook whose intent best fits, then wait for the answer with await_user_message. One question at a time; clear it once answered. If the page cannot carry a question — a long free-text answer, or something not on the page — ask in the conversation instead, where the person is prompted to reply."
	notAttached := "- Ask on the page: put an ap:question in the hook the page sets aside for questions when it has one (its intent says so), otherwise in the hook whose intent best fits. One question at a time; clear it once answered."

	t.Run("channel-attached: names the questions hook, then the await rule, then the conversation fallback", func(t *testing.T) {
		o := offerWith(t, runtimeFixtureWithView(t, twoHookPage))
		o.Class = classGranting(t, "update_view", "read_view")
		o.Binding = &spiceboxv1alpha1.ChannelBinding{}
		_, sections, skip := (updateViewCapability{}).OfferWithSections(o)
		require.Nil(t, skip)
		require.Len(t, sections, 1)
		assert.Contains(t, sections[0].Body, attached)
	})

	t.Run("not channel-attached: names the questions hook, without the await rule or the conversation fallback", func(t *testing.T) {
		o := offerWith(t, runtimeFixtureWithView(t, twoHookPage))
		o.Class = classGranting(t, "update_view", "read_view")
		require.Nil(t, o.Binding, "the fixture is not channel-attached")
		_, sections, skip := (updateViewCapability{}).OfferWithSections(o)
		require.Nil(t, skip)
		require.Len(t, sections, 1)
		assert.Contains(t, sections[0].Body, notAttached)
		assert.NotContains(t, sections[0].Body, "ask in the conversation instead",
			"with no channel there is no conversation to fall back to")
	})
}

func TestOfferAndOfferWithSectionsAgreeOnTheTools(t *testing.T) {
	rt := runtimeFixtureWithView(t, twoHookPage)
	tools, skip := (updateViewCapability{}).Offer(offerWith(t, rt))
	withTools, _, withSkip := (updateViewCapability{}).OfferWithSections(offerWith(t, rt))
	require.Nil(t, skip)
	require.Nil(t, withSkip)
	assert.Equal(t, toolNames(tools), toolNames(withTools))
	assert.JSONEq(t, string(tools[0].InputSchema()), string(withTools[0].InputSchema()))
}

func TestAPageWithNoHookContributesNoSection(t *testing.T) {
	got := assembleWith(t, runtimeFixtureAllReadOnlySlots(t), "update_view", "read_view")
	assert.NotContains(t, toolNames(got.Tools), "update_view")
	assert.Empty(t, got.Sections, "the zero-hook skip withholds the text with the tool")
}
