package toolcall

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/gateway"
)

// TestStreamingStdin asserts that a streaming ToolCall's exec stdin is wired
// per mode: a stream-mode call (no interactive bridge, nothing ever writes
// to stdin) gets an empty/EOF reader so a one-shot tool like `claude --print`
// does not stall waiting for piped input; an interactive-mode call gets a nil
// reader so the exec layer opens a live pipe the channel→bridge path feeds.
func TestStreamingStdin(t *testing.T) {
	tc := func(mode spiceboxv1alpha1.ToolCallMode, stdin string) *spiceboxv1alpha1.ToolCall {
		return &spiceboxv1alpha1.ToolCall{
			ObjectMeta: metav1.ObjectMeta{Name: "tc", Namespace: "default"},
			Spec:       spiceboxv1alpha1.ToolCallSpec{Mode: mode, Stdin: stdin},
		}
	}

	t.Run("stream mode without spec.stdin: empty EOF reader, no open pipe", func(t *testing.T) {
		r := streamingStdin(tc(spiceboxv1alpha1.ToolCallModeStream, ""))
		require.NotNil(t, r, "stream mode must pass a non-nil reader so the exec layer EOFs stdin instead of opening a never-written pipe")
		b, err := io.ReadAll(r)
		require.NoError(t, err, "read streaming stdin")
		assert.Empty(t, b, "stream mode without spec.stdin is immediately at EOF")
	})

	t.Run("stream mode with spec.stdin: finite reader of those bytes", func(t *testing.T) {
		r := streamingStdin(tc(spiceboxv1alpha1.ToolCallModeStream, "payload"))
		require.NotNil(t, r, "stream mode with spec.stdin must pass that reader")
		b, err := io.ReadAll(r)
		require.NoError(t, err, "read streaming stdin")
		assert.Equal(t, "payload", string(b), "stream mode passes spec.stdin through, then EOF")
	})

	t.Run("interactive mode: nil reader so the exec opens a live pipe", func(t *testing.T) {
		r := streamingStdin(tc(spiceboxv1alpha1.ToolCallModeInteractive, ""))
		assert.Nil(t, r, "interactive mode keeps stdin nil so StreamExec opens a live pipe the bridge feeds")
	})
}

// runningStreamingToolCall builds a streaming ToolCall already carrying
// Running=True and no terminal condition — the exact state an operator restart
// leaves behind, and also the state a duplicate reconcile of a healthy call
// observes.
func runningStreamingToolCall(t *testing.T, name string) *spiceboxv1alpha1.ToolCall {
	t.Helper()
	return &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session:         "sess-x",
			Tool:            "shell",
			Mode:            spiceboxv1alpha1.ToolCallModeInteractive,
			StreamTokenHash: "deadbeef",
		},
		Status: spiceboxv1alpha1.ToolCallStatus{
			Conditions: []metav1.Condition{{
				Type:               spiceboxv1alpha1.ToolCallConditionRunning,
				Status:             metav1.ConditionTrue,
				Reason:             "Started",
				LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute)),
			}},
			Streaming: &spiceboxv1alpha1.StreamingEndpoint{Available: true, GatewayEndpoint: "gw:8443"},
		},
	}
}

// reconcileRunningStream builds a Reconciler over a fake client holding tc,
// runs reconcileStreaming against the live copy, and returns it.
func reconcileRunningStream(t *testing.T, tc *spiceboxv1alpha1.ToolCall, reg *gateway.Registry) *spiceboxv1alpha1.ToolCall {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch), "build scheme")
	cl := fake.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(&spiceboxv1alpha1.ToolCall{}).
		WithObjects(tc).
		Build()

	ctx := context.Background()
	var live spiceboxv1alpha1.ToolCall
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(tc), &live), "load the ToolCall the reconciler will see")

	r := &Reconciler{Client: cl, Registry: reg}
	resolved := &resolvedCall{
		session: &spiceboxv1alpha1.SpiceboxSession{ObjectMeta: metav1.ObjectMeta{Name: "sess-x", Namespace: "default"}},
		tool:    spiceboxv1alpha1.SpiceboxTool{Name: "shell", Command: []string{"sh"}},
	}
	res, err := r.reconcileStreaming(ctx, &live, resolved, nil)
	require.NoError(t, err, "reconcileStreaming must not error on an already-Running call")
	assert.Zero(t, res.RequeueAfter, "the Running branch never schedules a retry")

	var got spiceboxv1alpha1.ToolCall
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(tc), &got), "re-read after reconcile")
	return &got
}

// TestReconcileStreaming_RunningWithoutARegisteredStream asserts the
// Running=True short-circuit consults the gateway registry — the thing its own
// comment claims it checks — rather than only the CR condition.
//
// An operator restart drops the whole in-process registry while the CR still
// says Running=True with no terminal condition. Reconcile's terminal
// short-circuit does not fire, so the call lands here; returning a bare no-op
// leaves the ToolCall Running forever with no goroutine left to finalize it.
// That is not merely a stale CR: channelsd's liveInteractiveToolCall selects
// any interactive ToolCall that is Running-and-not-terminal, so every
// subsequent user message is routed to the dead call and dropped by the runner
// — the session goes permanently unresponsive. The sync path handles exactly
// this case with ReasonOperatorRestart; the streaming path must match it.
func TestReconcileStreaming_RunningWithoutARegisteredStream(t *testing.T) {
	t.Run("registry has no stream (operator restart): Failed=True/OperatorRestart, FinishedAt set", func(t *testing.T) {
		got := reconcileRunningStream(t, runningStreamingToolCall(t, "tc-orphan"), gateway.NewRegistry())

		require.Equal(t, spiceboxv1alpha1.ToolCallConditionFailed, terminalCondition(got),
			"an orphaned streaming ToolCall must finalize, not stay Running forever")
		var failed metav1.Condition
		for _, c := range got.Status.Conditions {
			if c.Type == spiceboxv1alpha1.ToolCallConditionFailed {
				failed = c
			}
		}
		assert.Equal(t, spiceboxv1alpha1.ReasonOperatorRestart, failed.Reason,
			"same reason the sync path stamps for the identical situation")
		assert.NotNil(t, got.Status.FinishedAt, "a finalized ToolCall records when it finished")
		assert.False(t, hasTrueCondition(got, spiceboxv1alpha1.ToolCallConditionRunning),
			"Running must be cleared so liveInteractiveToolCall stops routing to it")
	})

	t.Run("registry holds the stream (duplicate reconcile): no-op, still Running", func(t *testing.T) {
		reg := gateway.NewRegistry()
		require.NoError(t, reg.Register(&gateway.ActiveStream{
			Namespace: "default", ToolCallName: "tc-live", TokenHash: "deadbeef",
		}), "the exec goroutine of this process still owns the stream")

		got := reconcileRunningStream(t, runningStreamingToolCall(t, "tc-live"), reg)

		assert.Empty(t, terminalCondition(got),
			"a live stream must never be failed out from under its own watcher goroutine")
		assert.True(t, hasTrueCondition(got, spiceboxv1alpha1.ToolCallConditionRunning),
			"the watcher goroutine still owns finalization")
	})
}
