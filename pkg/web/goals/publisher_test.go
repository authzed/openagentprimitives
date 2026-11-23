package goals

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"

	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/memory"
	goalinmem "github.com/authzed/openagentprimitives/pkg/memory/goals/inmem"
	goalsqlite "github.com/authzed/openagentprimitives/pkg/memory/goals/sqlite"
	meminmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalevent"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type failedPut struct {
	memory.Memory
	fail bool
}

func TestPublisherSignsDurableOccurrenceAuditAcrossRestart(t *testing.T) {
	ctx := context.Background()
	db, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "publisher.db"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	store := goalsqlite.New(db.DB())
	require.NoError(t, store.Migrate(ctx))
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	d := core.Domain{Namespace: "team", Owner: "alice", Class: "assistant", ClassUID: "class-uid"}
	g := core.Goal{ID: "goal-reminder", Domain: d, Revision: 4, State: core.Active, UpdatedAt: now, Execution: &core.ExecutionConsent{
		RequestRevision: 3, Digest: "reviewed-work", Terms: core.ExecutionTerms{DueAt: now, ExpiresAt: now.Add(time.Hour)},
		Decision: &core.ExecutionDecision{Owner: d.Owner, Digest: "reviewed-work", Approved: true, Witness: "test-only-human-proof"},
	}}
	_, err = store.Commit(ctx, core.Mutation{Goal: g, RequestID: "seed", Hash: "seed", Event: core.Event{ID: "goalev-seed", Goal: g}})
	require.NoError(t, err)
	o, err := store.Schedule(ctx, g)
	require.NoError(t, err)
	claimTime := now.Add(time.Minute)
	_, err = store.Claim(ctx, core.ClaimRequest{ID: o.ID, Worker: "worker", Now: claimTime, Lease: time.Minute, OwnerLimit: 1, ClassLimit: 1})
	require.NoError(t, err)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer := provenance.NewSigner(priv, "system:operator")
	keys := provenance.MapKeyLookup{provenance.PubKeyRef{Publisher: signer.Publisher(), KeyID: signer.KeyID()}: pub}
	mem := memory.NewLocal(meminmem.NewBackend(), memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
	failing := &failedPut{Memory: mem, fail: true}
	publisher := &Publisher{Store: store, Memory: failing, Signer: signer}
	require.Error(t, publisher.Flush(ctx))
	failing.fail = false
	restarted := &Publisher{Store: store, Memory: failing, Signer: provenance.NewSigner(priv, "system:operator")}
	require.NoError(t, restarted.Flush(ctx))
	scope, err := memory.ResourceScope(ResourceType, d.ID())
	require.NoError(t, err)
	q, err := mem.Query(memory.WithSystemApproval(ctx, "test"), memory.Query{Scope: scope, Kinds: []string{goalevent.KindName}})
	require.NoError(t, err)
	require.Len(t, q.Entries, 3)
	sort.Slice(q.Entries, func(i, j int) bool { return q.Entries[i].Provenance.Seq < q.Entries[j].Provenance.Seq })
	report := provenance.NewVerifier(keys).VerifyChain("system:operator", q.Entries, nil)
	assert.Empty(t, report.Findings)
	var event core.Event
	require.NoError(t, json.Unmarshal(q.Entries[2].Content, &event))
	assert.Equal(t, "execution_claimed", event.Action)
	require.NotNil(t, event.Occurrence)
	assert.Equal(t, o.SessionName, event.Occurrence.SessionName)
	assert.Equal(t, claimTime, q.Entries[2].CreatedAt)
	pending, err := store.Pending(ctx, 100)
	require.NoError(t, err)
	assert.Empty(t, pending)
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

type countedHistoryReads struct {
	memory.Memory
	rows int
}

func (m *countedHistoryReads) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	result, err := m.Memory.Query(ctx, q)
	m.rows += len(result.Entries)
	return result, err
}
func TestPublisherFreshEventsDoNotRescanGrowingAuditHistory(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "system:operator")
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	signer := provenance.NewSigner(priv, "system:operator")
	keys := provenance.MapKeyLookup{{Publisher: signer.Publisher(), KeyID: signer.KeyID()}: priv.Public().(ed25519.PublicKey)}
	mem := &countedHistoryReads{Memory: memory.NewLocal(meminmem.NewBackend(), memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))}
	store := goalinmem.New()
	d := core.Domain{Namespace: "team", Owner: "alice", Class: "assistant", ClassUID: "uid"}
	for n := 0; n < 50; n++ {
		id := fmt.Sprintf("goal-%03d", n)
		g := core.Goal{ID: id, Domain: d, Revision: 1, State: core.Draft, UpdatedAt: time.Now().UTC()}
		_, err := store.Commit(ctx, core.Mutation{Goal: g, RequestID: id, Hash: id, Event: core.Event{ID: "goalev-" + id, Goal: g}})
		require.NoError(t, err)
	}
	p := &Publisher{Store: store, Memory: mem, Signer: signer}
	require.NoError(t, p.Flush(ctx))
	require.Less(t, mem.rows, 50, "a fresh append must not reseed from the growing domain log")
	scope, err := memory.ResourceScope(ResourceType, d.ID())
	require.NoError(t, err)
	result, err := mem.Query(ctx, memory.Query{Scope: scope})
	require.NoError(t, err)
	require.Len(t, result.Entries, 50)
	report := provenance.NewVerifier(keys).VerifyChain(signer.Publisher(), result.Entries, nil)
	require.Empty(t, report.Findings)
}
