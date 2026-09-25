package sessions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/agentui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/chat"
)

// This file is package sessions (internal), not sessions_test, so J4 can call
// startHandler and shellPageBuild in the SAME test — the spanning test starts
// at the HTTP layer and ends at the real page build, which no external-package
// test can do without a full webui.Server.

const (
	startTestNS      = "demo-ns"
	startTestClass   = "demo-agent"
	startTestExists  = "demo-existing"
	startTestOrigin  = "https://trusted.sessions.example"
	startTestOtherNS = "other-ns"
)

// startTestSubjectDisplay/startTestSubject are a genuinely base64-encoded
// canonical-subject pair. The fake CheckInteract below runs the SAME
// DeriveIdentity + Canonical round trip browsersession.Create performs, so a
// subject that is not real base64 would make the two disagree about the
// canonical id and the whole join would pass or fail for the wrong reason.
const startTestSubjectDisplay = "starter@example.com"

var startTestSubject = "user:" + base64.RawURLEncoding.EncodeToString([]byte(startTestSubjectDisplay))

// --- fakes -----------------------------------------------------------------

// startTouchCall records one startFakeGranter.TouchStartedBy invocation.
type startTouchCall struct {
	ns, name  string
	canonical identity.CanonicalUserID
}

// startFakeGranter is an authz.Granter recording every TouchStartedBy call.
// It is the SAME value the fake CheckInteract reads and browsersession.Create
// writes through, so a viewer's standing on a session this suite creates is
// established exactly the way the real schema establishes it (started_by is in
// interact) rather than by a second, independent stub that would say yes
// regardless — which is what would make the spanning test's first leg
// unfalsifiable.
type startFakeGranter struct {
	mu      sync.Mutex
	touched []startTouchCall
	// skipTouch, when true, makes TouchStartedBy a silent no-op — the mutation
	// that proves the spanning test's first leg actually depends on the
	// started_by write rather than merely on an object appearing.
	skipTouch bool
}

func (g *startFakeGranter) TouchStartedBy(_ context.Context, ns, name string, canonicalID identity.CanonicalUserID) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.skipTouch {
		return nil
	}
	g.touched = append(g.touched, startTouchCall{ns: ns, name: name, canonical: canonicalID})
	return nil
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

// startBrowserDeps adapts startFixtureDeps to browsersession.Deps. Authz is
// declared as the INTERFACE per browsersession.Deps' own doc comment, so this
// fake mirrors the shape a real construction site must use rather than one
// that would hide the typed-nil distinction.
type startBrowserDeps struct {
	k8s     client.Client
	granter authz.Granter
	logger  logr.Logger
}

func (d startBrowserDeps) K8s() client.Client   { return d.k8s }
func (d startBrowserDeps) Authz() authz.Granter { return d.granter }
func (d startBrowserDeps) Logger() logr.Logger  { return d.logger }

// StartChecker: nil. Every fixture in this file is ungated (no
// spec.authz.session.allowedStarters), so browsersession.Create's gate is
// skipped before a nil checker would ever be dereferenced.
func (d startBrowserDeps) StartChecker() browsersession.StartChecker { return nil }

// lookupCall records one LookupInteractableSessions invocation, INCLUDING the
// consistency it was asked for. Recording the argument is the only thing that
// protects the gate/list distinction: a fake that discarded fullyConsistent
// would make "the gate is fully consistent" unfalsifiable, and the same fake
// would pass whether the handler asked for true, false, or nothing at all.
type lookupCall struct {
	canonical       identity.CanonicalUserID
	limit           uint32
	fullyConsistent bool
}

// interactCall records one CheckInteract invocation, INCLUDING the object it
// was asked about. Same reasoning as lookupCall: a fake that ignored ns/name
// could not tell "the gate was asked about the selected session" apart from
// "the gate was asked about something".
type interactCall struct{ ns, name, subject string }

// startFixtureDeps implements sessions.Deps AND agentui.Deps (the latter for
// viewFor's own cast), backed by a real fake Kubernetes client and the
// recording Granter above.
type startFixtureDeps struct {
	k8s     client.Client
	granter *startFakeGranter
	origin  string
	log     logr.Logger

	// lookupRefs is what LookupInteractableSessions answers with. It is
	// deliberately FIXED at fixture time and never updated by a create, which
	// is what makes it a MinimizeLatency-stale answer for the spanning test's
	// third leg: the newly created session is genuinely absent from it.
	lookupRefs []spicedb.SessionRef
	lookupErr  error
	// lookupTruncated makes the lookup report it returned only the first
	// maxListedSessions of a longer answer — the second way the gate's own list
	// can be incomplete without any error being returned.
	lookupTruncated bool

	// sessionGetErrors makes the AgentSession Get for "ns/name" fail with the
	// given (non-NotFound) error. Wired through errorInjectingClient
	// (list_test.go), which is the only way to make a Get on an object that DOES
	// exist return an arbitrary error.
	sessionGetErrors map[string]error

	// settingsGetErr makes the ClusterAgentSettings Get fail with a
	// non-NotFound error. The workshop cap reads the cluster tier to learn
	// whether this class makes a workshop at all, and an unread tier is an
	// indeterminate cap rather than a verdict either way.
	settingsGetErr error

	// workshopListErr makes the cluster-wide Workshop List fail. Counting is
	// the cap check itself, so a failed count is indeterminate — never "no
	// workshops", which would let a person at their ceiling straight through.
	workshopListErr error

	// classGetErrors makes the AgentClass Get for "ns/class" fail the same way.
	// Reached from TWO places, which is the point: buildSessionList's title
	// lookup (which tolerates it) and browsersession.Create's own read (which
	// must not read it as a verdict on the class).
	classGetErrors map[string]error

	// noStart makes StartBrowserSession() return nil.
	noStart bool
	// startNamespaces overrides what this fixture's webd can create in. nil
	// means "the fixture's own namespace" (see StartableNamespaces) so the
	// ordinary rows exercise a reachable start; an explicit empty slice is how
	// a row asks for the unreachable case.
	startNamespaces []string
	// live is the fake live-session table, or nil for a process that hosts none.
	live *fakeLiveSessions

	// startableClasses / startableClassesErr drive the gate's bootstrap arm:
	// the first names the classes this viewer may start via
	// agentclass#start_session, the second makes the lookup indeterminate
	// (which must produce 503, never a refusal).
	startableClasses    spicedb.StartableClasses
	startableClassesErr error

	mu                            sync.Mutex
	lookupCalls                   []lookupCall
	interactSeen                  []interactCall
	loggedLines                   []string
	gotClassLookupCalled          bool
	gotClassLookupFullyConsistent bool
}

func (d *startFixtureDeps) LookupInteractableSessions(_ context.Context, canonicalID identity.CanonicalUserID,
	limit uint32, fullyConsistent bool,
) (spicedb.InteractableSessions, error) {
	d.mu.Lock()
	d.lookupCalls = append(d.lookupCalls, lookupCall{canonical: canonicalID, limit: limit, fullyConsistent: fullyConsistent})
	d.mu.Unlock()
	if d.lookupErr != nil {
		return spicedb.InteractableSessions{}, d.lookupErr
	}
	return spicedb.InteractableSessions{
		Refs:      append([]spicedb.SessionRef(nil), d.lookupRefs...),
		Truncated: d.lookupTruncated,
	}, nil
}

// CheckInteract answers from the SAME recording Granter browsersession.Create
// writes started_by into — true iff a started_by was recorded for that
// (ns, name, canonical), which is what the real schema's
// `interact = owner + started_by + participant - denied` reduces to for a
// session's own starter.
// LookupStartableClasses drives the gate's bootstrap arm. It records the
// consistency for the same reason listFixtureDeps does: the gate is only
// correct if it reads fully-consistently, and an answer alone cannot
// distinguish a fully-consistent call from a stale one.
func (d *startFixtureDeps) LookupStartableClasses(_ context.Context,
	_ identity.CanonicalUserID, _ uint32, fullyConsistent bool,
) (spicedb.StartableClasses, error) {
	d.mu.Lock()
	d.gotClassLookupCalled = true
	d.gotClassLookupFullyConsistent = fullyConsistent
	d.mu.Unlock()
	return d.startableClasses, d.startableClassesErr
}

func (d *startFixtureDeps) CheckInteract(_ context.Context, ns, name, subject string) (bool, error) {
	d.mu.Lock()
	d.interactSeen = append(d.interactSeen, interactCall{ns: ns, name: name, subject: subject})
	d.mu.Unlock()
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

func (d *startFixtureDeps) K8s() client.Client    { return d.k8s }
func (d *startFixtureDeps) TrustedOrigin() string { return d.origin }
func (d *startFixtureDeps) Logger() logr.Logger   { return d.log }

// StartableNamespaces defaults to the fixture's own namespace so the ordinary
// rows exercise a REACHABLE start; a row that wants the unreachable case sets
// startNamespaces to something else (or to nothing).
func (d *startFixtureDeps) StartableNamespaces() []string {
	if d.startNamespaces == nil {
		return []string{startTestNS}
	}
	return d.startNamespaces
}

// WorkshopNamespacesFor: no test in this file exercises the dynamic arm —
// the static namespace above already reaches every row these tests build.
func (d *startFixtureDeps) WorkshopNamespacesFor(context.Context, string) ([]string, error) {
	return nil, nil
}

func (d *startFixtureDeps) LiveSessions() LiveSessions {
	// A genuine nil interface when there is no table — never a typed-nil
	// pointer wrapped in one (AGENTS.md's typed-nil rule), which would pass the
	// handler's `live != nil` check and then panic.
	if d.live == nil {
		return nil
	}
	return d.live
}

func (d *startFixtureDeps) StartBrowserSession() browsersession.StartFunc {
	if d.noStart {
		return nil
	}
	bd := startBrowserDeps{k8s: d.k8s, granter: d.granter, logger: d.log}
	return func(ctx context.Context, p browsersession.Params) (browsersession.Created, error) {
		return browsersession.Create(ctx, bd, p)
	}
}

// The five methods below exist so *startFixtureDeps additionally satisfies
// agentui.Deps — viewFor casts to it for any `?session=` selection, which the
// spanning test's second leg performs.
func (d *startFixtureDeps) NATSRequest() channelevents.RequestFunc                  { return nil }
func (d *startFixtureDeps) Memory() memory.Memory                                   { return fakeViewMemory{} }
func (d *startFixtureDeps) Artifacts() *artifacts.Service                           { return nil }
func (d *startFixtureDeps) ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc { return nil }
func (d *startFixtureDeps) NATS() *nats.Conn                                        { return nil }

var (
	_ Deps         = (*startFixtureDeps)(nil)
	_ agentui.Deps = (*startFixtureDeps)(nil)
)

func (d *startFixtureDeps) logged() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.Join(d.loggedLines, "\n")
}

func (d *startFixtureDeps) lookups() []lookupCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]lookupCall(nil), d.lookupCalls...)
}

// adoptCall records one fakeLiveSessions.Adopt invocation.
type adoptCall struct{ ns, name, subject string }

// fakeLiveSessions is a recording LiveSessions. reserveErr/adoptErr drive the
// capacity and wiring rows; the recorded calls are what let a test assert the
// handler reserved BEFORE it created and adopted the session it actually made
// (rather than the class name, which is the mutation that would otherwise look
// identical).
type fakeLiveSessions struct {
	mu         sync.Mutex
	reserved   []adoptCall
	released   int
	adopted    []adoptCall
	reserveErr error
	adoptErr   error
}

func (f *fakeLiveSessions) Reserve(ns, name, subject string) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reserveErr != nil {
		return nil, f.reserveErr
	}
	f.reserved = append(f.reserved, adoptCall{ns: ns, name: name, subject: subject})
	return func() {
		f.mu.Lock()
		f.released++
		f.mu.Unlock()
	}, nil
}

func (f *fakeLiveSessions) Adopt(_ context.Context, ns, name, subject string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adopted = append(f.adopted, adoptCall{ns: ns, name: name, subject: subject})
	return f.adoptErr
}

func (f *fakeLiveSessions) snapshot() (reserved, adopted []adoptCall, released int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]adoptCall(nil), f.reserved...), append([]adoptCall(nil), f.adopted...), f.released
}

// --- fixtures --------------------------------------------------------------

// startFixtureSession is an AgentSession the viewer already interacts with —
// the standing that authorizes starting another of the same class in the same
// namespace.
func startFixtureSession(ns, name, class string) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: class},
	}
	s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
	return s
}

// startFixtureClass carries an explicit UID: the fake client does not
// synthesize one for objects seeded via WithObjects, and browsersession.Create
// owner-refs the Channel it creates to this AgentClass's UID.
func startFixtureClass(ns, name string, valid bool) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(ns + "-" + name + "-uid")},
	}
	status := metav1.ConditionFalse
	if valid {
		status = metav1.ConditionTrue
	}
	ac.Status.Conditions = []metav1.Condition{{Type: spiceboxv1alpha1.AgentClassConditionValid, Status: status, Reason: "Fixture"}}
	return ac
}

type startDepsOption func(*startFixtureDeps, *[]client.Object)

// lookupAnswers fixes what LookupInteractableSessions returns. It is never
// updated by a create — see startFixtureDeps.lookupRefs.
func lookupAnswers(refs ...spicedb.SessionRef) startDepsOption {
	return func(d *startFixtureDeps, _ *[]client.Object) { d.lookupRefs = append(d.lookupRefs, refs...) }
}

func lookupFails(err error) startDepsOption {
	return func(d *startFixtureDeps, _ *[]client.Object) { d.lookupErr = err }
}

// lookupTruncates makes the lookup report a capped answer. Nothing errors —
// which is the point: the gate's set is silently incomplete.
func lookupTruncates() startDepsOption {
	return func(d *startFixtureDeps, _ *[]client.Object) { d.lookupTruncated = true }
}

// sessionGetFails makes the AgentSession Get for one object return a
// non-NotFound error, the way an apiserver blip does. buildSessionList drops
// the row and counts it into notices.Unavailable, so the viewer's standing on
// it becomes invisible to the gate.
func sessionGetFails(ns, name string, err error) startDepsOption {
	return func(d *startFixtureDeps, _ *[]client.Object) {
		if d.sessionGetErrors == nil {
			d.sessionGetErrors = map[string]error{}
		}
		d.sessionGetErrors[ns+"/"+name] = err
	}
}

// classGetFails makes the AgentClass Get for one object return a non-NotFound
// error — a throttled apiserver, a webhook stall, an RBAC edge. The class
// itself is seeded and Valid, so this isolates "the read failed" from "the
// class is not usable", which are two different answers with two different
// statuses.
func classGetFails(ns, class string, err error) startDepsOption {
	return func(d *startFixtureDeps, _ *[]client.Object) {
		if d.classGetErrors == nil {
			d.classGetErrors = map[string]error{}
		}
		d.classGetErrors[ns+"/"+class] = err
	}
}

func withStartSession(ns, name, class string) startDepsOption {
	return func(_ *startFixtureDeps, objs *[]client.Object) {
		*objs = append(*objs, startFixtureSession(ns, name, class))
	}
}

func withStartClass(ns, class string, valid bool) startDepsOption {
	return func(_ *startFixtureDeps, objs *[]client.Object) {
		*objs = append(*objs, startFixtureClass(ns, class, valid))
	}
}

// withStartClassOfferingUI seeds a class that declares an agent-defined view,
// which is what makes an opening message optional for it.
func withStartClassOfferingUI(ns, class string, valid bool) startDepsOption {
	return func(_ *startFixtureDeps, objs *[]client.Object) {
		ac := startFixtureClass(ns, class, valid)
		ac.Spec.AgentUI = &spiceboxv1alpha1.AgentClassUIGrant{Ref: class + "-ui"}
		*objs = append(*objs, ac)
	}
}

// withGatedStartClass seeds a Valid class that declares a start-gate
// allowlist (spec.authz.session.allowedStarters), the way gatedDemoAgentClass
// does in pkg/web/browsersession's own tests. startBrowserDeps.StartChecker()
// (above) always returns nil, so a session of THIS class reaches
// browsersession.Create's gate with perm != "" and no checker configured —
// the real fail-closed branch, not a mocked StartFunc — and Create answers
// ErrNotAnAllowedStarter for real.
func withGatedStartClass(ns, class string) startDepsOption {
	return func(_ *startFixtureDeps, objs *[]client.Object) {
		ac := startFixtureClass(ns, class, true)
		ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Session: &spiceboxv1alpha1.SessionAuthz{
			AllowedStarters: []string{"user:listed"},
		}}
		*objs = append(*objs, ac)
	}
}

func withLiveSessions(l *fakeLiveSessions) startDepsOption {
	return func(d *startFixtureDeps, _ *[]client.Object) { d.live = l }
}

func withoutStartCollaborator() startDepsOption {
	return func(d *startFixtureDeps, _ *[]client.Object) { d.noStart = true }
}

// creatableNowhere makes this webd's create-reachable namespace set EMPTY —
// the shared-cluster shape where a viewer's standing comes from a namespace no
// Role grants this pod write verbs in. An empty (non-nil) slice, because nil
// means "the fixture's own namespace" (see StartableNamespaces).
func creatableNowhere() startDepsOption {
	return func(d *startFixtureDeps, _ *[]client.Object) { d.startNamespaces = []string{} }
}

// newStartDeps builds the default fixture: the viewer may interact with ONE
// existing session of demo-agent in demo-ns (seeded into the Granter, so the
// same CheckInteract the page will call answers true for it), that session's
// object exists, and its class is Valid.
func newStartDeps(t *testing.T, opts ...startDepsOption) *startFixtureDeps {
	t.Helper()

	granter := &startFakeGranter{}
	d := &startFixtureDeps{granter: granter, origin: startTestOrigin}
	d.log = funcr.New(func(_, args string) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.loggedLines = append(d.loggedLines, args)
	}, funcr.Options{})

	var objs []client.Object
	for _, o := range opts {
		o(d, &objs)
	}

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	classErrors := d.classGetErrors
	if classErrors == nil {
		classErrors = map[string]error{}
	}
	d.k8s = &errorInjectingClient{
		Client:          fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		sessionErrors:   d.sessionGetErrors,
		classErrors:     classErrors,
		settingsErr:     d.settingsGetErr,
		workshopListErr: d.workshopListErr,
	}
	return d
}

// grantInteract seeds the recording Granter so CheckInteract admits the viewer
// on an EXISTING fixture session — the standing the whole rule derives from.
// Written through the same TouchStartedBy browsersession.Create uses, so the
// canonical id can never differ between the fixture and a created session.
func grantInteract(t *testing.T, d *startFixtureDeps, ns, name string) {
	t.Helper()
	_, principal, err := browsersession.DeriveIdentity(identity.Subject(startTestSubject))
	require.NoError(t, err)
	canonical, err := principal.Canonical()
	require.NoError(t, err)
	require.NoError(t, d.granter.TouchStartedBy(context.Background(), ns, name, canonical))
}

// defaultStartFixture is the shape almost every case starts from: one
// interactable session of demo-agent in demo-ns, its Valid class, and a live
// table.
func defaultStartFixture(t *testing.T, extra ...startDepsOption) (*startFixtureDeps, *fakeLiveSessions) {
	t.Helper()
	live := &fakeLiveSessions{}
	opts := append([]startDepsOption{
		lookupAnswers(spicedb.SessionRef{Namespace: startTestNS, Name: startTestExists}),
		withStartSession(startTestNS, startTestExists, startTestClass),
		withStartClass(startTestNS, startTestClass, true),
		withLiveSessions(live),
	}, extra...)
	d := newStartDeps(t, opts...)
	grantInteract(t, d, startTestNS, startTestExists)
	return d, live
}

// postStart drives the real handler. Origin defaults to the trusted one; pass
// a different value to exercise the CSRF pin.
func postStart(t *testing.T, d Deps, body string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/sessions/api/start", strings.NewReader(body))
	r.Header.Set("Origin", startTestOrigin)
	r = r.WithContext(webui.WithSubjectForTest(context.Background(), startTestSubject))
	for _, m := range mutate {
		m(r)
	}
	rec := httptest.NewRecorder()
	startHandler(d).ServeHTTP(rec, r)
	return rec
}

// pageRequestSelecting builds the GET the shell serves for `?session=ns/name`.
func pageRequestSelecting(ns, name string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/sessions?session="+url.QueryEscape(ns+"/"+name), nil)
}

func ctxWithStartSubject() context.Context {
	return webui.WithSubjectForTest(context.Background(), startTestSubject)
}

// assertNoNewObjects fails if any Channel or Secret exists, or if the
// AgentSession count has grown past baseline — the "nothing was written" half
// of a refusal, which matters as much as the status: a refusal that has
// already created a Channel is the orphan case a status-only assertion cannot
// see.
func assertNoNewObjects(t *testing.T, k8s client.Client, baselineSessions int) {
	t.Helper()
	for _, ns := range []string{startTestNS, startTestOtherNS} {
		var chans spiceboxv1alpha1.ChannelList
		require.NoError(t, k8s.List(context.Background(), &chans, client.InNamespace(ns)))
		assert.Empty(t, chans.Items, "no Channel may exist in %s after a refused start", ns)

		var secs corev1.SecretList
		require.NoError(t, k8s.List(context.Background(), &secs, client.InNamespace(ns)))
		assert.Empty(t, secs.Items, "no Secret may exist in %s after a refused start", ns)
	}
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, k8s.List(context.Background(), &sessions))
	assert.Len(t, sessions.Items, baselineSessions, "no NEW AgentSession may exist after a refused start")
}

// assertReservationReleased fails unless every slot the handler reserved was
// also released. A leaked reservation is UNRECOVERABLE: the idle reaper
// explicitly skips slots with no entry, so one that is never released counts
// against maxLiveSessionsPerSubject until the process restarts — sixteen
// failed starts would permanently lock that viewer out of starting sessions on
// that webd.
func assertReservationReleased(t *testing.T, d *startFixtureDeps) {
	t.Helper()
	require.NotNil(t, d.live, "this assertion needs a live table to observe")
	reserved, _, released := d.live.snapshot()
	require.NotEmpty(t, reserved, "precondition: the handler must have reserved before it created, or there is nothing to leak")
	assert.Equal(t, len(reserved), released,
		"every reservation the handler took must be released when the start fails; a leaked one is never reclaimed")
}

func startErrorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	msg, _ := body["error"].(string)
	return msg
}

// --- J4: the spanning test -------------------------------------------------

// TestStartThenServeAndListTheNewSession is J4. Three legs, because the two
// obvious ones are individually satisfiable by a broken implementation:
// creating an object proves nothing about serveability, and serving it proves
// nothing about the sidebar the viewer lands in front of.
//
// The fake lookup is primed to answer WITHOUT the new session — which is what
// a MinimizeLatency read genuinely does moments after a started_by write — so
// leg (c) fails unless buildSessionList's selected-session union is present.
//
// The response is decoded into a raw map, NOT the StartResponse struct the
// handler itself encoded with: unmarshalling into the same type the encoder
// used moves encoder and decoder together and pins no JSON key at all.
func TestStartThenServeAndListTheNewSession(t *testing.T) {
	d, live := defaultStartFixture(t)

	rec := postStart(t, d, `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"pick up where we left off"}`)
	require.Equal(t, http.StatusOK, rec.Code,
		"starting a class the viewer already interacts with must succeed; body: %s", rec.Body.String())

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.Contains(t, raw, "ns", "the wire contract must carry \"ns\"")
	require.Contains(t, raw, "name", "the wire contract must carry \"name\"")
	require.Contains(t, raw, "href", "the wire contract must carry \"href\" — the field the browser follows")

	gotNS, _ := raw["ns"].(string)
	gotName, _ := raw["name"].(string)
	gotHref, _ := raw["href"].(string)
	require.Equal(t, startTestNS, gotNS)
	require.NotEqual(t, startTestExists, gotName, "the response must name the NEW session, not the existing one")
	require.Equal(t, agentui.SessionShellHref(gotNS, gotName), gotHref,
		"href must address the session that was created")

	// The address is taken from the HREF, not from the typed Name, so the test
	// proves where a browser following it would actually land.
	hrefNS, hrefName := parseShellHref(t, gotHref)

	// (a) + (b): the shell serves it, and reports it as freshly attached.
	props, _, err := shellPageBuild(d)(ctxWithStartSubject(), pageRequestSelecting(hrefNS, hrefName))
	require.NoError(t, err, "the shell must serve the session the start route just created; a *webui.PageError here means it cannot")
	shell, ok := props.(shellProps)
	require.True(t, ok, "the shell must return props, not %T", props)
	require.NotNil(t, shell.Selected, "the selection must resolve")
	assert.Equal(t, string(agentui.Attached), shell.Selected.SessionOrigin)

	// (c) the sidebar. The lookup is deliberately stale — it never learned
	// about this session — so this passes only because the selected session is
	// unioned in.
	assert.NotContains(t, refNames(d.lookupRefs), gotName,
		"fixture precondition: the lookup must be STALE, or leg (c) is not testing the union")
	assert.Contains(t, names(shell.Sessions), gotName,
		"the new session must be in the sidebar on the very next load, even though the list lookup is stale")

	// The live table saw the session that was actually created — reserved
	// before it existed, adopted after. A handler that reserved the CLASS name,
	// or adopted the existing session, would still 200.
	reserved, adopted, _ := live.snapshot()
	require.Len(t, reserved, 1)
	assert.Equal(t, adoptCall{ns: gotNS, name: gotName, subject: startTestSubject}, reserved[0])
	require.Len(t, adopted, 1)
	assert.Equal(t, adoptCall{ns: gotNS, name: gotName, subject: startTestSubject}, adopted[0])
}

// parseShellHref splits a same-origin session-shell href
// ("/sessions?session=<ns>/<name>") by PARSING the URL, exactly as a browser
// following it would resolve the query.
func parseShellHref(t *testing.T, href string) (ns, name string) {
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

func refNames(refs []spicedb.SessionRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Name
	}
	return out
}

// TestStartGateConsistency is mutation 3's cover. Passing fullyConsistent=false
// to the gate's lookup changes NOTHING a fake can observe by outcome — a stale
// fake answers the same either way — so the distinction is protected by
// asserting the recorded ARGUMENT instead: the gate call must be fully
// consistent (a stale positive would admit a subject whose access was just
// revoked) and the sidebar call must not be (it re-authorizes at every door it
// links to, and paying full consistency on every poll buys nothing).
func TestStartGateConsistency(t *testing.T) {
	d, _ := defaultStartFixture(t)

	rec := postStart(t, d, `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"go"}`)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	gateCalls := d.lookups()
	require.Len(t, gateCalls, 1, "the start gate must perform exactly one lookup")
	assert.True(t, gateCalls[0].fullyConsistent,
		"the start gate is a GATE: a MinimizeLatency read would admit a subject whose access was just revoked")
	assert.Equal(t, uint32(maxListedSessions), gateCalls[0].limit)

	// Now the sidebar, through the same fixture and the same fake.
	_, _, err := shellPageBuild(d)(ctxWithStartSubject(), httptest.NewRequest(http.MethodGet, "/sessions", nil))
	require.NoError(t, err)

	all := d.lookups()
	require.Len(t, all, 2, "the page build must perform its own lookup")
	assert.False(t, all[1].fullyConsistent,
		"the sidebar read stays MinimizeLatency — only the gate differs")
}

// --- the gate table --------------------------------------------------------

// TestStartHandlerGate is every way the route refuses, with the same two
// assertions on each row: the status, and that NOTHING was written. A refusal
// that has already created a Channel is the orphan case a status-only
// assertion cannot see.
func TestStartHandlerGate(t *testing.T) {
	cases := []struct {
		name string
		// build returns the fixture and the number of AgentSessions it seeded.
		build      func(t *testing.T) (*startFixtureDeps, int)
		body       string
		mutate     []func(*http.Request)
		wantStatus int
		// wantMessage, when set, is asserted verbatim — the identical-copy rows
		// depend on the exact string, not on a substring.
		wantMessage string
		check       func(t *testing.T, d *startFixtureDeps, rec *httptest.ResponseRecorder)
	}{
		{
			name: "no subject in context: 401, nothing created",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t)
				return d, 1
			},
			body: `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"go"}`,
			mutate: []func(*http.Request){func(r *http.Request) {
				*r = *r.WithContext(context.Background())
			}},
			wantStatus: http.StatusUnauthorized,
		},
		// The empty-prompt path had NO coverage at all, which is how the two
		// halves of one rule came to disagree: the dialog stopped requiring a
		// message for a view-declaring agent, this handler kept requiring one,
		// and pressing "Open UI" was refused by a server nobody had asked.
		// Every other row in this table supplies a prompt, so none of them
		// could have noticed.
		{
			name: "no message for an agent with no view of its own: 400, still required",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t)
				return d, 1
			},
			body:        `{"ns":"demo-ns","agentClass":"demo-agent","prompt":""}`,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "A message is required to start a session.",
		},
		{
			// The shape check and the message check used to share one branch
			// and one message, so a request missing its NAMESPACE was told a
			// message was required — advice for a mistake nobody made.
			name: "missing namespace: 400 naming the shape, not the message",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t)
				return d, 1
			},
			body:        `{"ns":"","agentClass":"demo-agent","prompt":"go"}`,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "That request could not be read.",
		},
		{
			name: "Origin is not the trusted origin: 403, nothing created",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t)
				return d, 1
			},
			body: `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"go"}`,
			mutate: []func(*http.Request){func(r *http.Request) {
				r.Header.Set("Origin", "https://evil.example")
			}},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "a class the viewer has no interactable session of: 409, nothing created",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t, withStartClass(startTestNS, "other-agent", true))
				return d, 1
			},
			body:        `{"ns":"demo-ns","agentClass":"other-agent","prompt":"go"}`,
			wantStatus:  http.StatusConflict,
			wantMessage: startUnavailableMessage,
		},
		{
			name: "the right class in the WRONG namespace: 409 — the PAIR is checked, not the class",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				// demo-agent is Valid and exists in other-ns too, so only the
				// pair check can refuse this.
				d, _ := defaultStartFixture(t, withStartClass(startTestOtherNS, startTestClass, true))
				return d, 1
			},
			body:        `{"ns":"other-ns","agentClass":"demo-agent","prompt":"go"}`,
			wantStatus:  http.StatusConflict,
			wantMessage: startUnavailableMessage,
		},
		{
			// Named for the branch it actually reaches: a class the viewer holds
			// nothing on is refused by the AUTHORIZATION gate whether or not it
			// exists, so this row does not exercise Create's unknown-class
			// branch. That branch is covered by the not-Valid row below and by
			// TestStartUnauthorizedAndUnknownClassShareTheirCopy.
			name: "a class the viewer holds nothing on and that does not exist: 409, same copy",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t)
				return d, 1
			},
			body:        `{"ns":"demo-ns","agentClass":"no-such-agent","prompt":"go"}`,
			wantStatus:  http.StatusConflict,
			wantMessage: startUnavailableMessage,
		},
		{
			// The viewer's standing is real and the gate admits the pair; this
			// server simply holds no create RBAC in that namespace. It must be a
			// refusal with its OWN copy — not the unauthorized-or-unknown 409,
			// whose copy asserts a verdict on the agent that is false here, and
			// not the generic 500 an apiserver Forbidden used to produce. And
			// nothing may be created.
			name: "the namespace is one this server cannot create in: 409 with its own copy, nothing created",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t, creatableNowhere())
				return d, 1
			},
			body:        `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"go"}`,
			wantStatus:  http.StatusConflict,
			wantMessage: startNotStartableHere,
			check: func(t *testing.T, d *startFixtureDeps, _ *httptest.ResponseRecorder) {
				assert.Contains(t, d.logged(), "cannot create a session in that namespace",
					"the operator log must name the real cause — RBAC reach, not authorization")
				assert.NotContains(t, d.logged(), "holds no interactable session",
					"the viewer's standing is real here; the refusal line that denies it must not be emitted")
			},
		},
		{
			// The bootstrap arm's own indeterminate case. Same rule as the two
			// below it, on the other arm: an absent pair cannot tell "this
			// viewer may not bootstrap" apart from "SpiceDB did not answer".
			name: "the agentclass start_session lookup errors: 503, never a 409",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				// No standing at all, so the derived arm contributes nothing
				// and the requested pair's presence rests entirely on the arm
				// that just failed.
				live := &fakeLiveSessions{}
				d := newStartDeps(t,
					lookupAnswers(),
					withStartClass(startTestNS, startTestClass, true),
					withLiveSessions(live))
				d.startableClassesErr = errors.New("injected class lookup outage")
				return d, 0
			},
			body:        `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"go"}`,
			wantStatus:  http.StatusServiceUnavailable,
			wantMessage: startAuthzUnavailable,
			check: func(t *testing.T, d *startFixtureDeps, _ *httptest.ResponseRecorder) {
				assert.Contains(t, d.logged(), "injected class lookup outage",
					"the underlying cause must reach the operator log")
				assert.Contains(t, d.logged(), "could not be established completely",
					"the log must say the standing was UNKNOWN, not that the viewer holds nothing")
				assert.NotContains(t, d.logged(), "holds no interactable session",
					"the refusal line asserts something false here and must not be emitted")
			},
		},
		{
			name: "the lookup errors: 503, never 409 and never 403",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t, lookupFails(errors.New("injected spicedb outage")))
				return d, 1
			},
			body:        `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"go"}`,
			wantStatus:  http.StatusServiceUnavailable,
			wantMessage: startAuthzUnavailable,
			check: func(t *testing.T, d *startFixtureDeps, _ *httptest.ResponseRecorder) {
				assert.Contains(t, d.logged(), "injected spicedb outage",
					"an authorization outage must reach the operator log with its cause")
			},
		},
		{
			// I1: a transient Kubernetes fault on the ONE session that grants
			// the viewer's standing makes it invisible to the gate. The
			// authorized set narrows to nothing, and reading that as a denial
			// would tell every viewer they have no access during an apiserver
			// blip — while the operator log pointed at authorization instead of
			// at the apiserver. Only a COMPLETE list may refuse.
			name: "the standing session's Get fails for a non-NotFound reason: 503, never a 409",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t,
					sessionGetFails(startTestNS, startTestExists, errors.New("etcdserver: request timed out")))
				return d, 1
			},
			body:        `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"go"}`,
			wantStatus:  http.StatusServiceUnavailable,
			wantMessage: startAuthzUnavailable,
			check: func(t *testing.T, d *startFixtureDeps, _ *httptest.ResponseRecorder) {
				assert.Contains(t, d.logged(), "etcdserver: request timed out",
					"the underlying cause must reach the operator log")
				assert.Contains(t, d.logged(), "could not be established completely",
					"the log must say the standing was UNKNOWN, not that the viewer holds nothing")
				assert.NotContains(t, d.logged(), "holds no interactable session",
					"the refusal line asserts something false here and must not be emitted")
			},
		},
		{
			// The same shape with no error at all: the lookup answered only the
			// first maxListedSessions of a longer set, so a pair outside that
			// window is absent for a reason that is not a denial.
			name: "the lookup is truncated: 503, never a 409",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				// The viewer's standing session is NOT in the (capped) answer,
				// exactly as it would not be for a pair beyond the window.
				live := &fakeLiveSessions{}
				d := newStartDeps(t,
					lookupTruncates(),
					withStartSession(startTestNS, startTestExists, startTestClass),
					withStartClass(startTestNS, startTestClass, true),
					withLiveSessions(live))
				grantInteract(t, d, startTestNS, startTestExists)
				return d, 1
			},
			body:        `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"go"}`,
			wantStatus:  http.StatusServiceUnavailable,
			wantMessage: startAuthzUnavailable,
			check: func(t *testing.T, d *startFixtureDeps, _ *httptest.ResponseRecorder) {
				assert.Contains(t, d.logged(), "could not be established completely")
				assert.NotContains(t, d.logged(), "holds no interactable session")
			},
		},
		{
			name: "body is not JSON: 400",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t)
				return d, 1
			},
			body:       `not json at all`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "prompt absent: 400",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t)
				return d, 1
			},
			body:       `{"ns":"demo-ns","agentClass":"demo-agent"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "prompt is whitespace only: 400",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t)
				return d, 1
			},
			body:       `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"   \n  "}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "ns blank: 400",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t)
				return d, 1
			},
			body:       `{"ns":"","agentClass":"demo-agent","prompt":"go"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "agentClass blank: 400",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t)
				return d, 1
			},
			body:       `{"ns":"demo-ns","agentClass":"","prompt":"go"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "a malformed body AND an unauthorized class: 400 — the body is decoded first, by design",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t)
				return d, 1
			},
			// Truncated JSON that would, if it parsed, name a class the viewer
			// holds nothing on. This row is what makes the ordering claim in
			// startHandler's doc checkable rather than an unverified assertion.
			body:       `{"ns":"demo-ns","agentClass":"no-such-agent","prompt":"go"`,
			wantStatus: http.StatusBadRequest,
			check: func(t *testing.T, d *startFixtureDeps, _ *httptest.ResponseRecorder) {
				assert.Empty(t, d.lookups(),
					"a malformed body must be answered before the authorization lookup runs at all")
			},
		},
		{
			name: "the subject is at their own live-session limit: 429, nothing created",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t,
					withLiveSessions(&fakeLiveSessions{reserveErr: chat.ErrTooManySessionsForSubject}))
				return d, 1
			},
			body:        `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"go"}`,
			wantStatus:  http.StatusTooManyRequests,
			wantMessage: startTooManyForYou,
			check: func(t *testing.T, d *startFixtureDeps, _ *httptest.ResponseRecorder) {
				assert.Contains(t, d.logged(), "the viewer is at their own live-session limit")
				assert.NotContains(t, d.logged(), "capacity ceiling",
					"the two limits must not share a log line — they are different problems with different remedies")
			},
		},
		{
			name: "the process is at its live-session capacity ceiling: 503, nothing created",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t,
					withLiveSessions(&fakeLiveSessions{reserveErr: chat.ErrServerAtCapacity}))
				return d, 1
			},
			body:        `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"go"}`,
			wantStatus:  http.StatusServiceUnavailable,
			wantMessage: startAtCapacity,
			check: func(t *testing.T, d *startFixtureDeps, _ *httptest.ResponseRecorder) {
				assert.Contains(t, d.logged(), "capacity ceiling")
				assert.NotContains(t, d.logged(), "their own live-session limit")
			},
		},
		{
			name: "the class exists but is not Valid: 409, nothing created, same copy as the unauthorized case",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				// The viewer DOES interact with a session of not-ready in
				// demo-ns, so the authorization passes and only Create refuses.
				live := &fakeLiveSessions{}
				d := newStartDeps(t,
					lookupAnswers(spicedb.SessionRef{Namespace: startTestNS, Name: "demo-stale"}),
					withStartSession(startTestNS, "demo-stale", "not-ready"),
					withStartClass(startTestNS, "not-ready", false),
					withLiveSessions(live))
				grantInteract(t, d, startTestNS, "demo-stale")
				return d, 1
			},
			body:        `{"ns":"demo-ns","agentClass":"not-ready","prompt":"go"}`,
			wantStatus:  http.StatusConflict,
			wantMessage: startUnavailableMessage,
			check: func(t *testing.T, d *startFixtureDeps, _ *httptest.ResponseRecorder) {
				assertReservationReleased(t, d)
			},
		},
		{
			// The class EXISTS and is Valid; only the READ of it fails. That is
			// not a verdict on the agent, so it must not borrow the 409 above:
			// a 409 with "that agent is not available" tells the viewer their
			// agent is unusable, and the Info line beside it tells an operator
			// the same, while the truth is that the apiserver did not answer.
			//
			// The refusal copy is deliberately unchanged in the OTHER direction
			// too: a 500 says a read failed and nothing about whether the class
			// exists, so the shared unauthorized/unknown message keeps its
			// anti-enumeration property.
			name: "the class read FAILS (not a NotFound): 500, never the class-verdict 409",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t,
					classGetFails(startTestNS, startTestClass, errors.New("etcdserver: request timed out")))
				return d, 1
			},
			body:        `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"go"}`,
			wantStatus:  http.StatusInternalServerError,
			wantMessage: startInternalMessage,
			check: func(t *testing.T, d *startFixtureDeps, _ *httptest.ResponseRecorder) {
				assertReservationReleased(t, d)
				assert.Contains(t, d.logged(), "etcdserver",
					"the real cause must reach the log, or an operator debugs the agent instead of the apiserver")
				assert.NotContains(t, d.logged(), "agent class is unknown or not yet valid",
					"the log must not assert a verdict on a class that is fine")
			},
		},
		{
			// An unrecognized Reserve error is neither capacity limit: it is an
			// internal wiring fault and must not be reported to the viewer as
			// something they did, nor as something waiting will fix.
			name: "Reserve fails for an unrecognized reason: 500, nothing created, not reported as a capacity limit",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t,
					withLiveSessions(&fakeLiveSessions{reserveErr: errors.New("injected reservation fault")}))
				return d, 1
			},
			body:        `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"go"}`,
			wantStatus:  http.StatusInternalServerError,
			wantMessage: startInternalMessage,
			check: func(t *testing.T, d *startFixtureDeps, _ *httptest.ResponseRecorder) {
				assert.Contains(t, d.logged(), "injected reservation fault")
				assert.NotContains(t, d.logged(), "at their own live-session limit")
				assert.NotContains(t, d.logged(), "capacity ceiling")
			},
		},
		{
			name: "the start collaborator is absent at request time: 500, nothing created",
			build: func(t *testing.T) (*startFixtureDeps, int) {
				d, _ := defaultStartFixture(t, withoutStartCollaborator())
				return d, 1
			},
			body:        `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"go"}`,
			wantStatus:  http.StatusInternalServerError,
			wantMessage: startInternalMessage,
			check: func(t *testing.T, d *startFixtureDeps, _ *httptest.ResponseRecorder) {
				assert.Contains(t, d.logged(), "StartBrowserSession is nil at request time")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, baseline := tc.build(t)
			rec := postStart(t, d, tc.body, tc.mutate...)

			assert.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())
			if tc.wantMessage != "" {
				assert.Equal(t, tc.wantMessage, startErrorBody(t, rec))
			}
			assertNoNewObjects(t, d.k8s, baseline)
			if tc.check != nil {
				tc.check(t, d, rec)
			}
		})
	}
}

// TestStartUnauthorizedAndUnknownClassShareTheirCopy is mutation 7's cover:
// giving the unauthorized case distinct copy would turn this endpoint into a
// probe for which agents exist on the cluster.
//
// The two halves must exercise the TWO DIFFERENT refusal branches, or the test
// proves nothing. A class the viewer holds nothing on and a class that does
// not exist at all BOTH stop at the authorization gate — comparing those two
// compares one branch with itself. So the second half is a class the viewer
// DOES hold standing on (an existing session names it) whose AgentClass is
// absent from the cluster: that one passes the gate and is refused by
// browsersession.Create instead, which is the other branch.
func TestStartUnauthorizedAndUnknownClassShareTheirCopy(t *testing.T) {
	// Refused at the gate: the viewer interacts with no session of other-agent.
	unauthorized, _ := defaultStartFixture(t, withStartClass(startTestNS, "other-agent", true))

	// Refused by Create: the viewer interacts with a session whose spec.class
	// is "vanished-agent", but no such AgentClass exists — deliberately NOT
	// seeded, so Create answers ErrUnknownAgentClass.
	byCreate := newStartDeps(t,
		lookupAnswers(spicedb.SessionRef{Namespace: startTestNS, Name: "demo-orphan"}),
		withStartSession(startTestNS, "demo-orphan", "vanished-agent"),
		withLiveSessions(&fakeLiveSessions{}))
	grantInteract(t, byCreate, startTestNS, "demo-orphan")

	a := postStart(t, unauthorized, `{"ns":"demo-ns","agentClass":"other-agent","prompt":"go"}`)
	b := postStart(t, byCreate, `{"ns":"demo-ns","agentClass":"vanished-agent","prompt":"go"}`)

	require.Equal(t, http.StatusConflict, a.Code, "body: %s", a.Body.String())
	require.Equal(t, http.StatusConflict, b.Code, "body: %s", b.Body.String())
	// Fixture precondition: the second really did reach the Create branch. The
	// class here is genuinely ABSENT (a NotFound), which is the only Get
	// outcome that still produces this branch — a failed read answers 500
	// instead, and would fail this precondition rather than silently comparing
	// two 500s.
	require.Contains(t, byCreate.logged(), "agent class is unknown or not yet valid",
		"fixture precondition: this half must be refused by Create, not by the gate")
	assert.Equal(t, startErrorBody(t, a), startErrorBody(t, b),
		"an unauthorized class and one the cluster does not have must be indistinguishable to the caller")
	assertNoNewObjects(t, byCreate.k8s, 1)
	assertReservationReleased(t, byCreate)
}

// TestStartGatedClassAndUnknownClassShareTheirCopy proves the start-gate
// refusal (browsersession.ErrNotAnAllowedStarter) is indistinguishable from
// the unknown-class refusal (browsersession.ErrUnknownAgentClass) at the HTTP
// layer — same status, byte-identical body — so a caller cannot tell "this
// class carries an allowlist you are not on" apart from "this class does not
// exist". Giving the gated case distinct copy would turn this endpoint into a
// probe for which agents declare start allowlists, the same leak
// TestStartUnauthorizedAndUnknownClassShareTheirCopy guards for the
// authorization-gate/unknown-class pair; this test is its sibling for
// browsersession.Create's OWN two refusal branches.
//
// Both halves pass the route's own authorization gate (the viewer interacts
// with an existing session of each class) and are refused inside Create
// itself, so this compares the two branches Create can take, not that gate
// against Create.
func TestStartGatedClassAndUnknownClassShareTheirCopy(t *testing.T) {
	// Refused by Create's start gate: the viewer interacts with a session of
	// gated-agent, whose class declares spec.authz.session.allowedStarters.
	// startBrowserDeps.StartChecker() (above) always returns nil, so the gate
	// fails closed with ErrNotAnAllowedStarter — the real production branch,
	// not a mocked StartFunc.
	byGate := newStartDeps(t,
		lookupAnswers(spicedb.SessionRef{Namespace: startTestNS, Name: "demo-gated"}),
		withStartSession(startTestNS, "demo-gated", "gated-agent"),
		withGatedStartClass(startTestNS, "gated-agent"),
		withLiveSessions(&fakeLiveSessions{}))
	grantInteract(t, byGate, startTestNS, "demo-gated")

	// Refused by Create's unknown-class branch: same shape as
	// TestStartUnauthorizedAndUnknownClassShareTheirCopy's byCreate half.
	byUnknown := newStartDeps(t,
		lookupAnswers(spicedb.SessionRef{Namespace: startTestNS, Name: "demo-orphan"}),
		withStartSession(startTestNS, "demo-orphan", "vanished-agent"),
		withLiveSessions(&fakeLiveSessions{}))
	grantInteract(t, byUnknown, startTestNS, "demo-orphan")

	a := postStart(t, byGate, `{"ns":"demo-ns","agentClass":"gated-agent","prompt":"go"}`)
	b := postStart(t, byUnknown, `{"ns":"demo-ns","agentClass":"vanished-agent","prompt":"go"}`)

	require.Equal(t, http.StatusConflict, a.Code, "body: %s", a.Body.String())
	require.Equal(t, http.StatusConflict, b.Code, "body: %s", b.Body.String())
	// Fixture preconditions: each half reached the branch it claims to, not
	// the other one or the authorization gate above both.
	require.Contains(t, byGate.logged(), "viewer is not an allowed starter of this class",
		"fixture precondition: this half must be refused by the start gate")
	require.Contains(t, byUnknown.logged(), "agent class is unknown or not yet valid",
		"fixture precondition: this half must be refused by Create's unknown-class branch")
	assert.Equal(t, a.Code, b.Code, "status must be identical")
	assert.Equal(t, a.Body.String(), b.Body.String(),
		"the full response body must be byte-identical: a gated class the viewer is not listed on and a class "+
			"the cluster does not have must be indistinguishable to the caller")
	assertNoNewObjects(t, byGate.k8s, 1)
	assertReservationReleased(t, byGate)
}

// TestStartAdoptFailure_StillAnswersWithTheAddress: the session exists and is
// the user's. Deleting it because the in-process wiring failed is how a
// session vanishes out from under someone, so this answers 200 and logs.
//
// The reservation is KEPT, which this test previously asserted the opposite of.
// The old assertion described the code rather than the requirement: the
// reservation is the only thing bounding how many AgentSession + Channel +
// creds-Secret triples one viewer can create through this route, and releasing
// it here removes that bound at precisely the moment adopts start failing — a
// degraded operator that never mints a session's memory-token Secret makes
// every start answer 200, drop its slot, and leave three objects behind, with
// no ceiling on the total. The slot is not held forever either: the registry's
// reaper reclaims a reservation nothing ever published into.
func TestStartAdoptFailure_StillAnswersWithTheAddress(t *testing.T) {
	live := &fakeLiveSessions{adoptErr: errors.New("injected wiring failure")}
	d, _ := defaultStartFixture(t, withLiveSessions(live))

	rec := postStart(t, d, `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"go"}`)
	require.Equal(t, http.StatusOK, rec.Code,
		"a failed adopt must not fail the request — the session exists and is addressable; body: %s", rec.Body.String())

	var got StartResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.NotEmpty(t, got.Name)

	// The object must still be there.
	var sess spiceboxv1alpha1.AgentSession
	assert.NoError(t, d.k8s.Get(context.Background(),
		client.ObjectKey{Namespace: got.Ns, Name: got.Name}, &sess),
		"the created session must survive a failed adopt")

	assert.Contains(t, d.logged(), "injected wiring failure",
		"a failed adopt must be surfaced to the operator log, never swallowed")
	_, _, released := live.snapshot()
	assert.Equal(t, 0, released,
		"the reservation must be KEPT: the objects it bounds exist and are the viewer's, so releasing here "+
			"lets one viewer create them without limit for as long as adopts keep failing")
}

// TestStartWithoutALiveSessionTable_StillCreatesAndSaysSo: a webd hosting no
// live-session table still creates an addressable session, and says out loud
// that it is doing so without a reservation — because that also means the two
// capacity limits are not being applied to the request.
func TestStartWithoutALiveSessionTable_StillCreatesAndSaysSo(t *testing.T) {
	d := newStartDeps(t,
		lookupAnswers(spicedb.SessionRef{Namespace: startTestNS, Name: startTestExists}),
		withStartSession(startTestNS, startTestExists, startTestClass),
		withStartClass(startTestNS, startTestClass, true))
	grantInteract(t, d, startTestNS, startTestExists)
	require.Nil(t, d.LiveSessions(), "fixture precondition: no live table")

	rec := postStart(t, d, `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"go"}`)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, d.logged(), "hosts no live-session table",
		"running without the caps must be logged, not silently skipped")
}

// --- the include gate ------------------------------------------------------

// TestShellPageDeniedSelection_NeverAppearsInTheList pins the ONE parameter
// that can widen buildSessionList past its per-subject lookup filter.
// shellPageBuild passes `include` for the `?session=` selection, and the ONLY
// thing that makes that safe is the fully-consistent CheckInteract in front of
// it. With the gate denying, the browser-supplied id must produce a 403 and —
// the part a status assertion alone would miss — must not reach the list at
// all.
func TestShellPageDeniedSelection_NeverAppearsInTheList(t *testing.T) {
	// The viewer interacts with demo-existing. They name a DIFFERENT session in
	// `?session=`, which the Granter-backed CheckInteract refuses.
	d, _ := defaultStartFixture(t, withStartSession(startTestNS, "someone-elses", startTestClass))

	props, _, err := shellPageBuild(d)(ctxWithStartSubject(), pageRequestSelecting(startTestNS, "someone-elses"))

	require.Error(t, err, "a session the viewer may not interact with must be refused")
	var pe *webui.PageError
	require.ErrorAs(t, err, &pe, "the refusal must be a typed *webui.PageError, or renderAuthorizeFailure collapses it")
	assert.Equal(t, http.StatusForbidden, pe.Status)
	assert.Nil(t, props, "a refused selection must render no props at all — including no session list")

	// The gate was asked about the session the URL named, under this subject.
	d.mu.Lock()
	seen := append([]interactCall(nil), d.interactSeen...)
	d.mu.Unlock()
	require.NotEmpty(t, seen, "the gate must actually be consulted")
	assert.Contains(t, seen, interactCall{ns: startTestNS, name: "someone-elses", subject: startTestSubject},
		"the gate must be asked about the session the URL named, not some other one")
}

// TestShellPageIncludesOnlyTheAuthorizedSelection is the positive half of the
// same pin: the union widens the list by EXACTLY the gated session, never by
// anything else the request mentions.
func TestShellPageIncludesOnlyTheAuthorizedSelection(t *testing.T) {
	d, _ := defaultStartFixture(t,
		withStartSession(startTestNS, "unrelated", startTestClass))
	// Grant standing on a second session that the (stale) lookup does not name.
	grantInteract(t, d, startTestNS, "unrelated")

	props, _, err := shellPageBuild(d)(ctxWithStartSubject(), pageRequestSelecting(startTestNS, "unrelated"))
	require.NoError(t, err)
	shell := props.(shellProps)

	got := names(shell.Sessions)
	assert.Contains(t, got, "unrelated", "the gated selection is unioned in")
	assert.Contains(t, got, startTestExists, "the lookup's own answer is still listed")
	assert.Len(t, got, 2, "the union adds exactly one session, never more")
}

// --- the shared start-response golden --------------------------------------

// TestStartResponseMatchesTheSharedGolden asserts this package's StartResponse
// marshals to exactly the shape pkg/web/webui/agentui's startResponse does — one
// file, three readers (both producers and the browser's single parse helper).
// See pkg/web/webui/agentui/start_golden_internal_test.go for the other half.
func TestStartResponseMatchesTheSharedGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean("ui/testdata/start.golden.json"))
	require.NoError(t, err)

	var want map[string]any
	require.NoError(t, json.Unmarshal(raw, &want))
	ns, _ := want["ns"].(string)
	name, _ := want["name"].(string)
	require.NotEmpty(t, ns)
	require.NotEmpty(t, name)

	got, err := json.Marshal(StartResponse{Ns: ns, Name: name, Href: agentui.SessionShellHref(ns, name)})
	require.NoError(t, err)
	var gotMap map[string]any
	require.NoError(t, json.Unmarshal(got, &gotMap))
	assert.Equal(t, want, gotMap,
		"StartResponse's wire shape must match the shared golden; regenerate it only when BOTH producers and the browser's parse helper change together")
}

// TestStartRouteMountsOnlyWithItsCollaborator pins the route-level gate: a
// webd that cannot host a browser session must serve no route that would
// create one it could never deliver to.
func TestStartRouteMountsOnlyWithItsCollaborator(t *testing.T) {
	withIt, _ := defaultStartFixture(t)
	withoutIt, _ := defaultStartFixture(t, withoutStartCollaborator())

	assert.True(t, hasStartRoute(ui{}.Routes(withIt)), "the start route must mount when the collaborator is wired")
	assert.False(t, hasStartRoute(ui{}.Routes(withoutIt)), "the start route must NOT mount without it")
}

func hasStartRoute(routes []webui.Route) bool {
	for _, r := range routes {
		if r.Pattern == "/sessions/api/start" {
			return true
		}
	}
	return false
}

// TestStartRequiresTheStartedByWrite is mutation 1's own row at the JOIN
// level: a Create that skips TouchStartedBy still produces every object, and
// still answers 200 — but the address it hands back is one the shell refuses,
// because the viewer holds no interact on it. This is what makes leg (a) of
// the spanning test depend on the write rather than on an object existing.
func TestStartRequiresTheStartedByWrite(t *testing.T) {
	d, _ := defaultStartFixture(t)
	// Flip the Granter to a no-op AFTER the fixture's own grant is seeded, so
	// the authorization gate still admits the request and only the NEW
	// session's started_by goes missing.
	d.granter.mu.Lock()
	d.granter.skipTouch = true
	d.granter.mu.Unlock()

	rec := postStart(t, d, `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"go"}`)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var got StartResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	_, _, err := shellPageBuild(d)(ctxWithStartSubject(), pageRequestSelecting(got.Ns, got.Name))
	require.Error(t, err, "without a started_by write the created session must not be servable")
	var pe *webui.PageError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, http.StatusForbidden, pe.Status)
}

// TestStartBootstrapsTheClustersFirstSession is the bootstrap arm's happy path
// and the whole reason the arm exists: a viewer holding platform#start_session
// but NO sessions at all starts the cluster's very first one.
//
// Under the derived arm alone this request is a 409 — and it is a 409 for
// every viewer, permanently, because the derivation reads standing off
// existing sessions and there are none to read. The assertion that matters is
// therefore the 200 on an empty lookup: a fixture with any prior standing
// would pass identically with the arm deleted.
func TestStartBootstrapsTheClustersFirstSession(t *testing.T) {
	live := &fakeLiveSessions{}
	d := newStartDeps(t,
		lookupAnswers(), // no standing whatsoever — the cold-start cluster
		withStartClass(startTestNS, startTestClass, true),
		withLiveSessions(live))
	d.startableClasses = spicedb.StartableClasses{
		Refs: []spicedb.ClassRef{{Namespace: startTestNS, Name: startTestClass}},
	}

	rec := postStart(t, d, `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"go"}`)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, d.k8s.List(context.Background(), &sessions, client.InNamespace(startTestNS)))
	assert.Len(t, sessions.Items, 1, "the first session must actually be created, not merely authorized")

	d.mu.Lock()
	defer d.mu.Unlock()
	assert.True(t, d.gotClassLookupCalled, "the gate must consult the bootstrap arm")
	assert.True(t, d.gotClassLookupFullyConsistent,
		"a gate reads fully consistently — the agentclass#platform link is written by the very reconcile "+
			"that created the class, so a stale read would refuse an admin who just installed an agent")
}

// A session always needs an opening prompt — spec.prompt.inline is
// structurally required, and runner.ResolvePrompt errors unless exactly one
// prompt source is set. For a view-declaring agent the viewer has already
// stated their request by asking for the view, so the platform supplies the
// sentence rather than making them narrate what they are about to look at.
//
// Its own test rather than a row in TestStartHandlerGate: that table asserts
// "nothing was created" after every case, which is the right check for a table
// of REFUSALS and the exact opposite of what this one needs to prove.
func TestStartWithNoMessageForAViewDeclaringAgent(t *testing.T) {
	d := newStartDeps(t,
		lookupAnswers(spicedb.SessionRef{Namespace: startTestNS, Name: startTestExists}),
		withStartSession(startTestNS, startTestExists, startTestClass),
		withStartClassOfferingUI(startTestNS, startTestClass, true),
		withLiveSessions(&fakeLiveSessions{}),
	)
	grantInteract(t, d, startTestNS, startTestExists)

	rec := postStart(t, d, `{"ns":"demo-ns","agentClass":"demo-agent","prompt":""}`)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// The substantive assertion, and the reason this is not a table row: a 200
	// proves the gate opened, not that the session can actually run. A session
	// created with a blank prompt has no turn 0 and fails in the runner, far
	// from here and long after this test would have gone green.
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, d.k8s.List(context.Background(), &sessions))
	var created *spiceboxv1alpha1.AgentSession
	for i := range sessions.Items {
		if sessions.Items[i].Name != startTestExists {
			created = &sessions.Items[i]
			break
		}
	}
	require.NotNil(t, created, "a session must have been created")
	assert.Equal(t, openedViaUIPrompt, created.Spec.Prompt.Inline,
		"the platform's own opening turn, not a blank prompt the runner cannot start from")

	// Written as a statement of fact, never as a question in the viewer's
	// voice. Inventing a plausible-sounding request would put words in their
	// transcript and invite the agent to answer something nobody asked.
	assert.NotContains(t, created.Spec.Prompt.Inline, "?",
		"the opening turn must not pose a question the viewer never asked")
}

// The negative control on the same path: an agent with no view of its own
// still requires a real message, and no session is created without one.
func TestStartWithNoMessageForATranscriptAgentCreatesNothing(t *testing.T) {
	d, _ := defaultStartFixture(t)

	rec := postStart(t, d, `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"   "}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "A message is required to start a session.", startErrorBody(t, rec))

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, d.k8s.List(context.Background(), &sessions))
	assert.Len(t, sessions.Items, 1, "only the seeded session; a refusal creates nothing")
}
