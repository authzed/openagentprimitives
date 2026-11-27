// pkg/controllers/agentsession/failedtrigger_test.go
//
// The operator's backstop for a Failed session whose trigger nobody answered:
// the runner's own failures (a budget cap, a halt), and the retry TTL / retry
// cap after a provider error, all reach Failed without anything telling the
// pull request. Reuses the fixture reporter kind from triggerstatus_test.go.
package agentsession_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/channels/triggeranswer"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// failedSession is a triggered session in Failed with reason, finished
// finishedAgo ago, whose runner had come up.
func failedSession(kind, reason string, finishedAgo time.Duration) *spiceboxv1alpha1.AgentSession {
	sess := bootFailedSession(kind)
	sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseFailed
	sess.Status.FailureReason = reason
	finished := metav1.NewTime(time.Now().Add(-finishedAgo))
	sess.Status.FinishedAt = &finished
	sess.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.AgentSessionConditionRunnerReady, Status: metav1.ConditionTrue,
		Reason: "Ready", LastTransitionTime: metav1.Now(),
	}}
	return sess
}

// settledReason reads the TriggerAnswered marker back from the API server, so
// the assertion is on what persisted, not on the in-memory object.
func settledReason(t *testing.T, r *agentsession.Reconciler, sess *spiceboxv1alpha1.AgentSession) string {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, r.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), &got))
	c := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionTriggerAnswered)
	if c == nil {
		return ""
	}
	return c.Reason
}

func TestFailedTriggerBackstop(t *testing.T) {
	cases := []struct {
		name    string
		sess    func() *spiceboxv1alpha1.AgentSession
		arrange func(k *operatorTriggerKind)
		// withoutChannel deletes the session's input Channel before the pass.
		withoutChannel bool
		// wantClaims: how many times the provider was read.
		wantClaims int
		// wantSummary: non-empty means exactly one could_not_finish was
		// published, carrying this text.
		wantSummary string
		wantMarker  string
		// wantMessage: a substring the marker's message must carry.
		wantMessage string
		wantRetry   bool
	}{
		{
			name: "runner failed it on its budget: answered 'stopped', marked Answered",
			sess: func() *spiceboxv1alpha1.AgentSession {
				return failedSession(tsKindName, spiceboxv1alpha1.ReasonAgentSessionBudget, time.Minute)
			},
			wantClaims:  1,
			wantSummary: triggeranswer.StoppedSummary(spiceboxv1alpha1.ReasonAgentSessionBudget),
			wantMarker:  spiceboxv1alpha1.ReasonTriggerAnswered,
		},
		{
			name: "retry TTL elapsed after a provider error: answered, marked Answered",
			sess: func() *spiceboxv1alpha1.AgentSession {
				return failedSession(tsKindName, spiceboxv1alpha1.ReasonAgentSessionRetryTimeout, time.Minute)
			},
			wantClaims:  1,
			wantSummary: triggeranswer.StoppedSummary(spiceboxv1alpha1.ReasonAgentSessionRetryTimeout),
			wantMarker:  spiceboxv1alpha1.ReasonTriggerAnswered,
		},
		{
			name: "retry cap exhausted (fold projected ProviderError): answered, marked Answered",
			sess: func() *spiceboxv1alpha1.AgentSession {
				return failedSession(tsKindName, spiceboxv1alpha1.ReasonAgentSessionProviderErr, time.Minute)
			},
			wantClaims:  1,
			wantSummary: triggeranswer.StoppedSummary(spiceboxv1alpha1.ReasonAgentSessionProviderErr),
			wantMarker:  spiceboxv1alpha1.ReasonTriggerAnswered,
		},
		{
			name: "runner never became ready: answered 'could not start'",
			sess: func() *spiceboxv1alpha1.AgentSession {
				s := failedSession(tsKindName, spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed, time.Minute)
				s.Status.Conditions = nil
				return s
			},
			wantClaims:  1,
			wantSummary: triggeranswer.CouldNotStartSummary(spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed),
			wantMarker:  spiceboxv1alpha1.ReasonTriggerAnswered,
		},
		{
			name: "runner wrote startedAt but RunnerReady never landed: still 'stopped'",
			sess: func() *spiceboxv1alpha1.AgentSession {
				s := failedSession(tsKindName, spiceboxv1alpha1.ReasonAgentSessionBudget, time.Minute)
				s.Status.Conditions = nil
				started := metav1.Now()
				s.Status.StartedAt = &started
				return s
			},
			wantClaims:  1,
			wantSummary: triggeranswer.StoppedSummary(spiceboxv1alpha1.ReasonAgentSessionBudget),
			wantMarker:  spiceboxv1alpha1.ReasonTriggerAnswered,
		},
		{
			name: "agent already posted its verdict: kept, marked AlreadyAnswered",
			sess: func() *spiceboxv1alpha1.AgentSession {
				return failedSession(tsKindName, spiceboxv1alpha1.ReasonAgentSessionBudget, time.Minute)
			},
			arrange: func(k *operatorTriggerKind) {
				k.standing = &channelkinds.TriggerClaim{Concluded: true, Outcome: channelkinds.TriggerOutcomeClean}
			},
			wantClaims: 1,
			wantMarker: spiceboxv1alpha1.ReasonTriggerAlreadyAnswered,
		},
		{
			name: "provider unreadable: nothing written, no marker, retried",
			sess: func() *spiceboxv1alpha1.AgentSession {
				return failedSession(tsKindName, spiceboxv1alpha1.ReasonAgentSessionBudget, time.Minute)
			},
			arrange:    func(k *operatorTriggerKind) { k.claimErr = errors.New("status 503") },
			wantClaims: 1,
			wantRetry:  true,
		},
		{
			name: "Channel deleted: settled as Unanswerable naming it, no requeue",
			sess: func() *spiceboxv1alpha1.AgentSession {
				return failedSession(tsKindName, spiceboxv1alpha1.ReasonAgentSessionBudget, time.Minute)
			},
			withoutChannel: true,
			wantMarker:     spiceboxv1alpha1.ReasonTriggerUnanswerable,
			wantMessage:    "not answered: read the input Channel " + tsChannelName,
		},
		{
			name: "already settled: the provider is not touched again",
			sess: func() *spiceboxv1alpha1.AgentSession {
				s := failedSession(tsKindName, spiceboxv1alpha1.ReasonAgentSessionBudget, time.Minute)
				s.Status.Conditions = append(s.Status.Conditions, metav1.Condition{
					Type: spiceboxv1alpha1.AgentSessionConditionTriggerAnswered, Status: metav1.ConditionTrue,
					Reason: spiceboxv1alpha1.ReasonTriggerAnswered, LastTransitionTime: metav1.Now(),
				})
				return s
			},
			wantMarker: spiceboxv1alpha1.ReasonTriggerAnswered,
		},
		{
			name: "lifetime expiry of an idle session: not owed, provider not read",
			sess: func() *spiceboxv1alpha1.AgentSession {
				return failedSession(tsKindName, spiceboxv1alpha1.ReasonAgentSessionExpired, time.Minute)
			},
			wantMarker: spiceboxv1alpha1.ReasonTriggerNotOwed,
		},
		{
			name: "continued by a restart: not owed, provider not read",
			sess: func() *spiceboxv1alpha1.AgentSession {
				s := failedSession(tsKindName, spiceboxv1alpha1.ReasonAgentSessionBudget, time.Minute)
				s.Status.SupersededBy = "demo-reviewbot-gh-abc123-r1"
				return s
			},
			wantMarker: spiceboxv1alpha1.ReasonTriggerNotOwed,
		},
		{
			name: "failed long ago (past the window): not answered, marked AnswerWindowElapsed",
			sess: func() *spiceboxv1alpha1.AgentSession {
				return failedSession(tsKindName, spiceboxv1alpha1.ReasonAgentSessionBudget,
					agentsession.FailedTriggerAnswerWindowForTest+time.Minute)
			},
			wantMarker: spiceboxv1alpha1.ReasonTriggerAnswerWindowElapsed,
		},
		{
			name: "still Running: not the backstop's business",
			sess: func() *spiceboxv1alpha1.AgentSession {
				s := failedSession(tsKindName, "", time.Minute)
				s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
				return s
			},
		},
		{
			name: "conversational input kind: nothing owed, no marker written",
			sess: func() *spiceboxv1alpha1.AgentSession {
				return failedSession(fakekind.Kind{}.Name(), spiceboxv1alpha1.ReasonAgentSessionBudget, time.Minute)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := resetTriggerKind(t)
			if tc.arrange != nil {
				tc.arrange(k)
			}
			sess := tc.sess()
			r := triggerReconciler(t, sess)
			if tc.withoutChannel {
				require.NoError(t, r.Client.Delete(context.Background(), &spiceboxv1alpha1.Channel{
					ObjectMeta: metav1.ObjectMeta{Name: tsChannelName, Namespace: tsNS},
				}))
			}

			retry := r.ReconcileFailedTriggerForTest(context.Background(), sess)
			assert.Equal(t, tc.wantRetry, retry > 0, "requeue only when an attempt failed and another could succeed")

			claims, published := k.snapshot()
			assert.Equal(t, tc.wantClaims, claims)
			if tc.wantSummary != "" {
				require.Len(t, published, 1)
				assert.Equal(t, channelkinds.TriggerOutcomeCouldNotFinish, published[0].Outcome)
				assert.Equal(t, tc.wantSummary, published[0].Summary)
			} else {
				assert.Empty(t, published)
			}
			assert.Equal(t, tc.wantMarker, settledReason(t, r, sess))
			if tc.wantMessage != "" {
				c := conditions.Find(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionTriggerAnswered)
				require.NotNil(t, c)
				assert.Contains(t, c.Message, tc.wantMessage)
			}
		})
	}
}

// TestFailedTriggerBackstop_RetriesThenSettles: a transient provider failure
// is retried on the next reconcile rather than dropped.
func TestFailedTriggerBackstop_RetriesThenSettles(t *testing.T) {
	k := resetTriggerKind(t)
	k.claimErr = errors.New("status 503")
	sess := failedSession(tsKindName, spiceboxv1alpha1.ReasonAgentSessionRetryTimeout, time.Minute)
	r := triggerReconciler(t, sess)

	assert.Positive(t, r.ReconcileFailedTriggerForTest(context.Background(), sess))
	assert.Empty(t, settledReason(t, r, sess))

	k.mu.Lock()
	k.claimErr = nil
	k.mu.Unlock()
	assert.Zero(t, r.ReconcileFailedTriggerForTest(context.Background(), sess))

	_, published := k.snapshot()
	require.Len(t, published, 1, "the retry is what finally answers the pull request")
	assert.Equal(t, spiceboxv1alpha1.ReasonTriggerAnswered, settledReason(t, r, sess))
}

// TestFailedTriggerBackstop_SecondWriterSeesTheFirstAnswer is the two-writer
// property: the operator's immediate boot-failure answer lands first, and the
// backstop's later pass reads it back as AlreadyAnswered instead of publishing
// a second conclusion. A second pass after that never reaches the provider.
func TestFailedTriggerBackstop_SecondWriterSeesTheFirstAnswer(t *testing.T) {
	k := resetTriggerKind(t)
	sess := bootFailedSession(tsKindName)
	r := triggerReconciler(t, sess)

	_, err := r.MarkBootFailedForTest(context.Background(), sess,
		spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed, "boom")
	require.NoError(t, err)
	// Re-read what markBootFailed persisted, as the next reconcile would.
	require.NoError(t, r.Client.Get(context.Background(), client.ObjectKeyFromObject(sess), sess))
	require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, sess.Status.Phase)

	assert.Zero(t, r.ReconcileFailedTriggerForTest(context.Background(), sess))
	assert.Zero(t, r.ReconcileFailedTriggerForTest(context.Background(), sess))

	claims, published := k.snapshot()
	assert.Len(t, published, 1, "exactly one conclusion reaches the pull request")
	assert.Equal(t, 2, claims, "the boot-failure answer and the backstop's one check; the settled pass reads nothing")
	assert.Equal(t, spiceboxv1alpha1.ReasonTriggerAlreadyAnswered, settledReason(t, r, sess))
}

// TestFailedTriggerBackstop_BoundsTheProviderCall: the answer runs under its
// own deadline, so a provider that hangs cannot hold a reconcile worker — and
// under the backstop's tighter one, not the immediate answers', because this
// attempt is the one that repeats.
func TestFailedTriggerBackstop_BoundsTheProviderCall(t *testing.T) {
	k := resetTriggerKind(t)
	sess := failedSession(tsKindName, spiceboxv1alpha1.ReasonAgentSessionBudget, time.Minute)
	r := triggerReconciler(t, sess)

	before := time.Now()
	r.ReconcileFailedTriggerForTest(context.Background(), sess)

	k.mu.Lock()
	defer k.mu.Unlock()
	require.True(t, k.sawDeadline, "Claim must run under a bounded context")
	assert.LessOrEqual(t, k.deadline.Sub(before), agentsession.FailedTriggerAnswerTimeoutForTest+time.Second,
		"the backstop's own bound, not the immediate answers' longer one")
	assert.Less(t, agentsession.FailedTriggerAnswerTimeoutForTest, agentsession.TriggerAnswerTimeoutForTest)
}

// TestFailedTriggerBackstop_HangingProviderIsCutOffAtTheBound drives a
// provider that never answers and asserts the pass returns, asking for a
// retry, once its bound expires.
func TestFailedTriggerBackstop_HangingProviderIsCutOffAtTheBound(t *testing.T) {
	k := resetTriggerKind(t)
	k.hang = true
	sess := failedSession(tsKindName, spiceboxv1alpha1.ReasonAgentSessionBudget, time.Minute)
	r := triggerReconciler(t, sess)

	// The reconcile's own context stands in for the bound here so the test
	// does not sit out the real one; the answer derives its deadline from it.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan time.Duration, 1)
	go func() { done <- r.ReconcileFailedTriggerForTest(ctx, sess) }()

	select {
	case retry := <-done:
		assert.Positive(t, retry, "a cut-off attempt is retried, not settled")
	case <-time.After(5 * time.Second):
		t.Fatal("the backstop did not return once its context expired")
	}
	assert.Empty(t, settledReason(t, r, sess))
}
