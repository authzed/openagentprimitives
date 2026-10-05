package meta

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestRespondRetainsReplyOperationAcrossProcessReconstruction(t *testing.T) {
	var published []channelevents.OutboundUserMessagePayload
	var notes []map[string]any
	cfg := RespondConfig{
		ChannelKind: "fake", Capabilities: []string{"text"},
		NATSPublish: func(_ context.Context, _ string, raw []byte) error {
			var env channelevents.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				return err
			}
			var p channelevents.OutboundUserMessagePayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				return err
			}
			published = append(published, p)
			return p.Validate()
		},
		AppendSystemNote: func(_ context.Context, note map[string]any) error {
			require.Len(t, published, len(notes)+1, "publication marker must follow the publish")
			notes = append(notes, note)
			return nil
		},
	}
	ctx := sandbox.WithIDs(context.Background(), sandbox.IDs{ToolUseID: "durable-call"})
	for _, uid := range []types.UID{"original-session", "original-session", "recreated-session"} {
		// Reconstruct both objects as a restarted runner would; no process-local
		// counter or timestamp may influence the operation identity.
		sess := &tool.SessionContext{Namespace: "default", Name: "reply-test", AgentSessionUID: uid}
		res, err := newRespondTool(cfg).Execute(ctx, json.RawMessage(`{"text":"Stand up and stretch!"}`), sess)
		require.NoError(t, err)
		require.False(t, res.IsError, res.Content)
	}
	require.Len(t, published, 3)
	require.NotNil(t, published[0].Delivery)
	require.Equal(t, published[0].Delivery, published[1].Delivery)
	require.NotEqual(t, published[0].Delivery.ID, published[2].Delivery.ID)
	for i, note := range notes {
		require.Equal(t, published[i].Delivery, note["replyPublication"])
		require.Equal(t, []string{"durable-call"}, note["delivered"])
	}
}
