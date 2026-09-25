package agentui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/browserstart"
)

// This file is J2's spanning test: it proves the SAME merged view a runner
// write produces is what webd's own resolveDeclaration/ViewFor serve —
// through the REAL resolveView, not a stand-in — and that a grant narrowed
// underneath a stored fragment makes webd fall that slot back to Tier 0
// rather than serving (or 500ing on) a document the platform would no
// longer accept.

const (
	vmNS      = "demo-ns"
	vmSession = "demo-session"
	vmClass   = "demo-agent"
	vmUIName  = "demo-ui"
	vmSubject = "vm-subject"

	// vmToolBound/vmToolAction are the two tools the fixture's AgentUI both
	// REQUESTS (spec.tools) and the AgentClass both GRANTS
	// (spec.agentUI.grantedTools) — one bound by "panel"'s writable-slot
	// fragment (a data binding), one named by the "advance" action. Listed
	// in BOTH places deliberately: viewOptions' ceiling formula
	// (uigrant.Ceiling(spec.Tools, grantedTools)) does not additionally
	// union in an action's own tool the way the AgentUI reconciler's
	// Tier-0 validation does (see viewOptions' doc comment) — putting the
	// action's tool in spec.Tools too keeps this fixture on the side of
	// that gap validateActions would otherwise trip on.
	vmToolBound  = "crm_list_leads"
	vmToolAction = "demo_advance_stage"
)

// vmLogSink is where newAgentUIFixture's Deps.Logger writes, and what
// logSink(t) reads back. A package-level var, not a fixture field, so
// logSink(t) can match the brief's own one-argument shape; tests in this
// file run sequentially (no t.Parallel()), and t.Cleanup clears it, so at
// most one is ever active.
var vmLogSink *strings.Builder

// logSink returns the log buffer the most recent newAgentUIFixture(t)
// installed.
func logSink(t *testing.T) *strings.Builder {
	t.Helper()
	require.NotNil(t, vmLogSink, "logSink: call newAgentUIFixture first")
	return vmLogSink
}

// vmFakeDeps implements agentui.Deps for this file's own fixture. Origin/
// NATS/Artifacts collaborators stay at their zero value — none of these
// tests reach a POST route or the live route, only ViewFor/
// resolveDeclaration.
type vmFakeDeps struct {
	k8s    client.Client
	mem    memory.Memory
	logger logr.Logger
}

func (d *vmFakeDeps) CheckInteract(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (d *vmFakeDeps) K8s() client.Client                                      { return d.k8s }
func (d *vmFakeDeps) Logger() logr.Logger                                     { return d.logger }
func (d *vmFakeDeps) TrustedOrigin() string                                   { return "" }
func (d *vmFakeDeps) NATSRequest() channelevents.RequestFunc                  { return nil }
func (d *vmFakeDeps) Memory() memory.Memory                                   { return d.mem }
func (d *vmFakeDeps) Artifacts() *artifacts.Service                           { return nil }
func (d *vmFakeDeps) ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc { return nil }
func (d *vmFakeDeps) NATS() *nats.Conn                                        { return nil }

// StartBrowserSession is nil: no test in this file reaches a POST route,
// only ViewFor/resolveDeclaration.
func (d *vmFakeDeps) StartBrowserSession() browsersession.StartFunc { return nil }

// LiveSessions is nil for the same reason: no test here reaches a POST route.
func (d *vmFakeDeps) LiveSessions() browserstart.LiveSessions { return nil }
func (d *vmFakeDeps) StartableNamespaces() []string           { return nil }
func (d *vmFakeDeps) WorkshopNamespacesFor(context.Context, string) ([]string, error) {
	return nil, nil
}

var _ Deps = (*vmFakeDeps)(nil)

// newAgentUIFixture builds ONE memory.Memory shared by webd's Deps and the
// runner's uiview.Runtime — the load-bearing property of the J2 span: a
// fragment written through the Runtime must be readable through Deps
// without any bridging.
//
// The AgentUI declares two slots — "panel" (agentWritable, Tier-0 default
// ap:text) and "notes" (not writable, Tier-0 default an ap:select
// declaring binding parameter "span" — the SAME name "panel"'s tool
// binding fragments reference via {"$param":"span"}, so a written fragment
// naming it validates) — and one action ("advance" -> vmToolAction). The
// AgentClass grants both vmToolBound and vmToolAction.
func newAgentUIFixture(t *testing.T) (context.Context, Deps, *uiview.Runtime) {
	t.Helper()

	scheme := k8sruntime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: vmSession, Namespace: vmNS},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: vmClass},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: vmClass, Namespace: vmNS},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{
				Ref:          vmUIName,
				GrantedTools: []string{vmToolBound, vmToolAction},
			},
		},
	}
	aui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: vmUIName, Namespace: vmNS},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Tools: []string{vmToolBound, vmToolAction},
			Actions: []spiceboxv1alpha1.AgentUIAction{
				{Name: "advance", Tool: vmToolAction},
			},
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{
					Name:          "panel",
					AgentWritable: true,
					Default:       &apiextensionsv1.JSON{Raw: []byte(`{"component":"ap:text","props":{"text":"placeholder"}}`)},
				},
				{
					Name:    "notes",
					Default: &apiextensionsv1.JSON{Raw: []byte(`{"component":"ap:select","props":{"param":"span","value":"30d"}}`)},
				},
			},
		},
		Status: spiceboxv1alpha1.AgentUIStatus{Conditions: []metav1.Condition{{
			Type:   spiceboxv1alpha1.AgentUIConditionValid,
			Status: metav1.ConditionTrue,
			Reason: spiceboxv1alpha1.ReasonAgentUISpecOK,
		}}},
	}

	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sess, ac, aui).Build()
	mem := &fakeUIActionMemory{}

	var sb strings.Builder
	vmLogSink = &sb
	t.Cleanup(func() { vmLogSink = nil })
	logger := funcr.New(func(prefix, args string) {
		sb.WriteString(prefix)
		sb.WriteString(" ")
		sb.WriteString(args)
		sb.WriteString("\n")
	}, funcr.Options{})

	d := &vmFakeDeps{k8s: k8s, mem: mem, logger: logger}

	// ToolOptions mimics the RUNNER's own materialized grant (Loop.AppTools),
	// a DIFFERENT authority than webd's own viewOptions ceiling — Runtime.Write
	// validates against this, resolveDeclaration validates against the
	// latter. Both grant vmToolBound+vmToolAction initially, matching the
	// AgentClass/AgentUI fixture above, so the runner's write below succeeds
	// under "the current grant" as the brief's test names it.
	granted := map[string]bool{vmToolBound: true, vmToolAction: true}
	rt := &uiview.Runtime{
		Namespace: vmNS, Session: vmSession, UIName: vmUIName,
		Client: k8s, Mem: mem,
		ToolOptions: func() uicomponents.Options {
			o := uicomponents.DefaultOptions()
			o.GrantedTools = granted
			o.ReadonlyTools = granted
			return o
		},
	}

	ctx := webui.WithSubjectForTest(context.Background(), vmSubject)
	return ctx, d, rt
}

// shrinkGrant narrows the AgentClass's deployment grant to no longer
// include vmToolBound — a bundle redeploy or a consent change, the
// scenario resolveView's re-validation exists for.
func shrinkGrant(t *testing.T, d Deps, ns, agentClass string) {
	t.Helper()
	ctx := context.Background()
	var ac spiceboxv1alpha1.AgentClass
	require.NoError(t, d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: agentClass}, &ac))
	require.NotNil(t, ac.Spec.AgentUI, "shrinkGrant: fixture AgentClass carries no agentUI grant")
	ac.Spec.AgentUI.GrantedTools = []string{vmToolAction} // vmToolBound dropped
	require.NoError(t, d.K8s().Update(ctx, &ac))
}

// mustNode parses raw as a uicomponents.Node the same way update_view's own
// Execute does (uicomponents.ParseNode), for a Runtime.Write call.
func mustNode(t *testing.T, raw string) uicomponents.Node {
	t.Helper()
	n, err := uicomponents.ParseNode([]byte(raw))
	require.NoError(t, err)
	return n
}

// buildProps drives the REAL ViewFor — the same production path the session
// shell's viewFor takes for a selected session — and returns the marshalled
// ViewProps, so this file can assert on the declaration's wire shape without
// standing up a server.
func buildProps(t *testing.T, ctx context.Context, d Deps) []byte {
	t.Helper()
	props, _, avail, pe := ViewFor(ctx, d, vmNS, vmSession)
	require.Nil(t, pe, "the fixture graph must reach the happy path")
	require.Equal(t, UIAvailable, avail)
	out, err := json.Marshal(props)
	require.NoError(t, err)
	return out
}

// mustField extracts one top-level field's raw JSON.
func mustField(t *testing.T, raw []byte, field string) []byte {
	t.Helper()
	var obj map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &obj))
	v, ok := obj[field]
	require.True(t, ok, "props carry no %q field", field)
	return v
}

// hookNode locates the hook named name inside a page tree and returns the
// hook node ITSELF — its Children carry the current fill (a fragment's
// content, the Tier-0 default nothing has touched, or none for a hook the
// agent cleared). Shared by this file and live_test.go: both need to read a
// hook's content out of a resolved uicomponents.Declaration's View, or a
// live push's declarationWire.View, by the same rule uicomponents.Hooks
// itself uses (this package's tests are one compiled unit — a second copy of
// this walk would be exactly the duplication CLAUDE.md's DRY rule flags).
func hookNode(t *testing.T, view *uicomponents.Node, name string) uicomponents.Node {
	t.Helper()
	require.NotNil(t, view, "hookNode: declaration carries no view")
	for _, h := range uicomponents.Hooks(uicomponents.Declaration{View: view}) {
		if h.Name != name {
			continue
		}
		n := *view
		for _, i := range h.Path {
			n = n.Children[i]
		}
		return n
	}
	t.Fatalf("hookNode: no hook %q in view", name)
	return uicomponents.Node{}
}

// TestMergedViewSurvivesAndThenFallsBack is the J2 span. Half one: a
// fragment written through the RUNNER's own write path (uiview.Runtime.Write)
// is served by webd's REAL resolveDeclaration. Half two: the grant shrinks
// under it and webd falls the hook back to Tier 0 rather than serving it.
// Only the second half fails when the read-time re-validation is deleted —
// which is why a test that stops after half one proves nothing.
func TestMergedViewSurvivesAndThenFallsBack(t *testing.T) {
	ctx, d, rt := newAgentUIFixture(t)

	_, err := rt.Write(ctx, "panel", mustNode(t,
		`{"component":"ap:table","bindings":{"rows":{"source":"tool","ref":"`+vmToolBound+`","args":{"span":{"$param":"span"}}}}}`))
	require.NoError(t, err, "the runner accepts this fragment under the current grant")

	decl, pe := resolveDeclaration(ctx, d, vmNS, vmSession)
	require.Nil(t, pe)
	panel := hookNode(t, decl.View, "panel")
	require.Len(t, panel.Children, 1)
	assert.Equal(t, "ap:table", panel.Children[0].Component, "webd serves what the runner wrote")

	// The deployment narrows its grant — a bundle redeploy, a consent change.
	shrinkGrant(t, d, vmNS, vmClass) // grantedTools no longer includes vmToolBound

	decl, pe = resolveDeclaration(ctx, d, vmNS, vmSession)
	require.Nil(t, pe, "one stale fragment must not take the page down")
	panel = hookNode(t, decl.View, "panel")
	require.Len(t, panel.Children, 1)
	assert.Equal(t, "ap:text", panel.Children[0].Component, "the hook falls back to its Tier-0 default")
	assert.Contains(t, logSink(t).String(), "panel", "a dropped fragment MUST be logged with its hook")
}

// TestMergedViewIsWhatBindingsAndActionsResolveAgainst proves the merged
// declaration's action table is what actionsHandler's own decl.Action
// lookup would find — the property that says the merge went in at the
// right layer (resolveDeclaration), because if this passes and a POST to
// /actions still 400s, the merge is not actually shared.
func TestMergedViewIsWhatBindingsAndActionsResolveAgainst(t *testing.T) {
	ctx, d, rt := newAgentUIFixture(t)
	_, err := rt.Write(ctx, "panel", mustNode(t, `{"component":"ap:button","props":{"label":"Advance","action":"advance"}}`))
	require.NoError(t, err)

	decl, pe := resolveDeclaration(ctx, d, vmNS, vmSession)
	require.Nil(t, pe)
	_, found := decl.Action("advance")
	assert.True(t, found, "the action table travels with the merged declaration")

	// The control the agent wrote must be findable by the SAME lookup
	// actionsHandler performs; if this passes and a POST to /actions 400s,
	// the merge went in at the wrong layer.
	refs := uicomponents.ActionRefs(decl)
	require.Len(t, refs, 1)
	assert.Equal(t, "advance", refs[0].Action)
}

// TestViewAndBindingsServeTheSameMergedSlot is the explicit cross-surface
// span: ONE write through the runner's path (uiview.Runtime.Write), then
// BOTH the view-props surface (ViewFor, via buildProps) and the
// bindings/actions surface (resolveDeclaration) are read, and their "panel"
// hooks are asserted to carry the IDENTICAL component — not merely "both
// happen to be ap:table", but the same value read from two independently
// invoked code paths. A regression that reverts ONE surface to serving
// doors.UI.Spec directly (the raw Tier-0 CR, bypassing the merge) makes
// this test fail even though a Tier-0-only fixture (no fragment written —
// every golden/table test elsewhere in this package) could never tell the
// two apart, since raw and merged are identical with nothing written.
func TestViewAndBindingsServeTheSameMergedSlot(t *testing.T) {
	ctx, d, rt := newAgentUIFixture(t)
	_, err := rt.Write(ctx, "panel", mustNode(t, `{"component":"ap:markdown","props":{"body":"agent-written"}}`))
	require.NoError(t, err)

	decl, pe := resolveDeclaration(ctx, d, vmNS, vmSession)
	require.Nil(t, pe)
	panel := hookNode(t, decl.View, "panel")
	require.Len(t, panel.Children, 1)

	props := buildProps(t, ctx, d)
	var wire struct {
		View *uicomponents.Node `json:"view"`
	}
	require.NoError(t, json.Unmarshal(mustField(t, props, "declaration"), &wire))
	wirePanel := hookNode(t, wire.View, "panel")
	require.Len(t, wirePanel.Children, 1)

	assert.Equal(t, "ap:markdown", panel.Children[0].Component, "bindings/actions surface: resolveDeclaration")
	assert.Equal(t, "ap:markdown", wirePanel.Children[0].Component, "view-props surface: ViewFor")
	wirePanelProps, err := json.Marshal(wirePanel.Children[0].Props)
	require.NoError(t, err)
	assert.JSONEq(t, `{"body":"agent-written"}`, string(wirePanelProps),
		"the two surfaces must render the SAME agent-written content, not merely the same component type")
}

// TestViewPropsCarryAgentComposedAsAHookList proves the wire-only
// agentComposed marker (declarationWireFor) reaches the ACTUAL bootstrap
// props ViewFor sends, as the top-level hook list, and never names a hook
// the agent never wrote.
func TestViewPropsCarryAgentComposedAsAHookList(t *testing.T) {
	ctx, d, rt := newAgentUIFixture(t)
	_, err := rt.Write(ctx, "panel", mustNode(t, `{"component":"ap:markdown","props":{"body":"agent"}}`))
	require.NoError(t, err)

	props := buildProps(t, ctx, d) // drives the real ViewFor
	var wire struct {
		AgentComposed []string `json:"agentComposed"`
	}
	require.NoError(t, json.Unmarshal(mustField(t, props, "declaration"), &wire))
	assert.Equal(t, []string{"panel"}, wire.AgentComposed,
		`only the hook a fragment actually changed is listed — "notes" is not agentWritable and can never appear`)
}

// TestDeclarationWireCarriesTheViewAndNoSlots proves the wire document is
// the page TREE, never the legacy flat slot list — a fragment on "panel"
// still surfaces only through the top-level agentComposed hook list, never
// as a per-node field the tree itself could carry.
func TestDeclarationWireCarriesTheViewAndNoSlots(t *testing.T) {
	ctx, d, rt := newAgentUIFixture(t)
	raw := buildProps(t, ctx, d)
	var props struct {
		Declaration struct {
			View          json.RawMessage `json:"view"`
			Slots         json.RawMessage `json:"slots"`
			AgentComposed []string        `json:"agentComposed"`
		} `json:"declaration"`
	}
	require.NoError(t, json.Unmarshal(raw, &props))
	assert.NotEmpty(t, props.Declaration.View, "the wire is the tree")
	assert.Empty(t, props.Declaration.Slots, "slots never reach the browser; the shim compiled them")
	assert.Empty(t, props.Declaration.AgentComposed)
	_ = rt
}

// TestAgentComposedIsNotADeclarationField proves agentComposed lives ONLY
// on the wire type (declarationWire) — a fragment or a Tier-0
// declaration naming that key at all must be rejected by
// ParseNode/ParseDeclaration's DisallowUnknownFields, never silently
// accepted and dropped. The marker's whole purpose is that an agent's own
// write can never set or clear it; this is the type-level half of that
// guarantee (declarationWireFor computing it server-side, per read, is the
// other half — see viewmodel.go).
func TestAgentComposedIsNotADeclarationField(t *testing.T) {
	_, err := uicomponents.ParseNode([]byte(`{"component":"ap:text","agentComposed":false}`))
	assert.Error(t, err, "a fragment must not be able to name the marker's field at all")

	_, err = uicomponents.ParseDeclaration([]byte(`{"slots":[{"name":"a","agentComposed":false}]}`))
	assert.Error(t, err, "agentComposed lives on the WIRE type; a declaration carrying it must be rejected")
}

// This section is ViewFor's own coverage — the session shell (pkg/web/webui/
// sessions) is ViewFor's one caller and has its own, broader table
// (view_test.go), but ViewFor's contract (walkAgentUIDoors' one fork,
// UINotDeclared vs every other door, and chrome forwarding) is asserted
// directly here so it does not depend on the shell's own Ended short-circuit
// never calling it.

const (
	vwNS      = "wsl"
	vwSession = "sess-live"
	vwClass   = "wsl-class"
	vwUI      = "wsl-ui"
)

// vwSessionObj builds the named AgentSession (vwNS/vwSession) in the given
// phase.
func vwSessionObj(phase string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: vwNS, Name: vwSession},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: vwClass},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
	}
}

// vwClassObj builds the AgentClass ViewFor Gets by name (vwClass). grant nil
// exercises "no AgentUI on the class" — the fork ViewFor and
// resolveAgentUIDoors disagree on.
func vwClassObj(grant *spiceboxv1alpha1.AgentClassUIGrant) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: vwNS, Name: vwClass},
		Spec:       spiceboxv1alpha1.AgentClassSpec{AgentUI: grant},
	}
}

// vwUIObj builds the referenced AgentUI CR (vwUI) with one renderable slot,
// the given Valid condition, and — when chromeInitial is non-empty — a
// chrome request, proving Spec.Chrome still forwards into ViewFor's
// UIChromeRequest return — this is the row that covers that forwarding now
// that the page which used to carry it is gone (I4: deleting the forwarding
// left the suite green until a row like this existed).
func vwUIObj(status metav1.ConditionStatus, message, chromeInitial string) *spiceboxv1alpha1.AgentUI {
	aui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Namespace: vwNS, Name: vwUI},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "root", Default: &apiextensionsv1.JSON{Raw: []byte(`{"component":"ap:stack"}`)}},
			},
		},
		Status: spiceboxv1alpha1.AgentUIStatus{Conditions: []metav1.Condition{
			{Type: spiceboxv1alpha1.AgentUIConditionValid, Status: status, Reason: "Test", Message: message},
		}},
	}
	if chromeInitial != "" {
		aui.Spec.Chrome = &spiceboxv1alpha1.AgentUIChrome{InitialState: chromeInitial}
	}
	return aui
}

// newViewForDeps builds a *vmFakeDeps over a fake client seeded with objs,
// plus a working Tier-0-only memory backend (fakeUIActionMemory) — ViewFor's
// merge step requires a non-nil Memory even when nothing was ever written.
func newViewForDeps(t *testing.T, objs ...client.Object) *vmFakeDeps {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &vmFakeDeps{k8s: fc, mem: &fakeUIActionMemory{}, logger: logr.Discard()}
}

// TestViewFor_UIAvailable proves the happy path: a Valid=True AgentUI
// produces UIAvailable, a populated ViewProps carrying the merged
// declaration, and the AgentUI's own chrome request forwarded into
// UIChromeRequest.
func TestViewFor_UIAvailable(t *testing.T) {
	d := newViewForDeps(t, vwSessionObj(spiceboxv1alpha1.AgentSessionPhaseRunning),
		vwClassObj(&spiceboxv1alpha1.AgentClassUIGrant{Ref: vwUI}),
		vwUIObj(metav1.ConditionTrue, "", "collapsed"))

	props, chrome, avail, pe := ViewFor(context.Background(), d, vwNS, vwSession)
	require.Nil(t, pe)
	assert.Equal(t, UIAvailable, avail)
	assert.Equal(t, vwNS, props.Ns)
	assert.Equal(t, vwSession, props.Name)
	assert.Contains(t, string(props.Declaration), `"component":"ap:stack"`)
	assert.Equal(t, "collapsed", chrome.InitialState, "Spec.Chrome must still forward now that it lives in ViewFor")
}

// TestViewFor_UINotDeclared is the whole of D3 and the mutation this task's
// brief calls out by name: a class with no spec.agentUI returns
// (zero, UINotDeclared, nil) — NOT the 404 *webui.PageError
// resolveAgentUIDoors answers for the exact same doors state. Mutating
// ViewFor to return that 404 instead is what proves the shell is not just
// the old page with a sidebar (see pkg/web/webui/sessions/view_test.go's own
// "class declares no AgentUI" row, which fails Kind=="chat" under that
// mutation).
func TestViewFor_UINotDeclared(t *testing.T) {
	d := newViewForDeps(t, vwSessionObj(spiceboxv1alpha1.AgentSessionPhaseRunning), vwClassObj(nil))

	props, chrome, avail, pe := ViewFor(context.Background(), d, vwNS, vwSession)
	assert.Nil(t, pe, "no-UI-declared is not an error — see UINotDeclared's own doc comment")
	assert.Equal(t, UINotDeclared, avail)
	assert.Equal(t, ViewProps{}, props)
	assert.Equal(t, UIChromeRequest{}, chrome)
}

// TestViewFor_DoorErrors_MatchResolveAgentUIDoors proves ViewFor's OTHER
// doors — Valid=False here, plus the terminal (Ended) branch that
// resolveAgentUIDoors itself 410s on — carry the SAME status and copy
// resolveAgentUIDoors produces for an identical object graph, because both
// now walk the ONE shared ladder (walkAgentUIDoors). A second, drifted copy
// of the ladder is exactly the duplication resolveAgentUIDoors' own doc
// comment warns against; this is the regression test for that duplication
// creeping back in.
func TestViewFor_DoorErrors_MatchResolveAgentUIDoors(t *testing.T) {
	t.Run("AgentUI Valid=False: same 422 and author message as resolveAgentUIDoors", func(t *testing.T) {
		d := newViewForDeps(t, vwSessionObj(spiceboxv1alpha1.AgentSessionPhaseRunning),
			vwClassObj(&spiceboxv1alpha1.AgentClassUIGrant{Ref: vwUI}),
			vwUIObj(metav1.ConditionFalse, "slot root: unknown component ap:bogus", ""))

		_, _, avail, pe := ViewFor(context.Background(), d, vwNS, vwSession)
		require.NotNil(t, pe)
		assert.Equal(t, UIAvailable, avail, "a door FAILURE, not UINotDeclared — the UI IS declared, just invalid")
		assert.Equal(t, http.StatusUnprocessableEntity, pe.Status)
		assert.Equal(t, "slot root: unknown component ap:bogus", pe.Message)

		_, doorsPe := resolveAgentUIDoors(context.Background(), d, vwNS, vwSession)
		require.NotNil(t, doorsPe)
		assert.Equal(t, doorsPe.Status, pe.Status)
		assert.Equal(t, doorsPe.Title, pe.Title)
		assert.Equal(t, doorsPe.Message, pe.Message)
	})

	t.Run("session Ended: same 410 as resolveAgentUIDoors — this door is NOT the shell's Ended short-circuit", func(t *testing.T) {
		d := newViewForDeps(t, vwSessionObj(spiceboxv1alpha1.AgentSessionPhaseSucceeded),
			vwClassObj(&spiceboxv1alpha1.AgentClassUIGrant{Ref: vwUI}),
			vwUIObj(metav1.ConditionTrue, "", ""))

		_, _, _, pe := ViewFor(context.Background(), d, vwNS, vwSession)
		require.NotNil(t, pe)
		assert.Equal(t, http.StatusGone, pe.Status)

		_, doorsPe := resolveAgentUIDoors(context.Background(), d, vwNS, vwSession)
		require.NotNil(t, doorsPe)
		assert.Equal(t, doorsPe.Status, pe.Status)
		assert.Equal(t, doorsPe.Message, pe.Message)
	})
}
