package meta

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	// Registers the slack kind so ChannelKind "slack" resolves its real
	// ComponentFormatter/ComponentValidator — the same wiring the runner uses.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

func respondCtx() context.Context {
	return memory.WithSystemApproval(context.Background(), "test")
}

func TestRespondSchemaIncludesBlocksWithComponents(t *testing.T) {
	tt := newRespondTool(RespondConfig{
		Capabilities: []string{"text", "markdown", "components"},
		ChannelKind:  "slack",
	})
	var schema map[string]any
	require.NoError(t, json.Unmarshal(tt.InputSchema(), &schema))
	props, _ := schema["properties"].(map[string]any)

	blocks, ok := props["blocks"].(map[string]any)
	require.True(t, ok, "blocks property present when 'components' capability is set")
	desc, _ := blocks["description"].(string)
	assert.Contains(t, desc, "rich_text", "blocks description carries the kind's authoring instructions, not the old stub text")
	assert.Contains(t, tt.Description(), "blocks", "tool description advertises components")
}

func TestRespondComponentsNotSupportedChannel_IsErrorNoPublish(t *testing.T) {
	calls := 0
	tt := newRespondTool(RespondConfig{
		Capabilities:      []string{"text"}, // no "components"
		ChannelKind:       "fake",
		NATSPublish:       func(context.Context, string, []byte) error { calls++; return nil },
		NATSSubjectPrefix: "ap.session.default.foo",
	})
	res, err := tt.Execute(respondCtx(),
		json.RawMessage(`{"text":"hi","blocks":[{"type":"divider"}]}`),
		&tool.SessionContext{Namespace: "default", Name: "foo"})
	require.NoError(t, err)
	assert.True(t, res.IsError, "blocks on a channel without the components capability must be an error")
	assert.Contains(t, res.Content, "not supported")
	assert.Equal(t, 0, calls, "nothing may be published when components are unsupported")
}

func TestRespondComponentsValidationFailure_IsErrorNoPublish(t *testing.T) {
	calls := 0
	tt := newRespondTool(RespondConfig{
		Capabilities:      []string{"text", "components"},
		ChannelKind:       "slack",
		NATSPublish:       func(context.Context, string, []byte) error { calls++; return nil },
		NATSSubjectPrefix: "ap.session.default.foo",
	})
	// An actions block is rejected by the slack validator (interactive).
	args := `{"text":"hi","blocks":[{"type":"actions","elements":[{"type":"button","text":{"type":"plain_text","text":"Go"},"action_id":"a"}]}]}`
	res, err := tt.Execute(respondCtx(), json.RawMessage(args),
		&tool.SessionContext{Namespace: "default", Name: "foo"})
	require.NoError(t, err)
	assert.True(t, res.IsError, "invalid components must surface IsError")
	assert.Contains(t, res.Content, "interactive", "the detailed validator error must reach the model")
	assert.Equal(t, 0, calls, "no envelope may be published when validation fails")
}

func TestRespondComponentsSuccess_CarriesComponentsOnPayload(t *testing.T) {
	var published [][]byte
	tt := newRespondTool(RespondConfig{
		Capabilities:      []string{"text", "markdown", "components"},
		ChannelKind:       "slack",
		NATSPublish:       func(_ context.Context, _ string, p []byte) error { published = append(published, p); return nil },
		NATSSubjectPrefix: "ap.session.default.foo",
	})
	blocksJSON := `[{"type":"section","text":{"type":"mrkdwn","text":"*hi*"}}]`
	args := `{"text":"summary","blocks":` + blocksJSON + `}`
	res, err := tt.Execute(respondCtx(), json.RawMessage(args),
		&tool.SessionContext{Namespace: "default", Name: "foo"})
	require.NoError(t, err)
	require.False(t, res.IsError, "valid components must publish; got: %s", res.Content)
	require.Len(t, published, 1)

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(published[0], &env))
	var pl channelevents.OutboundUserMessagePayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, "summary", pl.Text, "text is carried as the fallback")
	assert.JSONEq(t, blocksJSON, string(pl.Components), "validated components are carried on the payload")
}

// The leakage gate measures text; without feeding it the block content, an agent
// could route blockable content through blocks and bypass the gate. This proves
// the gate sees the block text even when the reply text is innocuous.
func TestRespondComponentsLeakageGateSeesBlockText(t *testing.T) {
	var sawGateText string
	tt := newRespondTool(RespondConfig{
		Capabilities:      []string{"text", "components"},
		ChannelKind:       "slack",
		NATSPublish:       func(context.Context, string, []byte) error { return nil },
		NATSSubjectPrefix: "ap.session.default.foo",
		LeakageGate: func(_ context.Context, _ *tool.SessionContext, text string, _ []channelevents.AttachmentRef) error {
			sawGateText = text
			if strings.Contains(text, "SECRET") {
				return errors.New("blocked by audience check")
			}
			return nil
		},
	})
	// "SECRET" appears only in the block, not in the reply text.
	args := `{"text":"all clear","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"the SECRET value is 42"}}]}`
	res, err := tt.Execute(respondCtx(), json.RawMessage(args),
		&tool.SessionContext{Namespace: "default", Name: "foo"})
	require.NoError(t, err)
	assert.Contains(t, sawGateText, "SECRET", "the gate must measure block text, not just the reply text")
	assert.True(t, res.IsError, "the gate blocked on content carried only in blocks")
}
