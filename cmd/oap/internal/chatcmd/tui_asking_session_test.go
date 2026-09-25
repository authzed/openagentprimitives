package chatcmd

// The relay resolves a human-directed card up the lineage to the nearest
// binding a PERSON reads, so a card about a delegated CHILD is delivered to
// the host serving its root — this TUI. The person is then deciding on behalf
// of a session they are not watching and never started.
//
// Without attribution the modal reads as though their own agent is asking,
// which is the failure the spec names: "the request must stay attributable to
// the child, and the card must name the asking session".
//
// Slack has carried this since the interaction rewrite (buildInteractionBlocks
// appends a session provenance footer). This host had no equivalent, and it is
// the one the delivery fix newly routes children's cards to.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	local "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
)

func askingPayload(ns, name string) channelevents.InteractionRequestPayload {
	return channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: ns, Name: name},
		Category:        "tool_approval",
		RequestRef:      "req-1",
		Lead:            "Approve writing to the repository?",
	}
}

func asker(ns, name string) local.SessionRef {
	return local.SessionRef{Namespace: ns, Name: name}
}

// TestADelegatedChildsPromptNamesTheChild is the requirement itself.
func TestADelegatedChildsPromptNamesTheChild(t *testing.T) {
	got := askingSessionLine(asker("default", "worker-7"), "root-session")
	assert.Equal(t, "asked by session default/worker-7", got,
		"a card routed up the lineage must say which session is asking, or the approver cannot tell what they are approving")
}

// TestYourOwnSessionIsNotRestated is the other half, and it is what keeps the
// line above worth reading.
//
// Every ordinary prompt in your own session would otherwise carry "asked by
// session <the one you are looking at>". Noise on every card is what makes the
// one card that matters invisible.
func TestYourOwnSessionIsNotRestated(t *testing.T) {
	assert.Empty(t, askingSessionLine(asker("default", "root-session"), "root-session"),
		"the ordinary case must stay silent")
}

// TestAnUnnamedAskerSaysNothingRatherThanHalfASentence: an absent ref must not
// render "asked by session default/" or a bare "asked by session".
func TestAnUnnamedAskerSaysNothingRatherThanHalfASentence(t *testing.T) {
	assert.Empty(t, askingSessionLine(asker("default", ""), "root-session"))
	assert.Empty(t, askingSessionLine(local.SessionRef{}, "root-session"))
}

// TestANamespacelessRefStillNamesTheSession — degrade to the name rather than
// emitting a leading slash.
func TestANamespacelessRefStillNamesTheSession(t *testing.T) {
	assert.Equal(t, "asked by session worker-7",
		askingSessionLine(asker("", "worker-7"), "root-session"))
}

// TestTheAskerComesFromTheCHECKEDRefNotThePayload is the security pin, and it
// is about SUPPRESSION rather than spoofing.
//
// InteractionRequestPayload.AgentSessionRef is publisher-controlled JSON its
// own doc calls "not validated here". The ref the local kind emits comes from
// the SessionInfo the relay built AFTER cross-checking the envelope against
// its NATS subject.
//
// If the line were read off the payload, a runner that wrote the WATCHED
// session's name there would make its own attribution VANISH — the "only show
// when different" rule would suppress it — hiding that a delegated child is
// asking, at exactly the moment the approver needs to know. So the payload
// here claims to be the watched session while the checked ref says otherwise,
// and the line must still appear.
func TestTheAskerComesFromTheCHECKEDRefNotThePayload(t *testing.T) {
	forged := askingPayload("default", "root-session") // claims to be the watched session
	out := renderInteractionModal(forged, asker("default", "worker-7"), "root-session", 100, 40, aptest.ColorTheme())

	assert.Contains(t, out, "worker-7",
		"the checked ref must decide what is shown; a payload claim must not be able to suppress attribution")
	assert.Contains(t, out, "asked by session")
}

// TestTheModalShowsItAboveTheQuestion.
//
// Placement is the point: this changes what the question MEANS, so it has to
// be read BEFORE the question rather than discovered in a footer after the
// decision is already made.
func TestTheModalShowsItAboveTheQuestion(t *testing.T) {
	out := renderInteractionModal(askingPayload("default", "worker-7"),
		asker("default", "worker-7"), "root-session", 100, 40, aptest.ColorTheme())

	assert.Contains(t, out, "worker-7", "the modal must name the asking session")
	askIdx := strings.Index(out, "asked by session")
	leadIdx := strings.Index(out, "Approve writing")
	assert.Positive(t, askIdx)
	assert.Positive(t, leadIdx)
	assert.Less(t, askIdx, leadIdx,
		"attribution must precede the question it reframes")
}

func chainOf(names ...string) []channelevents.SessionRef {
	out := make([]channelevents.SessionRef, 0, len(names))
	for _, n := range names {
		out = append(out, channelevents.SessionRef{Namespace: "default", Name: n})
	}
	return out
}

// TestTheChainSaysHOWTheAskerRelates is the spec's requirement: "an approver
// seeing a request needs the whole chain — 'this is a grandchild of the
// session you are in' is material to the decision."
//
// Naming the asker alone leaves the approver to guess whether they delegated
// this themselves or it is two removes away.
func TestTheChainSaysHOWTheAskerRelates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		chain []channelevents.SessionRef
		want  string
	}{
		{"one hop is a subagent", chainOf("worker-7", "root-session"), "a subagent of this session"},
		{"two hops is a grandchild", chainOf("worker-7", "mid", "root-session"), "a grandchild of this session"},
		{"deeper counts explicitly", chainOf("w", "a", "b", "root-session"), "3 levels below this session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, askingChainLine(tc.chain, "root-session"))
		})
	}
}

// TestNoChainSaysNothing: a card delivered through the asking session's own
// binding has no relationship to explain, and neither does one that never went
// through the lineage walk. Announcing a delegation that did not happen is
// worse than staying quiet.
func TestNoChainSaysNothing(t *testing.T) {
	assert.Empty(t, askingChainLine(nil, "root-session"))
	assert.Empty(t, askingChainLine(chainOf("root-session"), "root-session"),
		"a chain of one is a card about the session that owns the binding")
}

// TestAnAnchorThatIsNotOnScreenIsNamed.
//
// The far end is normally the session the reader is watching, and "of this
// session" reads better than repeating its name. When it is NOT — a host
// serving several sessions — the sentence has to say which one, or it points
// at nothing.
func TestAnAnchorThatIsNotOnScreenIsNamed(t *testing.T) {
	assert.Equal(t, "a subagent of other-root",
		askingChainLine(chainOf("worker-7", "other-root"), "root-session"))
}

// TestTheModalCombinesWhoAndHow — both facts in one line, since either alone
// leaves the approver guessing.
func TestTheModalCombinesWhoAndHow(t *testing.T) {
	p := askingPayload("default", "worker-7")
	p.AskingChain = chainOf("worker-7", "mid", "root-session")

	out := renderInteractionModal(p, asker("default", "worker-7"), "root-session", 100, 40, aptest.ColorTheme())

	assert.Contains(t, out, "worker-7", "who is asking")
	assert.Contains(t, out, "a grandchild of this session", "and how they relate to what is on screen")
}

// TestTheModalStaysCleanForYourOwnSession.
func TestTheModalStaysCleanForYourOwnSession(t *testing.T) {
	out := renderInteractionModal(askingPayload("default", "root-session"),
		asker("default", "root-session"), "root-session", 100, 40, aptest.ColorTheme())
	assert.NotContains(t, out, "asked by session",
		"a prompt from the session you are watching must not be annotated")
}
