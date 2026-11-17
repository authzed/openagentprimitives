package fake

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
	"github.com/authzed/openagentprimitives/pkg/memory"
	meminmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/replydelivery"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

// lostAck stores a write and then reports it failed, the shape of a crash
// between the transport accepting a post and the caller hearing about it.
type lostAck struct {
	memory.Memory
	lose bool
}

func (m *lostAck) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	stored, err := m.Memory.Put(ctx, e)
	if err == nil && m.lose {
		return stored, errors.New("acknowledgement lost")
	}
	return stored, err
}

func fakeDeliveryIntent(t *testing.T) delivery.Intent {
	t.Helper()
	p := channelevents.OutboundUserMessagePayload{Text: "Weekly summary"}
	var err error
	p.Delivery, err = channelevents.NewDeliveryOperation("run-uid", "call", p)
	require.NoError(t, err)
	return delivery.Intent{Session: channelevents.SessionRef{Namespace: "team", Name: "run"}, Payload: p, Destination: delivery.Destination{Kind: "fake", ChannelUID: "channel-uid", BindingDigest: "group-pin", Recipient: "group:g1#member"}, CreatedAt: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)}
}

func TestFakeDeliveryReceiverIsIdempotentByDeliveryID(t *testing.T) {
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:operator"), "system:operator")
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	signer := provenance.NewSigner(priv, "system:operator")
	keys := provenance.MapKeyLookup{{Publisher: signer.Publisher(), KeyID: signer.KeyID()}: priv.Public().(ed25519.PublicKey)}
	base := memory.NewLocal(meminmem.NewBackend(), memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
	lost := &lostAck{Memory: base, lose: true}
	receiver := Kind{}.NewDeliveryReceiver(provenance.NewSigningMemory(lost, signer))
	require.NotNil(t, receiver)
	assert.True(t, receiver.RetryAbsent(), "acceptance is one append-only write, so retrying an absent post is safe")
	i := fakeDeliveryIntent(t)

	before, err := receiver.Lookup(ctx, i)
	require.NoError(t, err)
	assert.Nil(t, before, "nothing posted yet")

	_, err = receiver.Accept(ctx, i)
	require.ErrorContains(t, err, "acknowledgement lost")
	lost.lose = false

	found, err := receiver.Lookup(ctx, i)
	require.NoError(t, err)
	require.NotNil(t, found, "the post that landed is found by its delivery ID")
	require.NoError(t, found.Validate(i))
	again, err := receiver.Accept(ctx, i)
	require.NoError(t, err)
	assert.Equal(t, *found, again, "a retry returns the first post's receipt")
	entries, err := base.Query(ctx, memory.Query{Scope: memory.Scope{Kind: "session", ID: "team/run"}, Kinds: []string{replydelivery.KindName}})
	require.NoError(t, err)
	assert.Len(t, entries.Entries, 1, "a retry never posts twice")

	closed, err := receiver.(delivery.Closer).Close(ctx, i)
	require.NoError(t, err)
	assert.Equal(t, found, closed, "closing after acceptance returns the acceptance")

	changed := i
	changed.Destination.Recipient = "group:other#member"
	_, err = receiver.Accept(ctx, changed)
	assert.ErrorIs(t, err, delivery.ErrConflict, "the same delivery ID to another destination is a conflict")
}

func TestFakeDeliveryReceiverCloseRefusesLaterAcceptance(t *testing.T) {
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:operator"), "system:operator")
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	signer := provenance.NewSigner(priv, "system:operator")
	keys := provenance.MapKeyLookup{{Publisher: signer.Publisher(), KeyID: signer.KeyID()}: priv.Public().(ed25519.PublicKey)}
	base := memory.NewLocal(meminmem.NewBackend(), memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
	receiver := Kind{}.NewDeliveryReceiver(provenance.NewSigningMemory(base, signer))
	i := fakeDeliveryIntent(t)

	closed, err := receiver.(delivery.Closer).Close(ctx, i)
	require.NoError(t, err)
	assert.Nil(t, closed, "closed before any post")
	_, err = receiver.Lookup(ctx, i)
	assert.ErrorIs(t, err, delivery.ErrClosed)
	_, err = receiver.Accept(ctx, i)
	assert.ErrorIs(t, err, delivery.ErrClosed, "a closed delivery is never posted")
}
