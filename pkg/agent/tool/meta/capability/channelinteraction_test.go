package capability

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"k8s.io/utils/clock"
)

func TestChannelInteractionInactiveWithoutChannel(t *testing.T) {
	c, ok := Lookup("channel_interaction")
	require.True(t, ok)
	assert.True(t, c.DefaultOn())
	// active() gates on channel-attach via Binding == nil in Offer:
	tools, skip := c.Offer(OfferContext{Ctx: context.Background(), Binding: nil})
	assert.Nil(t, skip)
	assert.Empty(t, tools, "no channel → no channel tools")
}

func TestChannelInteractionOffersFourTools(t *testing.T) {
	c, _ := Lookup("channel_interaction")
	inbound := make(chan struct{}, 1)
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Binding: &spiceboxv1alpha1.ChannelBinding{Kind: "slack", Capabilities: nil},
		Env: RunnerEnv{
			NATSPublish:   func(context.Context, string, []byte) error { return nil },
			SubjectPrefix: "p",
			InboundCh:     inbound,
			IdleTTL:       time.Minute,
			Clock:         clock.RealClock{},
		},
	})
	assert.Nil(t, skip)
	assert.ElementsMatch(t, []string{"respond_to_user", "await_user_message", "update_status", "set_thread_title"}, toolNames(tools))
}

// channelInteractionOn offers the capability against a binding of the given
// kind, with everything else it needs wired.
func channelInteractionOn(t *testing.T, kind string) ([]tool.Tool, *SkipReason) {
	t.Helper()
	c, ok := Lookup("channel_interaction")
	require.True(t, ok)
	return c.Offer(OfferContext{
		Ctx:     context.Background(),
		Binding: &spiceboxv1alpha1.ChannelBinding{Kind: kind},
		Env: RunnerEnv{
			NATSPublish:   func(context.Context, string, []byte) error { return nil },
			SubjectPrefix: "p",
			InboundCh:     make(chan struct{}, 1),
			IdleTTL:       time.Minute,
			Clock:         clock.RealClock{},
		},
	})
}

// Brief test 7: a session whose channel reaches ANOTHER SESSION is not offered
// respond_to_user. It has two ways to put text into that session's context and
// only ask_parent is inspected, so leaving both wired lets it launder arbitrary
// text past the inspection by choosing the other tool.
//
// The other three tools survive, and that is the assertion that separates a
// narrowing from an outage: withholding one tool must not cost the session the
// ability to wait, to report status, or to name its thread.
func TestChannelInteraction_SessionCounterpartyBinding_WithholdsRespondToUserLoudly(t *testing.T) {
	tools, skip := channelInteractionOn(t, "agent")

	assert.NotContains(t, toolNames(tools), "respond_to_user",
		"an uninspected path into another agent's transcript must not be offered")
	assert.ElementsMatch(t, []string{"await_user_message", "update_status", "set_thread_title"}, toolNames(tools),
		"withholding one tool must not withdraw the rest of the capability")

	require.NotNil(t, skip, "a withheld tool must never be silent")
	assert.Equal(t, "channel_interaction", skip.Capability)
	assert.Contains(t, skip.Reason, "respond_to_user", "the reason must name the tool that was withheld")
	assert.Contains(t, skip.Reason, "no content inspection", "the reason must say WHY, not just that it happened")
	assert.Contains(t, skip.Reason, "ask_parent",
		"the reason must say what remains, so the next reader knows this is not a degradation")
}

// The "what remains" half of that reason has to name a tool the session was
// actually GIVEN. Every session that reaches this skip in tree is a delegated
// child — only the SubagentRequest controller creates an `agent` Channel, and
// it binds one to a child it pre-created with spec.parent set — and
// coreCapability offers such a session return_result IN PLACE OF
// agent_work_complete. Naming the absent door tells an operator reading the
// skip that the session has an escape it cannot reach.
//
// Both messages are covered: the counterparty answer and the fail-closed
// unregistered-kind answer, which is reachable for the same session.
func TestChannelInteraction_DelegatedChild_SkipNamesTheToolItActuallyHas(t *testing.T) {
	cases := []struct {
		name        string
		bindingKind string
	}{
		{"agent binding: names return_result, not agent_work_complete", "agent"},
		{"unregistered binding (fail-closed): names return_result too", "not-a-real-kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := Lookup("channel_interaction")
			require.True(t, ok)
			in := &spiceboxv1alpha1.ChannelBinding{Kind: tc.bindingKind, Name: "in"}
			_, skip := c.Offer(OfferContext{
				Ctx: context.Background(),
				Session: &spiceboxv1alpha1.AgentSession{
					Spec: spiceboxv1alpha1.AgentSessionSpec{
						InputChannel: in,
						// What makes it a delegated child, and the one fact
						// coreCapability keys the terminal-tool swap on.
						Parent: &spiceboxv1alpha1.NamespacedRef{Namespace: "default", Name: "parent-session"},
					},
				},
				Binding: in,
				Env: RunnerEnv{
					NATSPublish:   func(context.Context, string, []byte) error { return nil },
					SubjectPrefix: "p",
					InboundCh:     make(chan struct{}, 1),
					IdleTTL:       time.Minute,
					Clock:         clock.RealClock{},
				},
			})

			require.NotNil(t, skip, "a withheld tool must never be silent")
			assert.Contains(t, skip.Reason, "return_result",
				"the reason must name the terminal tool this child was offered")
			assert.NotContains(t, skip.Reason, "agent_work_complete",
				"a delegated child is offered return_result INSTEAD of agent_work_complete; "+
					"naming it hands the operator a door the session does not have")
		})
	}
}

// The mirror: a session with no parent keeps agent_work_complete in the reason,
// so the fix above did not simply rename the string for everyone.
func TestChannelInteraction_NonChildSkip_StillNamesAgentWorkComplete(t *testing.T) {
	_, skip := channelInteractionOn(t, "agent")

	require.NotNil(t, skip)
	assert.Contains(t, skip.Reason, "agent_work_complete",
		"a session with no spec.parent is offered agent_work_complete, and the reason must say so")
}

// Brief test 8, and the guard that proves test 7 did not withdraw the tool from
// everyone: a session bound to a HUMAN surface still gets respond_to_user, with
// no skip at all.
func TestChannelInteraction_HumanBoundSession_StillGetsRespondToUser(t *testing.T) {
	tools, skip := channelInteractionOn(t, "slack")

	assert.Nil(t, skip, "nothing is withheld from a session whose channel a person reads")
	assert.Contains(t, toolNames(tools), "respond_to_user",
		"withholding is scoped to session-to-session bindings; a human-facing session is untouched")
}

// An unregistered binding kind fails CLOSED. It is a wiring bug — every shipped
// kind is blank-imported by the runner — and treating "cannot resolve" as "not
// a session counterparty" is exactly the direction that re-opens the laundering
// path.
func TestChannelInteraction_UnregisteredBindingKind_WithholdsRespondToUser(t *testing.T) {
	tools, skip := channelInteractionOn(t, "not-a-real-kind")

	assert.NotContains(t, toolNames(tools), "respond_to_user")
	require.NotNil(t, skip)
	assert.Contains(t, skip.Reason, "not-a-real-kind", "the reason must name the kind an operator has to go and register")
}

// channelInteractionSplit offers the capability for a SPLIT-CHANNEL session:
// one kind on spec.inputChannel (which is what the runner passes as Binding)
// and a different one on spec.outputChannel. Every case above has the two
// coincide, which is why asking the wrong one of them read as correct.
func channelInteractionSplit(t *testing.T, inputKind, outputKind string) ([]tool.Tool, *SkipReason) {
	t.Helper()
	c, ok := Lookup("channel_interaction")
	require.True(t, ok)
	in := &spiceboxv1alpha1.ChannelBinding{Kind: inputKind, Name: "in"}
	return c.Offer(OfferContext{
		Ctx: context.Background(),
		Session: &spiceboxv1alpha1.AgentSession{
			Spec: spiceboxv1alpha1.AgentSessionSpec{
				InputChannel:  in,
				OutputChannel: &spiceboxv1alpha1.ChannelBinding{Kind: outputKind, Name: "out"},
			},
		},
		Binding: in,
		Env: RunnerEnv{
			NATSPublish:   func(context.Context, string, []byte) error { return nil },
			SubjectPrefix: "p",
			InboundCh:     make(chan struct{}, 1),
			IdleTTL:       time.Minute,
			Clock:         clock.RealClock{},
		},
	})
}

// The guard has to be asked of the binding respond_to_user is DELIVERED
// through, not the one it is built from. A session whose input is a Slack
// thread but whose output is an `agent` Channel has its KindUserMessage routed
// by v1alpha1.OutboundBinding — which prefers spec.outputChannel — through the
// agent Sender and into the counterparty's transcript, uninspected. Asking the
// input kind alone offers the tool for exactly that session.
func TestChannelInteraction_OutboundBindingReachesASession_WithholdsRespondToUser(t *testing.T) {
	tools, skip := channelInteractionSplit(t, "slack", "agent")

	assert.NotContains(t, toolNames(tools), "respond_to_user",
		"delivery follows the OUTBOUND binding, so a human-looking input binding must not unlock the tool")
	assert.ElementsMatch(t, []string{"await_user_message", "update_status", "set_thread_title"}, toolNames(tools),
		"withholding one tool must not withdraw the rest of the capability")

	require.NotNil(t, skip, "a withheld tool must never be silent")
	assert.Contains(t, skip.Reason, "outbound",
		"the reason must name WHICH binding decided it, or the next reader looks at the wrong field")
	assert.Contains(t, skip.Reason, `"agent"`, "the reason must name the kind that reaches another agent")
}

// The mirror image fails closed too: an `agent` input binding withholds the
// tool even when the output binding reaches a person. Nothing in tree produces
// this shape, and the direction that guesses in favour of offering the tool is
// the one that re-opens the laundering path.
func TestChannelInteraction_InputBindingReachesASession_WithholdsRespondToUserEvenWithAHumanOutput(t *testing.T) {
	tools, skip := channelInteractionSplit(t, "agent", "slack")

	assert.NotContains(t, toolNames(tools), "respond_to_user")
	require.NotNil(t, skip, "a withheld tool must never be silent")
	assert.Contains(t, skip.Reason, "input", "the reason must name WHICH binding decided it")
	assert.Contains(t, skip.Reason, `"agent"`)
}

// And the guard that proves the split rows above did not withdraw the tool from
// every split-channel session: a cron/split session whose BOTH bindings reach
// people keeps respond_to_user, with no skip at all.
func TestChannelInteraction_SplitButBothBindingsHuman_StillGetsRespondToUser(t *testing.T) {
	tools, skip := channelInteractionSplit(t, "local", "slack")

	assert.Nil(t, skip, "nothing is withheld when neither binding reaches a session")
	assert.Contains(t, toolNames(tools), "respond_to_user",
		"a split-channel session that talks to people is untouched by this guard")
}

func TestChannelInteraction_OffersSetThreadTitle(t *testing.T) {
	c, ok := Lookup("channel_interaction")
	require.True(t, ok)
	inbound := make(chan struct{}, 1)
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Binding: &spiceboxv1alpha1.ChannelBinding{Kind: "slack", Capabilities: nil},
		Env: RunnerEnv{
			NATSPublish:   func(context.Context, string, []byte) error { return nil },
			SubjectPrefix: "p",
			InboundCh:     inbound,
			IdleTTL:       time.Minute,
			Clock:         clock.RealClock{},
		},
	})
	require.Nil(t, skip)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name()] = true
	}
	assert.True(t, names["set_thread_title"], "channel-attached session must be offered set_thread_title")
	assert.True(t, names["update_status"], "sanity: still offers update_status")
}
