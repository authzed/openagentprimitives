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
