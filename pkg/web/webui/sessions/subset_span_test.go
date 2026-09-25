package sessions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/agentui"
)

// This file exists for ONE claim, made in prose at pkg/web/webui/agentui's start
// handler and relied on by the whole authorization moment of this plan:
//
//	the ended-session start route's gate admits a STRICT SUBSET of what the
//	dashboard start route admits.
//
// The claim is not a property of either package. It is a relationship between
// them, resting on three facts in two packages:
//
//  1. startableClassesFor filters on nothing — every class the viewer holds an
//     interactable session of is startable, INCLUDING an ended one;
//  2. both gates ask about the same subject and the same (namespace, session)
//     pair, so a witness for one is a witness for the other;
//  3. both are recomputed at request time, fully consistently.
//
// Neither package's own suite can see that relationship, which is the shape
// this branch has been bitten by repeatedly: an invariant that is true,
// documented, and unowned. The concrete regression it guards against is one
// plausible line — "skip rows whose Ended is true", added to stop the dashboard
// offering dead controls — which every existing test survives, and which
// silently makes the agent-UI route BROADER than the dashboard while the
// comment claiming otherwise stays put.
//
// pkg/web/webui/sessions is the package that may import both, and its
// *startFixtureDeps already satisfies agentui.Deps (viewFor casts to it), so
// one fixture can drive both routes — which is itself part of fact 2: a single
// value cannot answer CheckInteract two different ways for two callers.

// spanEndedSession is the ONE session the viewer holds standing on in these
// tests, and it is ENDED — the state the agent-UI control exists for, and the
// one a filter would most plausibly drop.
const spanEndedSession = "demo-ended"

// endedFixtureSession is startFixtureSession in a terminal phase.
func endedFixtureSession(ns, name, class string) *spiceboxv1alpha1.AgentSession {
	s := startFixtureSession(ns, name, class)
	s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
	return s
}

// agentUIStartHandler finds the ended-session start route's handler on the
// SAME deps value the dashboard route is served from. Found by pattern rather
// than constructed, so the route this asserts about is the one that mounts.
func agentUIStartHandler(t *testing.T, d *startFixtureDeps) http.Handler {
	t.Helper()
	for _, rt := range agentui.New().Routes(d) {
		if rt.Pattern == "/agent-ui/{ns}/{name}/start" {
			require.NotNil(t, rt.Handler)
			return rt.Handler
		}
	}
	t.Fatal("the agent-UI start route is not mounted for this fixture; the subset claim has nothing to be about")
	return nil
}

// postAgentUIStart drives that route for one (ns, name).
func postAgentUIStart(t *testing.T, d *startFixtureDeps, ns, name string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/agent-ui/"+ns+"/"+name+"/start",
		strings.NewReader(`{"prompt":"pick up where we left off"}`))
	r.Header.Set("Origin", startTestOrigin)
	r.SetPathValue("ns", ns)
	r.SetPathValue("name", name)
	r = r.WithContext(webui.WithSubjectForTest(context.Background(), startTestSubject))
	rec := httptest.NewRecorder()
	agentUIStartHandler(t, d).ServeHTTP(rec, r)
	return rec
}

// spanFixture: the viewer holds interact on exactly ONE session — an ENDED one
// of startTestClass in startTestNS — and nothing else.
func spanFixture(t *testing.T) *startFixtureDeps {
	t.Helper()
	d := newStartDeps(t,
		lookupAnswers(spicedb.SessionRef{Namespace: startTestNS, Name: spanEndedSession}),
		withStartClass(startTestNS, startTestClass, true),
		withLiveSessions(&fakeLiveSessions{}),
		func(_ *startFixtureDeps, objs *[]client.Object) {
			*objs = append(*objs, endedFixtureSession(startTestNS, spanEndedSession, startTestClass))
		},
	)
	grantInteract(t, d, startTestNS, spanEndedSession)
	return d
}

// TestSubsetSpan_AnEndedSessionIsAWitnessForBothRoutes is the positive
// direction: the ONE session the viewer holds standing on is ended, and both
// routes must accept it — the dashboard by listing its class as startable, the
// agent-UI route by starting a replacement for it.
//
// Assertion (a) is the one a filter added to startableClassesFor breaks, and
// its message names the argument so the next reader knows what they invalidated
// rather than only that a test went red.
func TestSubsetSpan_AnEndedSessionIsAWitnessForBothRoutes(t *testing.T) {
	d := spanFixture(t)

	// (a) The DASHBOARD route's own derivation. Driven through
	// authorizedStartableClasses — the function the start gate calls — not
	// through startableClassesFor directly, so a filter added at either layer
	// fails here.
	allowed, notices, err := authorizedStartableClasses(context.Background(), d, startTestSubject)
	require.NoError(t, err)
	require.Zero(t, notices.Unavailable, "fixture precondition: the standing list must be complete")
	assert.True(t, containsStartableClass(allowed, startTestNS, startTestClass),
		"an ENDED session must still make its class startable. The agent-UI start route's gate is documented as a "+
			"STRICT SUBSET of this set — it admits a viewer who holds interact on one specific session of the class. "+
			"Filtering ended rows out here does not narrow that route; it makes it BROADER than this one, and turns "+
			"the subset argument at pkg/web/webui/agentui's startHandler into a false comment in the authorization "+
			"moment of this feature.")

	// (b) The AGENT-UI route, on the same fixture and the same subject.
	rec := postAgentUIStart(t, d, startTestNS, spanEndedSession)
	require.Equal(t, http.StatusOK, rec.Code,
		"the same standing that makes the class startable above must let the ended-session control start one; body: %s",
		rec.Body.String())

	var got struct {
		Ns   string `json:"ns"`
		Name string `json:"name"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, startTestNS, got.Ns)
	assert.NotEqual(t, spanEndedSession, got.Name, "a REPLACEMENT session, not the ended one")

	// Fact 2, observed rather than assumed: the agent-UI gate asked about the
	// session the URL named, under the same subject the dashboard derivation
	// used. A gate that asked about something else could admit where the
	// dashboard denies, whatever either package's own tests say.
	d.mu.Lock()
	seen := append([]interactCall(nil), d.interactSeen...)
	lookups := append([]lookupCall(nil), d.lookupCalls...)
	d.mu.Unlock()

	var asked bool
	for _, c := range seen {
		if c.ns == startTestNS && c.name == spanEndedSession && c.subject == startTestSubject {
			asked = true
		}
	}
	assert.True(t, asked,
		"the agent-UI gate must ask about the (namespace, session) the URL names, under the viewer's own subject")

	// Fact 3's half that lives in this package: the dashboard's gate recomputes
	// FULLY CONSISTENTLY. (The agent-UI gate's consistency is bound where its
	// CheckInteract is implemented — internal/cmd/webd — and is pinned there.)
	require.NotEmpty(t, lookups)
	assert.True(t, lookups[len(lookups)-1].fullyConsistent,
		"the start gate must recompute fully consistently, or a just-revoked subject is admitted")
}

// TestSubsetSpan_NoStandingIsRefusedByBoth is the converse, and the reason the
// positive direction alone would prove little: a fixture where BOTH routes
// admit everything would pass the test above.
func TestSubsetSpan_NoStandingIsRefusedByBoth(t *testing.T) {
	d := spanFixture(t)

	// A second ended session of a DIFFERENT class, present in Kubernetes, that
	// the viewer holds no interact on and the lookup never returns.
	const strangerSession = "demo-stranger"
	const strangerClass = "other-agent"
	require.NoError(t, d.k8s.Create(context.Background(),
		endedFixtureSession(startTestNS, strangerSession, strangerClass)))
	require.NoError(t, d.k8s.Create(context.Background(),
		startFixtureClass(startTestNS, strangerClass, true)))

	// (a) The dashboard does not offer it.
	allowed, _, err := authorizedStartableClasses(context.Background(), d, startTestSubject)
	require.NoError(t, err)
	assert.False(t, containsStartableClass(allowed, startTestNS, strangerClass),
		"a class the viewer holds no interactable session of must not be startable")

	// (b) Nor does the agent-UI route, for the same reason: no interact on the
	// session it would read the class from.
	rec := postAgentUIStart(t, d, startTestNS, strangerSession)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"the subset claim is only meaningful if the narrower gate actually refuses; body: %s", rec.Body.String())

	// And nothing was created by the refusal.
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, d.k8s.List(context.Background(), &sessions))
	assert.Len(t, sessions.Items, 2, "a refused start must write nothing (the two seeded sessions only)")
}
