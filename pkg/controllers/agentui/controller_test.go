package agentui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/synthesize"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentui"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "spicebox.AddToScheme")
	return s
}

// jsonNode is a fabricated helper building a raw *apiextensionsv1.JSON node
// literal for a slot's Default field.
func jsonNode(t *testing.T, raw string) *apiextensionsv1.JSON {
	t.Helper()
	return &apiextensionsv1.JSON{Raw: []byte(raw)}
}

// minimalPage is the smallest page a CR can declare: an empty root stack,
// no hooks, no bindings. A fixture about something OTHER than the page (the
// eligible-tools ceiling, the ToolsGranted condition, the action table)
// carries it so the CR clears admission — "neither view nor slots" is a
// refusal — without adding content those tests would then have to ignore.
func minimalPage() *apiextensionsv1.JSON {
	return &apiextensionsv1.JSON{Raw: []byte(`{"component":"ap:stack"}`)}
}

// manyLeafNodesJSON builds an ap:stack root with enough ap:text leaf
// children that the returned node tree contains exactly totalNodes nodes
// (the root itself counts as one).
func manyLeafNodesJSON(t *testing.T, totalNodes int) *apiextensionsv1.JSON {
	t.Helper()
	children := make([]map[string]any, totalNodes-1)
	for i := range children {
		children[i] = map[string]any{"component": "ap:text"}
	}
	raw, err := json.Marshal(map[string]any{"component": "ap:stack", "children": children})
	require.NoError(t, err, "marshal synthetic node tree fixture")
	return &apiextensionsv1.JSON{Raw: raw}
}

func reconcileNN(t *testing.T, r *agentui.Reconciler, ns, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: name},
	})
	require.NoError(t, err, "Reconcile must succeed for this fixture")
	return res
}

func getUI(t *testing.T, c client.Client, ns, name string) *spiceboxv1alpha1.AgentUI {
	t.Helper()
	got := &spiceboxv1alpha1.AgentUI{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, got), "Get AgentUI after Reconcile")
	return got
}

// reconcileOnce builds a fake client seeded with ui alone, reconciles it
// once, and returns the freshly-Get'd object — the shared shape the
// admission-table tests need, where each case is a whole CR and nothing
// else.
func reconcileOnce(t *testing.T, ui *spiceboxv1alpha1.AgentUI) *spiceboxv1alpha1.AgentUI {
	t.Helper()
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ui).WithStatusSubresource(ui).Build()
	r := &agentui.Reconciler{Client: c}
	reconcileNN(t, r, ui.Namespace, ui.Name)
	return getUI(t, c, ui.Namespace, ui.Name)
}

// viewUI builds a minimal AgentUI CR carrying a raw spec.view document.
func viewUI(name string, view string) *spiceboxv1alpha1.AgentUI {
	return &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: name},
		Spec:       spiceboxv1alpha1.AgentUISpec{View: &apiextensionsv1.JSON{Raw: []byte(view)}},
	}
}

// listFailsFor builds a fake-client interceptor that fails every List call
// against the given ObjectList type, passing everything else through.
func listFailsFor[T client.ObjectList](msg string) interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(T); ok {
				return fmt.Errorf("%s", msg)
			}
			return cl.List(ctx, list, opts...)
		},
	}
}

// TestReconcileAdmitsThePageTree is the spec §12 admission table for
// spec.view: every row the hook contract enforces (found at any depth,
// unique names, a registered allowlist, no nesting, an author default that
// need not fit the hook's own allowlist), driven straight through Reconcile
// rather than against validateDeclaration directly, so it also proves the
// Valid condition and status.hooks are wired to the same verdict.
func TestReconcileAdmitsThePageTree(t *testing.T) {
	cases := []struct {
		name      string
		view      string
		wantValid metav1.ConditionStatus
		wantInMsg string
		wantHooks []spiceboxv1alpha1.AgentUIHook
	}{
		{name: "hooks at any depth are found and observed onto status in document order",
			view: `{"component":"ap:stack","children":[
			  {"component":"oap:generative","props":{"name":"phase","intent":"the timeline","allowedComponents":["ap:steps"]}},
			  {"component":"ap:card","children":[{"component":"oap:generative","props":{"name":"brief","allowedComponents":["*"]}}]}]}`,
			wantValid: metav1.ConditionTrue,
			wantHooks: []spiceboxv1alpha1.AgentUIHook{{Name: "phase", Intent: "the timeline", AllowedComponents: []string{"ap:steps"}}, {Name: "brief", AllowedComponents: []string{"*"}}}},
		{name: "duplicate hook names fail the CR",
			view:      `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"x","allowedComponents":["*"]}},{"component":"oap:generative","props":{"name":"x","allowedComponents":["*"]}}]}`,
			wantValid: metav1.ConditionFalse, wantInMsg: `duplicate hook name "x"`},
		{name: "an allowlist naming an unregistered component fails the CR naming it",
			view:      `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"x","allowedComponents":["ap:nope"]}}]}`,
			wantValid: metav1.ConditionFalse, wantInMsg: `allows unknown component type "ap:nope"`},
		{name: "* is accepted",
			view:      `{"component":"oap:generative","props":{"name":"x","allowedComponents":["*"]}}`,
			wantValid: metav1.ConditionTrue, wantHooks: []spiceboxv1alpha1.AgentUIHook{{Name: "x", AllowedComponents: []string{"*"}}}},
		{name: "an author default OUTSIDE the hook's allowlist is accepted (the ap:card-around-a-form case)",
			view: `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"intake","allowedComponents":["ap:markdown","ap:form"]},"children":[
			  {"component":"ap:card","props":{"title":"Describe the agent"},"children":[{"component":"ap:markdown","props":{"body":"tell me"}}]}]}]}`,
			wantValid: metav1.ConditionTrue, wantHooks: []spiceboxv1alpha1.AgentUIHook{{Name: "intake", AllowedComponents: []string{"ap:markdown", "ap:form"}}}},
		{name: "an author default naming an unregistered component still fails the CR",
			view:      `{"component":"oap:generative","props":{"name":"x","allowedComponents":["*"]},"children":[{"component":"ap:bogus"}]}`,
			wantValid: metav1.ConditionFalse, wantInMsg: `unknown component type "ap:bogus"`},
		{name: "a hook nested in a hook fails the CR",
			view:      `{"component":"oap:generative","props":{"name":"outer","allowedComponents":["*"]},"children":[{"component":"oap:generative","props":{"name":"inner","allowedComponents":["*"]}}]}`,
			wantValid: metav1.ConditionFalse, wantInMsg: `hooks cannot nest`},
		{name: "a hook with no allowlist fails the CR",
			view:      `{"component":"oap:generative","props":{"name":"x"}}`,
			wantValid: metav1.ConditionFalse, wantInMsg: `declares no allowedComponents`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ui := viewUI("page", tc.view)
			got := reconcileOnce(t, ui)
			cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
			require.NotNil(t, cond)
			assert.Equal(t, tc.wantValid, cond.Status)
			if tc.wantInMsg != "" {
				assert.Contains(t, cond.Message, tc.wantInMsg)
				assert.Empty(t, got.Status.Hooks, "an invalid page observes no hooks")
			}
			assert.Equal(t, tc.wantHooks, got.Status.Hooks)
		})
	}
}

// TestReconcileRefusesViewAndSlotsTogether covers Normalize's "declare either
// view or slots, not both" error, surfaced as Valid=False rather than a
// silently-picked winner: two descriptions of one page cannot be reconciled
// by favoring one over the other.
func TestReconcileRefusesViewAndSlotsTogether(t *testing.T) {
	ui := viewUI("page", `{"component":"ap:stack"}`)
	ui.Spec.Slots = []spiceboxv1alpha1.AgentUISlot{{Name: "root"}}
	got := reconcileOnce(t, ui)
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "either view or slots")
}

// TestReconcileRefusesAPageDeclaringNeitherViewNorSlots is the other half of
// "exactly one of view or slots". A CR with neither describes no page at all,
// and admitting it would be the quietest possible failure: Valid=True over an
// empty tree, no hooks, and every update_view call refused for naming a hook
// that does not exist. It is also what a typo'd top-level key looks like by
// the time the reconciler sees it — spec prunes unknown fields, so a
// misspelled "view" arrives as neither — which is exactly the author mistake
// a Valid=False message has to name.
func TestReconcileRefusesAPageDeclaringNeitherViewNorSlots(t *testing.T) {
	ui := &spiceboxv1alpha1.AgentUI{ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "page"}}
	got := reconcileOnce(t, ui)
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "declare either view or slots")
	assert.Nil(t, got.Status.Hooks, "a page that declares nothing observes no hooks")
}

// TestReconcileCompilesLegacySlotsAndObservesTheWritableOnesAsHooks proves the
// spec.slots shim end to end through Reconcile: only the agentWritable slots
// ("phase", "brief") become hooks observed onto status; "notes" (read-only)
// contributes its default straight into the compiled view and is not a hook
// at all, and the shim admits the whole page even though nothing in it was
// authored as a view tree.
func TestReconcileCompilesLegacySlotsAndObservesTheWritableOnesAsHooks(t *testing.T) {
	ui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "legacy"},
		Spec: spiceboxv1alpha1.AgentUISpec{Slots: []spiceboxv1alpha1.AgentUISlot{
			{Name: "phase", AgentWritable: true, Default: jsonNode(t, `{"component":"ap:text","props":{"text":"Intake"}}`)},
			{Name: "notes", Default: jsonNode(t, `{"component":"ap:text","props":{"text":"ro"}}`)},
			{Name: "brief", AgentWritable: true},
		}},
	}
	got := reconcileOnce(t, ui)
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, []spiceboxv1alpha1.AgentUIHook{
		{Name: "phase", AllowedComponents: []string{"*"}},
		{Name: "brief", AllowedComponents: []string{"*"}},
	}, got.Status.Hooks, "non-writable slots are not hooks; the shim admits everything")
}

// TestReconcileClearsHooksWhenAValidPageIsEditedInvalid proves the
// `cr.Status.Hooks = nil` clear in Reconcile's validateDeclaration-failure
// branch actually fires on a TRANSITION, not merely that an always-invalid
// CR never had hooks to begin with (which the admission-table cases alone
// cannot distinguish, since every one of those starts from a fresh CR with
// nil status). It exercises the validateDeclaration-failure clear
// specifically — editing spec.view to introduce a duplicate hook name.
// Gate A's OWN clear is a different branch with its own `= nil`, and is
// covered separately by TestReconcileClearsHooksWhenGateAStartsFailing.
func TestReconcileClearsHooksWhenAValidPageIsEditedInvalid(t *testing.T) {
	scheme := newScheme(t)
	ui := viewUI("page", `{"component":"oap:generative","props":{"name":"x","allowedComponents":["*"]}}`)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ui).WithStatusSubresource(ui).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, ui.Namespace, ui.Name)
	first := getUI(t, c, ui.Namespace, ui.Name)
	firstCond := meta.FindStatusCondition(first.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, firstCond)
	require.Equal(t, metav1.ConditionTrue, firstCond.Status, "fixture must start Valid before this test means anything")
	require.NotEmpty(t, first.Status.Hooks, "fixture must start with an observed hook before this test means anything")

	// Break the hook contract: two hooks now share the name "x".
	first.Spec.View = &apiextensionsv1.JSON{Raw: []byte(`{"component":"ap:stack","children":[
	  {"component":"oap:generative","props":{"name":"x","allowedComponents":["*"]}},
	  {"component":"oap:generative","props":{"name":"x","allowedComponents":["*"]}}]}`)}
	require.NoError(t, c.Update(context.Background(), first))

	reconcileNN(t, r, ui.Namespace, ui.Name)
	second := getUI(t, c, ui.Namespace, ui.Name)
	secondCond := meta.FindStatusCondition(second.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, secondCond)
	assert.Equal(t, metav1.ConditionFalse, secondCond.Status)
	assert.Contains(t, secondCond.Message, "duplicate hook name")
	assert.Nil(t, second.Status.Hooks,
		"an edit that breaks the hook contract must clear the previously-observed hook table, not leave it describing a page the CR no longer declares")
}

// TestReconcileClearsHooksWhenGateAStartsFailing is the OTHER half of the
// Status.Hooks-clears coverage: TestReconcileClearsHooksWhenAValidPageIsEditedInvalid
// exercises the validateDeclaration-failure clear; this one exercises Gate
// A's own clear (uigrant.UnrequestedActionTools) — a DIFFERENT branch in
// Reconcile with its own `cr.Status.Hooks = nil`, which the admission-table
// and validateDeclaration-transition tests cannot reach because Gate A runs
// BEFORE validateDeclaration and short-circuits it. Reuses the "granted by
// the AgentClass but never requested via spec.Tools" shape
// dropActionToolFromSpecTools exercises, in reverse: the CR starts with no
// such action (so Gate A has nothing to reject) and the edit ADDS one.
func TestReconcileClearsHooksWhenGateAStartsFailing(t *testing.T) {
	scheme := newScheme(t)
	ui := viewUI("page", `{"component":"oap:generative","props":{"name":"x","allowedComponents":["*"]}}`)
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "page-agent", Namespace: ui.Namespace},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: ui.Name, GrantedTools: []string{"demo_advance"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ui, ac).WithStatusSubresource(ui).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, ui.Namespace, ui.Name)
	first := getUI(t, c, ui.Namespace, ui.Name)
	firstCond := meta.FindStatusCondition(first.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, firstCond)
	require.Equal(t, metav1.ConditionTrue, firstCond.Status, "fixture must start Valid before this test means anything")
	require.NotEmpty(t, first.Status.Hooks, "fixture must start with an observed hook before this test means anything")

	// Add an action naming a tool the AgentClass grants but spec.Tools never
	// requested — Gate A's own precondition, and dropActionToolFromSpecTools'
	// shape run in reverse (there it removes the tool from an already-present
	// request; here the request was never made).
	first.Spec.Actions = []spiceboxv1alpha1.AgentUIAction{{Name: "advance", Tool: "demo_advance"}}
	require.NoError(t, c.Update(context.Background(), first))

	reconcileNN(t, r, ui.Namespace, ui.Name)
	second := getUI(t, c, ui.Namespace, ui.Name)
	secondCond := meta.FindStatusCondition(second.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, secondCond)
	assert.Equal(t, metav1.ConditionFalse, secondCond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentUIActionToolNotRequested, secondCond.Reason)
	assert.Nil(t, second.Status.Hooks,
		"Gate A failing must ALSO clear the previously-observed hook table — a different branch in Reconcile from validateDeclaration's own clear")
}

func TestReconcileMarksAValidDeclarationValid(t *testing.T) {
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{{
				Name: "root",
				Default: jsonNode(t, `{"component":"ap:stack","children":[
					{"component":"ap:heading","props":{"text":"Leads"}}
				]}`),
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "leads-console")

	got := getUI(t, c, "default", "leads-console")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, cond, "Valid condition must be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "well-formed default must validate")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentUISpecOK, cond.Reason)
}

func TestReconcileMarksAnUnknownComponentInvalid(t *testing.T) {
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{{
				Name:    "root",
				Default: jsonNode(t, `{"component":"ap:nope"}`),
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "leads-console")

	got := getUI(t, c, "default", "leads-console")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, cond, "Valid condition must be set")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "an unregistered component type must be rejected")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentUIInvalidDefault, cond.Reason)
	assert.Contains(t, cond.Message, "ap:nope", "message must carry the ValidationError's Reason (names the bad component)")
	assert.Contains(t, cond.Message, "view.children[0]", "message must carry the ValidationError's real Path into the COMPILED view — the non-writable slot's default sits directly at the root's first child")
}

func TestReconcileInvalidDefaultPathNamesTheRealSlotIndex(t *testing.T) {
	// Slot 0 is well-formed; slot 1 is the one that fails. The reported path
	// must say view.children[1] — proving it carries Validate's REAL index
	// into the COMPILED view built from the CR's real slot list, not a
	// hardcoded index from a per-slot wrapper (a per-slot wrapper's own
	// one-node parse would report index 0 regardless of which real slot
	// failed).
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "first", Default: jsonNode(t, `{"component":"ap:heading","props":{"text":"ok"}}`)},
				{Name: "second", Default: jsonNode(t, `{"component":"ap:nope"}`)},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "leads-console")

	got := getUI(t, c, "default", "leads-console")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "view.children[1]", "path must name the SECOND slot's real index in the compiled view")
	assert.NotContains(t, cond.Message, "view.children[0]", "the first, valid slot must not be blamed for the second slot's failure")
}

func TestReconcileRejectsDuplicateHookNamesFromWritableSlots(t *testing.T) {
	// Both slots are agentWritable, so both compile into hooks — CompileSlots
	// gives a non-writable slot with no default no hook at all (nothing to
	// address), so a writable/writable name collision is the only shape of
	// duplicate the shim can still produce.
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "root", AgentWritable: true},
				{Name: "root", AgentWritable: true},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "leads-console")

	got := getUI(t, c, "default", "leads-console")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"two writable slots sharing a name compile into two hooks of the same name, which the hook contract rejects as ambiguous")
	assert.Contains(t, cond.Message, "duplicate hook name")
}

func TestReconcileRejectsAWritableSlotWithNoName(t *testing.T) {
	// AgentWritable, so the shim compiles it into a hook rather than dropping
	// it: a non-writable nameless slot with no default now contributes
	// nothing to the compiled view at all, so it is no longer a rejection —
	// only an addressable-but-nameless region still is.
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "", AgentWritable: true},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "leads-console")

	got := getUI(t, c, "default", "leads-console")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"a nameless hook is unaddressable by the agent and must be rejected even with no Default to validate")
	assert.Contains(t, cond.Message, "missing hook name")
}

func TestReconcileRejectsNodeCountExceedingTheBoundOnlyInAggregate(t *testing.T) {
	// uicomponents.DefaultOptions's MaxNodes is 512. Each slot below is 300
	// nodes — comfortably under the bound alone — but 600 together exceed
	// it. Validating one slot's Declaration at a time (the pre-fix
	// behavior) would pass both independently; this only fails if MaxNodes
	// is enforced across the whole declaration, matching Plan 1's Task 2
	// hoisting the node counter above the slot loop for exactly this reason.
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "left", Default: manyLeafNodesJSON(t, 300)},
				{Name: "right", Default: manyLeafNodesJSON(t, 300)},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "leads-console")

	got := getUI(t, c, "default", "leads-console")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"600 nodes split across two 300-node slots must be bounded by ONE declaration-wide MaxNodes, not 512 per slot")
	assert.Contains(t, cond.Message, "maximum node count exceeded")
}

func TestReconcileSlotDefaultValidityIsUnaffectedByAgentWritable(t *testing.T) {
	cases := []struct {
		name          string
		agentWritable bool
	}{
		{name: "agentWritable=false (author-owned, the default posture)", agentWritable: false},
		{name: "agentWritable=true (agent may overwrite at runtime)", agentWritable: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)
			cr := &spiceboxv1alpha1.AgentUI{
				ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
				Spec: spiceboxv1alpha1.AgentUISpec{
					Slots: []spiceboxv1alpha1.AgentUISlot{{
						Name:          "root",
						AgentWritable: tc.agentWritable,
						Default:       jsonNode(t, `{"component":"ap:text","props":{"text":"static"}}`),
					}},
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
			r := &agentui.Reconciler{Client: c}

			reconcileNN(t, r, "default", "leads-console")

			got := getUI(t, c, "default", "leads-console")
			cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionTrue, cond.Status, "AgentWritable must not affect Tier-0 default validity")
			assert.Equal(t, spiceboxv1alpha1.ReasonAgentUISpecOK, cond.Reason)
		})
	}
}

func TestReconcileObservesTheEligibleToolsCeilingFromTheAgentClass(t *testing.T) {
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			View: minimalPage(),
			// advance_stage is requested but never granted below: must NOT survive.
			Tools: []string{"list_leads", "advance_stage"},
		},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{
				Ref:          "leads-console",
				GrantedTools: []string{"list_leads"},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, ac).WithStatusSubresource(cr).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "leads-console")

	got := getUI(t, c, "default", "leads-console")
	assert.Equal(t, []string{"list_leads"}, got.Status.EligibleTools,
		"eligibleTools must be the intersection of the UI's request and the AgentClass's grant, not either input alone")

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "a page binding nothing is valid whatever the ceiling drops")
}

func TestReconcileEligibleToolsIsTheUnionAcrossMultipleReferencingAgentClasses(t *testing.T) {
	// Nothing prevents two deployments from sharing one AgentUI with different
	// grants, and there is no single correct "the grant" to pick between them.
	// The union is correct here specifically BECAUSE this field is a ceiling:
	// a tool absent from every referencing class is one none of them could
	// ever authorize, so union (not an arbitrary pick, and not an
	// intersection across classes) is sound in the REJECT direction for this
	// field's one purpose — gating which tool names a Tier-0 default may bind
	// to. (Not "fail-closed": the union is the LOOSEST choice among the
	// referencing classes, the opposite of what fail-closed would mean in the
	// accept direction.)
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentUISpec{View: minimalPage(), Tools: []string{"list_leads", "advance_stage"}},
	}
	acA := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-agent-a", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: "leads-console", GrantedTools: []string{"list_leads"}},
		},
	}
	acB := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-agent-b", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: "leads-console", GrantedTools: []string{"advance_stage"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, acA, acB).WithStatusSubresource(cr).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "leads-console")

	got := getUI(t, c, "default", "leads-console")
	assert.Equal(t, []string{"advance_stage", "list_leads"}, got.Status.EligibleTools)
}

func TestReconcileWritesNoEligibleToolsWhenTheAgentClassGrantsNone(t *testing.T) {
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentUISpec{View: minimalPage(), Tools: []string{"list_leads"}},
	}
	// An AgentClass exists and references this AgentUI, but grants nothing.
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: "leads-console"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, ac).WithStatusSubresource(cr).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "leads-console")

	got := getUI(t, c, "default", "leads-console")
	assert.Empty(t, got.Status.EligibleTools, "no grant on the AgentClass ⇒ nothing enters the ceiling")

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "an empty grant is not itself a validity failure")
}

func TestReconcileWritesNoEligibleToolsWhenNoAgentClassReferencesIt(t *testing.T) {
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentUISpec{View: minimalPage(), Tools: []string{"list_leads"}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "leads-console")

	got := getUI(t, c, "default", "leads-console")
	assert.Empty(t, got.Status.EligibleTools, "no referencing AgentClass at all ⇒ an empty ceiling")
}

func TestReconcileRejectsASlotDefaultBoundToAnUngrantedTool(t *testing.T) {
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Tools: []string{"list_leads"},
			Slots: []spiceboxv1alpha1.AgentUISlot{{
				Name: "root",
				Default: jsonNode(t, `{"component":"ap:metric","bindings":{
					"value":{"source":"tool","ref":"list_leads"}
				}}`),
			}},
		},
	}
	// No AgentClass references this AgentUI, so the ceiling is empty — the
	// binding above must be rejected at reconcile.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "leads-console")

	got := getUI(t, c, "default", "leads-console")
	assert.Empty(t, got.Status.EligibleTools)
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"a Tier-0 default binding to a tool outside the eligible-tools ceiling must be rejected at reconcile, not left for click time")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentUIInvalidDefault, cond.Reason)
	assert.Contains(t, cond.Message, "list_leads")
}

func TestReconcileAllowsASlotDefaultBoundToAGrantedTool(t *testing.T) {
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Tools: []string{"list_leads"},
			Slots: []spiceboxv1alpha1.AgentUISlot{{
				Name: "root",
				Default: jsonNode(t, `{"component":"ap:metric","bindings":{
					"value":{"source":"tool","ref":"list_leads"}
				}}`),
			}},
		},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: "leads-console", GrantedTools: []string{"list_leads"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, ac).WithStatusSubresource(cr).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "leads-console")

	got := getUI(t, c, "default", "leads-console")
	assert.Equal(t, []string{"list_leads"}, got.Status.EligibleTools)
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "requested + within the eligible-tools ceiling must validate")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentUISpecOK, cond.Reason)
}

// TestReconcileNormalizesBindingRefBeforeMatchingTheEligibleToolsCeiling
// covers the normalize seam: spec.tools and grantedTools are both already
// normalized ("widgets_createissue" — the CRD pattern for both fields is
// synthesize.NormalizeName's output alphabet), but a slot's Default binding
// ref lives inside opaque JSON with no such pattern, so an author can still
// write a camelCase ref ("widgets_createIssue"). Without
// opts.NormalizeToolName set to synthesize.NormalizeName, that ref would
// miss the lowercase ceiling and this AgentUI would wrongly report
// Valid=False even though the tool IS eligible.
func TestReconcileNormalizesBindingRefBeforeMatchingTheEligibleToolsCeiling(t *testing.T) {
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets-console", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Tools: []string{"widgets_createissue"},
			Slots: []spiceboxv1alpha1.AgentUISlot{{
				Name: "root",
				Default: jsonNode(t, `{"component":"ap:table","bindings":{
					"rows":{"source":"tool","ref":"widgets_createIssue"}
				}}`),
			}},
		},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: "widgets-console", GrantedTools: []string{"widgets_createissue"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, ac).WithStatusSubresource(cr).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "widgets-console")

	got := getUI(t, c, "default", "widgets-console")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"the camelCase binding ref must normalize onto the lowercase eligible-tools ceiling")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentUISpecOK, cond.Reason)
}

func TestReconcileSetsToolsGrantedTrueWhenEveryRequestedToolIsEligible(t *testing.T) {
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentUISpec{View: minimalPage(), Tools: []string{"list_leads"}},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: "leads-console", GrantedTools: []string{"list_leads"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, ac).WithStatusSubresource(cr).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "leads-console")

	got := getUI(t, c, "default", "leads-console")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionToolsGranted)
	require.NotNil(t, cond, "ToolsGranted must be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentUIToolsFullyGranted, cond.Reason)
}

func TestReconcileSetsToolsGrantedFalseCarryingExplainCeilingWhenPartiallyGranted(t *testing.T) {
	// Regression coverage for uigrant.Explain/ExplainCeiling having zero
	// callers: before this, a bundle requesting a tool no class grants
	// produced no condition, no event, nothing.
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentUISpec{View: minimalPage(), Tools: []string{"list_leads", "advance_stage"}},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			// advance_stage is requested but never granted.
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: "leads-console", GrantedTools: []string{"list_leads"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, ac).WithStatusSubresource(cr).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "leads-console")

	got := getUI(t, c, "default", "leads-console")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionToolsGranted)
	require.NotNil(t, cond, "ToolsGranted must be set")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentUIToolsPartiallyGranted, cond.Reason)
	assert.Contains(t, cond.Message, "advance_stage")
	assert.Contains(t, cond.Message, "deployment grant")
	assert.NotContains(t, cond.Message, `"list_leads"`, "a fully-eligible tool needs no explanation in the message")
}

func TestReconcileToolsGrantedMessageIsIdempotentAcrossMultipleUngrantedTools(t *testing.T) {
	// formatExplanation must sort ExplainCeiling's map keys before joining;
	// an unsorted join would make the Message differ run to run for
	// identical inputs (Go does not guarantee stable map iteration order),
	// defeating only-changed-writes and repatching status on every
	// reconcile even though nothing actually changed.
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentUISpec{View: minimalPage(), Tools: []string{"list_leads", "advance_stage", "query_series"}},
	}
	// No AgentClass references this AgentUI: none of the three tools are granted.
	var statusPatchCount int
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if sub == "status" {
					statusPatchCount++
				}
				return cl.Status().Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "leads-console")
	require.Equal(t, 1, statusPatchCount, "first reconcile must write status once")
	first := getUI(t, c, "default", "leads-console")

	for i := 0; i < 5; i++ {
		reconcileNN(t, r, "default", "leads-console")
	}
	assert.Equal(t, 1, statusPatchCount, "5 more reconciles of unchanged spec must not repatch status")

	second := getUI(t, c, "default", "leads-console")
	assert.Equal(t, first.Status, second.Status, "status, including the ToolsGranted message, must be byte-identical")
}

func TestReconcileIsIdempotent(t *testing.T) {
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Tools: []string{"list_leads"},
			// AgentWritable: the idempotency claim is about status as a
			// whole, and status.hooks is part of that whole — a non-writable
			// slot compiles to no hook at all (see CompileSlots), so it
			// would leave status.hooks nil on both reconciles and prove
			// nothing about THAT field's idempotency.
			Slots: []spiceboxv1alpha1.AgentUISlot{{
				Name:          "root",
				AgentWritable: true,
				Default:       jsonNode(t, `{"component":"ap:heading","props":{"text":"Leads"}}`),
			}},
		},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: "leads-console", GrantedTools: []string{"list_leads"}},
		},
	}

	var statusPatchCount int
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, ac).WithStatusSubresource(cr).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if sub == "status" {
					statusPatchCount++
				}
				return cl.Status().Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "leads-console")
	require.Equal(t, 1, statusPatchCount, "first reconcile must write status once")
	first := getUI(t, c, "default", "leads-console")
	require.NotEmpty(t, first.Status.Hooks, "the writable slot must have compiled into an observed hook")

	reconcileNN(t, r, "default", "leads-console")
	assert.Equal(t, 1, statusPatchCount, "second reconcile of unchanged spec must NOT write status again")

	second := getUI(t, c, "default", "leads-console")
	assert.Equal(t, first.Status, second.Status, "status must be byte-identical across idempotent reconciles")
}

func TestReconcileGrantResolveFailureLeavesTheCeilingUntouchedAndReportsUnknown(t *testing.T) {
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentUISpec{View: minimalPage(), Tools: []string{"list_leads"}},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: "leads-console", GrantedTools: []string{"list_leads"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, ac).WithStatusSubresource(cr).
		WithInterceptorFuncs(listFailsFor[*spiceboxv1alpha1.AgentClassList]("simulated apiserver outage")).
		Build()
	r := &agentui.Reconciler{Client: c}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "leads-console"}})
	assert.Error(t, err, "a List failure must be returned, not silently swallowed, so it is retried")

	got := getUI(t, c, "default", "leads-console")
	assert.Empty(t, got.Status.EligibleTools,
		"a NEVER-yet-observed ceiling stays empty (there is no prior value to preserve); this is not a fail-closed DENIAL derived from the unread input")

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionUnknown, cond.Status,
		"an unresolved grant must report Unknown, never a False verdict derived from an input this reconcile never read")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentUIGrantUnresolved, cond.Reason)
}

func TestReconcileGrantResolveFailureDoesNotFlipAHealthyObjectToFalse(t *testing.T) {
	// The regression this guards: a healthy AgentUI
	// (eligible=[list_leads], Valid=True) must NOT become
	// eligible=[] Valid=False on a purely transient List blip — that would
	// send an operator hunting a grant problem that does not exist, and
	// status.eligibleTools is explicitly not an authorization input, so
	// there is nothing gained by denying on an unread input.
	scheme := newScheme(t)
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentUISpec{View: minimalPage(), Tools: []string{"list_leads"}},
		Status: spiceboxv1alpha1.AgentUIStatus{
			EligibleTools: []string{"list_leads"},
			Conditions: []metav1.Condition{
				{
					Type: spiceboxv1alpha1.AgentUIConditionValid, Status: metav1.ConditionTrue,
					Reason: spiceboxv1alpha1.ReasonAgentUISpecOK, LastTransitionTime: metav1.Now(),
				},
				{
					Type: spiceboxv1alpha1.AgentUIConditionToolsGranted, Status: metav1.ConditionTrue,
					Reason: spiceboxv1alpha1.ReasonAgentUIToolsFullyGranted, LastTransitionTime: metav1.Now(),
				},
			},
		},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: "leads-console", GrantedTools: []string{"list_leads"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, ac).WithStatusSubresource(cr).
		WithInterceptorFuncs(listFailsFor[*spiceboxv1alpha1.AgentClassList]("simulated apiserver outage")).
		Build()
	r := &agentui.Reconciler{Client: c}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "leads-console"}})
	assert.Error(t, err)

	got := getUI(t, c, "default", "leads-console")
	assert.Equal(t, []string{"list_leads"}, got.Status.EligibleTools,
		"the last successfully-observed ceiling must be preserved, not wiped, on a transient resolve failure")

	valid := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, valid)
	assert.Equal(t, metav1.ConditionUnknown, valid.Status,
		"a previously-healthy object must go Unknown, never a False verdict manufactured from an unread input")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentUIGrantUnresolved, valid.Reason)

	toolsGranted := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionToolsGranted)
	require.NotNil(t, toolsGranted)
	assert.Equal(t, metav1.ConditionTrue, toolsGranted.Status,
		"ToolsGranted must also be left untouched — it was never recomputed this reconcile")
}

// TestAgentUIValidatesTheActionTable covers Plan 5 Task 2's controller-side
// wiring: spec.Actions travels through the SAME Validate call as the page
// (validateDeclaration assembles one uicomponents.Declaration for both), so
// an action naming an ungranted tool is a reconcile-time Valid=False, not a
// dead button discovered only at click time. Both cases request the action's
// tool via spec.Tools so Gate A (uigrant.UnrequestedActionTools, run before
// validateDeclaration) is satisfied and the case actually exercises the
// grant check below it, rather than Gate A's own ActionToolNotRequested
// reason.
func TestAgentUIValidatesTheActionTable(t *testing.T) {
	cases := []struct {
		name      string
		tools     []string
		actions   []spiceboxv1alpha1.AgentUIAction
		wantValid metav1.ConditionStatus
		wantInMsg string
	}{
		{
			name:      "an action on a GRANTED mutating tool keeps Valid=True",
			tools:     []string{"demo_advance"},
			actions:   []spiceboxv1alpha1.AgentUIAction{{Name: "advance", Tool: "demo_advance"}},
			wantValid: metav1.ConditionTrue,
		},
		{
			name:      "an action on a REQUESTED but UNGRANTED tool sets Valid=False naming the tool",
			tools:     []string{"demo-ungranted"},
			actions:   []spiceboxv1alpha1.AgentUIAction{{Name: "advance", Tool: "demo-ungranted"}},
			wantValid: metav1.ConditionFalse,
			wantInMsg: "demo-ungranted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)
			cr := &spiceboxv1alpha1.AgentUI{
				ObjectMeta: metav1.ObjectMeta{Name: "leads-console", Namespace: "default"},
				Spec: spiceboxv1alpha1.AgentUISpec{
					View:    minimalPage(),
					Tools:   tc.tools,
					Actions: tc.actions,
				},
			}
			ac := &spiceboxv1alpha1.AgentClass{
				ObjectMeta: metav1.ObjectMeta{Name: "leads-agent", Namespace: "default"},
				Spec: spiceboxv1alpha1.AgentClassSpec{
					AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: "leads-console", GrantedTools: []string{"demo_advance"}},
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, ac).WithStatusSubresource(cr).Build()
			r := &agentui.Reconciler{Client: c}

			reconcileNN(t, r, "default", "leads-console")

			got := getUI(t, c, "default", "leads-console")
			cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
			require.NotNil(t, cond)
			assert.Equal(t, tc.wantValid, cond.Status)
			if tc.wantInMsg != "" {
				assert.Contains(t, cond.Message, tc.wantInMsg)
			}
		})
	}
}

// --- J1: TestConsoleShapedAgentUIReconciles -------------------------------

// pipelineDeskConsole builds the AgentUI + AgentClass pair the reconciler
// sees for a console shaped like the task's target: an ap:daterange
// ("window") and an ap:select ("stage") drive two READONLY data bindings
// (an ap:table's rows, an ap:chart's data), plus an ap:form that fires one
// side-effecting action ("advance", tool "deals_advance_stage"). Every
// mutation in TestConsoleShapedAgentUIReconciles starts from a fresh copy of
// this pair and breaks exactly one thing. Fabricated names only.
func pipelineDeskConsole(t *testing.T) (*spiceboxv1alpha1.AgentUI, *spiceboxv1alpha1.AgentClass) {
	t.Helper()
	cr := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: "pipeline-desk-ui", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Tools: []string{"deals_list_leads", "deals_pipeline_summary", "deals_advance_stage"},
			Actions: []spiceboxv1alpha1.AgentUIAction{{
				Name:   "advance",
				Tool:   "deals_advance_stage",
				Args:   jsonNode(t, `{"stage":{"$param":"stage"}}`),
				Inputs: []string{"note"},
			}},
			Slots: []spiceboxv1alpha1.AgentUISlot{{
				Name: "root",
				Default: jsonNode(t, `{"component":"ap:stack","children":[
					{"component":"ap:select","props":{"param":"stage","options":[{"value":"open","label":"Open"},{"value":"won","label":"Won"}]}},
					{"component":"ap:daterange","props":{"param":"window"}},
					{"component":"ap:table","props":{"columns":[{"key":"account"}]},"bindings":{"rows":{
						"source":"tool","ref":"deals_list_leads","args":{"stage":{"$param":"stage"}}}}},
					{"component":"ap:chart","props":{"kind":"bar","xKey":"month"},"bindings":{"data":{
						"source":"tool","ref":"deals_pipeline_summary","args":{"from":{"$param":"window.from"},"to":{"$param":"window.to"}}}}},
					{"component":"ap:form","props":{"action":"advance","fields":[{"name":"note"}]}}
				]}`),
			}},
		},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "pipeline-desk", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{
				Ref:          "pipeline-desk-ui",
				GrantedTools: []string{"deals_list_leads", "deals_pipeline_summary", "deals_advance_stage"},
			},
		},
	}
	return cr, ac
}

// rawJSON wraps a JSON literal for a mutation function — the mutation table's
// `mutate` signature carries no *testing.T, unlike jsonNode.
func rawJSON(raw string) *apiextensionsv1.JSON { return &apiextensionsv1.JSON{Raw: []byte(raw)} }

// dropActionInput removes the action's only declared Input ("note") while
// the oap:form still supplies a "note" field — the control/action Inputs
// containment check (validateActions' ActionRefs loop) must reject this.
func dropActionInput(ui *spiceboxv1alpha1.AgentUI, _ *spiceboxv1alpha1.AgentClass) {
	ui.Spec.Actions[0].Inputs = nil
}

// renameInputToParamKey renames the action's Input from "note" to "stage" —
// "stage" is already a declared binding parameter (the oap:select), so this
// must trip the pre-existing Inputs/ParamKeys disjointness check.
func renameInputToParamKey(ui *spiceboxv1alpha1.AgentUI, _ *spiceboxv1alpha1.AgentClass) {
	ui.Spec.Actions[0].Inputs = []string{"stage"}
}

// typoActionArgsParam misspells the action's args placeholder ("stage" ->
// "stagee") — Gate B (validateActions' per-action placeholder check) must
// reject a reference nothing in ParamKeys ∪ Inputs supplies.
func typoActionArgsParam(ui *spiceboxv1alpha1.AgentUI, _ *spiceboxv1alpha1.AgentClass) {
	ui.Spec.Actions[0].Args = rawJSON(`{"stage":{"$param":"stagee"}}`)
}

// dropActionToolFromSpecTools removes "deals_advance_stage" from spec.Tools
// while it stays granted on the AgentClass and named by the action — Gate A
// (uigrant.UnrequestedActionTools, run before validateDeclaration) must
// reject this: the tool is granted but never REQUESTED, so it can never enter
// Loop.AppTools (see the reachability chain UnrequestedActionTools' own doc
// comment cites), and the action is dead on every click.
func dropActionToolFromSpecTools(ui *spiceboxv1alpha1.AgentUI, _ *spiceboxv1alpha1.AgentClass) {
	ui.Spec.Tools = []string{"deals_list_leads", "deals_pipeline_summary"}
}

// TestConsoleShapedAgentUIReconciles is J1: it drives the REAL reconciler
// over a declaration with the console's exact shape (pipelineDeskConsole)
// and then breaks it one way at a time. The happy row alone would prove
// nothing — the defect class this task exists for is a document that
// validates and is dead.
//
// One row named in the task brief is deliberately ABSENT: "a data binding
// naming the MUTATING tool" (bindTableRowsToTheMutation). validateDeclaration
// passes eligibleTools for BOTH opts.GrantedTools and opts.ReadonlyTools —
// literally the same map — so a binding's readonly check
// (!o.ReadonlyTools[ref] in validateBindings) can never fire at this layer:
// whatever passes the grant check trivially passes the identical readonly
// check too. This reconciler is namespace-scoped and has no session, so it
// can see neither a tool's StateImpact nor its origin's readOnlyHint — the
// two halves of the real readonly predicate — which is exactly why
// validateDeclaration's own doc comment says the check is vacuous here BY
// DESIGN.
// The real, unconditional gate is runner.UIToolOptions, already covered by
// Task 1's J2 test. Shipping this row here would promise a check the
// reconciler does not perform.
func TestConsoleShapedAgentUIReconciles(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*spiceboxv1alpha1.AgentUI, *spiceboxv1alpha1.AgentClass)
		wantStatus metav1.ConditionStatus
		wantReason string
		wantMsg    string // substring
	}{
		{name: "the console shape reconciles Valid=True/SpecOK",
			mutate: nil, wantStatus: metav1.ConditionTrue, wantReason: spiceboxv1alpha1.ReasonAgentUISpecOK},
		{name: "a form field absent from the action's inputs: Valid=False/InvalidDefault",
			mutate: dropActionInput, wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonAgentUIInvalidDefault, wantMsg: "which action"},
		{name: "an action input colliding with a param key: Valid=False/InvalidDefault",
			mutate: renameInputToParamKey, wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonAgentUIInvalidDefault, wantMsg: "collides with a binding parameter"},
		{name: "an action args placeholder naming nothing: Valid=False/InvalidDefault",
			mutate: typoActionArgsParam, wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonAgentUIInvalidDefault, wantMsg: "reference a value nothing supplies"},
		{name: "an action tool absent from spec.tools: Valid=False/ActionToolNotRequested",
			mutate: dropActionToolFromSpecTools, wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonAgentUIActionToolNotRequested, wantMsg: "Add each to spec.tools"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newScheme(t)
			cr, ac := pipelineDeskConsole(t)
			if tc.mutate != nil {
				tc.mutate(cr, ac)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, ac).WithStatusSubresource(cr).Build()
			r := &agentui.Reconciler{Client: c}

			reconcileNN(t, r, "default", "pipeline-desk-ui")

			got := getUI(t, c, "default", "pipeline-desk-ui")
			cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
			require.NotNil(t, cond, "Valid condition must be set")
			assert.Equal(t, tc.wantStatus, cond.Status)
			assert.Equal(t, tc.wantReason, cond.Reason)
			if tc.wantMsg != "" {
				assert.Contains(t, cond.Message, tc.wantMsg)
			}
		})
	}
}

// TestEveryValidAgentUIHasItsActionToolsInEligibleTools asserts the property
// Gate A buys, rather than merely claiming it in a comment: once the console
// fixture is Valid=True, EVERY normalized spec.Actions[*].Tool is present in
// status.eligibleTools. That is why validateDeclaration needs no separate
// action-table ceiling — Gate A already guarantees an action reaching
// Valid=True has its tool both requested in spec.Tools and granted by the
// AgentClass, which is exactly membership in eligibleTools.
func TestEveryValidAgentUIHasItsActionToolsInEligibleTools(t *testing.T) {
	scheme := newScheme(t)
	cr, ac := pipelineDeskConsole(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, ac).WithStatusSubresource(cr).Build()
	r := &agentui.Reconciler{Client: c}

	reconcileNN(t, r, "default", "pipeline-desk-ui")

	got := getUI(t, c, "default", "pipeline-desk-ui")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status, "the fixture must be Valid before this property means anything")

	eligible := make(map[string]bool, len(got.Status.EligibleTools))
	for _, name := range got.Status.EligibleTools {
		eligible[name] = true
	}
	for _, a := range got.Spec.Actions {
		assert.True(t, eligible[synthesize.NormalizeName(a.Tool)],
			"action %q's tool %q must be in status.eligibleTools once Valid=True", a.Name, a.Tool)
	}
}

func TestMapAgentClassToAgentUIsReenqueuesTheReferencedAgentUI(t *testing.T) {
	r := &agentui.Reconciler{}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: "leads-console"},
		},
	}
	got := r.MapAgentClassToAgentUIs(context.Background(), ac)
	require.Len(t, got, 1)
	assert.Equal(t, "leads-console", got[0].Name)
	assert.Equal(t, "default", got[0].Namespace)
}

func TestMapAgentClassToAgentUIsIgnoresAnAgentClassWithNoUIGrant(t *testing.T) {
	r := &agentui.Reconciler{}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "leads-agent", Namespace: "default"},
	}
	got := r.MapAgentClassToAgentUIs(context.Background(), ac)
	assert.Empty(t, got, "an AgentClass with no AgentUI grant must not enqueue anything")
}
