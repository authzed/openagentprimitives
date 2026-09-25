package outbound

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"

	// The relay asks registry.DeliversToHuman for the answer, so these tests
	// must run against a populated registry or every human-directed envelope
	// drops on "unknown kind". Registering the real kinds (rather than
	// stubbing the predicate) is the point: it is the shipped agent/fake/slack
	// answers this routing rule stands or falls on. bento is here because
	// relay_test.go's split-channel fixtures bind it.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// boundSession builds an AgentSession bound to a Channel of channelKind,
// optionally parented to parentName. An empty channelKind leaves it headless.
func boundSession(name, parentName, channelKind string) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
	}
	if parentName != "" {
		s.Spec.Parent = &spiceboxv1alpha1.NamespacedRef{Namespace: "default", Name: parentName}
	}
	if channelKind != "" {
		s.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{
			Name:              name + "-" + channelKind + "-channel",
			Kind:              channelKind,
			NATSSubjectPrefix: "ap.session.default." + name,
		}
	}
	return s
}

// TestRelay_ConversationalChildRouting covers the routing split a conversational
// subagent forces: the child IS channel-attached, to an `agent` Channel whose
// far side is its parent SESSION. Its own binding is therefore the right
// destination for its ordinary output and the wrong one for anything a person
// has to answer.
//
// Every fixture here makes root, mid and leaf three DIFFERENT sessions with
// three different bindings. Collapse any two and the test passes whether or
// not the split exists.
func TestRelay_ConversationalChildRouting(t *testing.T) {
	t.Run("human-directed envelope from an agent-bound leaf resolves to the root's human binding", func(t *testing.T) {
		root := boundSession("hr-root", "", "fake")
		mid := boundSession("hr-mid", root.Name, "")
		leaf := boundSession("hr-leaf", mid.Name, "agent")

		nc := connectNATS(t)
		cli := fakeClientWith(t, root, mid, leaf)
		sndr := &captureSessionInfoSender{}
		res := &fixedResolver{subS: sndr}
		startRelay(t, nc, cli, res)

		publishOut(t, nc, "default", leaf.Name, "interaction_request",
			interactionRequestEnvelope(t, leaf.Name))

		require.True(t, waitUntil(t, 2*time.Second, func() bool {
			_, ok := sndr.first()
			return ok
		}), "sender.Send was never called")

		got, ok := sndr.first()
		require.True(t, ok)
		require.NotNil(t, got.Channel, "SessionInfo.Channel is nil")
		assert.Equal(t, root.Spec.InputChannel.Name, got.Channel.Name,
			"a permission prompt must land on the root's human surface")
		assert.NotEqual(t, leaf.Spec.InputChannel.Name, got.Channel.Name,
			"delivering it to the leaf's own agent Channel would hand a human's decision to the parent agent")
		// The card is still ABOUT the leaf; only its delivery moved.
		assert.Equal(t, leaf.Name, got.Name)

		// The resolver must be handed the resolved binding too, or it would
		// build an agent Sender for a card the walk just routed to a human.
		lookupBinding := res.lastSubChannelBinding()
		require.NotNil(t, lookupBinding, "SubChannelSenderFor must have been called")
		assert.Equal(t, "fake", lookupBinding.Kind,
			"the sub-channel Sender must be built against the human Channel, not the leaf's agent one")
	})

	t.Run("an agent binding on the middle ancestor is skipped too, not just the leaf's own", func(t *testing.T) {
		// The case that distinguishes this walk from a plain first-non-nil
		// one: mid HAS a binding, and it is still not an answer.
		root := boundSession("skip-root", "", "slack")
		mid := boundSession("skip-mid", root.Name, "agent")
		leaf := boundSession("skip-leaf", mid.Name, "agent")

		nc := connectNATS(t)
		cli := fakeClientWith(t, root, mid, leaf)
		sndr := &captureSessionInfoSender{}
		startRelay(t, nc, cli, &fixedResolver{subS: sndr})

		publishOut(t, nc, "default", leaf.Name, "interaction_request",
			interactionRequestEnvelope(t, leaf.Name))

		require.True(t, waitUntil(t, 2*time.Second, func() bool {
			_, ok := sndr.first()
			return ok
		}), "sender.Send was never called")

		got, ok := sndr.first()
		require.True(t, ok)
		require.NotNil(t, got.Channel)
		assert.Equal(t, root.Spec.InputChannel.Name, got.Channel.Name,
			"the walk must climb past mid's agent binding to the root's human one")
		assert.NotEqual(t, mid.Spec.InputChannel.Name, got.Channel.Name,
			"stopping at the first non-nil binding would deliver a human's prompt to mid's parent agent")
	})

	t.Run("ordinary output from the same leaf still routes to the leaf's own agent Channel", func(t *testing.T) {
		// The conversational half, which must keep working: talking to your
		// parent is exactly what an agent binding is for.
		root := boundSession("conv-root", "", "fake")
		mid := boundSession("conv-mid", root.Name, "")
		leaf := boundSession("conv-leaf", mid.Name, "agent")

		nc := connectNATS(t)
		cli := fakeClientWith(t, root, mid, leaf)
		sndr := &captureSessionInfoSender{}
		startRelay(t, nc, cli, &fixedResolver{s: sndr, subS: sndr})

		publishOut(t, nc, "default", leaf.Name, "user_message",
			buildEnv(t, leaf.Name, channelevents.KindUserMessage,
				channelevents.OutboundUserMessagePayload{Text: "parent, I finished the sweep"}))

		require.True(t, waitUntil(t, 2*time.Second, func() bool {
			_, ok := sndr.first()
			return ok
		}), "an ordinary reply from a channel-attached child must be delivered, not dropped")

		got, ok := sndr.first()
		require.True(t, ok)
		require.NotNil(t, got.Channel)
		assert.Equal(t, leaf.Spec.InputChannel.Name, got.Channel.Name,
			"ordinary output goes to the child's own binding — its parent — never up the lineage")
		assert.Equal(t, "agent", got.Channel.Kind)
	})

	t.Run("no human-readable binding anywhere: the prompt is dropped loudly, never delivered to an agent", func(t *testing.T) {
		root := boundSession("nohuman-root", "", "agent")
		leaf := boundSession("nohuman-leaf", root.Name, "agent")

		nc := connectNATS(t)
		cli := fakeClientWith(t, root, leaf)
		sndr := &captureSessionInfoSender{}
		logger, joined := captureLogSink(t)
		startRelayWithLogger(t, nc, cli, &fixedResolver{subS: sndr}, logger)

		publishOut(t, nc, "default", leaf.Name, "interaction_request",
			interactionRequestEnvelope(t, leaf.Name))

		require.True(t, waitUntil(t, 2*time.Second, func() bool {
			return strings.Contains(joined(), "no human-readable channel binding anywhere in the lineage")
		}), "the drop must be logged with enough context to locate it; expected log line never appeared")

		assert.Empty(t, sndr.all(),
			"an all-agent lineage has nobody to ask; the card must NOT fall back onto an agent surface")
	})

	t.Run("an unregistered channel kind drops as a wiring error, distinctly from having no human", func(t *testing.T) {
		// Fail-closed AND loud: the operator must be sent to the binary's
		// blank imports, not to the session tree.
		leaf := boundSession("unreg-leaf", "", "not-a-registered-kind")

		nc := connectNATS(t)
		cli := fakeClientWith(t, leaf)
		sndr := &captureSessionInfoSender{}
		logger, joined := captureLogSink(t)
		startRelayWithLogger(t, nc, cli, &fixedResolver{subS: sndr}, logger)

		publishOut(t, nc, "default", leaf.Name, "interaction_request",
			interactionRequestEnvelope(t, leaf.Name))

		require.True(t, waitUntil(t, 2*time.Second, func() bool {
			return strings.Contains(joined(), "unknown kind")
		}), "expected the unknown-kind error to be logged")

		assert.Empty(t, sndr.all(), "an unanswerable kind must drop, not deliver")
		assert.NotContains(t, joined(), "no human-readable channel binding anywhere in the lineage",
			"a wiring bug must not be indistinguishable from a lineage with no human in it")
	})
}

// TestRelay_CardDeliveredUpTheLineage_DoesNotWriteBackOntoTheChildsBinding
// covers the consequence of routing a card through someone else's channel: the
// thread root the Sender captures belongs to the ANCESTOR's conversation, and
// patching it onto the child's own binding would point the inbound matcher at a
// thread the child does not live in.
//
// The two write-back branches are live for a conversational child in a way
// they are not for a headless one — a headless child has a nil OutputChannel
// and no forked-from annotation, so both are unreachable there whatever the
// routing does. A bound child arms them, which is why the guard is asserted
// against exactly this shape.
func TestRelay_CardDeliveredUpTheLineage_DoesNotWriteBackOntoTheChildsBinding(t *testing.T) {
	root := boundSession("wb-root", "", "slack")
	leaf := boundSession("wb-leaf", root.Name, "agent")
	// The fork annotation is what arms patchInputChannelThreadTS.
	leaf.Annotations = map[string]string{spiceboxv1alpha1.AnnotationForkedFromThread: "C-OLD:1111.2222"}

	nc := connectNATS(t)
	cli := fakeClientWith(t, root, leaf)
	// The card path captures metadata describing the ROOT's slack thread —
	// which is where it actually landed.
	card := &writeBackSender{external: map[string]string{
		"thread_ts":  "root-thread.9999",
		"channel_id": "C-ROOT",
	}}
	// The ordinary path captures nothing, so the barrier below cannot itself
	// write anything back and confuse the assertion.
	barrier := &captureSessionInfoSender{}
	startRelay(t, nc, cli, &fixedResolver{s: barrier, subS: card})

	publishOut(t, nc, "default", leaf.Name, "interaction_request",
		interactionRequestEnvelope(t, leaf.Name))

	// Barrier: an ordinary reply on the same subject, published second and
	// delivered through the leaf's OWN binding. nats.go serializes one
	// subscription's callbacks on a single goroutine, so its arrival at the
	// sender proves the card above was fully handled — write-back included, had
	// one been attempted.
	publishOut(t, nc, "default", leaf.Name, "user_message",
		buildEnv(t, leaf.Name, channelevents.KindUserMessage,
			channelevents.OutboundUserMessagePayload{Text: "barrier"}))

	require.True(t, waitUntil(t, 3*time.Second, func() bool {
		return len(barrier.all()) >= 1
	}), "barrier reply never reached the sender; the card's handling cannot be assumed complete")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: leaf.Name}, &got))
	require.NotNil(t, got.Spec.InputChannel)
	assert.Empty(t, got.Spec.InputChannel.External["thread_ts"],
		"the root's thread root must not be stamped onto the child's agent Channel")
	assert.Empty(t, got.Labels[spiceboxv1alpha1.LabelChannelKey],
		"nor may the child's channel-key label be rewritten to an ancestor's thread")
}

// TestHumanDirectedKinds_AreTheInteractionFamily pins the membership of the
// set itself. The routing rule is only as good as this list: a kind that a
// person must answer and that is missing here routes to whatever channel the
// session happens to be bound to, which for a conversational child is its
// parent agent.
func TestHumanDirectedKinds_AreTheInteractionFamily(t *testing.T) {
	for _, k := range []channelevents.Kind{
		channelevents.KindInteractionRequest,
		channelevents.KindInteractionApplied,
		channelevents.KindInteractionDecisionRejected,
	} {
		assert.True(t, isHumanDirected(k), "%s is answered by a person and must route to a human channel", k)
	}
	for _, k := range []channelevents.Kind{
		channelevents.KindUserMessage,
		channelevents.KindNotification,
		channelevents.KindAgentMessageSend,
		channelevents.KindPlanUpdate,
		channelevents.KindThreadTitle,
	} {
		assert.False(t, isHumanDirected(k), "%s is ordinary output and must route conversationally", k)
	}
}
