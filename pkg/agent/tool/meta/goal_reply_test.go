package meta

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
	"github.com/stretchr/testify/require"
)

func replyFixture(t *testing.T) (channelevents.OutboundUserMessagePayload, delivery.Intent, delivery.Receipt) {
	t.Helper()
	p := channelevents.OutboundUserMessagePayload{Text: "Stretch"}
	var err error
	p.Delivery, err = channelevents.NewDeliveryOperation("uid", "call", p)
	require.NoError(t, err)
	i := delivery.Intent{Session: channelevents.SessionRef{Namespace: "team", Name: "root"}, Destination: delivery.Destination{Kind: "browser", ChannelUID: "channel", BindingDigest: "pin", Recipient: "owner"}, Payload: p, CreatedAt: time.Now().UTC()}
	digest, err := i.Destination.Digest()
	require.NoError(t, err)
	r := delivery.Receipt{OperationID: p.Delivery.ID, SessionUID: "uid", PayloadDigest: p.Delivery.PayloadDigest, DestinationDigest: digest, Transport: "browser-transcript", Reference: "reply-receipt", AcceptedAt: time.Now().UTC()}
	return p, i, r
}

func TestGoalReplyAcceptorWaitsForMatchingReceipt(t *testing.T) {
	p, i, r := replyFixture(t)
	calls := 0
	accept := GoalReplyAcceptor(func(_ context.Context, req goals.Request) (goals.Response, error) {
		calls++
		require.Equal(t, "prepare_reply", req.Operation)
		require.Equal(t, p, *req.Reply)
		reply := &goals.RunReply{Intent: i, State: "prepared"}
		if calls == 2 {
			reply.State, reply.Receipt = "accepted", &r
		}
		return goals.Response{Run: &goals.Occurrence{Reply: reply}}, nil
	})
	got, err := accept(context.Background(), p)
	require.NoError(t, err)
	require.Equal(t, r, got)
	require.Equal(t, 2, calls)
	wrong := GoalReplyAcceptor(func(context.Context, goals.Request) (goals.Response, error) {
		changed := i
		changed.Payload.Text = "another body"
		return goals.Response{Run: &goals.Occurrence{Reply: &goals.RunReply{Intent: changed, State: "accepted", Receipt: &r}}}, nil
	})
	_, err = wrong(context.Background(), p)
	require.ErrorIs(t, err, delivery.ErrConflict)
}

func TestRespondDurableAcceptancePrecedesLivePublish(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfirmed acceptance", true: "accepted but live notification lost"}[accepted], func(t *testing.T) {
			_, _, receipt := replyFixture(t)
			acceptanceChecked := false
			publishes := 0
			var note map[string]any
			cfg := RespondConfig{ChannelKind: "fake", Capabilities: []string{"text"}, AcceptReply: func(_ context.Context, p channelevents.OutboundUserMessagePayload) (delivery.Receipt, error) {
				acceptanceChecked = true
				require.Equal(t, receipt.OperationID, p.Delivery.ID)
				if !accepted {
					return delivery.Receipt{}, errors.New("acknowledgement unknown")
				}
				return receipt, nil
			}, NATSPublish: func(context.Context, string, []byte) error {
				require.True(t, acceptanceChecked)
				publishes++
				return errors.New("live bus unavailable")
			}, AppendSystemNote: func(_ context.Context, n map[string]any) error { note = n; return nil }}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			ctx = sandbox.WithIDs(ctx, sandbox.IDs{ToolUseID: "call"})
			result, err := newRespondTool(cfg).Execute(ctx, json.RawMessage(`{"text":"Stretch"}`), &tool.SessionContext{Namespace: "team", Name: "root", AgentSessionUID: "uid"})
			require.NoError(t, err)
			if accepted {
				require.False(t, result.IsError, result.Content)
				require.Positive(t, publishes)
				require.Contains(t, result.Content, "does not prove the user read it")
				require.Equal(t, &receipt, note["replyReceipt"])
				require.NotContains(t, note, "replyPublication", "failed publication is not recorded as successful")
			} else {
				require.True(t, result.IsError)
				require.Zero(t, publishes)
				require.Nil(t, note)
			}
		})
	}
}
