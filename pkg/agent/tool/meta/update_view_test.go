package meta_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	memartifact "github.com/authzed/openagentprimitives/pkg/memory/kinds/artifact"
	memrev "github.com/authzed/openagentprimitives/pkg/memory/kinds/artifactrevision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/label"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiaction"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents/registry"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
)

const (
	fixtureNS      = "demo-ns"
	fixtureSession = "demo-session"
	fixtureUI      = "demo-ui"
)

// sess is the *tool.SessionContext every test in this file executes
// against — a fixed identity, since no test here varies session attribution.
func sess() *tool.SessionContext {
	return &tool.SessionContext{Namespace: fixtureNS, Name: fixtureSession}
}

// hooksNamed builds the UpdateViewConfig.Hooks a test wants when only the
// names matter: each hook admits the whole registry and carries no intent.
func hooksNamed(names ...string) []uicomponents.Hook {
	out := make([]uicomponents.Hook, len(names))
	for i, n := range names {
		out[i] = uicomponents.Hook{Name: n, AllowedComponents: []string{uicomponents.AllowAll}}
	}
	return out
}

// rawJSONField wraps a JSON literal as the *apiextensionsv1.JSON
// AgentUISlot.Default expects, so the fixture below can spell each Tier-0
// default as plain JSON text instead of building uicomponents.Node values by
// hand.
func rawJSONField(s string) *apiextensionsv1.JSON {
	return &apiextensionsv1.JSON{Raw: []byte(s)}
}

// The three sentinels below are the VALUES a resolver would return if
// anything in read_view ever resolved one of the fixture's Tier-0 bindings.
// Every one of them is really there, behind a real binding, in the fixture's
// own memory scope — which is the whole point: a pin over a payload whose
// bindings resolve to nothing cannot tell "read_view leaks no data" from
// "there was no data to leak". See plantBindableData.
const (
	// memoryLeakSentinel is the Label of a real `label` memory entry, which
	// pkg/web/uibindings/memoryref would return for the "activity" slot's binding
	// (a memory ref names a memory KIND, which is why the ref is "label").
	memoryLeakSentinel = "MEMORY_SENTINEL_NEVER_IN_READ_VIEW"
	// artifactLeakSentinel is the RenderName artifacts.Service.ResolveToRender
	// returns for the "attachment" slot's handle. Artifact METADATA needs only
	// a memory.Memory (artifacts.NewService takes nothing else), so this is
	// the artifact source's reachable value even with no render-bytes fetcher.
	artifactLeakSentinel = "ar-artifact-sentinel-never-in-read-view"
	// fixtureArtifactHandle is the head the "attachment" slot binds to.
	fixtureArtifactHandle = memartifact.IDPrefix + "fixture0000000001"
	fixtureArtifactRevID  = memrev.IDPrefix + "fixture0000000001"
)

// actionLeakSentinel is the copy pkg/web/uibindings/actionstate would return for
// the "status" slot's binding, given the ui_action record plantBindableData
// writes. A function, not a const, because the value is uiaction's own
// framework-authored display copy rather than a string this test may invent.
func actionLeakSentinel() string {
	return uiaction.DisplayCopy(uiaction.StateAwaitingApproval, true)
}

// newRuntimeFixture builds a real memory.Memory (in-process, in-memory
// backend) and a fake controller-runtime client holding one AgentUI, then
// wires both into a *uiview.Runtime — the same construction update_view and
// read_view see in production, minus the K8s apiserver and NATS.
//
// The fixture AgentUI declares:
//   - "panel", agentWritable — the slot every write/read test targets.
//   - "notes", not agentWritable, with a Tier-0 default bound to the
//     granted, readonly tool "crm_list_leads" — a binding the agent never
//     wrote, present so read_view's tests can assert it is DESCRIBED and
//     never RESOLVED.
//   - "controls", not agentWritable, with a Tier-0 ap:select declaring the
//     "span" binding parameter — the one parameter read_view's tests expect
//     back from a document the agent never touched.
//   - "activity" (MEMORY source), "status" (ACTION source) and "attachment"
//     (ARTIFACT source), none agentWritable. Together with "notes" they give
//     the fixture one Tier-0 binding per registered binding source, and
//     plantBindableData puts REAL, resolvable content behind each of the
//     three that a memory.Memory alone can reach. That is what lets
//     read_view's pins assert "no values, from any source" against a document
//     where every source genuinely has something to leak.
//   - one action, "advance", naming the granted tool "crm_advance_stage"
//     with input "why" — the fixture update_view's own collision-rejection
//     test needs (a fragment introducing a control parameter named "why").
//
// "crm_delete_lead" is deliberately never granted, so a fragment binding to
// it is the fixture's negative case for "not granted".
func newRuntimeFixture(t *testing.T) (context.Context, *uiview.Runtime) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())

	scheme := k8sruntime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	ui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Namespace: fixtureNS, Name: fixtureUI},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Actions: []spiceboxv1alpha1.AgentUIAction{
				{Name: "advance", Tool: "crm_advance_stage", Inputs: []string{"why"}},
			},
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "panel", AgentWritable: true},
				{Name: "notes", AgentWritable: false, Default: rawJSONField(
					`{"component":"ap:table","bindings":{"rows":{"source":"tool","ref":"crm_list_leads"}}}`)},
				{Name: "controls", AgentWritable: false, Default: rawJSONField(
					`{"component":"ap:select","props":{"param":"span","value":"30d","options":[{"value":"30d"},{"value":"90d"}]}}`)},
				{Name: "activity", AgentWritable: false, Default: rawJSONField(
					`{"component":"ap:table","bindings":{"rows":{"source":"memory","ref":"label"}}}`)},
				{Name: "status", AgentWritable: false, Default: rawJSONField(
					`{"component":"ap:alert","bindings":{"body":{"source":"action","ref":"advance"}}}`)},
				{Name: "attachment", AgentWritable: false, Default: rawJSONField(
					`{"component":"ap:markdown","bindings":{"body":{"source":"artifact","ref":"` + fixtureArtifactHandle + `"}}}`)},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ui).Build()

	rt := &uiview.Runtime{
		Namespace: fixtureNS, Session: fixtureSession, UIName: fixtureUI,
		Client: cl, Mem: mem,
		ToolOptions: func() uicomponents.Options {
			o := uicomponents.DefaultOptions()
			o.GrantedTools = map[string]bool{"crm_advance_stage": true, "crm_list_leads": true}
			o.ReadonlyTools = map[string]bool{"crm_list_leads": true}
			return o
		},
	}
	plantBindableData(t, ctx, rt)
	return ctx, rt
}

// plantBindableData writes real, resolvable content behind each of the
// fixture's three memory-reachable binding sources.
//
// It exists because of a hole a review found in read_view's own pin: a
// payload-key assertion can only see a leak that produced a NON-EMPTY value,
// so a fixture whose bindings resolve to nothing makes the pin's coverage an
// accident of the fixture rather than a property of the code. With this
// planted, a hypothetical values field sourced from ANY of memory, action, or
// artifact carries a sentinel that read_view's tests assert is absent.
//
// The `tool` source is the one that gets nothing: its transport
// (channelevents.RequestFunc) is absent from ReadViewConfig by type, so no
// value can exist for it to leak — see NewReadView's own doc comment for
// which sources are structurally blocked and which are held out by discipline.
func plantBindableData(t *testing.T, ctx context.Context, rt *uiview.Runtime) {
	t.Helper()
	scope := memory.Scope{Kind: "session", ID: rt.Namespace + "/" + rt.Session}

	// memory source: memoryref queries Kinds:[ref], so the ref names a memory
	// KIND and the planted entry must be of that kind.
	labelRaw, err := json.Marshal(label.Label{ResourceType: "lead", ResourceID: "42", Label: memoryLeakSentinel})
	require.NoError(t, err)
	_, err = rt.Mem.Put(ctx, memory.Entry{
		Scope: scope, Kind: label.Kind{}.Name(), ID: "label-fixture0000000001", Content: labelRaw,
	})
	require.NoError(t, err)

	// action source: actionstate reads the viewer's own ui_action records and
	// answers with uiaction.DisplayCopy for the newest one naming the ref.
	require.NoError(t, uiaction.Record(ctx, rt.Mem, scope, uiaction.Content{
		RequestID: "req-fixture-1", Action: "advance", State: uiaction.StateAwaitingApproval,
		Message: actionLeakSentinel(), ApprovalAddressedToViewer: true,
		UpdatedAt: time.Now().UTC(), Requester: uiaction.RequesterKey("user:fixture-viewer"),
	}))

	// artifact source: a head plus the revision its "latest" tag points at is
	// the whole graph ResolveToRender walks. Written as raw entries rather
	// than through FinalizeRevision, which needs an ArtifactRender CR this
	// fixture has no reason to build.
	headRaw, err := json.Marshal(memartifact.Artifact{
		RendererKind: "html", RevisionCount: 1, Tags: map[string]string{artifacts.TagLatest: fixtureArtifactRevID},
	})
	require.NoError(t, err)
	_, err = rt.Mem.Put(ctx, memory.Entry{
		Scope: scope, Kind: memartifact.Kind{}.Name(), ID: fixtureArtifactHandle, Content: headRaw,
	})
	require.NoError(t, err)

	revRaw, err := json.Marshal(memrev.Revision{Seq: 1, RenderName: artifactLeakSentinel, MIME: "text/html"})
	require.NoError(t, err)
	_, err = rt.Mem.Put(ctx, memory.Entry{
		Scope: scope, Kind: memrev.Kind{}.Name(), ID: fixtureArtifactRevID, Content: revRaw,
		Links: []memory.Link{{Relation: "revision_of", Kind: memartifact.Kind{}.Name(), ID: fixtureArtifactHandle}},
	})
	require.NoError(t, err)
}

// hookChildren returns the current children of the hook named name inside
// decl — walking decl.View by the hook's own recorded Path, since
// Declaration.Slots is always nil after normalization and a hook's content
// can only be read back through the tree it actually landed in.
func hookChildren(t *testing.T, decl uicomponents.Declaration, name string) []uicomponents.Node {
	t.Helper()
	for _, h := range uicomponents.Hooks(decl) {
		if h.Name != name {
			continue
		}
		require.NotNil(t, decl.View)
		n := *decl.View
		for _, i := range h.Path {
			require.Less(t, i, len(n.Children), "hook %q path out of range", name)
			n = n.Children[i]
		}
		return n.Children
	}
	t.Fatalf("hook %q not found in declaration", name)
	return nil
}

// TestUpdateViewWritesWhatTheViewerActuallySees is the J1 span: the tool's
// own write path and the read path webd uses are asserted TOGETHER, through
// the real Execute and the real Runtime.View. Testing only "Record was
// called" would leave a writer and a reader that key the same slot
// differently — and both halves would stay green.
func TestUpdateViewWritesWhatTheViewerActuallySees(t *testing.T) {
	ctx, rt := newRuntimeFixture(t)

	tl := meta.NewUpdateView(meta.UpdateViewConfig{View: rt, Hooks: hooksNamed("panel")})
	res, err := tl.Execute(ctx, json.RawMessage(
		`{"hook":"panel","node":{"component":"ap:markdown","props":{"body":"agent copy"}}}`),
		&tool.SessionContext{Namespace: "demo-ns", Name: "demo-session"})
	require.NoError(t, err, "an agent-correctable failure is a Result, never a Go error")
	require.False(t, res.IsError, "Content: %s", res.Content)
	assert.True(t, res.Trusted, "framework-authored content")
	assert.JSONEq(t, `{"hook":"panel","updated":true}`, res.Content)

	v, err := rt.View(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"panel"}, v.AgentComposed)
	children := hookChildren(t, v.Declaration, "panel")
	require.Len(t, children, 1)
	assert.Equal(t, "ap:markdown", children[0].Component)
}

func TestUpdateViewRejections(t *testing.T) {
	cases := []struct {
		name   string
		args   string
		wantIn string // substring of the structured rejection
	}{
		{
			// "notes" is not agentWritable, so CompileSlots never turned it into a
			// hook — it carries no name a fragment could target, the same outcome
			// as a name the page never declared at all.
			name:   "a non-writable slot compiled no hook, so targeting it is refused as unknown",
			args:   `{"hook":"notes","node":{"component":"ap:text","props":{"text":"x"}}}`,
			wantIn: "unknown hook",
		},
		{
			name:   "a hook the UI does not declare is refused",
			args:   `{"hook":"ghost","node":{"component":"ap:text","props":{"text":"x"}}}`,
			wantIn: "unknown hook",
		},
		{
			name:   "an unknown component type is refused",
			args:   `{"hook":"panel","node":{"component":"ap:doesnotexist"}}`,
			wantIn: "unknown component type",
		},
		{
			name:   "an invented prop is refused",
			args:   `{"hook":"panel","node":{"component":"ap:text","props":{"txt":"x"}}}`,
			wantIn: "unknown prop",
		},
		{
			name:   "an unknown wire field is refused rather than dropped",
			args:   `{"hook":"panel","node":{"component":"ap:text","childs":[]}}`,
			wantIn: "unknown field",
		},
		{
			// The carry-forward, asserted from the TOOL's side as well as
			// ResolveView's: the fixture UI declares an action with input
			// "why", and this fragment would introduce a control driving a
			// binding parameter of the same name.
			name:   "a fragment colliding with an action input is refused",
			args:   `{"hook":"panel","node":{"component":"ap:select","props":{"param":"why","value":"x","options":[{"value":"x"}]}}}`,
			wantIn: "collides with a binding parameter",
		},
		{
			name:   "a binding to an ungranted tool is refused",
			args:   `{"hook":"panel","node":{"component":"ap:table","bindings":{"rows":{"source":"tool","ref":"crm_delete_lead"}}}}`,
			wantIn: "not granted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, rt := newRuntimeFixture(t)
			tl := meta.NewUpdateView(meta.UpdateViewConfig{View: rt, Hooks: hooksNamed("panel")})

			res, err := tl.Execute(ctx, json.RawMessage(tc.args),
				&tool.SessionContext{Namespace: "demo-ns", Name: "demo-session"})
			require.NoError(t, err)
			require.True(t, res.IsError, "must be a correctable tool error, not a success")
			assert.Contains(t, res.Content, tc.wantIn)

			v, verr := rt.View(ctx)
			require.NoError(t, verr)
			assert.Empty(t, v.AgentComposed, "NOTHING may be written on a rejection")
		})
	}
}

func TestUpdateViewSchemaIsDerivedFromTheRegistry(t *testing.T) {
	tl := meta.NewUpdateView(meta.UpdateViewConfig{View: nil, Hooks: hooksNamed("panel")})
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
			Type string   `json:"type"`
		} `json:"properties"`
		Required []string                              `json:"required"`
		Defs     map[string]map[string]json.RawMessage `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(tl.InputSchema(), &schema), "the emitted schema must be valid JSON")
	assert.Equal(t, []string{"panel"}, schema.Properties["hook"].Enum)
	assert.Equal(t, "boolean", schema.Properties["clear"].Type)
	assert.Equal(t, []string{"hook"}, schema.Required)

	for _, c := range registry.All() {
		if c.Structural {
			continue // oap:generative is a hook, never a component the agent fills AS
		}
		assert.Contains(t, schema.Defs["components"], c.Type,
			"every registered component must be published; a transcribed list goes stale on the first new one")
	}
	assert.NotContains(t, schema.Defs["components"], uicomponents.GenerativeType,
		"a hook is structural — never something the agent can fill AS a component")
}

func TestUpdateViewWithNoRuntimeFailsLoudly(t *testing.T) {
	tl := meta.NewUpdateView(meta.UpdateViewConfig{Hooks: hooksNamed("panel")})
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"hook":"panel","node":{"component":"ap:empty"}}`),
		&tool.SessionContext{Namespace: "demo-ns", Name: "demo-session"})
	require.NoError(t, err)
	assert.True(t, res.IsError, "an inert tool that reports success is the worst available failure")
}

// newHookRuntimeFixture builds a *uiview.Runtime backed by an AgentUI
// declaring Spec.View — the tree shape, not the Spec.Slots shim — with three
// hooks in document order: "phase" and "brief" admit anything ("*"), and
// "intake" admits only ap:form and ap:markdown, giving the allowlist
// rejection tests a concrete illegal component to name.
func newHookRuntimeFixture(t *testing.T) (context.Context, *uiview.Runtime) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())

	scheme := k8sruntime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	view := `{"component":"ap:stack","children":[
	  {"component":"oap:generative","props":{"name":"phase","intent":"the current stage of the intake","allowedComponents":["*"]}},
	  {"component":"oap:generative","props":{"name":"brief","intent":"a short written summary","allowedComponents":["*"]}},
	  {"component":"oap:generative","props":{"name":"intake","intent":"a form or a note","allowedComponents":["ap:form","ap:markdown"]}}
	]}`
	ui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Namespace: fixtureNS, Name: fixtureUI},
		Spec:       spiceboxv1alpha1.AgentUISpec{View: &apiextensionsv1.JSON{Raw: []byte(view)}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ui).Build()

	rt := &uiview.Runtime{
		Namespace: fixtureNS, Session: fixtureSession, UIName: fixtureUI,
		Client:      cl,
		Mem:         mem,
		ToolOptions: func() uicomponents.Options { return uicomponents.DefaultOptions() },
	}
	return ctx, rt
}

func TestUpdateViewTargetsAHookByName(t *testing.T) {
	ctx, rt := newHookRuntimeFixture(t)
	tl := meta.NewUpdateView(meta.UpdateViewConfig{View: rt, Hooks: hooksNamed("phase", "brief", "intake")})
	res, err := tl.Execute(ctx, json.RawMessage(`{"hook":"brief","node":{"component":"ap:markdown","props":{"body":"agent copy"}}}`), sess())
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content)
	assert.JSONEq(t, `{"hook":"brief","updated":true}`, res.Content)
}

func TestUpdateViewClearCollapsesAHook(t *testing.T) {
	ctx, rt := newHookRuntimeFixture(t)
	tl := meta.NewUpdateView(meta.UpdateViewConfig{View: rt, Hooks: hooksNamed("phase", "brief", "intake")})
	res, err := tl.Execute(ctx, json.RawMessage(`{"hook":"intake","clear":true}`), sess())
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content)
	assert.JSONEq(t, `{"hook":"intake","cleared":true}`, res.Content)
	v, err := rt.View(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"intake"}, v.AgentComposed)
}

func TestUpdateViewRefusesAFillOutsideTheAllowlistAsAToolResult(t *testing.T) {
	ctx, rt := newHookRuntimeFixture(t)
	tl := meta.NewUpdateView(meta.UpdateViewConfig{View: rt, Hooks: hooksNamed("phase", "brief", "intake")})
	res, err := tl.Execute(ctx, json.RawMessage(`{"hook":"intake","node":{"component":"ap:card","props":{"title":"x"}}}`), sess())
	require.NoError(t, err, "a rejection is a tool RESULT, never a protocol error")
	require.True(t, res.IsError)
	var body map[string]string
	require.NoError(t, json.Unmarshal([]byte(res.Content), &body))
	assert.Equal(t, "invalid_view", body["error"])
	assert.Equal(t, "intake", body["hook"])
	assert.Contains(t, body["reason"], `component "ap:card" is not allowed in hook "intake"; allowed: [ap:form, ap:markdown]`)
}

func TestUpdateViewRequiresExactlyOneOfNodeAndClear(t *testing.T) {
	ctx, rt := newHookRuntimeFixture(t)
	tl := meta.NewUpdateView(meta.UpdateViewConfig{View: rt, Hooks: hooksNamed("brief")})
	for _, args := range []string{`{"hook":"brief"}`, `{"hook":"brief","clear":true,"node":{"component":"ap:text"}}`} {
		res, err := tl.Execute(ctx, json.RawMessage(args), sess())
		require.NoError(t, err)
		assert.True(t, res.IsError, args)
		assert.Contains(t, res.Content, "exactly one of node and clear", args)
	}
}

// TestUpdateViewSchemaDescribesEachHookWithItsIntentAndAllowlist pins that
// the hook property's DESCRIPTION carries every hook's line — the same line
// the "Your page" prompt section prints — while the enum stays the plain
// name list. A model reads the description; the enum is for validation.
func TestUpdateViewSchemaDescribesEachHookWithItsIntentAndAllowlist(t *testing.T) {
	hooks := []uicomponents.Hook{
		{Name: "phase", Intent: "The stage timeline.", AllowedComponents: []string{"ap:steps"}},
		{Name: "brief", AllowedComponents: []string{"*"}},
	}
	tl := meta.NewUpdateView(meta.UpdateViewConfig{View: nil, Hooks: hooks})
	var schema struct {
		Properties struct {
			Hook struct {
				Enum        []string `json:"enum"`
				Description string   `json:"description"`
			} `json:"hook"`
			Node struct {
				Description string `json:"description"`
			} `json:"node"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(tl.InputSchema(), &schema))
	assert.Equal(t, []string{"phase", "brief"}, schema.Properties.Hook.Enum)
	assert.Contains(t, schema.Properties.Hook.Description, "- "+meta.HookLine(hooks[0]))
	assert.Contains(t, schema.Properties.Hook.Description, "- "+meta.HookLine(hooks[1]))
	assert.Less(t, strings.Index(schema.Properties.Hook.Description, "- phase:"), strings.Index(schema.Properties.Hook.Description, "- brief:"),
		"page order, as the agent reads the page")
	assert.Contains(t, schema.Properties.Node.Description, "allowed set",
		"the node property tells the agent the fill must stay inside the hook's allowlist")
}

func TestUpdateViewDescriptionCarriesItsOwnRules(t *testing.T) {
	desc := meta.NewUpdateView(meta.UpdateViewConfig{Hooks: hooksNamed("brief")}).Description()
	for _, want := range []string{"by name", "allowed", "ap:question", "clear: true"} {
		assert.Contains(t, desc, want, "update_view's description must carry the rule about %q itself", want)
	}
}
