package workshopmcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// shrinkSubagentPollForTest lowers awaitSubagentTerminal's poll interval so
// a test exercising the wait path doesn't have to wait out the real 500ms
// interval, and restores the production value on cleanup. Mirrors
// shrinkProbePollForTest (tools_probe_test.go).
func shrinkSubagentPollForTest(t *testing.T) {
	t.Helper()
	orig := subagentPollInterval
	subagentPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { subagentPollInterval = orig })
}

// newSubagentControllerStubClient builds a fake client standing in for the
// SubagentRequest controller this package's unit tests never run: its
// Create interceptor lets the real Create land, copies the just-created
// object's Spec/ObjectMeta into captured (when non-nil) BEFORE stamping
// status, then immediately writes status via Status().Update — so
// awaitSubagentTerminal's very first Get already observes a terminal
// phase, exactly as if the controller had already finished. objs preloads
// any other object a test needs the same fake client to already hold (e.g.
// the child AgentSession a terminal ChildRef names). Mirrors
// newProbeControllerStubClient (tools_probe_test.go).
func newSubagentControllerStubClient(t *testing.T, status spiceboxv1alpha1.SubagentRequestStatus, captured *spiceboxv1alpha1.SubagentRequest, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.SubagentRequest{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if err := cl.Create(ctx, obj, opts...); err != nil {
					return err
				}
				sr, ok := obj.(*spiceboxv1alpha1.SubagentRequest)
				if !ok {
					return nil
				}
				if captured != nil {
					*captured = *sr
				}
				sr.Status = status
				return cl.Status().Update(ctx, sr)
			},
		}).Build()
}

// newTestRunServer builds a Server around c, scoped to the workshop
// namespace ns with a fixed builder session identity (b/x) — mirrors
// newProbeTestServer (tools_probe_test.go).
func newTestRunServer(ns string, c client.Client) *Server {
	return &Server{
		K8s: c,
		Identity: WorkshopIdentity{
			Namespace:        ns,
			SessionNamespace: "b",
			SessionName:      "x",
			WorkshopID:       ns,
		},
		FieldOwner: defaultFieldOwner,
	}
}

// newTranscriptStub stands up an httptest server answering the operator's
// transcript route with turns, and points OPERATOR_MEMORY_URL/token (the
// env readChildToolResults reads, transcript.go) at it for the test's
// duration.
func newTranscriptStub(t *testing.T, turns []memory.Turn) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"turns": turns})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "the-operator-bearer")
	return srv
}

// TestHandleTestTool_SucceededChild_ReturnsToolRecordsNoProse is the happy
// path: test_tool creates a single_turn SubagentRequest naming the builder
// session (b/x) as parent in the workshop namespace, waits for it to
// succeed, and returns only the child's tool-call/tool-result records —
// never its prose.
func TestHandleTestTool_SucceededChild_ReturnsToolRecordsNoProse(t *testing.T) {
	shrinkSubagentPollForTest(t)
	newTranscriptStub(t, []memory.Turn{{Role: "assistant", Content: []memory.ContentBlock{
		{Type: "text", Text: "I will check the weather now"},
		{Type: "tool_use", ToolUse: &memory.ToolUseBlock{ID: "u1", Name: "get_weather", Input: []byte(`{"city":"testville"}`)}},
		{Type: "tool_result", ToolResult: &memory.ToolResultBlock{ToolUseID: "u1", Content: "sunny"}},
	}}})

	var captured spiceboxv1alpha1.SubagentRequest
	c := newSubagentControllerStubClient(t, spiceboxv1alpha1.SubagentRequestStatus{
		Phase:    spiceboxv1alpha1.SubagentRequestPhaseSucceeded,
		ChildRef: &spiceboxv1alpha1.NamespacedRef{Namespace: "ws-demo123", Name: "child-1"},
	}, &captured)
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleTestTool, testToolArgs{Class: "weather-agent", Request: "check the weather in testville"})
	require.False(t, res.IsError, "a succeeded test child must not be a tool error")

	// The created SR itself, captured before status was stamped.
	assert.Equal(t, spiceboxv1alpha1.SubagentModeSingleTurn, captured.Spec.Mode, "Mode must be single_turn")
	assert.Equal(t, "b", captured.Spec.Parent.Namespace, "Parent must name the builder session's namespace")
	assert.Equal(t, "x", captured.Spec.Parent.Name, "Parent must name the builder session")
	assert.Equal(t, "ws-demo123", captured.Namespace, "the SR itself lives in the workshop namespace W")
	assert.Equal(t, "weather-agent", captured.Spec.Class)
	assert.Contains(t, captured.Spec.Task, "check the weather in testville", "Task must carry the request")

	body := decodeResultBody(t, res)
	assert.Equal(t, "child-1", body["child"])
	records, ok := body["toolRecords"].([]any)
	require.True(t, ok, "toolRecords must be a list")
	require.Len(t, records, 2, "one call + one result; prose dropped")
	assert.Equal(t, "call", records[0].(map[string]any)["kind"])
	assert.Equal(t, "get_weather", records[0].(map[string]any)["tool"])
	assert.Equal(t, "result", records[1].(map[string]any)["kind"])
	assert.Equal(t, "sunny", records[1].(map[string]any)["result"])
	for _, raw := range records {
		m, ok := raw.(map[string]any)
		require.True(t, ok)
		if resultText, ok := m["result"].(string); ok {
			assert.NotContains(t, resultText, "I will check the weather now", "no prose may leak into a record")
		}
	}
}

// TestHandleTestTool_DeniedPhase_SurfacesAsToolError proves a roster/ceiling
// denial — decided ASYNCHRONOUSLY by the SubagentRequest controller after
// Create already succeeded — is a tool ERROR naming the reason, never a
// success. Create landing is not proof the child ran.
func TestHandleTestTool_DeniedPhase_SurfacesAsToolError(t *testing.T) {
	shrinkSubagentPollForTest(t)
	c := newSubagentControllerStubClient(t, spiceboxv1alpha1.SubagentRequestStatus{
		Phase:         spiceboxv1alpha1.SubagentRequestPhaseDenied,
		FailureReason: "OffRoster",
	}, nil)
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleTestTool, testToolArgs{Class: "not-on-roster", Request: "do a thing"})
	require.True(t, res.IsError, "a Denied phase must be a tool error, not a success")
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "OffRoster")
}

// TestHandleTestTool_DeniedPhase_AttendedChildInProgress is a second Denied
// reason, proving the surfaced message names WHATEVER reason the
// controller recorded, not a hardcoded string.
func TestHandleTestTool_DeniedPhase_AttendedChildInProgress(t *testing.T) {
	shrinkSubagentPollForTest(t)
	c := newSubagentControllerStubClient(t, spiceboxv1alpha1.SubagentRequestStatus{
		Phase:         spiceboxv1alpha1.SubagentRequestPhaseDenied,
		FailureReason: "AttendedChildInProgress",
	}, nil)
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleTestTool, testToolArgs{Class: "attended-agent", Request: "do a thing"})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "AttendedChildInProgress")
}

// TestHandleTestTool_TerminalWithoutChildRef_SurfacesAsToolError covers the
// nil-guard: a request that reaches a terminal phase but never populated a
// ChildRef (the child never actually started) is a tool error, not a nil
// dereference and not a silent empty success.
func TestHandleTestTool_TerminalWithoutChildRef_SurfacesAsToolError(t *testing.T) {
	shrinkSubagentPollForTest(t)
	c := newSubagentControllerStubClient(t, spiceboxv1alpha1.SubagentRequestStatus{
		Phase:    spiceboxv1alpha1.SubagentRequestPhaseSucceeded,
		ChildRef: nil,
	}, nil)
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleTestTool, testToolArgs{Class: "weather-agent", Request: "check the weather"})
	require.True(t, res.IsError, "a terminal phase with no ChildRef must be a tool error, not a nil-deref or empty success")
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "never started")
}

// TestHandleTestTool_RequiresClassAndRequest pins the argument guard.
func TestHandleTestTool_RequiresClassAndRequest(t *testing.T) {
	s := newTestRunServer("ws-demo123", fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build())
	res := callTool(t, s.handleTestTool, testToolArgs{})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "class and request are both required")
}

// TestHandleTestTool_DeniedCreate_SurfacesVerbatim proves the WEBHOOK-denial
// path — a Create refusal, distinct from a later Denied PHASE — comes back
// as {denied:true, message:<verbatim>}, exactly like probe_image's own
// refusing test.
func TestHandleTestTool_DeniedCreate_SurfacesVerbatim(t *testing.T) {
	denyErr := apierrors.NewForbidden(
		schema.GroupResource{Group: spiceboxv1alpha1.GroupName, Resource: "subagentrequests"},
		"", errors.New("runner identity does not match spec.parent"))

	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				return denyErr
			},
		}).Build()
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleTestTool, testToolArgs{Class: "not-on-roster", Request: "do a thing"})
	require.True(t, res.IsError, "a denied create must be an error result")
	body := decodeResultBody(t, res)
	assert.Equal(t, true, body["denied"], "denial must be flagged")
	assert.Contains(t, body["message"], "runner identity does not match spec.parent")
}

// TestHandleTestTool_ChildFailedAtBoot_ReportsPhaseAndReason mirrors
// TestHandleReadTestLog_EmptyLogStillReportsTheSessionsOwnFate for test_tool:
// a delegated child that never called a tool — it failed the same
// MCPAllowlistDrift boot check a person's own test session can hit — must not
// come back as an empty toolRecords list with no explanation. The builder
// needs the same phase/failureReason/failureMessage/auditEntries read_test_log
// carries, from the same source (the child's own AgentSession status).
func TestHandleTestTool_ChildFailedAtBoot_ReportsPhaseAndReason(t *testing.T) {
	shrinkSubagentPollForTest(t)
	newTranscriptStubWithLog(t, nil, []map[string]any{
		{"at": "2026-09-13T12:00:00Z", "type": "runner_terminal", "details": map[string]any{
			"Phase": "Failed", "Reason": "MCPAllowlistDrift",
			"Message": "MCPServer/demo-connector: pinned tools not served: [get_thing]",
		}},
	})
	c := newSubagentControllerStubClient(t, spiceboxv1alpha1.SubagentRequestStatus{
		Phase:    spiceboxv1alpha1.SubagentRequestPhaseFailed,
		ChildRef: &spiceboxv1alpha1.NamespacedRef{Namespace: "ws-demo123", Name: "child-1"},
	}, nil, failedTestSession("ws-demo123", "child-1"))
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleTestTool, testToolArgs{Class: "weather-agent", Request: "check the weather"})
	require.False(t, res.IsError, "a failed child is not itself a tool error — the caller reads the verdict fields")

	body := decodeResultBody(t, res)
	assert.Equal(t, "child-1", body["child"])
	assert.Empty(t, body["toolRecords"], "the child called nothing before it failed")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, body["phase"])
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionMCPAllowlistDrift, body["failureReason"])
	assert.Contains(t, body["failureMessage"], "pinned tools not served",
		"the failure's own words reach the builder, not just its classification")

	entries, ok := body["auditEntries"].([]any)
	require.True(t, ok, "auditEntries must be a list")
	require.Len(t, entries, 1, "the child's own recorded transition comes through")
}

// TestAwaitSubagentTerminal_TimesOutWithoutTerminalPhase proves the
// wait-deadline path: a SubagentRequest that never leaves its zero-value
// phase (no controller ever runs against this fake client) must error —
// not hang, and not silently report a fabricated terminal result.
func TestAwaitSubagentTerminal_TimesOutWithoutTerminalPhase(t *testing.T) {
	shrinkSubagentPollForTest(t)
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.SubagentRequest{}).Build()
	s := newTestRunServer("ws-demo123", c)

	stuck := &spiceboxv1alpha1.SubagentRequest{}
	stuck.Name = "subreq-stuck"
	stuck.Namespace = "ws-demo123"
	stuck.Spec = spiceboxv1alpha1.SubagentRequestSpec{
		Parent: spiceboxv1alpha1.NamespacedRef{Namespace: "b", Name: "x"},
		Class:  "weather-agent",
		Task:   "never finishes",
		Mode:   spiceboxv1alpha1.SubagentModeSingleTurn,
	}
	require.NoError(t, c.Create(context.Background(), stuck))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := s.awaitSubagentTerminal(ctx, "ws-demo123", "subreq-stuck")
	require.Error(t, err, "a SubagentRequest that never reaches a terminal phase must error, not hang or fabricate a result")
}

// TestHandleReadTestLog_ReturnsToolRecordsIncludingErrors proves read_test_log
// reads a NAMED child's transcript directly — no SubagentRequest wait at
// all, unlike test_tool — and that a tool ERROR result (IsError:true) comes
// through intact rather than being dropped or masked as a success.
func TestHandleReadTestLog_ReturnsToolRecordsIncludingErrors(t *testing.T) {
	newTranscriptStub(t, []memory.Turn{{Role: "assistant", Content: []memory.ContentBlock{
		{Type: "text", Text: "attempting the lookup"},
		{Type: "tool_use", ToolUse: &memory.ToolUseBlock{ID: "u1", Name: "get_weather", Input: []byte(`{"city":"testville"}`)}},
		{Type: "tool_result", ToolResult: &memory.ToolResultBlock{ToolUseID: "u1", Content: "rate limited", IsError: true}},
	}}})
	s := newTestRunServer("ws-demo123", fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build())

	res := callTool(t, s.handleReadTestLog, readTestLogArgs{Session: "test-session-1"})
	require.False(t, res.IsError, "a successful transcript read must not be a tool error")

	body := decodeResultBody(t, res)
	assert.Equal(t, "test-session-1", body["session"], "the result echoes the session it read")
	records, ok := body["toolRecords"].([]any)
	require.True(t, ok, "toolRecords must be a list")
	require.Len(t, records, 2, "one call + one result")

	call := records[0].(map[string]any)
	assert.Equal(t, "call", call["kind"])
	assert.Equal(t, "get_weather", call["tool"])

	result := records[1].(map[string]any)
	assert.Equal(t, "result", result["kind"])
	assert.Equal(t, "rate limited", result["result"])
	assert.Equal(t, true, result["isError"], "a tool ERROR result must be preserved, not dropped or reported as a success")
}

// TestHandleReadTestLog_RequiresSession pins the argument guard.
func TestHandleReadTestLog_RequiresSession(t *testing.T) {
	s := newTestRunServer("ws-demo123", fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build())
	res := callTool(t, s.handleReadTestLog, readTestLogArgs{})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "session is required")
}

// TestHandleReadTestLog_TranscriptReadError_SurfacesAsToolError proves a
// failure reading the child's transcript (the operator route refusing, or
// erroring) surfaces as a tool error naming the failure — never swallowed,
// never an empty success.
func TestHandleReadTestLog_TranscriptReadError_SurfacesAsToolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "workshop is not Ready", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "the-operator-bearer")
	s := newTestRunServer("ws-demo123", fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build())

	res := callTool(t, s.handleReadTestLog, readTestLogArgs{Session: "test-session-1"})
	require.True(t, res.IsError, "a transcript-read failure must surface as a tool error, not be swallowed")
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "workshop is not Ready")
}

// stop_test now deletes the person's own AgentSession, not a SubagentRequest
// — see tools_trytest_test.go's TestStopTest_DeletesTheSessionInTheWorkshop,
// TestStopTest_AlreadyGone, TestStopTest_DeleteError_SurfacesAsToolError and
// TestStopTest_RequiresSession for its current coverage.

// newTranscriptStubWithLog is newTranscriptStub plus the session's signed
// lifecycle log, which the operator's transcript route returns alongside the
// turns.
func newTranscriptStubWithLog(t *testing.T, turns []memory.Turn, audit []map[string]any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"turns": turns, "auditEntries": audit})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPERATOR_MEMORY_URL", srv.URL)
	t.Setenv("token", "the-operator-bearer")
}

// failedTestSession is the person's own test session after it failed before
// calling anything — the shape that made a builder report "the failure
// happened before any calls" and stop the session unread.
func failedTestSession(ns, name string) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{}
	s.Namespace = ns
	s.Name = name
	s.Spec = spiceboxv1alpha1.AgentSessionSpec{
		Class:  "demo-class",
		Prompt: spiceboxv1alpha1.PromptSource{Inline: "try it"},
	}
	s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseFailed
	s.Status.FailureReason = spiceboxv1alpha1.ReasonAgentSessionMCPAllowlistDrift
	s.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentSessionConditionFailed,
		Status:             metav1.ConditionTrue,
		Reason:             spiceboxv1alpha1.ReasonAgentSessionMCPAllowlistDrift,
		Message:            "MCPServer/demo-connector: pinned tools not served: [get_thing]; the server offers: [thing_read]",
		LastTransitionTime: metav1.Now(),
	}}
	return s
}

// TestHandleReadTestLog_EmptyLogStillReportsTheSessionsOwnFate is the whole
// point of the tool growing a second half: a session that failed before it
// called anything has no tool records at all, and a reader given only those
// concludes nothing happened and stops the session unread — destroying the
// only evidence. The result now carries the session's own verdict beside the
// (empty) records.
func TestHandleReadTestLog_EmptyLogStillReportsTheSessionsOwnFate(t *testing.T) {
	newTranscriptStubWithLog(t, nil, []map[string]any{
		{"at": "2026-09-13T12:00:00Z", "type": "runner_claimed"},
		{"at": "2026-09-13T12:00:04Z", "type": "runner_terminal", "details": map[string]any{
			"Phase": "Failed", "Reason": "MCPAllowlistDrift",
			"Message": "MCPServer/demo-connector: pinned tools not served: [get_thing]",
		}},
	})
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithObjects(failedTestSession("ws-demo123", "test-session-1")).Build()
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleReadTestLog, readTestLogArgs{Session: "test-session-1"})
	require.False(t, res.IsError, "an empty log is not an error — the session's own fate explains it")

	body := decodeResultBody(t, res)
	assert.Equal(t, "test-session-1", body["session"])
	assert.Empty(t, body["toolRecords"], "the session failed before calling anything")

	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, body["phase"])
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionMCPAllowlistDrift, body["failureReason"])
	assert.Contains(t, body["failureMessage"], "pinned tools not served",
		"the failure's own words reach the reader, not just its classification")
	assert.Contains(t, body["failureMessage"], "the server offers",
		"including what the connector does serve, which is the actionable half")

	entries, ok := body["auditEntries"].([]any)
	require.True(t, ok, "auditEntries must be a list")
	require.Len(t, entries, 2, "the session's own recorded transitions come through")
	assert.Equal(t, "runner_terminal", entries[1].(map[string]any)["type"])
}

// TestHandleReadTestLog_SessionStateUnreadable_SaysSoRatherThanOmitting: the
// records still come back, and the missing verdict is named rather than
// quietly absent — an absent field would read as "the session is fine". The
// field carries a fixed sentence, never the raw apiserver error: a builder
// session may relay this text to a person, and the apiserver's own wording
// (resource groups, API paths) must never reach them.
func TestHandleReadTestLog_SessionStateUnreadable_SaysSoRatherThanOmitting(t *testing.T) {
	newTranscriptStubWithLog(t, nil, nil)
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build() // no session object at all
	s := newTestRunServer("ws-demo123", c)

	res := callTool(t, s.handleReadTestLog, readTestLogArgs{Session: "gone-session"})
	require.False(t, res.IsError, "the log read succeeded; only the verdict is missing")

	body := decodeResultBody(t, res)
	assert.Equal(t, "the test's state could not be read right now", body["stateUnreadable"],
		"the missing verdict must be named, not silently omitted")
	assert.NotContains(t, body["stateUnreadable"], "agentsessions",
		"the raw apiserver error's infrastructure words must never reach the builder")
	assert.Empty(t, body["phase"], "no phase is claimed when none could be read")
}
