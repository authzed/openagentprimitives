package runner

// preferences_deps_test.go is the Task 12 TDD suite for preferencesSaver and
// preferencesReader: a fake approvalHost drives Save's publish/await
// round-trip without a real Loop/orchestrator/session, and a small
// httptest.Server drives Current's turn-index query-param wiring — the
// "simplest correct" seams named in the task brief.

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
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// fakeApprovalHost is the narrow approvalHost double: it records exactly what
// Save published and returns a scripted decision, with no Loop/orchestrator
// involved at all.
type fakeApprovalHost struct {
	gotAsk      pipeline.ApprovalAsk
	publishErr  error
	reqID       string
	publishCall int

	gotTimeout time.Duration
	awaitCall  int
	approved   bool
	by         string
	timedOut   bool
	awaitErr   error
}

func (f *fakeApprovalHost) PublishApproval(_ context.Context, ask pipeline.ApprovalAsk) (string, error) {
	f.publishCall++
	f.gotAsk = ask
	if f.publishErr != nil {
		return "", f.publishErr
	}
	if f.reqID == "" {
		return "req-1", nil
	}
	return f.reqID, nil
}

func (f *fakeApprovalHost) AwaitDecision(_ context.Context, _ string, timeout time.Duration) (bool, string, bool, error) {
	f.awaitCall++
	f.gotTimeout = timeout
	return f.approved, f.by, f.timedOut, f.awaitErr
}

// resolvingFakeApprovalHost additionally satisfies timeoutResolver, so tests
// can prove Save prefers the host's own resolved timeout over the flat
// package default when preferencesSaver.timeout is 0.
type resolvingFakeApprovalHost struct {
	fakeApprovalHost
	resolved time.Duration
}

func (r *resolvingFakeApprovalHost) ResolveTimeout() time.Duration { return r.resolved }

// TestPreferencesSaver_Save_PublishesExactWireContract is the brief's Step 1
// keystone: Save's published ask.Payload must marshal with EXACTLY the keys
// key/value/display — "value" entirely OMITTED (never JSON null) on a clear.
func TestPreferencesSaver_Save_PublishesExactWireContract(t *testing.T) {
	cases := []struct {
		name     string
		value    *apiextv1.JSON
		display  string
		wantJSON string
	}{
		{
			name:     "saving a value: key, value, and display all present",
			value:    &apiextv1.JSON{Raw: []byte(`"de"`)},
			display:  `Save "language: de" as your default for this agent?`,
			wantJSON: `{"key":"language","value":"de","display":"Save \"language: de\" as your default for this agent?"}`,
		},
		{
			name:     "clearing: value key omitted entirely, never sent as null",
			value:    nil,
			display:  "Clear your saved language preference?",
			wantJSON: `{"key":"language","display":"Clear your saved language preference?"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := &fakeApprovalHost{approved: true, by: "alice-canonical"}
			saver := NewPreferenceSaver(host, time.Second)

			_, err := saver.Save(context.Background(), "language", tc.value, tc.display)
			require.NoError(t, err)

			require.Equal(t, 1, host.publishCall, "Save must publish exactly one ask")
			assert.Equal(t, "preference_save", host.gotAsk.Kind)
			assert.Equal(t, tc.display, host.gotAsk.Summary)

			gotJSON, merr := json.Marshal(host.gotAsk.Payload)
			require.NoError(t, merr)
			assert.JSONEq(t, tc.wantJSON, string(gotJSON))

			if tc.value == nil {
				var m map[string]any
				require.NoError(t, json.Unmarshal(gotJSON, &m))
				_, hasValue := m["value"]
				assert.False(t, hasValue, `a clear must OMIT "value" entirely, not send it as null`)
			}
		})
	}
}

// TestPreferencesSaver_Save_OutcomeMapping is the brief's Step 1 table:
// approve/deny/timeout each map to the documented SaveOutcome shape.
func TestPreferencesSaver_Save_OutcomeMapping(t *testing.T) {
	cases := []struct {
		name     string
		approved bool
		by       string
		timedOut bool
		want     meta.SaveOutcome
	}{
		{
			name:     "approve: Approved with the decider's canonical id",
			approved: true, by: "YWxpY2U",
			want: meta.SaveOutcome{Approved: true, DecidedBy: "YWxpY2U"},
		},
		{
			name:     "deny: Denied with the decider's canonical id",
			approved: false, by: "Ym9i",
			want: meta.SaveOutcome{Denied: true, DecidedBy: "Ym9i"},
		},
		{
			name:     "timeout: TimedOut, nobody decided",
			timedOut: true,
			want:     meta.SaveOutcome{TimedOut: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := &fakeApprovalHost{approved: tc.approved, by: tc.by, timedOut: tc.timedOut}
			saver := NewPreferenceSaver(host, time.Second)

			got, err := saver.Save(context.Background(), "language", &apiextv1.JSON{Raw: []byte(`"de"`)}, "Save?")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestPreferencesSaver_Save_PublishErrorPropagates proves a publish failure
// (e.g. no addressable current-turn author) surfaces to the caller rather
// than being swallowed into a zero-value outcome.
func TestPreferencesSaver_Save_PublishErrorPropagates(t *testing.T) {
	host := &fakeApprovalHost{publishErr: errors.New("no addressable current-turn author")}
	saver := NewPreferenceSaver(host, time.Second)

	_, err := saver.Save(context.Background(), "language", nil, "Save?")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no addressable current-turn author")
	assert.Equal(t, 0, host.awaitCall, "AwaitDecision must never be called after a publish error")
}

// TestPreferencesSaver_Save_AwaitErrorPropagates proves a genuine
// transport/publish failure inside AwaitDecision surfaces to the caller.
func TestPreferencesSaver_Save_AwaitErrorPropagates(t *testing.T) {
	host := &fakeApprovalHost{awaitErr: errors.New("orchestrator wait failed")}
	saver := NewPreferenceSaver(host, time.Second)

	_, err := saver.Save(context.Background(), "language", nil, "Save?")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "orchestrator wait failed")
}

// TestPreferencesSaver_Save_NilHost_ErrorsRatherThanPanicking covers a
// preferencesSaver constructed with no host (should never happen in
// production wiring, but must fail loud rather than nil-panic).
func TestPreferencesSaver_Save_NilHost_ErrorsRatherThanPanicking(t *testing.T) {
	saver := NewPreferenceSaver(nil, time.Second)
	_, err := saver.Save(context.Background(), "language", nil, "Save?")
	require.Error(t, err)
}

// TestPreferencesSaver_Save_ZeroTimeout_PrefersHostResolvedTimeout proves the
// "one window, one source" intent: when preferencesSaver.timeout is 0 and the
// host can resolve the session's own tiered approval timeout
// (timeoutResolver), Save uses THAT rather than the flat package default.
func TestPreferencesSaver_Save_ZeroTimeout_PrefersHostResolvedTimeout(t *testing.T) {
	host := &resolvingFakeApprovalHost{
		fakeApprovalHost: fakeApprovalHost{approved: true},
		resolved:         42 * time.Second,
	}
	saver := NewPreferenceSaver(host, 0)

	_, err := saver.Save(context.Background(), "language", nil, "Save?")
	require.NoError(t, err)
	assert.Equal(t, 42*time.Second, host.gotTimeout)
}

// TestPreferencesSaver_Save_ZeroTimeout_FallsBackToDefault proves a host that
// does NOT implement timeoutResolver (a plain fake, or a lateBoundHost whose
// Loop never resolved) still gets a sane, non-zero wait rather than 0.
func TestPreferencesSaver_Save_ZeroTimeout_FallsBackToDefault(t *testing.T) {
	host := &fakeApprovalHost{approved: true}
	saver := NewPreferenceSaver(host, 0)

	_, err := saver.Save(context.Background(), "language", nil, "Save?")
	require.NoError(t, err)
	assert.Equal(t, defaultPreferenceSaveTimeout, host.gotTimeout)
}

// TestNewLateBoundPreferenceSaver_EndToEnd_OneHostSpansSave drives the REAL
// production adapter (NewLateBoundPreferenceSaver over a real *Loop +
// orchestrator) through a full Save round-trip, approve delivered via the
// published envelope's own RequestRef.
//
// This is the test the fake-host suite structurally could not fail: a
// single stateful fakeApprovalHost cannot exhibit per-call host
// re-resolution, but pendingApprovals lives ON the concrete *runnerHost
// (one-host-per-dispatch invariant, host_approval.go), so an adapter that
// resolves a FRESH host for AwaitDecision finds no pending entry and every
// production Save fails with "host: no pending approval for request ..."
// before the card is ever published. This test fails against exactly that
// bug.
func TestNewLateBoundPreferenceSaver_EndToEnd_OneHostSpansSave(t *testing.T) {
	orch := approval.New()
	author := emailCanonicalSubject(t, "alice@example.com")
	publishedRef := make(chan string, 1)
	l := &Loop{
		Status:            LocalStatusPatcher(),
		Approval:          orch,
		SessionKey:        memory.NamespacedName{Namespace: "ns", Name: "s"},
		lastInboundAuthor: author,
		InteractionRequestPublish: func(_ context.Context, _, _ string, env channelevents.Envelope) error {
			var pl channelevents.InteractionRequestPayload
			if err := json.Unmarshal(env.Payload, &pl); err != nil {
				return err
			}
			publishedRef <- pl.RequestRef
			return nil
		},
	}
	saver := NewLateBoundPreferenceSaver(func() *Loop { return l })

	// Approve as soon as the card is actually published. The orchestrator's
	// per-request channel is buffered, so delivering right after OnPublish
	// (which runs inside Await, after registration) cannot be lost. done
	// keeps the goroutine from leaking if publish never happens (the very
	// bug this test exists to catch fails Save before any publish).
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		select {
		case reqID := <-publishedRef:
			orch.DeliverDecision(reqID, approval.Decision{Approved: true, ApproverID: "alice-canonical"})
		case <-done:
		}
	}()

	out, err := saver.Save(context.Background(), "language", &apiextv1.JSON{Raw: []byte(`"de"`)},
		`Save "language: de" as your default for this agent?`)
	require.NoError(t, err,
		"ONE resolved host must span publish AND await — a fresh host per call has no pending entry and fails before the card is published")
	assert.Equal(t, meta.SaveOutcome{Approved: true, DecidedBy: "alice-canonical"}, out)
}

// TestNewLateBoundPreferenceSaver_LoopNotReady_FailsClosed proves the
// production adapter refuses loudly (never nil-panics) when Save somehow
// fires before the Loop is stored — the "session not ready" guard every
// other loopRef-late-bound closure in internal/cmd/runner/main.go carries.
func TestNewLateBoundPreferenceSaver_LoopNotReady_FailsClosed(t *testing.T) {
	saver := NewLateBoundPreferenceSaver(func() *Loop { return nil })
	_, err := saver.Save(context.Background(), "language", nil, "Save?")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not ready")
}

// TestPreferencesReader_Current_TurnIndexQueryParam proves preferencesReader
// forwards its captured turnIndex() into httpclient.GetPreferences's turn
// query-param contract: omitted for -1 (unknown), present when known.
func TestPreferencesReader_Current_TurnIndexQueryParam(t *testing.T) {
	cases := []struct {
		name      string
		turnIndex func() int
		wantQuery string
	}{
		{name: "unknown turn (-1): no turn query param", turnIndex: func() int { return -1 }, wantQuery: ""},
		{name: "known turn: turn query param carries it", turnIndex: func() int { return 3 }, wantQuery: "turn=3"},
		{name: "nil turnIndex func: treated defensively as unknown", turnIndex: nil, wantQuery: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotQuery = r.URL.RawQuery
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(preferences.SnapshotResponse{
					ClassNamespace: "ns", ClassName: "demo-class",
				})
			}))
			t.Cleanup(srv.Close)

			reader := NewPreferencesReader(httpclient.New(srv.URL, "tok"), "ns", "demo-session", tc.turnIndex)
			snap, err := reader.Current(context.Background())
			require.NoError(t, err)

			assert.Equal(t, "/memory/_preferences/ns/demo-session", gotPath)
			assert.Equal(t, tc.wantQuery, gotQuery)
			assert.Equal(t, "demo-class", snap.ClassName)
		})
	}
}

// TestPreferencesReader_ForRef_UserRefQueryParam proves preferencesReader's
// ForRef forwards ref as the ?user-ref= query param, escaped, and carries no
// ?turn param at all — the two are mutually exclusive server-side (see
// httpsrv's handlePreferencesGet).
func TestPreferencesReader_ForRef_UserRefQueryParam(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(preferences.SnapshotResponse{
			ClassNamespace: "ns", ClassName: "demo-class", Subject: "alice",
		})
	}))
	t.Cleanup(srv.Close)

	reader := NewPreferencesReader(httpclient.New(srv.URL, "tok"), "ns", "demo-session", func() int { return 3 })
	snap, err := reader.ForRef(context.Background(), "trigger-author")
	require.NoError(t, err)

	assert.Equal(t, "/memory/_preferences/ns/demo-session", gotPath)
	assert.Equal(t, "user-ref=trigger-author", gotQuery)
	assert.Equal(t, "alice", snap.Subject)
}
