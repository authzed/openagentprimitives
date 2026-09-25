package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// demoSubject is a fabricated raw SpiceDB subject ("user:" + a canonical id
// that is NOT base64 — buildSessionList only needs a non-empty canonical
// after stripping "user:", never a real DecodeForDisplay round-trip, which is
// page.go's concern, not this file's). Fabricated per AGENTS.md's
// no-examples-in-tests rule.
const demoSubject = "user:demo-user"

// listFixtureDeps implements sessions.Deps for this file's tests: a
// canned LookupInteractableSessions answer plus a real (fake) Kubernetes
// client, wrapped so individual Gets can be made to fail on demand
// (errorInjectingClient below). Logger captures every line so tests can
// assert an id/cause was actually logged, not merely that a count changed.
// gotLimit/gotFullyConsistent/gotCalled record the lookup call's own
// arguments — buildSessionList's choice of MinimizeLatency and the
// maxListedSessions bound are otherwise unobserved by any assertion.
type listFixtureDeps struct {
	lookupResult spicedb.InteractableSessions
	lookupErr    error
	k8s          client.Client
	log          logr.Logger

	mu          sync.Mutex
	loggedLines []string

	gotLimit           uint32
	gotFullyConsistent bool
	gotCalled          bool

	// startableClasses / startableClassesErr drive the bootstrap arm; the got*
	// fields record what it was asked. Guarded by mu — startableClassesFor may
	// be reached from a handler goroutine in the API tests.
	startableClasses              spicedb.StartableClasses
	startableClassesErr           error
	gotClassLookupCalled          bool
	gotClassLookupLimit           uint32
	gotClassLookupFullyConsistent bool

	// canStart makes StartBrowserSession() return a non-nil (never-invoked)
	// func. Only the shell golden sets it, so shellProps.CanStartSessions is
	// pinned TRUE there rather than at its zero value — a golden carrying a
	// false boolean pins nothing, since a builder hardcoding false would match
	// it exactly.
	canStart bool

	// startNamespaces is what this fixture's webd could create in. Left unset
	// it is EMPTY, which means "nowhere" (browserstart.StartableIn) — the
	// honest default for a fixture that configures nothing, and the one that
	// makes a test wanting a startable pair say so (withStartableNamespaces).
	startNamespaces []string

	// workshopNamespaces / workshopNamespacesErr drive the dynamic arm
	// (WorkshopNamespacesFor). Left unset, the lookup answers (nil, nil) for
	// every subject — no workshops, no error — which leaves every existing
	// test in this file exercising the static arm alone, unchanged.
	workshopNamespaces    map[string][]string
	workshopNamespacesErr error
	// workshopNamespacesCalls counts WorkshopNamespacesFor invocations —
	// guarded by mu, same as gotClassLookupCalled above. This is what proves
	// startableClassesFor resolves the dynamic arm ONCE per call rather than
	// once per (ns, class) row: a cluster-wide Workshop List re-issued per
	// row would still produce a correct answer, just an N+1 one.
	workshopNamespacesCalls int
}

func (d *listFixtureDeps) LookupInteractableSessions(_ context.Context, _ identity.CanonicalUserID,
	limit uint32, fullyConsistent bool,
) (spicedb.InteractableSessions, error) {
	d.mu.Lock()
	d.gotLimit = limit
	d.gotFullyConsistent = fullyConsistent
	d.gotCalled = true
	d.mu.Unlock()
	return d.lookupResult, d.lookupErr
}

func (d *listFixtureDeps) CheckInteract(context.Context, string, string, string) (bool, error) {
	return true, nil
}

// LookupStartableClasses drives the start set's bootstrap arm. It RECORDS that
// it was called and with what consistency, not merely the answer: the arm is
// only correct if the GATE reads fully-consistently, and an answer alone
// cannot distinguish a fully-consistent call from a stale one.
func (d *listFixtureDeps) LookupStartableClasses(_ context.Context,
	_ identity.CanonicalUserID, limit uint32, fullyConsistent bool,
) (spicedb.StartableClasses, error) {
	d.mu.Lock()
	d.gotClassLookupCalled = true
	d.gotClassLookupLimit = limit
	d.gotClassLookupFullyConsistent = fullyConsistent
	d.mu.Unlock()
	return d.startableClasses, d.startableClassesErr
}
func (d *listFixtureDeps) K8s() client.Client { return d.k8s }
func (d *listFixtureDeps) StartBrowserSession() browsersession.StartFunc {
	if !d.canStart {
		return nil
	}
	// Never invoked: no test using this fixture posts to the start route. Its
	// only job is to be non-nil, which is the collaborator-presence fact both
	// Routes and shellProps.CanStartSessions read.
	return func(context.Context, browsersession.Params) (browsersession.Created, error) {
		return browsersession.Created{}, errors.New("listFixtureDeps: StartBrowserSession must not be invoked")
	}
}
func (d *listFixtureDeps) LiveSessions() LiveSessions    { return nil }
func (d *listFixtureDeps) StartableNamespaces() []string { return d.startNamespaces }

// WorkshopNamespacesFor is the dynamic arm's fixture: workshopNamespacesErr
// (withWorkshopNamespacesErr) makes it fail, otherwise it answers whatever
// withWorkshopNamespaces recorded for subject (nil for any other subject —
// a workshop belongs to the one viewer who owns it).
func (d *listFixtureDeps) WorkshopNamespacesFor(_ context.Context, subject string) ([]string, error) {
	d.mu.Lock()
	d.workshopNamespacesCalls++
	d.mu.Unlock()
	if d.workshopNamespacesErr != nil {
		return nil, d.workshopNamespacesErr
	}
	return d.workshopNamespaces[subject], nil
}
func (d *listFixtureDeps) Logger() logr.Logger { return d.log }

// The five methods below exist ONLY so *listFixtureDeps additionally
// satisfies agentui.Deps — required by viewFor's `d.(agentui.Deps)` cast
// (view.go) whenever a caller selects a `?session=`. No test in THIS file
// reaches that cast (list_test.go tests buildSessionList directly, never
// through shellPageBuild's selection branch); page_golden_internal_test.go's
// golden fixture is the one caller that does, so the golden can populate a
// real `selected` agent-ui view. Memory returns a real, working (if
// always-empty) backend — view_test.go's fakeViewMemory — because
// agentui.ViewFor's merge step 503s on a nil one.
func (d *listFixtureDeps) TrustedOrigin() string                                   { return "" }
func (d *listFixtureDeps) NATSRequest() channelevents.RequestFunc                  { return nil }
func (d *listFixtureDeps) Memory() memory.Memory                                   { return fakeViewMemory{} }
func (d *listFixtureDeps) Artifacts() *artifacts.Service                           { return nil }
func (d *listFixtureDeps) ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc { return nil }
func (d *listFixtureDeps) NATS() *nats.Conn                                        { return nil }

var _ agentui.Deps = (*listFixtureDeps)(nil)

// logged joins every captured log line into one string so callers can do a
// simple substring Contains check for an id or cause, without caring which
// exact call site logged it.
func (d *listFixtureDeps) logged() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.Join(d.loggedLines, "\n")
}

// errorInjectingClient wraps a real client.Client and makes Get fail for
// specific (type, namespace, name) triples — the fake client alone has no way
// to make a Get on an object that DOES exist return an arbitrary non-NotFound
// error, which the "a Get errors for a reason other than NotFound" test case
// needs. Keyed by concrete Go type because AgentSession and AgentClass Gets
// share the same client method and could otherwise collide on the same name.
type errorInjectingClient struct {
	client.Client
	sessionErrors map[string]error // "ns/name" -> error, for *AgentSession Gets
	classErrors   map[string]error // "ns/name" -> error, for *AgentClass Gets
	// settingsErr fails every ClusterAgentSettings Get. Not keyed by name:
	// the singleton has exactly one ("cluster"), so a map would be a key
	// nobody could get wrong and a second place to spell it.
	settingsErr error
	// workshopListErr fails every Workshop List. The fake client can return an
	// empty list but never a failed one, and "the workshops could not be
	// counted" must not read as "you have none".
	workshopListErr error
}

func (c *errorInjectingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*spiceboxv1alpha1.WorkshopList); ok && c.workshopListErr != nil {
		return c.workshopListErr
	}
	return c.Client.List(ctx, list, opts...)
}

func (c *errorInjectingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	k := key.Namespace + "/" + key.Name
	switch obj.(type) {
	case *spiceboxv1alpha1.AgentSession:
		if err, ok := c.sessionErrors[k]; ok {
			return err
		}
	case *spiceboxv1alpha1.AgentClass:
		if err, ok := c.classErrors[k]; ok {
			return err
		}
	case *spiceboxv1alpha1.ClusterAgentSettings:
		if c.settingsErr != nil {
			return c.settingsErr
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// listFixtureBuilder accumulates the options below into the pieces
// newListDeps needs: the InteractableSessions the fake lookup answers, the
// Kubernetes objects the fake client is seeded with, and any Get errors to
// inject.
type listFixtureBuilder struct {
	refs            []spicedb.SessionRef
	unrepresentable []string
	conditional     []string
	truncated       bool
	lookupErr       error
	objects         []client.Object
	sessionErrors   map[string]error
	canStart        bool
	startNamespaces []string
	classErrors     map[string]error
	// workshopNamespaces is the dynamic arm's per-subject answer
	// (withWorkshopNamespaces); workshopNamespacesErr makes the lookup fail
	// instead (withWorkshopNamespacesErr).
	workshopNamespaces    map[string][]string
	workshopNamespacesErr error
}

type listDepsOption func(*listFixtureBuilder)

// ref builds a spicedb.SessionRef — the lookup's own coordinate shape.
func ref(ns, name string) spicedb.SessionRef { return spicedb.SessionRef{Namespace: ns, Name: name} }

func lookupReturns(refs ...spicedb.SessionRef) listDepsOption {
	return func(b *listFixtureBuilder) { b.refs = append(b.refs, refs...) }
}

func lookupUnrepresentable(ids ...string) listDepsOption {
	return func(b *listFixtureBuilder) { b.unrepresentable = append(b.unrepresentable, ids...) }
}

func lookupConditional(ids ...string) listDepsOption {
	return func(b *listFixtureBuilder) { b.conditional = append(b.conditional, ids...) }
}

func lookupTruncated() listDepsOption {
	return func(b *listFixtureBuilder) { b.truncated = true }
}

func lookupErrors(err error) listDepsOption {
	return func(b *listFixtureBuilder) { b.lookupErr = err }
}

// sessionOption configures one fabricated AgentSession object that
// withSession seeds into the fake Kubernetes client.
type sessionOption func(*spiceboxv1alpha1.AgentSession)

func classNamed(class string) sessionOption {
	return func(s *spiceboxv1alpha1.AgentSession) { s.Spec.Class = class }
}

func inPhase(phase string) sessionOption {
	return func(s *spiceboxv1alpha1.AgentSession) { s.Status.Phase = phase }
}

func startedAt(ts time.Time) sessionOption {
	return func(s *spiceboxv1alpha1.AgentSession) {
		mt := metav1.NewTime(ts)
		s.Status.StartedAt = &mt
	}
}

// createdAt sets ONLY the object's CreationTimestamp, leaving
// Status.StartedAt unset — the fixture shape that exercises
// buildSessionList's CreationTimestamp fallback branch (no other fixture
// helper reaches it: withSession's plain object literal leaves
// CreationTimestamp at its zero value, and the fake client's tracker does
// not stamp one on its own).
func createdAt(ts time.Time) sessionOption {
	return func(s *spiceboxv1alpha1.AgentSession) {
		s.ObjectMeta.CreationTimestamp = metav1.NewTime(ts)
	}
}

// withSession seeds a fabricated AgentSession into the fake Kubernetes
// client — the "Kubernetes holds this object" half of the join. It does NOT
// make the lookup name it; pair with lookupReturns (or an include ref) for a
// session that should actually render.
func withSession(ns, name string, opts ...sessionOption) listDepsOption {
	return func(b *listFixtureBuilder) {
		sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
		for _, o := range opts {
			o(sess)
		}
		b.objects = append(b.objects, sess)
	}
}

// withClass seeds a fabricated AgentClass so resolveClassTitle finds a
// displayName instead of falling back to the class name.
func withClass(ns, class, displayName string) listDepsOption {
	return func(b *listFixtureBuilder) {
		b.objects = append(b.objects, &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: class},
			Spec:       spiceboxv1alpha1.AgentClassSpec{DisplayName: displayName},
		})
	}
}

// withStartCollaborator makes StartBrowserSession() non-nil, which is what
// shellProps.CanStartSessions reports and what Routes gates the start route
// on. Used by the shell golden so that field is pinned TRUE rather than at
// its zero value.
func withStartCollaborator() listDepsOption {
	return func(b *listFixtureBuilder) { b.canStart = true }
}

// withStartableNamespaces names the namespaces the fixture's webd can create
// in — the SECOND fact each startableClass carries, independent of the
// viewer's standing. Unset means none, so a fixture that wants a pair marked
// startable must say which namespace, and one that wants the unreachable case
// simply omits it.
func withStartableNamespaces(ns ...string) listDepsOption {
	return func(b *listFixtureBuilder) { b.startNamespaces = append(b.startNamespaces, ns...) }
}

// withWorkshopNamespaces makes the dynamic arm (WorkshopNamespacesFor) answer
// ns for subject — the workshop namespaces owned by that one viewer, as
// opposed to withStartableNamespaces' static, cluster-wide set.
func withWorkshopNamespaces(subject string, ns ...string) listDepsOption {
	return func(b *listFixtureBuilder) {
		if b.workshopNamespaces == nil {
			b.workshopNamespaces = map[string][]string{}
		}
		b.workshopNamespaces[subject] = append(b.workshopNamespaces[subject], ns...)
	}
}

// withWorkshopNamespacesErr makes the dynamic arm's lookup fail — the case
// StartableIn and bootstrapStartableClasses must both fail CLOSED on, without
// taking the static arm down with it.
func withWorkshopNamespacesErr(err error) listDepsOption {
	return func(b *listFixtureBuilder) { b.workshopNamespacesErr = err }
}

// withObjects is a generic escape hatch for a fixture that needs an object
// shape none of the named helpers above build (e.g. an AgentClass carrying
// an AgentUI grant, or the AgentUI object itself) — used once, by
// page_golden_internal_test.go's goldenShellFixtureDeps, so the shell's
// golden can populate a real `selected` agent-ui view without every other
// test in this file paying for a bespoke helper it does not need.
func withObjects(objs ...client.Object) listDepsOption {
	return func(b *listFixtureBuilder) { b.objects = append(b.objects, objs...) }
}

// getSessionFails injects a non-NotFound error for a specific AgentSession
// Get — the fake client cannot otherwise produce one for an object that DOES
// exist.
func getSessionFails(ns, name string, err error) listDepsOption {
	return func(b *listFixtureBuilder) { b.sessionErrors[ns+"/"+name] = err }
}

// getClassFails injects a non-NotFound error for a specific AgentClass Get.
func getClassFails(ns, class string, err error) listDepsOption {
	return func(b *listFixtureBuilder) { b.classErrors[ns+"/"+class] = err }
}

// newListDeps builds a *listFixtureDeps from the given options: a canned
// LookupInteractableSessions answer, a fake Kubernetes client seeded with
// withSession/withClass objects, and any injected Get errors. Shared by every
// test in this file — see this file's own top comment.
func newListDeps(t *testing.T, opts ...listDepsOption) *listFixtureDeps {
	t.Helper()

	b := &listFixtureBuilder{sessionErrors: map[string]error{}, classErrors: map[string]error{}}
	for _, o := range opts {
		o(b)
	}

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(b.objects...).Build()

	d := &listFixtureDeps{
		lookupResult: spicedb.InteractableSessions{
			Refs:            append([]spicedb.SessionRef(nil), b.refs...),
			Unrepresentable: append([]string(nil), b.unrepresentable...),
			Conditional:     append([]string(nil), b.conditional...),
			Truncated:       b.truncated,
		},
		lookupErr:             b.lookupErr,
		canStart:              b.canStart,
		startNamespaces:       b.startNamespaces,
		workshopNamespaces:    b.workshopNamespaces,
		workshopNamespacesErr: b.workshopNamespacesErr,
		k8s: &errorInjectingClient{
			Client:        fakeClient,
			sessionErrors: b.sessionErrors,
			classErrors:   b.classErrors,
		},
	}
	d.log = funcr.New(func(_, args string) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.loggedLines = append(d.loggedLines, args)
	}, funcr.Options{})
	return d
}

func names(rows []sessionRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}
	return out
}

// TestListJoinsTheLookupAgainstKubernetes is J1. The fixture is built so each
// of the four join outcomes is present exactly once, because a fixture with
// only joinable sessions cannot distinguish a correct join from one that
// ignores the lookup entirely.
func TestListJoinsTheLookupAgainstKubernetes(t *testing.T) {
	d := newListDeps(t,
		// The lookup answers three ids...
		lookupReturns(
			ref("demo-ns", "alpha"), // ...this one has a Kubernetes object
			ref("demo-ns", "beta"),  // ...so does this one
			ref("demo-ns", "ghost"), // ...this one does NOT
		),
		lookupUnrepresentable("not-a-valid-ref"), // ...and this id will not split
		// Kubernetes additionally holds a session the lookup never named. It
		// must NOT appear: absence from the lookup is a denial, and rendering
		// it would be the authorization-leak direction of this join.
		withSession("demo-ns", "alpha", classNamed("demo-agent")),
		withSession("demo-ns", "beta", classNamed("demo-agent")),
		withSession("demo-ns", "unlisted", classNamed("demo-agent")),
	)

	rows, notices, err := buildSessionList(context.Background(), d, demoSubject, nil, false)
	require.NoError(t, err)

	assert.Equal(t, []string{"alpha", "beta"}, names(rows),
		"only sessions the lookup named AND Kubernetes holds may render")
	assert.Equal(t, 2, notices.Unavailable,
		"the missing object and the unrepresentable id are each counted, not dropped")
	assert.False(t, notices.Truncated)
	assert.NotContains(t, names(rows), "unlisted",
		"a session the lookup did not name must never render — that is a denial, not a display gap")
	assert.Contains(t, d.logged(), "ghost", "the dropped id must reach the operator log")
	assert.Contains(t, d.logged(), "not-a-valid-ref")
}

// TestBuildSessionList_OutcomeTable covers the join's other outcomes: each
// shares the same "build a fixture, call buildSessionList, check the result"
// shape but differs in fixture and expected outcome, so a table keeps that
// shared shape visible.
func TestBuildSessionList_OutcomeTable(t *testing.T) {
	cases := []struct {
		name    string
		build   func(t *testing.T) *listFixtureDeps
		include *spicedb.SessionRef
		check   func(t *testing.T, rows []sessionRow, notices listNotices, err error, d *listFixtureDeps)
	}{
		{
			name: "the lookup errors: an error is returned and rows is nil, never a nil-error empty list",
			build: func(t *testing.T) *listFixtureDeps {
				return newListDeps(t, lookupErrors(errors.New("spicedb unavailable")))
			},
			check: func(t *testing.T, rows []sessionRow, _ listNotices, err error, _ *listFixtureDeps) {
				require.Error(t, err)
				assert.Nil(t, rows, "a lookup error must never fall through to an empty (nil-error) list")
			},
		},
		{
			name: "the lookup returns nothing, no error: an empty dashboard is a legitimate state",
			build: func(t *testing.T) *listFixtureDeps {
				return newListDeps(t)
			},
			check: func(t *testing.T, rows []sessionRow, notices listNotices, err error, _ *listFixtureDeps) {
				require.NoError(t, err)
				assert.Empty(t, rows)
				assert.Equal(t, listNotices{}, notices)
			},
		},
		{
			name: "a non-NotFound Get error on one of three refs: the other two render, Unavailable is 1, the cause is logged",
			build: func(t *testing.T) *listFixtureDeps {
				return newListDeps(t,
					lookupReturns(ref("demo-ns", "alpha"), ref("demo-ns", "beta"), ref("demo-ns", "broken")),
					withSession("demo-ns", "alpha", classNamed("demo-agent")),
					withSession("demo-ns", "beta", classNamed("demo-agent")),
					withSession("demo-ns", "broken", classNamed("demo-agent")),
					getSessionFails("demo-ns", "broken", errors.New("etcd timeout")),
				)
			},
			check: func(t *testing.T, rows []sessionRow, notices listNotices, err error, d *listFixtureDeps) {
				require.NoError(t, err)
				assert.ElementsMatch(t, []string{"alpha", "beta"}, names(rows))
				assert.Equal(t, 1, notices.Unavailable)
				assert.Contains(t, d.logged(), "etcd timeout")
			},
		},
		{
			name: "an AgentClass Get fails: the row still renders with Title == Class, Unavailable is 0",
			build: func(t *testing.T) *listFixtureDeps {
				return newListDeps(t,
					lookupReturns(ref("demo-ns", "alpha")),
					withSession("demo-ns", "alpha", classNamed("demo-agent")),
					getClassFails("demo-ns", "demo-agent", errors.New("etcd timeout")),
				)
			},
			check: func(t *testing.T, rows []sessionRow, notices listNotices, err error, _ *listFixtureDeps) {
				require.NoError(t, err)
				require.Len(t, rows, 1)
				assert.Equal(t, "demo-agent", rows[0].Title, "a missing display name is not an unavailable session")
				assert.Equal(t, 0, notices.Unavailable)
			},
		},
		{
			name: "include names a session the lookup did not return: it renders, Unavailable is 0",
			build: func(t *testing.T) *listFixtureDeps {
				return newListDeps(t, withSession("demo-ns", "fresh", classNamed("demo-agent")))
			},
			include: &spicedb.SessionRef{Namespace: "demo-ns", Name: "fresh"},
			check: func(t *testing.T, rows []sessionRow, notices listNotices, err error, _ *listFixtureDeps) {
				require.NoError(t, err)
				assert.Equal(t, []string{"fresh"}, names(rows))
				assert.Equal(t, 0, notices.Unavailable)
			},
		},
		{
			name: "include names a session the lookup already returned: it renders once, not twice",
			build: func(t *testing.T) *listFixtureDeps {
				return newListDeps(t,
					lookupReturns(ref("demo-ns", "alpha")),
					withSession("demo-ns", "alpha", classNamed("demo-agent")),
				)
			},
			include: &spicedb.SessionRef{Namespace: "demo-ns", Name: "alpha"},
			check: func(t *testing.T, rows []sessionRow, _ listNotices, err error, _ *listFixtureDeps) {
				require.NoError(t, err)
				assert.Equal(t, []string{"alpha"}, names(rows))
			},
		},
		{
			name: "the lookup reports Truncated: notices.Truncated is true and the rows still render",
			build: func(t *testing.T) *listFixtureDeps {
				return newListDeps(t,
					lookupReturns(ref("demo-ns", "alpha")),
					lookupTruncated(),
					withSession("demo-ns", "alpha", classNamed("demo-agent")),
				)
			},
			check: func(t *testing.T, rows []sessionRow, notices listNotices, err error, _ *listFixtureDeps) {
				require.NoError(t, err)
				assert.True(t, notices.Truncated)
				assert.Equal(t, []string{"alpha"}, names(rows))
			},
		},
		{
			name: "a Conditional id: counted into Unavailable, not rendered as a row",
			build: func(t *testing.T) *listFixtureDeps {
				return newListDeps(t, lookupConditional("demo-ns/maybe"))
			},
			check: func(t *testing.T, rows []sessionRow, notices listNotices, err error, _ *listFixtureDeps) {
				require.NoError(t, err)
				assert.Empty(t, rows)
				assert.Equal(t, 1, notices.Unavailable)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.build(t)
			rows, notices, err := buildSessionList(context.Background(), d, demoSubject, tc.include, false)
			tc.check(t, rows, notices, err, d)
		})
	}
}

// TestBuildSessionList_Ordering proves the deterministic sort: newest
// StartedAt first, ties broken ns/name ascending.
func TestBuildSessionList_Ordering(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	d := newListDeps(t,
		lookupReturns(ref("demo-ns", "old-session"), ref("demo-ns", "new-session"),
			ref("demo-ns", "tie-b"), ref("demo-ns", "tie-a")),
		withSession("demo-ns", "old-session", classNamed("demo-agent"), startedAt(older)),
		withSession("demo-ns", "new-session", classNamed("demo-agent"), startedAt(newer)),
		withSession("demo-ns", "tie-b", classNamed("demo-agent"), startedAt(older)),
		withSession("demo-ns", "tie-a", classNamed("demo-agent"), startedAt(older)),
	)

	rows, _, err := buildSessionList(context.Background(), d, demoSubject, nil, false)
	require.NoError(t, err)

	assert.Equal(t, []string{"new-session", "old-session", "tie-a", "tie-b"}, names(rows),
		"newest StartedAt first; ties broken ns/name ascending")
}

// TestBuildSessionList_PhaseFlowsThroughToTheRow proves the mapped label
// (not the raw control-plane phase) reaches the row buildSessionList itself
// returns — TestPhaseLabel_AllPhases below covers phaseLabel in isolation,
// but a mutation that bypasses phaseLabel at buildSessionList's own
// row-construction call site would not be caught by that test alone.
func TestBuildSessionList_PhaseFlowsThroughToTheRow(t *testing.T) {
	d := newListDeps(t,
		lookupReturns(ref("demo-ns", "waiting")),
		withSession("demo-ns", "waiting", classNamed("demo-agent"),
			inPhase(spiceboxv1alpha1.AgentSessionPhaseAwaitingApproval)),
	)

	rows, _, err := buildSessionList(context.Background(), d, demoSubject, nil, false)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	assert.Equal(t, "Waiting for you", rows[0].Phase)
	assert.True(t, rows[0].AwaitingHuman)
	assert.False(t, rows[0].Ended)
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingApproval, rows[0].Phase,
		"the raw control-plane phase string must never reach the row")
}

// TestBuildSessionList_StartableClasses proves the derivation is over the
// RENDERED rows only — a class that exists solely on a Kubernetes session the
// lookup never named must be absent, the same authorization-leak direction
// buildSessionList itself refuses.
//
// It also pins the two facts each pair carries as INDEPENDENT: the fixture's
// webd can create in "demo-ns" and not in "other-ns", so both pairs are listed
// (standing is the viewer's, and nothing here narrows it) and only one is
// marked startable. A derivation that filtered on reachability instead of
// marking it would drop the second pair, and one that ignored reachability
// would mark both.
func TestBuildSessionList_StartableClasses(t *testing.T) {
	d := newListDeps(t,
		lookupReturns(ref("demo-ns", "alpha"), ref("demo-ns", "beta"), ref("other-ns", "gamma")),
		withStartableNamespaces("demo-ns"),
		withSession("demo-ns", "alpha", classNamed("demo-agent")),
		withSession("demo-ns", "beta", classNamed("demo-agent")), // same (ns, class) as alpha
		withSession("other-ns", "gamma", classNamed("other-agent")),
		withClass("demo-ns", "demo-agent", "Demo Agent"),
		withClass("other-ns", "other-agent", "Other Agent"),
		withSession("demo-ns", "unlisted", classNamed("never-offered")),
	)

	rows, _, err := buildSessionList(context.Background(), d, demoSubject, nil, false)
	require.NoError(t, err)

	got, notices := startableClassesFor(context.Background(), d, demoSubject, rows, listNotices{}, false)
	assert.Equal(t, []startableClass{
		{Ns: "demo-ns", Class: "demo-agent", Title: "Demo Agent", Startable: true},
		{Ns: "other-ns", Class: "other-agent", Title: "Other Agent", Startable: false},
	}, got)
	// This fixture's viewer holds no start_session (platformOK is false), so
	// the bootstrap arm contributes nothing and reports no incompleteness —
	// "never-offered" stays absent even though it is an AgentClass in a
	// startable namespace.
	assert.False(t, notices.BootstrapUnavailable, "a viewer without start_session is a clean answer, not an incomplete one")
}

// TestStartableClassesFor_ResolvesWorkshopNamespacesOnce proves the dynamic
// arm — WorkshopNamespacesFor, a cluster-wide Workshop List — is resolved
// EXACTLY ONCE per startableClassesFor call, regardless of how many distinct
// non-static (ns, class) rows it evaluates. Before this fix, each row's own
// browserstart.StartableIn call re-issued the List independently: three
// distinct pairs across two non-static namespaces meant three Lists for one
// page load, contradicting WorkshopNamespacesFor's own "single round trip"
// doc.
func TestStartableClassesFor_ResolvesWorkshopNamespacesOnce(t *testing.T) {
	d := newListDeps(t,
		withStartableNamespaces("default"),
		withWorkshopNamespaces(demoSubject, "ws-1"),
		lookupReturns(ref("ws-1", "alpha"), ref("ws-1", "beta"), ref("ws-2", "gamma")),
		withSession("ws-1", "alpha", classNamed("class-a")),
		withSession("ws-1", "beta", classNamed("class-b")),
		withSession("ws-2", "gamma", classNamed("class-c")),
	)

	rows, _, err := buildSessionList(context.Background(), d, demoSubject, nil, false)
	require.NoError(t, err)
	require.Len(t, rows, 3, "three distinct sessions across two non-static namespaces — three distinct (ns, class) rows for add()")

	_, _ = startableClassesFor(context.Background(), d, demoSubject, rows, listNotices{}, true)

	d.mu.Lock()
	defer d.mu.Unlock()
	assert.Equal(t, 1, d.workshopNamespacesCalls,
		"one cluster-wide Workshop List for the whole call, not one per (ns, class) row")
}

// TestPhaseLabel_AllPhases is the table over all ten AgentSessionPhase*
// constants declared in pkg/apis/v1alpha1/conditions.go: wantLabel pins the
// ACTUAL copy string (not merely "label != phase") — "Active"/"Waiting for
// you" happen to also be pinned by the golden, but "Retrying"/"Asleep"/
// "Ended" were previously asserted only by shape, so swapping two case
// bodies (e.g. AwaitingRetry <-> Idle) survived every test. awaitingHuman is
// pinned true for exactly the four Awaiting* phases.
//
// The row count below (10) is HAND-MAINTAINED, not derived from
// pkg/apis/v1alpha1's own constant list — if that package gains an eleventh
// AgentSessionPhase* constant, this table and the require.Len guard must be
// updated by hand; nothing here detects a forgotten update.
func TestPhaseLabel_AllPhases(t *testing.T) {
	cases := []struct {
		phase        string
		wantLabel    string
		wantAwaiting bool
		wantEnded    bool
	}{
		{spiceboxv1alpha1.AgentSessionPhasePending, "Active", false, false},
		{spiceboxv1alpha1.AgentSessionPhaseRunning, "Active", false, false},
		{spiceboxv1alpha1.AgentSessionPhaseSucceeded, "Ended", false, true},
		{spiceboxv1alpha1.AgentSessionPhaseFailed, "Ended", false, true},
		{spiceboxv1alpha1.AgentSessionPhaseAwaitingApproval, "Waiting for you", true, false},
		{spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision, "Waiting for you", true, false},
		{spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry, "Retrying", false, false},
		{spiceboxv1alpha1.AgentSessionPhaseIdle, "Asleep", false, false},
		{spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, "Waiting for you", true, false},
		{spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice, "Waiting for you", true, false},
	}
	require.Len(t, cases, 10,
		"this table must cover all ten AgentSessionPhase* constants (hand-maintained count — "+
			"update it if pkg/apis/v1alpha1 gains a new phase constant)")

	for _, tc := range cases {
		t.Run(tc.phase+": label="+tc.wantLabel+", awaitingHuman as expected", func(t *testing.T) {
			label, awaitingHuman, ended := phaseLabel(tc.phase)
			assert.Equal(t, tc.wantLabel, label)
			assert.NotEqual(t, tc.phase, label, "the raw phase constant must never reach the browser as-is")
			assert.Equal(t, tc.wantAwaiting, awaitingHuman)
			assert.Equal(t, tc.wantEnded, ended)
		})
	}
}

// TestPhaseLabel_UnrecognizedPhase_FallsBackToUnknown proves phaseLabel's
// default branch (list.go's leak guard for a FUTURE phase constant this
// mapping has not been updated for yet) is actually reachable and correct —
// no test exercised it before this case.
func TestPhaseLabel_UnrecognizedPhase_FallsBackToUnknown(t *testing.T) {
	label, awaitingHuman, ended := phaseLabel("SomeFuturePhaseNotYetMapped")
	assert.Equal(t, "Unknown", label)
	assert.False(t, awaitingHuman)
	assert.False(t, ended)
}

// TestBuildSessionList_StartedAtIsAlwaysUTC proves the .UTC() normalization
// (list.go, both time sources) HOST-INDEPENDENTLY: asserting Location() ==
// time.UTC observes the zone regardless of the machine's own TZ, unlike the
// golden test, which only caught a dropped .UTC() because the developer's
// own machine happened not to be UTC (see TZ=UTC go test -count=1
// ./pkg/web/webui/sessions/... — removing both .UTC() calls stays green there).
// Both time sources get their own subtest: Status.StartedAt, and the
// CreationTimestamp fallback (createdAt), which no other test in this file
// exercises at all.
func TestBuildSessionList_StartedAtIsAlwaysUTC(t *testing.T) {
	// A non-UTC source zone on purpose: if the normalization were dropped,
	// the row would carry this offset instead of "Z" — and, unlike the
	// golden fixture (which uses time.UTC source values and so only differs
	// from correct output when the HOST is non-UTC), this fixture's source
	// value differs from correct output on EVERY host, UTC included.
	nonUTC := time.FixedZone("EST", -5*60*60)

	t.Run("Status.StartedAt is normalized to UTC regardless of host TZ", func(t *testing.T) {
		ts := time.Date(2026, 3, 4, 5, 6, 7, 0, nonUTC)
		d := newListDeps(t,
			lookupReturns(ref("demo-ns", "alpha")),
			withSession("demo-ns", "alpha", classNamed("demo-agent"), startedAt(ts)),
		)

		rows, _, err := buildSessionList(context.Background(), d, demoSubject, nil, false)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.NotNil(t, rows[0].StartedAt)
		assert.Equal(t, time.UTC, rows[0].StartedAt.Location(),
			"row timestamps must be normalized to UTC regardless of the host's TZ")
		assert.True(t, ts.Equal(*rows[0].StartedAt))
	})

	t.Run("the CreationTimestamp fallback is also normalized to UTC", func(t *testing.T) {
		ts := time.Date(2026, 2, 1, 12, 0, 0, 0, nonUTC)
		d := newListDeps(t,
			lookupReturns(ref("demo-ns", "fallback")),
			withSession("demo-ns", "fallback", classNamed("demo-agent"), createdAt(ts)),
		)

		rows, _, err := buildSessionList(context.Background(), d, demoSubject, nil, false)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.NotNil(t, rows[0].StartedAt, "falls back to CreationTimestamp when Status.StartedAt is unset")
		assert.Equal(t, time.UTC, rows[0].StartedAt.Location(),
			"the CreationTimestamp fallback must also be normalized to UTC")
		assert.True(t, ts.Equal(*rows[0].StartedAt))
	})
}

// TestShouldWriteClassTitle is the direct, deterministic test of the
// cache-write guard: shouldWriteClassTitle is extracted purely so this
// decision can be tested WITHOUT going through resolveClassTitle end-to-end.
// A first attempt at this test called resolveClassTitle twice against a
// shared cache (successful resolution, then a failing one) and asserted the
// SECOND call still returned the good title — that assertion passed even
// with the guard's condition reverted to an unconditional
// `cache[key] = title`, because resolveClassTitle's OWN cache-hit fast path
// (`if title, ok := cache[key]; ok { return title }`) returns before ever
// reaching the write it was supposed to be testing, once the key is already
// cached. Confirmed by mutating the write back to unconditional in a /tmp
// scratch copy: the black-box test stayed green. This table tests the
// extracted decision function directly instead, which has no such
// short-circuit to hide behind.
func TestShouldWriteClassTitle(t *testing.T) {
	cases := []struct {
		name          string
		alreadyCached string
		ok            bool
		class         string
		want          bool
	}{
		{"nothing cached yet: always write", "", false, "demo-agent", true},
		{"cached value is exactly the fallback: a write may proceed (upgrade or same fallback again)",
			"demo-agent", true, "demo-agent", true},
		{"cached value is a REAL resolved title: must not be overwritten",
			"Demo Agent", true, "demo-agent", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shouldWriteClassTitle(tc.alreadyCached, tc.ok, tc.class))
		})
	}
}

// TestResolveClassTitle_EndToEnd_SuccessAndFailure covers resolveClassTitle
// itself (not just the extracted decision) for the two straightforward,
// SEPARATE-cache-key cases: a class whose Get succeeds caches and returns
// the real displayName; a class whose Get fails caches and returns the
// class-name fallback. Each uses its own class name so neither call's cache
// entry can mask the other's Get — the exact trap TestShouldWriteClassTitle's
// own doc comment describes avoiding.
func TestResolveClassTitle_EndToEnd_SuccessAndFailure(t *testing.T) {
	var mu sync.Mutex
	cache := make(map[classKey]string)

	resolved := newListDeps(t, withClass("demo-ns", "resolves-ok", "Demo Agent"))
	got := resolveClassTitle(context.Background(), resolved, &mu, cache, "demo-ns", "resolves-ok")
	assert.Equal(t, "Demo Agent", got)
	assert.Equal(t, "Demo Agent", cache[classKey{ns: "demo-ns", class: "resolves-ok"}])

	failing := newListDeps(t) // no AgentClass seeded -> Get returns NotFound
	got = resolveClassTitle(context.Background(), failing, &mu, cache, "demo-ns", "fails-to-resolve")
	assert.Equal(t, "fails-to-resolve", got, "a failed Get falls back to the class name")
	assert.Equal(t, "fails-to-resolve", cache[classKey{ns: "demo-ns", class: "fails-to-resolve"}])
}

// TestBuildSessionList_LookupCalledWithBoundedLimitAndMinimizeLatency proves
// the lookup call's own arguments — buildSessionList's choice of
// fullyConsistent=false and the maxListedSessions bound were previously
// observed by nothing: mutating either argument left every test green.
func TestBuildSessionList_LookupCalledWithBoundedLimitAndMinimizeLatency(t *testing.T) {
	d := newListDeps(t,
		lookupReturns(ref("demo-ns", "alpha")),
		withSession("demo-ns", "alpha", classNamed("demo-agent")),
	)

	_, _, err := buildSessionList(context.Background(), d, demoSubject, nil, false)
	require.NoError(t, err)

	assert.True(t, d.gotCalled, "LookupInteractableSessions must actually be called")
	assert.Equal(t, uint32(maxListedSessions), d.gotLimit,
		"the lookup must be bounded by maxListedSessions, not unlimited")
	assert.False(t, d.gotFullyConsistent,
		"the list read is MinimizeLatency; every door it links to re-checks fully-consistently")
}

// concurrencyTrackingClient wraps a client.Client and records the maximum
// number of concurrent in-flight Get calls it observed — the observation
// channel for g.SetLimit(listGetConcurrency) at buildSessionList's fan-out,
// which no assertion covered before this test: removing the SetLimit call
// left every other test green. The artificial delay inside Get is load
// -bearing: an in-memory fake client alone completes far too fast for
// concurrent Gets to ever overlap long enough to be observed.
type concurrencyTrackingClient struct {
	client.Client
	mu      sync.Mutex
	cur     int
	maxSeen int
}

func (c *concurrencyTrackingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.mu.Lock()
	c.cur++
	if c.cur > c.maxSeen {
		c.maxSeen = c.cur
	}
	c.mu.Unlock()

	time.Sleep(5 * time.Millisecond)

	err := c.Client.Get(ctx, key, obj, opts...)

	c.mu.Lock()
	c.cur--
	c.mu.Unlock()
	return err
}

// TestBuildSessionList_BoundsGetConcurrency proves g.SetLimit(listGetConcurrency)
// is actually enforced: with more refs than the concurrency bound, the
// observed maximum concurrent Gets must never exceed it, while still proving
// the fan-out really does run concurrently (not serialized) in the first
// place.
func TestBuildSessionList_BoundsGetConcurrency(t *testing.T) {
	const n = 20
	require.Greater(t, n, listGetConcurrency, "need more refs than the concurrency bound to observe the cap")

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	var refs []spicedb.SessionRef
	var objs []client.Object
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("session-%02d", i)
		refs = append(refs, ref("demo-ns", name))
		objs = append(objs, &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: name},
			Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo-agent"},
		})
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	tracked := &concurrencyTrackingClient{Client: fakeClient}

	d := &listFixtureDeps{
		lookupResult: spicedb.InteractableSessions{Refs: refs},
		k8s:          tracked,
	}
	d.log = funcr.New(func(string, string) {}, funcr.Options{})

	rows, _, err := buildSessionList(context.Background(), d, demoSubject, nil, false)
	require.NoError(t, err)
	require.Len(t, rows, n)

	tracked.mu.Lock()
	maxSeen := tracked.maxSeen
	tracked.mu.Unlock()

	assert.LessOrEqual(t, maxSeen, listGetConcurrency,
		"the Get fan-out must never exceed listGetConcurrency in-flight requests")
	assert.Greater(t, maxSeen, 1, "the fan-out should actually run concurrently, not serialize")
}

// TestSessionsEnvelope_ShellPropsAndAPIResponseShareJSONTags proves the page's
// first-paint envelope (shellProps) and the poll's envelope
// (sessionsAPIResponse) declare IDENTICAL json tags for the three fields they
// share. The two types are independently declared (page.go / list.go), so
// nothing else stops one from drifting out of sync with the other — a rename
// on one side alone would leave the poll silently emptying the sidebar a few
// seconds after every page load, with both suites otherwise green.
func TestSessionsEnvelope_ShellPropsAndAPIResponseShareJSONTags(t *testing.T) {
	shared := []string{"Sessions", "Notices", "StartableClasses"}
	pageType := reflect.TypeOf(shellProps{})
	apiType := reflect.TypeOf(sessionsAPIResponse{})

	for _, name := range shared {
		t.Run(name, func(t *testing.T) {
			pf, ok := pageType.FieldByName(name)
			require.True(t, ok, "shellProps must declare field %s", name)
			af, ok := apiType.FieldByName(name)
			require.True(t, ok, "sessionsAPIResponse must declare field %s", name)
			assert.Equal(t, pf.Tag.Get("json"), af.Tag.Get("json"),
				"shellProps.%s and sessionsAPIResponse.%s must carry the same json tag", name, name)
		})
	}
}

// TestSessionsAPIHandler_HappyPath_ReturnsTheJoinedEnvelope is
// sessionsAPIHandler's first behavioral test: nothing previously exercised
// its 200 body, its Content-Type, or its use of the SAME buildSessionList +
// startableClassesFor call the page uses.
func TestSessionsAPIHandler_HappyPath_ReturnsTheJoinedEnvelope(t *testing.T) {
	d := newListDeps(t,
		lookupReturns(ref("demo-ns", "alpha")),
		withSession("demo-ns", "alpha", classNamed("demo-agent")),
		withClass("demo-ns", "demo-agent", "Demo Agent"),
	)

	r := httptest.NewRequest(http.MethodGet, "/sessions/api/sessions", nil)
	ctx := webui.WithSubjectForTest(context.Background(), demoSubject)
	rec := httptest.NewRecorder()

	sessionsAPIHandler(d)(rec, r.WithContext(ctx))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var got sessionsAPIResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Sessions, 1)
	assert.Equal(t, "alpha", got.Sessions[0].Name)
	assert.Equal(t, "Demo Agent", got.Sessions[0].Title)
	assert.Equal(t, listNotices{}, got.Notices)
	assert.Equal(t, []startableClass{{Ns: "demo-ns", Class: "demo-agent", Title: "Demo Agent"}}, got.StartableClasses)
}

// TestSessionsAPIHandler_LookupErrors_Returns500WithNoInternalIdentifier
// proves the handler's error path: a 500 status, a JSON body (never HTML —
// this route has no styled system-page fallback, unlike the Page route), and
// no internal identifier (a SpiceDB cause, a raw address) in that body.
func TestSessionsAPIHandler_LookupErrors_Returns500WithNoInternalIdentifier(t *testing.T) {
	d := newListDeps(t, lookupErrors(errors.New("spicedb: dial tcp 10.0.0.5:50051: connection refused")))

	r := httptest.NewRequest(http.MethodGet, "/sessions/api/sessions", nil)
	ctx := webui.WithSubjectForTest(context.Background(), demoSubject)
	rec := httptest.NewRecorder()

	sessionsAPIHandler(d)(rec, r.WithContext(ctx))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	body := rec.Body.String()
	assert.NotContains(t, body, "spicedb", "the error body must not leak the underlying cause")
	assert.NotContains(t, body, "10.0.0.5", "the error body must not leak an internal address")

	var got map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.NotEmpty(t, got["error"], "the error body must still carry SOME human-readable message")
}

// TestBuildSessionList_NonUserSubject_IsTreatedAsEmptyCanonical proves
// identity.Subject(subject).CanonicalUserID() fails closed on a subject that
// is not "user:"-prefixed, folding into the same errEmptySubject path a
// literal empty subject takes — rather than the untyped
// strings.TrimPrefix(subject, "user:") equivalent, which would have passed a
// non-user subject through unchanged as if it were a genuine canonical id.
func TestBuildSessionList_NonUserSubject_IsTreatedAsEmptyCanonical(t *testing.T) {
	d := newListDeps(t)
	_, _, err := buildSessionList(context.Background(), d, "service:not-a-user-subject", nil, false)
	require.Error(t, err)
	assert.ErrorIs(t, err, errEmptySubject)
}

// TestBootstrapArm is the start set's SECOND arm: the platform#start_session
// enumeration that exists because the derived arm cannot bootstrap. Its whole
// reason for existing is the zero-session cluster — the state in which the
// derived arm is empty for EVERY viewer, so the browser start control would be
// permanently unreachable and no first session could ever be started from a
// browser.
// classesStartable is the lookup answer for a viewer who may start each of
// the given "<ns>/<class>" pairs — what agentclass#start_session resolves to,
// through either of its arms.
func classesStartable(pairs ...string) spicedb.StartableClasses {
	out := spicedb.StartableClasses{Refs: make([]spicedb.ClassRef, 0, len(pairs))}
	for _, p := range pairs {
		ns, class, _ := strings.Cut(p, "/")
		out.Refs = append(out.Refs, spicedb.ClassRef{Namespace: ns, Name: class})
	}
	return out
}

// TestBootstrapArm is the start set's SECOND arm: the agentclass#start_session
// lookup that exists because the derived arm cannot bootstrap. Its whole
// reason for existing is the zero-session cluster — the state in which the
// derived arm is empty for EVERY viewer, so the browser start control would be
// permanently unreachable and no first session could ever be started from a
// browser.
func TestBootstrapArm(t *testing.T) {
	t.Run("a holder with NO sessions can still start: the picker lists the classes the lookup names", func(t *testing.T) {
		// No withSession at all — this is the cold-start cluster.
		d := newListDeps(t,
			withStartableNamespaces("demo-ns"),
			withClass("demo-ns", "demo-agent", "Demo Agent"),
			withClass("demo-ns", "other-agent", ""),
		)
		d.startableClasses = classesStartable("demo-ns/demo-agent", "demo-ns/other-agent")

		rows, _, err := buildSessionList(context.Background(), d, demoSubject, nil, false)
		require.NoError(t, err)
		require.Empty(t, rows, "the fixture must have no sessions — that is the state under test")

		got, notices := startableClassesFor(context.Background(), d, demoSubject, rows, listNotices{}, true)

		assert.Equal(t, []startableClass{
			{Ns: "demo-ns", Class: "demo-agent", Title: "Demo Agent", Startable: true},
			// An AgentClass with no displayName falls back to its own name,
			// matching the derived arm's resolveClassTitle.
			{Ns: "demo-ns", Class: "other-agent", Title: "other-agent", Startable: true},
		}, got)
		assert.False(t, notices.BootstrapUnavailable)
	})

	t.Run("the gate's lookup is fully consistent", func(t *testing.T) {
		// Not stylistic: the agentclass#platform link is written by the very
		// reconcile that creates the class, so a MinimizeLatency read can miss
		// it and refuse a legitimate admin who just installed an agent.
		d := newListDeps(t, withStartableNamespaces("demo-ns"), withClass("demo-ns", "demo-agent", "Demo Agent"))
		d.startableClasses = classesStartable("demo-ns/demo-agent")

		_, _ = startableClassesFor(context.Background(), d, demoSubject, nil, listNotices{}, true)

		d.mu.Lock()
		defer d.mu.Unlock()
		assert.True(t, d.gotClassLookupCalled)
		assert.True(t, d.gotClassLookupFullyConsistent, "the gate's own lookup must be fully consistent")
		assert.Equal(t, uint32(maxListedClasses), d.gotClassLookupLimit)
	})

	t.Run("the list path stays MinimizeLatency — only the gate pays for consistency", func(t *testing.T) {
		d := newListDeps(t, withStartableNamespaces("demo-ns"), withClass("demo-ns", "demo-agent", "Demo Agent"))
		d.startableClasses = classesStartable("demo-ns/demo-agent")

		_, _ = startableClassesFor(context.Background(), d, demoSubject, nil, listNotices{}, false)

		d.mu.Lock()
		defer d.mu.Unlock()
		assert.False(t, d.gotClassLookupFullyConsistent)
	})

	t.Run("a viewer the lookup names nothing for gets the derived arm alone, and that is a COMPLETE answer", func(t *testing.T) {
		d := newListDeps(t, withStartableNamespaces("demo-ns"), withClass("demo-ns", "demo-agent", "Demo Agent"))
		// Zero-value StartableClasses: an empty, non-error answer.

		got, notices := startableClassesFor(context.Background(), d, demoSubject, nil, listNotices{}, true)

		assert.Empty(t, got, "no standing and no grant means nothing to start")
		assert.False(t, notices.BootstrapUnavailable,
			"a clean 'you hold nothing' must NOT be reported as incompleteness — that would turn every "+
				"ordinary viewer's refusal into a 503")
	})

	t.Run("an indeterminate lookup reports incompleteness and never silently narrows", func(t *testing.T) {
		d := newListDeps(t, withStartableNamespaces("demo-ns"), withClass("demo-ns", "demo-agent", "Demo Agent"))
		d.startableClassesErr = errors.New("spicedb unavailable")

		got, notices := startableClassesFor(context.Background(), d, demoSubject, nil, listNotices{}, true)

		assert.Empty(t, got, "the classes really are absent — the flag is what stops that reading as a denial")
		assert.True(t, notices.BootstrapUnavailable)
	})

	t.Run("a truncated lookup is incompleteness too, not a short-but-complete picker", func(t *testing.T) {
		d := newListDeps(t, withStartableNamespaces("demo-ns"), withClass("demo-ns", "demo-agent", "Demo Agent"))
		res := classesStartable("demo-ns/demo-agent")
		res.Truncated = true
		d.startableClasses = res

		_, notices := startableClassesFor(context.Background(), d, demoSubject, nil, listNotices{}, true)

		assert.True(t, notices.BootstrapUnavailable,
			"a picker missing agents the viewer may start must say so; silently short is how a viewer "+
				"concludes an agent does not exist")
	})

	t.Run("a tuple whose AgentClass is gone does not appear — the K8s half of the join is load-bearing", func(t *testing.T) {
		// SpiceDB says the viewer may start it; Kubernetes has no such class.
		// Neither store's answer alone is the set.
		d := newListDeps(t, withStartableNamespaces("demo-ns"), withClass("demo-ns", "demo-agent", "Demo Agent"))
		d.startableClasses = classesStartable("demo-ns/demo-agent", "demo-ns/deleted-agent")

		got, _ := startableClassesFor(context.Background(), d, demoSubject, nil, listNotices{}, true)

		assert.Equal(t, []startableClass{
			{Ns: "demo-ns", Class: "demo-agent", Title: "Demo Agent", Startable: true},
		}, got)
	})

	t.Run("a class outside the startable namespaces does not appear, even when the lookup names it", func(t *testing.T) {
		// This webd cannot create in other-ns, so offering it would be a
		// control whose every press fails at the apiserver.
		d := newListDeps(t,
			withStartableNamespaces("demo-ns"),
			withClass("demo-ns", "demo-agent", "Demo Agent"),
			withClass("other-ns", "other-agent", "Other Agent"),
		)
		d.startableClasses = classesStartable("demo-ns/demo-agent", "other-ns/other-agent")

		got, _ := startableClassesFor(context.Background(), d, demoSubject, nil, listNotices{}, true)

		assert.Equal(t, []startableClass{
			{Ns: "demo-ns", Class: "demo-agent", Title: "Demo Agent", Startable: true},
		}, got)
	})

	t.Run("the arms are unioned: a holder keeps derived classes outside the startable namespaces", func(t *testing.T) {
		// "other-ns" is NOT startable, so the bootstrap arm deliberately drops
		// it — but the viewer's own session there is standing the grant must
		// not erase.
		d := newListDeps(t,
			lookupReturns(ref("other-ns", "gamma")),
			withStartableNamespaces("demo-ns"),
			withSession("other-ns", "gamma", classNamed("other-agent")),
			withClass("other-ns", "other-agent", "Other Agent"),
			withClass("demo-ns", "demo-agent", "Demo Agent"),
		)
		d.startableClasses = classesStartable("demo-ns/demo-agent")

		rows, _, err := buildSessionList(context.Background(), d, demoSubject, nil, false)
		require.NoError(t, err)

		got, _ := startableClassesFor(context.Background(), d, demoSubject, rows, listNotices{}, true)

		assert.Equal(t, []startableClass{
			{Ns: "demo-ns", Class: "demo-agent", Title: "Demo Agent", Startable: true},
			{Ns: "other-ns", Class: "other-agent", Title: "Other Agent", Startable: false},
		}, got, "the bootstrap arm adds to the derived one; it never replaces or filters it")
	})

	t.Run("no startable namespaces means no lookup at all", func(t *testing.T) {
		d := newListDeps(t, withClass("demo-ns", "demo-agent", "Demo Agent")) // no withStartableNamespaces
		d.startableClasses = classesStartable("demo-ns/demo-agent")

		got, notices := startableClassesFor(context.Background(), d, demoSubject, nil, listNotices{}, true)

		assert.Empty(t, got)
		assert.False(t, notices.BootstrapUnavailable)
		d.mu.Lock()
		defer d.mu.Unlock()
		assert.False(t, d.gotClassLookupCalled,
			"nowhere to create means nothing to enumerate; spending a SpiceDB round trip to prove it is waste")
	})

	t.Run("a class in a viewer's own workshop namespace is listed as startable, even though it is outside the static set", func(t *testing.T) {
		// "default" is this webd's only STATIC namespace, but the viewer also
		// owns a Ready workshop at "ws-1" — bound by the Workshop controller
		// (pkg/controllers/workshop.BuildWorkshopBrowserRBAC) rather than
		// configured here. The dynamic arm is what reaches it.
		d := newListDeps(t,
			withStartableNamespaces("default"),
			withWorkshopNamespaces(demoSubject, "ws-1"),
			withClass("ws-1", "ws-agent", "Workshop Agent"),
		)
		d.startableClasses = classesStartable("ws-1/ws-agent")

		got, notices := startableClassesFor(context.Background(), d, demoSubject, nil, listNotices{}, true)

		assert.Equal(t, []startableClass{
			{Ns: "ws-1", Class: "ws-agent", Title: "Workshop Agent", Startable: true},
		}, got)
		assert.False(t, notices.BootstrapUnavailable)
	})

	t.Run("the dynamic arm's lookup error is logged and does not disturb the static arm's answer", func(t *testing.T) {
		d := newListDeps(t,
			withStartableNamespaces("demo-ns"),
			withClass("demo-ns", "demo-agent", "Demo Agent"),
			withWorkshopNamespacesErr(errors.New("k8s unavailable")),
		)
		d.startableClasses = classesStartable("demo-ns/demo-agent")

		got, notices := startableClassesFor(context.Background(), d, demoSubject, nil, listNotices{}, true)

		assert.Equal(t, []startableClass{
			{Ns: "demo-ns", Class: "demo-agent", Title: "Demo Agent", Startable: true},
		}, got, "the static arm alone is still a correct, if narrower, answer")
		assert.False(t, notices.BootstrapUnavailable,
			"a workshop-lookup error is not the same incompleteness as a SpiceDB or Kubernetes failure — it narrows the dynamic arm only")
		assert.Contains(t, d.logged(), "k8s unavailable")
	})
}

// delegatedChildOf marks the fabricated session as the child half of a
// delegation, which is what the sidebar filter keys on.
func delegatedChildOf(parent string) sessionOption {
	return func(s *spiceboxv1alpha1.AgentSession) {
		s.Spec.Parent = &spiceboxv1alpha1.NamespacedRef{Namespace: s.Namespace, Name: parent}
	}
}

// TestBuildSessionList_DelegatedChildrenAreNotListed pins that a delegated
// child never reaches a human's session list, and — the half that matters —
// that it is filtered on PRESENTATION rather than by narrowing the lookup.
//
// The lookup returning the child is CORRECT: a child inherits its parent's
// started_by, so the person accountable for the root genuinely holds interact
// on every descendant, which is what makes the tree auditable and stoppable.
// This test therefore seeds the child as interactable and asserts it is dropped
// anyway. A future change that "fixes" this by removing the grant would keep
// this test green while breaking the accountability it exists to preserve, so
// the notice assertion below is the guard: a filtered child is not an
// UNAVAILABLE session, and must not be counted as one.
func TestBuildSessionList_DelegatedChildrenAreNotListed(t *testing.T) {
	d := newListDeps(t,
		lookupReturns(ref("demo-ns", "root-session"),
			ref("demo-ns", "child-a"), ref("demo-ns", "child-b")),
		withSession("demo-ns", "root-session", classNamed("demo-lead")),
		withSession("demo-ns", "child-a", classNamed("demo-helper"), delegatedChildOf("root-session")),
		withSession("demo-ns", "child-b", classNamed("demo-helper"), delegatedChildOf("root-session")),
	)

	rows, notices, err := buildSessionList(context.Background(), d, demoSubject, nil, false)
	require.NoError(t, err)

	assert.Equal(t, []string{"root-session"}, names(rows),
		"only the root is a conversation; its delegated children are machinery")
	assert.Zero(t, notices.Unavailable,
		"a delegated child is filtered, not unavailable — counting it would tell the "+
			"viewer sessions were hidden by an error when nothing failed")
}
