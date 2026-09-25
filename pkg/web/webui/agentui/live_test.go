package agentui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/gorilla/websocket"
	natsserver "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
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
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiaction"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/browserstart"
	"github.com/authzed/openagentprimitives/pkg/web/webui/internal/wstest"
)

// This file is package agentui (internal), not agentui_test, so it can call
// liveHandler/runActionMirror directly — the same shape bindings_test.go and
// actions_test.go use for their own routes.

// --- fakes -------------------------------------------------------------

// liveFakeDeps implements agentui.Deps for live.go's own test suite.
type liveFakeDeps struct {
	interactOK  bool
	interactErr error
	origin      string
	k8s         client.Client
	mem         memory.Memory
	nc          *nats.Conn
	logger      logr.Logger
}

func (d *liveFakeDeps) CheckInteract(context.Context, string, string, string) (bool, error) {
	return d.interactOK, d.interactErr
}
func (d *liveFakeDeps) K8s() client.Client                                      { return d.k8s }
func (d *liveFakeDeps) Logger() logr.Logger                                     { return d.logger }
func (d *liveFakeDeps) TrustedOrigin() string                                   { return d.origin }
func (d *liveFakeDeps) NATSRequest() channelevents.RequestFunc                  { return nil }
func (d *liveFakeDeps) Memory() memory.Memory                                   { return d.mem }
func (d *liveFakeDeps) Artifacts() *artifacts.Service                           { return nil }
func (d *liveFakeDeps) ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc { return nil }
func (d *liveFakeDeps) NATS() *nats.Conn                                        { return d.nc }

// StartBrowserSession is nil: no case in this file exercises the start
// route, only .../live.
func (d *liveFakeDeps) StartBrowserSession() browsersession.StartFunc { return nil }

// LiveSessions is nil for the same reason: only the start route reserves.
func (d *liveFakeDeps) LiveSessions() browserstart.LiveSessions { return nil }
func (d *liveFakeDeps) StartableNamespaces() []string           { return nil }
func (d *liveFakeDeps) WorkshopNamespacesFor(context.Context, string) ([]string, error) {
	return nil, nil
}

var _ Deps = (*liveFakeDeps)(nil)

// fakeUIActionMemory is a minimal thread-safe memory.Memory test double
// backing uiaction.Record/List — enough for THIS route's tests (a Kind
// filter and newest-first ordering), not a general-purpose memory fake.
type fakeUIActionMemory struct {
	mu      sync.Mutex
	entries map[string]memory.Entry

	// beforeQuery, when set, fires ONCE, synchronously, BEFORE this Query
	// call's own result is computed — so whatever it writes lands IN this
	// query's own returned snapshot. Used only by
	// TestLiveRouteSettleRacingBothArmsIsNeverDuplicated to construct the
	// case where a settle is visible to the snapshot AND already subscribed
	// on the live channel, without a sleep-based race.
	beforeQuery func()

	// afterQuerySnapshot, when set, fires ONCE, synchronously, AFTER this
	// Query call's own result has already been computed (so THIS query's
	// return value is unaffected by it). Used only by
	// TestLiveRouteSettleRacingSnapshotAndSubscribeIsNeverLost to land a
	// settle deterministically inside the window between "the snapshot read
	// captured its data" and "the snapshot frame reaches the client".
	afterQuerySnapshot func()
}

func (m *fakeUIActionMemory) Put(_ context.Context, e memory.Entry) (memory.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = map[string]memory.Entry{}
	}
	m.entries[e.ID] = e
	return e, nil
}

func (m *fakeUIActionMemory) Query(_ context.Context, q memory.Query) (memory.QueryResult, error) {
	m.mu.Lock()
	before := m.beforeQuery
	m.beforeQuery = nil
	m.mu.Unlock()
	if before != nil {
		before()
	}

	m.mu.Lock()
	all := make([]memory.Entry, 0, len(m.entries))
	for _, e := range m.entries {
		if len(q.Kinds) > 0 && !slices.Contains(q.Kinds, e.Kind) {
			continue
		}
		if len(q.IDs) > 0 && !slices.Contains(q.IDs, e.ID) {
			continue
		}
		all = append(all, e)
	}
	after := m.afterQuerySnapshot
	m.afterQuerySnapshot = nil
	m.mu.Unlock()

	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })

	if after != nil {
		after()
	}
	return memory.QueryResult{Entries: all}, nil
}

func (m *fakeUIActionMemory) Search(context.Context, memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{}, nil
}
func (m *fakeUIActionMemory) SendSignal(context.Context, memory.Signal) error { return nil }

var _ memory.Memory = (*fakeUIActionMemory)(nil)

// --- fixture -------------------------------------------------------------

// liveFixture wires one liveHandler-backed httptest.Server over a real
// embedded NATS server (so livemirror.WatchOutbound exercises its real
// subscribe/publish behavior, matching sessionview's own live_test.go
// approach) and a fakeUIActionMemory, mounted for ONE fixed subject — every
// f.dial connects as that same subject, mirroring one browser tab
// reconnecting, not a new viewer. f.rt is a uiview.Runtime sharing this SAME
// mem/k8s pair, so a fragment written through it is exactly what the live
// route's own resolveView (J4's server side) reads back — the same
// shared-backend discipline viewmodel_test.go's newAgentUIFixture uses for
// J2.
type liveFixture struct {
	ns, name string
	subject  string
	scope    memory.Scope
	d        *liveFakeDeps
	mem      *fakeUIActionMemory
	k8s      client.Client
	rt       *uiview.Runtime
	pubConn  *nats.Conn
	srv      *httptest.Server
}

const (
	// liveViewClass/liveViewUI/liveViewHook/liveViewTool name the AgentUI
	// object graph newLiveFixture seeds so resolveView (the view arm's read
	// path) has a real AgentSession -> AgentClass -> AgentUI ladder to walk —
	// the live route's action arm never needed one before this task, since
	// only resolveView (not uiaction) touches K8s.
	liveViewClass = "wsl-class"
	liveViewUI    = "wsl-ui"
	liveViewHook  = "panel"
	liveViewTool  = "crm_list_leads"
)

// newLiveViewFixtureClient builds the AgentSession/AgentClass/AgentUI graph
// resolveView needs: a live session, a class granting liveViewTool, and an
// AgentUI whose page is declared through the legacy spec.slots shim: one
// agent-writable slot, which compiles into the single hook named liveViewHook,
// with a plain ap:text placeholder as its Tier-0 default — so a test that
// narrows the grant underneath a tool-bound fragment can assert the exact
// Tier-0 fallback.
func newLiveViewFixtureClient(t *testing.T, ns, name string) client.Client {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: liveViewClass},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: liveViewClass, Namespace: ns},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: liveViewUI, GrantedTools: []string{liveViewTool}},
		},
	}
	aui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: liveViewUI, Namespace: ns},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Tools: []string{liveViewTool},
			Slots: []spiceboxv1alpha1.AgentUISlot{{
				Name:          liveViewHook,
				AgentWritable: true,
				Default: &apiextensionsv1.JSON{Raw: []byte(
					`{"component":"ap:text","props":{"text":"placeholder"}}`)},
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

func newLiveFixture(t *testing.T, subject string) *liveFixture {
	t.Helper()
	const ns, name = "wsl", "sess-live"

	natsSrv := natstest.RunServer(&natsserver.Options{Port: -1})
	t.Cleanup(natsSrv.Shutdown)
	subConn, err := nats.Connect(natsSrv.ClientURL())
	require.NoError(t, err)
	t.Cleanup(subConn.Close)
	pubConn, err := nats.Connect(natsSrv.ClientURL())
	require.NoError(t, err)
	t.Cleanup(pubConn.Close)

	mem := &fakeUIActionMemory{}
	k8s := newLiveViewFixtureClient(t, ns, name)
	d := &liveFakeDeps{interactOK: true, k8s: k8s, mem: mem, nc: subConn, logger: logr.Discard()}

	mux := http.NewServeMux()
	mux.Handle("/agent-ui/{ns}/{name}/live", withTestSubject(subject, liveHandler(d)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	d.origin = srv.URL

	granted := map[string]bool{liveViewTool: true}
	rt := &uiview.Runtime{
		Namespace: ns, Session: name, UIName: liveViewUI,
		Client: k8s, Mem: mem,
		ToolOptions: func() uicomponents.Options {
			o := uicomponents.DefaultOptions()
			o.GrantedTools = granted
			o.ReadonlyTools = granted
			return o
		},
	}

	return &liveFixture{
		ns: ns, name: name, subject: subject,
		scope:   memory.Scope{Kind: "session", ID: ns + "/" + name},
		d:       d,
		mem:     mem,
		k8s:     k8s,
		rt:      rt,
		pubConn: pubConn,
		srv:     srv,
	}
}

// narrowGrant simulates a bundle redeploy/consent change: the AgentClass no
// longer grants liveViewTool. Mirrors viewmodel_test.go's shrinkGrant for
// this fixture's own object graph — a SEPARATE helper (not a reuse) because
// that one hardcodes viewmodel_test.go's own fixture's tool names.
func (f *liveFixture) narrowGrant(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var ac spiceboxv1alpha1.AgentClass
	require.NoError(t, f.k8s.Get(ctx, client.ObjectKey{Namespace: f.ns, Name: liveViewClass}, &ac))
	require.NotNil(t, ac.Spec.AgentUI)
	ac.Spec.AgentUI.GrantedTools = nil
	require.NoError(t, f.k8s.Update(ctx, &ac))
}

// publishViewUpdate publishes a KindUIViewUpdate envelope directly — the
// PUSH half of a real Write, decoupled from the durable half so a test can
// drive them at different points (mirrors settleFull's identical
// write-then-publish split for the action arm, just without going through
// f.rt.Write's own Publish wiring).
func (f *liveFixture) publishViewUpdate(t *testing.T, hook string) {
	t.Helper()
	payload := channelevents.UIViewUpdatePayload{Hook: hook, UpdatedAt: time.Now().UTC()}
	publish := func(subj string, data []byte) error { return f.pubConn.Publish(subj, data) }
	require.NoError(t, channelevents.PublishOut(publish, f.ns, f.name, channelevents.KindUIViewUpdate, payload))
	require.NoError(t, f.pubConn.Flush())
}

// readViewRaw reads frames off conn, skipping any non-"view" frame, until it
// finds the next view-arm frame, and returns its BYTES. Skip semantics are
// load-bearing, not convenience: this route's connect sequence sends the
// action snapshot BEFORE the view snapshot (both unconditionally, on every
// open — see runActionMirror), so a caller reading only the view arm must be
// able to look past the action frame without needing to know its exact shape.
//
// Bytes rather than a decoded liveViewMessage because the golden capture
// (live_golden_internal_test.go) must compare the route's OWN bytes against
// the file the browser reads — decoding through the Go struct on the way
// would launder exactly the json-tag renames that comparison exists to catch.
// readView below decodes for every other caller.
func (f *liveFixture) readViewRaw(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	for {
		raw := f.readRaw(t, conn)
		var probe struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(raw, &probe), "unmarshal ws frame")
		if probe.Type == "view" {
			return raw
		}
	}
}

// readView is readViewRaw decoded into this route's own view-frame type, for
// the tests that assert on the resolved declaration rather than on the wire.
func (f *liveFixture) readView(t *testing.T, conn *websocket.Conn) liveViewMessage {
	t.Helper()
	raw := f.readViewRaw(t, conn)
	var msg liveViewMessage
	require.NoError(t, json.Unmarshal(raw, &msg), "unmarshal view frame")
	return msg
}

// dialAs opens a connection to THIS fixture's session as an ARBITRARY
// subject — used only to prove the view arm is NOT requester-filtered (two
// different viewers of one session must both receive a push). f.srv's own
// mux fixes its subject at construction via withTestSubject, so a second
// viewer needs its own short-lived server; it shares f.d (same
// memory/K8s/NATS state) and dials with f.d.origin as the Origin header
// (the ORIGIN CheckOrigin actually compares against — see liveHandler),
// never the second server's own URL.
func (f *liveFixture) dialAs(t *testing.T, subject string) *websocket.Conn {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/agent-ui/{ns}/{name}/live", withTestSubject(subject, liveHandler(f.d)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/agent-ui/" + f.ns + "/" + f.name + "/live"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{f.d.origin}})
	require.NoError(t, err, "dial live socket as another viewer")
	t.Cleanup(func() { conn.Close() })
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	return conn
}

// withTestSubject injects subject into the request context via
// webui.WithSubjectForTest before calling next — the real HTTP-handshake
// equivalent of what postBindings/doPostAction do directly on a
// *http.Request for the POST routes' httptest.Recorder-based tests. A
// websocket test needs a real server (websocket.DefaultDialer.Dial performs
// an actual handshake), so the subject has to be injected by server-side
// middleware rather than by mutating a request object the client never
// round-trips through.
func withTestSubject(subject string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subject != "" {
			r = r.WithContext(webui.WithSubjectForTest(r.Context(), subject))
		}
		next.ServeHTTP(w, r)
	})
}

// dial opens a new client connection against the fixture's running server,
// as the fixture's fixed subject, with the trusted Origin header set.
func (f *liveFixture) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	wsURL := "ws://" + strings.TrimPrefix(f.srv.URL, "http://") + "/agent-ui/" + f.ns + "/" + f.name + "/live"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{f.srv.URL}})
	require.NoError(t, err, "dial live socket")
	t.Cleanup(func() { conn.Close() })
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	return conn
}

func (f *liveFixture) read(t *testing.T, conn *websocket.Conn) liveActionMessage {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(wstest.Deadline(3*time.Second)))
	_, raw, err := conn.ReadMessage()
	require.NoError(t, err, "read ws frame")
	var msg liveActionMessage
	require.NoError(t, json.Unmarshal(raw, &msg), "unmarshal ws frame")
	return msg
}

func (f *liveFixture) readRaw(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(wstest.Deadline(3*time.Second)))
	_, raw, err := conn.ReadMessage()
	require.NoError(t, err, "read ws frame")
	return raw
}

// ensureSubscribed blocks until every write already queued on f.d.nc — in
// particular, WatchOutbound's SUB frame for ui_action_update, since
// subscribe strictly precedes this route's snapshot read (see
// runActionMirror's own doc comment) — has reached and been processed by
// the NATS server.
//
// nats.Conn buffers protocol writes rather than flushing them synchronously,
// so "Subscribe returned" does not by itself mean the server has registered
// the subscription yet: a Publish moments later on a DIFFERENT connection
// (f.pubConn) can race ahead of it and be silently dropped (core NATS has no
// queueing for a not-yet-registered subscriber). Flush sends a PING
// immediately after whatever is already queued on THIS SAME connection and
// blocks for the PONG, which the server can only send once everything ahead
// of it — the SUB — has been processed. A publish issued once this call
// returns is therefore GUARANTEED delivered, not merely likely to be; this
// replaces a probabilistic retry-publish loop with an exact synchronization
// point, and is safe to call from either the test's own goroutine (once it
// has observed proof subscribe was called, e.g. the snapshot frame) or from
// inside a fakeUIActionMemory hook running ON THE SAME GOROUTINE as the
// subscribe call itself (WatchOutbound always precedes uiaction.List in
// runActionMirror, so by the time any Query hook fires, the Subscribe call
// on this exact connection has already unconditionally happened).
func (f *liveFixture) ensureSubscribed(t *testing.T) {
	t.Helper()
	require.NoError(t, f.d.nc.Flush(), "flush the subscribing connection")
}

// settle writes (then publishes) one action record AS the fixture's own
// subject — the write-then-publish order uiActionRecorder uses in
// production, reproduced here so the test exercises this route the same way
// a real settle would.
func (f *liveFixture) settle(t *testing.T, requestID, action string, state uiaction.State) {
	t.Helper()
	f.settleAs(t, f.subject, requestID, action, state)
}

// settleAs writes (then publishes) one action record as an ARBITRARY
// SpiceDB subject — used to prove the requester filter (a settle "as"
// someone other than the fixture's own connected viewer must never reach
// that viewer's socket).
func (f *liveFixture) settleAs(t *testing.T, spicedbSubject, requestID, action string, state uiaction.State) {
	t.Helper()
	f.settleFull(t, spicedbSubject, requestID, action, state, false, time.Now().UTC())
}

// settleFull is the one settle body every other settle helper delegates to.
// addressedToViewer and at are parameters rather than derived here because
// the golden the frontend suite reads (live_golden_internal_test.go) needs
// BOTH pinned: a wall-clock UpdatedAt would make the golden's bytes differ on
// every run, and the approval-addressability flag is exactly the field the
// Go↔TS live-frame seam exists to protect.
func (f *liveFixture) settleFull(t *testing.T, spicedbSubject, requestID, action string, state uiaction.State, addressedToViewer bool, at time.Time) {
	t.Helper()
	requester := uiaction.RequesterKey(spicedbSubject)
	msg := uiaction.DisplayCopy(state, addressedToViewer)

	require.NoError(t, uiaction.Record(context.Background(), f.mem, f.scope, uiaction.Content{
		RequestID:                 requestID,
		Action:                    action,
		State:                     state,
		Message:                   msg,
		ApprovalAddressedToViewer: addressedToViewer,
		UpdatedAt:                 at,
		Requester:                 requester,
	}))

	payload := channelevents.UIActionUpdatePayload{
		RequestID:                 requestID,
		Action:                    action,
		Requester:                 requester,
		State:                     string(state),
		Message:                   msg,
		ApprovalAddressedToViewer: addressedToViewer,
		UpdatedAt:                 at,
	}
	publish := func(subj string, data []byte) error { return f.pubConn.Publish(subj, data) }
	require.NoError(t, channelevents.PublishOut(publish, f.ns, f.name, channelevents.KindUIActionUpdate, payload))
	require.NoError(t, f.pubConn.Flush())
}

// --- J2: the detached-result join ----------------------------------------

// TestLiveRouteSpansTheDetachedResultJoin owns J2: a terminal state produced
// by a DETACHED exec must reach a browser on BOTH arms — the one that was
// connected when it settled, and the one that opened afterwards. Two tests,
// one seeded session, one asserted outcome.
//
// A test that only exercises the live arm proves nothing about reload, which
// is the arm the design spec explicitly requires ("survives a tab reload and
// an approval that takes ten minutes").
func TestLiveRouteSpansTheDetachedResultJoin(t *testing.T) {
	const subject = "user:viewer-1" // PREFIXED, as webui.SubjectFromContext supplies it

	t.Run("live arm: a socket open across the push receives the terminal state", func(t *testing.T) {
		f := newLiveFixture(t, subject)
		conn := f.dial(t)
		require.Equal(t, "snapshot", f.read(t, conn).Type, "the first frame is always the snapshot")
		_ = f.readView(t, conn) // the view arm's own open-time frame, sent right after — see runActionMirror

		f.settle(t, "req-1", "advance", uiaction.StateSucceeded) // writes memory, then publishes
		msg := f.read(t, conn)
		require.Equal(t, "event", msg.Type)
		require.NotNil(t, msg.Action)
		assert.Equal(t, string(uiaction.StateSucceeded), msg.Action.State)
	})

	t.Run("reload arm: a FRESH socket opened after it settled receives the same state", func(t *testing.T) {
		f := newLiveFixture(t, subject)
		f.settle(t, "req-1", "advance", uiaction.StateSucceeded) // nobody is connected

		conn := f.dial(t)
		msg := f.read(t, conn)
		require.Equal(t, "snapshot", msg.Type)
		require.Len(t, msg.Actions, 1, "the record survived with no socket attached — that IS the closed gap")
		assert.Equal(t, string(uiaction.StateSucceeded), msg.Actions[0].State)
		assert.Equal(t, "req-1", msg.Actions[0].RequestID)
	})

	t.Run("both arms agree, field for field", func(t *testing.T) {
		f := newLiveFixture(t, subject)
		conn := f.dial(t)
		_ = f.read(t, conn)
		_ = f.readView(t, conn) // the view arm's own open-time frame
		// The snapshot arriving already proves subscribe was CALLED (it
		// strictly precedes the snapshot read in runActionMirror), but not
		// that the SUB frame has reached the server yet — see
		// ensureSubscribed's doc comment. Without this, settle's publish
		// (on a separate connection) can race that registration.
		f.ensureSubscribed(t)
		f.settle(t, "req-1", "advance", uiaction.StateSucceeded)
		live := *f.read(t, conn).Action

		reloaded := f.read(t, f.dial(t)).Actions[0]
		assert.Equal(t, live, reloaded,
			"memory is the source of truth; if these differ, live and reconnect have diverged")
	})
}

func TestLiveRouteFiltersToTheConnectedViewer(t *testing.T) {
	f := newLiveFixture(t, "user:viewer-1")
	f.settleAs(t, "user:viewer-2", "req-other", "advance", uiaction.StateAwaitingApproval)

	msg := f.read(t, f.dial(t))
	assert.Empty(t, msg.Actions, "another viewer's in-flight action must never appear on this page")
}

func TestLiveRouteNeverLeaksTheRequesterToTheBrowser(t *testing.T) {
	f := newLiveFixture(t, "user:viewer-1")
	f.settle(t, "req-1", "advance", uiaction.StateAwaitingApproval)
	raw := f.readRaw(t, f.dial(t))
	assert.NotContains(t, string(raw), "viewer-1",
		"Requester is the server's filter input, not a field a browser receives")
}

func TestLiveRouteWithNilMemoryFailsClosedLoudly(t *testing.T) {
	var mu sync.Mutex
	var logged []string
	capLogger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})

	f := newLiveFixture(t, "user:viewer-1")
	f.d.mem = nil
	f.d.logger = capLogger

	conn := f.dial(t)
	msg := f.read(t, conn)
	assert.Equal(t, "error", msg.Type)
	assert.NotEmpty(t, msg.Message,
		"an empty snapshot would read as \"no pending actions\" — a claim nothing checked")
	assert.Empty(t, msg.Actions)

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, logged, "a nil Memory must be logged, not silently degraded to an empty snapshot")
	assert.Contains(t, strings.Join(logged, "\n"), "Memory")
}

// --- gates -----------------------------------------------------------------

func TestLiveRouteGates(t *testing.T) {
	// Table over: no subject -> 401; CheckInteract error -> 500 (never read as
	// a denial); CheckInteract false -> 403; wrong Origin -> upgrade refused.
	// Each asserts the socket never opened.
	cases := []struct {
		name        string
		subject     string
		interactOK  bool
		interactErr error
		badOrigin   bool
		wantStatus  int
	}{
		{name: "no subject -> 401, never reaches CheckInteract",
			subject: "", interactOK: true, wantStatus: http.StatusUnauthorized},
		{name: "CheckInteract error -> 500, never read as a denial",
			subject: "user:eve", interactErr: assert.AnError, wantStatus: http.StatusInternalServerError},
		{name: "CheckInteract false -> 403",
			subject: "user:eve", interactOK: false, wantStatus: http.StatusForbidden},
		{name: "wrong Origin -> upgrade refused with 403",
			subject: "user:eve", interactOK: true, badOrigin: true, wantStatus: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &liveFakeDeps{interactOK: tc.interactOK, interactErr: tc.interactErr, logger: logr.Discard()}
			mux := http.NewServeMux()
			mux.Handle("/agent-ui/{ns}/{name}/live", withTestSubject(tc.subject, liveHandler(d)))
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			d.origin = srv.URL

			dialOrigin := srv.URL
			if tc.badOrigin {
				dialOrigin = "https://evil.example"
			}
			wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/agent-ui/gatens/gatesess/live"
			conn, resp, _ := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{dialOrigin}})
			if conn != nil {
				t.Cleanup(func() { conn.Close() })
			}
			assert.Nil(t, conn, "a gated request must never complete the ws handshake")
			require.NotNil(t, resp)
			assert.Equal(t, tc.wantStatus, resp.StatusCode)
		})
	}
}

// --- the snapshot/subscribe race -------------------------------------------

// TestLiveRouteSettleRacingSnapshotAndSubscribeIsNeverLost proves the "lost"
// half of the snapshot/stream race: a settle landing exactly after the
// snapshot's own memory read has already computed its result (so the
// snapshot the client is about to receive does NOT include it) must still
// reach the browser — via the live channel, since this route subscribes
// BEFORE reading the snapshot. If a future change reordered that (snapshot
// then subscribe), the settle here would race a NATS subscription that does
// not exist yet: gone forever, not merely delayed. See runActionMirror's own
// doc comment.
func TestLiveRouteSettleRacingSnapshotAndSubscribeIsNeverLost(t *testing.T) {
	f := newLiveFixture(t, "user:viewer-1")
	f.mem.afterQuerySnapshot = func() {
		// This hook runs on the SAME goroutine as runActionMirror itself
		// (called from inside uiaction.List -> mem.Query), strictly AFTER
		// WatchOutbound's Subscribe call in program order — so
		// ensureSubscribed's Flush round-trip on f.d.nc deterministically
		// proves the SUB frame has reached the server before settle's publish
		// (on the separate f.pubConn) is issued. See ensureSubscribed's doc
		// comment for why "Subscribe returned" alone is not enough.
		f.ensureSubscribed(t)
		f.settle(t, "req-race", "advance", uiaction.StateSucceeded)
	}

	conn := f.dial(t)
	snap := f.read(t, conn)
	require.Equal(t, "snapshot", snap.Type)
	assert.Empty(t, snap.Actions, "the settle landed AFTER this query's own read — it must be absent from THIS snapshot")
	_ = f.readView(t, conn) // the view arm's own open-time frame, sent right after the action snapshot

	evt := f.read(t, conn)
	require.Equal(t, "event", evt.Type, "the settle must still arrive — as a live event, since it raced the snapshot")
	require.NotNil(t, evt.Action)
	assert.Equal(t, "req-race", evt.Action.RequestID)
	assert.Equal(t, string(uiaction.StateSucceeded), evt.Action.State)
}

// TestLiveRouteSettleRacingBothArmsIsNeverDuplicated proves the "duplicated"
// half of the same race: a settle landing BEFORE the snapshot's own memory
// read computes its result (so it IS captured by the snapshot) races an
// already-active subscription that ALSO captures the corresponding publish —
// the same fact arriving from both sources. The browser must see it exactly
// once.
func TestLiveRouteSettleRacingBothArmsIsNeverDuplicated(t *testing.T) {
	f := newLiveFixture(t, "user:viewer-1")
	f.mem.beforeQuery = func() {
		// Same deterministic synchronization as
		// TestLiveRouteSettleRacingSnapshotAndSubscribeIsNeverLost: without it,
		// an occasional dropped publish (the cross-connection SUB/PUB ordering
		// race — see ensureSubscribed's doc comment) would make THIS test pass
		// vacuously (no event arrives to duplicate, whether or not dedup
		// actually works) rather than genuinely exercising sentAsOfSnapshot's
		// suppression.
		f.ensureSubscribed(t)
		f.settle(t, "req-dup", "advance", uiaction.StateSucceeded)
	}

	conn := f.dial(t)
	snap := f.read(t, conn)
	require.Equal(t, "snapshot", snap.Type)
	require.Len(t, snap.Actions, 1, "the settle landed BEFORE this query's own read — it belongs in THIS snapshot")
	assert.Equal(t, "req-dup", snap.Actions[0].RequestID)
	_ = f.readView(t, conn) // the view arm's own open-time frame, sent right after the action snapshot

	// The corresponding publish also happened while already subscribed
	// (subscribe-before-snapshot) — assert it is NOT replayed as a second
	// frame carrying the identical fact.
	// Deliberately an unscaled literal: this read waits for NOTHING, so its
	// timeout IS the pass condition. wstest.Scale belongs on the reads above,
	// which wait for a frame that is on its way; applying it here would only
	// lengthen the pass path.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(500*time.Millisecond)))
	_, _, err := conn.ReadMessage()
	assert.Error(t, err, "no further frame should arrive — a replay would be a duplicate of the settle already in the snapshot")
}

// --- disconnect cleanup -----------------------------------------------------

// TestLiveRouteClientDisconnectUnsubscribesFromNATS proves the read-pump
// wiring in liveHandler: closing the CLIENT side of the socket must cancel
// runActionMirror's context, which must tear down its NATS subscription.
// NumSubscriptions is a direct, deterministic signal of that cleanup — a
// leaked subscription here is a leaked goroutine (livemirror.WatchOutbound's
// own unsubscribe-on-ctx.Done goroutine, plus runActionMirror's own blocked
// select) that accumulates per client visit and would block a clean
// shutdown.
func TestLiveRouteClientDisconnectUnsubscribesFromNATS(t *testing.T) {
	f := newLiveFixture(t, "user:viewer-1")
	conn := f.dial(t)
	_ = f.read(t, conn) // snapshot — proves the mirror is up and (by then) subscribed

	require.Eventually(t, func() bool { return f.d.nc.NumSubscriptions() > 0 }, time.Second, 10*time.Millisecond,
		"the mirror must be subscribed to ui_action_update by the time the snapshot has been sent")

	require.NoError(t, conn.Close())

	assert.Eventually(t, func() bool { return f.d.nc.NumSubscriptions() == 0 }, 2*time.Second, 20*time.Millisecond,
		"closing the client connection must cancel the mirror's context, which must unsubscribe — "+
			"a leaked subscription here is a leaked goroutine that blocks a clean shutdown")
}

// --- J4: an update_view lands on an open page --------------------------

// TestLiveViewUpdatePushesWhatWEBDResolved owns J4. Arm one: an accepted
// fragment reaches an open socket. Arm two: a fragment that is NOT valid
// under the CURRENT grant produces a frame carrying the Tier-0 node. A push
// that forwarded the triggering envelope's own contents (rather than
// re-resolving) would pass arm one and fail arm two — which is exactly why
// arm two exists (see channelevents.UIViewUpdatePayload's own doc comment).
func TestLiveViewUpdatePushesWhatWEBDResolved(t *testing.T) {
	t.Run("an accepted fragment reaches an open socket", func(t *testing.T) {
		f := newLiveFixture(t, "user:viewer-1")
		conn := f.dial(t)
		_ = f.readView(t, conn) // drain the open-time view frame (skips the action snapshot ahead of it)

		_, err := f.rt.Write(context.Background(), liveViewHook,
			mustNode(t, `{"component":"ap:markdown","props":{"body":"agent copy"}}`))
		require.NoError(t, err)
		f.ensureSubscribed(t) // proves the SUB frames reached the server before the publish below
		f.publishViewUpdate(t, liveViewHook)

		msg := f.readView(t, conn)
		assert.Equal(t, liveViewHook, msg.Hook)
		panel := hookNode(t, msg.Declaration.View, liveViewHook)
		require.Len(t, panel.Children, 1)
		assert.Equal(t, "ap:markdown", panel.Children[0].Component)
		assert.Contains(t, msg.Declaration.AgentComposed, liveViewHook)
	})

	t.Run("a fragment invalid under the current grant pushes the Tier-0 node, never the fragment", func(t *testing.T) {
		f := newLiveFixture(t, "user:viewer-1")
		_, err := f.rt.Write(context.Background(), liveViewHook, mustNode(t,
			`{"component":"ap:table","bindings":{"rows":{"source":"tool","ref":"`+liveViewTool+`"}}}`))
		require.NoError(t, err, "the write is accepted under the CURRENT (wide) grant")
		f.narrowGrant(t) // the deployment narrows AFTER the write — same shape as viewmodel_test.go's J2 span

		conn := f.dial(t)
		_ = f.readView(t, conn)
		f.ensureSubscribed(t)
		f.publishViewUpdate(t, liveViewHook)

		msg := f.readView(t, conn)
		panel := hookNode(t, msg.Declaration.View, liveViewHook)
		require.Len(t, panel.Children, 1)
		assert.Equal(t, "ap:text", panel.Children[0].Component, "the hook falls back to its Tier-0 default")
		assert.NotContains(t, msg.Declaration.AgentComposed, liveViewHook, "a fallen-back hook is never marked agent-composed")
	})
}

// TestLiveClearPushesAFrameNamingTheHook proves a Clear pushes exactly the
// same shape of frame a Write does: the hook that changed is still named,
// even though a clear leaves nothing to diff against — the "something
// changed here" cue for a removal has no content to point at besides the
// hook's own name.
func TestLiveClearPushesAFrameNamingTheHook(t *testing.T) {
	f := newLiveFixture(t, "user:viewer-1")
	conn := f.dial(t)
	_ = f.readView(t, conn) // drain the open-time view frame

	_, err := f.rt.Clear(context.Background(), liveViewHook)
	require.NoError(t, err, "a hook nothing has touched may still be cleared")
	f.ensureSubscribed(t)
	f.publishViewUpdate(t, liveViewHook)

	msg := f.readView(t, conn)
	assert.Equal(t, liveViewHook, msg.Hook)
	panel := hookNode(t, msg.Declaration.View, liveViewHook)
	assert.Empty(t, panel.Children, "a cleared hook carries no content")
	assert.Contains(t, msg.Declaration.AgentComposed, liveViewHook, "a clear is still a compose — the marker names it too")
}

// TestLiveOpenFrameCarriesTheCurrentView proves the OTHER half of
// subscribe-then-snapshot for the view arm: a write can land between the
// page rendering its bootstrap and the socket opening (or between a
// disconnect and a reconnect), and the open-time frame must still carry it —
// mirrors TestLiveRouteSpansTheDetachedResultJoin's "reload arm" for the
// action arm.
func TestLiveOpenFrameCarriesTheCurrentView(t *testing.T) {
	f := newLiveFixture(t, "user:viewer-1")
	_, err := f.rt.Write(context.Background(), liveViewHook,
		mustNode(t, `{"component":"ap:markdown","props":{"body":"agent copy"}}`))
	require.NoError(t, err)

	conn := f.dial(t)
	msg := f.readView(t, conn) // skips the action snapshot automatically
	assert.Empty(t, msg.Hook, "the open-time frame is a snapshot, not one hook's update")
	panel := hookNode(t, msg.Declaration.View, liveViewHook)
	require.Len(t, panel.Children, 1)
	assert.Equal(t, "ap:markdown", panel.Children[0].Component)
}

// TestLiveViewUpdateIsNotRequesterFiltered proves the view arm's own
// contract (liveViewMessage's doc comment): TWO different viewers of one
// session must BOTH receive a push. A single-viewer test would pass
// vacuously whether or not a requester filter had been copied in from the
// action arm — see the mutation this test exists to catch (task-9-brief.md
// mutation 2).
func TestLiveViewUpdateIsNotRequesterFiltered(t *testing.T) {
	f := newLiveFixture(t, "user:viewer-1")
	conn1 := f.dial(t)
	_ = f.readView(t, conn1)

	conn2 := f.dialAs(t, "user:viewer-2")
	_ = f.readView(t, conn2)

	f.ensureSubscribed(t)
	_, err := f.rt.Write(context.Background(), liveViewHook,
		mustNode(t, `{"component":"ap:markdown","props":{"body":"agent copy"}}`))
	require.NoError(t, err)
	f.publishViewUpdate(t, liveViewHook)

	msg1 := f.readView(t, conn1)
	msg2 := f.readView(t, conn2)
	assert.Equal(t, "ap:markdown", hookNode(t, msg1.Declaration.View, liveViewHook).Children[0].Component,
		"the FIRST viewer must receive the update")
	assert.Equal(t, "ap:markdown", hookNode(t, msg2.Declaration.View, liveViewHook).Children[0].Component,
		"the SECOND, differently-subjected viewer must ALSO receive it — a view update carries no requester to filter on")
}
