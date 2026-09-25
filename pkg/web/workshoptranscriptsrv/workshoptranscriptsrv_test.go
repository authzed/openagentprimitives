package workshoptranscriptsrv_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	lifecyclekind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/web/workshoptranscriptsrv"
)

// fakeChecker records every call so a test can assert the route re-checks
// the LIVE permission (childNS, childName, subjectNS, subjectName) rather
// than trusting the bearer alone.
type fakeChecker struct {
	allow bool
	err   error
	calls []checkCall
}

type checkCall struct{ childNS, childName, subjectNS, subjectName string }

func (f *fakeChecker) CheckReadTranscriptForSession(_ context.Context, childNS, childName, subjectNS, subjectName string) (bool, error) {
	f.calls = append(f.calls, checkCall{childNS, childName, subjectNS, subjectName})
	return f.allow, f.err
}

const (
	sessNS      = "default"
	sessName    = "builder-x"
	wsNamespace = "ws-abc123456789"
	childName   = "child-c"
)

func readyWorkshop() *spiceboxv1alpha1.Workshop {
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
			Namespace: wsNamespace,
			Phase:     spiceboxv1alpha1.WorkshopPhaseReady,
		},
	}
}

func provisioningWorkshop() *spiceboxv1alpha1.Workshop {
	ws := readyWorkshop()
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseProvisioning
	return ws
}

// childInWorkshop is the child AgentSession living inside the workshop's own
// namespace W — the shape the delegation controller's SubagentRequest
// produces a child in.
func childInWorkshop() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: wsNamespace, Name: childName},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "demo-class",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "test the built agent"},
		},
	}
}

// harness bundles the handler under test with the fakes a case inspects
// after the request.
type harness struct {
	srv     *httptest.Server
	c       client.Client
	mem     memory.Memory
	reg     *tokens.Registry
	checker *fakeChecker
}

func newHarness(t *testing.T, checker *fakeChecker, objs ...client.Object) *harness {
	t.Helper()
	scheme := testfixtures.NewScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.Workshop{}).
		Build()
	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()

	var h harness
	var handler http.Handler
	if checker == nil {
		handler = workshoptranscriptsrv.NewHandler(c, mem, reg, nil)
	} else {
		handler = workshoptranscriptsrv.NewHandler(c, mem, reg, checker)
	}
	h = harness{srv: httptest.NewServer(handler), c: c, mem: mem, reg: reg, checker: checker}
	t.Cleanup(h.srv.Close)
	return &h
}

// registerBearer registers a workshop bearer keyed the way plan 2's
// AgentSession reconciler does: the SYNTHETIC {sessNS, WorkshopName(sessName)}
// pair — never the builder session {sessNS, sessName} itself.
func (h *harness) registerBearer(token string) {
	h.reg.Set(memory.NamespacedName{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}, token, "")
}

// seedTurn writes one turn into the child's transcript scope, as an
// in-process trusted writer would (the runner, via turn.Appender).
func (h *harness) seedTurn(t *testing.T, ns, name string, idx int, role, text string) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	a := turn.NewAppender(h.mem, memory.Scope{Kind: "session", ID: ns + "/" + name})
	require.NoError(t, a.Append(ctx, memory.Turn{
		Index:     idx,
		Role:      role,
		Content:   []memory.ContentBlock{{Type: "text", Text: text}},
		CreatedAt: time.Now().UTC(),
	}), "seed turn")
}

func getTranscript(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err, "NewRequest")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Do")
	return resp
}

type transcriptResponse struct {
	Turns        []memory.Turn `json:"turns"`
	AuditEntries []auditEntry  `json:"auditEntries"`
}

// auditEntry mirrors the route's own entry shape for decoding.
type auditEntry struct {
	At      time.Time       `json:"at"`
	Type    string          `json:"type"`
	Details json.RawMessage `json:"details,omitempty"`
}

func decodeTranscript(t *testing.T, resp *http.Response) transcriptResponse {
	t.Helper()
	var out transcriptResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out), "decode transcriptResponse")
	return out
}

func TestGetTranscript_ValidBearerReadTranscriptHolds_ReturnsChildTurns(t *testing.T) {
	checker := &fakeChecker{allow: true}
	h := newHarness(t, checker, readyWorkshop(), childInWorkshop())
	h.registerBearer("tok-workshop")
	h.seedTurn(t, wsNamespace, childName, 0, "user", "hello from the child")

	resp := getTranscript(t, h.srv.URL+workshoptranscriptsrv.Path+childName, "tok-workshop")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "read_transcript holds → 200")

	out := decodeTranscript(t, resp)
	require.Len(t, out.Turns, 1, "the child's one seeded turn is returned")
	assert.Equal(t, "hello from the child", out.Turns[0].Content[0].Text)

	// The route re-checked the LIVE permission with the values it derived
	// from the resolved Workshop CR — the workshop namespace W as the
	// child's namespace, and the BUILDER session as subject — not the
	// synthetic bearer key.
	require.Len(t, checker.calls, 1, "CheckReadTranscriptForSession called exactly once")
	assert.Equal(t, checkCall{wsNamespace, childName, sessNS, sessName}, checker.calls[0])
}

// TestGetTranscript_ChildNotInWorkshopNamespace_403 is a load-bearing
// refusing-direction test: a child session name that resolves to no
// AgentSession in the bearer's OWN workshop namespace W — because it
// actually lives in a different workshop's namespace — is refused before
// authorization is even asked. The body/URL never override which namespace
// is searched; that is fixed to W by the resolved Workshop CR alone.
func TestGetTranscript_ChildNotInWorkshopNamespace_403(t *testing.T) {
	checker := &fakeChecker{allow: true}
	// No AgentSession seeded in wsNamespace at all — the requested child
	// exists nowhere the bearer's own workshop can see, as if it actually
	// lived in a different workshop's namespace.
	h := newHarness(t, checker, readyWorkshop())
	h.registerBearer("tok-workshop")

	resp := getTranscript(t, h.srv.URL+workshoptranscriptsrv.Path+childName, "tok-workshop")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "child absent from W → 403")
	assert.Empty(t, checker.calls, "never reaches the permission check for a child outside this workshop")
}

// TestGetTranscript_ReadTranscriptDoesNotHold_403DeniesAndReturnsNoTurns is
// the load-bearing refusing-direction test for the authorization arm itself:
// a child that DOES live in the bearer's own workshop namespace is still
// denied when the read_transcript permission does not hold (e.g. the
// delegation controller never wrote the `parent` relationship, or it named a
// different parent). The route re-checks the live permission; it does not
// trust the bearer's mere existence, nor the child's mere presence in W.
func TestGetTranscript_ReadTranscriptDoesNotHold_403DeniesAndReturnsNoTurns(t *testing.T) {
	checker := &fakeChecker{allow: false}
	h := newHarness(t, checker, readyWorkshop(), childInWorkshop())
	h.registerBearer("tok-workshop")
	h.seedTurn(t, wsNamespace, childName, 0, "user", "should never be returned")

	resp := getTranscript(t, h.srv.URL+workshoptranscriptsrv.Path+childName, "tok-workshop")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "read_transcript false → 403")
	require.Len(t, checker.calls, 1, "the permission WAS re-checked")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read denial body")
	assert.NotContains(t, string(body), "should never be returned",
		"a denial must not carry any of the child's turn content")
}

func TestGetTranscript_CheckErrors_403DeniesAndReturnsNoTurns(t *testing.T) {
	checker := &fakeChecker{err: errors.New("spicedb unavailable")}
	h := newHarness(t, checker, readyWorkshop(), childInWorkshop())
	h.registerBearer("tok-workshop")

	resp := getTranscript(t, h.srv.URL+workshoptranscriptsrv.Path+childName, "tok-workshop")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "check error is fail-closed, not fail-open")
}

func TestGetTranscript_NilChecker_503DeniesWithoutPanicking(t *testing.T) {
	h := newHarness(t, nil, readyWorkshop(), childInWorkshop())
	h.registerBearer("tok-workshop")

	assert.NotPanics(t, func() {
		resp := getTranscript(t, h.srv.URL+workshoptranscriptsrv.Path+childName, "tok-workshop")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "an unwired checker denies, it does not panic")
	})
}

func TestGetTranscript_UnknownBearer_401(t *testing.T) {
	checker := &fakeChecker{allow: true}
	h := newHarness(t, checker, readyWorkshop(), childInWorkshop())
	// No bearer registered at all.

	resp := getTranscript(t, h.srv.URL+workshoptranscriptsrv.Path+childName, "bogus-token")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Empty(t, checker.calls, "an unauthenticated request never reaches the permission check")
}

func TestGetTranscript_NoBearerHeader_401(t *testing.T) {
	checker := &fakeChecker{allow: true}
	h := newHarness(t, checker, readyWorkshop(), childInWorkshop())

	resp := getTranscript(t, h.srv.URL+workshoptranscriptsrv.Path+childName, "")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// TestGetTranscript_WorkshopNotReady_403 covers a bearer that resolves to a
// real Workshop CR that has not finished provisioning: the sidecar cannot
// even exist yet in this state in production, but a stale/racing bearer
// must still be refused rather than trusted.
func TestGetTranscript_WorkshopNotReady_403(t *testing.T) {
	checker := &fakeChecker{allow: true}
	h := newHarness(t, checker, provisioningWorkshop())
	h.registerBearer("tok-workshop")

	resp := getTranscript(t, h.srv.URL+workshoptranscriptsrv.Path+childName, "tok-workshop")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "not-Ready workshop denies")
	assert.Empty(t, checker.calls, "never reaches the permission check for a not-yet-provisioned workshop")
}

// TestGetTranscript_BearerWithNoWorkshopCR_403 covers a registered bearer
// whose synthetic key names no Workshop CR at all (e.g. torn down between
// token registration and this request).
func TestGetTranscript_BearerWithNoWorkshopCR_403(t *testing.T) {
	checker := &fakeChecker{allow: true}
	h := newHarness(t, checker) // no Workshop object seeded
	h.registerBearer("tok-workshop")

	resp := getTranscript(t, h.srv.URL+workshoptranscriptsrv.Path+childName, "tok-workshop")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Empty(t, checker.calls)
}

// builderUser is the fixture person a workshop belongs to: the bare canonical
// id the AgentSession reconciler records on Workshop.spec.starterCanonical,
// and — in its "user:"-prefixed form — the started-by annotation the browser
// stamps on a session that person starts.
const builderUser = "builder-user"

// ownedWorkshop is a Ready workshop that records builderUser as the person it
// belongs to. readyWorkshop() deliberately records nobody, which is the
// fail-closed shape the empty-starter case below exercises.
func ownedWorkshop() *spiceboxv1alpha1.Workshop {
	ws := readyWorkshop()
	ws.Spec.StarterCanonical = builderUser
	return ws
}

// startedBy stamps the started-by annotation a session creator writes —
// the "user:<canonical>" PREFIXED form, as pkg/web/browsersession does on
// the AgentSession it creates for a browser-started session.
func startedBy(sess *spiceboxv1alpha1.AgentSession, canonical string) *spiceboxv1alpha1.AgentSession {
	if sess.Annotations == nil {
		sess.Annotations = map[string]string{}
	}
	sess.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID] = "user:" + canonical
	return sess
}

// rootTestSession is the shape of the person's OWN test session in W: nobody
// delegated it, so it names no parent and holds no SubagentRequest owner, and
// its started-by annotation names the person who started it.
func rootTestSession(canonical string) *spiceboxv1alpha1.AgentSession {
	return startedBy(childInWorkshop(), canonical)
}

// delegatedChildObjects is the shape a SubagentRequest-created child has: it
// names its parent AND carries the controller OwnerReference back to the
// request, which is the link OwningSubagentRequest follows. The request
// itself is returned alongside so the fake client can resolve that link.
func delegatedChildObjects(canonical string) []client.Object {
	sr := &spiceboxv1alpha1.SubagentRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: wsNamespace, Name: "test-tool-abc"},
		Spec: spiceboxv1alpha1.SubagentRequestSpec{
			Parent: spiceboxv1alpha1.NamespacedRef{Namespace: sessNS, Name: sessName},
			Class:  "demo-class",
			Task:   "test the built agent",
		},
	}
	child := startedBy(childInWorkshop(), canonical)
	child.Spec.Parent = &spiceboxv1alpha1.NamespacedRef{Namespace: sessNS, Name: sessName}
	child.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: spiceboxv1alpha1.SchemeGroupVersion.String(),
		Kind:       "SubagentRequest",
		Name:       sr.Name,
		UID:        "sr-uid",
	}}
	return []client.Object{sr, child}
}

// brokenDelegationLink is a session that names a parent but carries no
// SubagentRequest owner reference — the one shape OwningSubagentRequest
// reports as an ERROR rather than as "not delegated".
func brokenDelegationLink(canonical string) *spiceboxv1alpha1.AgentSession {
	child := startedBy(childInWorkshop(), canonical)
	child.Spec.Parent = &spiceboxv1alpha1.NamespacedRef{Namespace: sessNS, Name: sessName}
	return child
}

// TestGetTranscript_WorkshopOwnerArm covers the arm that admits the person's
// OWN root test session — the "Try it" session they started themselves in the
// workshop namespace, which nobody delegated and which therefore holds no
// `parent` relationship for read_transcript to resolve through. Every row
// seeds the same turn, so a row expecting a refusal also proves the refusal
// carries none of it.
func TestGetTranscript_WorkshopOwnerArm(t *testing.T) {
	const (
		otherPerson = "another-person"
		turnText    = "what the test session did"
	)
	cases := []struct {
		name      string
		ws        *spiceboxv1alpha1.Workshop
		sessions  []client.Object
		allow     bool
		wantCode  int
		wantCalls int
	}{
		{
			name:      "root session started by the workshop's own owner: 200, and read_transcript is never asked",
			ws:        ownedWorkshop(),
			sessions:  []client.Object{rootTestSession(builderUser)},
			allow:     false, // the tuple path would refuse; this arm does not consult it
			wantCode:  http.StatusOK,
			wantCalls: 0,
		},
		{
			name:      "root session started by someone else: 403 on the tuple path",
			ws:        ownedWorkshop(),
			sessions:  []client.Object{rootTestSession(otherPerson)},
			allow:     false,
			wantCode:  http.StatusForbidden,
			wantCalls: 1,
		},
		{
			name:      "workshop records no starter and the session names none: 403, empty never matches empty",
			ws:        readyWorkshop(),
			sessions:  []client.Object{childInWorkshop()},
			allow:     false,
			wantCode:  http.StatusForbidden,
			wantCalls: 1,
		},
		{
			name:      "delegated child started by the workshop's owner: 403, it stays on the tuple path",
			ws:        ownedWorkshop(),
			sessions:  delegatedChildObjects(builderUser),
			allow:     false,
			wantCode:  http.StatusForbidden,
			wantCalls: 1,
		},
		{
			name:      "delegated child whose tuple holds: 200 on the tuple path, unchanged",
			ws:        ownedWorkshop(),
			sessions:  delegatedChildObjects(builderUser),
			allow:     true,
			wantCode:  http.StatusOK,
			wantCalls: 1,
		},
		{
			name:      "unreadable delegation link: 403 before the tuple check, even though it would allow",
			ws:        ownedWorkshop(),
			sessions:  []client.Object{brokenDelegationLink(builderUser)},
			allow:     true,
			wantCode:  http.StatusForbidden,
			wantCalls: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checker := &fakeChecker{allow: tc.allow}
			h := newHarness(t, checker, append([]client.Object{tc.ws}, tc.sessions...)...)
			h.registerBearer("tok-workshop")
			h.seedTurn(t, wsNamespace, childName, 0, "user", turnText)

			resp := getTranscript(t, h.srv.URL+workshoptranscriptsrv.Path+childName, "tok-workshop")
			defer resp.Body.Close()
			require.Equal(t, tc.wantCode, resp.StatusCode)
			assert.Len(t, checker.calls, tc.wantCalls, "how often read_transcript was asked")

			if tc.wantCode == http.StatusOK {
				out := decodeTranscript(t, resp)
				require.Len(t, out.Turns, 1, "the session's one seeded turn is returned")
				assert.Equal(t, turnText, out.Turns[0].Content[0].Text)
				return
			}
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err, "read denial body")
			assert.NotContains(t, string(body), turnText, "a denial must not carry any of the session's turn content")
		})
	}
}

// seedLifecycleEvent appends one typed transition to the session's signed
// lifecycle log, the way the operator and the runner both do.
func (h *harness) seedLifecycleEvent(t *testing.T, ns, name string, ev lifecyclecore.Event) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	require.NoError(t, lifecyclekind.Append(ctx, h.mem,
		memory.Scope{Kind: "session", ID: ns + "/" + name}, ev, time.Now().UTC(),
		lifecyclekind.OrderKey{}), "seed lifecycle event")
}

// TestGetTranscript_ReturnsTheSessionsAuditEntries covers the reason this
// route grew a second list: a test session that failed BEFORE it called
// anything has an empty transcript, and a reader given only that concludes
// nothing happened. The signed log says what actually did.
func TestGetTranscript_ReturnsTheSessionsAuditEntries(t *testing.T) {
	checker := &fakeChecker{allow: true}
	h := newHarness(t, checker, readyWorkshop(), childInWorkshop())
	h.registerBearer("tok-workshop")
	h.seedLifecycleEvent(t, wsNamespace, childName, lifecyclecore.RunnerClaimed{})
	h.seedLifecycleEvent(t, wsNamespace, childName, lifecyclecore.RunnerTerminal{
		Phase:   lifecyclecore.PhaseFailed,
		Reason:  "MCPAllowlistDrift",
		Message: "MCPServer/demo-connector: pinned tools not served: [get_thing]",
	})

	resp := getTranscript(t, h.srv.URL+workshoptranscriptsrv.Path+childName, "tok-workshop")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	out := decodeTranscript(t, resp)
	assert.Empty(t, out.Turns, "the session failed before it said or did anything")
	require.Len(t, out.AuditEntries, 2, "both recorded transitions come back")
	assert.Equal(t, "runner_claimed", out.AuditEntries[0].Type)
	assert.Equal(t, "runner_terminal", out.AuditEntries[1].Type)
	assert.Contains(t, string(out.AuditEntries[1].Details), "MCPAllowlistDrift",
		"the failure's reason is in the entry a reader gets")
	assert.Contains(t, string(out.AuditEntries[1].Details), "pinned tools not served",
		"and so is the message that says what to do about it")
	assert.False(t, out.AuditEntries[0].At.IsZero(), "each entry is stamped")
}

// TestGetTranscript_AuditEntriesObeyTheSameAuthorization pins the entries to
// the route's existing gate rather than to a second one: a denied
// read_transcript must return no log either, not just no turns.
func TestGetTranscript_AuditEntriesObeyTheSameAuthorization(t *testing.T) {
	checker := &fakeChecker{allow: false}
	h := newHarness(t, checker, readyWorkshop(), childInWorkshop())
	h.registerBearer("tok-workshop")
	h.seedLifecycleEvent(t, wsNamespace, childName, lifecyclecore.RunnerClaimed{})

	resp := getTranscript(t, h.srv.URL+workshoptranscriptsrv.Path+childName, "tok-workshop")
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.NotContains(t, string(body), "runner_claimed", "a refused read returns no log entries")
}
