package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/channels/sessionnotice"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/internal/wstest"
)

// withTestSubject wraps a handler so it observes an authenticated subject,
// mirroring what the webui.Server's AuthAuthenticated middleware injects in
// production (see webui.WithSubjectForTest's doc).
func withTestSubject(subject string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(webui.WithSubjectForTest(r.Context(), subject)))
	})
}

// seedLiveSession inserts a finalized live session entry directly into the
// registry table (bypassing the builder) so the detail/messages/message/
// interrupt/decision/ws handlers can be exercised without a real session
// build.
func seedLiveSession(t *testing.T, reg *Registry, ns, name, agentClass, title, owner string, createdAt time.Time) {
	t.Helper()
	e := newSessionEntry(ns, name, owner)
	e.agentClass = agentClass
	e.title = title
	e.createdAt = createdAt
	putTestEntry(reg, sessionKey{Namespace: ns, Name: name}, e)
}

// --- GET /sessions/api/{ns}/{name}/detail ----------------------------------

// serveDetail serves sessionDetailHandler with r.PathValue("ns")/("name")
// populated, mirroring what the framework's ServeMux wildcard match
// provides in production.
func serveDetail(subject, ns, name string, h http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/sessions/api/"+ns+"/"+name+"/detail", nil)
	req.SetPathValue("ns", ns)
	req.SetPathValue("name", name)
	if subject != "" {
		req = req.WithContext(webui.WithSubjectForTest(req.Context(), subject))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestSessionDetailHandler_AuthorizedReturnsFields(t *testing.T) {
	const name = "demo-agent-abcd1234"
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: newChatSessionNamespace,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour).UTC().Truncate(time.Second)),
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "demo-agent"},
	}
	sess.Status.Phase = "Running"
	sess.Status.EffectiveSettings = &spiceboxv1alpha1.EffectiveSettings{
		Model:  spiceboxv1alpha1.ModelConfig{Provider: "anthropic", Name: "claude-opus-4-8"},
		Budget: spiceboxv1alpha1.BudgetConfig{MaxTurns: 40, MaxTokens: 120000, MaxDuration: metav1.Duration{Duration: 30 * time.Minute}},
	}
	k8s := newFakeK8sClient(t, sess)
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)
	seedLiveSession(t, reg, newChatSessionNamespace, name, "demo-agent", "hello", "user:owner", time.Now())

	w := serveDetail("user:owner", newChatSessionNamespace, name, sessionDetailHandler(&fakeDeps{k8s: k8s}, reg))
	require.Equal(t, http.StatusOK, w.Code)
	var got sessionDetail
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, name, got.SessionID)
	assert.Equal(t, "demo-agent", got.AgentClass)
	assert.Equal(t, modelInfo{Provider: "anthropic", Name: "claude-opus-4-8"}, got.Model)
	assert.Equal(t, int64(120000), got.MaxTokens)
	assert.Equal(t, int32(40), got.MaxTurns)
	assert.Equal(t, "30m0s", got.MaxDuration)
	assert.Equal(t, "user:owner", got.Owner)
	assert.Equal(t, "Running", got.Phase)
}

func TestSessionDetailHandler_FallsBackToClassSpecWhenEffectiveUnset(t *testing.T) {
	const name = "demo-agent-nofx"
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: newChatSessionNamespace},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo-agent"},
	}
	sess.Status.Phase = "Pending"
	ac := validAgentClass("demo-agent")
	ac.Spec.Model = &spiceboxv1alpha1.ModelConfig{Provider: "anthropic", Name: "claude-from-class"}
	ac.Spec.Budget = &spiceboxv1alpha1.BudgetConfig{MaxTurns: 12, MaxTokens: 5000, MaxDuration: metav1.Duration{Duration: 0}}
	k8s := newFakeK8sClient(t, sess, ac)
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)
	seedLiveSession(t, reg, newChatSessionNamespace, name, "demo-agent", "hi", "user:owner", time.Now())

	w := serveDetail("user:owner", newChatSessionNamespace, name, sessionDetailHandler(&fakeDeps{k8s: k8s}, reg))
	require.Equal(t, http.StatusOK, w.Code)
	var got sessionDetail
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "claude-from-class", got.Model.Name, "model falls back to the AgentClass spec")
	assert.Equal(t, int32(12), got.MaxTurns)
	assert.Equal(t, "none", got.MaxDuration, "a zero max-duration renders as none")
}

// TestSessionDetailHandler_NoStandingForbidden replaces the old
// owner-annotation-comparison test: a subject CheckInteract refuses gets
// 403, regardless of whether they equal the session's owner string.
func TestSessionDetailHandler_NoStandingForbidden(t *testing.T) {
	const name = "demo-agent-guarded"
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t), checkInteract: checkInteractOnlyFor("user:owner")}, newFakeBuilder().build)
	seedLiveSession(t, reg, newChatSessionNamespace, name, "demo-agent", "secret", "user:owner", time.Now())

	w := serveDetail("user:intruder", newChatSessionNamespace, name,
		sessionDetailHandler(&fakeDeps{k8s: newFakeK8sClient(t), checkInteract: checkInteractOnlyFor("user:owner")}, reg))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestSessionDetailHandler_UnknownSessionNotFound(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	w := serveDetail("user:owner", newChatSessionNamespace, "nope", sessionDetailHandler(&fakeDeps{k8s: newFakeK8sClient(t)}, reg))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// --- GET /sessions/api/{ns}/{name}/messages --------------------------------

// serveMessages drives sessionMessagesHandler for an authenticated GET, with
// r.PathValue("ns")/("name") populated as the framework's wildcard match
// would. subject=="" leaves the context subject unset (the no-auth case).
func serveMessages(subject, ns, name string, d Deps, reg *Registry) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/sessions/api/"+ns+"/"+name+"/messages", nil)
	req.SetPathValue("ns", ns)
	req.SetPathValue("name", name)
	if subject != "" {
		req = req.WithContext(webui.WithSubjectForTest(req.Context(), subject))
	}
	w := httptest.NewRecorder()
	sessionMessagesHandler(d, reg).ServeHTTP(w, req)
	return w
}

// TestSessionMessagesHandler_NoStandingForbidden replaces the old
// owner-annotation-comparison test: a subject CheckInteract refuses gets
// 403, and the response body must carry no transcript bytes.
func TestSessionMessagesHandler_NoStandingForbidden(t *testing.T) {
	const name = "demo-agent-mine"
	d := &fakeDeps{k8s: newFakeK8sClient(t), checkInteract: checkInteractOnlyFor("user:owner")}
	reg := newRegistryWithBuilder(d, newFakeBuilder().build)
	seedLiveSession(t, reg, newChatSessionNamespace, name, "demo-agent", "t", "user:owner", time.Now())

	w := serveMessages("user:intruder", newChatSessionNamespace, name, d, reg)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.NotContains(t, w.Body.String(), "timeline", "a denied request must carry no transcript bytes")
}

func TestSessionMessagesHandler_NoMemoryAccessFailsClosed(t *testing.T) {
	const name = "demo-agent-nomem"
	d := &fakeDeps{}
	reg := newRegistryWithBuilder(d, newFakeBuilder().build)
	seedLiveSession(t, reg, newChatSessionNamespace, name, "demo-agent", "t", "user:owner", time.Now())

	// fakeDeps with empty operatorURL/memoryToken → no memory access.
	w := serveMessages("user:owner", newChatSessionNamespace, name, d, reg)
	assert.Equal(t, http.StatusNotImplemented, w.Code, "must fail closed (not a silent empty transcript) when webd has no memory token")
}

func TestSessionMessagesHandler_SuccessReturnsTimeline(t *testing.T) {
	const name = "demo-agent-ok"
	at := time.Now().UTC().Truncate(time.Second)
	entries := []memory.Entry{
		turnTestEntry(t, name, 0, "user", "hi there", at),
		turnTestEntry(t, name, 1, "assistant", "ahoy matey", at.Add(time.Second)),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{Entries: entries}))
	}))
	defer srv.Close()

	d := &fakeDeps{k8s: newFakeK8sClient(t), operatorURL: srv.URL, memoryToken: "webd-token"}
	reg := newRegistryWithBuilder(d, newFakeBuilder().build)
	seedLiveSession(t, reg, newChatSessionNamespace, name, "demo-agent", "t", "user:owner", time.Now())

	w := serveMessages("user:owner", newChatSessionNamespace, name, d, reg)
	require.Equal(t, http.StatusOK, w.Code)

	var resp messagesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Timeline, 2)
	assert.Equal(t, "message", resp.Timeline[0].Kind)
	assert.Equal(t, "user", resp.Timeline[0].Role)
	assert.Equal(t, "hi there", resp.Timeline[0].Text)
	assert.Equal(t, "agent", resp.Timeline[1].Role, "an assistant turn maps to the agent role")
	assert.Equal(t, "ahoy matey", resp.Timeline[1].Text)
}

func TestSessionMessagesHandler_OperatorErrorReturnsGeneric500(t *testing.T) {
	const name = "demo-agent-500"
	// The operator answers with a 4xx: readTranscript surfaces a
	// non-ErrNoMemoryAccess error, which the handler must map to a generic 500
	// (raw error logged, not leaked). A 4xx is used rather than a 5xx on purpose
	// — the memory httpclient retries a 5xx 8× (~17s), while a 4xx is permanent
	// and returns at once, exercising the very same generic-500 branch.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	d := &fakeDeps{k8s: newFakeK8sClient(t), operatorURL: srv.URL, memoryToken: "webd-token"}
	reg := newRegistryWithBuilder(d, newFakeBuilder().build)
	seedLiveSession(t, reg, newChatSessionNamespace, name, "demo-agent", "t", "user:owner", time.Now())

	w := serveMessages("user:owner", newChatSessionNamespace, name, d, reg)
	require.Equal(t, http.StatusInternalServerError, w.Code)

	var resp errorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "failed to load conversation history", resp.Error,
		"the generic message must not leak the operator's raw error")
}

// --- a FINISHED conversation stays readable --------------------------------

// transcriptServer serves one canned two-turn transcript, standing in for the
// operator's memory API so a read-path test can assert a real 200 payload.
func transcriptServer(t *testing.T, sessionName string) *httptest.Server {
	t.Helper()
	at := time.Now().UTC().Truncate(time.Second)
	entries := []memory.Entry{
		turnTestEntry(t, sessionName, 0, "user", "how did it go?", at),
		turnTestEntry(t, sessionName, 1, "assistant", "all done", at.Add(time.Second)),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{Entries: entries}))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSessionMessagesHandler_TerminalSessionStaysReadable is the read-path
// regression.
//
// A finished conversation is still listed in the shell — and both
// terminalPhase's and rehydrate's doc comments promise "its transcript stays
// readable". It was not. authorize() falls through to rehydrate() on ANY
// in-memory miss, and rehydrate refuses every terminal phase with
// ErrSessionNotFound, which mapSubmitError renders as 404. The miss is not an
// edge case: it is the normal state for a finished conversation (the phase
// watcher's teardown drops the entry) and the guaranteed state after a webd
// restart. The client's non-OK branch then renders an empty transcript with no
// error shown, so the user clicks a conversation the shell advertises and
// silently gets nothing. J5's liveness-rule row: the same phase that keeps
// .../messages readable (200) makes .../ws refuse a resume (404, unchanged —
// see TestWSHandler_TerminalSessionRefusesResume).
//
// The two rows reach the same miss by deliberately DIFFERENT routes: one runs
// the real teardown over a live entry, the other is a fresh Registry over the
// same cluster — which is exactly what a restart is.
func TestSessionMessagesHandler_TerminalSessionStaysReadable(t *testing.T) {
	const owner = "user:alice@example.com"
	const name = "demo-agent-done1111"

	cases := []struct {
		name string
		seed func(t *testing.T, reg *Registry)
	}{
		{
			name: "teardown dropped the in-memory entry: transcript still 200",
			seed: func(t *testing.T, reg *Registry) {
				seedLiveSession(t, reg, newChatSessionNamespace, name, "demo-agent", "finished", owner, time.Now())
				reg.teardown(sessionKey{Namespace: newChatSessionNamespace, Name: name}, terminalNotice{Reason: "succeeded"})
			},
		},
		{
			name: "webd restarted, nothing in memory: transcript still 200",
			seed: func(*testing.T, *Registry) {}, // a fresh process IS the empty table
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := transcriptServer(t, name)
			k8s := newFakeK8sClient(t,
				chatSessionCR(newChatSessionNamespace, name, owner, "demo-agent", "finished",
					spiceboxv1alpha1.AgentSessionPhaseSucceeded, time.Now()),
				chatChannelCR(newChatSessionNamespace, name),
			)
			d := &fakeDeps{k8s: k8s, operatorURL: srv.URL, memoryToken: "webd-token"}
			reg := newRegistryWithBuilder(d, newFakeBuilder().build)
			tc.seed(t, reg)

			w := serveMessages(owner, newChatSessionNamespace, name, d, reg)
			require.Equal(t, http.StatusOK, w.Code, "a finished conversation the shell lists must be readable")

			var resp messagesResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			require.Len(t, resp.Timeline, 2)
			assert.Equal(t, "how did it go?", resp.Timeline[0].Text)
			assert.Equal(t, "all done", resp.Timeline[1].Text)

			reg.mu.Lock()
			_, attached := reg.sessions[sessionKey{Namespace: newChatSessionNamespace, Name: name}]
			reg.mu.Unlock()
			assert.False(t, attached,
				"reading a finished conversation must NOT re-attach it — that spins up a health watcher that tears it down again")
		})
	}
}

// TestWSHandler_TerminalSessionRefusesResume is J5's liveness-rule row for
// the websocket door: a terminal session's transcript stays readable (see
// above) but a resume attempt must still be refused — re-attaching a
// finished conversation would spin up a health watcher that immediately
// tears it down again. This rule is untouched by the CheckInteract
// replacement; only the standing check inside it changed.
func TestWSHandler_TerminalSessionRefusesResume(t *testing.T) {
	const owner = "user:alice@example.com"
	const name = "demo-agent-done9999"
	k8s := newFakeK8sClient(t,
		chatSessionCR(newChatSessionNamespace, name, owner, "demo-agent", "finished",
			spiceboxv1alpha1.AgentSessionPhaseSucceeded, time.Now()),
		chatChannelCR(newChatSessionNamespace, name),
	)
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)

	req := httptest.NewRequest(http.MethodGet, "/sessions/api/"+newChatSessionNamespace+"/"+name+"/ws", nil)
	req.SetPathValue("ns", newChatSessionNamespace)
	req.SetPathValue("name", name)
	w := httptest.NewRecorder()
	withTestSubject(owner, wsHandler(&fakeDeps{k8s: k8s}, reg)).ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "resuming a terminal session is refused the same way an unknown session is")
}

// TestSessionDetailHandler_TerminalSessionStaysReadable: the info panel is the
// transcript's sibling door, gated by the same authorize() call, so it 404'd on
// exactly the same conversations.
func TestSessionDetailHandler_TerminalSessionStaysReadable(t *testing.T) {
	const owner = "user:alice@example.com"
	const name = "demo-agent-done2222"
	k8s := newFakeK8sClient(t,
		chatSessionCR(newChatSessionNamespace, name, owner, "demo-agent", "finished",
			spiceboxv1alpha1.AgentSessionPhaseSucceeded, time.Now()),
		chatChannelCR(newChatSessionNamespace, name),
	)
	d := &fakeDeps{k8s: k8s}
	reg := newRegistryWithBuilder(d, newFakeBuilder().build)

	w := serveDetail(owner, newChatSessionNamespace, name, sessionDetailHandler(d, reg))
	require.Equal(t, http.StatusOK, w.Code)

	var got sessionDetail
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, owner, got.Owner, "the owner comes from the CR's started-by annotation once the entry is gone")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, got.Phase)
}

// TestSessionMessagesHandler_TerminalSessionAuthorizationHolds is J5's core
// authorization table: making a finished conversation readable must not make
// it readable to ANYONE the platform hasn't granted standing to, and must
// admit anyone it has — not only the CR's own starter. A Slack/CLI session
// sharing the chat namespace is not a chat conversation at all, regardless
// of standing.
func TestSessionMessagesHandler_TerminalSessionAuthorizationHolds(t *testing.T) {
	const starter = "user:alice@example.com"
	const name = "demo-agent-done3333"

	slackSess := chatSessionCR(newChatSessionNamespace, name, starter, "demo-agent", "from slack",
		spiceboxv1alpha1.AgentSessionPhaseSucceeded, time.Now())
	slackSess.Labels[spiceboxv1alpha1.LabelChannelKind] = "slack"

	cases := []struct {
		name          string
		objs          []client.Object
		subject       string
		checkInteract func(ctx context.Context, ns, name, subject string) (bool, error)
		want          int
		wantBodyLacks string // substring that must NOT appear in the response body
	}{
		{
			name: "CheckInteract denies: 403, not its transcript",
			objs: []client.Object{chatSessionCR(newChatSessionNamespace, name, starter, "demo-agent", "finished",
				spiceboxv1alpha1.AgentSessionPhaseSucceeded, time.Now())},
			subject:       "user:mallory@example.com",
			checkInteract: checkInteractOnlyFor(starter),
			want:          http.StatusForbidden,
			wantBodyLacks: "all done",
		},
		{
			name: "a participant CheckInteract admits, not the CR's starter: 200 — this is the case the annotation comparison refused",
			objs: []client.Object{chatSessionCR(newChatSessionNamespace, name, starter, "demo-agent", "finished",
				spiceboxv1alpha1.AgentSessionPhaseSucceeded, time.Now())},
			subject:       "user:participant@example.com",
			checkInteract: checkInteractOnlyFor("user:participant@example.com"),
			want:          http.StatusOK,
		},
		{
			name: "CheckInteract errors: 503, never 403",
			objs: []client.Object{chatSessionCR(newChatSessionNamespace, name, starter, "demo-agent", "finished",
				spiceboxv1alpha1.AgentSessionPhaseSucceeded, time.Now())},
			subject: starter,
			checkInteract: func(context.Context, string, string, string) (bool, error) {
				return false, errors.New("spicedb: dial tcp: connection refused")
			},
			want:          http.StatusServiceUnavailable,
			wantBodyLacks: "connection refused",
		},
		{
			name:          "a finished Slack session in the chat namespace: 404, not a chat conversation",
			objs:          []client.Object{slackSess},
			subject:       starter,
			checkInteract: checkInteractOnlyFor(starter),
			want:          http.StatusNotFound,
		},
		{
			name:          "no such session at all: 404",
			objs:          nil,
			subject:       starter,
			checkInteract: checkInteractOnlyFor(starter),
			want:          http.StatusNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := transcriptServer(t, name)
			d := &fakeDeps{k8s: newFakeK8sClient(t, tc.objs...), operatorURL: srv.URL, memoryToken: "webd-token", checkInteract: tc.checkInteract}
			reg := newRegistryWithBuilder(d, newFakeBuilder().build)

			w := serveMessages(tc.subject, newChatSessionNamespace, name, d, reg)
			assert.Equal(t, tc.want, w.Code)
			if tc.wantBodyLacks != "" {
				assert.NotContains(t, w.Body.String(), tc.wantBodyLacks)
			}
		})
	}
}

// --- POST /sessions/api/{ns}/{name}/message ---------------------------------

// postToSession builds a POST to the URL's ns/name/action route, with
// r.PathValue("ns")/("name") populated as the framework's wildcard match
// would. action is one of "message", "interrupt", "decision".
//
// It sets the trusted Origin header, because a browser always does and these
// routes now pin it with webui.TrustedOriginMatch — a request without one is
// refused before any handler logic runs, so a fixture that omitted it would
// test the CSRF guard over and over instead of the case it is named for. The
// guard's own cases set the header themselves.
func postToSession(ns, name, action, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/sessions/api/"+ns+"/"+name+"/"+action, strings.NewReader(body))
	req.SetPathValue("ns", ns)
	req.SetPathValue("name", name)
	req.Header.Set("Origin", testTrustedOrigin)
	return req
}

func TestMessageHandler_MissingText(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	req := postToSession(newChatSessionNamespace, "demo-agent-abcd", "message", `{"text":"  "}`)
	w := httptest.NewRecorder()
	withTestSubject("user:owner", messageHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestMessageHandler_InvalidJSON(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	req := postToSession(newChatSessionNamespace, "demo-agent-abcd", "message", `not json`)
	w := httptest.NewRecorder()
	withTestSubject("user:owner", messageHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestMessageHandler_ExistingSession_MapsDecisionOutcomes(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, fb.build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	cases := []struct {
		name       string
		decision   channelkinds.InboundDecision
		wantRouted bool
		wantEnded  bool
		wantDeny   string
	}{
		{"routed", channelkinds.InboundDecision{Outcome: channelkinds.OutcomeRouted}, true, false, ""},
		{"no active session -> ended", channelkinds.InboundDecision{Outcome: channelkinds.OutcomeNoActiveSession}, false, true, ""},
		{"denied by permission", channelkinds.InboundDecision{
			Outcome: channelkinds.OutcomeDeniedByPermission,
			Notice: notice.New(categories.ArchivedSessionNotYours, notice.Args{
				Lead:     "nope",
				NextStep: "Start a new thread.",
				Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
			}),
		}, false, false, "nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb.listener.decision = tc.decision
			req := postToSession(newChatSessionNamespace, name, "message", `{"text":"more"}`)
			w := httptest.NewRecorder()
			withTestSubject("user:owner", messageHandler(reg)).ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code)
			var resp messageResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			assert.Equal(t, tc.wantRouted, resp.Routed)
			assert.Equal(t, tc.wantEnded, resp.Ended)
			if tc.wantDeny == "" {
				assert.True(t, resp.Notice.IsZero(), "no notice expected")
			} else {
				require.False(t, resp.Notice.IsZero(), "a notice must reach the browser")
				assert.Equal(t, tc.wantDeny, resp.Notice.Lead)
				assert.Equal(t, "degraded", resp.Notice.Tone,
					"tone must be denormalised onto the wire: the browser has no category registry")
			}
		})
	}
}

// TestMessageHandler_BodySessionIDIgnored_ActsOnURLSession pins the deletion
// of messageRequest.SessionID: a body that names a DIFFERENT session than the
// URL must act on the URL's session regardless — the field no longer exists
// on the wire type, so a stray "sessionId" in the body is inert JSON, not a
// routing input. Without this row the deletion is untested (mutation 5).
func TestMessageHandler_BodySessionIDIgnored_ActsOnURLSession(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, fb.build)
	urlSession, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)
	otherSession, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)
	require.NotEqual(t, urlSession, otherSession)

	body := fmt.Sprintf(`{"sessionId":%q,"text":"hijack via body"}`, otherSession)
	req := postToSession(newChatSessionNamespace, urlSession, "message", body)
	w := httptest.NewRecorder()
	withTestSubject("user:owner", messageHandler(reg)).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp messageResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, urlSession, resp.SessionID, "the URL's session must be acted on, never the body's")
	assert.Contains(t, fb.listener.calls, "hijack via body", "the listener that received the text is the URL session's, not the body session's")
}

func TestMessageHandler_ExistingSession_UnknownID(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	req := postToSession(newChatSessionNamespace, "nope", "message", `{"text":"hi"}`)
	w := httptest.NewRecorder()
	withTestSubject("user:owner", messageHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestMessageHandler_NoStandingForbidden(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s, checkInteract: checkInteractOnlyFor("user:owner")}, newFakeBuilder().build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	req := postToSession(newChatSessionNamespace, name, "message", `{"text":"hijack"}`)
	w := httptest.NewRecorder()
	withTestSubject("user:intruder", messageHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// trailingSlashOriginDeps is a fakeDeps whose TrustedOrigin carries a trailing
// slash, exercising the origin guard's trailing-slash trimming.
type trailingSlashOriginDeps struct{ fakeDeps }

func (d *trailingSlashOriginDeps) TrustedOrigin() string { return testTrustedOrigin + "/" }

// blankOriginDeps is a fakeDeps that knows no trusted origin at all — a webd
// whose external URL is unconfigured. The guard must refuse everything for it.
type blankOriginDeps struct{ fakeDeps }

func (d *blankOriginDeps) TrustedOrigin() string { return "" }

// TestMessageHandler_OriginGuard verifies the CSRF Origin pin on the mutating
// POST handler: only an exact match is allowed. A MISSING Origin is refused
// like a mismatched one — this route is reachable only from the shell's own
// pages, where a browser always sends the header, and the allowance the local
// guard used to make for non-browser callers had no caller left to serve.
func TestMessageHandler_OriginGuard(t *testing.T) {
	cases := []struct {
		name       string
		setOrigin  bool
		origin     string
		wantStatus int
	}{
		{name: "matching Origin: allowed (200)", setOrigin: true, origin: testTrustedOrigin, wantStatus: http.StatusOK},
		{name: "mismatched Origin: rejected (403)", setOrigin: true, origin: "https://evil.example", wantStatus: http.StatusForbidden},
		{name: "absent Origin: rejected (403), same as a mismatch", setOrigin: false, wantStatus: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
			reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)
			name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
			require.NoError(t, err)
			req := postToSession(newChatSessionNamespace, name, "message", `{"text":"hi"}`)
			req.Header.Del("Origin")
			if tc.setOrigin {
				req.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			withTestSubject("user:owner", messageHandler(reg)).ServeHTTP(w, req)
			assert.Equal(t, tc.wantStatus, w.Code)
		})
	}
}

// TestMessageHandler_OriginGuard_BlankTrustedOriginRefuses is what moving to
// the shared helper actually bought: a deployment with no trusted origin
// configured can vouch for nothing, so every mutating POST is refused rather
// than admitted on the strength of a header nobody could compare.
func TestMessageHandler_OriginGuard_BlankTrustedOriginRefuses(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	reg := newRegistryWithBuilder(&blankOriginDeps{fakeDeps{k8s: k8s}}, newFakeBuilder().build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)
	req := postToSession(newChatSessionNamespace, name, "message", `{"text":"hi"}`)
	req.Header.Del("Origin")
	w := httptest.NewRecorder()
	withTestSubject("user:owner", messageHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code, "a blank trusted origin must fail closed, not admit a header-less caller")
}

// TestMessageHandler_OriginGuard_TrimsTrailingSlash proves the guard trims a
// trailing slash on the trusted origin before comparing, like the ws check.
func TestMessageHandler_OriginGuard_TrimsTrailingSlash(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	reg := newRegistryWithBuilder(&trailingSlashOriginDeps{fakeDeps{k8s: k8s}}, newFakeBuilder().build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)
	req := postToSession(newChatSessionNamespace, name, "message", `{"text":"hi"}`)
	req.Header.Set("Origin", testTrustedOrigin) // no trailing slash
	w := httptest.NewRecorder()
	withTestSubject("user:owner", messageHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, "a trusted origin matches even when TrustedOrigin() has a trailing slash")
}

// TestInterruptHandler_RejectsMismatchedOrigin proves interruptHandler carries
// the same CSRF Origin guard as messageHandler.
func TestInterruptHandler_RejectsMismatchedOrigin(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	req := postToSession(newChatSessionNamespace, name, "interrupt", `{"requestId":"req-1"}`)
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	withTestSubject("user:owner", interruptHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- POST /sessions/api/{ns}/{name}/interrupt ------------------------------

func TestInterruptHandler_Valid(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, fb.build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	req := postToSession(newChatSessionNamespace, name, "interrupt", `{"requestId":"req-1"}`)
	w := httptest.NewRecorder()
	withTestSubject("user:owner", interruptHandler(reg)).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp interruptResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.OK)
	assert.Contains(t, fb.listener.interrupts, "req-1")
}

func TestInterruptHandler_MissingRequestID(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	req := postToSession(newChatSessionNamespace, "demo-agent-abcd", "interrupt", `{}`)
	w := httptest.NewRecorder()
	withTestSubject("user:owner", interruptHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestInterruptHandler_InvalidJSON(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	req := postToSession(newChatSessionNamespace, "demo-agent-abcd", "interrupt", `not json`)
	w := httptest.NewRecorder()
	withTestSubject("user:owner", interruptHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestInterruptHandler_Unauthenticated(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	req := postToSession(newChatSessionNamespace, "demo-agent-abcd", "interrupt", `{"requestId":"req-1"}`)
	w := httptest.NewRecorder()
	interruptHandler(reg).ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestInterruptHandler_UnknownSession(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	req := postToSession(newChatSessionNamespace, "nope", "interrupt", `{"requestId":"req-1"}`)
	w := httptest.NewRecorder()
	withTestSubject("user:owner", interruptHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestInterruptHandler_NoStandingForbidden(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s, checkInteract: checkInteractOnlyFor("user:owner")}, newFakeBuilder().build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	req := postToSession(newChatSessionNamespace, name, "interrupt", `{"requestId":"req-1"}`)
	w := httptest.NewRecorder()
	withTestSubject("user:intruder", interruptHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- POST /sessions/api/{ns}/{name}/decision -------------------------------

func TestDecisionHandler_Valid(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, fb.build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	req := postToSession(newChatSessionNamespace, name, "decision", `{"category":"identity_choice","requestRef":"req-1","actionId":"agent"}`)
	w := httptest.NewRecorder()
	withTestSubject("user:owner", decisionHandler(reg)).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp decisionResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.OK)
	require.Len(t, fb.listener.decisions, 1)
	wantExt := channelkinds.ExternalIdentity{Kind: "idp", ExternalID: "user:owner", Email: "user:owner"}
	assert.Equal(t, decisionCall{ext: wantExt, category: "identity_choice", requestRef: "req-1", actionID: "agent"}, fb.listener.decisions[0],
		"the Decider must be the submitting subject's OWN derived identity, not a construction-time default")
}

func TestDecisionHandler_RejectsMismatchedOrigin(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	req := postToSession(newChatSessionNamespace, name, "decision", `{"category":"identity_choice","requestRef":"req-1","actionId":"agent"}`)
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	withTestSubject("user:owner", decisionHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDecisionHandler_MissingCategory(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	req := postToSession(newChatSessionNamespace, "demo-agent-abcd", "decision", `{"requestRef":"req-1","actionId":"agent"}`)
	w := httptest.NewRecorder()
	withTestSubject("user:owner", decisionHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDecisionHandler_MissingRequestRef(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	req := postToSession(newChatSessionNamespace, "demo-agent-abcd", "decision", `{"category":"identity_choice","actionId":"agent"}`)
	w := httptest.NewRecorder()
	withTestSubject("user:owner", decisionHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDecisionHandler_MissingActionID(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	req := postToSession(newChatSessionNamespace, "demo-agent-abcd", "decision", `{"category":"identity_choice","requestRef":"req-1"}`)
	w := httptest.NewRecorder()
	withTestSubject("user:owner", decisionHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDecisionHandler_InvalidJSON(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	req := postToSession(newChatSessionNamespace, "demo-agent-abcd", "decision", `not json`)
	w := httptest.NewRecorder()
	withTestSubject("user:owner", decisionHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDecisionHandler_Unauthenticated(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	req := postToSession(newChatSessionNamespace, "demo-agent-abcd", "decision", `{"category":"identity_choice","requestRef":"req-1","actionId":"agent"}`)
	w := httptest.NewRecorder()
	decisionHandler(reg).ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestDecisionHandler_UnknownSession(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	req := postToSession(newChatSessionNamespace, "nope", "decision", `{"category":"identity_choice","requestRef":"req-1","actionId":"agent"}`)
	w := httptest.NewRecorder()
	withTestSubject("user:owner", decisionHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDecisionHandler_NoStandingForbidden(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s, checkInteract: checkInteractOnlyFor("user:owner")}, newFakeBuilder().build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	req := postToSession(newChatSessionNamespace, name, "decision", `{"category":"identity_choice","requestRef":"req-1","actionId":"agent"}`)
	w := httptest.NewRecorder()
	withTestSubject("user:intruder", decisionHandler(reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- GET /sessions/api/{ns}/{name}/ws --------------------------------------

const testTrustedOrigin = "https://trusted.example"

func TestWSHandler_MissingSessionParam(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	req := httptest.NewRequest(http.MethodGet, "/sessions/api//ws", nil) // ns and name both unset
	w := httptest.NewRecorder()
	withTestSubject("user:owner", wsHandler(&fakeDeps{}, reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestWSHandler_UnknownSession(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	req := httptest.NewRequest(http.MethodGet, "/sessions/api/"+newChatSessionNamespace+"/nope/ws", nil)
	req.SetPathValue("ns", newChatSessionNamespace)
	req.SetPathValue("name", "nope")
	w := httptest.NewRecorder()
	withTestSubject("user:owner", wsHandler(&fakeDeps{}, reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestWSHandler_NoStandingForbidden(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s, checkInteract: checkInteractOnlyFor("user:owner")}, newFakeBuilder().build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/sessions/api/"+newChatSessionNamespace+"/"+name+"/ws", nil)
	req.SetPathValue("ns", newChatSessionNamespace)
	req.SetPathValue("name", name)
	w := httptest.NewRecorder()
	withTestSubject("user:intruder", wsHandler(&fakeDeps{}, reg)).ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// wsTestDeps is a fakeDeps with a configurable TrustedOrigin, used by the
// real-upgrade tests below (CheckOrigin gates on it).
type wsTestDeps struct{ fakeDeps }

func (d *wsTestDeps) TrustedOrigin() string { return testTrustedOrigin }

// wsServeMux wraps h behind the real ws route pattern so a request the test
// dials against a real httptest.Server has PathValue("ns")/("name")
// populated by ServeMux's own wildcard matching — exactly how production
// routing works. A bare handler passed straight to httptest.NewServer never
// gets PathValue populated from the dialed URL.
func wsServeMux(h http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /sessions/api/{ns}/{name}/ws", h)
	return mux
}

// wsDialURL builds the ws:// URL for a real-upgrade test against srv.
func wsDialURL(srv *httptest.Server, ns, name string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/sessions/api/" + ns + "/" + name + "/ws"
}

func TestWSHandler_RejectsUntrustedOrigin(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	d := &wsTestDeps{fakeDeps{k8s: k8s}}
	srv := httptest.NewServer(wsServeMux(withTestSubject("user:owner", wsHandler(d, reg))))
	defer srv.Close()

	wsURL := wsDialURL(srv, newChatSessionNamespace, name)
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{"https://evil.example"}})
	require.Error(t, err, "an untrusted Origin must fail the upgrade")
	if resp != nil {
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	}
}

func TestWSHandler_StreamsBroadcastEventsToAttachedClient(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	d := &wsTestDeps{fakeDeps{k8s: k8s}}
	reg := newRegistryWithBuilder(d, fb.build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	srv := httptest.NewServer(wsServeMux(withTestSubject("user:owner", wsHandler(d, reg))))
	defer srv.Close()

	wsURL := wsDialURL(srv, newChatSessionNamespace, name)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{testTrustedOrigin}})
	require.NoError(t, err)
	defer conn.Close()

	entry, ok := reg.lookup(newChatKey(name))
	require.True(t, ok)
	require.Eventually(t, func() bool {
		entry.mu.Lock()
		defer entry.mu.Unlock()
		return len(entry.sinks) == 1
	}, 2*time.Second, 10*time.Millisecond, "the server must attach the sink shortly after the upgrade completes")

	entry.Emit(browser.MsgUserMessage{
		Session: browser.SessionRef{Namespace: newChatSessionNamespace, Name: name},
		Text:    "hello from the agent",
	})

	require.NoError(t, conn.SetReadDeadline(wstest.Deadline(2*time.Second)))
	var frame wsFrame
	require.NoError(t, conn.ReadJSON(&frame))
	assert.Equal(t, "user_message", frame.Type)
	assert.Equal(t, name, frame.Session.Name)

	// Closing the client connection must cause the server to detach — the
	// read pump's ReadMessage unblocks with an error and returns.
	require.NoError(t, conn.Close())
	require.Eventually(t, func() bool {
		entry.mu.Lock()
		defer entry.mu.Unlock()
		return len(entry.sinks) == 0
	}, 2*time.Second, 10*time.Millisecond, "closing the client connection must detach its sink")
}

// TestWSHandler_SendsKeepalivePings verifies the server pings an otherwise-idle
// chat websocket. Without this, an idle connection (a slow runner cold start, a
// long turn) is dropped by browsers/proxies and — because the sink is live-only
// — the agent's reply / turn-complete emitted during the gap is lost until a
// full reload (the "stuck starting" / "stuck working" regression).
// TestWSHandler_AttachAsksChannelsdToResurfaceParkedPrompts guards the
// credential-prompt-lost-to-a-late-subscriber fix at the handler seam: every
// successful attach must ask channelsd to re-surface whatever the session is
// parked on. Without it, the attach is silent and a prompt published before the
// tab connected is never seen again — the dedup on the publishing side means it
// is sent exactly once, ever. A reload of an already-parked conversation hits
// this deterministically, not just as a startup race.
//
// A failure to publish must NOT fail the attach: the request is a best-effort
// nudge, and refusing the websocket over it would trade a missing card for a
// dead conversation.
func TestWSHandler_AttachAsksChannelsdToResurfaceParkedPrompts(t *testing.T) {
	cases := []struct {
		name         string
		resurfaceErr error
	}{
		{name: "attach publishes the resurface request"},
		{name: "publish fails: logged, attach still streams", resurfaceErr: errors.New("nats down")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
			fb := newFakeBuilder()
			fb.listener.resurfaceErr = tc.resurfaceErr
			d := &wsTestDeps{fakeDeps{k8s: k8s}}
			reg := newRegistryWithBuilder(d, fb.build)
			name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
			require.NoError(t, err)

			srv := httptest.NewServer(wsServeMux(withTestSubject("user:owner", wsHandler(d, reg))))
			defer srv.Close()

			wsURL := wsDialURL(srv, newChatSessionNamespace, name)
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{testTrustedOrigin}})
			require.NoError(t, err)
			defer conn.Close()

			require.Eventually(t, func() bool {
				return fb.listener.resurfaceCount() == 1
			}, 2*time.Second, 10*time.Millisecond, "attaching must ask channelsd to re-surface the session's parked prompts")

			// The connection is live either way: a failed nudge never costs the tab
			// its stream.
			entry, ok := reg.lookup(newChatKey(name))
			require.True(t, ok)
			entry.Emit(browser.MsgUserMessage{
				Session: browser.SessionRef{Namespace: newChatSessionNamespace, Name: name},
				Text:    "still streaming",
			})
			require.NoError(t, conn.SetReadDeadline(wstest.Deadline(2*time.Second)))
			var frame wsFrame
			require.NoError(t, conn.ReadJSON(&frame))
			assert.Equal(t, "user_message", frame.Type)
		})
	}
}

func TestWSHandler_SendsKeepalivePings(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	d := &wsTestDeps{fakeDeps{k8s: k8s}}
	reg := newRegistryWithBuilder(d, newFakeBuilder().build)
	reg.pingInterval = 15 * time.Millisecond // per-Registry; no shared-global race
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	srv := httptest.NewServer(wsServeMux(withTestSubject("user:owner", wsHandler(d, reg))))
	defer srv.Close()

	wsURL := wsDialURL(srv, newChatSessionNamespace, name)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{testTrustedOrigin}})
	require.NoError(t, err)
	defer conn.Close()

	pinged := make(chan struct{}, 1)
	conn.SetPingHandler(func(string) error {
		select {
		case pinged <- struct{}{}:
		default:
		}
		return nil
	})
	// Control frames (pings) are processed inside ReadMessage — run it so the
	// ping handler fires.
	go func() {
		for {
			if _, _, e := conn.ReadMessage(); e != nil {
				return
			}
		}
	}()

	select {
	case <-pinged:
	case <-time.After(2 * time.Second):
		t.Fatal("client never received a keepalive ping — an idle chat ws will be dropped and lose live frames")
	}
}

func TestWSHandler_SessionEndedNoticeOnTeardown(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	d := &wsTestDeps{fakeDeps{k8s: k8s}}
	reg := newRegistryWithBuilder(d, fb.build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	srv := httptest.NewServer(wsServeMux(withTestSubject("user:owner", wsHandler(d, reg))))
	defer srv.Close()

	wsURL := wsDialURL(srv, newChatSessionNamespace, name)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{testTrustedOrigin}})
	require.NoError(t, err)
	defer conn.Close()

	entry, ok := reg.lookup(newChatKey(name))
	require.True(t, ok)
	require.Eventually(t, func() bool {
		entry.mu.Lock()
		defer entry.mu.Unlock()
		return len(entry.sinks) == 1
	}, 2*time.Second, 10*time.Millisecond)

	reg.teardown(newChatKey(name), terminalNotice{Reason: "succeeded"})

	require.NoError(t, conn.SetReadDeadline(wstest.Deadline(2*time.Second)))
	var frame wsFrame
	require.NoError(t, conn.ReadJSON(&frame))
	assert.Equal(t, "session_ended", frame.Type)

	// The server closes the connection right after teardown notifies it.
	require.Eventually(t, func() bool {
		_, _, err := conn.ReadMessage()
		return err != nil
	}, 2*time.Second, 10*time.Millisecond, "teardown must close the connection")
}

// --- J5: the chat data plane addresses the session the shell selected, in
// the namespace it actually lives in, and admits every subject who may
// interact — not only its starter -----------------------------------------

// TestChatPlaneAddressesTheNamespaceItWasGiven is J5's first half: the chat
// data plane serves the session the URL names, in the namespace the URL
// names. Two sessions with the SAME NAME in DIFFERENT namespaces is the only
// fixture that catches a restored namespace constant — with one namespace
// seeded, a hardcoded "default" and a correct PathValue read are
// indistinguishable.
func TestChatPlaneAddressesTheNamespaceItWasGiven(t *testing.T) {
	const shared = "shared-name"
	at := time.Now().UTC().Truncate(time.Second)
	transcripts := map[string][]memory.Entry{
		"/memory/turn/demo-ns/" + shared:                 {turnTestEntry(t, shared, 0, "assistant", "first namespace", at)},
		"/memory/reply_delivery/demo-ns/" + shared:       {},
		"/memory/interaction_history/demo-ns/" + shared:  {},
		"/memory/turn/other-ns/" + shared:                {turnTestEntry(t, shared, 0, "assistant", "second namespace", at)},
		"/memory/reply_delivery/other-ns/" + shared:      {},
		"/memory/interaction_history/other-ns/" + shared: {},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entries, ok := transcripts[r.URL.Path]
		require.True(t, ok, "unexpected transcript request path %q", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{Entries: entries}))
	}))
	defer srv.Close()

	k8s := newFakeK8sClient(t,
		chatSessionCR("demo-ns", shared, "user:owner", "demo-agent", "hi", spiceboxv1alpha1.AgentSessionPhaseIdle, time.Now()),
		chatSessionCR("other-ns", shared, "user:owner", "demo-agent", "hi", spiceboxv1alpha1.AgentSessionPhaseIdle, time.Now()),
	)
	d := &fakeDeps{k8s: k8s, operatorURL: srv.URL, memoryToken: "webd-token"}
	reg := newRegistryWithBuilder(d, newFakeBuilder().build)

	first := serveMessages("user:owner", "demo-ns", shared, d, reg)
	second := serveMessages("user:owner", "other-ns", shared, d, reg)

	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, http.StatusOK, second.Code)
	assert.Contains(t, first.Body.String(), "first namespace")
	assert.Contains(t, second.Body.String(), "second namespace",
		"the second namespace's transcript must not be the first's — a hardcoded namespace reads the wrong session rather than failing")
}

// TestSessionMessagesHandler_CheckInteractConsultedForTheURLsObject is I2's
// regression: every OTHER scripted fake in this package
// (checkInteractOnlyFor) admits or denies purely by SUBJECT, ignoring the
// ns/name it was asked about — so nothing else in the suite proves the gate
// is actually consulted about the URL's own session rather than some other
// one. A future edit that resolved CheckInteract against, say, a cached
// namespace instead of the URL's could pass every other test here. Two
// sessions sharing a name across two namespaces, with CheckInteract scripted
// to admit only ONE of the two (ns, name) pairs, is the only fixture that
// catches it.
func TestSessionMessagesHandler_CheckInteractConsultedForTheURLsObject(t *testing.T) {
	const shared = "shared-name"
	srv := transcriptServer(t, shared)
	k8s := newFakeK8sClient(t,
		chatSessionCR("demo-ns", shared, "user:owner", "demo-agent", "hi", spiceboxv1alpha1.AgentSessionPhaseIdle, time.Now()),
		chatSessionCR("other-ns", shared, "user:owner", "demo-agent", "hi", spiceboxv1alpha1.AgentSessionPhaseIdle, time.Now()),
	)
	d := &fakeDeps{k8s: k8s, operatorURL: srv.URL, memoryToken: "webd-token",
		checkInteract: checkInteractOnlyForObject("demo-ns", shared)}
	reg := newRegistryWithBuilder(d, newFakeBuilder().build)

	admitted := serveMessages("user:owner", "demo-ns", shared, d, reg)
	denied := serveMessages("user:owner", "other-ns", shared, d, reg)

	assert.Equal(t, http.StatusOK, admitted.Code, "CheckInteract was scripted to admit demo-ns/shared-name")
	assert.Equal(t, http.StatusForbidden, denied.Code,
		"CheckInteract was scripted to deny other-ns/shared-name — same name, different namespace, so admitting it would mean the gate wasn't actually asked about THIS object")
}

// TestSessionMessagesHandler_LiveEntryCheckInteractErrors is I5's regression:
// TestSessionMessagesHandler_TerminalSessionAuthorizationHolds's "CheckInteract
// errors" row seeds NO live entry, so it exercises only the CR-fallback
// branch of authorizeRead (interactableChatSession). The LIVE-entry branch —
// the common production path, since every ACTIVE conversation has an entry —
// has its own, separate checkInteract call, and nothing else in the suite
// proved a SpiceDB error there answers 503 rather than being folded into a
// denial.
func TestSessionMessagesHandler_LiveEntryCheckInteractErrors(t *testing.T) {
	const name = "demo-agent-live5030"
	d := &fakeDeps{k8s: newFakeK8sClient(t), checkInteract: func(context.Context, string, string, string) (bool, error) {
		return false, errors.New("spicedb: dial tcp: connection refused")
	}}
	reg := newRegistryWithBuilder(d, newFakeBuilder().build)
	seedLiveSession(t, reg, newChatSessionNamespace, name, "demo-agent", "t", "user:owner", time.Now())

	w := serveMessages("user:owner", newChatSessionNamespace, name, d, reg)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.NotContains(t, w.Body.String(), "connection refused", "the raw SpiceDB cause must not leak to the client")
}

// TestSessionDetailHandler_LiveEntryCheckInteractErrors is M13's cheap
// second-handler row: mapSubmitError is shared code, but a handler that grew
// its own error branch wouldn't be caught by asserting only one caller of it.
func TestSessionDetailHandler_LiveEntryCheckInteractErrors(t *testing.T) {
	const name = "demo-agent-live5031"
	d := &fakeDeps{k8s: newFakeK8sClient(t), checkInteract: func(context.Context, string, string, string) (bool, error) {
		return false, errors.New("spicedb: dial tcp: connection refused")
	}}
	reg := newRegistryWithBuilder(d, newFakeBuilder().build)
	seedLiveSession(t, reg, newChatSessionNamespace, name, "demo-agent", "t", "user:owner", time.Now())

	w := serveDetail("user:owner", newChatSessionNamespace, name, sessionDetailHandler(d, reg))

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestMessageHandler_LiveEntryCheckInteractErrors is New-3's regression:
// TestSessionMessagesHandler_LiveEntryCheckInteractErrors and
// TestSessionDetailHandler_LiveEntryCheckInteractErrors both reach
// authorizeRead's live-entry branch (registry.go's authorizeRead), never
// authorize's (registry.go's authorize) — a SEPARATE live-entry branch with
// its own checkInteract call. authorize is what every WRITE
// (SubmitMessage/SubmitInterrupt/SubmitDecision), Attach and wsHandler's
// pre-upgrade check go through, so it is the higher-consequence half: during
// a SpiceDB outage, a POST to an active conversation must answer 503 "try
// again", never 403 "you do not have access".
func TestMessageHandler_LiveEntryCheckInteractErrors(t *testing.T) {
	const name = "demo-agent-live6040"
	d := &fakeDeps{k8s: newFakeK8sClient(t), checkInteract: func(context.Context, string, string, string) (bool, error) {
		return false, errors.New("spicedb: dial tcp: connection refused")
	}}
	reg := newRegistryWithBuilder(d, newFakeBuilder().build)
	seedLiveSession(t, reg, newChatSessionNamespace, name, "demo-agent", "t", "user:owner", time.Now())

	req := postToSession(newChatSessionNamespace, name, "message", `{"text":"hi"}`)
	w := httptest.NewRecorder()
	withTestSubject("user:owner", messageHandler(reg)).ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.NotContains(t, w.Body.String(), "connection refused", "the raw SpiceDB cause must not leak to the client")
}

// TestSessionMessagesHandler_NoSubjectUnauthorizedBeforeAnyRead is J5's 401
// row: a request with no authenticated subject must be refused before ANY
// Kubernetes read — proven with a k8s client that records every Get/List it
// serves, asserted zero after the call.
func TestSessionMessagesHandler_NoSubjectUnauthorizedBeforeAnyRead(t *testing.T) {
	var reads atomic.Int64
	k8s := newFakeInterceptedK8sClient(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			reads.Add(1)
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			reads.Add(1)
			return c.List(ctx, list, opts...)
		},
	})
	d := &fakeDeps{k8s: k8s}
	reg := newRegistryWithBuilder(d, newFakeBuilder().build)

	w := serveMessages("", newChatSessionNamespace, "demo-agent-abcd", d, reg)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Zero(t, reads.Load(), "an unauthenticated request must be refused before any Kubernetes read")
}

// --- the two capacity refusals, as HTTP -----------------------------------

// TestMessageHandler_CapacityRefusals is the HTTP half of the two-cap model.
// The branches are reachable in production through the RESUME path, not a
// create path: a viewer already at their limit who opens a 17th dormant
// conversation drives authorize -> rehydrate -> claimSlot, which returns one
// of the two sentinels.
//
// Both statuses matter and they are deliberately different. 429 says "you have
// too many open" and closing one fixes it; 503 says "this server is full",
// which the viewer can do nothing about. Collapsing them onto one status is
// exactly what the single process-wide cap used to do, and it told a viewer to
// go close a session when the real problem was capacity.
func TestMessageHandler_CapacityRefusals(t *testing.T) {
	const owner = "user:crowded@example.com"
	const dormant = "demo-agent-dormant1"

	cases := []struct {
		name string
		// fill saturates the registry so the rehydrate's claimSlot refuses.
		fill       func(t *testing.T, reg *Registry)
		wantStatus int
		wantBody   string
	}{
		{
			name: "the viewer is at their own limit: 429, and the copy says to close one",
			fill: func(t *testing.T, reg *Registry) {
				fillSlots(t, reg, owner, limits.perSubject)
			},
			wantStatus: http.StatusTooManyRequests,
			wantBody:   ErrTooManySessionsForSubject.Error(),
		},
		{
			name: "the process is at its capacity ceiling: 503, and the copy does not blame the viewer",
			fill: func(t *testing.T, reg *Registry) {
				// Spread across many subjects so no single one is near the
				// per-subject limit — a refusal here can only be the ceiling.
				per := limits.perSubject - 1
				for i := 0; i*per < limits.total; i++ {
					fillSlotsFrom(t, reg, fmt.Sprintf("user:filler-%d", i), i*per, min(per, limits.total-i*per))
				}
			},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   ErrServerAtCapacity.Error(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k8s := newFakeK8sClient(t,
				chatSessionCR(newChatSessionNamespace, dormant, owner, "demo-agent", "resume me",
					spiceboxv1alpha1.AgentSessionPhaseIdle, time.Now()),
				chatChannelCR(newChatSessionNamespace, dormant))
			reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)
			tc.fill(t, reg)

			req := postToSession(newChatSessionNamespace, dormant, "message", `{"text":"hello again"}`)
			w := httptest.NewRecorder()
			withTestSubject(owner, messageHandler(reg)).ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code, "body: %s", w.Body.String())
			assert.Contains(t, w.Body.String(), tc.wantBody,
				"the two limits must reach the caller with their own copy, not a shared one")
		})
	}
}

// The detail payload must carry the session's derived notices, because this is
// the ONLY route they reach a webchat user by.
//
// channelsd publishes these to a channel and its sessionWatcher skips every
// client-hosted session — `browser` is one — so a parked webchat session had
// nothing to render and the transcript simply stopped. Guarded here because the
// UI half can pass on fixtures while the handler sends nothing at all: dropping
// this wiring left every browser test green.
func TestSessionDetailHandler_CarriesDerivedNotices(t *testing.T) {
	const name = "demo-agent-parked01"
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: newChatSessionNamespace},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo-agent"},
	}
	sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry
	sess.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentSessionConditionAwaitingRetry,
		Status:             metav1.ConditionTrue,
		Reason:             "ProviderErr",
		Message:            "anthropic: stream: connection reset by peer",
		LastTransitionTime: metav1.Now(),
	}}
	k8s := newFakeK8sClient(t, sess)
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)
	seedLiveSession(t, reg, newChatSessionNamespace, name, "demo-agent", "hello", "user:owner", time.Now())

	w := serveDetail("user:owner", newChatSessionNamespace, name, sessionDetailHandler(&fakeDeps{k8s: k8s}, reg))
	require.Equal(t, http.StatusOK, w.Code)
	var got sessionDetail
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))

	require.Len(t, got.Notices, 1, "a parked session must send the user something to act on")
	assert.Equal(t, sessionnotice.KindAwaitingRetry, got.Notices[0].Kind)
	assert.NotEmpty(t, got.Notices[0].NextStep, "and how to resume")
	assert.Contains(t, got.Notices[0].Body, "connection reset",
		"the underlying cause travels, so a user can say what happened")
}

// A healthy session sends none. A surface that always has a banner trains
// people to skim past the one that matters.
func TestSessionDetailHandler_HealthySessionCarriesNoNotices(t *testing.T) {
	const name = "demo-agent-healthy1"
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: newChatSessionNamespace},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo-agent"},
	}
	sess.Status.Phase = "Running"
	k8s := newFakeK8sClient(t, sess)
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)
	seedLiveSession(t, reg, newChatSessionNamespace, name, "demo-agent", "hello", "user:owner", time.Now())

	w := serveDetail("user:owner", newChatSessionNamespace, name, sessionDetailHandler(&fakeDeps{k8s: k8s}, reg))
	require.Equal(t, http.StatusOK, w.Code)
	var got sessionDetail
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Empty(t, got.Notices)
}

// TestSessionMessagesHandler_ReturnsOpeningTurnBeforeTheRunnerPlacesIt spans
// the join the transcript-level table cannot: a viewer who just pressed start
// on the dashboard is navigated here by a full page load, and this route is the
// only thing that can tell them what they said. It runs seconds before the
// runner appends turn 0, so the durable transcript is still empty — and the
// message must be on the wire anyway, as a user message.
func TestSessionMessagesHandler_ReturnsOpeningTurnBeforeTheRunnerPlacesIt(t *testing.T) {
	const name, prompt = "demo-agent-fresh", "chart the tides for the morning watch"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The runner's pod is still starting: no turns exist yet.
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{}))
	}))
	defer srv.Close()

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: newChatSessionNamespace},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "demo-agent",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: prompt},
		},
	}
	d := &fakeDeps{k8s: newFakeK8sClient(t, sess), operatorURL: srv.URL, memoryToken: "webd-token"}
	reg := newRegistryWithBuilder(d, newFakeBuilder().build)
	seedLiveSession(t, reg, newChatSessionNamespace, name, "demo-agent", prompt, "user:owner", time.Now())

	w := serveMessages("user:owner", newChatSessionNamespace, name, d, reg)
	require.Equal(t, http.StatusOK, w.Code)

	var resp messagesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Timeline, 1, "the viewer's own opening message, not an empty conversation")
	assert.Equal(t, "message", resp.Timeline[0].Kind)
	assert.Equal(t, "user", resp.Timeline[0].Role)
	assert.Equal(t, prompt, resp.Timeline[0].Text)
}
