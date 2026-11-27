package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalactor"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// inboundTurn is an "inbox" item as channelsd writes it: a human author and
// the per-inbound ref its goal-actor attestation also carries.
func inboundTurn(idx int, author identity.Subject, ref string) memory.Turn {
	t := drainTextTurn(idx, "inbox", "message")
	t.Author = author
	t.InboundRef = ref
	return t
}

func TestTurnRefFollowsTheTurnsOwnAuthor(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	alice, bob := identity.Subject("user:YWxpY2U"), identity.Subject("user:Ym9i")

	t.Run("a mid-turn inbound does not move the ref off the turn's originator", func(t *testing.T) {
		l := newDrainLoop(t)
		l.startTurnOnNextDrain()
		require.NoError(t, l.Memory.Append(ctx, inboundTurn(1, alice, "ref-a")))
		_, next, err := l.drainInbox(ctx, 1)
		require.NoError(t, err)
		assert.Equal(t, "ref-a", l.TurnRef())

		require.NoError(t, l.Memory.Append(ctx, inboundTurn(next+1, bob, "ref-b")))
		_, _, err = l.drainInbox(ctx, next+2)
		require.NoError(t, err)
		assert.Equal(t, "ref-a", l.TurnRef(), "bob replied mid-turn; the turn is still alice's")
	})

	// drainOnce appends the given inbox items after the current transcript
	// and drains them as one batch.
	drainOnce := func(t *testing.T, l *Loop, items ...memory.Turn) {
		t.Helper()
		all, err := l.Memory.ReadAll(ctx)
		require.NoError(t, err)
		next := 0
		for _, tn := range all {
			if tn.Index >= next {
				next = tn.Index + 1
			}
		}
		for i, it := range items {
			it.Index = next + i
			require.NoError(t, l.Memory.Append(ctx, it))
		}
		_, _, err = l.drainInbox(ctx, next+len(items))
		require.NoError(t, err)
	}
	opened := func(t *testing.T) *Loop {
		t.Helper()
		l := newDrainLoop(t)
		l.startTurnOnNextDrain()
		drainOnce(t, l, inboundTurn(0, alice, "ref-a1"))
		require.Equal(t, "ref-a1", l.TurnRef())
		return l
	}

	t.Run("the opener replying at an await advances the ref to the reply", func(t *testing.T) {
		l := opened(t)
		drainOnce(t, l, inboundTurn(0, alice, "ref-a2"))
		assert.Equal(t, "ref-a2", l.TurnRef())
	})

	t.Run("someone else replying at an await leaves the ref unchanged", func(t *testing.T) {
		l := opened(t)
		drainOnce(t, l, inboundTurn(0, bob, "ref-b"))
		assert.Equal(t, "ref-a1", l.TurnRef())
	})

	t.Run("a mid-turn drain mixing the opener and someone else leaves the ref unchanged", func(t *testing.T) {
		l := opened(t)
		drainOnce(t, l, inboundTurn(0, alice, "ref-a2"), inboundTurn(0, bob, "ref-b"))
		assert.Equal(t, "ref-a1", l.TurnRef())
	})

	t.Run("after an interjection the opener speaking again takes their latest ref", func(t *testing.T) {
		l := opened(t)
		drainOnce(t, l, inboundTurn(0, bob, "ref-b"))
		drainOnce(t, l, inboundTurn(0, alice, "ref-a2"))
		assert.Equal(t, "ref-a2", l.TurnRef(), "the service then accepts it: bob's attestation is earlier")
	})

	t.Run("a new turn started by B takes B's ref", func(t *testing.T) {
		l := newDrainLoop(t)
		l.startTurnOnNextDrain()
		require.NoError(t, l.Memory.Append(ctx, inboundTurn(1, alice, "ref-a")))
		_, next, err := l.drainInbox(ctx, 1)
		require.NoError(t, err)

		l.startTurnOnNextDrain()
		require.NoError(t, l.Memory.Append(ctx, inboundTurn(next+1, bob, "ref-b")))
		_, _, err = l.drainInbox(ctx, next+2)
		require.NoError(t, err)
		assert.Equal(t, "ref-b", l.TurnRef())
	})

	t.Run("a batch drained at turn start is pinned to its first human inbound", func(t *testing.T) {
		l := newDrainLoop(t)
		l.startTurnOnNextDrain()
		notice := drainTextTurn(1, "inbox", "operator notice")
		require.NoError(t, l.Memory.Append(ctx, notice))
		require.NoError(t, l.Memory.Append(ctx, inboundTurn(2, alice, "ref-a")))
		require.NoError(t, l.Memory.Append(ctx, inboundTurn(3, bob, "ref-b")))
		_, _, err := l.drainInbox(ctx, 4)
		require.NoError(t, err)
		assert.Equal(t, "ref-a", l.TurnRef(), "an authorless notice names nobody; the first human does")
	})

	t.Run("a human inbound with no ref leaves the turn unattributable", func(t *testing.T) {
		l := newDrainLoop(t)
		l.startTurnOnNextDrain()
		require.NoError(t, l.Memory.Append(ctx, inboundTurn(1, alice, "ref-a")))
		_, next, err := l.drainInbox(ctx, 1)
		require.NoError(t, err)

		l.startTurnOnNextDrain()
		require.NoError(t, l.Memory.Append(ctx, inboundTurn(next+1, bob, "")))
		_, _, err = l.drainInbox(ctx, next+2)
		require.NoError(t, err)
		assert.Empty(t, l.TurnRef(), "never fall back to an earlier turn's ref")
	})

	t.Run("the cold-start turn is named by the initial-prompt ref", func(t *testing.T) {
		l := newDrainLoop(t)
		l.startInitialPromptTurn()
		assert.Equal(t, goalactor.InitialPromptRef, l.TurnRef())
		// The opening message's attachment item drains inside turn 0.
		require.NoError(t, l.Memory.Append(ctx, inboundTurn(1, alice, goalactor.InitialPromptRef)))
		require.NoError(t, l.Memory.Append(ctx, inboundTurn(2, bob, "ref-b")))
		_, _, err := l.drainInbox(ctx, 1)
		require.NoError(t, err)
		assert.Equal(t, goalactor.InitialPromptRef, l.TurnRef())
	})
}
