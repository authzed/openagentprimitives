package workshopmcp

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// builderWorkshopKey mirrors newTestRunServer's fixed builder identity
// (b/x): the Workshop CR watch_test/test_sessions read/write always lives
// at this key, {SessionNamespace, WorkshopName(SessionName)}.
func builderWorkshopKey() client.ObjectKey {
	return client.ObjectKey{Namespace: "b", Name: spiceboxv1alpha1.WorkshopName("x")}
}

// TestTestLink_LabelsPerPersonAgents proves a userPassthrough class gets the
// "as yourself" label — the class runs as the person testing it.
func TestTestLink_LabelsPerPersonAgents(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ws-demo123", Name: "demo-agent"},
		Spec:       spiceboxv1alpha1.AgentClassSpec{IdentityMode: spiceboxv1alpha1.IdentityModeUserPassthrough},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ac).Build()
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleTestLink, testLinkArgs{Class: "demo-agent", Prompt: "help me plan a trip"})
	require.False(t, res.IsError, "a class in this workshop must not be a tool error")
	body := decodeResultBody(t, res)
	assert.Equal(t, "ws-demo123", body["namespace"])
	assert.Equal(t, "demo-agent", body["agentClass"])
	assert.Equal(t, "help me plan a trip", body["prompt"], "the prompt is echoed back verbatim")
	assert.Equal(t, tryLabelAsYourself, body["label"])
}

// TestTestLink_LabelsAgentIdentityAgents proves the default identityMode
// (agent) gets the plain "Try it" label — the class runs as itself, not as
// the person testing it.
func TestTestLink_LabelsAgentIdentityAgents(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ws-demo123", Name: "demo-agent"},
		Spec:       spiceboxv1alpha1.AgentClassSpec{IdentityMode: spiceboxv1alpha1.IdentityModeAgent},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ac).Build()
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleTestLink, testLinkArgs{Class: "demo-agent", Prompt: "help me plan a trip"})
	require.False(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Equal(t, tryLabel, body["label"])
}

// TestTestLink_RefusesAClassNotInTheWorkshop proves test_link never hands
// back a link to nowhere: a class the builder has not (yet) authored in
// this workshop is a tool error naming it.
func TestTestLink_RefusesAClassNotInTheWorkshop(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleTestLink, testLinkArgs{Class: "no-such-agent", Prompt: "hello"})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "no-such-agent")
}

// TestTestLink_RequiresClassAndPrompt pins the argument guard.
func TestTestLink_RequiresClassAndPrompt(t *testing.T) {
	s := newTestRunServer("ws-demo123", fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build())
	res := callTool(t, s.handleTestLink, testLinkArgs{})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "class and prompt are both required")
}

// TestWatchTest_WritesTheWatchOnTheWorkshop is watch_test's happy path: it
// writes spec.testWatch onto the BUILDER session's own Workshop CR (at
// SessionNamespace/WorkshopName(SessionName) — "b"/"x-workshop" for
// newTestRunServer — never the workshop namespace W itself), and returns a
// "watching" status with the same deadline.
func TestWatchTest_WritesTheWatchOnTheWorkshop(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Namespace: "ws-demo123", Name: "demo-agent"}}
	ws := &spiceboxv1alpha1.Workshop{ObjectMeta: builderWorkshopKeyMeta()}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ac, ws).Build()
	s := newTestRunServer("ws-demo123", c)

	before := time.Now().UTC()
	res := callTool(t, s.handleWatchTest, watchTestArgs{Class: "demo-agent", TimeoutMinutes: 45})
	require.False(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Equal(t, "demo-agent", body["class"])
	assert.Equal(t, "watching", body["status"])

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), builderWorkshopKey(), &got))
	require.NotNil(t, got.Spec.TestWatch, "watch_test must write spec.testWatch")
	assert.Equal(t, "demo-agent", got.Spec.TestWatch.Class)
	assert.WithinDuration(t, before, got.Spec.TestWatch.StartedAt.Time, 5*time.Second)
	assert.WithinDuration(t, before.Add(45*time.Minute), got.Spec.TestWatch.Deadline.Time, 5*time.Second)

	deadlineStr, ok := body["deadline"].(string)
	require.True(t, ok, "deadline must be a string")
	parsed, err := time.Parse(time.RFC3339, deadlineStr)
	require.NoError(t, err, "deadline must be RFC3339")
	assert.WithinDuration(t, before.Add(45*time.Minute), parsed, 5*time.Second)
}

// TestWatchTest_DefaultsAndCapsTheTimeout proves the two timeoutMinutes
// boundary rules: omitted (zero) defaults to 30m, and anything above the
// 120m ceiling is capped rather than honored verbatim.
func TestWatchTest_DefaultsAndCapsTheTimeout(t *testing.T) {
	cases := []struct {
		name    string
		minutes int
		want    time.Duration
	}{
		{name: "omitted timeoutMinutes defaults to 30m", minutes: 0, want: 30 * time.Minute},
		{name: "500m is capped at the 120m ceiling", minutes: 500, want: 120 * time.Minute},
		{name: "an absurdly large timeoutMinutes clamps to the 120m ceiling, not an overflowed duration", minutes: math.MaxInt, want: 120 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Namespace: "ws-demo123", Name: "demo-agent"}}
			ws := &spiceboxv1alpha1.Workshop{ObjectMeta: builderWorkshopKeyMeta()}
			c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ac, ws).Build()
			s := newTestRunServer("ws-demo123", c)

			before := time.Now().UTC()
			res := callTool(t, s.handleWatchTest, watchTestArgs{Class: "demo-agent", TimeoutMinutes: tc.minutes})
			require.False(t, res.IsError)

			var got spiceboxv1alpha1.Workshop
			require.NoError(t, c.Get(context.Background(), builderWorkshopKey(), &got))
			require.NotNil(t, got.Spec.TestWatch)
			assert.WithinDuration(t, before.Add(tc.want), got.Spec.TestWatch.Deadline.Time, 5*time.Second)
		})
	}
}

// TestWatchTest_ReplacesAnExistingWatch proves a new watch_test call
// entirely REPLACES a previous watch — a different class, and a fresh
// startedAt/deadline — rather than merging with or being refused by one
// already recorded.
func TestWatchTest_ReplacesAnExistingWatch(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Namespace: "ws-demo123", Name: "demo-agent"}}
	ws := &spiceboxv1alpha1.Workshop{
		ObjectMeta: builderWorkshopKeyMeta(),
		Spec: spiceboxv1alpha1.WorkshopSpec{
			TestWatch: &spiceboxv1alpha1.WorkshopTestWatch{
				Class:     "some-other-agent",
				StartedAt: metav1.NewTime(time.Now().Add(-time.Hour)),
				Deadline:  metav1.NewTime(time.Now().Add(-time.Minute)),
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ac, ws).Build()
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleWatchTest, watchTestArgs{Class: "demo-agent"})
	require.False(t, res.IsError)

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), builderWorkshopKey(), &got))
	require.NotNil(t, got.Spec.TestWatch)
	assert.Equal(t, "demo-agent", got.Spec.TestWatch.Class, "the new watch entirely replaces the old one")
	assert.True(t, got.Spec.TestWatch.StartedAt.Time.After(time.Now().Add(-time.Minute)),
		"the replaced watch's startedAt must be fresh, not inherited from the stale one")
}

// TestWatchTest_KeepsTheWatchIdentityWhileItsTestIsStillRunning is the
// "Keep watching" button's correctness, which a fresh startedAt silently
// broke. The controller identifies a watch by (class, startedAt) and skips
// every session created BEFORE startedAt (qualifyingTestSession,
// pkg/controllers/workshop/testwatch.go), so re-arming with the current time
// arms a watch that can never see the test the person is still running. The
// handler keeps the identity exactly while the recorded session is still
// there and has not finished, and the result's "keptWatching" says which
// happened — named for the lease it reports, which is not the watcher's own
// event about a person coming back to a paused test.
func TestWatchTest_KeepsTheWatchIdentityWhileItsTestIsStillRunning(t *testing.T) {
	// Truncated to the second: a metav1.Time survives the fake client's JSON
	// round trip only at RFC3339 precision, exactly as it survives a real
	// apiserver, so an unrounded fixture would compare unequal to the value
	// the handler read back and re-wrote.
	started := time.Now().UTC().Truncate(time.Second).Add(-40 * time.Minute)

	cases := []struct {
		name string
		// specClass/recorded describe the watch already on the workshop; an
		// empty recorded means status.testWatch recorded no session yet.
		specClass        string
		recorded         string
		sessionName      string // "" means no session object exists at all
		phase            string
		wantKeptWatching bool
	}{
		{
			name:      "the recorded test is still running: keeps startedAt, extends the deadline",
			specClass: "demo-agent", recorded: "try-1", sessionName: "try-1",
			phase: spiceboxv1alpha1.AgentSessionPhaseRunning, wantKeptWatching: true,
		},
		{
			name:      "the recorded test is paused: still this test, so it keeps startedAt",
			specClass: "demo-agent", recorded: "try-1", sessionName: "try-1",
			phase: spiceboxv1alpha1.AgentSessionPhaseIdle, wantKeptWatching: true,
		},
		{
			name:      "the recorded test finished: a new watch, so a fresh startedAt",
			specClass: "demo-agent", recorded: "try-1", sessionName: "try-1",
			phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded, wantKeptWatching: false,
		},
		{
			name:      "the recorded test stopped with an error: a new watch, so a fresh startedAt",
			specClass: "demo-agent", recorded: "try-1", sessionName: "try-1",
			phase: spiceboxv1alpha1.AgentSessionPhaseFailed, wantKeptWatching: false,
		},
		{
			name:      "the recorded test is gone (stop_test deleted it): a new watch, so a fresh startedAt",
			specClass: "demo-agent", recorded: "try-1", sessionName: "",
			wantKeptWatching: false,
		},
		{
			name:      "no session was ever recorded: nothing to keep watching, so a fresh startedAt",
			specClass: "demo-agent", recorded: "", sessionName: "",
			wantKeptWatching: false,
		},
		{
			name:      "the watch was for another agent: a different test, so a fresh startedAt",
			specClass: "some-other-agent", recorded: "try-1", sessionName: "try-1",
			phase: spiceboxv1alpha1.AgentSessionPhaseRunning, wantKeptWatching: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Namespace: "ws-demo123", Name: "demo-agent"}}
			ws := &spiceboxv1alpha1.Workshop{
				ObjectMeta: builderWorkshopKeyMeta(),
				Spec: spiceboxv1alpha1.WorkshopSpec{
					TestWatch: &spiceboxv1alpha1.WorkshopTestWatch{
						Class:     tc.specClass,
						StartedAt: metav1.NewTime(started),
						Deadline:  metav1.NewTime(started.Add(30 * time.Minute)),
					},
				},
				Status: spiceboxv1alpha1.WorkshopStatus{
					TestWatch: &spiceboxv1alpha1.WorkshopTestWatchStatus{
						Class:     tc.specClass,
						StartedAt: metav1.NewTime(started),
						Session:   tc.recorded,
						Delivered: []string{"started", "timedOut"},
					},
				},
			}
			objs := []client.Object{ac, ws}
			if tc.sessionName != "" {
				objs = append(objs, &spiceboxv1alpha1.AgentSession{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ws-demo123", Name: tc.sessionName},
					Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo-agent"},
					Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: tc.phase},
				})
			}
			c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(objs...).Build()
			s := newTestRunServer("ws-demo123", c)

			before := time.Now().UTC()
			res := callTool(t, s.handleWatchTest, watchTestArgs{Class: "demo-agent", TimeoutMinutes: 20})
			require.False(t, res.IsError)
			body := decodeResultBody(t, res)
			assert.Equal(t, tc.wantKeptWatching, body["keptWatching"], "the builder is told whether it kept the old watch")

			var got spiceboxv1alpha1.Workshop
			require.NoError(t, c.Get(context.Background(), builderWorkshopKey(), &got))
			require.NotNil(t, got.Spec.TestWatch)
			assert.Equal(t, "demo-agent", got.Spec.TestWatch.Class)
			if tc.wantKeptWatching {
				assert.True(t, got.Spec.TestWatch.StartedAt.Time.Equal(started),
					"a kept watch keeps its identity, or the controller stops seeing the running test")
			} else {
				assert.True(t, got.Spec.TestWatch.StartedAt.Time.After(before.Add(-time.Minute)),
					"a new watch stamps a fresh startedAt")
			}
			assert.WithinDuration(t, before.Add(20*time.Minute), got.Spec.TestWatch.Deadline.Time, 5*time.Second,
				"either way the deadline moves forward by the requested timeout")
		})
	}
}

// TestWatchTest_RefusesAClassNotInTheWorkshop mirrors test_link's own guard:
// watch_test must not record a watch for a class that does not exist here.
func TestWatchTest_RefusesAClassNotInTheWorkshop(t *testing.T) {
	ws := &spiceboxv1alpha1.Workshop{ObjectMeta: builderWorkshopKeyMeta()}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ws).Build()
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleWatchTest, watchTestArgs{Class: "no-such-agent"})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "no-such-agent")
}

// TestWatchTest_RequiresClass pins the argument guard.
func TestWatchTest_RequiresClass(t *testing.T) {
	s := newTestRunServer("ws-demo123", fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build())
	res := callTool(t, s.handleWatchTest, watchTestArgs{})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "class is required")
}

// TestWatchTest_DeniedUpdate_SurfacesVerbatim proves an apiserver/webhook
// refusal of the Workshop CR update surfaces verbatim (deniedResult), the
// same discipline every other Workshop-CR-writing tool follows.
func TestWatchTest_DeniedUpdate_SurfacesVerbatim(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Namespace: "ws-demo123", Name: "demo-agent"}}
	ws := &spiceboxv1alpha1.Workshop{ObjectMeta: builderWorkshopKeyMeta()}
	denyErr := apierrors.NewForbidden(
		schema.GroupResource{Group: spiceboxv1alpha1.GroupName, Resource: "workshops"},
		ws.Name, errors.New("workshop policy forbids this write"))
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ac, ws).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				return denyErr
			},
		}).Build()
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleWatchTest, watchTestArgs{Class: "demo-agent"})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Equal(t, true, body["denied"])
	assert.Contains(t, body["message"], "workshop policy forbids this write")
}

// startedByAnnotation is the shorthand this file's fixtures use to attribute
// an AgentSession to a canonical starter, matching
// spiceboxv1alpha1.AnnotationStartedByCanonicalID's prefixed-subject form.
func startedByAnnotation(canonical string) map[string]string {
	return map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:" + canonical}
}

// TestTestSessions_ListsTheOwnersSessionsOfThatClass proves the (class,
// starter) filter: of three sessions in the workshop namespace, only the one
// matching BOTH the requested class and this workshop's own starter
// qualifies.
func TestTestSessions_ListsTheOwnersSessionsOfThatClass(t *testing.T) {
	ws := &spiceboxv1alpha1.Workshop{
		ObjectMeta: builderWorkshopKeyMeta(),
		Spec:       spiceboxv1alpha1.WorkshopSpec{StarterCanonical: "alice"},
	}
	qualifying := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "ws-demo123",
			Name:              "test-session-1",
			Annotations:       startedByAnnotation("alice"),
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "demo-agent"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:    "Running",
			Progress: &spiceboxv1alpha1.AgentSessionProgress{TurnCount: 3},
		},
	}
	otherClass := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "ws-demo123",
			Name:        "other-class-session",
			Annotations: startedByAnnotation("alice"),
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "other-agent"},
	}
	otherStarter := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "ws-demo123",
			Name:        "other-starter-session",
			Annotations: startedByAnnotation("bob"),
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "demo-agent"},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithObjects(ws, qualifying, otherClass, otherStarter).Build()
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleTestSessions, testSessionsArgs{Class: "demo-agent"})
	require.False(t, res.IsError)
	body := decodeResultBody(t, res)
	sessions, ok := body["sessions"].([]any)
	require.True(t, ok, "sessions must be a list")
	require.Len(t, sessions, 1, "only the session matching BOTH class and starter qualifies")
	entry, ok := sessions[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "test-session-1", entry["name"])
	// The builder paints an ap:chat for this session, and ChatProps.SessionRef
	// is "<namespace>/<name>" — a name alone cannot address a session — so the
	// namespace comes back beside it, and ref is the two already joined, which
	// is the form the skill can name (its own vocabulary guard bans the word
	// "namespace" from every skill body).
	assert.Equal(t, "ws-demo123", entry["namespace"])
	assert.Equal(t, "ws-demo123/test-session-1", entry["ref"])
	assert.Equal(t, "Running", entry["phase"])
	assert.Equal(t, float64(3), entry["turnCount"], "turnCount decodes as a JSON number (float64) in a map[string]any")
	assert.NotEmpty(t, entry["started"], "started must be filled")
}

// TestTestSessions_EmptyStarterCanonical_FailsClosedToEmptyList proves the
// guard mirrored from pkg/controllers/workshop/testwatch.go's
// qualifyingTestSession: an empty (absent) spec.starterCanonical must never
// let an unattributed session compare equal to it — this asserts a session
// with NO started-by annotation would otherwise match, and must not.
func TestTestSessions_EmptyStarterCanonical_FailsClosedToEmptyList(t *testing.T) {
	ws := &spiceboxv1alpha1.Workshop{ObjectMeta: builderWorkshopKeyMeta()}
	unattributed := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ws-demo123", Name: "kubectl-driven-session"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo-agent"},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ws, unattributed).Build()
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleTestSessions, testSessionsArgs{Class: "demo-agent"})
	require.False(t, res.IsError)

	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	assert.JSONEq(t, `{"sessions":[]}`, text.Text,
		"an empty starterCanonical must fail closed to an empty list, not match an unattributed session")
}

// TestTestSessions_NoQualifyingSession_ReturnsEmptyListNotNull proves the
// empty case is always [], never a JSON null, regardless of the reason.
func TestTestSessions_NoQualifyingSession_ReturnsEmptyListNotNull(t *testing.T) {
	ws := &spiceboxv1alpha1.Workshop{
		ObjectMeta: builderWorkshopKeyMeta(),
		Spec:       spiceboxv1alpha1.WorkshopSpec{StarterCanonical: "alice"},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ws).Build()
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleTestSessions, testSessionsArgs{Class: "demo-agent"})
	require.False(t, res.IsError)
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	assert.JSONEq(t, `{"sessions":[]}`, text.Text)
}

// TestTestSessions_RequiresClass pins the argument guard.
func TestTestSessions_RequiresClass(t *testing.T) {
	s := newTestRunServer("ws-demo123", fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build())
	res := callTool(t, s.handleTestSessions, testSessionsArgs{})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "class is required")
}

// TestStopTest_DeletesTheSessionInTheWorkshop proves stop_test deletes the
// named AgentSession — the person's own root test session, found via
// test_sessions — not a SubagentRequest: the object must actually be gone
// afterward.
func TestStopTest_DeletesTheSessionInTheWorkshop(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ws-demo123", Name: "test-session-1"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo-agent"},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sess).Build()
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleStopTest, stopTestArgs{Session: "test-session-1"})
	require.False(t, res.IsError, "stopping a running test must not fail")
	body := decodeResultBody(t, res)
	assert.Equal(t, "test-session-1", body["session"])
	assert.Equal(t, "stopped", body["status"])

	var check spiceboxv1alpha1.AgentSession
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "ws-demo123", Name: "test-session-1"}, &check)
	assert.True(t, apierrors.IsNotFound(err), "the AgentSession must actually be gone after stop_test")
}

// TestStopTest_AlreadyGone proves stopping a test that has already ended —
// the person already ended it, or a previous stop_test call already
// succeeded — is reported as success, not an error.
func TestStopTest_AlreadyGone(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleStopTest, stopTestArgs{Session: "never-existed"})
	require.False(t, res.IsError, "a NotFound delete must be reported as idempotent success, not a tool error")
	body := decodeResultBody(t, res)
	assert.Equal(t, "never-existed", body["session"])
	assert.Equal(t, "already stopped", body["status"])
}

// TestStopTest_DeleteError_SurfacesAsToolError proves a genuine
// (non-NotFound) delete failure surfaces as a tool error, and leaves the
// session untouched, rather than being mistaken for the idempotent
// already-stopped case.
func TestStopTest_DeleteError_SurfacesAsToolError(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "ws-demo123", Name: "test-session-1"}}
	deleteErr := apierrors.NewForbidden(
		schema.GroupResource{Group: spiceboxv1alpha1.GroupName, Resource: "agentsessions"},
		"test-session-1", errors.New("workshop policy forbids stopping this test"))
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sess).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				return deleteErr
			},
		}).Build()
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleStopTest, stopTestArgs{Session: "test-session-1"})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "workshop policy forbids stopping this test")

	var check spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "ws-demo123", Name: "test-session-1"}, &check),
		"a denied/errored delete must leave the session untouched")
}

// TestStopTest_RequiresSession pins the argument guard.
func TestStopTest_RequiresSession(t *testing.T) {
	s := newTestRunServer("ws-demo123", fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build())
	res := callTool(t, s.handleStopTest, stopTestArgs{})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "session is required")
}

// TestRunTest_IsNoLongerAnnounced proves run_test is gone from the served
// tool surface — replaced by test_link/watch_test/test_sessions, with
// read_test_log and stop_test retargeted to the person's own session —
// registered through the SAME Register a real sidecar process calls.
func TestRunTest_IsNoLongerAnnounced(t *testing.T) {
	s := newTestWorkshopServer(t)
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "ap-workshop", Version: "0"}, nil)
	s.Register(mcpSrv)

	srv := newTestMux(t, mcpSrv)
	names := listToolNames(t, srv)

	assert.Contains(t, names, toolTestLink)
	assert.Contains(t, names, toolWatchTest)
	assert.Contains(t, names, toolTestSessions)
	assert.Contains(t, names, toolReadTestLog)
	assert.Contains(t, names, toolStopTest)
	assert.NotContains(t, names, "run_test", "run_test must no longer be announced")
}

// builderWorkshopKeyMeta returns the ObjectMeta fields (as a plain struct
// literal, so callers can embed it directly) naming the builder session's
// own Workshop CR — {SessionNamespace, WorkshopName(SessionName)} — for
// newTestRunServer's fixed "b"/"x" builder identity.
func builderWorkshopKeyMeta() metav1.ObjectMeta {
	key := builderWorkshopKey()
	return metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}
}
