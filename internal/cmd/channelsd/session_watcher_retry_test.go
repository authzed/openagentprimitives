package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// TestSessionWatcher_AwaitingRetry_PublishesEnvelope_OncePerAttempt
// verifies the AwaitingRetry branch publishes exactly one generic
// interaction_request(provider_error_retry) envelope per (uid, RetryAttempts)
// tuple, with an AudienceParticipants broadcast, the same reason Lead + fenced
// truncated message + trailer the legacy KindProviderErrorRetry text carried,
// and a single generic ActionKindDecision "retry" button.
func TestSessionWatcher_AwaitingRetry_PublishesEnvelope_OncePerAttempt(t *testing.T) {
	ctx := context.Background()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "s1",
			Namespace: "default",
			UID:       "uid-1",
			Labels:    map[string]string{spiceboxv1alpha1.LabelChannelName: "ch"},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:        "agent-a",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "ch"},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:         spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry,
			FailureReason: spiceboxv1alpha1.ReasonAgentSessionProviderErr,
			RetryAttempts: 1,
			Conditions: []metav1.Condition{{
				Type:    spiceboxv1alpha1.AgentSessionConditionAwaitingRetry,
				Status:  metav1.ConditionTrue,
				Reason:  spiceboxv1alpha1.ReasonAgentSessionProviderErr,
				Message: "anthropic: credit balance too low",
			}},
		},
	}
	cli := fakeclient.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).Build()
	pub := &recordingPub{}
	w := newSessionWatcher(cli, nil, nil)
	w.publish = pub.Publish

	w.reconcile(ctx, testr.New(t))
	require.Len(t, pub.envelopes, 1, "exactly one envelope on first observation")
	assert.Equal(t, channelevents.KindInteractionRequest, pub.envelopes[0].Kind)

	var req channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(pub.envelopes[0].Payload, &req), "unmarshal InteractionRequestPayload")
	assert.Equal(t, categories.ProviderErrorRetry, req.Category, "Category")
	assert.Equal(t, channelevents.AudienceParticipants, req.Audience.Scope,
		"Audience.Scope must be the addressee-less participants broadcast")
	assert.Equal(t, "⚠️ Agent failed: "+sess.Status.FailureReason, req.Lead, "Lead carries the failure reason")
	// Body reproduces legacy buildRetryPromptText minus the (now-Lead) reason
	// line: the fenced truncated message + the "Click Retry" trailer.
	assert.Contains(t, req.Body, "```\nanthropic: credit balance too low\n```", "Body carries the fenced error message")
	assert.Contains(t, req.Body, "Click Retry to try again.", "Body carries the retry trailer")
	require.Len(t, req.Actions, 1, "one Retry action")
	assert.Equal(t, "retry", req.Actions[0].ID, "action id")
	assert.Equal(t, channelevents.ActionKindDecision, req.Actions[0].Kind, "Retry is a generic decision action")
	assert.Equal(t, channelevents.ActionStylePrimary, req.Actions[0].Style, "Retry is styled primary")
	require.NoError(t, req.Validate(), "published payload must be wire-valid")

	// Second reconcile with same RetryAttempts — must dedupe.
	w.reconcile(ctx, testr.New(t))
	assert.Len(t, pub.envelopes, 1, "second tick must not re-publish (same RetryAttempts)")

	// Bump RetryAttempts (simulates a retry that also failed) — must re-publish.
	require.NoError(t, cli.Patch(ctx, sess, client.RawPatch(types.MergePatchType,
		[]byte(`{"status":{"retryAttempts":2}}`))))
	w.reconcile(ctx, testr.New(t))
	assert.Len(t, pub.envelopes, 2, "RetryAttempts bump must trigger a fresh publish")
}

// awaitingRetrySession builds a session parked in AwaitingRetry at the given
// attempt — the state a provider 5xx leaves behind, which persists until the
// user clicks Retry or the 30-minute retry TTL expires it.
func awaitingRetrySession(attempts int32) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "s1", Namespace: "default", UID: "uid-1",
			Labels: map[string]string{spiceboxv1alpha1.LabelChannelName: "ch"},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:        "agent-a",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "ch"},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:         spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry,
			FailureReason: spiceboxv1alpha1.ReasonAgentSessionProviderErr,
			RetryAttempts: attempts,
			Conditions: []metav1.Condition{{
				Type:    spiceboxv1alpha1.AgentSessionConditionAwaitingRetry,
				Status:  metav1.ConditionTrue,
				Reason:  spiceboxv1alpha1.ReasonAgentSessionProviderErr,
				Message: "provider unavailable",
			}},
		},
	}
}

// restartedWatcher returns a watcher with the dedup state a freshly-started
// channelsd process has: none.
func restartedWatcher(cli client.Client, pub *recordingPub) *sessionWatcher {
	w := newSessionWatcher(cli, nil, nil)
	w.publish = pub.Publish
	return w
}

func TestSessionWatcher_AwaitingRetry_DoesNotRepublishAfterProcessRestart(t *testing.T) {
	ctx := context.Background()
	sess := awaitingRetrySession(1)
	cli := fakeclient.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).Build()
	pub := &recordingPub{}

	restartedWatcher(cli, pub).reconcile(ctx, testr.New(t))
	require.Len(t, pub.envelopes, 1)

	// channelsd is redeployed inside the 30-minute AwaitingRetry window. The
	// phase is still AwaitingRetry and RetryAttempts has not moved, so without
	// durable dedup the fresh process posts a SECOND Retry card into the thread
	// — two live buttons for one stalled turn, one of them stale.
	restartedWatcher(cli, pub).reconcile(ctx, testr.New(t))

	assert.Len(t, pub.envelopes, 1,
		"a channelsd restart must not re-post a Retry prompt the user already has")
}

func TestSessionWatcher_AwaitingRetry_RepublishesNewAttemptAfterProcessRestart(t *testing.T) {
	ctx := context.Background()
	sess := awaitingRetrySession(1)
	cli := fakeclient.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).Build()
	pub := &recordingPub{}

	restartedWatcher(cli, pub).reconcile(ctx, testr.New(t))
	require.Len(t, pub.envelopes, 1)

	// The user clicked Retry, it failed again, and channelsd restarted. The new
	// attempt is a new prompt and must survive the restart in this direction.
	require.NoError(t, cli.Patch(ctx, sess, client.RawPatch(types.MergePatchType,
		[]byte(`{"status":{"retryAttempts":2}}`))))
	restartedWatcher(cli, pub).reconcile(ctx, testr.New(t))

	assert.Len(t, pub.envelopes, 2, "a new retry attempt must post even across a restart")
}

// recordingPub is a no-op NATS publish that records every envelope.
type recordingPub struct {
	envelopes []channelevents.Envelope
}

func (r *recordingPub) Publish(subject string, data []byte) error {
	var env channelevents.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return err
	}
	r.envelopes = append(r.envelopes, env)
	return nil
}
