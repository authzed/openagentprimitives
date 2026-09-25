//go:build integration

// Integration tests for the workshop transcript route against a REAL
// apiserver (envtest) and a REAL SpiceDB (testspicedb), covering both arms
// the route authorizes on: the `parent` relationship a delegation writes,
// and the workshop's own recorded owner for a root session the person
// started themselves.
// workshoptranscriptsrv_test.go's fake ReadTranscriptChecker proves the ROUTE
// re-checks a permission rather than trusting the bearer; it says nothing
// about whether CheckReadTranscriptForSession itself, against a live
// backend, answers that check correctly — nor whether the `+ parent` schema
// arm (plan 4a Task 3) and TouchLineage (the delegation controller's own
// write) actually connect end to end. These tests wire the real
// (*pkg/authz/spicedb.Client).CheckReadTranscriptForSession: TouchLineage
// really writes the child->parent tuple the route's permission resolves
// through, the route really reads it back, and the no-tuple case proves the
// refusal holds against the real authorization backend, not a stand-in for
// it. Mirrors pkg/web/workshopdraftsrv's integration test shape.
package workshoptranscriptsrv_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	lifecyclekind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/web/workshoptranscriptsrv"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

func TestMain(m *testing.M) {
	code := testenv.RunPackage(m)
	// These tests boot a shared SpiceDB container; stop it after the
	// apiserver so a package run leaves no container behind — mirrors
	// pkg/web/workshopdraftsrv's integration test.
	testspicedb.StopShared()
	os.Exit(code)
}

func newIntegrationSpiceDBClient(t *testing.T) *spicedb.Client {
	t.Helper()
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	c, err := spicedb.NewClient(endpoint, token, true /* insecure */)
	require.NoError(t, err, "spicedb.NewClient")
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func integrationWorkshop(sessNS, sessName, workshopNS string) *spiceboxv1alpha1.Workshop {
	return &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spiceboxv1alpha1.WorkshopName(sessName),
			Namespace: sessNS,
		},
		Spec: spiceboxv1alpha1.WorkshopSpec{
			Session:        spiceboxv1alpha1.NamespacedRef{Namespace: sessNS, Name: sessName},
			SidecarToolbox: "workshop",
			Limits: spiceboxv1alpha1.WorkshopLimits{
				MaxAge:              metav1.Duration{Duration: time.Hour},
				MaxObjectsPerKind:   10,
				MaxObjects:          50,
				MaxConcurrentProbes: 2,
			},
		},
		Status: spiceboxv1alpha1.WorkshopStatus{
			Namespace: workshopNS,
			Phase:     spiceboxv1alpha1.WorkshopPhaseReady,
		},
	}
}

// ensureNamespace creates the real Namespace object a workshop's child
// AgentSession is about to live in — envtest starts with only "default"
// pre-created, and the workshop namespace W is a synthetic per-workshop
// namespace no fixture here has a reason to reuse.
func ensureNamespace(t *testing.T, env *testenv.Env, ns string) {
	t.Helper()
	if ns == "default" {
		return
	}
	err := env.Client.Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	})
	require.NoError(t, client.IgnoreAlreadyExists(err), "create namespace %s", ns)
}

func integrationChild(workshopNS, childName string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: workshopNS, Name: childName},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "demo-class",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "test the built agent"},
		},
	}
}

func getTranscriptIntegration(t *testing.T, srvURL, token, child string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srvURL+workshoptranscriptsrv.Path+child, nil)
	require.NoError(t, err, "NewRequest")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Do")
	return resp
}

// transcriptResponse (decodeTranscript's shape) is declared once in
// workshoptranscriptsrv_test.go; both files are the SAME package, so an
// integration run (which compiles every file in the directory together)
// would fail to build a second declaration here.

// TestIntegration_ParentTupleWritten_ReturnsChildTurns proves the happy path
// end to end: the delegation controller's own write (TouchLineage) is what
// the route's permission check resolves through, against a real SpiceDB —
// not a fake standing in for the `+ parent` schema arm.
func TestIntegration_ParentTupleWritten_ReturnsChildTurns(t *testing.T) {
	env := testenv.Shared(t)
	spdb := newIntegrationSpiceDBClient(t)
	ctx := context.Background()

	const (
		sessNS     = "default"
		sessName   = "builder-int-ok"
		workshopNS = "ws-int-ok-1"
		childName  = "child-int-ok"
	)

	ws := integrationWorkshop(sessNS, sessName, workshopNS)
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")
	ws.Status.Namespace = workshopNS
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseReady
	require.NoError(t, env.Client.Status().Update(ctx, ws), "mark Workshop Ready")

	ensureNamespace(t, env, workshopNS)
	child := integrationChild(workshopNS, childName)
	require.NoError(t, env.Client.Create(ctx, child), "create child AgentSession")

	// The delegation controller's own write: the child's upward #parent tuple
	// naming the builder session — this is what read_transcript's `+ parent`
	// arm (plan 4a Task 3) resolves through.
	require.NoError(t, spdb.TouchLineage(ctx, workshopNS, childName, sessNS, sessName), "seed the parent lineage tuple")

	mem := memory.NewLocal(inmem.NewBackend())
	appendCtx := memory.WithSystemApproval(ctx, "test")
	require.NoError(t, turn.NewAppender(mem, memory.Scope{Kind: "session", ID: workshopNS + "/" + childName}).
		Append(appendCtx, memory.Turn{
			Index: 0, Role: "user",
			Content:   []memory.ContentBlock{{Type: "text", Text: "the child's own transcript"}},
			CreatedAt: time.Now().UTC(),
		}), "seed a turn in the child's transcript")
	require.NoError(t, lifecyclekind.Append(appendCtx, mem,
		memory.Scope{Kind: "session", ID: workshopNS + "/" + childName},
		lifecyclecore.RunnerTerminal{
			Phase:   lifecyclecore.PhaseFailed,
			Reason:  "MCPAllowlistDrift",
			Message: "MCPServer/demo-connector: pinned tools not served: [get_thing]",
		}, time.Now().UTC(), lifecyclekind.OrderKey{}), "seed a transition in the child's signed log")

	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}, "tok-int-ok", "")

	srv := httptest.NewServer(workshoptranscriptsrv.NewHandler(env.Client, mem, reg, spdb))
	t.Cleanup(srv.Close)

	resp := getTranscriptIntegration(t, srv.URL, "tok-int-ok", childName)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "a genuinely-written parent tuple lets the read through")

	var out transcriptResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out), "decode transcriptResponse")
	require.Len(t, out.Turns, 1, "the child's one seeded turn is returned")
	assert.Equal(t, "the child's own transcript", out.Turns[0].Content[0].Text)

	// The signed log comes back under the SAME real permission, from the same
	// real memory backend — a fake checker cannot show that, and the log is
	// what a reader gets when a session failed before it said anything.
	require.Len(t, out.AuditEntries, 1, "the one seeded transition is returned")
	assert.Equal(t, "runner_terminal", out.AuditEntries[0].Type)
	assert.Contains(t, string(out.AuditEntries[0].Details), "MCPAllowlistDrift")
}

// TestIntegration_ParentTupleAbsent_DeniesAndReturnsNoTurns is the
// load-bearing refusing-direction integration test: a real, Ready Workshop
// with a correctly-registered bearer, and a child session that genuinely
// exists in the workshop's own namespace, is still denied when the
// delegation controller never wrote the parent lineage tuple in the real
// SpiceDB backend — e.g. the child was spawned by something other than a
// SubagentRequest naming this builder as spec.parent.
//
// The workshop-owner arm cannot admit it either, and that is the point of
// the fixtures it uses: integrationWorkshop records no spec.starterCanonical
// and integrationChild carries no started-by annotation, so this workshop
// belongs to nobody and the session is attributed to nobody — two empties
// that must never compare equal. Giving either one a real value belongs in
// the two owner-arm tests below, not here.
func TestIntegration_ParentTupleAbsent_DeniesAndReturnsNoTurns(t *testing.T) {
	env := testenv.Shared(t)
	spdb := newIntegrationSpiceDBClient(t)
	ctx := context.Background()

	const (
		sessNS     = "default"
		sessName   = "builder-int-deny"
		workshopNS = "ws-int-deny-1"
		childName  = "child-int-deny"
	)

	ws := integrationWorkshop(sessNS, sessName, workshopNS)
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")
	ws.Status.Namespace = workshopNS
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseReady
	require.NoError(t, env.Client.Status().Update(ctx, ws), "mark Workshop Ready")

	ensureNamespace(t, env, workshopNS)
	child := integrationChild(workshopNS, childName)
	require.NoError(t, env.Client.Create(ctx, child), "create child AgentSession")

	// Deliberately NOT calling TouchLineage: no #parent tuple exists in this
	// test's SpiceDB datastore, so read_transcript's `+ parent` arm has
	// nothing to resolve through.

	mem := memory.NewLocal(inmem.NewBackend())
	appendCtx := memory.WithSystemApproval(ctx, "test")
	require.NoError(t, turn.NewAppender(mem, memory.Scope{Kind: "session", ID: workshopNS + "/" + childName}).
		Append(appendCtx, memory.Turn{
			Index: 0, Role: "user",
			Content:   []memory.ContentBlock{{Type: "text", Text: "must never be returned"}},
			CreatedAt: time.Now().UTC(),
		}), "seed a turn in the child's transcript")

	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}, "tok-int-deny", "")

	srv := httptest.NewServer(workshoptranscriptsrv.NewHandler(env.Client, mem, reg, spdb))
	t.Cleanup(srv.Close)

	resp := getTranscriptIntegration(t, srv.URL, "tok-int-deny", childName)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "no parent tuple in the real backend ⇒ denied")
}

// integrationBuilderUser is the fixture person a workshop belongs to: the
// bare canonical id the AgentSession reconciler records on
// Workshop.spec.starterCanonical.
const integrationBuilderUser = "builder-user"

// ownedIntegrationWorkshop is integrationWorkshop plus the recorded owner —
// the person whose own root test sessions in W this workshop's builder may
// read back.
func ownedIntegrationWorkshop(sessNS, sessName, workshopNS, starter string) *spiceboxv1alpha1.Workshop {
	ws := integrationWorkshop(sessNS, sessName, workshopNS)
	ws.Spec.StarterCanonical = starter
	return ws
}

// integrationRootTestSession is the person's OWN test session in W: nobody
// delegated it (no spec.parent, no SubagentRequest owner), and its started-by
// annotation names the person who started it, in the "user:"-prefixed form
// pkg/web/browsersession stamps on a browser-started session.
func integrationRootTestSession(workshopNS, name, starter string) *spiceboxv1alpha1.AgentSession {
	sess := integrationChild(workshopNS, name)
	sess.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:" + starter,
	}
	return sess
}

// TestIntegration_RootSessionStartedByWorkshopOwner_ReturnsTurns is the
// "Try it as yourself" path end to end against a REAL SpiceDB: the person
// started the test session themselves, so nothing ever wrote a `parent`
// tuple for it and read_transcript genuinely does not hold — the route
// admits it on the workshop's own recorded owner instead. The real backend
// is what makes this test worth having: it proves the read no longer depends
// on a relationship nobody writes for a root session, rather than on a fake
// that could be told to say yes.
func TestIntegration_RootSessionStartedByWorkshopOwner_ReturnsTurns(t *testing.T) {
	env := testenv.Shared(t)
	spdb := newIntegrationSpiceDBClient(t)
	ctx := context.Background()

	const (
		sessNS     = "default"
		sessName   = "builder-int-owner"
		workshopNS = "ws-int-owner-1"
		testName   = "sess-live"
	)

	ws := ownedIntegrationWorkshop(sessNS, sessName, workshopNS, integrationBuilderUser)
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")
	ws.Status.Namespace = workshopNS
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseReady
	require.NoError(t, env.Client.Status().Update(ctx, ws), "mark Workshop Ready")

	ensureNamespace(t, env, workshopNS)
	require.NoError(t, env.Client.Create(ctx, integrationRootTestSession(workshopNS, testName, integrationBuilderUser)),
		"create the person's own root test session")

	// Deliberately NOT calling TouchLineage: a session the person started
	// themselves was delegated by nobody, so no #parent tuple exists — and
	// none is ever written for one.

	mem := memory.NewLocal(inmem.NewBackend())
	appendCtx := memory.WithSystemApproval(ctx, "test")
	require.NoError(t, turn.NewAppender(mem, memory.Scope{Kind: "session", ID: workshopNS + "/" + testName}).
		Append(appendCtx, memory.Turn{
			Index: 0, Role: "user",
			Content:   []memory.ContentBlock{{Type: "text", Text: "what the person's own test did"}},
			CreatedAt: time.Now().UTC(),
		}), "seed a turn in the test session's transcript")

	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}, "tok-int-owner", "")

	srv := httptest.NewServer(workshoptranscriptsrv.NewHandler(env.Client, mem, reg, spdb))
	t.Cleanup(srv.Close)

	resp := getTranscriptIntegration(t, srv.URL, "tok-int-owner", testName)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "the workshop owner's own root test session is readable with no tuple at all")

	var out transcriptResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out), "decode transcriptResponse")
	require.Len(t, out.Turns, 1, "the test session's one seeded turn is returned")
	assert.Equal(t, "what the person's own test did", out.Turns[0].Content[0].Text)
}

// TestIntegration_RootSessionStartedByAnotherPerson_Denies is the refusing
// direction of the same arm: a root session sitting in the SAME workshop
// namespace, with everything else identical, is refused when its recorded
// starter is not the person the workshop belongs to. The workshop namespace
// is not itself an authorization — being in W admits nothing on its own.
func TestIntegration_RootSessionStartedByAnotherPerson_Denies(t *testing.T) {
	env := testenv.Shared(t)
	spdb := newIntegrationSpiceDBClient(t)
	ctx := context.Background()

	const (
		sessNS      = "default"
		sessName    = "builder-int-other"
		workshopNS  = "ws-int-other-1"
		testName    = "sess-live"
		otherPerson = "another-person"
	)

	ws := ownedIntegrationWorkshop(sessNS, sessName, workshopNS, integrationBuilderUser)
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")
	ws.Status.Namespace = workshopNS
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseReady
	require.NoError(t, env.Client.Status().Update(ctx, ws), "mark Workshop Ready")

	ensureNamespace(t, env, workshopNS)
	require.NoError(t, env.Client.Create(ctx, integrationRootTestSession(workshopNS, testName, otherPerson)),
		"create a root session someone else started")

	mem := memory.NewLocal(inmem.NewBackend())
	appendCtx := memory.WithSystemApproval(ctx, "test")
	require.NoError(t, turn.NewAppender(mem, memory.Scope{Kind: "session", ID: workshopNS + "/" + testName}).
		Append(appendCtx, memory.Turn{
			Index: 0, Role: "user",
			Content:   []memory.ContentBlock{{Type: "text", Text: "must never be returned"}},
			CreatedAt: time.Now().UTC(),
		}), "seed a turn in the other person's transcript")

	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}, "tok-int-other", "")

	srv := httptest.NewServer(workshoptranscriptsrv.NewHandler(env.Client, mem, reg, spdb))
	t.Cleanup(srv.Close)

	resp := getTranscriptIntegration(t, srv.URL, "tok-int-other", testName)
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "a root session started by someone else is not this workshop's to read")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read denial body")
	assert.NotContains(t, string(body), "must never be returned", "a denial carries none of the session's turn content")
}
