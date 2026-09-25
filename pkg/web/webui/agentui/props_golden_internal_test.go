package agentui

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiviewmodel"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/webui/browserstart"
)

// goldenPropsPath is the ONE artifact both halves of the Go->React view seam
// read. This file asserts that ViewFor's own output marshals to it;
// AgentUIView.test.tsx imports the same file and renders it through
// AgentUIView's props, asserting the declaration's node tree appears. Every key
// in the golden is therefore load-bearing on both sides.
//
// Why this exists: the shell reads its props through readBootstrap, an
// unchecked `JSON.parse(...) as T`. A key present in the JSON under a different
// name than the TS type declares becomes `undefined` at runtime, with nothing
// failing anywhere. A review proved it — renaming a json tag on the Go side,
// and updating the Go assertion that named it, left the Go suite green AND
// every frontend test green while the shipped page silently lost a field. With
// the golden in place, that rename fails HERE; regenerating the golden to match
// it then fails the TS rows instead. There is no edit to one side alone that
// keeps both suites green.
//
// To change the contract deliberately: update viewmodel.go's ViewProps, this
// file's expectation of it, the golden, AND AgentUIView.tsx's props together.
const goldenPropsPath = "ui/testdata/props.golden.json"

const (
	goldenNs      = "workshop"
	goldenSession = "sess-live"
	goldenClass   = "demo-agent"
	goldenUI      = "console-ui"
)

// goldenDeps is the narrowest Deps that lets ViewFor run to its happy path:
// interact always granted (the gate has its own coverage in page_test.go), the
// fixture object graph, and a discarding logger. The bindings-route
// collaborators (TrustedOrigin/NATSRequest/Memory/Artifacts/
// ArtifactRenderBytes) are present only to satisfy the Deps interface —
// ViewFor never calls them, so their zero values are never exercised
// here; bindings_test.go is where they get real coverage.
type goldenDeps struct{ k8s client.Client }

func (g goldenDeps) CheckInteract(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (g goldenDeps) K8s() client.Client  { return g.k8s }
func (g goldenDeps) Logger() logr.Logger { return logr.Discard() }

func (g goldenDeps) TrustedOrigin() string                  { return "" }
func (g goldenDeps) NATSRequest() channelevents.RequestFunc { return nil }

// Memory returns a fresh, always-empty fakeUIActionMemory (live_test.go) —
// resolveDeclaration (via resolveView/uiview.Resolve, viewmodel.go) now
// requires a non-nil backend to serve ANY declaration, Tier-0-only included.
// Both golden fixtures below are Tier-0-only (no stored fragment), so an
// empty backend is exactly right; a nil Memory would 503 the happy path.
func (g goldenDeps) Memory() memory.Memory                                   { return &fakeUIActionMemory{} }
func (g goldenDeps) Artifacts() *artifacts.Service                           { return nil }
func (g goldenDeps) ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc { return nil }
func (g goldenDeps) NATS() *nats.Conn                                        { return nil }

// StartBrowserSession is nil: this file's goldens only exercise ViewFor,
// never the start route.
func (g goldenDeps) StartBrowserSession() browsersession.StartFunc { return nil }

// LiveSessions is nil for the same reason: only the start route reserves.
func (g goldenDeps) LiveSessions() browserstart.LiveSessions { return nil }
func (g goldenDeps) StartableNamespaces() []string           { return nil }
func (g goldenDeps) WorkshopNamespacesFor(context.Context, string) ([]string, error) {
	return nil, nil
}

// goldenFixtureClient carries the object graph that produces the golden props:
// a live session, its AgentClass, and an AgentUI that is Valid, requests
// collapsed chrome, and declares one slot with a renderable Tier-0 default.
func goldenFixtureClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: goldenSession, Namespace: goldenNs},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: goldenClass},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: goldenClass, Namespace: goldenNs},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: goldenUI},
		},
	}
	aui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: goldenUI, Namespace: goldenNs},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Chrome: &spiceboxv1alpha1.AgentUIChrome{InitialState: "collapsed"},
			Slots: []spiceboxv1alpha1.AgentUISlot{{
				Name: "root",
				Default: &apiextensionsv1.JSON{Raw: []byte(
					`{"component":"ap:heading","props":{"text":"Fleet Console","level":1}}`)},
			}},
		},
		Status: spiceboxv1alpha1.AgentUIStatus{Conditions: []metav1.Condition{{
			Type:   spiceboxv1alpha1.AgentUIConditionValid,
			Status: metav1.ConditionTrue,
			Reason: spiceboxv1alpha1.ReasonAgentUISpecOK,
		}}},
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(sess, ac, aui).Build()
}

// buildGoldenViewProps runs the REAL ViewFor for a fixture graph and returns
// the ViewProps it produced, marshalled exactly as the shell marshals them into
// its own bootstrap payload. Going through ViewFor rather than constructing
// ViewProps by hand is what ties each golden to production output: a field the
// server stops sending, or sends under a new tag, changes these bytes.
//
// The one shared helper serves all three goldens — they differ only in their
// object graph, and a per-golden copy of this would be three chances for one of
// them to stop reading production output without anyone noticing.
func buildGoldenViewProps(t *testing.T, d Deps, ns, name string) []byte {
	t.Helper()
	props, _, avail, pe := ViewFor(context.Background(), d, ns, name)
	require.Nil(t, pe, "the fixture graph must reach the happy path")
	require.Equal(t, UIAvailable, avail, "the fixture's class must declare a usable UI")

	out, err := json.Marshal(props)
	require.NoError(t, err, "props must marshal — the shell marshals them the same way")
	return out
}

func buildGoldenProps(t *testing.T) []byte {
	t.Helper()
	return buildGoldenViewProps(t, goldenDeps{k8s: goldenFixtureClient(t)}, goldenNs, goldenSession)
}

// jsonKeys returns the sorted top-level key set of a JSON object.
func jsonKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var obj map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &obj))
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestBootstrapProps_MatchTheGoldenTheBrowserRenders(t *testing.T) {
	got := buildGoldenProps(t)
	want, err := os.ReadFile(filepath.Clean(goldenPropsPath))
	require.NoError(t, err, "the golden the frontend suite renders must exist")

	// Asserted separately from the value comparison below because a RENAMED key
	// is the failure this file exists to catch, and a key-set diff names it
	// directly instead of burying it in a whole-document mismatch.
	assert.Equal(t, jsonKeys(t, want), jsonKeys(t, got),
		"the view props' key set changed: update %s and AgentUIView.tsx's props together", goldenPropsPath)

	assert.JSONEq(t, string(want), string(got),
		"ViewFor's props no longer match %s, which AgentUIView.test.tsx renders", goldenPropsPath)
}

// goldenActionsPath is the SECOND shared golden — the action-table join.
// It is a separate file from goldenPropsPath (props.golden.json) rather than
// an edit to it, deliberately: props.golden.json is the bootstrap-props pin
// and its fixture, byte layout, and every existing assertion against it must stay
// untouched (see goldenPropsPath's own doc comment) — extending it here would
// force every future edit to either contract to touch both. This file's own
// fixture (actionsGoldenFixtureClient, below) is a SEPARATE object graph from
// goldenFixtureClient's, for the same reason.
//
// TestShellBuildActionsGolden below asserts ViewFor's REAL output against
// it; AgentUIView.test.tsx's "AgentUIView — the golden action-table props
// ViewFor actually emits (J3)" describe block reads the SAME file two ways —
// rendered (a button naming one declared action appears) AND read directly by
// key (`declaration.actions`, through @ap/agentui's own `Declaration.actions`
// field) — so a rename on either side of the actions wire fails the other
// side's suite. See that file for why the direct-read half is required: a
// render-only check never touches `declaration.actions` at all, since no
// control in this plan's scope yet consults the action table to render
// anything differently.
const goldenActionsPath = "ui/testdata/props.actions.golden.json"

const (
	goldenActionsNs      = "workshop"
	goldenActionsSession = "sess-live"
	goldenActionsClass   = "demo-agent"
	goldenActionsUI      = "console-ui-actions"
)

// actionsGoldenFixtureClient carries the object graph TestShellBuildActionsGolden
// asserts against: a live session, its AgentClass, and an AgentUI that is
// Valid, declares TWO actions (one named by a control, one that is not — so
// the golden proves the FULL table travels, not just the referenced entry),
// and one slot whose ap:button names the first action.
func actionsGoldenFixtureClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: goldenActionsSession, Namespace: goldenActionsNs},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: goldenActionsClass},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: goldenActionsClass, Namespace: goldenActionsNs},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: goldenActionsUI},
		},
	}
	aui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: goldenActionsUI, Namespace: goldenActionsNs},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Actions: []spiceboxv1alpha1.AgentUIAction{
				{
					Name:   "advance",
					Tool:   "demo_advance_stage",
					Args:   &apiextensionsv1.JSON{Raw: []byte(`{"id":"42"}`)},
					Inputs: []string{"note"},
				},
				{
					Name: "archive",
					Tool: "demo_archive_stage",
				},
			},
			Slots: []spiceboxv1alpha1.AgentUISlot{{
				Name: "root",
				Default: &apiextensionsv1.JSON{Raw: []byte(
					`{"component":"ap:button","props":{"label":"Advance","action":"advance"}}`)},
			}},
		},
		Status: spiceboxv1alpha1.AgentUIStatus{Conditions: []metav1.Condition{{
			Type:   spiceboxv1alpha1.AgentUIConditionValid,
			Status: metav1.ConditionTrue,
			Reason: spiceboxv1alpha1.ReasonAgentUISpecOK,
		}}},
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(sess, ac, aui).Build()
}

// buildGoldenActionsProps is buildGoldenProps against the actions fixture.
func buildGoldenActionsProps(t *testing.T) []byte {
	t.Helper()
	return buildGoldenViewProps(t, goldenDeps{k8s: actionsGoldenFixtureClient(t)},
		goldenActionsNs, goldenActionsSession)
}

// TestShellBuildActionsGolden pins the Go->TS declaration wire for the ACTION
// table, the same way TestBootstrapProps_MatchTheGoldenTheBrowserRenders pins
// it for slots. It is a SECOND file rather than an edit to props.golden.json
// so the bootstrap-props assertion keeps its exact fixture; AgentUIView.test.tsx
// imports this file too and reads it through AgentUIViewProps.
//
// The pin only covers a field the TS side names EXPLICITLY: its assertion uses
// toMatchObject, a subset match, so an unnamed field is unasserted and a
// coordinated rename of it survives both suites — `args` was exactly that gap.
// Adding a field to AgentUIAction therefore means adding it to that
// assertion too, or it ships unprotected. That join
// is the whole reason this test exists — a Go-only assertion here would be
// green through a TS rename, which is the defect shape this seam has already
// shipped once (see goldenPropsPath's doc comment).
func TestShellBuildActionsGolden(t *testing.T) {
	got := buildGoldenActionsProps(t)
	want, err := os.ReadFile(filepath.Clean(goldenActionsPath))
	require.NoError(t, err, "the actions golden the frontend suite renders must exist")

	assert.JSONEq(t, string(want), string(got),
		"page.go's props no longer match %s, which AgentUIView.test.tsx renders", goldenActionsPath)
}

// goldenViewModelPath is the FIFTH shared golden. Like goldenActionsPath it is
// a separate FILE rather than an edit to props.golden.json, so the
// bootstrap-props pin keeps its exact fixture and byte layout — see
// goldenActionsPath's own
// comment for the same reasoning.
//
// TestShellBuildViewModelGolden asserts ViewFor's REAL output against it
// for a session with one agent-composed slot ("panel") and one Tier-0 slot
// ("notes"); AgentUIView.test.tsx renders the SAME file and asserts the
// persistent marker (renderSlot, @ap/agentui) is visible on the first and
// absent on the second. A rename of the agentComposed json tag on either
// side fails the other side's suite — which is the entire reason both
// halves exist (J3, this plan's marker join).
const goldenViewModelPath = "ui/testdata/props.viewmodel.golden.json"

const (
	goldenViewModelNs      = "workshop"
	goldenViewModelSession = "sess-live"
	goldenViewModelClass   = "demo-agent"
	goldenViewModelUI      = "console-ui-viewmodel"
)

// goldenViewModelFixtureClient carries the object graph
// TestShellBuildViewModelGolden asserts against: a live session, its
// AgentClass, and an AgentUI that is Valid and declares TWO slots — "panel"
// (AgentWritable, so the fragment written below is legal) and "notes" (not
// writable, so it can never carry a fragment and must never be marked
// agent-composed).
func goldenViewModelFixtureClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: goldenViewModelSession, Namespace: goldenViewModelNs},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: goldenViewModelClass},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: goldenViewModelClass, Namespace: goldenViewModelNs},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: goldenViewModelUI},
		},
	}
	aui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: goldenViewModelUI, Namespace: goldenViewModelNs},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{
					Name:          "panel",
					AgentWritable: true,
					Default: &apiextensionsv1.JSON{Raw: []byte(
						`{"component":"ap:text","props":{"text":"placeholder"}}`)},
				},
				{
					Name: "notes",
					Default: &apiextensionsv1.JSON{Raw: []byte(
						`{"component":"ap:heading","props":{"text":"Notes","level":2}}`)},
				},
			},
		},
		Status: spiceboxv1alpha1.AgentUIStatus{Conditions: []metav1.Condition{{
			Type:   spiceboxv1alpha1.AgentUIConditionValid,
			Status: metav1.ConditionTrue,
			Reason: spiceboxv1alpha1.ReasonAgentUISpecOK,
		}}},
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(sess, ac, aui).Build()
}

// goldenViewModelDeps is goldenDeps' shape, except Memory() returns a
// REUSABLE backend instead of a fresh, always-empty one on every call.
// goldenDeps' own Memory() (above) constructs a new fakeUIActionMemory per
// call specifically because none of its callers ever need to seed it first —
// this golden does: the ui_view_model fragment below must still be there
// when ViewFor itself calls d.Memory().
type goldenViewModelDeps struct {
	k8s client.Client
	mem memory.Memory
}

func (g goldenViewModelDeps) CheckInteract(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (g goldenViewModelDeps) K8s() client.Client                                      { return g.k8s }
func (g goldenViewModelDeps) Logger() logr.Logger                                     { return logr.Discard() }
func (g goldenViewModelDeps) TrustedOrigin() string                                   { return "" }
func (g goldenViewModelDeps) NATSRequest() channelevents.RequestFunc                  { return nil }
func (g goldenViewModelDeps) Memory() memory.Memory                                   { return g.mem }
func (g goldenViewModelDeps) Artifacts() *artifacts.Service                           { return nil }
func (g goldenViewModelDeps) ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc { return nil }
func (g goldenViewModelDeps) NATS() *nats.Conn                                        { return nil }

// StartBrowserSession is nil: this golden only exercises ViewFor, never
// the start route.
func (g goldenViewModelDeps) StartBrowserSession() browsersession.StartFunc { return nil }

// LiveSessions is nil for the same reason: only the start route reserves.
func (g goldenViewModelDeps) LiveSessions() browserstart.LiveSessions { return nil }
func (g goldenViewModelDeps) StartableNamespaces() []string           { return nil }
func (g goldenViewModelDeps) WorkshopNamespacesFor(context.Context, string) ([]string, error) {
	return nil, nil
}

var _ Deps = goldenViewModelDeps{}

// buildGoldenViewModelProps writes one ui_view_model fragment for "panel"
// directly into the fixture's memory (the same shape uiview.Runtime.Write
// itself would record — see uiviewmodel.Record), then drives the REAL ViewFor
// so the golden is production output, not a hand-built literal.
func buildGoldenViewModelProps(t *testing.T) []byte {
	t.Helper()
	mem := &fakeUIActionMemory{}
	scope := memory.Scope{Kind: "session", ID: goldenViewModelNs + "/" + goldenViewModelSession}
	require.NoError(t, uiviewmodel.Record(context.Background(), mem, scope, uiviewmodel.Content{
		UI:        goldenViewModelUI,
		Slot:      "panel",
		Node:      json.RawMessage(`{"component":"ap:markdown","props":{"body":"agent copy"}}`),
		WrittenAt: time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC),
	}), "seed the ui_view_model fragment the golden's declaration depends on")

	return buildGoldenViewProps(t,
		goldenViewModelDeps{k8s: goldenViewModelFixtureClient(t), mem: mem},
		goldenViewModelNs, goldenViewModelSession)
}

// declarationKeys extracts the RAW top-level key set of a bootstrap props
// payload's `declaration` object — deliberately not decoded through a Go
// struct first. Decoding through declarationWire before comparing would use
// the SAME json tag the rename this test exists to catch would have moved,
// so the rename would vanish before the assertion ever saw it; reading the
// wire's own keys is what actually catches it.
func declarationKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var wire struct {
		Declaration map[string]json.RawMessage `json:"declaration"`
	}
	require.NoError(t, json.Unmarshal(raw, &wire))
	keys := make([]string, 0, len(wire.Declaration))
	for k := range wire.Declaration {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestShellBuildViewModelGolden pins the Go->TS declaration wire for the
// top-level agentComposed hook list (J3), the same way TestShellBuildActionsGolden
// pins it for the action table. It is a SEPARATE golden file rather than an
// edit to props.golden.json for the identical reason goldenActionsPath is
// (see that const's doc comment).
func TestShellBuildViewModelGolden(t *testing.T) {
	got := buildGoldenViewModelProps(t)
	want, err := os.ReadFile(filepath.Clean(goldenViewModelPath))
	require.NoError(t, err, "the view-model golden the frontend suite renders must exist")

	// A direct key-set assertion on the declaration object, so a RENAMED
	// agentComposed key is named directly rather than buried inside a whole-
	// document JSONEq diff.
	assert.Contains(t, declarationKeys(t, got), "agentComposed",
		"declaration no longer carries an agentComposed key — update viewmodel.go's declarationWire, "+
			"%s, and @ap/agentui's Declaration.agentComposed together", goldenViewModelPath)

	assert.JSONEq(t, string(want), string(got),
		"page.go's props no longer match %s, which AgentUIView.test.tsx renders", goldenViewModelPath)
}

// TestRegenerateGoldensScratch is a scratch generator, not part of this
// package's real coverage: it exists only so the three golden files above
// can be regenerated from ViewFor's own real output when the view
// contract changes. Gated behind REGEN_GOLDEN so it never runs as part of
// the normal suite.
func TestRegenerateGoldensScratch(t *testing.T) {
	if os.Getenv("REGEN_GOLDEN") == "" {
		t.Skip("set REGEN_GOLDEN=1 to regenerate")
	}
	write := func(path string, raw []byte) {
		var buf bytes.Buffer
		require.NoError(t, json.Indent(&buf, raw, "", "  "))
		buf.WriteByte('\n')
		require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
	}
	write(goldenPropsPath, buildGoldenProps(t))
	write(goldenActionsPath, buildGoldenActionsProps(t))
	write(goldenViewModelPath, buildGoldenViewModelProps(t))
}
