package goals

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sort"
	"testing"

	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/memory"
	goalinmem "github.com/authzed/openagentprimitives/pkg/memory/goals/inmem"
	meminmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalevent"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type failedPut struct {
	memory.Memory
	fail bool
}

func (m *failedPut) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	if m.fail {
		return memory.Entry{}, errors.New("injected publication outage")
	}
	return m.Memory.Put(ctx, e)
}
func TestPublisherReplaysSavedEnvelopeBeforeSigningAfterRestart(t *testing.T) {
	ctx := context.Background()
	store := goalinmem.New()
	keys := provenance.MapKeyLookup{}
	mem := memory.NewLocal(meminmem.NewBackend(), memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
	failing := &failedPut{Memory: mem, fail: true}
	newSigner := func() *provenance.Signer {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		s := provenance.NewSigner(priv, "system:operator")
		keys[provenance.PubKeyRef{Publisher: s.Publisher(), KeyID: s.KeyID()}] = pub
		return s
	}
	d := core.Domain{Namespace: "team", Owner: "alice", Class: "assistant", ClassUID: "uid"}
	commit := func(id, event string) {
		g := core.Goal{ID: id, Domain: d, Revision: 1, State: core.Draft}
		_, err := store.Commit(ctx, core.Mutation{Goal: g, RequestID: id, Hash: id, Event: core.Event{ID: event, Goal: g}})
		require.NoError(t, err)
	}
	commit("goal-z", "goalev-z")
	first := &Publisher{Store: store, Memory: failing, Signer: newSigner()}
	require.Error(t, first.Flush(ctx))
	saved, err := store.Envelope(ctx, "goalev-z")
	require.NoError(t, err)
	require.NotEmpty(t, saved)
	// A new goal sorts before the signed-but-unpublished goal. Publishing it
	// first would mint sequence 1 twice, despite every signature being valid.
	commit("goal-a", "goalev-a")
	failing.fail = false
	restarted := &Publisher{Store: store, Memory: failing, Signer: newSigner()}
	require.NoError(t, restarted.Flush(ctx))
	require.NoError(t, restarted.Flush(ctx))
	again, err := store.Envelope(ctx, "goalev-z")
	require.NoError(t, err)
	assert.Equal(t, saved, again)
	scope, err := memory.ResourceScope(ResourceType, d.ID())
	require.NoError(t, err)
	q, err := mem.Query(memory.WithSystemApproval(ctx, "test"), memory.Query{Scope: scope, Kinds: []string{goalevent.KindName}})
	require.NoError(t, err)
	require.Len(t, q.Entries, 2)
	sort.Slice(q.Entries, func(i, j int) bool { return q.Entries[i].Provenance.Seq < q.Entries[j].Provenance.Seq })
	assert.Equal(t, "goalev-z", q.Entries[0].ID)
	report := provenance.NewVerifier(keys).VerifyChain("system:operator", q.Entries, nil)
	assert.Empty(t, report.Findings)
	assert.Equal(t, 2, report.OK)
	pending, err := store.Pending(ctx, 100)
	require.NoError(t, err)
	assert.Empty(t, pending)
}
