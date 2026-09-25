package capability

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

	// Three kinds this file needs registered: "agent" declares a session
	// counterparty (the parent-facing shape task/chat share), "local" and
	// "slack" both do not (two DIFFERENT human kinds, so a test can tell "the
	// child's own binding" apart from "the root's binding" by which kind
	// respond_to_user's description names).
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"k8s.io/utils/clock"
)

// attendedTestEnv is the RunnerEnv both capabilities under test need: a
// recorder for ask_parent, and the channel_interaction plumbing
// (NATSPublish/SubjectPrefix/await wiring) that respond_to_user and its
// siblings are built from. Mirrors askParentEnv/channelInteractionOn's own
// fixtures elsewhere in this package.
func attendedTestEnv() RunnerEnv {
	return RunnerEnv{
		AskParent:     func(context.Context, string) (int64, error) { return 1, nil },
		NATSPublish:   func(context.Context, string, []byte) error { return nil },
		SubjectPrefix: "p",
		InboundCh:     make(chan struct{}, 1),
		IdleTTL:       time.Minute,
		Clock:         clock.RealClock{},
	}
}

// rootHumanBinding is the ROOT session's own output binding -- a "slack"
// channel a person reads, carrying an attachment capability the child's own
// binding below deliberately does not.
func rootHumanBinding() *spiceboxv1alpha1.ChannelBinding {
	return &spiceboxv1alpha1.ChannelBinding{
		Kind: "slack", Name: "root-thread", Key: "thread:root",
		Capabilities: []string{"text", "markdown", "asset:text/html"},
	}
}

// childOwnHumanBinding is a binding of a DIFFERENT human kind than the root's
// (so a test can tell them apart), with no capabilities of its own -- proof
// that a tool built from it, rather than from root, would look different.
func childOwnHumanBinding() *spiceboxv1alpha1.ChannelBinding {
	return &spiceboxv1alpha1.ChannelBinding{Kind: "local", Name: "child-own", Key: "local:child"}
}

// TestAttendedChild_GetsRootBoundRespondToUser_NotAskParent is Task 4 of the
// agent-builder delegation framework: an attended child's respond_to_user
// must bind to the ROOT session's output binding -- its reply goes out to the
// HUMAN the root talks to, not into a channel of the child's own -- and it
// must never ALSO carry ask_parent, the parent-only agent-channel tool.
// Wiring both would let the same words reach the parent's transcript through
// a path content inspection never sees at all (channelinteraction.go's
// respondToUserSkip SECURITY note): ask_parent is inspected as an untrusted
// tool result, respond_to_user's inbound landing in another session's
// transcript is not.
func TestAttendedChild_GetsRootBoundRespondToUser_NotAskParent(t *testing.T) {
	subagentConv, ok := Lookup("subagent_conversation")
	require.True(t, ok, "the capability must be registered")
	channelInt, ok := Lookup("channel_interaction")
	require.True(t, ok, "the capability must be registered")

	t.Run("OutputChannel set to root's binding: respond_to_user wired to root, no ask_parent", func(t *testing.T) {
		root := rootHumanBinding()
		own := childOwnHumanBinding()
		sess := &spiceboxv1alpha1.AgentSession{
			Spec: spiceboxv1alpha1.AgentSessionSpec{
				Parent:        &spiceboxv1alpha1.NamespacedRef{Namespace: "ns", Name: "demo-root"},
				InputChannel:  own,
				OutputChannel: root,
			},
		}
		octx := OfferContext{
			Ctx:     context.Background(),
			Session: sess,
			Binding: own,
			// What assemble.go's outBindingFor derives once Spec.OutputChannel is
			// set: the ROOT's binding, not the child's own.
			OutBinding: root,
			Env:        attendedTestEnv(),
		}

		convTools, convSkip := subagentConv.Offer(octx)
		intTools, intSkip := channelInt.Offer(octx)
		assert.Nil(t, convSkip, "an attended child's ask_parent absence is a normal inactive state")
		assert.Nil(t, intSkip, "nothing is withheld when both bindings reach a person")

		all := append(append([]tool.Tool{}, convTools...), intTools...)
		names := toolNames(all)
		assert.NotContains(t, names, "ask_parent",
			"an attended child's counterparty is the human on the root's channel, not its parent")
		require.Contains(t, names, "respond_to_user",
			"an attended child must be able to speak to the human on the root's channel")

		respond := toolNamed(t, all, "respond_to_user")
		assert.Contains(t, respond.Description(), "slack",
			"respond_to_user must be shaped by the ROOT's binding (slack), not the child's own (local)")
		assert.NotContains(t, respond.Description(), "local",
			"and must NOT describe the child's own binding")
	})

	t.Run("OutputChannel set even though InputChannel still resolves to the parent: ask_parent still withheld", func(t *testing.T) {
		// Defense in depth: even in the shape task/chat use for their own
		// InputChannel (an "agent" kind reaching the delegating session, which
		// makes IsDelegatedChild true), a non-nil OutputChannel marks this
		// child attended and ask_parent must stay withheld.
		root := rootHumanBinding()
		parentFacing := &spiceboxv1alpha1.ChannelBinding{Kind: "agent", Name: "req1-inbox"}
		sess := &spiceboxv1alpha1.AgentSession{
			Spec: spiceboxv1alpha1.AgentSessionSpec{
				Parent:        &spiceboxv1alpha1.NamespacedRef{Namespace: "ns", Name: "demo-root"},
				InputChannel:  parentFacing,
				OutputChannel: root,
			},
		}
		tools, skip := subagentConv.Offer(OfferContext{
			Ctx:     context.Background(),
			Session: sess,
			Binding: parentFacing,
			Env:     attendedTestEnv(),
		})
		assert.Nil(t, skip, "withheld as a normal inactive state, not a granted-but-unavailable skip")
		assert.NotContains(t, toolNames(tools), "ask_parent",
			"spec.OutputChannel marks this child attended even when its InputChannel still resolves to the parent's session")
	})
}

// TestChatChild_Unchanged_AskParentNotRespondToUser is the refusing
// direction and the invariant itself: a chat child -- no OutputChannel, its
// InputChannel resolves to the parent -- still gets ask_parent and never
// respond_to_user. Both assertions in one test, on one fixture, is what
// proves the per-mode split added for attended did not regress chat: the two
// tools stay mutually exclusive for a session-counterparty channel exactly as
// before.
func TestChatChild_Unchanged_AskParentNotRespondToUser(t *testing.T) {
	subagentConv, ok := Lookup("subagent_conversation")
	require.True(t, ok, "the capability must be registered")
	channelInt, ok := Lookup("channel_interaction")
	require.True(t, ok, "the capability must be registered")

	parentFacing := &spiceboxv1alpha1.ChannelBinding{Kind: "agent", Name: "req1-inbox"}
	sess := &spiceboxv1alpha1.AgentSession{
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Parent:       &spiceboxv1alpha1.NamespacedRef{Namespace: "ns", Name: "demo-parent"},
			InputChannel: parentFacing,
			// No OutputChannel: task and chat share ONE Channel for both
			// directions (see channelinteraction.go's respondToUserSkip doc).
		},
	}
	octx := OfferContext{
		Ctx:     context.Background(),
		Session: sess,
		Binding: parentFacing,
		Env:     attendedTestEnv(),
	}

	convTools, convSkip := subagentConv.Offer(octx)
	intTools, intSkip := channelInt.Offer(octx)
	assert.Nil(t, convSkip, "a chat child's ask_parent is unconditionally offered")
	require.NotNil(t, intSkip, "a withheld respond_to_user must never be silent")

	all := append(append([]tool.Tool{}, convTools...), intTools...)
	names := toolNames(all)
	assert.Contains(t, names, "ask_parent", "a chat child still talks to its parent")
	assert.NotContains(t, names, "respond_to_user",
		"a chat child's reply must not launder past inspection into the parent's transcript")
}
