package main

import (
	"context"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	slackkind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// recordingResolver counts SenderFor invocations so a test can assert that a
// session was skipped entirely (zero resolutions) versus processed.
type recordingResolver struct{ senderForCalls int }

func (r *recordingResolver) SenderFor(context.Context, *spiceboxv1alpha1.AgentSession) (channelkinds.Sender, error) {
	r.senderForCalls++
	return noopSender{}, nil
}

func (r *recordingResolver) SubChannelSenderFor(context.Context, *spiceboxv1alpha1.AgentSession, string) (channelkinds.Sender, error) {
	return nil, nil
}

func (r *recordingResolver) StreamDeltaSinkFor(context.Context, *spiceboxv1alpha1.AgentSession) (channelkinds.StreamDeltaSink, error) {
	return nil, nil
}

type noopSender struct{}

func (noopSender) Send(context.Context, channelkinds.SessionInfo, channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	return channelkinds.SubChannelSendResult{}, nil
}

// failedChannelSession builds a Failed, channel-attached session with the given
// input and output channel kinds (the LabelChannelName label makes the
// sessionWatcher's label-selected List pick it up). Input and output are
// separate because channelsd surfaces to the OUTPUT channel: a triggered
// session is routinely cross-transport (e.g. github-in / slack-out), and the
// watcher's skip gate must be decided by the output kind, not the input.
func failedChannelSession(name, inputKind, outputKind string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: name, UID: types.UID(name + "-uid"),
			Labels: map[string]string{spiceboxv1alpha1.LabelChannelName: "ch"},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel:  &spiceboxv1alpha1.ChannelBinding{Name: "ch", Kind: inputKind},
			OutputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "out", Kind: outputKind},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:         spiceboxv1alpha1.AgentSessionPhaseFailed,
			FailureReason: "BudgetExceeded",
		},
	}
}

// TestSessionWatcher_SkipsClientHostedOutputKind proves channelsd's
// sessionWatcher does not touch a session whose OUTPUT is client-hosted
// (browser → webd). webd surfaces the failure; channelsd has no transport for
// it, so resolving a sender fails with `unknown kind "browser"` and — because
// reportFailure errors before markReported — spams that ERROR every 5s forever.
// The gate skips such sessions by their OUTPUT kind.
//
// Both halves are asserted: no sender resolved AND nothing published. The
// sender counter alone stopped discriminating once the notice moved onto the
// publish path — reportFailure resolves no sender for ANY session now — so a
// skip test that only counted resolutions would pass on a watcher that
// happily published a browser session's failure onto the bus.
func TestSessionWatcher_SkipsClientHostedOutputKind(t *testing.T) {
	sess := failedChannelSession("bi", browser.KindName, browser.KindName)
	cli := fake.NewClientBuilder().
		WithScheme(makeScheme()).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	res := &recordingResolver{}
	pub := &publishRecorder{}
	w := newSessionWatcher(cli, res, nil)
	w.publish = pub.publish

	w.reconcile(context.Background(), testr.New(t))

	assert.Zero(t, res.senderForCalls,
		"a browser-OUTPUT (client/webd-hosted) session must be skipped — channelsd must not resolve a sender for it")
	assert.Empty(t, pub.interactions(t, "default", "bi"),
		"a browser-output session's failure is webd's to surface; channelsd must not publish it")
}

// TestSessionWatcher_ProcessesRelayedKind is the guard against over-skipping: a
// channelsd-relayed kind (fake in and out) must still have its failure reported.
//
// Asserted as "a failure notice reached the bus" rather than "a sender was
// resolved": the notice is only deliverable through the relay, so the publish is
// the outcome that matters — and unlike a sender-resolution counter, it still
// catches a notice that fails to build.
func TestSessionWatcher_ProcessesRelayedKind(t *testing.T) {
	sess := failedChannelSession("fk", "fake", "fake")
	cli := fake.NewClientBuilder().
		WithScheme(makeScheme()).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	pub := &publishRecorder{}
	w := newSessionWatcher(cli, &recordingResolver{}, nil)
	w.publish = pub.publish

	w.reconcile(context.Background(), testr.New(t))

	got := pub.interactions(t, "default", "fk")
	require.Len(t, got, 1,
		"a channelsd-relayed (fake) session's failure must still be reported")
	assert.Equal(t, categories.AgentFailed, got[0].Category)
}

// TestSessionWatcher_ProcessesRelayedOutputDespiteClientHostedInput is the
// reviewbot regression: a triggered session is github-in (webd-hosted) but
// slack-out (channelsd-relayed). Gating on the INPUT kind skipped the whole
// session, so its Slack thread never received the terminal failure notice — the
// silent hang the operator saw. The failure MUST be published because the
// OUTPUT channel is one channelsd relays.
func TestSessionWatcher_ProcessesRelayedOutputDespiteClientHostedInput(t *testing.T) {
	sess := failedChannelSession("rb", "github", slackkind.KindName)
	cli := fake.NewClientBuilder().
		WithScheme(makeScheme()).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	pub := &publishRecorder{}
	w := newSessionWatcher(cli, &recordingResolver{}, nil)
	w.publish = pub.publish

	w.reconcile(context.Background(), testr.New(t))

	got := pub.interactions(t, "default", "rb")
	require.Len(t, got, 1,
		"a github-in/slack-out session's failure must reach the Slack thread — the gate must decide by the output kind, not the input")
	assert.Equal(t, categories.AgentFailed, got[0].Category)
}
