package outbound

// InteractionRequestPayload.AgentSessionRef is publisher-controlled JSON its
// own doc calls "a denormalized copy for renderer convenience, not validated
// here" — and nothing validated it. Envelope.Session is publisher-controlled
// too, but the relay drops any envelope whose Session disagrees with the
// subject the publisher's per-session JWT authorizes, so past that check it is
// a fact.
//
// Renderers read the payload copy to say WHO IS ASKING, and a card about a
// delegated child reaches a person who is not watching that session. So the
// checked value has to be written into the copy.

import (
	"encoding/json"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func requestWithAsker(t *testing.T, subjectSession, claimedAsker string) channelevents.Envelope {
	t.Helper()
	pl := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: claimedAsker},
		Category:        "tool_approval",
		RequestRef:      "req-1",
		Lead:            "Approve?",
	}
	raw, err := json.Marshal(pl)
	require.NoError(t, err)
	return channelevents.Envelope{
		Version: 1,
		Kind:    channelevents.KindInteractionRequest,
		Session: channelevents.SessionRef{Namespace: "default", Name: subjectSession},
		Payload: raw,
	}
}

func askerOf(t *testing.T, env channelevents.Envelope) channelevents.SessionRef {
	t.Helper()
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	return pl.AgentSessionRef
}

// TestAForgedAskingSessionIsCorrected is the point.
//
// The dangerous direction is SUPPRESSION. A client-hosted host shows the
// attribution only when the asking session differs from the one on screen, so
// a runner writing the WATCHED session's name here would make its own card
// look like it came from the session the person is already looking at.
func TestAForgedAskingSessionIsCorrected(t *testing.T) {
	env := requestWithAsker(t, "worker-7", "root-session")

	out := stampAskingSession(env, logr.Discard())

	assert.Equal(t, channelevents.SessionRef{Namespace: "default", Name: "worker-7"}, askerOf(t, out),
		"the subject-checked identity must replace the publisher's claim")
}

// TestAnHonestPayloadIsLeftAlone — byte-identical, so this cannot be satisfied
// by rewriting everything unconditionally and calling it correct.
func TestAnHonestPayloadIsLeftAlone(t *testing.T) {
	env := requestWithAsker(t, "worker-7", "worker-7")
	out := stampAskingSession(env, logr.Discard())
	assert.Equal(t, string(env.Payload), string(out.Payload),
		"a payload that already agrees with its subject must not be rewritten")
}

// TestAnEmptyAskerIsFilledIn: the common case is a publisher that simply did
// not set it. It still gets the checked value, so a renderer always has one.
func TestAnEmptyAskerIsFilledIn(t *testing.T) {
	env := requestWithAsker(t, "worker-7", "")
	out := stampAskingSession(env, logr.Discard())
	assert.Equal(t, "worker-7", askerOf(t, out).Name)
}

// TestOnlyInteractionRequestsAreTouched: every other kind decodes into a
// different payload type, and re-encoding one as an interaction request would
// destroy it.
func TestOnlyInteractionRequestsAreTouched(t *testing.T) {
	env := requestWithAsker(t, "worker-7", "root-session")
	env.Kind = channelevents.KindUserMessage

	out := stampAskingSession(env, logr.Discard())
	assert.Equal(t, string(env.Payload), string(out.Payload),
		"a non-interaction envelope must pass through untouched")
}

// TestAnUndecodableInteractionPayloadIsPassedThrough rather than dropped: the
// envelope is still deliverable, and a renderer that cannot read it will say
// so. Failing the whole card because attribution could not be corrected would
// be a worse outcome than an uncorrected card.
func TestAnUndecodableInteractionPayloadIsPassedThrough(t *testing.T) {
	env := requestWithAsker(t, "worker-7", "root-session")
	env.Payload = []byte("{not json")

	out := stampAskingSession(env, logr.Discard())
	assert.Equal(t, string(env.Payload), string(out.Payload))
}

func chainOf(t *testing.T, env channelevents.Envelope) []channelevents.SessionRef {
	t.Helper()
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	return pl.AskingChain
}

// TestTheChainIsStampedFromTheWalk. len-1 is the hop count a renderer turns
// into "a grandchild of X".
func TestTheChainIsStampedFromTheWalk(t *testing.T) {
	env := requestWithAsker(t, "worker-7", "worker-7")
	walked := []spiceboxv1alpha1.NamespacedRef{
		{Namespace: "default", Name: "worker-7"},
		{Namespace: "default", Name: "mid"},
		{Namespace: "default", Name: "root-session"},
	}

	out := stampAskingChain(env, walked, logr.Discard())

	assert.Equal(t, []channelevents.SessionRef{
		{Namespace: "default", Name: "worker-7"},
		{Namespace: "default", Name: "mid"},
		{Namespace: "default", Name: "root-session"},
	}, chainOf(t, out))
}

// TestAForgedChainIsOverwritten is the trust pin, and it is the same attack as
// the asker one a level up: a chain the asking agent could write would let it
// claim to be a child of a session the approver trusts.
func TestAForgedChainIsOverwritten(t *testing.T) {
	var pl channelevents.InteractionRequestPayload
	env := requestWithAsker(t, "worker-7", "worker-7")
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	pl.AskingChain = []channelevents.SessionRef{
		{Namespace: "default", Name: "worker-7"},
		{Namespace: "default", Name: "a-session-you-trust"},
	}
	raw, err := json.Marshal(pl)
	require.NoError(t, err)
	env.Payload = raw

	out := stampAskingChain(env, []spiceboxv1alpha1.NamespacedRef{
		{Namespace: "default", Name: "worker-7"},
		{Namespace: "default", Name: "root-session"},
	}, logr.Discard())

	got := chainOf(t, out)
	require.Len(t, got, 2)
	assert.Equal(t, "root-session", got[1].Name,
		"the walked path must replace the publisher's claim outright")
}

// TestAOneHopChainCLEARSTheField rather than leaving a stale claim.
//
// A card delivered through the asking session's own binding has no
// relationship to explain. If a publisher pre-populated the field, saying
// nothing here would let its invented chain survive precisely on the delivery
// that had no chain of its own.
func TestAOneHopChainCLEARSTheField(t *testing.T) {
	var pl channelevents.InteractionRequestPayload
	env := requestWithAsker(t, "root-session", "root-session")
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	pl.AskingChain = []channelevents.SessionRef{
		{Namespace: "default", Name: "root-session"},
		{Namespace: "default", Name: "invented"},
	}
	raw, err := json.Marshal(pl)
	require.NoError(t, err)
	env.Payload = raw

	out := stampAskingChain(env, []spiceboxv1alpha1.NamespacedRef{
		{Namespace: "default", Name: "root-session"},
	}, logr.Discard())

	assert.Empty(t, chainOf(t, out),
		"nothing to say must CLEAR the field, or a forged chain survives on exactly the delivery that had none")
}

// TestTheChainIsNotStampedOnOtherKinds: re-encoding a non-interaction payload
// as an interaction request would destroy it.
func TestTheChainIsNotStampedOnOtherKinds(t *testing.T) {
	env := requestWithAsker(t, "worker-7", "worker-7")
	env.Kind = channelevents.KindUserMessage

	out := stampAskingChain(env, []spiceboxv1alpha1.NamespacedRef{
		{Namespace: "default", Name: "worker-7"},
		{Namespace: "default", Name: "root-session"},
	}, logr.Discard())
	assert.Equal(t, string(env.Payload), string(out.Payload))
}
