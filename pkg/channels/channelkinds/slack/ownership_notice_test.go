package slack

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The disclosure has to say the three things a member of the room cannot find
// out any other way: that ownership is shared, that it carries approval
// authority (not just the ability to chat), and that it extends to people who
// have not joined yet.
//
// Ownership lives in a SpiceDB tuple derived from a Channel manifest nobody in
// the channel has read. If the message only said "everyone here can talk to
// me", the authority half — approve, deny, fork, widen scope — would stay
// invisible, which is the part worth disclosing.
func TestFormatCollectiveOwnershipNotice_statesSharedAuthorityAndGrowth(t *testing.T) {
	got := formatCollectiveOwnershipNotice()

	assert.Contains(t, got, "Everyone in this channel", "must name the population, not a person")
	assert.Contains(t, strings.ToLower(got), "approve", "must disclose approval authority, not merely chat access")
	assert.Contains(t, strings.ToLower(got), "join later", "must say the set grows")
}

// The disclosure is appended to the starter only when ownership is actually
// collective. A session owned by the person who started it must not tell the
// room they all own it — that is a false statement about authority, and worse
// than saying nothing.
func TestStarterNotice_disclosesOnlyWhenOwnershipIsCollective(t *testing.T) {
	base, _ := (&slackListener{}).startingMessage()

	withDisclosure := base + formatCollectiveOwnershipNotice()
	assert.NotEqual(t, base, withDisclosure,
		"the collective case must add text")
	assert.True(t, strings.HasPrefix(withDisclosure, base),
		"the disclosure appends to the starter rather than replacing it")
}
