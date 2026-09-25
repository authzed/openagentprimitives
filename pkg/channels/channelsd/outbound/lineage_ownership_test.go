package outbound

// A human-directed card about a delegated child has to reach the person who
// owns the tree. Two things used to stop that on a CLIENT-HOSTED host (the
// `local` TUI, webd's chat registry), and they had to be fixed together:
//
//   - Accept, the relay's pre-Get scope, is asked about the ENVELOPE's session.
//     A host answers it from process-local state about the sessions it serves,
//     so a descendant is unknown and the card died before the lineage walk ran.
//   - The interaction dispatch then handed the resolver the resolved ancestor's
//     BINDING but the child's IDENTITY, and a host gates on identity — so it
//     refused a card it was the right reader for.
//
// The symptom was a primary path degrading from "the human is asked" to "loud
// drop, nobody is asked, and the session parks until its approval times out".

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// TestTheResolverIsAskedAboutTheBindingsOWNER is the post-Get half.
//
// The card stays ABOUT the leaf — that is what the approver needs to see — but
// the session handed to the resolver must be the ancestor whose surface it is
// going out through, because that is the session a host recognizes.
func TestTheResolverIsAskedAboutTheBindingsOWNER(t *testing.T) {
	root := boundSession("own-root", "", "fake")
	mid := boundSession("own-mid", root.Name, "")
	leaf := boundSession("own-leaf", mid.Name, "agent")

	nc := connectNATS(t)
	cli := fakeClientWith(t, root, mid, leaf)
	sndr := &captureSessionInfoSender{}
	res := &fixedResolver{subS: sndr}
	startRelay(t, nc, cli, res)

	publishOut(t, nc, "default", leaf.Name, "interaction_request",
		interactionRequestEnvelope(t, leaf.Name))

	require.True(t, waitUntil(t, 2*time.Second, func() bool {
		_, ok := res.lastSubChannelRef()
		return ok
	}), "SubChannelSenderFor was never called")

	ref, _ := res.lastSubChannelRef()
	assert.Equal(t, root.Name, ref.Name,
		"the resolver must be asked about the session that OWNS the binding; asked about the child, "+
			"a client-hosted host refuses a card it is the right reader for")
	assert.Equal(t, "default", ref.Namespace)

	// The binding and the identity must describe the SAME session. Either one
	// alone being right is the defect in a different disguise.
	binding := res.lastSubChannelBinding()
	require.NotNil(t, binding)
	assert.Equal(t, root.Spec.InputChannel.Name, binding.Name,
		"binding and identity must come from one session, not two")

	// And the card is still about the leaf where the human reads it.
	got, ok := sndr.first()
	require.True(t, ok)
	assert.Equal(t, leaf.Name, got.Name,
		"only delivery moved; the approver must still see WHICH session is asking")
}

// TestAcceptDoesNotDropACardForADescendant is the pre-Get half, and it is the
// one that makes the other reachable at all.
//
// The Accept predicate here serves ONLY the root — exactly what a `local` TUI
// host or webd's chat registry answers. A card about the leaf must still get
// through, because the person that host serves owns the leaf's tree.
func TestAcceptDoesNotDropACardForADescendant(t *testing.T) {
	root := boundSession("acc-root", "", "fake")
	leaf := boundSession("acc-leaf", root.Name, "agent")

	nc := connectNATS(t)
	cli := fakeClientWith(t, root, leaf)
	sndr := &captureSessionInfoSender{}
	res := &fixedResolver{subS: sndr}

	// A host that serves exactly one session, and it is not the leaf's.
	startRelay(t, nc, cli, res, withAccept(func(ns, name string) bool {
		return ns == "default" && name == root.Name
	}))

	publishOut(t, nc, "default", leaf.Name, "interaction_request",
		interactionRequestEnvelope(t, leaf.Name))

	require.True(t, waitUntil(t, 2*time.Second, func() bool {
		_, ok := sndr.first()
		return ok
	}), "the card was dropped by the pre-Get Accept filter; the person who owns this tree was never asked")

	ref, ok := res.lastSubChannelRef()
	require.True(t, ok)
	assert.Equal(t, root.Name, ref.Name)
}

// TestAcceptStillDropsOrdinaryTrafficForAForeignSession.
//
// Accept exists to spare a round-trip on the single serialized callback
// goroutine, in front of the hot path — stream deltas, one per token. Widening
// it for CARDS must not widen it for everything, or every client-hosted host
// pays a Get per delta for every session in the cluster.
func TestAcceptStillDropsOrdinaryTrafficForAForeignSession(t *testing.T) {
	root := boundSession("nar-root", "", "fake")
	other := boundSession("nar-other", "", "fake")

	nc := connectNATS(t)
	cli := fakeClientWith(t, root, other)
	sndr := &captureSessionInfoSender{}
	res := &fixedResolver{s: sndr, subS: sndr}

	var mu sync.Mutex
	var asked []spiceboxv1alpha1.NamespacedRef
	startRelay(t, nc, cli, res, withAccept(func(ns, name string) bool {
		mu.Lock()
		asked = append(asked, spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: name})
		mu.Unlock()
		return ns == "default" && name == root.Name
	}))

	publishOut(t, nc, "default", other.Name, "user_message",
		buildEnv(t, other.Name, channelevents.KindUserMessage,
			channelevents.OutboundUserMessagePayload{Text: "hello"}))

	require.True(t, waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(asked) > 0
	}), "Accept was never consulted for an ordinary envelope")

	_, delivered := sndr.first()
	assert.False(t, delivered,
		"an ordinary envelope for a session this consumer does not serve must still be dropped pre-Get")
}

// withAccept sets the relay's pre-Get scope predicate, mirroring what a
// client-hosted host wires (local.Host.Accepts, chat.Registry.acceptsSession).
func withAccept(fn func(ns, name string) bool) func(*Relay) {
	return func(r *Relay) { r.Accept = fn }
}
