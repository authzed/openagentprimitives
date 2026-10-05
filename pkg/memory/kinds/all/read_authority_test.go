package all_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

// sessionUnreadable is the reviewed set of Kinds a session credential may NOT
// read, each with the reason it is held back from the agent whose session it
// belongs to.
//
// The read door is an OPTIONAL interface — a Kind that says nothing is
// readable, which is what every Kind was before the door existed — so nothing
// in the type system forces a new sensitive Kind to declare itself. This pin
// is what does. It is the read-side twin of
// TestSessionWritableSet_MatchesTheReviewedPin, and it exists for the same
// reason: the failure it guards is silent.
var sessionUnreadable = map[string]string{
	"goal_actor":     "platform attestation of the current human; goal ownership is resolved by the operator, not by agent-authored memory",
	"goal_event":     "historical goal snapshots may contain revoked source data; sessions must use the goal API, which rechecks retained sources",
	"pt_tag_content": "redacted content the model was deliberately not shown; readable back through query_memory would make redaction a formality",
}

func TestSessionUnreadableSet_MatchesTheReviewedPin(t *testing.T) {
	got := map[string]string{}
	for _, k := range memory.RegisteredKinds() {
		if !memory.SessionMayRead(k.Name()) {
			got[k.Name()] = sessionUnreadable[k.Name()]
		}
	}
	assert.Equal(t, sessionUnreadable, got,
		"a Kind became session-unreadable without being reviewed, or a reviewed one became readable. "+
			"Hiding a Kind from the agent is a real restriction — an agent that legitimately needs it will "+
			"fail with a 403 it cannot act on — and UN-hiding one silently exposes data the platform holds "+
			"on the session's behalf. Add it here with the reason, or take it out of the set.")
}

// TestUnregisteredKindIsNotSessionReadable pins the fail-closed direction.
//
// "Declared nothing" must not read as "anyone may read it" — the same rule
// authorizeKindWrite applies to writes. A typo'd Kind name in a query is
// refused rather than answered.
func TestUnregisteredKindIsNotSessionReadable(t *testing.T) {
	assert.False(t, memory.SessionMayRead("no_such_kind_exists"),
		"an unregistered Kind has declared no read authority, and absence of a declaration must fail closed")
}

// TestOrdinaryKindsStaySessionReadable stops the door from becoming a blanket
// restriction. If this ever fails, the default flipped and every agent's
// memory tools went dark at once.
func TestOrdinaryKindsStaySessionReadable(t *testing.T) {
	for _, name := range []string{"turn", "artifact", "label"} {
		assert.True(t, memory.SessionMayRead(name),
			"%q must stay readable: the door is for the rare platform-only Kind, not a new default", name)
	}
}
