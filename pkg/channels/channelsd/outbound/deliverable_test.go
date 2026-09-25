package outbound

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// failingSender fails every Send with a fixed error — the shape of a channel
// that is genuinely unreachable (bot not in the Slack channel, deleted
// destination), where the degraded fallback notice fails identically.
type failingSender struct{}

func (failingSender) Send(_ context.Context, _ channelkinds.SessionInfo, _ channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	return channelkinds.SubChannelSendResult{}, errSendFailed
}

// withRecordDeliverability opts the relay into Channel Deliverable-condition
// recording, as channelsd's wiring does.
func withRecordDeliverability() func(*Relay) {
	return func(r *Relay) { r.RecordDeliverability = true }
}

// fakeClientWithChannelStatus is fakeClientWith plus the Channel status
// subresource, which the relay's Status().Patch needs the fake to model —
// without the registration the fake returns NotFound for status writes on an
// object that plainly exists.
func fakeClientWithChannelStatus(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(scheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.Channel{}).
		WithObjects(objs...).Build()
}

// deliverableChannel returns the Channel CR the defaultSession()'s binding
// ("c1", kind fake) names, optionally pre-seeded with a Deliverable condition.
func deliverableChannel(conds ...metav1.Condition) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "fake", Role: "both"},
		Status:     spiceboxv1alpha1.ChannelStatus{Conditions: conds},
	}
}

// getDeliverable fetches the Channel and returns its Deliverable condition, or
// nil when absent.
func getDeliverable(t *testing.T, cli client.Client) *metav1.Condition {
	t.Helper()
	var ch spiceboxv1alpha1.Channel
	require.NoError(t, cli.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "c1"}, &ch), "Get Channel c1")
	for i := range ch.Status.Conditions {
		if ch.Status.Conditions[i].Type == spiceboxv1alpha1.ChannelConditionDeliverable {
			return &ch.Status.Conditions[i]
		}
	}
	return nil
}

// channelRV fetches the Channel's current resourceVersion.
func channelRV(t *testing.T, cli client.Client) string {
	t.Helper()
	var ch spiceboxv1alpha1.Channel
	require.NoError(t, cli.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "c1"}, &ch), "Get Channel c1")
	return ch.ResourceVersion
}

// TestRelay_SendFailure_MarksChannelUndeliverable: a Send that fails — and
// whose fallback notice fails the same way — must stamp
// Deliverable=False/DeliveryFailed with the send error on the Channel the
// envelope was routed through, so the monitoring watcher can announce it.
// This is the "bot was never invited to the channel" incident that previously
// died as a channelsd log line while every review's delivery was silently
// lost.
func TestRelay_SendFailure_MarksChannelUndeliverable(t *testing.T) {
	nc := connectNATS(t)
	cli := fakeClientWithChannelStatus(t, defaultSession(), deliverableChannel())

	startRelay(t, nc, cli, &fixedResolver{s: failingSender{}}, withRecordDeliverability())

	env := buildEnv(t, "foo", channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "the lost reply"})
	publishOut(t, nc, "default", "foo", "user_message", env)

	require.True(t, waitUntil(t, 2*time.Second, func() bool {
		c := getDeliverable(t, cli)
		return c != nil && c.Status == metav1.ConditionFalse
	}), "Deliverable=False was never stamped on the Channel")

	c := getDeliverable(t, cli)
	require.NotNil(t, c)
	assert.Equal(t, spiceboxv1alpha1.ReasonChannelDeliveryFailed, c.Reason)
	assert.Contains(t, c.Message, "simulated send failure",
		"the condition message must carry the send error an operator needs")

	// A second identical failure must NOT rewrite status: a broken channel
	// receives every envelope the session emits, and one status write per
	// failed envelope would churn the apiserver and the monitoring watch for
	// no new information.
	rv := channelRV(t, cli)
	publishOut(t, nc, "default", "foo", "user_message", env)
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, rv, channelRV(t, cli),
		"an unchanged failure outcome must not rewrite Channel status")
}

// TestRelay_SendSuccess_RestoresChannelDeliverable: once a send lands again,
// the condition must flip back to True/DeliverySucceeded so the monitoring
// watcher can announce recovery — the operator invited the bot, the next
// delivery worked, the incident is over.
func TestRelay_SendSuccess_RestoresChannelDeliverable(t *testing.T) {
	nc := connectNATS(t)
	cli := fakeClientWithChannelStatus(t, defaultSession(), deliverableChannel(metav1.Condition{
		Type:               spiceboxv1alpha1.ChannelConditionDeliverable,
		Status:             metav1.ConditionFalse,
		Reason:             spiceboxv1alpha1.ReasonChannelDeliveryFailed,
		Message:            "simulated send failure",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
	}))

	startRelay(t, nc, cli, &fixedResolver{s: &captureSender{}}, withRecordDeliverability())

	env := buildEnv(t, "foo", channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "delivered at last"})
	publishOut(t, nc, "default", "foo", "user_message", env)

	require.True(t, waitUntil(t, 2*time.Second, func() bool {
		c := getDeliverable(t, cli)
		return c != nil && c.Status == metav1.ConditionTrue
	}), "Deliverable never recovered to True after a successful send")

	c := getDeliverable(t, cli)
	require.NotNil(t, c)
	assert.Equal(t, spiceboxv1alpha1.ReasonChannelDeliverySucceeded, c.Reason)
}

// TestRelay_SendSuccess_AlreadyDeliverable_NoStatusWrite: the healthy steady
// state — every send succeeding on a channel already marked True — must cost
// zero status writes. This is the guard that keeps the condition from turning
// every relayed envelope into an apiserver write.
func TestRelay_SendSuccess_AlreadyDeliverable_NoStatusWrite(t *testing.T) {
	nc := connectNATS(t)
	cli := fakeClientWithChannelStatus(t, defaultSession(), deliverableChannel(metav1.Condition{
		Type:               spiceboxv1alpha1.ChannelConditionDeliverable,
		Status:             metav1.ConditionTrue,
		Reason:             spiceboxv1alpha1.ReasonChannelDeliverySucceeded,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
	}))

	sndr := &captureSender{}
	startRelay(t, nc, cli, &fixedResolver{s: sndr}, withRecordDeliverability())

	rv := channelRV(t, cli)
	env := buildEnv(t, "foo", channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "routine reply"})
	publishOut(t, nc, "default", "foo", "user_message", env)

	require.True(t, waitUntil(t, 2*time.Second, func() bool {
		return sndr.count() >= 1
	}), "send never reached the Sender")
	// Nothing to positively wait for on the no-write side; settle briefly.
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, rv, channelRV(t, cli),
		"a send matching the recorded True outcome must not rewrite Channel status")
}

// TestRelay_EnvelopeSpecificFailure_FallbackSuccessRestoresDeliverable: when
// the original user_message Send fails but the degraded plain-text fallback on
// the SAME sender lands, the channel itself is provably deliverable — the
// failure was envelope-specific (over-long text, a bad attachment). The
// condition must converge to True rather than leaving a healthy channel
// flagged as an incident.
func TestRelay_EnvelopeSpecificFailure_FallbackSuccessRestoresDeliverable(t *testing.T) {
	nc := connectNATS(t)
	cli := fakeClientWithChannelStatus(t, defaultSession(), deliverableChannel())

	sndr := &failFirstSender{}
	startRelay(t, nc, cli, &fixedResolver{s: sndr}, withRecordDeliverability())

	env := buildEnv(t, "foo", channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "the real reply"})
	publishOut(t, nc, "default", "foo", "user_message", env)

	// Both sends observed: the failing original + the succeeding fallback.
	require.True(t, waitUntil(t, 2*time.Second, func() bool {
		return len(sndr.envelopes()) >= 2
	}), "fallback notice was never attempted")

	require.True(t, waitUntil(t, 2*time.Second, func() bool {
		c := getDeliverable(t, cli)
		return c != nil && c.Status == metav1.ConditionTrue
	}), "Deliverable must converge to True once the fallback send proves the channel works")
}
