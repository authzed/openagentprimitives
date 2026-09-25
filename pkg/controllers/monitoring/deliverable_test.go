package monitoring

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

func channelTarget(t *testing.T) Target {
	t.Helper()
	for _, tg := range Targets() {
		if tg.GVKName == "Channel" {
			return tg
		}
	}
	t.Fatal("Channel target missing")
	return Target{}
}

// chWithDeliverable returns an output Channel carrying only a Deliverable
// condition — Connected deliberately absent, so any event this reconcile
// emits can only have come from a Deliverable rule.
func chWithDeliverable(status metav1.ConditionStatus, reason, message string) *spiceboxv1alpha1.Channel {
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo-out"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "slack", Role: "output"},
	}
	ch.Status.Conditions = []metav1.Condition{
		{Type: spiceboxv1alpha1.ChannelConditionDeliverable, Status: status, Reason: reason, Message: message},
	}
	return ch
}

func reconcileCh(t *testing.T, r *Reconciler) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: "default", Name: "demo-out"},
	})
	require.NoError(t, err)
}

// TestChannelDeliverableRule_FailureEmitsThenRecovers: the whole point of the
// Deliverable condition is that a channel silently swallowing agent replies
// (bot never invited to its Slack channel) reaches the monitoring channel.
// Deliverable=False must emit an error-level transport event, and the flip
// back to True — the operator invited the bot, a send landed — must emit the
// matching recovery.
func TestChannelDeliverableRule_FailureEmitsThenRecovers(t *testing.T) {
	scheme := testfixtures.NewScheme(t)
	rec := &recorder{}
	ch := chWithDeliverable(metav1.ConditionFalse,
		spiceboxv1alpha1.ReasonChannelDeliveryFailed, "not_in_channel")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ch).
		WithStatusSubresource(&spiceboxv1alpha1.Channel{}).Build()
	r := &Reconciler{Client: c, Publish: rec.publish, Target: channelTarget(t), tracker: newTracker()}

	reconcileCh(t, r)

	events := rec.snapshot()
	require.Len(t, events, 1, "Deliverable=False must emit exactly one monitoring event")
	assert.Equal(t, channelevents.MonitoringTransitionFailed, events[0].Transition)
	assert.Equal(t, channelevents.MonitoringLevelError, events[0].Level)
	assert.Equal(t, "transport", events[0].Category)
	assert.Equal(t, spiceboxv1alpha1.ChannelConditionDeliverable, events[0].Condition)
	assert.Equal(t, "not_in_channel", events[0].Summary,
		"the send error must ride along so the operator sees WHY deliveries fail")

	// Recovery: a send landed, the relay flipped the condition True.
	var cur spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "demo-out"}, &cur))
	cur.Status.Conditions = []metav1.Condition{
		{Type: spiceboxv1alpha1.ChannelConditionDeliverable, Status: metav1.ConditionTrue,
			Reason: spiceboxv1alpha1.ReasonChannelDeliverySucceeded},
	}
	require.NoError(t, c.Status().Update(context.Background(), &cur))
	reconcileCh(t, r)

	events = rec.snapshot()
	require.Len(t, events, 2, "the True flip must emit the matching recovery")
	assert.Equal(t, channelevents.MonitoringTransitionRecovered, events[1].Transition)
	assert.Equal(t, channelevents.MonitoringLevelError, events[1].Level,
		"recovery carries the failure's level so the same filter surfaces both")
}

// TestChannelDeliverableRule_AbsentConditionIsSilent: a Channel that has
// never had an outbound send attempted carries no Deliverable condition, and
// must emit nothing — absence of evidence is not a failure.
func TestChannelDeliverableRule_AbsentConditionIsSilent(t *testing.T) {
	scheme := testfixtures.NewScheme(t)
	rec := &recorder{}
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo-out"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "slack", Role: "output"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ch).
		WithStatusSubresource(&spiceboxv1alpha1.Channel{}).Build()
	r := &Reconciler{Client: c, Publish: rec.publish, Target: channelTarget(t), tracker: newTracker()}

	reconcileCh(t, r)

	assert.Empty(t, rec.snapshot(), "no Deliverable condition ⇒ no event")
}
