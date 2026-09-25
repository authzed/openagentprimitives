package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const selfBot = "U_SELF"

// TestOurOwnPostsAreNeverAdmitted is the self-loop guard, and the single most
// important assertion in this track.
//
// Our own posts carry a BotID as well as our user id, so an implementation
// that tested BotID first would classify us as "another agent" and admit the
// loop. It would look exactly like the feature working, right up until the
// thread ran away.
func TestOurOwnPostsAreNeverAdmitted(t *testing.T) {
	o := classifyOrigin(selfBot, "B_SELF", selfBot)
	assert.Equal(t, originSelf, o,
		"our own post carries a bot id too; self must be decided BEFORE the bot-id arm")

	for _, enabled := range []bool{false, true} {
		assert.False(t, admitsInbound(o, enabled),
			"our own post must be refused with cross-agent participation %v — there is no configuration in which answering ourselves is correct", enabled)
	}
}

// TestAnotherAgentIsAdmittedOnlyWhenEnabled — the case the track exists for,
// and off by default.
func TestAnotherAgentIsAdmittedOnlyWhenEnabled(t *testing.T) {
	o := classifyOrigin("U_OTHER", "B_OTHER", selfBot)
	assert.Equal(t, originOtherAgent, o)

	assert.False(t, admitsInbound(o, false), "cross-agent participation is opt-in")
	assert.True(t, admitsInbound(o, true))
}

// TestAnUnattributedMessageIsNeverAdmitted.
//
// No user and no bot id: neither mention-gating nor credit can be charged to a
// party, so there is nobody to bound. Refused in every configuration.
func TestAnUnattributedMessageIsNeverAdmitted(t *testing.T) {
	o := classifyOrigin("", "", selfBot)
	assert.Equal(t, originUnattributed, o)
	assert.False(t, admitsInbound(o, true))
}

func TestAHumanIsAlwaysAdmitted(t *testing.T) {
	o := classifyOrigin("U_PERSON", "", selfBot)
	assert.Equal(t, originHuman, o)
	assert.True(t, admitsInbound(o, false))
	assert.True(t, admitsInbound(o, true))
}

// TestAnUnknownSelfIDDoesNotMisclassify.
//
// Before auth.test resolves, botUserID is empty. An empty self id must not make
// every message look like ours (which would reject everything) nor make our
// own look like another agent's (which would admit the loop). With no self id
// known, a bot-authored message is another agent's — and cross-agent
// participation is opt-in, so the unresolved window stays closed unless an
// operator opened it deliberately.
func TestAnUnknownSelfIDDoesNotMisclassify(t *testing.T) {
	assert.Equal(t, originHuman, classifyOrigin("U_PERSON", "", ""),
		"an unresolved self id must not turn people into bots")
	assert.Equal(t, originOtherAgent, classifyOrigin("U_OTHER", "B_OTHER", ""))
}
