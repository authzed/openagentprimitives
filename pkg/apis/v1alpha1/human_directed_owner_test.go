package v1alpha1_test

// The walk resolves a card about a delegated child onto an ANCESTOR's binding.
// These pin the half that was being thrown away: WHICH ancestor.
//
// It matters because a client-hosted host (the `local` TUI, webd's chat
// registry) decides what it may render by asking whether it serves the session
// it was handed. Handed the child's identity it refuses a card it is the right
// reader for — the person it serves owns that whole tree — so the card is
// dropped, nobody is asked, and the child parks until its approval times out.
// A containment mechanism that parks a session because it could not find a
// person is worse than one that asks.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestTheOwnerIsTheSessionTheBindingWasFoundOn, at every depth, because the
// whole point is that it is NOT the session asked about.
func TestTheOwnerIsTheSessionTheBindingWasFoundOn(t *testing.T) {
	root := sess(t, "root", "", "fake")
	mid := sess(t, "mid", "root", "")
	leaf := sess(t, "leaf", "mid", "")
	c := newReader(t, root, mid, leaf).Build()

	for _, tc := range []struct {
		name  string
		from  *v1.AgentSession
		owner string
	}{
		{"a root resolves onto itself", root, "root"},
		{"a child resolves onto its bound root", mid, "root"},
		{"a grandchild resolves onto the same root, two hops up", leaf, "root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := v1.ResolveHumanDirectedBinding(context.Background(), c, tc.from, deliversToHuman)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.owner, got.Owner.Name,
				"the host is asked about the session that OWNS the binding, not the one the card is about")
			assert.Equal(t, "root-channel", got.Binding.Name)
		})
	}
}

// TestTheChainRecordsEveryHopToTheOwner.
//
// An approver needs to know HOW the asking session relates to the one they are
// watching — "a grandchild of the session you are in" is material to the
// decision. The walk already visits every hop; this asserts it keeps them,
// asker first and owner last, so len-1 is the hop count a renderer keys on.
func TestTheChainRecordsEveryHopToTheOwner(t *testing.T) {
	root := sess(t, "root", "", "fake")
	mid := sess(t, "mid", "root", "")
	leaf := sess(t, "leaf", "mid", "")
	c := newReader(t, root, mid, leaf).Build()

	got, err := v1.ResolveHumanDirectedBinding(context.Background(), c, leaf, deliversToHuman)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, []v1.NamespacedRef{
		{Namespace: "default", Name: "leaf"},
		{Namespace: "default", Name: "mid"},
		{Namespace: "default", Name: "root"},
	}, got.Chain, "asker first, owner last, every hop between")
}

// TestARootsChainIsJustItself: a session delivering through its OWN binding has
// no relationship to explain, and len-1 == 0 is what tells a renderer to stay
// silent rather than announce a delegation that did not happen.
func TestARootsChainIsJustItself(t *testing.T) {
	root := sess(t, "root", "", "fake")
	c := newReader(t, root).Build()

	got, err := v1.ResolveHumanDirectedBinding(context.Background(), c, root, deliversToHuman)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []v1.NamespacedRef{{Namespace: "default", Name: "root"}}, got.Chain)
}

// TestTheOwnerSkipsPastAnAgentBindingWithTheBindingItself.
//
// The conversational-subagent shape: the child IS bound, to an `agent` Channel
// whose far side is its parent session. The binding half already climbs past
// it; the owner must climb with it. An owner naming the agent-bound child while
// the binding named the root would be worse than either being wrong alone — the
// host would be asked about a session it does not serve, using a surface it
// does.
func TestTheOwnerSkipsPastAnAgentBindingWithTheBindingItself(t *testing.T) {
	root := sess(t, "root", "", "slack")
	mid := sess(t, "mid", "root", "")
	leaf := sess(t, "leaf", "mid", "agent")
	c := newReader(t, root, mid, leaf).Build()

	got, err := v1.ResolveHumanDirectedBinding(context.Background(), c, leaf, deliversToHuman)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, "slack", got.Binding.Kind)
	assert.Equal(t, "root", got.Owner.Name,
		"binding and owner must come from the SAME session; they are read together and must not describe different ones")
}

// TestNobodyToAskReportsNoOwnerEither.
//
// Every binding in the lineage leads to another agent, so there is nobody to
// ask. The nil result must stay nil rather than becoming a zero-valued target:
// an Owner of "" would read as a session to a caller that only nil-checks, and
// the relay uses that value to decide who to hand the card to.
func TestNobodyToAskReportsNoOwnerEither(t *testing.T) {
	root := sess(t, "root", "", "agent")
	leaf := sess(t, "leaf", "root", "agent")
	c := newReader(t, root, leaf).Build()

	got, err := v1.ResolveHumanDirectedBinding(context.Background(), c, leaf, deliversToHuman)
	require.NoError(t, err, "nobody to ask is not an error")
	assert.Nil(t, got,
		"a lineage of agent-only bindings must report nothing, never a zero-valued target a caller would route on")
}

// TestTheResolvedBindingIsAlwaysHumanRead is the guarantee that makes widening
// the host gate safe, asserted rather than inherited.
//
// The relay stops consulting its pre-Get Accept filter for human-directed
// envelopes, so a card can now reach a host that does not serve the session it
// is about. What keeps that from landing a person's decision on an agent
// surface is this and only this: the walk's !human arm keeps climbing and never
// records. If a future edit ever let it record an agent binding, the widened
// gate would deliver a human's decision to a machine — so the property gets its
// own test rather than being read off the code.
func TestTheResolvedBindingIsAlwaysHumanRead(t *testing.T) {
	root := sess(t, "root", "", "slack")
	agentMid := sess(t, "mid", "root", "agent")
	leaf := sess(t, "leaf", "mid", "agent")
	c := newReader(t, root, agentMid, leaf).Build()

	got, err := v1.ResolveHumanDirectedBinding(context.Background(), c, leaf, deliversToHuman)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Len(t, got.Chain, 3,
		"a hop whose binding was SKIPPED is still a hop; dropping it would report a grandchild as a subagent")

	human, err := deliversToHuman(got.Binding.Kind)
	require.NoError(t, err)
	assert.True(t, human,
		"the walk may only ever return a binding a PERSON reads — the host gate now trusts this")
}
