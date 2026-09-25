package sessions

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// The per-starter workshop cap at the start route. The operator enforces the
// same ceiling from the other side (pkg/controllers/agentsession's
// ensureWorkshop), but only AFTER the session exists: the person lands on a
// session that boot-failed and is told nothing about why. These cases pin the
// refusal that happens BEFORE anything is created, and the copy that names
// the workshops they already have open.

// workshopCapToolbox is the SidecarToolbox the cluster tier sanctions for the
// builder class in these fixtures. A made-up name: no test may reference an
// example's.
const workshopCapToolbox = "workshop-tools"

// startTestCanonicalID is startTestSubject's bare canonical id — the form
// Workshop.spec.starterCanonical carries. Derived by trimming the subject
// type prefix, which is exactly what identity.Subject.CanonicalUserID does;
// TestStartWorkshopCap asserts the two agree, so a fixture built on this
// cannot silently stop naming the person the route counts for.
var startTestCanonicalID = strings.TrimPrefix(startTestSubject, "user:")

// workshopFixture describes one seeded Workshop and the builder session whose
// phase its state is read from.
type workshopFixture struct {
	// session is the builder session's name — the name the refusal copy must
	// use, carrying the workshop's namespace whenever ANY of the listed
	// workshops sits outside the one the start is being made in, since that is
	// what workshop_close_others accepts.
	session string
	// phase is that session's status.phase.
	phase string
	// noSession seeds the Workshop with NO AgentSession behind it, the way a
	// reaped or deleted builder session leaves one.
	noSession bool
	// deleting seeds the Workshop with a deletionTimestamp (and a finalizer,
	// without which the fake client refuses to create it): a workshop on its
	// way out has stopped counting.
	deleting bool
	// starter overrides spec.starterCanonical; empty means this viewer.
	starter string
	// ns is the namespace the Workshop and its builder session live in;
	// empty means startTestNS, the namespace the start is being made in.
	ns string
}

// withWorkshops seeds Workshops and their builder sessions. A fixture that
// names no namespace lands in startTestNS — the namespace the start is being
// made in, and the ordinary case: a person's workshops are in the builder
// class's own namespace unless a second sanctioned class puts them elsewhere.
// A row that wants that second namespace names startTestOtherNS.
func withWorkshops(ws ...workshopFixture) startDepsOption {
	return func(_ *startFixtureDeps, objs *[]client.Object) {
		for _, w := range ws {
			starter := w.starter
			if starter == "" {
				starter = startTestCanonicalID
			}
			ns := w.ns
			if ns == "" {
				ns = startTestNS
			}
			meta := metav1.ObjectMeta{
				Name:      spiceboxv1alpha1.WorkshopName(w.session),
				Namespace: ns,
			}
			if w.deleting {
				now := metav1.Now()
				meta.DeletionTimestamp = &now
				meta.Finalizers = []string{"demo.test/hold"}
			}
			*objs = append(*objs, &spiceboxv1alpha1.Workshop{
				ObjectMeta: meta,
				Spec: spiceboxv1alpha1.WorkshopSpec{
					Session:          spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: w.session},
					StarterCanonical: starter,
					SidecarToolbox:   workshopCapToolbox,
				},
			})
			if w.noSession {
				continue
			}
			sess := &spiceboxv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Name: w.session, Namespace: ns},
				Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: startTestClass},
			}
			sess.Status.Phase = w.phase
			*objs = append(*objs, sess)
		}
	}
}

// withStartClassReferencingSidecar seeds a Valid class that wires the named
// SidecarToolbox in — the second half of the workshop predicate, without
// which a cluster sanction names a sidecar with nothing behind it.
func withStartClassReferencingSidecar(ns, class, toolbox string) startDepsOption {
	return func(_ *startFixtureDeps, objs *[]client.Object) {
		ac := startFixtureClass(ns, class, true)
		ac.Spec.SidecarToolboxes = []spiceboxv1alpha1.AgentClassSidecarToolboxRef{{Name: "workshop", Ref: toolbox}}
		*objs = append(*objs, ac)
	}
}

// withClusterSanction seeds the singleton ClusterAgentSettings sanctioning
// (ns, class) for the workshop, with an optional non-default ceiling.
func withClusterSanction(ns, class, toolbox string, max *int32) startDepsOption {
	return func(_ *startFixtureDeps, objs *[]client.Object) {
		refs := []spiceboxv1alpha1.BuilderClassRef{{Namespace: ns, Name: class, SidecarToolbox: toolbox}}
		*objs = append(*objs, &spiceboxv1alpha1.ClusterAgentSettings{
			ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
			Spec: spiceboxv1alpha1.SettingsSpec{Limits: &spiceboxv1alpha1.SettingsLimits{
				BuilderClasses:         &refs,
				MaxWorkshopsPerStarter: max,
			}},
		})
	}
}

// settingsGetFails makes the ClusterAgentSettings read return a non-NotFound
// error — a throttled apiserver, an RBAC edge. The cluster tier is what says
// whether this class makes a workshop at all, so an unread one is an
// indeterminate cap, never a refusal and never a start.
func settingsGetFails(err error) startDepsOption {
	return func(d *startFixtureDeps, _ *[]client.Object) { d.settingsGetErr = err }
}

// workshopListFails makes the cluster-wide Workshop List return an error.
// Counting IS the cap check, so this is the one read whose failure must not
// degrade to "none": an empty count admits a start the operator then kills.
func workshopListFails(err error) startDepsOption {
	return func(d *startFixtureDeps, _ *[]client.Object) { d.workshopListErr = err }
}

// workshopCapFixture is defaultStartFixture with a class that actually wires
// the workshop sidecar in: the viewer holds standing on demo-ns/demo-agent,
// and that class is the one the cluster tier sanctions below.
func workshopCapFixture(t *testing.T, opts ...startDepsOption) *startFixtureDeps {
	t.Helper()
	base := []startDepsOption{
		lookupAnswers(spicedb.SessionRef{Namespace: startTestNS, Name: startTestExists}),
		withStartSession(startTestNS, startTestExists, startTestClass),
		withStartClassReferencingSidecar(startTestNS, startTestClass, workshopCapToolbox),
		withLiveSessions(&fakeLiveSessions{}),
	}
	d := newStartDeps(t, append(base, opts...)...)
	grantInteract(t, d, startTestNS, startTestExists)
	return d
}

// capOf returns a pointer to an explicit ceiling.
func capOf(n int32) *int32 { return &n }

// countSessions is the baseline both halves of a row compare against: the
// fixtures seed builder sessions of their own, so "nothing was created" is a
// comparison against what this fixture actually holds, not a constant.
func countSessions(t *testing.T, c client.Client) int {
	t.Helper()
	var list spiceboxv1alpha1.AgentSessionList
	require.NoError(t, c.List(context.Background(), &list))
	return len(list.Items)
}

func TestStartWorkshopCap(t *testing.T) {
	// The fixture's starter id must be the one the route derives from the
	// subject, or every count below is against a person nobody is.
	canonical, err := identity.Subject(startTestSubject).CanonicalUserID()
	require.NoError(t, err)
	require.Equal(t, canonical.String(), startTestCanonicalID,
		"the fixture must name the starter exactly as the route canonicalizes the subject")

	cases := []struct {
		name  string
		build func(t *testing.T) *startFixtureDeps
		// wantStatus is the answer; wantMessage is asserted VERBATIM where set,
		// because the whole point of this refusal is the sentence.
		wantStatus  int
		wantMessage string
		check       func(t *testing.T, d *startFixtureDeps, rec *httptest.ResponseRecorder)
	}{
		{
			// Seeded out of alphabetical order, so a copy that merely echoed
			// the List order would not produce this sentence. Every workshop
			// is in the namespace being started in, so every name is bare:
			// that is where the builder they open will resolve a bare name.
			name: "every open workshop in the namespace being started in: 409 naming each bare, nothing created",
			build: func(t *testing.T) *startFixtureDeps {
				return workshopCapFixture(t,
					withClusterSanction(startTestNS, startTestClass, workshopCapToolbox, nil),
					withWorkshops(
						workshopFixture{session: "agent-builder-cccccccc", noSession: true},
						workshopFixture{session: "agent-builder-aaaaaaaa", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
						workshopFixture{session: "agent-builder-bbbbbbbb", phase: spiceboxv1alpha1.AgentSessionPhaseFailed},
					))
			},
			wantStatus: http.StatusConflict,
			wantMessage: "You already have the maximum number of builder workshops open at once. " +
				"Finish or close one before starting another. Open now: agent-builder-aaaaaaaa (in progress), " +
				"agent-builder-bbbbbbbb (finished), agent-builder-cccccccc (finished). " +
				"Open one of them and ask it to close the others.",
			check: func(t *testing.T, d *startFixtureDeps, _ *httptest.ResponseRecorder) {
				assert.Contains(t, d.logged(), "workshop", "the operator log must name the cause")
				// The refusal happens before browserstart.Start, so no
				// live-session slot is taken and none can leak: a reservation
				// released by a later failure is a different code path, and a
				// reservation that is never released permanently narrows what
				// this viewer may start.
				reserved, _, _ := d.live.snapshot()
				assert.Empty(t, reserved, "a capped start must not reserve a slot at all")
			},
		},
		{
			name: "a ceiling the cluster tier lowered to one: 409 at the first workshop",
			build: func(t *testing.T) *startFixtureDeps {
				return workshopCapFixture(t,
					withClusterSanction(startTestNS, startTestClass, workshopCapToolbox, capOf(1)),
					withWorkshops(workshopFixture{session: "agent-builder-aaaaaaaa", phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded}))
			},
			wantStatus: http.StatusConflict,
			wantMessage: "You already have the maximum number of builder workshops open at once. " +
				"Finish or close one before starting another. Open now: agent-builder-aaaaaaaa (finished). " +
				"Open one of them and ask it to close the others.",
		},
		{
			// The cap counts a person's workshops CLUSTER-WIDE, so the ones it
			// names can live outside the namespace this start is being made in.
			// Once ONE of them does, EVERY name carries its namespace — the two
			// at home included. The person opens one builder out of this list,
			// and a bare name resolves in whichever namespace that builder sits
			// in; qualifying only the far ones would be right for a builder
			// opened at home and wrong for one opened away, and they have not
			// chosen yet.
			//
			// Ordered by what the person reads, which is not the order the
			// session names alone would give.
			name: "one open workshop outside the namespace being started in: every entry qualified, the ones at home included",
			build: func(t *testing.T) *startFixtureDeps {
				return workshopCapFixture(t,
					withClusterSanction(startTestNS, startTestClass, workshopCapToolbox, nil),
					withWorkshops(
						workshopFixture{session: "agent-builder-bbbbbbbb", phase: spiceboxv1alpha1.AgentSessionPhaseRunning, ns: startTestOtherNS},
						workshopFixture{session: "agent-builder-cccccccc", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
						workshopFixture{session: "agent-builder-aaaaaaaa", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
					))
			},
			wantStatus: http.StatusConflict,
			wantMessage: "You already have the maximum number of builder workshops open at once. " +
				"Finish or close one before starting another. Open now: demo-ns/agent-builder-aaaaaaaa (in progress), " +
				"demo-ns/agent-builder-cccccccc (in progress), other-ns/agent-builder-bbbbbbbb (in progress). " +
				"Open one of them and ask it to close the others.",
		},
		{
			name: "one under the ceiling: the start succeeds",
			build: func(t *testing.T) *startFixtureDeps {
				return workshopCapFixture(t,
					withClusterSanction(startTestNS, startTestClass, workshopCapToolbox, nil),
					withWorkshops(
						workshopFixture{session: "agent-builder-aaaaaaaa", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
						workshopFixture{session: "agent-builder-bbbbbbbb", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
					))
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "the third workshop is being deleted: it has stopped counting, the start succeeds",
			build: func(t *testing.T) *startFixtureDeps {
				return workshopCapFixture(t,
					withClusterSanction(startTestNS, startTestClass, workshopCapToolbox, nil),
					withWorkshops(
						workshopFixture{session: "agent-builder-aaaaaaaa", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
						workshopFixture{session: "agent-builder-bbbbbbbb", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
						workshopFixture{session: "agent-builder-cccccccc", phase: spiceboxv1alpha1.AgentSessionPhaseRunning, deleting: true},
					))
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "three workshops belonging to someone else: not this person's cap, the start succeeds",
			build: func(t *testing.T) *startFixtureDeps {
				return workshopCapFixture(t,
					withClusterSanction(startTestNS, startTestClass, workshopCapToolbox, nil),
					withWorkshops(
						workshopFixture{session: "agent-builder-aaaaaaaa", phase: spiceboxv1alpha1.AgentSessionPhaseRunning, starter: "c29tZWJvZHktZWxzZQ"},
						workshopFixture{session: "agent-builder-bbbbbbbb", phase: spiceboxv1alpha1.AgentSessionPhaseRunning, starter: "c29tZWJvZHktZWxzZQ"},
						workshopFixture{session: "agent-builder-cccccccc", phase: spiceboxv1alpha1.AgentSessionPhaseRunning, starter: "c29tZWJvZHktZWxzZQ"},
					))
			},
			wantStatus: http.StatusOK,
		},
		{
			// The cap is the BUILDER's, not every agent's. A class the cluster
			// tier does not sanction creates no workshop, so a person at their
			// workshop ceiling may still start an ordinary session.
			name: "a class the cluster tier does not sanction: never counted, the start succeeds",
			build: func(t *testing.T) *startFixtureDeps {
				return workshopCapFixture(t,
					withClusterSanction(startTestNS, "some-other-agent", workshopCapToolbox, nil),
					withWorkshops(
						workshopFixture{session: "agent-builder-aaaaaaaa", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
						workshopFixture{session: "agent-builder-bbbbbbbb", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
						workshopFixture{session: "agent-builder-cccccccc", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
					))
			},
			wantStatus: http.StatusOK,
		},
		{
			// Sanctioned by name, but the class does not wire that sidecar in —
			// the operator would create no workshop for it either.
			name: "a sanctioned class that references no such sidecar: never counted, the start succeeds",
			build: func(t *testing.T) *startFixtureDeps {
				return workshopCapFixture(t,
					withClusterSanction(startTestNS, startTestClass, "some-other-toolbox", nil),
					withWorkshops(
						workshopFixture{session: "agent-builder-aaaaaaaa", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
						workshopFixture{session: "agent-builder-bbbbbbbb", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
						workshopFixture{session: "agent-builder-cccccccc", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
					))
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "no cluster tier at all: nothing is sanctioned, so nothing is capped",
			build: func(t *testing.T) *startFixtureDeps {
				return workshopCapFixture(t,
					withWorkshops(
						workshopFixture{session: "agent-builder-aaaaaaaa", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
						workshopFixture{session: "agent-builder-bbbbbbbb", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
						workshopFixture{session: "agent-builder-cccccccc", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
					))
			},
			wantStatus: http.StatusOK,
		},
		{
			// Neither direction is safe to guess: a refusal would tell a person
			// with no workshops at all that they are at a limit, and a start
			// would hand them a session the operator boot-fails moments later.
			name: "the cluster settings read fails: 503, never a refusal and never a start",
			build: func(t *testing.T) *startFixtureDeps {
				return workshopCapFixture(t,
					withClusterSanction(startTestNS, startTestClass, workshopCapToolbox, nil),
					settingsGetFails(errors.New("etcdserver: request timed out")),
					withWorkshops(workshopFixture{session: "agent-builder-aaaaaaaa", phase: spiceboxv1alpha1.AgentSessionPhaseRunning}))
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantMessage: startCheckUnavailable,
			check: func(t *testing.T, d *startFixtureDeps, _ *httptest.ResponseRecorder) {
				assert.Contains(t, d.logged(), "etcdserver",
					"the real cause must reach the log, or an operator debugs the workshop cap instead of the apiserver")
				assert.Contains(t, d.logged(), "workshopCapRefusal: get ClusterAgentSettings",
					"the log must name WHICH read failed, in this check's own words")
				assert.NotContains(t, d.logged(), "Open now",
					"a read that did not answer must not be logged as a refusal")
			},
		},
		{
			// The class read is what says whether this class makes a workshop
			// at all. Unread, the answer is neither "it does" nor "it does
			// not" — and the AgentClass exists and is Valid here, so this row
			// isolates "the read failed" from "the class is unusable", which
			// is a different answer with a different status.
			name: "the class read fails (not a NotFound): 503, never a refusal and never a start",
			build: func(t *testing.T) *startFixtureDeps {
				return workshopCapFixture(t,
					withClusterSanction(startTestNS, startTestClass, workshopCapToolbox, nil),
					classGetFails(startTestNS, startTestClass, errors.New("etcdserver: request timed out")),
					withWorkshops(workshopFixture{session: "agent-builder-aaaaaaaa", phase: spiceboxv1alpha1.AgentSessionPhaseRunning}))
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantMessage: startCheckUnavailable,
			check: func(t *testing.T, d *startFixtureDeps, _ *httptest.ResponseRecorder) {
				assert.Contains(t, d.logged(), "etcdserver", "the real cause must reach the log")
				// The WRAPPED error, not the bare word: the standing gate ahead of
				// this one logs its own dropped-row line naming the AgentClass when
				// the same Get fails for it, and an assertion that matched that
				// line would pass with this read's context stripped off entirely.
				assert.Contains(t, d.logged(), "workshopCapRefusal: get AgentClass",
					"the log must name WHICH read failed, in THIS check's own words")
			},
		},
		{
			// Counting IS the cap check, so a failed count is the one result
			// that must never be read as zero: an empty answer would let a
			// person already at their ceiling straight through, and the
			// operator would boot-fail what they started.
			name: "the workshop list fails: 503, not an empty count that admits the start",
			build: func(t *testing.T) *startFixtureDeps {
				return workshopCapFixture(t,
					withClusterSanction(startTestNS, startTestClass, workshopCapToolbox, nil),
					workshopListFails(errors.New("etcdserver: leader changed")),
					withWorkshops(
						workshopFixture{session: "agent-builder-aaaaaaaa", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
						workshopFixture{session: "agent-builder-bbbbbbbb", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
						workshopFixture{session: "agent-builder-cccccccc", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
					))
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantMessage: startCheckUnavailable,
			check: func(t *testing.T, d *startFixtureDeps, _ *httptest.ResponseRecorder) {
				assert.Contains(t, d.logged(), "leader changed", "the real cause must reach the log")
				assert.Contains(t, d.logged(), "workshopCapRefusal: list workshops",
					"the log must name WHICH read failed, in this check's own words")
			},
		},
		{
			// A workshop whose builder session cannot be read is still one the
			// person has to go and close, so it stays in the list — reported
			// as open, which is the side that keeps them looking. bbbbbbbb has
			// SUCCEEDED, so "finished" is what a successful read would have
			// said: only the fallback can produce this sentence.
			name: "a builder session that cannot be read: still listed, as in progress, and logged",
			build: func(t *testing.T) *startFixtureDeps {
				return workshopCapFixture(t,
					withClusterSanction(startTestNS, startTestClass, workshopCapToolbox, nil),
					sessionGetFails(startTestNS, "agent-builder-bbbbbbbb", errors.New("etcdserver: request timed out")),
					withWorkshops(
						workshopFixture{session: "agent-builder-aaaaaaaa", phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
						workshopFixture{session: "agent-builder-bbbbbbbb", phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded},
						workshopFixture{session: "agent-builder-cccccccc", phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded},
					))
			},
			wantStatus: http.StatusConflict,
			wantMessage: "You already have the maximum number of builder workshops open at once. " +
				"Finish or close one before starting another. Open now: agent-builder-aaaaaaaa (in progress), " +
				"agent-builder-bbbbbbbb (in progress), agent-builder-cccccccc (finished). " +
				"Open one of them and ask it to close the others.",
			check: func(t *testing.T, d *startFixtureDeps, _ *httptest.ResponseRecorder) {
				assert.Contains(t, d.logged(), "could not read a workshop's builder session",
					"a workshop reported on a failed read must say so in the log, or the state is a guess nobody can audit")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.build(t)
			baseline := countSessions(t, d.k8s)

			rec := postStart(t, d, `{"ns":"demo-ns","agentClass":"demo-agent","prompt":"build me an agent"}`)

			assert.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())
			if tc.wantMessage != "" {
				assert.Equal(t, tc.wantMessage, startErrorBody(t, rec))
			}
			if tc.wantStatus == http.StatusOK {
				assert.Equal(t, baseline+1, countSessions(t, d.k8s),
					"an admitted start must actually create the session")
			} else {
				assertNoNewObjects(t, d.k8s, baseline)
			}
			if tc.check != nil {
				tc.check(t, d, rec)
			}
		})
	}
}
