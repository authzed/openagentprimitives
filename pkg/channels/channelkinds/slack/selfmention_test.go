package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSelfMention(t *testing.T) {
	cases := []struct {
		name     string
		external map[string]string
		want     string
	}{
		{name: "bot_user_id stamped: renders the bot's own token", external: map[string]string{"bot_user_id": "U0BOT"}, want: "<@U0BOT>"},
		{name: "bot_user_id absent: no token", external: map[string]string{"channel_id": "C01"}, want: ""},
		{name: "nil external: no token", external: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, (&Kind{}).SelfMention(tc.external))
		})
	}
}

// TestSenderMention pins that the token derived from a stored external id is
// the one the listener stamps on a live inbound, so a replayed message
// addresses the same person the original would have.
func TestSenderMention(t *testing.T) {
	cases := []struct {
		name       string
		externalID string
		want       string
	}{
		{name: "user id: that user's token", externalID: "U1", want: "<@U1>"},
		{name: "empty id: no token", externalID: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, (&Kind{}).SenderMention(tc.externalID))
			assert.Equal(t, mentionToken(tc.externalID), (&Kind{}).SenderMention(tc.externalID), "same token the listener mints")
		})
	}
}
