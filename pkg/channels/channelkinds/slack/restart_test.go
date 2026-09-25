package slack_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	slackapi "github.com/slack-go/slack"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/channel_msg_ref"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

func TestComputeRestartCut_FindsTurnAndDiscardCount(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/sess"}

	appender := turn.NewAppender(mem, scope)
	for i := 0; i <= 5; i++ {
		require.NoError(t, appender.Append(ctx, memory.Turn{
			Index: i, Role: "user", CreatedAt: time.Unix(int64(i), 0),
			Content: []memory.ContentBlock{{Type: "text", Text: "u"}},
		}))
	}
	require.NoError(t, channel_msg_ref.Record(ctx, mem, scope, "slack", "C1::1.5", 2))

	cut, discard, ok, err := slack.ComputeRestartCut(ctx, mem, scope, "C1", "", "1.5")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, cut)
	assert.Equal(t, 3, discard, "turns 3,4,5 will be discarded")
}

func TestComputeRestartCut_NotFound(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/sess"}

	_, _, ok, err := slack.ComputeRestartCut(ctx, mem, scope, "C1", "", "missing")
	require.NoError(t, err)
	assert.False(t, ok, "unknown msg ref → ok=false")
}

func TestParseRestartViewSubmission_ExtractsTextAndMetadata(t *testing.T) {
	raw := `{
        "type":"view_submission",
        "view":{
            "callback_id":"ap_restart_modal",
            "private_metadata":"{\"ns\":\"ns\",\"sess\":\"sess\",\"ch\":\"C1\",\"th\":\"1.0\",\"ts\":\"1.5\"}",
            "state":{
                "values":{
                    "restart_text_block":{
                        "restart_text":{"type":"plain_text_input","value":"edited!"}
                    }
                }
            }
        },
        "user":{"id":"U1"}
    }`
	var cb slackapi.InteractionCallback
	require.NoError(t, json.Unmarshal([]byte(raw), &cb))

	parsed, err := slack.ParseRestartViewSubmission(cb)
	require.NoError(t, err)
	assert.Equal(t, "edited!", parsed.NewUserText)
	assert.Equal(t, "ns", parsed.SessionNamespace)
	assert.Equal(t, "sess", parsed.SessionName)
	assert.Equal(t, "C1", parsed.ChannelID)
	assert.Equal(t, "1.5", parsed.MessageTS)
	assert.Equal(t, "U1", parsed.SubmitterID)
}

func TestParseRestartViewSubmission_MissingBlock_Error(t *testing.T) {
	raw := `{
        "type":"view_submission",
        "view":{
            "callback_id":"ap_restart_modal",
            "private_metadata":"{\"ns\":\"ns\",\"sess\":\"sess\"}",
            "state":{"values":{}}
        },
        "user":{"id":"U1"}
    }`
	var cb slackapi.InteractionCallback
	require.NoError(t, json.Unmarshal([]byte(raw), &cb))

	_, err := slack.ParseRestartViewSubmission(cb)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restart_text_block")
}
