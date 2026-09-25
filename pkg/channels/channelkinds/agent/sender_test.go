package agent_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
)

// publishedCall records one Deps.NATSPublish invocation, so tests can assert
// both what was (or was not) published.
type publishedCall struct {
	subject string
	payload []byte
}

// fakeBus is a fake Deps.NATSPublish: it records every call, or — when err is
// set — fails every call without recording it as delivered.
type fakeBus struct {
	calls []publishedCall
	err   error
}

func (b *fakeBus) publish(subject string, payload []byte) error {
	if b.err != nil {
		return b.err
	}
	b.calls = append(b.calls, publishedCall{subject: subject, payload: payload})
	return nil
}

// senderChannel builds a Channel of kind "agent" with the given
// authzSubject — the only two fields the sender itself reads.
func senderChannel(authzSubject string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "root-out"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:         "agent",
			AuthzSubject: authzSubject,
		},
	}
}

// userMessageEnvelope builds a realistic outbound envelope carrying text, the
// shape the outbound relay hands every Sender for a respond_to_user turn.
func userMessageEnvelope(t *testing.T, ns, name, text string) channelevents.Envelope {
	t.Helper()
	env, err := channelevents.BuildEnvelope(ns, name, channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: text})
	require.NoError(t, err)
	return env
}

func TestSender_MalformedCounterparty_RefusesAndPublishesNothing(t *testing.T) {
	cases := []struct {
		name         string
		authzSubject string
	}{
		{name: "empty authzSubject", authzSubject: ""},
		{name: "no agentsession: prefix", authzSubject: "service:hubspot-digest-bot"},
		{name: "agentsession: with no slash", authzSubject: "agentsession:demo-ns"},
		{name: "agentsession: with empty namespace", authzSubject: "agentsession:/lead-1"},
		{name: "agentsession: with empty name", authzSubject: "agentsession:demo-ns/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bus := &fakeBus{}
			deps := channelkinds.Deps{
				Channel:     senderChannel(tc.authzSubject),
				NATSPublish: bus.publish,
			}
			sender := agent.Kind{}.NewSender(deps)
			require.NotNil(t, sender)

			sess := channelkinds.SessionInfo{Namespace: "demo-ns", Name: "root-1"}
			env := userMessageEnvelope(t, sess.Namespace, sess.Name, "hello")

			_, err := sender.Send(context.Background(), sess, env)
			require.Error(t, err, "a malformed counterparty must be a loud failure")
			assert.Contains(t, err.Error(), "demo-ns/root-out",
				"the error must name the Channel whose authzSubject is malformed")
			assert.Empty(t, bus.calls, "a malformed counterparty must publish nothing")
		})
	}
}

func TestSender_NilChannel(t *testing.T) {
	bus := &fakeBus{}
	deps := channelkinds.Deps{NATSPublish: bus.publish}
	sender := agent.Kind{}.NewSender(deps)
	require.NotNil(t, sender)

	sess := channelkinds.SessionInfo{Namespace: "demo-ns", Name: "root-1"}
	env := userMessageEnvelope(t, sess.Namespace, sess.Name, "hello")

	_, err := sender.Send(context.Background(), sess, env)
	require.Error(t, err)
	assert.Empty(t, bus.calls)
}

func TestSender_UnsupportedEnvelopeKind(t *testing.T) {
	bus := &fakeBus{}
	deps := channelkinds.Deps{
		Channel:     senderChannel("agentsession:demo-ns/lead-1"),
		NATSPublish: bus.publish,
	}
	sender := agent.Kind{}.NewSender(deps)
	require.NotNil(t, sender)

	sess := channelkinds.SessionInfo{Namespace: "demo-ns", Name: "root-1"}
	// KindWidgetOffer is neither KindUserMessage nor one of the deliberately
	// dropped progress kinds — genuinely unsupported, unlike the five below.
	env, err := channelevents.BuildEnvelope(sess.Namespace, sess.Name,
		channelevents.KindWidgetOffer, channelevents.WidgetOfferPayload{ArtifactID: "art-1"})
	require.NoError(t, err)

	_, sendErr := sender.Send(context.Background(), sess, env)
	require.Error(t, sendErr, "the default sub-channel only carries user_message and deliberately-dropped progress kinds")
	assert.Empty(t, bus.calls)
}

// TestSender_ProgressKindsAreDeliberatelyDropped pins the deliberate-drop
// property for the five envelope kinds an agent counterparty has no surface
// to render onto: no error AND nothing published. That is otherwise
// indistinguishable from an accidental no-op, which is why both halves are
// asserted for every kind rather than just "returns nil".
func TestSender_ProgressKindsAreDeliberatelyDropped(t *testing.T) {
	cases := []struct {
		name string
		kind channelevents.Kind
		pl   any
	}{
		{name: "notification", kind: channelevents.KindNotification, pl: channelevents.NotificationPayload{Text: "thinking…"}},
		{name: "turn_progress", kind: channelevents.KindTurnProgress, pl: channelevents.TurnProgressPayload{}},
		{name: "tool_progress", kind: channelevents.KindToolProgress, pl: channelevents.ToolProgressPayload{CallID: "call-1"}},
		{name: "operation_activity", kind: channelevents.KindOperationActivity, pl: channelevents.OperationActivityPayload{}},
		{name: "plan_update", kind: channelevents.KindPlanUpdate, pl: channelevents.PlanUpdatePayload{PlanName: "plan-1", Items: []channelevents.PlanItemRef{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bus := &fakeBus{}
			deps := channelkinds.Deps{
				Channel:     senderChannel("agentsession:demo-ns/lead-1"),
				NATSPublish: bus.publish,
			}
			sender := agent.Kind{}.NewSender(deps)
			require.NotNil(t, sender)

			sess := channelkinds.SessionInfo{Namespace: "demo-ns", Name: "root-1"}
			env, err := channelevents.BuildEnvelope(sess.Namespace, sess.Name, tc.kind, tc.pl)
			require.NoError(t, err)

			_, sendErr := sender.Send(context.Background(), sess, env)
			require.NoError(t, sendErr, "an agent counterparty has no progress surface; this must be a silent, deliberate drop")
			assert.Empty(t, bus.calls, "a dropped progress envelope must publish nothing")
		})
	}
}

func TestSender_SelfReferentialCounterparty_Refuses(t *testing.T) {
	bus := &fakeBus{}
	deps := channelkinds.Deps{
		// The channel's counterparty is the SAME session that will call Send.
		Channel:     senderChannel("agentsession:demo-ns/root-1"),
		NATSPublish: bus.publish,
	}
	sender := agent.Kind{}.NewSender(deps)
	require.NotNil(t, sender)

	sess := channelkinds.SessionInfo{Namespace: "demo-ns", Name: "root-1"}
	env := userMessageEnvelope(t, sess.Namespace, sess.Name, "hello")

	_, err := sender.Send(context.Background(), sess, env)
	require.Error(t, err, "a channel whose counterparty is its own sending session must refuse")
	assert.Contains(t, err.Error(), "demo-ns/root-1")
	assert.Empty(t, bus.calls, "a self-referential counterparty must publish nothing")
}

// TestSender_UserMessageIsRefused replaces the trio that used to drive the
// removed KindUserMessage arm (it published, wrapped its publish error, and
// guarded a nil NATSPublish).
//
// There is no free-form session-to-session message any more. The three live
// directions of a conversational delegation — ask_parent, return_result,
// reply_to_subagent — are each typed and separately authorized, and each
// reaches the other side as an INSPECTED tool result. A user_message would
// land in the other agent's transcript with no content inspection anywhere,
// which is why respondToUserSkip withholds the tool that produces one and why
// this Sender no longer carries it.
func TestSender_UserMessageIsRefused(t *testing.T) {
	bus := &fakeBus{}
	deps := channelkinds.Deps{
		Channel:     senderChannel("agentsession:demo-ns/lead-1"),
		NATSPublish: bus.publish,
	}
	sender := agent.Kind{}.NewSender(deps)
	require.NotNil(t, sender)

	sess := channelkinds.SessionInfo{Namespace: "demo-ns", Name: "root-1"}
	env := userMessageEnvelope(t, sess.Namespace, sess.Name, "please review the draft")

	_, err := sender.Send(context.Background(), sess, env)
	require.Error(t, err, "a free-form message between sessions must be refused, not delivered")
	assert.Contains(t, err.Error(), "user_message")
	assert.Empty(t, bus.calls,
		"and nothing may reach the bus: a refusal that still published would be the untyped channel this removed")
}

// TestSender_RefusesEveryKindItDoesNotDrop is the property, so a future edit
// cannot re-open one envelope kind quietly.
//
// The arm was REMOVED rather than left dormant because unreachable code that
// would reactivate if a withholding rule changed is a latent re-opening:
// relaxing respondToUserSkip later would have brought it live carrying a path
// nobody re-reviewed, with nothing in that diff to say so.
func TestSender_RefusesEveryKindItDoesNotDrop(t *testing.T) {
	bus := &fakeBus{}
	deps := channelkinds.Deps{
		Channel:     senderChannel("agentsession:demo-ns/lead-1"),
		NATSPublish: bus.publish,
	}
	sender := agent.Kind{}.NewSender(deps)
	sess := channelkinds.SessionInfo{Namespace: "demo-ns", Name: "root-1"}

	for _, kind := range []channelevents.Kind{
		channelevents.KindUserMessage,
		channelevents.KindAgentMessageSend,
		channelevents.KindInteractionRequest,
	} {
		env, err := channelevents.BuildEnvelope(sess.Namespace, sess.Name, kind,
			channelevents.OutboundUserMessagePayload{Text: "x"})
		require.NoError(t, err)
		_, err = sender.Send(context.Background(), sess, env)
		assert.Error(t, err, "kind %q must be refused", kind)
	}
	assert.Empty(t, bus.calls, "no refused kind may publish anything")
}
