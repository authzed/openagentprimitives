package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents/native"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionobservation"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/sessionevents/sqlstore"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/stretchr/testify/require"
)

type keys map[string]ed25519.PublicKey

func (k keys) PublisherKey(publisher, id string) (ed25519.PublicKey, bool) {
	key, ok := k[publisher+"/"+id]
	return key, ok
}

type authority struct {
	uid    string
	denied bool
	calls  int
}

func (a *authority) CheckSource(_ context.Context, source sessionevents.Source, _ memory.Entry) ([]sessionevents.Dependency, error) {
	a.calls++
	if a.denied || source.UID != a.uid {
		return nil, sessionevents.ErrDenied
	}
	return []sessionevents.Dependency{{ResourceType: "agentsession", ResourceID: source.ID, Permission: "view_memory"}}, nil
}
func TestNativeVerificationAndDurableReplay(t *testing.T) {
	ctx := context.Background()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer := provenance.NewSigner(private, "system:operator")
	trustedKeys := keys{"system:operator/" + signer.KeyID(): private.Public().(ed25519.PublicKey)}
	observation := sessionevents.Observation{Source: sessionevents.Source{Kind: native.Key, Namespace: "team", ID: "team/source", UID: "current-uid"}, EventID: "event-1", Kind: "trip.created", Subject: "trip-1", ObservedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), Data: json.RawMessage(`{"destination":"Example city"}`), Dependencies: []sessionevents.Dependency{{ResourceType: "private_record", ResourceID: "trip-1", Permission: "read"}}}
	content, err := json.Marshal(sessionobservation.Content{Observation: observation})
	require.NoError(t, err)
	entry := memory.Entry{Scope: memory.Scope{Kind: "session", ID: "team/source"}, Kind: sessionobservation.KindName, ID: sessionobservation.IDPrefix + "event-1", Content: content, CreatedAt: observation.ObservedAt}
	require.NoError(t, signer.Sign(&entry))
	raw, err := json.Marshal(entry)
	require.NoError(t, err)
	live := &authority{uid: "current-uid"}
	adapter := &native.Adapter{Keys: trustedKeys, Authority: live, Publishers: map[string]bool{"system:operator": true}}
	c, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "events.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	store := sqlstore.New(c.DB(), false)
	require.NoError(t, store.Migrate(ctx))
	registry := sessionevents.NewRegistry()
	registry.Register(adapter)
	engine := &sessionevents.Ingester{Store: store, Adapters: registry}
	accepted, err := engine.Ingest(ctx, native.Key, raw)
	require.NoError(t, err)
	require.Equal(t, observation.Data, accepted.Data)
	require.Contains(t, accepted.Dependencies, observation.Dependencies[0]) // cannot strip private source gate
	require.Contains(t, accepted.Dependencies, sessionevents.Dependency{ResourceType: "agentsession", ResourceID: "team/source", Permission: "view_memory"})
	require.Equal(t, provenance.EntryDigest(entry), accepted.Witness.Digest)
	require.Equal(t, "team/source/"+entry.ID, accepted.Witness.Reference)
	restarted := &sessionevents.Ingester{Store: sqlstore.New(c.DB(), false), Adapters: registry}
	replayed, err := restarted.Ingest(ctx, native.Key, raw)
	require.NoError(t, err)
	require.Equal(t, accepted, replayed)
	checkpoint, err := store.Checkpoint(ctx, observation.Source, "system:operator")
	require.NoError(t, err)
	require.EqualValues(t, 1, checkpoint.Sequence)

	t.Run("tampering is refused before source lookup", func(t *testing.T) {
		changed := entry
		changed.Content = json.RawMessage(`{"observation":{}}`)
		forged, err := json.Marshal(changed)
		require.NoError(t, err)
		before := live.calls
		_, err = engine.Ingest(ctx, native.Key, forged)
		require.ErrorIs(t, err, memory.ErrBadProvenance)
		require.Equal(t, before, live.calls)
	})
	t.Run("source recreation and revocation refuse even exact replay", func(t *testing.T) {
		live.uid = "new-uid"
		_, err := restarted.Ingest(ctx, native.Key, raw)
		require.ErrorIs(t, err, sessionevents.ErrDenied)
		live.uid = "current-uid"
		live.denied = true
		_, err = restarted.Ingest(ctx, native.Key, raw)
		require.ErrorIs(t, err, sessionevents.ErrDenied)
		live.denied = false
		cp, err := store.Checkpoint(ctx, observation.Source, "system:operator")
		require.NoError(t, err)
		require.EqualValues(t, 1, cp.Sequence)
	})
	t.Run("valid runner signatures are not platform event authority", func(t *testing.T) {
		runner := provenance.NewSigner(private, "session:team/source")
		trustedKeys["session:team/source/"+runner.KeyID()] = private.Public().(ed25519.PublicKey)
		forged := entry
		require.NoError(t, runner.Sign(&forged))
		raw, err := json.Marshal(forged)
		require.NoError(t, err)
		// Even an accidentally permissive publisher list cannot promote a session.
		adapter.Publishers["session:team/source"] = true
		_, err = engine.Ingest(ctx, native.Key, raw)
		require.ErrorIs(t, err, sessionevents.ErrDenied)
	})
	t.Run("signed scope mismatch is refused", func(t *testing.T) {
		foreign := entry
		foreign.Scope.ID = "team/other"
		require.NoError(t, signer.Sign(&foreign))
		raw, err := json.Marshal(foreign)
		require.NoError(t, err)
		_, err = engine.Ingest(ctx, native.Key, raw)
		require.ErrorIs(t, err, sessionevents.ErrDenied)
	})
	t.Run("missing trust collaborators and unknown adapters deny", func(t *testing.T) {
		_, err := (&native.Adapter{}).Verify(ctx, raw)
		require.ErrorIs(t, err, sessionevents.ErrDenied)
		_, err = engine.Ingest(ctx, "unknown", raw)
		require.ErrorIs(t, err, sessionevents.ErrDenied)
	})
}
