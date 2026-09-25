// pkg/channels/channelsd/pipeline/self_join_guard_test.go
//
// A session's own starter must never be routed through the session-join
// approval flow. Production symptom this pins: a fresh session whose
// agentsession#owner tuple had not been written yet (the operator writes it;
// channelsd only writes started_by at create time) denied its OWN starter the
// interact check — `interact = owner + participant - denied` — and
// handlePermissionDeny then raised a join request addressed to the started-by
// user, i.e. one whose requester and whose sole approver are the same person.
//
// That prompt is unactionable, not merely confusing: `approve = owner` resolves
// off the SAME missing tuple, so the one person the Approve button was sent to
// cannot pass its decider check either. The thread wedges, with a durable
// PendingRequester + PermissionRequestPending condition stamped behind it.
package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// TestHandlePermissionDenyOnStarterRefusesSelfAddressedJoinRequest: when the
// denied requester IS the session's started-by user, no join request is
// published, nothing durable is stamped, and the user is told why their
// message was dropped instead of being asked to approve themselves.
func TestHandlePermissionDenyOnStarterRefusesSelfAddressedJoinRequest(t *testing.T) {
	starter := channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "USTARTER", Email: "starter@example.com"}
	sess := existingSession(t, "c1-self-join", starter.ExternalID.String(), spiceboxv1alpha1.AgentSessionPhaseRunning)
	// Byte-identical to what the fresh-session path stamps at create time
	// ("user:"+canonical.String()), so the comparison under test is the real one.
	sess.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID] = "user:" + canonicalID(starter).String()
	sess.Annotations[spiceboxv1alpha1.AnnotationStartedByEmail] = starter.Email.String()

	p, az, _, nats, _ := newPipeline(t, sess)
	az.checkResult = false // the ownerless-session deny this guard exists for

	dec, err := p.handlePermissionDeny(context.Background(), sess, channelkinds.InboundEvent{
		ExternalIDs: starter,
		ChannelKey:  "thread:C1:1",
		MessageText: "stop",
	})
	require.NoError(t, err, "handlePermissionDeny")

	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome, "dec.Outcome")
	assert.False(t, dec.Notice.IsSuppressed(),
		"the starter must be told their message was dropped — a suppressed notice strands them")
	assert.NotContains(t, dec.Notice.Args().Body, "awaiting approval",
		"must not tell the starter their own join request is awaiting approval")

	assert.Empty(t, nats.subjects,
		"no interaction_request may be published: its only approver would be the requester themselves")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, p.K8s.Get(context.Background(), client.ObjectKeyFromObject(sess), &got), "Get after handlePermissionDeny")
	assert.Empty(t, got.Status.PendingRequesters,
		"no durable pending-join record: there is no approver who could ever resolve it")
	if cond := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionPermissionRequestPending); cond != nil {
		assert.NotEqual(t, metav1.ConditionTrue, cond.Status,
			"PermissionRequestPending must not go True for a request that was never raised")
	}
}
