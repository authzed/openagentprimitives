package capability

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

	// Both kinds the gate must tell apart have to be REGISTERED in this test
	// binary: `agent` declares a session counterparty, `slack` does not. Two
	// genuine kinds rather than a stub is what makes this an end-to-end check
	// of the registry question instead of a restatement of it.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

func askParentEnv() RunnerEnv {
	return RunnerEnv{
		AskParent: func(context.Context, string) (int64, error) { return 1, nil },
		InboundCh: make(chan struct{}, 1),
		IdleTTL:   time.Minute,
	}
}

func delegatedChild(binding *spiceboxv1alpha1.ChannelBinding) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Parent:       &spiceboxv1alpha1.NamespacedRef{Namespace: "ns", Name: "demo-parent"},
			InputChannel: binding,
		},
	}
}

// Brief test 7 (capability half): a single_turn child is headless — no
// binding at all — and is offered no way to ask anything. So is an ordinary
// session that was never delegated to, and so is a delegated child bound to a
// HUMAN surface, where there is no parent on the other end to answer.
func TestSubagentConversation_OfferedOnlyToAChildBoundToItsParent(t *testing.T) {
	c, ok := Lookup("subagent_conversation")
	require.True(t, ok, "the capability must be registered")
	assert.True(t, c.DefaultOn(),
		"a child does not opt in: its PARENT's roster is what granted the conversational mode")

	cases := []struct {
		name    string
		session *spiceboxv1alpha1.AgentSession
		binding *spiceboxv1alpha1.ChannelBinding
		want    []string
	}{
		{
			name:    "a conversational child bound to its parent: ask_parent offered",
			session: delegatedChild(&spiceboxv1alpha1.ChannelBinding{Kind: "agent", Name: "req1-inbox"}),
			binding: &spiceboxv1alpha1.ChannelBinding{Kind: "agent", Name: "req1-inbox"},
			want:    []string{"ask_parent"},
		},
		{
			name:    "a single_turn child, headless: nothing offered",
			session: delegatedChild(nil),
			binding: nil,
			want:    nil,
		},
		{
			name:    "a delegated child bound to a human surface: nothing offered",
			session: delegatedChild(&spiceboxv1alpha1.ChannelBinding{Kind: "slack", Name: "team-chat"}),
			binding: &spiceboxv1alpha1.ChannelBinding{Kind: "slack", Name: "team-chat"},
			want:    nil,
		},
		{
			name: "an ordinary session that nobody delegated to: nothing offered",
			session: &spiceboxv1alpha1.AgentSession{
				Spec: spiceboxv1alpha1.AgentSessionSpec{
					InputChannel: &spiceboxv1alpha1.ChannelBinding{Kind: "agent", Name: "x"},
				},
			},
			binding: &spiceboxv1alpha1.ChannelBinding{Kind: "agent", Name: "x"},
			want:    nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools, skip := c.Offer(OfferContext{
				Ctx:     context.Background(),
				Session: tc.session,
				Binding: tc.binding,
				Env:     askParentEnv(),
			})
			assert.Nil(t, skip, "an inactive capability is a normal state, not a granted-but-unavailable skip")
			assert.ElementsMatch(t, tc.want, toolNames(tools))
		})
	}
}

// An unregistered binding kind is a WIRING bug, not one of the normal inactive
// states above: every shipped kind is blank-imported by the runner, so a kind
// nothing can resolve means a binary is missing one. Nothing is offered — the
// fail-closed direction, since ask_parent on a guess parks the child on an
// answer nobody can send — and the refusal is loud, because a silent one leaves
// a conversational child mute with no line anywhere saying why.
func TestSubagentConversation_UnregisteredBindingKind_OffersNothingAndSaysWhy(t *testing.T) {
	c, ok := Lookup("subagent_conversation")
	require.True(t, ok, "the capability must be registered")

	binding := &spiceboxv1alpha1.ChannelBinding{Kind: "not-a-real-kind"}
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Session: delegatedChild(binding),
		Binding: binding,
		Env:     askParentEnv(),
	})

	assert.Empty(t, toolNames(tools), "a kind that cannot be resolved is not evidence that a parent is reachable")
	require.NotNil(t, skip, "a wiring bug must never be silent")
	assert.Equal(t, "subagent_conversation", skip.Capability)
	assert.Contains(t, skip.Reason, "not-a-real-kind", "the reason must name the kind an operator has to go and register")
}

// A child that CAN converse but has nothing wired to record its question must
// be told, not handed a tool that would park it on a question nobody can see.
func TestSubagentConversation_NoRecorderWired_SkipsLoudly(t *testing.T) {
	c, _ := Lookup("subagent_conversation")
	binding := &spiceboxv1alpha1.ChannelBinding{Kind: "agent", Name: "req1-inbox"}
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Session: delegatedChild(binding),
		Binding: binding,
		Env:     RunnerEnv{}, // no AskParent
	})
	assert.Empty(t, tools)
	require.NotNil(t, skip, "granted but unsatisfiable must be reported, never silent")
	assert.Equal(t, "subagent_conversation", skip.Capability)
	assert.Contains(t, skip.Reason, "cannot be asked anything")
}

// The subagents grant is the DELEGATING side and stays separate: a child is
// under no obligation to hold it, and holding it does not make a session a
// child.
//
// THREE tools, not two, since send_input landed: a child can ask for data as
// well as ask a question, and answering the two is not the same act.
// reply_to_subagent sends WORDS; send_input offers a DATUM by reference, which
// the operator then grades and may route to a person. A parent that could only
// reply would have to paste the data into a message — stripping the
// permissions it carried, which is the laundering the whole by-reference
// design exists to prevent.
func TestSubagents_OffersBothHalvesOfTheParentSide(t *testing.T) {
	c, ok := Lookup("subagents")
	require.True(t, ok)
	tools, skip := c.Offer(OfferContext{
		Ctx: context.Background(),
		Class: &spiceboxv1alpha1.AgentClass{
			Spec: spiceboxv1alpha1.AgentClassSpec{Subagents: []string{"demo-coder"}},
		},
		Session: &spiceboxv1alpha1.AgentSession{},
		Env: RunnerEnv{
			SubagentCreate: func(context.Context, *spiceboxv1alpha1.SubagentRequest) error { return nil },
			SubagentPoll: func(context.Context, string) (*spiceboxv1alpha1.SubagentRequest, error) {
				return nil, nil
			},
			SubagentSend: func(context.Context, string, string, string) error { return nil },
		},
	})
	assert.Nil(t, skip)
	assert.ElementsMatch(t, []string{"delegate", "reply_to_subagent", "send_input"}, toolNames(tools),
		"a parent needs all three: one to open a delegation, one to answer a child's question, "+
			"and one to answer its request for data")
}
