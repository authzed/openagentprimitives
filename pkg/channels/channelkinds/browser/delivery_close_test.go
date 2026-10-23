package browser

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
	"github.com/authzed/openagentprimitives/pkg/memory"
	meminmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/stretchr/testify/require"
)

type delayedReplyWrite struct {
	memory.Memory
	started, resume chan struct{}
}

func (m *delayedReplyWrite) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	close(m.started)
	select {
	case <-m.resume:
	case <-ctx.Done():
		return memory.Entry{}, ctx.Err()
	}
	return m.Memory.Put(ctx, e)
}

func TestBrowserClosureFencesAnInFlightAcceptance(t *testing.T) {
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:operator"), "system:operator")
	seed := make([]byte, ed25519.SeedSize)
	oldKey := ed25519.NewKeyFromSeed(seed)
	seed[0] = 1
	newKey := ed25519.NewKeyFromSeed(seed)
	oldSigner, newSigner := provenance.NewSigner(oldKey, "system:operator"), provenance.NewSigner(newKey, "system:operator")
	keys := provenance.MapKeyLookup{
		{Publisher: oldSigner.Publisher(), KeyID: oldSigner.KeyID()}: oldKey.Public().(ed25519.PublicKey),
		{Publisher: newSigner.Publisher(), KeyID: newSigner.KeyID()}: newKey.Public().(ed25519.PublicKey),
	}
	base := memory.NewLocal(meminmem.NewBackend(), memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
	delayed := &delayedReplyWrite{Memory: base, started: make(chan struct{}), resume: make(chan struct{})}
	old := (&Kind{}).NewDeliveryReceiver(provenance.NewSigningMemory(delayed, oldSigner))
	current := (&Kind{}).NewDeliveryReceiver(provenance.NewSigningMemory(base, newSigner))
	p := channelevents.OutboundUserMessagePayload{Text: "Private reminder"}
	var err error
	p.Delivery, err = channelevents.NewDeliveryOperation("root-uid", "call", p)
	require.NoError(t, err)
	i := delivery.Intent{Session: channelevents.SessionRef{Namespace: "team", Name: "root"}, Payload: p, Destination: delivery.Destination{Kind: KindName, ChannelUID: "private", BindingDigest: "pin", Recipient: "owner"}, CreatedAt: time.Now().UTC()}
	done := make(chan error, 1)
	go func() { _, err := old.Accept(ctx, i); done <- err }()
	select {
	case <-delayed.started:
	case <-time.After(5 * time.Second):
		t.Fatal("old acceptance did not reach its write")
	}
	receipt, err := current.(delivery.Closer).Close(ctx, i)
	require.NoError(t, err)
	require.Nil(t, receipt)
	close(delayed.resume)
	select {
	case err := <-done:
		require.ErrorIs(t, err, delivery.ErrClosed)
	case <-time.After(5 * time.Second):
		t.Fatal("old acceptance did not finish")
	}
	_, err = current.Lookup(ctx, i)
	require.ErrorIs(t, err, delivery.ErrClosed)
	_, err = current.Accept(ctx, i)
	require.ErrorIs(t, err, delivery.ErrClosed, "later attempts cannot undo a durable closure")
}
