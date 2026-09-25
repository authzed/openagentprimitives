package agentui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/browserstart"
	"github.com/authzed/openagentprimitives/pkg/web/webui/chat"
)

// This file is package agentui (internal), not agentui_test, so it can call
// startHandler and ViewFor directly in the SAME test — the spanning test
// below (J4) starts at the HTTP layer and ends at the real page, which no
// external-package test can do without a full webui.Server.

const (
	startNS       = "demo-ns"
	startClass    = "demo-agent"
	startUIName   = "demo-ui"
	startEnded    = "demo-ended"
	startAttached = "demo-attached"
	startOrigin   = "https://trusted.start.example"
)

// demoSubject is the raw webui-form canonical subject ("user:" + base64url
// email) every case in this file authenticates as — mirrors
// browsersession_test.go's subjectFor helper (a different package; not
// reusable from here).
var demoSubject = "user:" + base64.RawURLEncoding.EncodeToString([]byte("demo@example.com"))

// canonicalFor derives the SpiceDB canonical id demoSubject resolves to,
// through the SAME DeriveIdentity + Canonical() round trip browsersession.Create
// itself uses — so a fixture's seeded touched record and Create's own write
// are guaranteed to compare equal, never two independently-typed "canonical
// enough" values.
func canonicalFor(t *testing.T, subject string) identity.CanonicalUserID {
	t.Helper()
	_, principal, err := browsersession.DeriveIdentity(identity.Subject(subject))
	require.NoError(t, err)
	canonical, err := principal.Canonical()
	require.NoError(t, err)
	return canonical
}

// --- fakes -----------------------------------------------------------------

// startTouchCall records one startFakeGranter.TouchStartedBy invocation.
type startTouchCall struct {
	ns, name  string
	canonical identity.CanonicalUserID
}

// startFakeGranter is an authz.Granter recording every TouchStartedBy call —
// the SAME Granter both startFakeDeps.CheckInteract and
// startFakeDeps.StartBrowserSession's browsersession.Create read/write
// through, so a viewer's standing on a session this suite creates is
// established the same way the real SpiceDB schema establishes it
// (started_by is in interact), not by a second, independent stub.
type startFakeGranter struct {
	mu      sync.Mutex
	touched []startTouchCall
	err     error
}

func (g *startFakeGranter) TouchStartedBy(_ context.Context, ns, name string, canonicalID identity.CanonicalUserID) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.touched = append(g.touched, startTouchCall{ns: ns, name: name, canonical: canonicalID})
	return g.err
}
func (g *startFakeGranter) TouchOwner(context.Context, string, string, string) error { return nil }
func (g *startFakeGranter) TouchInteractParticipant(context.Context, string, string, string) error {
	return nil
}
func (g *startFakeGranter) TouchInteractParticipantUser(context.Context, string, string, identity.CanonicalUserID) error {
	return nil
}
func (g *startFakeGranter) TouchDeniedUser(context.Context, string, string, identity.CanonicalUserID) error {
	return nil
}
func (g *startFakeGranter) TouchInteractor(context.Context, string, string, string) error { return nil }

func (g *startFakeGranter) has(ns, name string, canonical identity.CanonicalUserID) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.touched {
		if c.ns == ns && c.name == name && c.canonical == canonical {
			return true
		}
	}
	return false
}

var _ authz.Granter = (*startFakeGranter)(nil)

// startBrowserSessionDeps adapts startFakeDeps to browsersession.Deps.
// Authz is declared as the INTERFACE field per browsersession.Deps' own doc
// comment — a real construction site (internal/cmd/webd) must never assign a
// typed-nil *spicedb.Client into it, and this fake mirrors that shape rather
// than a struct that would hide the distinction.
type startBrowserSessionDeps struct {
	k8s     client.Client
	granter authz.Granter
	logger  logr.Logger
}

func (d startBrowserSessionDeps) K8s() client.Client   { return d.k8s }
func (d startBrowserSessionDeps) Authz() authz.Granter { return d.granter }
func (d startBrowserSessionDeps) Logger() logr.Logger  { return d.logger }

// StartChecker: nil. Every fixture in this file is ungated (no
// spec.authz.session.allowedStarters), so browsersession.Create's gate is
// skipped before a nil checker would ever be dereferenced.
func (d startBrowserSessionDeps) StartChecker() browsersession.StartChecker { return nil }

// startFakeLive is a recording browserstart.LiveSessions. It records the
// ARGUMENTS of every call, not merely a count: this route's whole cap story is
// that the slot is claimed for the session that is about to be created, under
// the viewer's own subject, BEFORE any cluster object exists — a fake that
// discarded its arguments would make all three claims unfalsifiable.
type startFakeLive struct {
	mu       sync.Mutex
	reserved []liveCall
	adopted  []liveCall
	released int
	// createsAtReserve snapshots how many AgentSessions existed when Reserve
	// was called, which is what proves the reservation came FIRST.
	createsAtReserve int
	countSessions    func() int

	reserveErr error
	adoptErr   error
}

// liveCall is one recorded Reserve/Adopt invocation.
type liveCall struct{ ns, name, subject string }

func (l *startFakeLive) Reserve(ns, name, subject string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.countSessions != nil {
		l.createsAtReserve = l.countSessions()
	}
	if l.reserveErr != nil {
		return nil, l.reserveErr
	}
	l.reserved = append(l.reserved, liveCall{ns, name, subject})
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.released++
	}, nil
}

func (l *startFakeLive) Adopt(_ context.Context, ns, name, subject string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.adoptErr != nil {
		return l.adoptErr
	}
	l.adopted = append(l.adopted, liveCall{ns, name, subject})
	return nil
}

func (l *startFakeLive) calls() ([]liveCall, []liveCall, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]liveCall(nil), l.reserved...), append([]liveCall(nil), l.adopted...), l.released
}

var _ browserstart.LiveSessions = (*startFakeLive)(nil)

// startFakeDeps implements agentui.Deps for start.go's own test suite AND
// (via ViewFor) the second leg of the spanning test below — the same value
// serves both the POST and the resolution of the session it created, which is
// the point.
type startFakeDeps struct {
	k8s     client.Client
	granter *startFakeGranter
	origin  string
	logger  logr.Logger

	// interactErr, when set, makes CheckInteract fail-closed regardless of
	// the Granter's own record.
	interactErr error
	// noStart, when true, makes StartBrowserSession() return nil — the
	// routing test's "collaborator absent" case.
	noStart bool
	// live is this process's live-session table. A CONCRETE pointer, converted
	// to the interface only when non-nil (LiveSessions below), so a nil one is
	// a genuine nil interface and not a typed-nil wrapped in a non-nil one.
	live *startFakeLive
	// startNamespaces overrides what this fixture's webd may create in. nil
	// means "the fixture's own namespace" (see StartableNamespaces); an
	// explicit empty slice is how a row asks for the unreachable case.
	startNamespaces []string
}

func (d *startFakeDeps) CheckInteract(_ context.Context, ns, name, subject string) (bool, error) {
	if d.interactErr != nil {
		return false, d.interactErr
	}
	_, principal, err := browsersession.DeriveIdentity(identity.Subject(subject))
	if err != nil {
		return false, nil //nolint:nilerr // an undecodable subject is a denial, not a fail-closed error
	}
	canonical, err := principal.Canonical()
	if err != nil {
		return false, nil //nolint:nilerr // same as above
	}
	return d.granter.has(ns, name, canonical), nil
}
func (d *startFakeDeps) K8s() client.Client { return d.k8s }
func (d *startFakeDeps) Logger() logr.Logger {
	if d.logger.GetSink() != nil {
		return d.logger
	}
	return logr.Discard()
}
func (d *startFakeDeps) TrustedOrigin() string                                   { return d.origin }
func (d *startFakeDeps) NATSRequest() channelevents.RequestFunc                  { return nil }
func (d *startFakeDeps) Memory() memory.Memory                                   { return &fakeUIActionMemory{} }
func (d *startFakeDeps) Artifacts() *artifacts.Service                           { return nil }
func (d *startFakeDeps) ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc { return nil }
func (d *startFakeDeps) NATS() *nats.Conn                                        { return nil }

func (d *startFakeDeps) StartBrowserSession() browsersession.StartFunc {
	if d.noStart {
		return nil
	}
	bd := startBrowserSessionDeps{k8s: d.k8s, granter: d.granter, logger: d.Logger()}
	return func(ctx context.Context, p browsersession.Params) (browsersession.Created, error) {
		return browsersession.Create(ctx, bd, p)
	}
}

// LiveSessions returns a genuine nil INTERFACE when this fixture hosts no
// table — never a typed-nil *startFakeLive promoted into a non-nil interface,
// whose Reserve call would then panic instead of taking the no-table branch
// (AGENTS.md's typed-nil rule).
func (d *startFakeDeps) LiveSessions() browserstart.LiveSessions {
	if d.live == nil {
		return nil
	}
	return d.live
}

// StartableNamespaces defaults to the fixture's own namespace so the ordinary
// rows exercise a reachable start; the unreachable row sets its own value.
func (d *startFakeDeps) StartableNamespaces() []string {
	if d.startNamespaces == nil {
		return []string{startNS}
	}
	return d.startNamespaces
}

// WorkshopNamespacesFor: no test in this file exercises the dynamic arm —
// the static namespace above already reaches every row these tests build.
func (d *startFakeDeps) WorkshopNamespacesFor(context.Context, string) ([]string, error) {
	return nil, nil
}

var _ Deps = (*startFakeDeps)(nil)

// --- fixtures ----------------------------------------------------------

func startFixtureEndedSession() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: startEnded, Namespace: startNS},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: startClass},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded},
	}
}

func startFixtureAttachedSession() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: startAttached, Namespace: startNS},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: startClass},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
}

// startFixtureAgentClass carries an explicit UID: the fake client does not
// synthesize one for objects seeded via WithObjects, and browsersession.Create
// owner-refs the Channel it creates to this AgentClass's UID.
func startFixtureAgentClass() *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: startClass, Namespace: startNS, UID: types.UID(startClass + "-fixed-uid")},
		Spec:       spiceboxv1alpha1.AgentClassSpec{AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: startUIName}},
	}
	ac.Status.Conditions = []metav1.Condition{{Type: spiceboxv1alpha1.AgentClassConditionValid, Status: metav1.ConditionTrue, Reason: "Valid"}}
	return ac
}

// startFixtureAgentUI must carry Valid=True and a renderable slot, or the
// second leg of the spanning test fails at resolveAgentUIDoors' own Valid
// gate before it ever reaches the branch under test here — see this file's
// own fixture-warning note in the task brief.
func startFixtureAgentUI() *spiceboxv1alpha1.AgentUI {
	return &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: startUIName, Namespace: startNS},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "root", Default: &apiextensionsv1.JSON{Raw: []byte(`{"component":"ap:stack"}`)}},
			},
		},
		Status: spiceboxv1alpha1.AgentUIStatus{
			Conditions: []metav1.Condition{{Type: spiceboxv1alpha1.AgentUIConditionValid, Status: metav1.ConditionTrue, Reason: "Test"}},
		},
	}
}

// newStartFakeK8s builds a scheme-aware fake client that assigns a UID on
// every Create — mirrors browsersession_test.go's newFakeK8sClient, needed
// here for the same reason: browsersession.Create's owner-ref chain
// (Channel -> Secret, Channel -> AgentSession) is meaningless against two
// empty UIDs.
func newStartFakeK8s(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return newStartFakeK8sIntercepted(t, interceptor.Funcs{}, objs...)
}

// newStartFakeK8sIntercepted is newStartFakeK8s with a caller-supplied
// interceptor merged in. The UID-assigning Create is kept whatever the caller
// installs (unless the caller overrides Create itself), so a test that only
// wants to fail one Get does not silently lose the owner-ref chain.
func newStartFakeK8sIntercepted(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	if funcs.Create == nil {
		funcs.Create = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetUID() == "" {
				obj.SetUID(types.UID(obj.GetName() + "-generated-uid"))
			}
			return c.Create(ctx, obj, opts...)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithInterceptorFuncs(funcs).Build()
}

// newStartDeps builds a startFakeDeps whose K8s carries an ENDED session
// (startEnded), a live one (startAttached), their shared (Valid=True,
// spec.agentUI-set) AgentClass, and a Valid=True AgentUI — every door
// resolveAgentUIDoors walks — plus a recording Granter seeded so demoSubject
// may interact with BOTH pre-existing sessions (as if each had been started
// under them originally). CheckInteract is backed by that SAME Granter, so
// Create's own TouchStartedBy write for a NEW session is what a later
// CheckInteract call actually observes — never a second, independently
// stubbed mechanism.
func newStartDeps(t *testing.T) *startFakeDeps {
	t.Helper()
	granter := &startFakeGranter{}
	canonical := canonicalFor(t, demoSubject)
	granter.touched = append(granter.touched,
		startTouchCall{ns: startNS, name: startEnded, canonical: canonical},
		startTouchCall{ns: startNS, name: startAttached, canonical: canonical},
	)
	k8s := newStartFakeK8s(t, startFixtureAgentClass(), startFixtureAgentUI(),
		startFixtureEndedSession(), startFixtureAttachedSession())
	return &startFakeDeps{k8s: k8s, granter: granter, origin: startOrigin, logger: logr.Discard()}
}

// --- HTTP helpers ------------------------------------------------------

func doPostStart(t *testing.T, d Deps, subject, origin, ns, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/agent-ui/"+ns+"/"+name+"/start", strings.NewReader(body))
	req.SetPathValue("ns", ns)
	req.SetPathValue("name", name)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if subject != "" {
		req = req.WithContext(webui.WithSubjectForTest(req.Context(), subject))
	}
	rec := httptest.NewRecorder()
	startHandler(d).ServeHTTP(rec, req)
	return rec
}

// postStart serves a request against d under the happy-path subject/origin,
// varying only the URL's {name} and the body — the join tests below.
func postStart(t *testing.T, d Deps, ns, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doPostStart(t, d, demoSubject, startOrigin, ns, name, body)
}

// ctxWithSubject builds a context carrying an authenticated subject, the way
// webui's cookie-auth middleware does for a real request.
func ctxWithSubject(subject string) context.Context {
	return webui.WithSubjectForTest(context.Background(), subject)
}

// parseStartHref splits a same-origin session-shell href
// ("/sessions?session=<ns>/<name>") into its two coordinates by PARSING the
// URL, exactly as a browser following it would resolve the query.
// TestStartThenServeTheNewSession drives the page-serve leg from THESE parsed
// values rather than the startResponse.Name the handler happened to encode —
// the point is to prove the ADDRESS a browser actually following href would
// land on, not merely that a correctly-named session object exists somewhere.
func parseStartHref(t *testing.T, href string) (ns, name string) {
	t.Helper()
	u, err := url.Parse(href)
	require.NoError(t, err, "href must parse as a URL, got %q", href)
	require.Equal(t, "/sessions", u.Path, "href must address the session shell, got %q", href)
	sel := u.Query().Get("session")
	parts := strings.SplitN(sel, "/", 2)
	require.Len(t, parts, 2, "href's session selection must carry both {ns} and {name}, got %q", sel)
	require.NotEmpty(t, parts[0])
	require.NotEmpty(t, parts[1])
	return parts[0], parts[1]
}

// assertNoNewSessionObjects fails the test if any Channel or Secret exists,
// or if the AgentSession count has grown past baseline — the "nothing new
// was written" half of a refused request, which matters as much as the
// returned status: a refusal that already created a Channel is the orphan
// case a status-only assertion cannot see.
func assertNoNewSessionObjects(t *testing.T, k8s client.Client, baselineSessions int) {
	t.Helper()
	var chans spiceboxv1alpha1.ChannelList
	require.NoError(t, k8s.List(context.Background(), &chans, client.InNamespace(startNS)))
	assert.Empty(t, chans.Items, "no Channel may exist after a refused start request")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, k8s.List(context.Background(), &sessions, client.InNamespace(startNS)))
	assert.Len(t, sessions.Items, baselineSessions, "no NEW AgentSession may exist after a refused start request")
}

// --- J4: the spanning test ----------------------------------------------

// TestStartThenServeTheNewSession is J4: the address the start route returns
// must be one the agent-UI page immediately serves. Asserting only that an
// AgentSession object appeared would prove creation and nothing about
// serveability — a session whose started_by was never written, or whose name
// the handler echoed from the URL, both create an object and both hand the
// browser a page it cannot open.
//
// The response is decoded into a raw map, NOT the startResponse struct the
// handler itself encoded with — unmarshalling into the same type the encoder
// used would move encoder and decoder together and pin no JSON key at all.
// Decoding into map[string]any and asserting the raw keys is what actually
// pins the wire contract the browser's parseStartResponse reads by name. The second leg
// is then driven from the PARSED href, not from a typed Name field, so the
// test proves the address a browser following href would actually land on.
func TestStartThenServeTheNewSession(t *testing.T) {
	d := newStartDeps(t) // fake client seeded with an ENDED session, its
	// AgentClass (Valid=True, spec.agentUI set) and a Valid=True AgentUI; a
	// recording fake Granter whose TouchStartedBy feeds the same CheckInteract
	// the page will call.

	rec := postStart(t, d, startNS, startEnded, `{"prompt":"pick up where we left off"}`)
	require.Equal(t, http.StatusOK, rec.Code, "start must succeed for an ended session the viewer may interact with; body: %s", rec.Body.String())

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.Contains(t, raw, "ns", "the wire contract must carry \"ns\"")
	require.Contains(t, raw, "name", "the wire contract must carry \"name\"")
	require.Contains(t, raw, "href", "the wire contract must carry \"href\" -- the field the browser actually follows")

	gotNS, _ := raw["ns"].(string)
	gotName, _ := raw["name"].(string)
	gotHref, _ := raw["href"].(string)
	require.NotEqual(t, startEnded, gotName, "the response must name the NEW session, not the ended one")
	assert.Equal(t, startNS, gotNS)
	require.Equal(t, SessionShellHref(gotNS, gotName), gotHref,
		"href must address the session that was created, not the one the URL named")

	hrefNS, hrefName := parseStartHref(t, gotHref)
	props, _, avail, pe := ViewFor(ctxWithSubject(demoSubject), d, hrefNS, hrefName)
	require.Nil(t, pe, "the agent-defined view must resolve for the session the start route just created")
	require.Equal(t, UIAvailable, avail)
	// "Resolved, not 410" is the load-bearing half: an Ended or otherwise
	// terminal session would have come back as a *webui.PageError above instead
	// of reaching this line at all.
	assert.Equal(t, gotName, props.Name, "the resolved view must name the newly created session")
}

// TestResolvingAView_NeverStartsASession_EvenWhenTheCollaboratorIsWired pins
// the design constraint the start route exists to respect (D4): starting a
// session is an explicit POST action, NEVER a side effect of resolving a view.
// newStartDeps' StartBrowserSession is a REAL, working func (backed by
// browsersession.Create against the same fake K8s+Granter the POST tests use)
// precisely so this test cannot pass by accident because the collaborator
// happened to be nil — if the resolution ladder ever called it on the Ended
// branch, this fixture would actually create a session and both assertions
// below would catch it.
func TestResolvingAView_NeverStartsASession_EvenWhenTheCollaboratorIsWired(t *testing.T) {
	d := newStartDeps(t)
	require.NotNil(t, d.StartBrowserSession(), "fixture precondition: the collaborator must be wired for this test to mean anything")

	_, _, _, pe := ViewFor(ctxWithSubject(demoSubject), d, startNS, startEnded)
	require.NotNil(t, pe, "an Ended session must not resolve to a view")
	assert.Equal(t, http.StatusGone, pe.Status, "an ended session's view answers 410, never silently starts one")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, d.k8s.List(context.Background(), &sessions, client.InNamespace(startNS)))
	assert.Len(t, sessions.Items, 2, "resolving a view must not create a new AgentSession as a side effect (baseline: startEnded + startAttached)")
}

// --- gate order + body validation ---------------------------------------

// TestStartHandler_GateAndBody table-drives the gate order (authentication,
// CSRF origin pin, CheckInteract fail-closed-on-error, the Ended-only rung)
// and the body validation (malformed JSON, absent prompt, whitespace-only
// prompt). Every row asserts BOTH the status and that no new object exists.
func TestStartHandler_GateAndBody(t *testing.T) {
	cases := []struct {
		name         string
		session      string // URL {name}; empty means startEnded
		subject      string
		origin       string
		interactErr  error
		denyInteract bool
		body         string
		wantStatus   int
	}{
		{name: "unauthenticated: rejected before any read",
			origin: startOrigin, body: `{"prompt":"hi"}`, wantStatus: http.StatusUnauthorized},
		{name: "Origin header not the trusted origin: rejected",
			subject: demoSubject, origin: "https://evil.example", body: `{"prompt":"hi"}`, wantStatus: http.StatusForbidden},
		{name: "CheckInteract denies: rejected",
			subject: demoSubject, origin: startOrigin, denyInteract: true, body: `{"prompt":"hi"}`, wantStatus: http.StatusForbidden},
		{name: "CheckInteract errors: fail-closed, never a denial",
			subject: demoSubject, origin: startOrigin, interactErr: errors.New("spicedb down"),
			body: `{"prompt":"hi"}`, wantStatus: http.StatusServiceUnavailable},
		{name: "named session is Attached (still live): refused",
			session: startAttached, subject: demoSubject, origin: startOrigin, body: `{"prompt":"hi"}`, wantStatus: http.StatusConflict},
		// Ordering row: a malformed body must NOT be read before CheckInteract
		// is consulted. If the decode moved above the gate, this row would
		// still incidentally read 403 for the WRONG reason unless it is paired
		// with a denial — see the handler's own doc comment and mutation 3.
		{name: "malformed body AND CheckInteract denies: still 403 (auth/CSRF/interact run before the body is read)",
			subject: demoSubject, origin: startOrigin, denyInteract: true, body: `{not json`, wantStatus: http.StatusForbidden},
		{name: "body is not JSON: 400",
			subject: demoSubject, origin: startOrigin, body: `{not json`, wantStatus: http.StatusBadRequest},
		{name: "prompt absent: 400",
			subject: demoSubject, origin: startOrigin, body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "prompt is whitespace only: 400",
			subject: demoSubject, origin: startOrigin, body: `{"prompt":"   "}`, wantStatus: http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			granter := &startFakeGranter{}
			if !tc.denyInteract {
				canonical := canonicalFor(t, demoSubject)
				granter.touched = append(granter.touched,
					startTouchCall{ns: startNS, name: startEnded, canonical: canonical},
					startTouchCall{ns: startNS, name: startAttached, canonical: canonical},
				)
			}
			k8s := newStartFakeK8s(t, startFixtureAgentClass(), startFixtureAgentUI(),
				startFixtureEndedSession(), startFixtureAttachedSession())
			d := &startFakeDeps{k8s: k8s, granter: granter, origin: startOrigin, logger: logr.Discard(), interactErr: tc.interactErr}

			session := tc.session
			if session == "" {
				session = startEnded
			}
			rec := doPostStart(t, d, tc.subject, tc.origin, startNS, session, tc.body)
			require.Equal(t, tc.wantStatus, rec.Code, "case %q; body: %s", tc.name, rec.Body.String())

			assertNoNewSessionObjects(t, k8s, 2) // baseline: startEnded + startAttached, pre-seeded
		})
	}
}

// TestStartHandler_ErrUnknownAgentClass_MapsTo409 exercises the error-mapping
// rule that isn't reachable through the gate/body table: browsersession.Create
// itself failing with ErrUnknownAgentClass (the class named on the session
// became invalid between CheckInteract and Create) must answer 409 with copy
// that names neither a CRD kind nor a condition, never a 500.
func TestStartHandler_ErrUnknownAgentClass_MapsTo409(t *testing.T) {
	granter := &startFakeGranter{}
	canonical := canonicalFor(t, demoSubject)
	granter.touched = append(granter.touched, startTouchCall{ns: startNS, name: startEnded, canonical: canonical})

	// The AgentClass is Valid=False: browsersession.Create's own
	// AgentClassReady gate refuses it, wrapping ErrUnknownAgentClass.
	notReady := startFixtureAgentClass()
	notReady.Status.Conditions = []metav1.Condition{{Type: spiceboxv1alpha1.AgentClassConditionValid, Status: metav1.ConditionFalse, Reason: "NotReady"}}
	k8s := newStartFakeK8s(t, notReady, startFixtureAgentUI(), startFixtureEndedSession())
	d := &startFakeDeps{k8s: k8s, granter: granter, origin: startOrigin, logger: logr.Discard()}

	rec := postStart(t, d, startNS, startEnded, `{"prompt":"hi"}`)
	require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())

	var got struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	for _, forbidden := range []string{"AgentClass", "Valid", "NotReady", "spicedb"} {
		assert.NotContains(t, got.Error, forbidden, "the response must not leak internal vocabulary")
	}
}

// TestStartHandler_GatedClassAndUnknownClassShareTheirCopy is this route's
// half of the no-enumeration property the dashboard route already holds
// (pkg/web/webui/sessions' TestStartGatedClassAndUnknownClassShareTheirCopy).
//
// browsersession.Create has two refusal branches this route can reach, and a
// caller must not be able to tell them apart: "this class carries a start
// allowlist you are not on" and "this class is unknown or not ready". Distinct
// answers would turn the endpoint into a probe for which agents declare
// allowlists — and, before this arm existed, ErrNotAnAllowedStarter fell into
// writeStartFailure's default: a 500 with generic copy and an ERROR log, which
// is both a different answer AND a false claim that something failed.
//
// Both halves clear the route's own authorization gate (the viewer interacts
// with the ended session named in the URL) and are refused INSIDE Create, so
// this compares Create's two branches, not that gate against Create.
func TestStartHandler_GatedClassAndUnknownClassShareTheirCopy(t *testing.T) {
	// post drives one half and returns the response plus everything logged.
	post := func(t *testing.T, ac *spiceboxv1alpha1.AgentClass) (*httptest.ResponseRecorder, string) {
		t.Helper()
		granter := &startFakeGranter{}
		granter.touched = append(granter.touched,
			startTouchCall{ns: startNS, name: startEnded, canonical: canonicalFor(t, demoSubject)})
		k8s := newStartFakeK8s(t, ac, startFixtureAgentUI(), startFixtureEndedSession())

		var mu sync.Mutex
		var logged []string
		d := &startFakeDeps{k8s: k8s, granter: granter, origin: startOrigin,
			logger: funcr.New(func(_, args string) {
				mu.Lock()
				defer mu.Unlock()
				logged = append(logged, args)
			}, funcr.Options{})}

		rec := postStart(t, d, startNS, startEnded, `{"prompt":"hi"}`)
		assertNoNewSessionObjects(t, k8s, 1) // baseline: startEnded, pre-seeded
		mu.Lock()
		defer mu.Unlock()
		return rec, strings.Join(logged, "\n")
	}

	// Refused by Create's start gate. The class declares an allowlist and
	// startBrowserSessionDeps.StartChecker() is nil, so the gate fails closed
	// with ErrNotAnAllowedStarter — the real production branch, not a stub.
	gated := startFixtureAgentClass()
	gated.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Session: &spiceboxv1alpha1.SessionAuthz{
		AllowedStarters: []string{"user:someone-else"},
	}}
	byGate, gateLog := post(t, gated)

	// Refused by Create's unknown-class branch, exactly as
	// TestStartHandler_ErrUnknownAgentClass_MapsTo409 sets it up.
	notReady := startFixtureAgentClass()
	notReady.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.AgentClassConditionValid, Status: metav1.ConditionFalse, Reason: "NotReady"}}
	byUnknown, unknownLog := post(t, notReady)

	// Fixture preconditions: each half reached the branch it claims to. Without
	// these, two 500s would satisfy every assertion below.
	require.Contains(t, gateLog, "viewer is not an allowed starter of this class",
		"fixture precondition: this half must be refused by the start gate")
	require.Contains(t, unknownLog, "agent class is unknown or not yet valid",
		"fixture precondition: this half must be refused by Create's unknown-class branch")
	assert.NotContains(t, gateLog, "could not start a browser session",
		"a deliberate refusal must not be logged as a failure")

	require.Equal(t, http.StatusConflict, byGate.Code, "body: %s", byGate.Body.String())
	assert.Equal(t, byUnknown.Code, byGate.Code, "status must be identical")
	assert.Equal(t, byUnknown.Body.String(), byGate.Body.String(),
		"the full response body must be byte-identical: a gated class the viewer is not listed on and a class "+
			"that is not ready must be indistinguishable to the caller")

	var got struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(byGate.Body.Bytes(), &got))
	assert.Equal(t, startAgentNotReadyMessage, got.Error)
	for _, forbidden := range []string{"allowedStarters", "starter", "AgentClass", "start_session", "spicedb"} {
		assert.NotContains(t, got.Error, forbidden, "the response must not leak internal vocabulary")
	}
}

// TestStartHandler_TransientClassReadFailure_IsNot409 is the route-level half
// of browsersession's own verdict-vs-failed-read split.
//
// The class here EXISTS and is Valid; only the read fails. Answering 409 "this
// agent is not ready" would tell the viewer their agent is broken, and the
// accompanying Info line would tell an operator the same — two false
// statements about a control plane that merely did not answer, arriving
// exactly when it is already unhealthy.
//
// Three assertions, because each catches a different way to get this wrong:
// the STATUS (not the class-verdict 409), the COPY (not the agent-not-ready
// message), and the LOG (the real cause present, the false claim absent).
func TestStartHandler_TransientClassReadFailure_IsNot409(t *testing.T) {
	granter := &startFakeGranter{}
	canonical := canonicalFor(t, demoSubject)
	granter.touched = append(granter.touched, startTouchCall{ns: startNS, name: startEnded, canonical: canonical})

	// The AgentClass is present and Valid — only the READ of it fails.
	k8s := newStartFakeK8sIntercepted(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*spiceboxv1alpha1.AgentClass); ok {
				return errors.New("etcdserver: request timed out")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, startFixtureAgentClass(), startFixtureAgentUI(), startFixtureEndedSession())

	var mu sync.Mutex
	var logged []string
	d := &startFakeDeps{k8s: k8s, granter: granter, origin: startOrigin,
		logger: funcr.New(func(_, args string) {
			mu.Lock()
			defer mu.Unlock()
			logged = append(logged, args)
		}, funcr.Options{})}

	rec := postStart(t, d, startNS, startEnded, `{"prompt":"hi"}`)
	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"a failed read is not a verdict on the agent; body: %s", rec.Body.String())

	var got struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, startInternalErrorMessage, got.Error)
	assert.NotEqual(t, startAgentNotReadyMessage, got.Error,
		"the viewer must not be told their agent is broken when the control plane did not answer")

	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(logged, "\n")
	assert.Contains(t, all, "etcdserver", "the real cause must reach the log")
	assert.NotContains(t, all, "not yet valid",
		"the log must not assert a verdict on the class — an operator reading it would debug the wrong thing")
}

// --- routing: gated on the collaborator's presence -----------------------

// TestRoutes_StartRoute_GatedOnCollaborator is J2-shaped: the start route
// must be mounted exactly when StartBrowserSession() is non-nil, and its
// absence must be LOGGED, not silently dropped — the same silent-vs-logged
// distinction the plugin's own deps-cast failure (agentui.go) relies on.
func TestRoutes_StartRoute_GatedOnCollaborator(t *testing.T) {
	t.Run("StartBrowserSession present: route is mounted", func(t *testing.T) {
		d := newStartDeps(t)
		routes := New().Routes(d)

		var found *webui.Route
		for i := range routes {
			if routes[i].Pattern == "/agent-ui/{ns}/{name}/start" {
				found = &routes[i]
			}
		}
		require.NotNil(t, found, "the start route must be mounted when StartBrowserSession is non-nil")
		assert.Equal(t, webui.OriginTrusted, found.Origin)
		assert.Equal(t, []string{http.MethodPost}, found.Methods)
		assert.Equal(t, webui.AuthAuthenticated, found.Auth)
		assert.NotNil(t, found.Handler)
		assert.Nil(t, found.Page, "start returns JSON, not an HTML document")
	})

	t.Run("StartBrowserSession nil: route is omitted AND the omission is logged", func(t *testing.T) {
		var mu sync.Mutex
		var logged []string
		d := newStartDeps(t)
		d.noStart = true
		d.logger = funcr.New(func(_, args string) {
			mu.Lock()
			defer mu.Unlock()
			logged = append(logged, args)
		}, funcr.Options{})

		routes := New().Routes(d)
		for _, r := range routes {
			assert.NotEqual(t, "/agent-ui/{ns}/{name}/start", r.Pattern,
				"the start route must not be mounted when StartBrowserSession is nil")
		}

		mu.Lock()
		defer mu.Unlock()
		require.NotEmpty(t, logged, "a nil StartBrowserSession must be logged, not silently omitted")
		assert.Contains(t, strings.Join(logged, "\n"), "start",
			"the log line must identify what was omitted")
	})
}

// TestStartHandler_ClassComesFromTheServersCopyOfTheSession pins what this
// route takes as authority to start.
//
// The gate is interact on the ONE session the URL names, and the class is read
// from the server's own copy of it — which is what makes that gate an instance
// of the rule the dashboard route derives (you may start a session of class C
// in namespace N when you may interact with a session of class C in N), rather
// than a weaker relative of it. A class the BROWSER named would break exactly
// that correspondence: the viewer's standing would be on one agent and the
// session would be started for another.
//
// The fixture seeds a second, fully valid AgentClass so honoring a body-named
// class would SUCCEED with the wrong one rather than fail — a fixture where the
// wrong answer errors anyway would make this assertion unfalsifiable.
func TestStartHandler_ClassComesFromTheServersCopyOfTheSession(t *testing.T) {
	d := newStartDeps(t)
	other := startFixtureAgentClass()
	other.Name = "other-agent"
	other.UID = types.UID("other-agent-fixed-uid")
	require.NoError(t, d.k8s.Create(context.Background(), other))

	rec := postStart(t, d, startNS, startEnded, `{"prompt":"again","agentClass":"other-agent","ns":"other-ns"}`)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var got startResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, startNS, got.Ns, "the namespace is the URL's, never the body's")

	var created spiceboxv1alpha1.AgentSession
	require.NoError(t, d.k8s.Get(context.Background(),
		client.ObjectKey{Namespace: startNS, Name: got.Name}, &created))
	assert.Equal(t, startClass, created.Spec.Class,
		"the new session's class must come from the server's copy of the named session, never from the request body")
}

// --- the capacity limits this route used to enforce nowhere ---------------

// countSessionsIn returns how many AgentSessions currently exist in startNS.
func countSessionsIn(t *testing.T, k8s client.Client) func() int {
	t.Helper()
	return func() int {
		var list spiceboxv1alpha1.AgentSessionList
		if err := k8s.List(context.Background(), &list, client.InNamespace(startNS)); err != nil {
			return -1
		}
		return len(list.Items)
	}
}

// newStartDepsWithLive is newStartDeps plus a recording live-session table,
// wired so the table can report how many AgentSessions existed at the moment
// Reserve was called.
func newStartDepsWithLive(t *testing.T, live *startFakeLive) *startFakeDeps {
	t.Helper()
	d := newStartDeps(t)
	live.countSessions = countSessionsIn(t, d.k8s)
	d.live = live
	return d
}

// startBaselineSessions is how many AgentSessions newStartDeps seeds: the
// ended one and the attached one.
const startBaselineSessions = 2

// TestStartHandler_ReservesBeforeCreating pins the cap this route enforced
// NOWHERE before: it called neither Reserve nor Adopt, so a viewer pressing
// the ended-session control repeatedly was bounded by neither the per-subject
// limit nor the process ceiling.
//
// The reservation must also come FIRST. A capacity refusal that has already
// written a Channel and an AgentSession leaves an orphan nobody will ever
// open, which is why the fake records how many sessions existed at the moment
// Reserve was called rather than merely that Reserve happened.
func TestStartHandler_ReservesBeforeCreating(t *testing.T) {
	live := &startFakeLive{}
	d := newStartDepsWithLive(t, live)

	rec := postStart(t, d, startNS, startEnded, `{"prompt":"again please"}`)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var got startResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	reserved, adopted, _ := live.calls()
	require.Len(t, reserved, 1, "the start must claim exactly one live-session slot")
	assert.Equal(t, startNS, reserved[0].ns)
	assert.Equal(t, demoSubject, reserved[0].subject,
		"the slot is claimed under the VIEWER's subject — a service identity would charge the wrong person's limit")
	assert.Equal(t, got.Name, reserved[0].name,
		"the slot must be keyed by the name that was actually created, or the reservation guards nothing")
	assert.Equal(t, startBaselineSessions, live.createsAtReserve,
		"Reserve must run BEFORE the session is created, or a refusal leaves an orphaned Channel and AgentSession behind")

	require.Len(t, adopted, 1, "the created session must be adopted, or its replies have no in-process sink")
	assert.Equal(t, liveCall{ns: startNS, name: got.Name, subject: demoSubject}, adopted[0])
}

// TestStartHandler_CapacityRefusals covers every way the reservation can
// refuse or fault. The two limits are different problems with different
// remedies, so they must not collapse into one status or one message; an
// unrecognized fault is neither and must not be reported as either.
func TestStartHandler_CapacityRefusals(t *testing.T) {
	cases := []struct {
		name        string
		reserveErr  error
		wantStatus  int
		wantMessage string
		wantLogged  string
		notLogged   []string
	}{
		{
			name:        "the viewer is at their own limit: 429, nothing created",
			reserveErr:  chat.ErrTooManySessionsForSubject,
			wantStatus:  http.StatusTooManyRequests,
			wantMessage: startTooManyForYouMessage,
			wantLogged:  "their own live-session limit",
			notLogged:   []string{"capacity ceiling"},
		},
		{
			name:        "the process is at its ceiling: 503, nothing created",
			reserveErr:  chat.ErrServerAtCapacity,
			wantStatus:  http.StatusServiceUnavailable,
			wantMessage: startAtCapacityMessage,
			wantLogged:  "capacity ceiling",
			notLogged:   []string{"their own live-session limit"},
		},
		{
			name:        "an unrecognized reservation fault: 500, reported as neither limit",
			reserveErr:  errors.New("injected reservation fault"),
			wantStatus:  http.StatusInternalServerError,
			wantMessage: startInternalErrorMessage,
			wantLogged:  "injected reservation fault",
			notLogged:   []string{"their own live-session limit", "capacity ceiling"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var logged []string
			d := newStartDepsWithLive(t, &startFakeLive{reserveErr: tc.reserveErr})
			d.logger = funcr.New(func(_, args string) {
				mu.Lock()
				defer mu.Unlock()
				logged = append(logged, args)
			}, funcr.Options{})

			rec := postStart(t, d, startNS, startEnded, `{"prompt":"again"}`)
			require.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())

			var got struct {
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			assert.Equal(t, tc.wantMessage, got.Error)

			assertNoNewSessionObjects(t, d.k8s, startBaselineSessions)

			mu.Lock()
			defer mu.Unlock()
			all := strings.Join(logged, "\n")
			assert.Contains(t, all, tc.wantLogged, "the refusal's cause must be diagnosable from the log")
			for _, no := range tc.notLogged {
				assert.NotContains(t, all, no, "one limit must never be logged as the other")
			}
		})
	}
}

// TestStartHandler_NamespaceThisServerCannotCreateIn is the ended-session half
// of the same limit the dashboard picker marks: a replacement is created in the
// ENDED session's own namespace, and this webd may hold no create RBAC there.
// It must refuse with its own copy and reserve NOTHING — the refusal comes
// before the reservation, so a viewer bouncing off a control that can never
// work is not charged a live-session slot for ten minutes each time.
//
// The copy must not be the agent-not-ready one: the agent is fine, and telling
// the viewer otherwise sends them to wait for something that will not change.
func TestStartHandler_NamespaceThisServerCannotCreateIn(t *testing.T) {
	var mu sync.Mutex
	var logged []string
	live := &startFakeLive{}
	d := newStartDepsWithLive(t, live)
	d.startNamespaces = []string{} // empty means "nowhere", not "anywhere"
	d.logger = funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})

	rec := postStart(t, d, startNS, startEnded, `{"prompt":"again"}`)
	require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())

	var got struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, startNotStartableHereMessage, got.Error)
	assert.NotEqual(t, startAgentNotReadyMessage, got.Error,
		"this is a limit of the server, not a verdict on the agent")

	assertNoNewSessionObjects(t, d.k8s, startBaselineSessions)
	reserved, _, _ := live.calls()
	assert.Empty(t, reserved, "a refusal this early must not claim a live-session slot")

	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, strings.Join(logged, "\n"), "cannot create a session in that namespace",
		"the operator log must name the real cause")
}

// TestStartHandler_AdoptFailureStillAnswersTheSession pins the one collaborator
// failure that must NOT fail the request: the session exists, it is the
// viewer's, and it is addressable — the next open wires it. Deleting it because
// the in-process wiring failed is how a session vanishes out from under
// someone.
func TestStartHandler_AdoptFailureStillAnswersTheSession(t *testing.T) {
	var mu sync.Mutex
	var logged []string
	live := &startFakeLive{adoptErr: errors.New("injected wiring failure")}
	d := newStartDepsWithLive(t, live)
	d.logger = funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})

	rec := postStart(t, d, startNS, startEnded, `{"prompt":"again"}`)
	require.Equal(t, http.StatusOK, rec.Code, "an adopt failure must not fail the request; body: %s", rec.Body.String())

	var got startResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.NotEmpty(t, got.Href, "the viewer must still be given the address of the session that was created")

	// The reservation is KEPT. The three cluster objects the start just wrote
	// exist and are the viewer's, and this slot is the only thing bounding how
	// many of them one viewer can create through this route — releasing it here
	// removes the bound exactly when adopts start failing, which is when a
	// retrying tab produces them fastest. The registry's reaper reclaims a
	// reservation nothing ever published into, so this is not a permanent hold.
	_, _, released := live.calls()
	assert.Equal(t, 0, released,
		"a created session keeps its slot; only a start that created nothing gives one back")

	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, strings.Join(logged, "\n"), "injected wiring failure",
		"a session that could not be wired must be logged, not silently answered as fully live")
}

// TestStartHandler_CreateFailureReleasesTheSlot is the other side of that rule,
// and the one the sessions suite alone used to witness: a start that created
// NOTHING must give its reservation back, or a viewer is charged for sessions
// that do not exist and is locked out after sixteen failed attempts.
//
// It lives here as well as there because both routes now share one
// implementation, and a property with a single witness in a package that does
// not own it is how a shared helper loses a behavior quietly.
func TestStartHandler_CreateFailureReleasesTheSlot(t *testing.T) {
	live := &startFakeLive{}
	d := newStartDepsWithLive(t, live)

	// The class is Valid=False, so browsersession.Create refuses AFTER the slot
	// is reserved and BEFORE any object is written. Read-modify-write rather
	// than a fresh object: the fake client enforces resourceVersion, and
	// re-Updating a stale copy would fail on the conflict instead of setting up
	// the case.
	var notReady spiceboxv1alpha1.AgentClass
	require.NoError(t, d.k8s.Get(context.Background(),
		client.ObjectKey{Namespace: startNS, Name: startClass}, &notReady))
	notReady.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.AgentClassConditionValid, Status: metav1.ConditionFalse,
		Reason: "NotReady", LastTransitionTime: metav1.Now()}}
	require.NoError(t, d.k8s.Update(context.Background(), &notReady))

	rec := postStart(t, d, startNS, startEnded, `{"prompt":"again"}`)
	require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())

	reserved, adopted, released := live.calls()
	require.Len(t, reserved, 1, "the slot must have been claimed, or this proves nothing about releasing it")
	assert.Empty(t, adopted)
	assert.Equal(t, 1, released, "a start that created nothing must return its reservation")
	assertNoNewSessionObjects(t, d.k8s, startBaselineSessions)
}

// TestStartHandler_NoLiveSessionTable_StartsAndSaysSo covers the process that
// hosts no table at all: the session is still created and addressable, but
// NEITHER capacity limit is being applied to this request, which is worth a log
// line rather than a silent skip.
func TestStartHandler_NoLiveSessionTable_StartsAndSaysSo(t *testing.T) {
	var mu sync.Mutex
	var logged []string
	d := newStartDeps(t) // live stays nil
	d.logger = funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})

	rec := postStart(t, d, startNS, startEnded, `{"prompt":"again"}`)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, strings.Join(logged, "\n"), "hosts no live-session table",
		"running without the caps must be logged, not silently skipped")
}
