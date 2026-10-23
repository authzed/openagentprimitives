package goals_test

import (
	"context"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
	"github.com/stretchr/testify/require"
)

func TestReplyIntentReceiptAndWorkerFence(t *testing.T) {
	for _, fixture := range durableFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			store := fixture.new(t)
			ledger := store.(goals.OccurrenceStore)
			replies := store.(goals.ReplyStore)
			svc, _ := executionService(store)
			g := approveExecution(t, svc, actor(), "reply")
			o, err := ledger.Schedule(ctx, g)
			require.NoError(t, err)
			now := executionNow.Add(2 * time.Minute)
			o, err = ledger.Claim(ctx, goals.ClaimRequest{ID: o.ID, Worker: "first", Now: now, Lease: time.Second, OwnerLimit: 1, ClassLimit: 1})
			require.NoError(t, err)
			o, err = ledger.Attach(ctx, o, "root-uid", now)
			require.NoError(t, err)
			p := channelevents.OutboundUserMessagePayload{Text: "Stretch"}
			p.Delivery, err = channelevents.NewDeliveryOperation("root-uid", "call", p)
			require.NoError(t, err)
			pin := g.Execution.Terms.Destination
			intent := delivery.Intent{Session: channelevents.SessionRef{Namespace: o.Domain.Namespace, Name: o.SessionName}, Payload: p, Destination: delivery.Destination{Kind: "browser", ChannelUID: pin.ChannelUID, BindingDigest: pin.BindingDigest, Recipient: o.Domain.Owner}, CreatedAt: now}
			// The consent fixture omits a binding digest; its trusted server pin
			// is supplied here as it would be by PrepareExecution.
			if intent.Destination.BindingDigest == "" {
				t.Fatal("fixture requires a binding digest")
			}
			prepared, err := replies.PrepareReply(ctx, o, goals.RunReply{Intent: intent}, now.Add(2*time.Second))
			require.NoError(t, err, "runner submission is independent of the expired worker lease")
			require.Equal(t, "prepared", prepared.Reply.State)
			changed := intent
			changed.Payload.Text = "Another reminder"
			changed.Payload.Delivery, err = channelevents.NewDeliveryOperation("root-uid", "second-call", changed.Payload)
			require.NoError(t, err)
			_, err = replies.PrepareReply(ctx, o, goals.RunReply{Intent: changed}, now)
			require.ErrorIs(t, err, goals.ErrConflict, "one execution cannot send a second operation")
			owner, err := ledger.Claim(ctx, goals.ClaimRequest{ID: o.ID, Worker: "replacement", Now: now.Add(2 * time.Second), Lease: time.Minute, OwnerLimit: 1, ClassLimit: 1})
			require.NoError(t, err)
			_, err = replies.AttemptReply(ctx, o, now.Add(2*time.Second))
			require.ErrorIs(t, err, goals.ErrConflict)
			owner, err = replies.AttemptReply(ctx, owner, now.Add(2*time.Second))
			require.NoError(t, err)
			digest, err := intent.Destination.Digest()
			require.NoError(t, err)
			receipt := &delivery.Receipt{OperationID: p.Delivery.ID, SessionUID: "root-uid", PayloadDigest: p.Delivery.PayloadDigest, DestinationDigest: digest, Transport: "browser-transcript", Reference: "stored-reply", AcceptedAt: now.Add(3 * time.Second)}
			wrong := *receipt
			wrong.SessionUID = "another-root"
			_, err = replies.ConcludeReply(ctx, owner, &wrong, now.Add(3*time.Second))
			require.ErrorIs(t, err, delivery.ErrConflict)
			accepted, err := replies.ConcludeReply(ctx, owner, receipt, now.Add(3*time.Second))
			require.NoError(t, err)
			require.Equal(t, "accepted", accepted.Reply.State)
			require.Equal(t, receipt, accepted.Reply.Receipt)
			replayed, err := replies.PrepareReply(ctx, o, goals.RunReply{Intent: intent}, now.Add(4*time.Second))
			require.NoError(t, err)
			require.Equal(t, accepted.Reply, replayed.Reply)
			current, err := store.Get(ctx, g.Domain, g.ID)
			require.NoError(t, err)
			require.Equal(t, g, current, "a receipt never completes the durable goal")
		})
	}
}
