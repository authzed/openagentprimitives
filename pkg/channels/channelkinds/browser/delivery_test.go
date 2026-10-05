package browser

import (
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/replydelivery"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/stretchr/testify/require"
)

type lostReplyAck struct {
	memory.Memory
	lose bool
}

func (m *lostReplyAck) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	stored, err := m.Memory.Put(ctx, e)
	if err == nil && m.lose {
		return stored, errors.New("lost acceptance acknowledgement")
	}
	return stored, err
}

func TestBrowserReceiptRecoversAcceptanceAcrossDatabaseReopen(t *testing.T) {
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:operator"), "system:operator")
	path := filepath.Join(t.TempDir(), "delivery.db")
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	signer := provenance.NewSigner(priv, "system:operator")
	keys := provenance.MapKeyLookup{{Publisher: signer.Publisher(), KeyID: signer.KeyID()}: priv.Public().(ed25519.PublicKey)}
	open := func() (*memsqlite.Client, memory.Memory) {
		db, err := memsqlite.NewClient(path)
		require.NoError(t, err)
		require.NoError(t, db.Migrate(ctx))
		mem := memory.NewLocal(memsqlite.NewBackend(db), memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
		return db, mem
	}
	db, mem := open()
	payload := channelevents.OutboundUserMessagePayload{Text: "Stand up and stretch!"}
	var err error
	payload.Delivery, err = channelevents.NewDeliveryOperation("root-uid", "approved-call", payload)
	require.NoError(t, err)
	intent := delivery.Intent{Session: channelevents.SessionRef{Namespace: "team", Name: "root"}, Payload: payload, Destination: delivery.Destination{Kind: KindName, ChannelUID: "private-channel", BindingDigest: "reviewed-binding", Recipient: "owner"}, CreatedAt: time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)}
	lost := &lostReplyAck{Memory: mem, lose: true}
	receiver := (&Kind{}).NewDeliveryReceiver(provenance.NewSigningMemory(lost, signer))
	_, err = receiver.Accept(ctx, intent)
	require.ErrorContains(t, err, "lost acceptance acknowledgement")
	require.NoError(t, db.Close())

	db, mem = open()
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	receiver = (&Kind{}).NewDeliveryReceiver(provenance.NewSigningMemory(mem, provenance.NewSigner(priv, "system:operator")))
	receipt, err := receiver.Lookup(ctx, intent)
	require.NoError(t, err)
	require.NotNil(t, receipt)
	require.NoError(t, receipt.Validate(intent))
	duplicate, err := receiver.Accept(ctx, intent)
	require.NoError(t, err)
	require.Equal(t, *receipt, duplicate)
	entries, err := mem.Query(ctx, memory.Query{Scope: replyScope(intent), Kinds: []string{replydelivery.KindName}})
	require.NoError(t, err)
	require.Len(t, entries.Entries, 1, "acceptance and receipt occupy one durable entry")
	changed := intent
	changed.Destination.Recipient = "someone-else"
	_, err = receiver.Accept(ctx, changed)
	require.ErrorIs(t, err, delivery.ErrConflict)
	changed = intent
	changed.Payload.Text = "Different message"
	changed.Payload.Delivery, err = channelevents.NewDeliveryOperation("root-uid", "approved-call", changed.Payload)
	require.NoError(t, err)
	_, err = receiver.Accept(ctx, changed)
	require.ErrorIs(t, err, delivery.ErrConflict)
}
