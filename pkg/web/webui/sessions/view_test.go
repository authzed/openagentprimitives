package sessions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
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
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/agentui"
)

// This file exercises viewFor and the ?session= wiring in shellPageBuild
// (page.go). Fabricated names only, per AGENTS.md's no-examples-in-tests
// rule.
const (
	vfNS      = "demo-ns"
	vfSession = "demo-session"
	vfClass   = "demo-agent"
	vfUI      = "demo-ui"
	vfSubject = "user:demo-viewer"
)

// fakeViewMemory is an always-empty memory.Memory: every fixture in this
// file is Tier-0-only (no stored fragment), so an empty backend is exactly
// right — see agentui's own goldenDeps.Memory() for the identical posture.
type fakeViewMemory struct{}

func (fakeViewMemory) Put(context.Context, memory.Entry) (memory.Entry, error) {
	return memory.Entry{}, nil
}
func (fakeViewMemory) Query(context.Context, memory.Query) (memory.QueryResult, error) {
	return memory.QueryResult{}, nil
}
func (fakeViewMemory) Search(context.Context, memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{}, nil
}
func (fakeViewMemory) SendSignal(context.Context, memory.Signal) error { return nil }

var _ memory.Memory = fakeViewMemory{}

// countingClient wraps a client.Client and counts Get calls per object type
// — the observation channel for TestViewFor_Ended's call-count assertion.
//
// sessionGets, specifically, is what actually discriminates: viewFor's own
// FIRST step (view.go) always Gets the AgentSession once, before deciding
// whether the session is Ended. If it is, and viewFor still calls
// agentui.ViewFor anyway, that function's own ladder walk (walkAgentUIDoors)
// Gets the SAME AgentSession a SECOND time as ITS first step — so
// sessionGets==2 is what a mutation that reaches agentui.ViewFor actually
// produces. classGets/uiGets are NOT that signal here and are kept only for
// documentation: walkAgentUIDoors' own Ended check fires immediately after
// its AgentSession Get and its own ResolveSession call, before it ever
// issues a Class or UI Get — so a genuinely-Ended session's class/uiGets
// stay at 0 whether or not agentui.ViewFor was reached at all. An earlier
// version of this fixture asserted only classGets/uiGets==0 for that reason
// and would have passed vacuously against the exact mutation this test
// exists to catch.
type countingClient struct {
	client.Client
	mu          sync.Mutex
	sessionGets int
	classGets   int
	uiGets      int
}

func (c *countingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	switch obj.(type) {
	case *spiceboxv1alpha1.AgentSession:
		c.mu.Lock()
		c.sessionGets++
		c.mu.Unlock()
	case *spiceboxv1alpha1.AgentClass:
		c.mu.Lock()
		c.classGets++
		c.mu.Unlock()
	case *spiceboxv1alpha1.AgentUI:
		c.mu.Lock()
		c.uiGets++
		c.mu.Unlock()
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// viewFixtureDeps implements BOTH sessions.Deps and agentui.Deps — the same
// shape internal/cmd/webd's *artifactViewDeps carries in production (see its
// compile-time guards in main.go), which is what makes viewFor's own
// `d.(agentui.Deps)` type assertion succeed here exactly as it does there.
type viewFixtureDeps struct {
	k8s    client.Client
	mem    memory.Memory
	logger logr.Logger

	mu          sync.Mutex
	loggedLines []string

	checkInteract func(ctx context.Context, ns, name, subject string) (bool, error)
}

func (d *viewFixtureDeps) LookupInteractableSessions(context.Context, identity.CanonicalUserID,
	uint32, bool,
) (spicedb.InteractableSessions, error) {
	return spicedb.InteractableSessions{}, nil
}

// LookupStartableClasses answers an empty set — see narrowDeps' identical note.
func (d *viewFixtureDeps) LookupStartableClasses(context.Context,
	identity.CanonicalUserID, uint32, bool,
) (spicedb.StartableClasses, error) {
	return spicedb.StartableClasses{}, nil
}

func (d *viewFixtureDeps) CheckInteract(ctx context.Context, ns, name, subject string) (bool, error) {
	if d.checkInteract == nil {
		return true, nil
	}
	return d.checkInteract(ctx, ns, name, subject)
}
func (d *viewFixtureDeps) K8s() client.Client                            { return d.k8s }
func (d *viewFixtureDeps) StartBrowserSession() browsersession.StartFunc { return nil }
func (d *viewFixtureDeps) LiveSessions() LiveSessions                    { return nil }
func (d *viewFixtureDeps) StartableNamespaces() []string                 { return nil }
func (d *viewFixtureDeps) WorkshopNamespacesFor(context.Context, string) ([]string, error) {
	return nil, nil
}
func (d *viewFixtureDeps) Logger() logr.Logger { return d.logger }

func (d *viewFixtureDeps) TrustedOrigin() string                                   { return "" }
func (d *viewFixtureDeps) NATSRequest() channelevents.RequestFunc                  { return nil }
func (d *viewFixtureDeps) Memory() memory.Memory                                   { return d.mem }
func (d *viewFixtureDeps) Artifacts() *artifacts.Service                           { return nil }
func (d *viewFixtureDeps) ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc { return nil }
func (d *viewFixtureDeps) NATS() *nats.Conn                                        { return nil }

var _ Deps = (*viewFixtureDeps)(nil)
var _ agentui.Deps = (*viewFixtureDeps)(nil)

// narrowDeps implements sessions.Deps and DELIBERATELY nothing more — the
// negative twin of viewFixtureDeps, standing in for a hypothetical future
// Deps split where the concrete umbrella satisfies this package's own
// interface but not agentui.Deps (in production, internal/cmd/webd's
// *artifactViewDeps always satisfies both — see its compile-time guards in
// main.go — so this fixture is the only way to observe viewFor's cast
// -failure branch at all). "Provably safe by inspection" is not a substitute
// for a red/green test.
type narrowDeps struct {
	k8s    client.Client
	logger logr.Logger

	mu          sync.Mutex
	loggedLines []string
}

func (d *narrowDeps) LookupInteractableSessions(context.Context, identity.CanonicalUserID,
	uint32, bool,
) (spicedb.InteractableSessions, error) {
	return spicedb.InteractableSessions{}, nil
}

// LookupStartableClasses answers an empty set: these fixtures exercise the
// SELECTED VIEW, not the start set, and a viewer with no bootstrap grant is
// the ordinary case.
func (d *narrowDeps) LookupStartableClasses(context.Context,
	identity.CanonicalUserID, uint32, bool,
) (spicedb.StartableClasses, error) {
	return spicedb.StartableClasses{}, nil
}

func (d *narrowDeps) CheckInteract(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (d *narrowDeps) K8s() client.Client                            { return d.k8s }
func (d *narrowDeps) StartBrowserSession() browsersession.StartFunc { return nil }
func (d *narrowDeps) LiveSessions() LiveSessions                    { return nil }
func (d *narrowDeps) StartableNamespaces() []string                 { return nil }
func (d *narrowDeps) WorkshopNamespacesFor(context.Context, string) ([]string, error) {
	return nil, nil
}
func (d *narrowDeps) TrustedOrigin() string { return "" }
func (d *narrowDeps) Logger() logr.Logger   { return d.logger }

var _ Deps = (*narrowDeps)(nil)

func newNarrowDeps(t *testing.T, objs ...client.Object) *narrowDeps {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	d := &narrowDeps{k8s: fc}
	d.logger = funcr.New(func(_, args string) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.loggedLines = append(d.loggedLines, args)
	}, funcr.Options{})
	return d
}

func (d *narrowDeps) logged() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := ""
	for _, l := range d.loggedLines {
		out += l + "\n"
	}
	return out
}

func (d *viewFixtureDeps) logged() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := ""
	for _, l := range d.loggedLines {
		out += l + "\n"
	}
	return out
}

// newViewDeps builds a *viewFixtureDeps over a fake Kubernetes client seeded
// with objs, a capturing logger, and a working Tier-0-only memory backend.
func newViewDeps(t *testing.T, objs ...client.Object) *viewFixtureDeps {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	d := &viewFixtureDeps{k8s: fc, mem: fakeViewMemory{}}
	d.logger = funcr.New(func(_, args string) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.loggedLines = append(d.loggedLines, args)
	}, funcr.Options{})
	return d
}

// vfSessionObj builds the named AgentSession (vfNS/vfSession) in the given
// phase.
func vfSessionObj(phase string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: vfNS, Name: vfSession},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: vfClass},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
	}
}

// vfClassObj builds the AgentClass every fixture Gets by name (vfClass).
// grant is nil to exercise "no AgentUI on the class".
func vfClassObj(grant *spiceboxv1alpha1.AgentClassUIGrant) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: vfNS, Name: vfClass},
		Spec:       spiceboxv1alpha1.AgentClassSpec{AgentUI: grant},
	}
}

// vfUIObj builds the referenced AgentUI CR (vfUI) with one renderable slot
// and the given Valid condition. chromeInitial, when non-empty, sets
// Spec.Chrome.InitialState — proving the chrome REQUEST still flows from the
// AgentUI CR into selectedView.Chrome now that the forwarding lives in
// agentui.ViewFor rather than agentui's own page.go (see the coverage this
// row replaces: pkg/web/webui/agentui/page_test.go's former "AgentUI requests
// collapsed chrome" row).
func vfUIObj(status metav1.ConditionStatus, message, chromeInitial string) *spiceboxv1alpha1.AgentUI {
	aui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Namespace: vfNS, Name: vfUI},
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

// vfUIObjNoConditions builds the referenced AgentUI with an empty status —
// the state every AgentUI is in between creation and its first reconcile.
func vfUIObjNoConditions() *spiceboxv1alpha1.AgentUI {
	aui := vfUIObj(metav1.ConditionTrue, "", "")
	aui.Status.Conditions = nil
	return aui
}

// assertNoInternalIdentifiers is the wantBodyLacks-style negative check
// page_test.go's own table already uses (e.g. its "agentsessions"/"apis/"
// row) — applied here to the copy THIS package authors itself (view.go's two
// unavailableMessage literals), which had no assertion at all before this
// fix round: mutating either string to name a CRD kind, kubectl, or a
// resource path left `go test ./pkg/web/webui/sessions/...` green.
func assertNoInternalIdentifiers(t *testing.T, texts ...string) {
	t.Helper()
	for _, text := range texts {
		for _, forbidden := range []string{"AgentUI", "AgentClass", "AgentSession", "kubectl", "spec.", "CRD"} {
			assert.NotContains(t, text, forbidden,
				"user-facing copy %q must not name an internal identifier (%q)", text, forbidden)
		}
	}
}

func TestViewFor(t *testing.T) {
	tests := []struct {
		name      string
		objs      []client.Object
		requested string
		check     func(t *testing.T, sv selectedView, pe *webui.PageError, d *viewFixtureDeps)
	}{
		{
			name: "class declares a Valid=True AgentUI, no ?view=: agent-ui view, chrome forwarded, switch offered",
			objs: []client.Object{vfSessionObj(spiceboxv1alpha1.AgentSessionPhaseRunning),
				vfClassObj(&spiceboxv1alpha1.AgentClassUIGrant{Ref: vfUI}),
				vfUIObj(metav1.ConditionTrue, "", "collapsed")},
			check: func(t *testing.T, sv selectedView, pe *webui.PageError, _ *viewFixtureDeps) {
				require.Nil(t, pe)
				assert.Equal(t, "attached", sv.SessionOrigin)
				assert.True(t, sv.OffersAgentUI)
				assert.Equal(t, "collapsed", sv.Chrome.InitialState, "the AgentUI's chrome request must still reach selectedView")
				require.Equal(t, viewAgentUI, sv.View.Kind)
				require.NotNil(t, sv.View.UI)
				assert.Equal(t, vfNS, sv.View.UI.Ns)
				assert.Equal(t, vfSession, sv.View.UI.Name)
				assert.Contains(t, string(sv.View.UI.Declaration), `"component":"ap:stack"`,
					"UI.Declaration must be the merged wire document, not an empty placeholder")
			},
		},
		{
			name: "class declares no AgentUI, no ?view=: chat view, no PageError — the case resolveAgentUIDoors answers 404 for",
			objs: []client.Object{vfSessionObj(spiceboxv1alpha1.AgentSessionPhaseRunning),
				vfClassObj(nil)},
			check: func(t *testing.T, sv selectedView, pe *webui.PageError, _ *viewFixtureDeps) {
				require.Nil(t, pe, "no-UI-declared must never surface as a page error")
				assert.False(t, sv.OffersAgentUI)
				require.Equal(t, viewChat, sv.View.Kind)
				require.NotNil(t, sv.View.Chat)
				assert.Equal(t, vfSession, sv.View.Chat.Name)
			},
		},
		{
			name: "class declares a UI, ?view=chat: chat view, switch still offered",
			objs: []client.Object{vfSessionObj(spiceboxv1alpha1.AgentSessionPhaseRunning),
				vfClassObj(&spiceboxv1alpha1.AgentClassUIGrant{Ref: vfUI}),
				vfUIObj(metav1.ConditionTrue, "", "")},
			requested: "chat",
			check: func(t *testing.T, sv selectedView, pe *webui.PageError, _ *viewFixtureDeps) {
				require.Nil(t, pe)
				require.Equal(t, viewChat, sv.View.Kind)
				assert.True(t, sv.OffersAgentUI, "the switch stays offered even while chat is the current view")
			},
		},
		{
			name: "class declares no UI, ?view=ui: unavailable naming no custom view — not a silent fallback to chat",
			objs: []client.Object{vfSessionObj(spiceboxv1alpha1.AgentSessionPhaseRunning),
				vfClassObj(nil)},
			requested: "ui",
			check: func(t *testing.T, sv selectedView, pe *webui.PageError, _ *viewFixtureDeps) {
				require.Nil(t, pe)
				require.Equal(t, viewUnavailable, sv.View.Kind)
				require.NotNil(t, sv.View.Unavailable)
				// Exact copy, not merely "something rendered": AGENTS.md forbids
				// CRD kinds, kubectl, and resource paths in user-facing text, and
				// nothing else in this row would catch a leak of any of those.
				assert.Equal(t, "No custom view", sv.View.Unavailable.Title)
				assert.Equal(t, "This agent has no custom view for this session.", sv.View.Unavailable.Message)
				assertNoInternalIdentifiers(t, sv.View.Unavailable.Title, sv.View.Unavailable.Message)
			},
		},
		{
			name: "?view=sideways is treated as unset: the capability default (agent-ui, since a UI is declared)",
			objs: []client.Object{vfSessionObj(spiceboxv1alpha1.AgentSessionPhaseRunning),
				vfClassObj(&spiceboxv1alpha1.AgentClassUIGrant{Ref: vfUI}),
				vfUIObj(metav1.ConditionTrue, "", "")},
			requested: "sideways",
			check: func(t *testing.T, sv selectedView, pe *webui.PageError, _ *viewFixtureDeps) {
				require.Nil(t, pe)
				assert.Equal(t, viewAgentUI, sv.View.Kind)
			},
		},
		{
			name: "AgentUI object missing: unavailable with resolveAgentUIDoors' own 404 copy, page still returns props",
			objs: []client.Object{vfSessionObj(spiceboxv1alpha1.AgentSessionPhaseRunning),
				vfClassObj(&spiceboxv1alpha1.AgentClassUIGrant{Ref: vfUI})},
			check: func(t *testing.T, sv selectedView, pe *webui.PageError, _ *viewFixtureDeps) {
				require.Nil(t, pe, "the page must still succeed — a broken UI never costs the rest of the dashboard")
				require.Equal(t, viewUnavailable, sv.View.Kind)
				require.NotNil(t, sv.View.Unavailable)
				assert.Equal(t, "UI unavailable", sv.View.Unavailable.Title)
				assert.Equal(t, "This agent's UI isn't available right now.", sv.View.Unavailable.Message)
			},
		},
		{
			name: "AgentUI Valid condition absent/Unknown: unavailable with 'not ready yet' copy, cause logged",
			objs: []client.Object{vfSessionObj(spiceboxv1alpha1.AgentSessionPhaseRunning),
				vfClassObj(&spiceboxv1alpha1.AgentClassUIGrant{Ref: vfUI}),
				vfUIObjNoConditions()},
			check: func(t *testing.T, sv selectedView, pe *webui.PageError, d *viewFixtureDeps) {
				require.Nil(t, pe)
				require.Equal(t, viewUnavailable, sv.View.Kind)
				require.NotNil(t, sv.View.Unavailable)
				assert.Equal(t, "UI not ready yet", sv.View.Unavailable.Title)
				assert.Contains(t, d.logged(), vfClass)
				assert.Contains(t, d.logged(), vfUI)
			},
		},
		{
			name: "AgentUI Valid=False: unavailable carrying the reconciler's own author-facing message",
			objs: []client.Object{vfSessionObj(spiceboxv1alpha1.AgentSessionPhaseRunning),
				vfClassObj(&spiceboxv1alpha1.AgentClassUIGrant{Ref: vfUI}),
				vfUIObj(metav1.ConditionFalse, "slot root: unknown component ap:bogus", "")},
			check: func(t *testing.T, sv selectedView, pe *webui.PageError, _ *viewFixtureDeps) {
				require.Nil(t, pe)
				require.Equal(t, viewUnavailable, sv.View.Kind)
				require.NotNil(t, sv.View.Unavailable)
				assert.Equal(t, "slot root: unknown component ap:bogus", sv.View.Unavailable.Message,
					"this is the one door whose message is deliberately the author's own")
			},
		},
		{
			name: "session is WakeEligible: origin asleep, view resolves normally",
			objs: []client.Object{vfSessionObj(spiceboxv1alpha1.AgentSessionPhaseIdle),
				vfClassObj(&spiceboxv1alpha1.AgentClassUIGrant{Ref: vfUI}),
				vfUIObj(metav1.ConditionTrue, "", "")},
			check: func(t *testing.T, sv selectedView, pe *webui.PageError, _ *viewFixtureDeps) {
				require.Nil(t, pe)
				assert.Equal(t, "asleep", sv.SessionOrigin)
				assert.Equal(t, viewAgentUI, sv.View.Kind)
			},
		},
		{
			name: "session absent from Kubernetes: a 404 *webui.PageError, not a viewUnavailable",
			objs: nil,
			check: func(t *testing.T, sv selectedView, pe *webui.PageError, _ *viewFixtureDeps) {
				require.NotNil(t, pe, "there is no session to put chrome around")
				assert.Equal(t, http.StatusNotFound, pe.Status)
				assert.Equal(t, selectedView{}, sv)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := newViewDeps(t, tc.objs...)
			sv, pe := viewFor(context.Background(), d, vfNS, vfSession, tc.requested)
			tc.check(t, sv, pe, d)
		})
	}
}

// TestViewFor_AgentUIDepsCastFailure (M6) proves the fail-closed branch
// viewFor takes when d does not additionally implement agentui.Deps: a 500
// *webui.PageError with generic copy, and errViewDepsCastFailed's message
// reaching the logger — never a panic, never a 403. Unreachable in
// production (see narrowDeps' own doc comment), but "provably safe by
// inspection" is not this repo's bar for a fail-closed branch.
func TestViewFor_AgentUIDepsCastFailure(t *testing.T) {
	d := newNarrowDeps(t, vfSessionObj(spiceboxv1alpha1.AgentSessionPhaseRunning),
		vfClassObj(&spiceboxv1alpha1.AgentClassUIGrant{Ref: vfUI}),
		vfUIObj(metav1.ConditionTrue, "", ""))

	sv, pe := viewFor(context.Background(), d, vfNS, vfSession, "")

	require.NotNil(t, pe, "a Deps that cannot resolve the agent-defined view must fail closed, not panic or proceed")
	assert.Equal(t, http.StatusInternalServerError, pe.Status)
	assert.Equal(t, selectedView{}, sv)
	assert.Contains(t, d.logged(), "agentui.Deps",
		"the cast failure's cause must reach the log, per errViewDepsCastFailed's own doc comment")
}

// TestViewFor_Ended is separate from TestViewFor's table because its
// distinguishing assertion (agentui.ViewFor was never reached) needs the
// countingClient wrapper, which the other rows do not.
func TestViewFor_Ended(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		checkView func(t *testing.T, sv selectedView)
	}{
		{
			name: "session is Ended, class declares a UI: chat, no switch",
			checkView: func(t *testing.T, sv selectedView) {
				assert.Equal(t, "ended", sv.SessionOrigin)
				assert.False(t, sv.OffersAgentUI)
				// No canStartReplacement to assert: an ended session is
				// identified by SessionOrigin alone (see selectedView).
				require.Equal(t, viewChat, sv.View.Kind)
			},
		},
		{
			// I4 (fix round 1): Kind stays chat, not unavailable — OffersAgentUI
			// is false for an Ended session, so a viewUnavailable card here would
			// be a dead end with no switch back to the transcript it names. The
			// request is answered via a NOTICE beside the still-rendered chat,
			// not by replacing the content region.
			name:      "session is Ended, ?view=ui: answered via a notice beside the transcript, not by replacing it",
			requested: "ui",
			checkView: func(t *testing.T, sv selectedView) {
				require.Equal(t, viewChat, sv.View.Kind, "the transcript must still render — no dead end")
				require.NotNil(t, sv.View.Chat)
				assert.Nil(t, sv.View.Unavailable, "Unavailable REPLACES content; this case must not use it")
				require.NotNil(t, sv.View.Notice, "the request must still be answered, via a non-blocking notice")
				assert.Equal(t, "Session ended", sv.View.Notice.Title)
				assert.Equal(t,
					"This session has ended, so its agent-defined view is no longer available. Its transcript is shown below.",
					sv.View.Notice.Message)
				assertNoInternalIdentifiers(t, sv.View.Notice.Title, sv.View.Notice.Message)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, clientgoscheme.AddToScheme(scheme))
			require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
			fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
				vfSessionObj(spiceboxv1alpha1.AgentSessionPhaseSucceeded),
				vfClassObj(&spiceboxv1alpha1.AgentClassUIGrant{Ref: vfUI}),
				vfUIObj(metav1.ConditionTrue, "", ""),
			).Build()
			counting := &countingClient{Client: fc}

			d := &viewFixtureDeps{k8s: counting, mem: fakeViewMemory{}, logger: logr.Discard()}

			sv, pe := viewFor(context.Background(), d, vfNS, vfSession, tc.requested)
			require.Nil(t, pe)
			tc.checkView(t, sv)

			counting.mu.Lock()
			sessionGets := counting.sessionGets
			counting.mu.Unlock()
			assert.Equal(t, 1, sessionGets,
				"the AgentSession must be Got exactly ONCE (viewFor's own first step) — a SECOND Get means "+
					"agentui.ViewFor's own walkAgentUIDoors ran too, i.e. viewFor called it for an Ended session")
		})
	}
}

// TestViewFor_EndedReason pins the disclosure a boot-failed session needs: a
// session that never ran has no transcript to explain itself and the shell's
// ended line is the only place the viewer can learn why. Without this field
// the page says "This session has ended." for a session that was refused
// seconds ago, which reads as an expiry rather than as an answer.
//
// It pins the three limits on that disclosure just as hard, because the same
// field is how operator prose would reach a person: the session's phase must
// be Failed (a finished session ended for no reason worth disclosing), the
// condition must be True (a False one describes a failure that is over), and
// the reason must be one whose message is copy
// (v1alpha1.FailureReasonWrittenForThePerson). Every withheld case still
// renders the plain ended sentence, which is true of all of them.
func TestViewFor_EndedReason(t *testing.T) {
	// The production case this exists for, spelled out rather than
	// paraphrased: the operator boot-fails a session whose starter is at the
	// workshop cap with exactly this condition.
	const failedMessage = spiceboxv1alpha1.WorkshopLimitBody

	failedSession := func(conds ...metav1.Condition) *spiceboxv1alpha1.AgentSession {
		s := vfSessionObj(spiceboxv1alpha1.AgentSessionPhaseFailed)
		s.Status.Conditions = conds
		return s
	}

	tests := []struct {
		name string
		sess *spiceboxv1alpha1.AgentSession
		want string
	}{
		{
			name: "a session that failed to boot: the Failed condition's message reaches the shell",
			sess: failedSession(metav1.Condition{
				Type:    spiceboxv1alpha1.AgentSessionConditionFailed,
				Status:  metav1.ConditionTrue,
				Reason:  spiceboxv1alpha1.ReasonAgentSessionWorkshopLimitExceeded,
				Message: failedMessage,
			}),
			want: failedMessage,
		},
		{
			// Nothing to disclose is not the same as a disclosure of nothing:
			// the shell renders its plain ended sentence rather than a colon
			// followed by silence.
			name: "a failed session whose condition carries no message: empty, not a stray colon",
			sess: failedSession(metav1.Condition{
				Type:   spiceboxv1alpha1.AgentSessionConditionFailed,
				Status: metav1.ConditionTrue,
				Reason: spiceboxv1alpha1.ReasonAgentSessionWorkshopLimitExceeded,
			}),
			want: "",
		},
		{
			// THE copy-rule row. Most Failed messages are written for an
			// operator: this is verbatim the shape the pod-start and
			// expiration writers produce, and forwarding it would put pods,
			// CR kinds and retry budgets in front of a person who can neither
			// read nor act on them. The plain ended sentence is true of this
			// session too, so nothing is lost by withholding it.
			name: "a failure written for an operator: withheld, whatever its message says",
			sess: failedSession(metav1.Condition{
				Type:   spiceboxv1alpha1.AgentSessionConditionFailed,
				Status: metav1.ConditionTrue,
				Reason: "PodStartFailed",
				Message: "runner pod demo-ns/demo-session-runner never became Ready; " +
					"AgentSession.status.retryCount=3 exceeds the class retry budget",
			}),
			want: "",
		},
		{
			// The other half of the allowlist: a reason that IS on it still
			// discloses, so the row above is proving the FILTER rather than a
			// disclosure that never worked.
			name: "a start refused by the class's allowed-starters list: disclosed, it is copy",
			sess: failedSession(metav1.Condition{
				Type:    spiceboxv1alpha1.AgentSessionConditionFailed,
				Status:  metav1.ConditionTrue,
				Reason:  spiceboxv1alpha1.ReasonAgentSessionNotAnAllowedStarter,
				Message: "This agent only runs for the people on its list, and you're not on it yet.",
			}),
			want: "This agent only runs for the people on its list, and you're not on it yet.",
		},
		{
			// A Failed condition set FALSE records that the session is NOT
			// failing — its message describes a state that is over. Reading it
			// as the reason this session ended would disclose a recovered
			// failure as the ending.
			name: "a Failed condition set False: withheld, it is not why this session ended",
			sess: failedSession(metav1.Condition{
				Type:    spiceboxv1alpha1.AgentSessionConditionFailed,
				Status:  metav1.ConditionFalse,
				Reason:  spiceboxv1alpha1.ReasonAgentSessionWorkshopLimitExceeded,
				Message: failedMessage,
			}),
			want: "",
		},
		{
			name: "a session that finished its work: no reason, because nothing went wrong",
			sess: vfSessionObj(spiceboxv1alpha1.AgentSessionPhaseSucceeded),
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, clientgoscheme.AddToScheme(scheme))
			require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
			fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
				tc.sess,
				vfClassObj(&spiceboxv1alpha1.AgentClassUIGrant{Ref: vfUI}),
				vfUIObj(metav1.ConditionTrue, "", ""),
			).Build()
			d := &viewFixtureDeps{k8s: fc, mem: fakeViewMemory{}, logger: logr.Discard()}

			sv, pe := viewFor(context.Background(), d, vfNS, vfSession, "")
			require.Nil(t, pe)
			require.Equal(t, "ended", sv.SessionOrigin, "precondition: every case here is an ended session")
			assert.Equal(t, tc.want, sv.EndedReason)

			// The browser reads this off the bootstrap JSON, so the KEY is
			// half the contract: a rename that kept the Go field would leave
			// the shell rendering nothing, with both sides still compiling.
			raw, err := json.Marshal(sv)
			require.NoError(t, err)
			var wire map[string]any
			require.NoError(t, json.Unmarshal(raw, &wire))
			if tc.want == "" {
				assert.NotContains(t, wire, "endedReason", "an absent reason must not ride the wire at all")
				return
			}
			assert.Equal(t, tc.want, wire["endedReason"])
		})
	}
}

// TestShellPageBuild_SessionSelection covers the ?session= parse + gate in
// shellPageBuild (page.go) — the half of this seam viewFor itself does not
// own, since viewFor's own contract assumes the caller already gated
// CheckInteract.
func TestShellPageBuild_SessionSelection(t *testing.T) {
	tests := []struct {
		name          string
		target        string
		checkInteract func(context.Context, string, string, string) (bool, error)
		wantStatus    int
	}{
		{name: "malformed: no slash", target: "/sessions?session=noslash", wantStatus: http.StatusBadRequest},
		{name: "malformed: two slashes", target: "/sessions?session=a/b/c", wantStatus: http.StatusBadRequest},
		{name: "malformed: empty value", target: "/sessions?session=", wantStatus: http.StatusBadRequest},
		{
			name:   "CheckInteract errors: 500, never 403",
			target: "/sessions?session=" + vfNS + "/" + vfSession,
			checkInteract: func(context.Context, string, string, string) (bool, error) {
				return false, assert.AnError
			},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:   "CheckInteract denies: 403",
			target: "/sessions?session=" + vfNS + "/" + vfSession,
			checkInteract: func(context.Context, string, string, string) (bool, error) {
				return false, nil
			},
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := newViewDeps(t, vfSessionObj(spiceboxv1alpha1.AgentSessionPhaseRunning),
				vfClassObj(&spiceboxv1alpha1.AgentClassUIGrant{Ref: vfUI}),
				vfUIObj(metav1.ConditionTrue, "", ""))
			d.checkInteract = tc.checkInteract

			r := httptest.NewRequest(http.MethodGet, tc.target, nil)
			ctx := webui.WithSubjectForTest(context.Background(), vfSubject)

			_, _, err := shellPageBuild(d)(ctx, r.WithContext(ctx))

			require.Error(t, err, "no selection may be silently dropped for a malformed or denied ?session=")
			var pe *webui.PageError
			require.ErrorAs(t, err, &pe)
			assert.Equal(t, tc.wantStatus, pe.Status)
		})
	}
}

// TestShellPageBuild_NoSessionParam_NoSelection proves the ordinary,
// unselected request (no `session` query key at all — distinct from the
// malformed-empty-value row above) still renders the plain list with
// Selected omitted, never mistaken for a malformed selection.
func TestShellPageBuild_NoSessionParam_NoSelection(t *testing.T) {
	d := newViewDeps(t)
	r := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	ctx := webui.WithSubjectForTest(context.Background(), vfSubject)

	props, _, err := shellPageBuild(d)(ctx, r.WithContext(ctx))
	require.NoError(t, err)
	sp, ok := props.(shellProps)
	require.True(t, ok)
	assert.Nil(t, sp.Selected)
}

// TestShellPageBuild_SessionSelection_HappyPath is C1's fix: a happy-path
// row driving shellPageBuild's own production wiring, not just viewFor in
// isolation. Before this test existed, page.go:122 (`include = &ref`) and
// page.go:128 (`selected = &sv`) could both be replaced with `_ = ...` and
// every suite still passed — viewFor itself had 13 rows, but its ONE
// production caller had none. This is the `trustEvent` dead-prop shape the
// standing briefing names as having bitten this branch three times: logic
// thoroughly tested, its wiring not at all.
//
// The fixture's LookupInteractableSessions always answers empty (see
// viewFixtureDeps), so this ALSO covers include's whole purpose in one row:
// the selected session appears in sp.Sessions ONLY because it was widened
// in, not because the lookup named it — proving the `include` assignment,
// not just the `selected` one.
func TestShellPageBuild_SessionSelection_HappyPath(t *testing.T) {
	d := newViewDeps(t, vfSessionObj(spiceboxv1alpha1.AgentSessionPhaseRunning),
		vfClassObj(&spiceboxv1alpha1.AgentClassUIGrant{Ref: vfUI}),
		vfUIObj(metav1.ConditionTrue, "", ""))

	r := httptest.NewRequest(http.MethodGet, "/sessions?session="+vfNS+"/"+vfSession, nil)
	ctx := webui.WithSubjectForTest(context.Background(), vfSubject)

	props, _, err := shellPageBuild(d)(ctx, r.WithContext(ctx))
	require.NoError(t, err)
	sp, ok := props.(shellProps)
	require.True(t, ok)

	require.NotNil(t, sp.Selected, "shellPageBuild must actually assign Selected, not merely compute it")
	assert.Equal(t, vfNS, sp.Selected.Ns)
	assert.Equal(t, vfSession, sp.Selected.Name)
	assert.Equal(t, viewAgentUI, sp.Selected.View.Kind)

	names := make([]string, len(sp.Sessions))
	for i, row := range sp.Sessions {
		names[i] = row.Name
	}
	assert.Contains(t, names, vfSession,
		"the selected session must be widened into the sidebar list via `include` — "+
			"the lookup itself named nothing (viewFixtureDeps always answers empty)")
}

// goldenOriginsPath is asserted, HERE, to equal agentui.AllBranches'
// CURRENT value — see TestOriginsGolden_PinsAllBranches below for exactly
// what that does and does not guarantee. No TypeScript file reads this file
// yet (a browser-side enumeration test is not part of this task), so it is
// not yet "the one artifact both halves of the seam read" — only this half
// exists today. It is generated FROM AllBranches, never hand-edited — see
// origins_golden_test.go (the REGEN_GOLDEN-gated generator, gofmt'd next to
// this file) for how to regenerate it.
const goldenOriginsPath = "ui/testdata/origins.golden.json"

// TestOriginsGolden_PinsAllBranches proves origins.golden.json is
// byte-for-byte agentui.AllBranches, so a regenerated golden that silently
// dropped or reordered a branch — or a Branch added to AllBranches with the
// golden left stale — is caught HERE.
//
// It does NOT (and per AllBranches' own doc comment, cannot) prove the
// converse: a NEW Branch added to AllBranches with the golden regenerated to
// match passes this test cleanly, because AllBranches pins the browser's
// enumeration to ITSELF, not the other way around. That failure surfaces one
// layer up, in the browser test that has no `case` for the new value — see
// AllBranches' own doc comment for why that is the intended shape, not a gap
// in this test.
func TestOriginsGolden_PinsAllBranches(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(goldenOriginsPath))
	require.NoError(t, err, "the golden the frontend suite reads must exist")

	var want []string
	require.NoError(t, json.Unmarshal(raw, &want))

	got := make([]string, len(agentui.AllBranches))
	for i, b := range agentui.AllBranches {
		got[i] = string(b)
	}
	assert.Equal(t, want, got,
		"%s no longer matches agentui.AllBranches — regenerate it", goldenOriginsPath)
}

// TestSelectedView_ArchiveSweptSession_DisagreesWithRowButResolvesCorrectly
// is the spanning proof for the DECIDED divergence documented on
// sessionRow.Ended (list.go): an archive-swept session (phase Succeeded,
// WakeEligible via spiceboxv1alpha1.ArchivedBySweep) reports Ended==true in
// its SIDEBAR ROW, built by buildSessionList — but viewFor, resolving the
// SAME session independently via agentui.ResolveSession, reports
// SessionOrigin=="asleep", never "ended". The row is a stale badge for this
// one case; the opened view is what a viewer actually lands on, and it is
// correct — a live, resumable session, not a dead end. Reconciling the row to
// match would be a legitimate alternative design; this test pins the one
// actually shipped: the SELECTION is authoritative, the row is not consulted
// to decide anything, and the two are allowed to disagree.
func TestSelectedView_ArchiveSweptSession_DisagreesWithRowButResolvesCorrectly(t *testing.T) {
	archived := vfSessionObj(spiceboxv1alpha1.AgentSessionPhaseSucceeded)
	archived.Status.Conditions = []metav1.Condition{{
		Type:   spiceboxv1alpha1.AgentSessionConditionIdle,
		Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonAgentSessionArchived,
	}}

	d := newViewDeps(t, archived, vfClassObj(&spiceboxv1alpha1.AgentClassUIGrant{Ref: vfUI}),
		vfUIObj(metav1.ConditionTrue, "", ""))

	rows, _, err := buildSessionList(context.Background(), d, vfSubject,
		&spicedb.SessionRef{Namespace: vfNS, Name: vfSession}, false)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.True(t, rows[0].Ended, "the row reports Ended for an archive-swept session — this is the documented, accepted divergence")

	sv, pe := viewFor(context.Background(), d, vfNS, vfSession, "")
	require.Nil(t, pe)
	assert.Equal(t, "asleep", sv.SessionOrigin,
		"the SELECTION must resolve independently and correctly, disagreeing with the row's stale Ended badge")
	assert.NotEqual(t, "ended", sv.SessionOrigin)
}
