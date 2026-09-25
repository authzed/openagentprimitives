package runner_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	return s
}

func newFakeClientWithSession(t *testing.T) (client.Client, *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", Generation: 1},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac"},
	}
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	return c, sess
}

func TestPatchProgress(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	sp := runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess))
	ctx := context.Background()
	require.NoError(t, sp.PatchProgress(ctx, runner.Progress{
		TurnCount: 3, InputTokens: 100, OutputTokens: 50, ToolCallCount: 4,
	}), "PatchProgress")

	var out spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out), "Get after PatchProgress")
	require.NotNil(t, out.Status.Progress, "Progress must be set")
	assert.Equal(t, int32(3), out.Status.Progress.TurnCount)
	assert.Equal(t, int64(100), out.Status.Progress.InputTokens)
}

func TestPatchRunDuration(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	sp := runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess))
	ctx := context.Background()
	require.NoError(t, sp.PatchRunDuration(ctx, 90*time.Second), "PatchRunDuration")

	var out spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out), "Get after PatchRunDuration")
	require.NotNil(t, out.Status.RunDuration, "RunDuration must be set")
	assert.Equal(t, 90*time.Second, out.Status.RunDuration.Duration)
}

func TestWriteSucceeded(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	sp := runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess))
	ctx := context.Background()
	require.NoError(t, sp.WriteSucceeded(ctx, spiceboxv1alpha1.AgentResult{Summary: "done"}),
		"WriteSucceeded")

	var out spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out), "Get after WriteSucceeded")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, out.Status.Phase)
	require.NotNil(t, out.Status.Result, "Result must be set on success")
	assert.Equal(t, "done", out.Status.Result.Summary)
}

func TestWriteFailed(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	sp := runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess))
	ctx := context.Background()
	require.NoError(t, sp.WriteFailed(ctx, spiceboxv1alpha1.ReasonAgentSessionStalled, "model gave up"),
		"WriteFailed")

	var out spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out), "Get after WriteFailed")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, out.Status.Phase)
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionStalled, out.Status.FailureReason)
}

func TestAppendRunnerNote(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	sp := runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess))
	ctx := context.Background()
	require.NoError(t, sp.AppendRunnerNote(ctx, "stale runner exiting"), "AppendRunnerNote")

	var out spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out), "Get after AppendRunnerNote")
	require.Len(t, out.Status.RunnerNotes, 1, "expected one runner note")
	assert.Equal(t, "stale runner exiting", out.Status.RunnerNotes[0].Message)
}

func TestPatchProgressTransitionsPhaseToRunning(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	sp := runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess))
	ctx := context.Background()
	require.NoError(t, sp.PatchProgress(ctx, runner.Progress{TurnCount: 1}), "first PatchProgress")

	var out spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out), "Get after first PatchProgress")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, out.Status.Phase)
	require.NotNil(t, out.Status.StartedAt, "StartedAt must be set on first progress write")
	first := out.Status.StartedAt.Time

	// Second call should preserve the original StartedAt and phase.
	require.NoError(t, sp.PatchProgress(ctx, runner.Progress{TurnCount: 2}), "second PatchProgress")
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out), "Get after second PatchProgress")
	require.NotNil(t, out.Status.StartedAt)
	assert.True(t, out.Status.StartedAt.Time.Equal(first),
		"StartedAt was overwritten: was %v, now %v", first, out.Status.StartedAt.Time)
}

func TestWriteSatisfiedSecretOutput(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	sp := runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess))
	ctx := context.Background()

	// First write: name "kubeconfig", handle "my-token" → secret key "session-s1-secrets".
	require.NoError(t, sp.WriteSatisfiedSecretOutput(ctx, "kubeconfig", "my-token", "session-s1-secrets"),
		"WriteSatisfiedSecretOutput (first)")

	var out spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out), "Get after first write")
	require.Len(t, out.Status.SatisfiedSecretOutputs, 1, "expected one entry after first write")
	assert.Equal(t, "kubeconfig", out.Status.SatisfiedSecretOutputs[0].Name)
	assert.Equal(t, "my-token", out.Status.SatisfiedSecretOutputs[0].Handle)
	assert.Equal(t, "session-s1-secrets", out.Status.SatisfiedSecretOutputs[0].SecretName)
	assert.NotNil(t, out.Status.SatisfiedSecretOutputs[0].WrittenAt, "WrittenAt must be set")

	// Second write with the same handle: must be idempotent — no duplicate appended.
	require.NoError(t, sp.WriteSatisfiedSecretOutput(ctx, "kubeconfig", "my-token", "session-s1-secrets"),
		"WriteSatisfiedSecretOutput (idempotent repeat)")

	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out), "Get after idempotent write")
	assert.Len(t, out.Status.SatisfiedSecretOutputs, 1,
		"duplicate write must not append a second entry")

	// Third write with a different handle: must append independently.
	require.NoError(t, sp.WriteSatisfiedSecretOutput(ctx, "token", "other-token", "session-s1-secrets"),
		"WriteSatisfiedSecretOutput (second distinct handle)")

	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out), "Get after second distinct write")
	assert.Len(t, out.Status.SatisfiedSecretOutputs, 2,
		"second distinct handle must produce a second entry")
	assert.Equal(t, "token", out.Status.SatisfiedSecretOutputs[1].Name)
}

func TestRecordObservedPin(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	sp := runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess))
	ctx := context.Background()

	pin1 := spiceboxv1alpha1.PinRecord{Kind: "mcp", Strength: "frozen", Digest: "sha256:aaaa"}
	require.NoError(t, sp.RecordObservedPin(ctx, "gh-tools", pin1), "RecordObservedPin (first)")

	var out spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out), "Get after first RecordObservedPin")
	require.Len(t, out.Status.ObservedPins, 1, "expected one entry after first write")
	assert.Equal(t, "gh-tools", out.Status.ObservedPins[0].Name)
	assert.Equal(t, "sha256:aaaa", out.Status.ObservedPins[0].Pin.Digest)

	// Upsert: same name updates the existing entry rather than appending.
	pin1updated := spiceboxv1alpha1.PinRecord{Kind: "mcp", Strength: "frozen", Digest: "sha256:bbbb"}
	require.NoError(t, sp.RecordObservedPin(ctx, "gh-tools", pin1updated), "RecordObservedPin (upsert)")

	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out), "Get after upsert")
	require.Len(t, out.Status.ObservedPins, 1, "upsert must not append a second entry")
	assert.Equal(t, "sha256:bbbb", out.Status.ObservedPins[0].Pin.Digest, "upsert must update the existing digest")

	// Different name appends independently.
	pin2 := spiceboxv1alpha1.PinRecord{Kind: "mcp", Strength: "unpinned", Digest: "sha256:cccc"}
	require.NoError(t, sp.RecordObservedPin(ctx, "linear-tools", pin2), "RecordObservedPin (second name)")

	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out), "Get after second name write")
	assert.Len(t, out.Status.ObservedPins, 2, "second distinct name must produce a second entry")
}

func TestSetAndClearAwaitingUserInput(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	p := runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess))
	ctx := context.Background()

	require.NoError(t, p.SetAwaitingUserInput(ctx), "SetAwaitingUserInput")
	var s spiceboxv1alpha1.AgentSession
	require.NoError(t, p.Get(ctx, &s), "Get after SetAwaitingUserInput")
	assert.NotNil(t, s.Status.AwaitingUserInputSince, "set records a timestamp")

	require.NoError(t, p.ClearAwaitingUserInput(ctx), "ClearAwaitingUserInput")
	require.NoError(t, p.Get(ctx, &s), "Get after ClearAwaitingUserInput")
	assert.Nil(t, s.Status.AwaitingUserInputSince, "clear removes it")
}

func TestAppendActiveWidget_Appends(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	sp := runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess))
	ctx := context.Background()

	require.NoError(t, sp.AppendActiveWidget(ctx, spiceboxv1alpha1.WidgetRef{
		ArtifactID: "artifact-1", Tool: "mcpserver/widgets.render_form", RendererKind: "mcpui",
	}))

	var out spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out))
	require.Len(t, out.Status.ActiveWidgets, 1)
	assert.Equal(t, "artifact-1", out.Status.ActiveWidgets[0].ArtifactID)
	assert.Equal(t, "mcpserver/widgets.render_form", out.Status.ActiveWidgets[0].Tool)
	assert.Equal(t, "mcpui", out.Status.ActiveWidgets[0].RendererKind)

	require.NoError(t, sp.AppendActiveWidget(ctx, spiceboxv1alpha1.WidgetRef{ArtifactID: "artifact-2"}))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out))
	require.Len(t, out.Status.ActiveWidgets, 2, "a second append must add, not replace")
	assert.Equal(t, "artifact-1", out.Status.ActiveWidgets[0].ArtifactID)
	assert.Equal(t, "artifact-2", out.Status.ActiveWidgets[1].ArtifactID)
}

func TestAppendActiveWidget_CapsAtMostRecentTen(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	sp := runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess))
	ctx := context.Background()

	for i := 0; i < 12; i++ {
		require.NoError(t, sp.AppendActiveWidget(ctx, spiceboxv1alpha1.WidgetRef{
			ArtifactID: fmt.Sprintf("artifact-%d", i),
		}))
	}

	var out spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out))
	require.Len(t, out.Status.ActiveWidgets, 10, "must cap at the most recent 10 entries")
	assert.Equal(t, "artifact-2", out.Status.ActiveWidgets[0].ArtifactID, "oldest entries must be dropped first")
	assert.Equal(t, "artifact-11", out.Status.ActiveWidgets[9].ArtifactID, "newest entry must be kept")
}

func TestAppendActiveWidget_UnaffectedByConcurrentFieldWrite(t *testing.T) {
	// A merge patch scoped to status.activeWidgets must not disturb a
	// different status field (e.g. runnerNotes) another writer set — mirrors
	// the "unincluded field is never re-sent" invariant setAwaitingUserInput
	// documents.
	c, sess := newFakeClientWithSession(t)
	sp := runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess))
	ctx := context.Background()

	require.NoError(t, sp.AppendRunnerNote(ctx, "unrelated note"))
	require.NoError(t, sp.AppendActiveWidget(ctx, spiceboxv1alpha1.WidgetRef{ArtifactID: "artifact-1"}))

	var out spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out))
	require.Len(t, out.Status.RunnerNotes, 1, "activeWidgets write must not clobber runnerNotes")
	require.Len(t, out.Status.ActiveWidgets, 1)
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// TestWriteIdle covers the two reasons WriteIdle is called: a wait for a
// user follow-up message (AwaitingUserMsg) and a clean agent_work_complete.
// Both must land in Phase=Idle with the matching condition reason; the
// agent_work_complete case additionally carries a hard-coded user-facing
// message.
func TestWriteIdle(t *testing.T) {
	cases := []struct {
		name        string
		reason      string
		wantMessage string // "" → don't assert
	}{
		{
			name:   "AwaitingUserMsg reason: phase Idle + matching condition reason",
			reason: spiceboxv1alpha1.ReasonAgentSessionAwaitingUserMsg,
		},
		{
			name:        "AgentWorkComplete reason: phase Idle + reason + canonical message",
			reason:      spiceboxv1alpha1.ReasonAgentSessionAgentWorkComplete,
			wantMessage: "agent_work_complete called; session idle",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newFakeClientWithSession(t)
			key := client.ObjectKey{Namespace: "default", Name: "s1"}
			patcher := runner.NewStatusPatcher(c, key)
			ctx := context.Background()
			require.NoError(t, patcher.WriteIdle(ctx, tc.reason), "WriteIdle")

			got := &spiceboxv1alpha1.AgentSession{}
			require.NoError(t, c.Get(ctx, key, got), "Get after WriteIdle")
			assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got.Status.Phase)
			idleCond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionIdle)
			require.NotNil(t, idleCond, "Idle condition must be set")
			assert.Equal(t, metav1.ConditionTrue, idleCond.Status)
			assert.Equal(t, tc.reason, idleCond.Reason)
			if tc.wantMessage != "" {
				assert.Equal(t, tc.wantMessage, idleCond.Message)
			}
		})
	}
}

func TestTerminalWriteIsIdempotent(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	sp := runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess))
	ctx := context.Background()
	require.NoError(t, sp.WriteSucceeded(ctx, spiceboxv1alpha1.AgentResult{Summary: "first"}),
		"WriteSucceeded (first)")

	var out spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out), "Get after WriteSucceeded")
	firstFinished := out.Status.FinishedAt
	require.NotNil(t, firstFinished, "FinishedAt must be set after WriteSucceeded")

	// Stale write attempt — should be ignored.
	require.NoError(t, sp.WriteFailed(ctx, "Stalled", "stale runner"), "stale WriteFailed")
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out), "Get after stale WriteFailed")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, out.Status.Phase,
		"phase must be preserved against stale WriteFailed")
	require.NotNil(t, out.Status.Result, "Result must be preserved")
	assert.Equal(t, "first", out.Status.Result.Summary, "Result must not be overwritten")
	require.NotNil(t, out.Status.FinishedAt)
	assert.True(t, firstFinished.Time.Equal(out.Status.FinishedAt.Time),
		"FinishedAt was overwritten: was %v, now %v", firstFinished, out.Status.FinishedAt)
}

// TestPatchEstimatedCost_NeverRegresses asserts the session cost estimate is
// monotonic. The SessionEnd cost hook stamps on EVERY terminal path — including
// idle — from per-pod token counters that reset on every wake, so a woken pod
// that reaches a terminal path having made no LLM calls used to overwrite a
// real number with zero.
func TestPatchEstimatedCost_NeverRegresses(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	sp := runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess))
	ctx := context.Background()

	require.NoError(t, sp.PatchEstimatedCost(ctx, spiceboxv1alpha1.EstimatedSessionCost{
		AmountMicroUSD: 4_200_000, Currency: "USD", Model: "m1", PricingKnown: true,
		ByModel: []spiceboxv1alpha1.ModelCostBucket{{Model: "m1", AmountMicroUSD: 4_200_000}},
	}), "first stamp")

	// A woken pod with no LLM calls of its own.
	require.NoError(t, sp.PatchEstimatedCost(ctx, spiceboxv1alpha1.EstimatedSessionCost{
		Currency: "USD", PricingKnown: false,
	}), "second stamp from a woken pod")

	var out spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out), "Get after stamps")
	require.NotNil(t, out.Status.EstimatedCost, "EstimatedCost must survive the second stamp")
	assert.Equal(t, int64(4_200_000), out.Status.EstimatedCost.AmountMicroUSD,
		"a pod that spent nothing must not wipe the session's recorded spend")
	assert.Len(t, out.Status.EstimatedCost.ByModel, 1,
		"the per-model breakdown must stay consistent with the amount it explains")
}

// TestAppendRunnerNote_CapsOldestFirst asserts runnerNotes is bounded. Nothing
// prunes it, the CRD declares no maxItems, and every append re-sends the whole
// list in a merge patch.
func TestAppendRunnerNote_CapsOldestFirst(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	sp := runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess))
	ctx := context.Background()

	for i := 0; i < 40; i++ {
		require.NoError(t, sp.AppendRunnerNote(ctx, fmt.Sprintf("note-%d", i)), "AppendRunnerNote")
	}

	var out spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &out), "Get after appends")
	assert.LessOrEqual(t, len(out.Status.RunnerNotes), 20,
		"runnerNotes must be bounded; every append re-sends the whole list")
	require.NotEmpty(t, out.Status.RunnerNotes)
	assert.Equal(t, "note-39", out.Status.RunnerNotes[len(out.Status.RunnerNotes)-1].Message,
		"the newest note must survive; the oldest are the ones dropped")
}
