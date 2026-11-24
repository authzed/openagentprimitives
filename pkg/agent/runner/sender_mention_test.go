package runner_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// TestColdStartTurnCarriesStarterMention pins the turn-0 half of the sender
// token: the first message never passes through a channelsd-written turn, so
// the runner must add the starter's token itself — durably, so a resumed
// session replays it, and visibly, so the model can address the starter
// instead of the bot mention the message text opens with.
func TestColdStartTurnCarriesStarterMention(t *testing.T) {
	cases := []struct {
		name           string
		starterMention string
		wantInRequest  bool
	}{
		{name: "starter token set: stored on turn 0 and shown to the model", starterMention: "<@U_DANA>", wantInRequest: true},
		{name: "no starter token: turn 0 carries no sender block", starterMention: "", wantInRequest: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, provider, _, store := newLoopFixture(t, []llmfake.Step{{Resp: oneShotWorkComplete()}})
			l.StarterMention = tc.starterMention
			ctx := memory.WithSystemApproval(context.Background(), "test")

			require.NoError(t, l.Run(ctx))

			reqs := provider.Requests()
			require.Len(t, reqs, 1)
			require.NotEmpty(t, reqs[0].Messages)
			first := reqMsgText(reqs[0].Messages[0])
			if tc.wantInRequest {
				assert.Contains(t, first, "Sender of this message, as a mention token: "+tc.starterMention)
			} else {
				assert.NotContains(t, first, "Sender of this message")
			}

			turns, err := store.ReadAll(ctx)
			require.NoError(t, err)
			var turn0 *memory.Turn
			for i := range turns {
				if turns[i].Index == 0 && turns[i].Role == "user" {
					turn0 = &turns[i]
				}
			}
			require.NotNil(t, turn0, "turn 0 must be placed")
			var stored []string
			for _, b := range turn0.Content {
				if b.Type == memory.BlockTypeSenderMention {
					stored = append(stored, b.Text)
				}
			}
			if tc.wantInRequest {
				assert.Equal(t, []string{tc.starterMention}, stored, "turn 0 stores the token as its own block")
			} else {
				assert.Empty(t, stored)
			}
		})
	}
}
