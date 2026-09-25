package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

func TestKind_Registration(t *testing.T) {
	k, ok := registry.Get("agent")
	require.True(t, ok, "the agent kind must register itself via init()")
	assert.Equal(t, "agent", k.Name())
}

func TestKind_IsRelayedByChannelsd(t *testing.T) {
	// Not client-hosted: delivery is a cluster-side write onto the bus, so
	// channelsd relays it. browser and local return false because their
	// surface lives in a client; this kind has no client.
	assert.True(t, agent.Kind{}.RelayedByChannelsd())
}

func TestKind_NeedsNoSecret(t *testing.T) {
	// The counterparty is in-cluster; there is no third-party credential.
	assert.Empty(t, agent.Kind{}.RequiredSecretKeys(&spiceboxv1alpha1.Channel{}))
}

func TestKind_DoesNotSupportMonitoring(t *testing.T) {
	// SupportsMonitoring and NewMonitoringSender are ONE fact; the interface
	// says any disagreement is a bug in the kind. A session-to-session
	// channel is not a place to deliver cluster-level framework events.
	k := agent.Kind{}
	assert.False(t, k.SupportsMonitoring())
	assert.Nil(t, k.NewMonitoringSender(channelkinds.Deps{}))
}

func TestKind_NoStreamDeltaSink(t *testing.T) {
	// The counterparty consumes whole turns, not token deltas; opting out
	// means the relay silently drops KindAssistantStreamDelta for this kind,
	// which is the intended behavior, not an oversight.
	assert.Nil(t, agent.Kind{}.NewStreamDeltaSink(channelkinds.Deps{}))
}

func TestKind_NotUserAttributable(t *testing.T) {
	// The counterparty is a session, not a human; there is no per-user
	// identity to attribute inbound messages to.
	assert.False(t, agent.Kind{}.UserAttributable())
}

func TestKind_DoesNotDeliverToHuman(t *testing.T) {
	// What reads an agent Channel is another AgentSession's context window.
	// Answering true would let the relay route a permission prompt to the
	// PARENT AGENT, which would then be positioned to answer a question only a
	// human has the standing to answer — so this is a security answer, not a
	// cosmetic one, and it is asserted separately from UserAttributable above
	// even though both are false.
	assert.False(t, agent.Kind{}.DeliversToHuman())
}

func TestKind_DoesNotAllowSyntheticIdentity(t *testing.T) {
	assert.False(t, agent.Kind{}.AllowsSyntheticIdentity())
}

func TestKind_DoesNotSpawnSessionOnInbound(t *testing.T) {
	// An agent Channel is dedicated to one pre-created counterparty session;
	// an inbound with no active session must not spawn a phantom one.
	assert.False(t, agent.Kind{}.SpawnsSessionOnInbound())
}

func TestKind_TrivialMetadata(t *testing.T) {
	k := agent.Kind{}
	assert.Empty(t, k.SupportedMentionLookups(), "agent kind supports no mention lookup")
	assert.Equal(t, "u-1", k.RenderMention("u-1"), "RenderMention returns the bare id")
	assert.Equal(t, "", k.MentionToolDescription())
	assert.Contains(t, k.Capabilities(), "text")
	assert.Nil(t, k.FeatureSupport(), "no permission model")
	assert.Nil(t, k.WebAuthenticator(channelkinds.WebAuthDeps{}), "no human to authenticate")
}

// TestKind_TextFormattingInstructions pins the agent kind's TextFormatter
// answer: plain text, no markup dialect, since the counterparty is another
// session's context rather than a rendering surface. Mirrors how the
// registry-wide sweep (pkg/channels/channelkinds/registry/textformat_test.go)
// checks slack's mrkdwn answer, but for the specific claim this kind makes.
func TestKind_TextFormattingInstructions(t *testing.T) {
	instr := agent.Kind{}.TextFormattingInstructions()
	require.NotEmpty(t, instr)
	assert.Equal(t, strings.TrimSpace(instr), instr, "caller owns the joining whitespace")
	assert.NotEqual(t, channelkinds.DefaultTextFormattingInstructions, instr,
		"agent kind must not degrade to the generic Markdown-is-supported default — "+
			"nothing on this channel interprets Markdown at all")
	assert.Contains(t, instr, "Plain text")
}

func TestKind_LookupUser_Unsupported(t *testing.T) {
	_, _, err := agent.Kind{}.LookupUser(context.Background(), channelkinds.LookupDeps{},
		channelkinds.MentionLookupAny, "anyone")
	assert.ErrorIs(t, err, channelkinds.ErrMentionUnsupported)
}

func TestKind_SubChannelSender_NilForEveryName(t *testing.T) {
	// Every sub-channel is a bespoke rendering for a person, and this kind's
	// counterparty is a session with no rendering surface — so nil, the
	// interface's own documented "this kind doesn't implement that
	// sub-channel" answer, is the permanent answer here rather than a
	// placeholder. Human-directed envelopes never reach this method: the relay
	// resolves them onto an ancestor's human binding first (DeliversToHuman).
	k := agent.Kind{}
	assert.Nil(t, k.SubChannelSender("message", channelkinds.Deps{}))
	assert.Nil(t, k.SubChannelSender("permission_request", channelkinds.Deps{}))
	assert.Nil(t, k.SubChannelSender("unknown", channelkinds.Deps{}))
}

// TestKind_Wizard_Unavailable pins that this kind offers no interactive
// configuration — its Channels are built by the delegation controller, never
// by an operator answering questions.
//
// Every method is asserted, not just the first: a refusal that held only for
// Inputs would still let a client walk on to Handoff or Result and get a
// half-built Channel for a kind that has no such flow.
func TestKind_Wizard_Unavailable(t *testing.T) {
	w := agent.Kind{}.Wizard()
	require.NotNil(t, w, "the interface requires a non-nil Wizard")

	t.Run("Inputs", func(t *testing.T) {
		_, err := w.Inputs(context.Background(), channelkinds.WizardInput{})
		assert.Error(t, err, "agent channels have no interactive wizard")
	})
	t.Run("Handoff", func(t *testing.T) {
		_, err := w.Handoff(context.Background(), channelkinds.WizardInput{})
		assert.Error(t, err, "agent channels have no external round trip either")
	})
	t.Run("Resolve", func(t *testing.T) {
		_, err := w.Resolve(context.Background(), channelkinds.WizardInput{}, map[string]string{})
		assert.Error(t, err, "there are no answers to derive further ones from")
	})
	t.Run("Result", func(t *testing.T) {
		_, err := w.Result(channelkinds.WizardInput{}, map[string]string{})
		assert.Error(t, err, "no answer set may be turned into an agent Channel this way")
	})
}

func TestKind_ValidateSpec(t *testing.T) {
	cases := []struct {
		name    string
		spec    spiceboxv1alpha1.ChannelSpec
		wantErr string
	}{
		{
			// An agent Channel with no counterparty names neither an end to
			// publish to nor an end whose traffic it may carry, so it is not
			// an agent Channel. Nothing else demands the field: the AgentClass
			// controller's userLessChannelMissingAuthz rule skips every kind
			// whose SpawnsSessionOnInbound is false, this kind included.
			name:    "bare agent spec, no counterparty: rejected at validation",
			spec:    spiceboxv1alpha1.ChannelSpec{Kind: "agent"},
			wantErr: `spec.authzSubject`,
		},
		{
			name: "agent + slack block: rejected",
			spec: spiceboxv1alpha1.ChannelSpec{
				Kind: "agent", AuthzSubject: "agentsession:demo-ns/lead-1",
				Slack: &spiceboxv1alpha1.SlackChannelConfig{},
			},
			wantErr: `spec.slack must be empty`,
		},
		{
			name: "agent + fake block: rejected",
			spec: spiceboxv1alpha1.ChannelSpec{
				Kind: "agent", AuthzSubject: "agentsession:demo-ns/lead-1",
				Fake: &spiceboxv1alpha1.FakeChannelConfig{},
			},
			wantErr: `spec.fake must be empty`,
		},
		{
			name: "agent + bento block: rejected",
			spec: spiceboxv1alpha1.ChannelSpec{
				Kind: "agent", AuthzSubject: "agentsession:demo-ns/lead-1",
				Bento: &spiceboxv1alpha1.BentoChannelConfig{},
			},
			wantErr: `spec.bento must be empty`,
		},
		{
			name: "agent + well-formed authzSubject: valid",
			spec: spiceboxv1alpha1.ChannelSpec{Kind: "agent", AuthzSubject: "agentsession:demo-ns/lead-1"},
		},
		{
			name:    "agent + authzSubject with no slash: rejected at validation",
			spec:    spiceboxv1alpha1.ChannelSpec{Kind: "agent", AuthzSubject: "agentsession:demo-ns"},
			wantErr: `spec.authzSubject`,
		},
		{
			name:    "agent + authzSubject with wrong type prefix: rejected at validation",
			spec:    spiceboxv1alpha1.ChannelSpec{Kind: "agent", AuthzSubject: "service:hubspot-digest-bot"},
			wantErr: `spec.authzSubject`,
		},
		{
			// This kind's own parser used to check the prefix and the '/' and
			// nothing else, so what kept an id like this out was the CRD
			// Pattern in channel_types.go — enforcement in a different file
			// that a Channel predating the pattern would not have met. It now
			// shares authz.ParseAgentSessionSubject, which owns the charset.
			name:    "agent + authzSubject whose id carries a colon: rejected here, not only by the CRD pattern",
			spec:    spiceboxv1alpha1.ChannelSpec{Kind: "agent", AuthzSubject: "agentsession:demo-ns/lead:1"},
			wantErr: `spec.authzSubject`,
		},
		{
			name:    "agent + authzSubject naming a subject SET: rejected, a relation is not an end",
			spec:    spiceboxv1alpha1.ChannelSpec{Kind: "agent", AuthzSubject: "agentsession:demo-ns/lead-1#parent"},
			wantErr: `spec.authzSubject`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := agent.Kind{}.ValidateSpec(&spiceboxv1alpha1.Channel{Spec: tc.spec})
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestKind_ValidateSpec_NilChannel(t *testing.T) {
	assert.NoError(t, agent.Kind{}.ValidateSpec(nil))
}

// TestKind_SenderFailsLoudlyOnMissingChannel pins that the real Sender does
// NOT silently drop a message when it's misconstructed: Deps{} carries no
// Channel to resolve a counterparty from, so Send must error rather than
// publish. The Sender's own fuller behavior (message delivery, malformed
// authzSubject, wrapped publish errors) is covered in sender_test.go.
func TestKind_SenderFailsLoudlyOnMissingChannel(t *testing.T) {
	sender := agent.Kind{}.NewSender(channelkinds.Deps{})
	require.NotNil(t, sender, "NewSender must return a non-nil Sender")
	_, err := sender.Send(context.Background(), channelkinds.SessionInfo{}, channelevents.Envelope{})
	assert.Error(t, err)
}

// TestKind_NoopListenerStartsAndStopsCleanly pins that the no-op Listener is
// inert but well-behaved: Start/Stop both succeed because this kind never owns
// a subscription to fail on. That is permanent, not pending — inbound for this
// kind arrives centrally through channelsd; see agent.Kind.NewListener.
func TestKind_NoopListenerStartsAndStopsCleanly(t *testing.T) {
	l := agent.Kind{}.NewListener(channelkinds.Deps{})
	require.NotNil(t, l, "NewListener must return a non-nil Listener")
	assert.NoError(t, l.Start(context.Background()))
	assert.NoError(t, l.Stop(context.Background()))
}

func TestKind_SatisfiesInterface(t *testing.T) {
	var _ channelkinds.Kind = agent.Kind{}
	var _ channelkinds.TextFormatter = agent.Kind{}
}
