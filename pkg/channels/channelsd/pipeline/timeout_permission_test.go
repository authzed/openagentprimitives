// pkg/channels/channelsd/pipeline/timeout_permission_test.go
//
// TimeoutPermissionRequest / publishPermissionTimeoutApplied must publish the
// generic interaction_applied kind (category permission_request,
// Outcome=Expired), never permission_decision_applied. The Slack kind has no
// consumer for the latter, so publishing it would strand a timed-out join
// prompt on the requester's surface forever; only the generic
// sendDecisionApplied path (pkg/channels/channelkinds/slack/interaction.go)
// resolves the card.
package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// TestTimeoutPermissionRequestPublishesInteractionApplied verifies a lapsed
// join request publishes a generic interaction_applied envelope — not the
// legacy permission_decision_applied — on the session's OUT subject, and
// clears the matching PendingRequesters entry, flipping
// PermissionRequestPending False once none remain.
func TestTimeoutPermissionRequestPublishesInteractionApplied(t *testing.T) {
	sess := existingSession(t, "c1-perm-timeout", "U_OWNER", spiceboxv1alpha1.AgentSessionPhaseRunning)
	p, _, _, nats, cli := newPipeline(t, sess)

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(sess), &got), "get session before seeding pending requester")
	got.Status.PendingRequesters = []spiceboxv1alpha1.PendingRequester{{
		Kind:        "slack",
		ExternalID:  "UREQ",
		Email:       "requester@example.com",
		RequestRef:  "perm-timeout-1",
		RequestedAt: metav1.Now(),
	}}
	got.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentSessionConditionPermissionRequestPending,
		Status:             metav1.ConditionTrue,
		Reason:             "RequesterPending",
		LastTransitionTime: metav1.Now(),
	}}
	seedApprovalStatus(t, cli, &got)

	require.NoError(t, p.TimeoutPermissionRequest(context.Background(), sess.Namespace, sess.Name, "perm-timeout-1"), "TimeoutPermissionRequest")

	// permission_decision_applied must NOT be published: no surface consumes it.
	legacySuffix := ".out." + string(channelevents.KindPermissionDecisionApplied)
	for _, s := range nats.subjects {
		assert.False(t, strings.HasSuffix(s, legacySuffix), "must not publish the legacy permission_decision_applied kind; subject=%s", s)
	}

	// The generic interaction_applied envelope must land on OUT (no .in — a
	// join is pre-runner, so there is no runner-side resume to unblock).
	appliedSuffix := ".out." + string(channelevents.KindInteractionApplied)
	found := false
	for i, s := range nats.subjects {
		if !strings.HasSuffix(s, appliedSuffix) {
			continue
		}
		var env channelevents.Envelope
		require.NoError(t, json.Unmarshal(nats.payloads[i], &env), "unmarshal envelope")
		var pl channelevents.InteractionAppliedPayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl), "unmarshal applied payload")
		assert.Equal(t, categories.PermissionRequest, pl.Category, "Category")
		assert.Equal(t, "perm-timeout-1", pl.RequestRef, "RequestRef")
		assert.Equal(t, channelevents.OutcomeExpired, pl.Outcome, "Outcome")
		assert.Nil(t, pl.DecidedBy, "no one decided a timeout")
		assert.Empty(t, pl.ResponseRef, "no click means no response_url to round-trip")
		found = true
	}
	assert.True(t, found, "expected an OUT interaction_applied envelope; subjects=%v", nats.subjects)

	var after spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(sess), &after), "Get after TimeoutPermissionRequest")
	assert.Empty(t, after.Status.PendingRequesters, "pending entry cleared on timeout")
	cond := conditions.Find(after.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionPermissionRequestPending)
	if assert.NotNil(t, cond, "PermissionRequestPending condition") {
		assert.Equal(t, metav1.ConditionFalse, cond.Status, "condition flips False once no requesters remain")
	}
}
