package main

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all" // register turn/lifecycle/tool_session/... Kinds
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolsession"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/tools/toolkitstream"
)

// newMemForTest stands up the v2 memory server (operator-hosted Local over
// HTTP) and returns a turn.Appender wired exactly as internal/cmd/runner does — an
// httpclient.Client scoped to one session. The server is registered with
// t.Cleanup.
func newMemForTest(t *testing.T) (*turn.Appender, memory.Scope) {
	t.Helper()
	const ns, name = "default", "sess1"
	k := memory.NamespacedName{Namespace: ns, Name: name}
	reg := tokens.NewRegistry()
	reg.Set(k, "tok", "")
	srv := httptest.NewServer(httpsrv.NewHandler(memory.NewLocal(inmem.NewBackend()), reg))
	t.Cleanup(srv.Close)

	memHTTP := httpclient.New(srv.URL, "tok")
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
	return turn.NewAppender(memHTTP, scope), scope
}

func TestAppendSystemNote_AssignsUniqueIndices(t *testing.T) {
	app, _ := newMemForTest(t)
	apd := runner.AppendSystemNoteFunc(app)
	ctx := context.Background()

	// Three consecutive appends must all succeed (no index conflict).
	for i, payload := range []map[string]any{
		{"delivered": []string{"tu_a"}},
		{"delivered": []string{"tu_b"}},
		{"kind": "plans", "v": 1, "data": map[string]any{"op": "upsert"}},
	} {
		require.NoError(t, apd(ctx, payload), "append %d", i)
	}

	turns, err := app.ReadAll(ctx)
	require.NoError(t, err)
	require.Len(t, turns, 3)
	seen := map[int]bool{}
	for _, tt := range turns {
		assert.Equal(t, "system_note", tt.Role, "turn %d role", tt.Index)
		assert.False(t, seen[tt.Index], "duplicate index %d", tt.Index)
		seen[tt.Index] = true
	}
}

func TestAppendSystemNote_RecoversFromConflict(t *testing.T) {
	app, _ := newMemForTest(t)
	apd := runner.AppendSystemNoteFunc(app)
	ctx := context.Background()

	// First append primes the closure's index to 1.
	require.NoError(t, apd(ctx, map[string]any{"first": true}), "first append")

	// Simulate a writer claiming index 1 behind the closure's back: append
	// a system_note at index 1 with a payload the closure would never
	// produce. The closure still thinks next is 1, so its next write
	// collides — turn.Appender surfaces that as memory.ErrIndexConflict.
	require.NoError(t, app.Append(ctx, memory.Turn{
		Index:     1,
		Role:      "system_note",
		Content:   []memory.ContentBlock{{Type: "text", Text: `{"injected":true}`}},
		CreatedAt: time.Now().UTC(),
	}), "inject conflict")

	// Closure should detect the conflict, refresh, and retry at the next
	// free index.
	require.NoError(t, apd(ctx, map[string]any{"after_conflict": true}), "recovery append")

	turns, err := app.ReadAll(ctx)
	require.NoError(t, err)
	// Should have 3 system_notes total: first(0), injected(1), recovery(2).
	assert.Len(t, turns, 3, "want 3 turns, got %+v", turns)
}

// TestIdleTTLFlag verifies the IDLE_TTL env var is routed through the typed
// --idle-ttl flag via clikit.EnvOverridePreRunE: a valid duration is parsed,
// an unset var keeps the 5m default, and a MALFORMED value is now a startup
// error (fail-closed) rather than a silent revert to the default — which was
// the bug this conversion fixes.
func TestIdleTTLFlag(t *testing.T) {
	cases := []struct {
		name      string
		env       string
		want      time.Duration
		wantErr   bool
		errSubstr string
	}{
		// 15m since the ui_presence heartbeat landed: a WATCHED agent-UI now
		// holds its runner open on its own, so this default no longer has to
		// cover a viewer who is reading rather than typing — it covers the
		// gap where exiting is correct but unhelpful, because the next thing
		// that happens is a cold start.
		{name: "unset env: 15 minute default", env: "", want: 15 * time.Minute},
		{name: "valid duration: parsed as-is", env: "30s", want: 30 * time.Second},
		{name: "0s: zero duration honored", env: "0s", want: 0},
		{name: "garbage: startup error naming IDLE_TTL", env: "garbage", wantErr: true, errSubstr: "IDLE_TTL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("IDLE_TTL", tc.env)
			cmd := newCommand()
			require.NoError(t, cmd.ParseFlags(nil), "ParseFlags")
			err := cmd.PreRunE(cmd, nil)
			if tc.wantErr {
				require.Error(t, err, "malformed IDLE_TTL must fail closed")
				assert.Contains(t, err.Error(), tc.errSubstr)
				return
			}
			require.NoError(t, err, "PreRunE")
			got, gerr := cmd.Flags().GetDuration("idle-ttl")
			require.NoError(t, gerr)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestPersistToolSessionEvent(t *testing.T) {
	cases := []struct {
		mode   string
		evType toolkitstream.EventType
		want   bool
	}{
		{"off", toolkitstream.EventResult, false},
		{"off", toolkitstream.EventToolUseStart, false},
		{"full", toolkitstream.EventTextDelta, true},
		{"full", toolkitstream.EventResult, true},
		{"highSignal", toolkitstream.EventTextDelta, false},
		{"highSignal", toolkitstream.EventToolUseStart, true},
		{"highSignal", toolkitstream.EventToolUseStop, true},
		{"highSignal", toolkitstream.EventResult, true},
		{"", toolkitstream.EventTextDelta, false},
		{"", toolkitstream.EventResult, true},
		{"bogus", toolkitstream.EventResult, false},
	}
	for _, tc := range cases {
		t.Run(tc.mode+"/"+string(tc.evType), func(t *testing.T) {
			assert.Equal(t, tc.want, persistToolSessionEvent(tc.mode, tc.evType))
		})
	}
}

// TestBuildToolSessionEventPublisher_Persist fires a tool_use_start and a
// text_delta event through the publisher closure and asserts which land in
// the tool_session Kind per log mode. The NATS pub is a no-op — only the
// persist side is under test here.
func TestBuildToolSessionEventPublisher_Persist(t *testing.T) {
	cases := []struct {
		mode      string
		wantCount int
	}{
		{"highSignal", 1}, // tool_use_start kept, text_delta dropped
		{"full", 2},       // both kept
		{"off", 0},        // none kept
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			ctx := memory.WithSystemApproval(context.Background(), "test")
			mem := memory.NewLocal(inmem.NewBackend())
			scope := memory.Scope{Kind: "session", ID: "default/sess1"}
			noopPub := func(context.Context, string, []byte) error { return nil }

			onEvent := buildToolSessionEventPublisher(
				ctx, noopPub, mem, scope, tc.mode, "default", "sess1", nil)
			onEvent("ref-1", "", "", toolkitstream.Event{
				Type: toolkitstream.EventToolUseStart, ToolName: "Edit", ToolID: "t1",
			})
			onEvent("ref-1", "", "", toolkitstream.Event{
				Type: toolkitstream.EventTextDelta, Text: "hello",
			})

			got, err := toolsession.ReadAll(ctx, mem, scope)
			require.NoError(t, err, "ReadAll after publishing")
			assert.Len(t, got, tc.wantCount)
		})
	}
}

// The BundledOnly-always-available / Standalone-cap-gated semantics of the
// former availableRendererKinds now live in
// pkg/agent/tool/meta/capability.AvailableAssetKinds and are covered by
// capability.TestAvailableAssetKinds. internal/cmd/runner maps those kind names to
// []runner.AssetKind inline (Name + kind-owned Instructions) gated on the
// artifacts capability having injected artifact_prepare.

// runServeOnly parks a pod that answers a dashboard's data bindings without
// running the agent. The reason it exists is a correctness one, not a cost
// one: the ordinary wake resumes the CONVERSATION, so opening a page put
// content in the agent's context that no human asked for — observed live, it
// left a session Failed.
func TestRunServeOnly(t *testing.T) {
	t.Run("exits once no viewer has been seen for the idle window", func(t *testing.T) {
		start := time.Now()
		err := runServeOnly(context.Background(), make(chan struct{}), 40*time.Millisecond, "demo-ns", "sess")
		require.NoError(t, err)
		assert.GreaterOrEqual(t, time.Since(start), 40*time.Millisecond,
			"it must actually wait out the window rather than returning immediately")
	})

	t.Run("a heartbeat extends the window, repeatedly", func(t *testing.T) {
		// The pod must outlive its own idle window for as long as someone is
		// watching. A single-shot extension would look correct in a short test
		// and drop a viewer who keeps a dashboard open.
		presence := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- runServeOnly(context.Background(), presence, 60*time.Millisecond, "demo-ns", "sess")
		}()
		for i := 0; i < 4; i++ {
			time.Sleep(30 * time.Millisecond)
			select {
			case presence <- struct{}{}:
			case err := <-done:
				t.Fatalf("exited early after %d heartbeats: %v", i, err)
			}
		}
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Fatal("must still exit once the heartbeats stop")
		}
	})

	t.Run("a canceled context exits cleanly, not as an error", func(t *testing.T) {
		// SIGTERM during a serve window is an ordinary shutdown. Returning an
		// error would surface a normal drain as a runner failure.
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- runServeOnly(ctx, make(chan struct{}), time.Hour, "demo-ns", "sess") }()
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Fatal("cancel must return promptly")
		}
	})

	t.Run("a disabled idle ttl exits immediately rather than parking forever", func(t *testing.T) {
		// idleTTL<=0 means "idle is disabled", which for the agent loop means
		// exit at once. A serve-only pod must read it the same way: with no
		// window there is nothing to serve, and the alternative — treating
		// zero as infinite — is a pod that never goes away.
		done := make(chan error, 1)
		go func() { done <- runServeOnly(context.Background(), make(chan struct{}), 0, "demo-ns", "sess") }()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Fatal("a zero idle ttl must not park")
		}
	})
}
