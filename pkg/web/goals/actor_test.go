package goals

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/memory"
	meminmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalactor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// operatorCtx is the caller context ServeHTTP gives actor resolution.
func operatorCtx() context.Context {
	return memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:operator"), "system:operator")
}

// turnProof records a signed channelsd attestation naming owner for the
// inbound identified by ref.
func (f *serverFixture) turnProof(t *testing.T, owner, ref string) {
	t.Helper()
	f.n++
	b, err := json.Marshal(goalactor.Content{Owner: owner, SessionUID: "session-uid", ClassUID: "class-uid", InboundRef: ref})
	require.NoError(t, err)
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:channelsd"), "system:channelsd")
	_, err = f.signed.Put(ctx, memory.Entry{Scope: memory.Scope{Kind: "session", ID: "team/session"}, Kind: goalactor.KindName, ID: fmt.Sprintf("goalactor-%d", f.n), CreatedAt: time.Now().UTC(), Content: b})
	require.NoError(t, err)
}

func TestActorForTurn(t *testing.T) {
	type proof struct{ owner, ref string }
	cases := []struct {
		name      string
		proofs    []proof
		turnRef   string
		wantOwner string
		wantAsk   bool
	}{
		{name: "the turn's own attestation: actor is its author", proofs: []proof{{"alice", "r1"}}, turnRef: "r1", wantOwner: "alice"},
		{name: "a different participant spoke after the turn began: denied, ask again", proofs: []proof{{"alice", "r1"}, {"bob", "r2"}}, turnRef: "r1", wantAsk: true},
		{name: "the same author spoke again mid-turn: actor is still its author", proofs: []proof{{"alice", "r1"}, {"alice", "r2"}}, turnRef: "r1", wantOwner: "alice"},
		{name: "earlier speakers do not matter: actor is the later turn's author", proofs: []proof{{"alice", "r1"}, {"bob", "r2"}}, turnRef: "r2", wantOwner: "bob"},
		{name: "the opener spoke again after an interjection: actor is the opener", proofs: []proof{{"alice", "r1"}, {"bob", "r2"}, {"alice", "r3"}}, turnRef: "r3", wantOwner: "alice"},
		{name: "an interjection after the opener's latest message: denied, ask again", proofs: []proof{{"alice", "r1"}, {"alice", "r2"}, {"bob", "r3"}}, turnRef: "r2", wantAsk: true},
		{name: "no attestation carries the turn ref: denied", proofs: []proof{{"alice", "r1"}}, turnRef: "unknown"},
		{name: "empty turn ref never selects an unreffed attestation: denied", proofs: []proof{{"alice", ""}}, turnRef: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture(t)
			for _, p := range tc.proofs {
				f.turnProof(t, p.owner, p.ref)
			}
			a, err := f.s.actorForTurn(operatorCtx(), "team", "session", tc.turnRef)
			if tc.wantOwner == "" {
				require.Error(t, err)
				assert.ErrorIs(t, err, core.ErrDenied)
				if tc.wantAsk {
					assert.Contains(t, err.Error(), "ask again")
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantOwner, a.Domain.Owner)
			assert.Equal(t, "team/session", a.Session)
			assert.Equal(t, "session-uid", a.SessionUID)
			assert.NotEmpty(t, a.Proof)
		})
	}
}

func TestActorForTurnRefusesMismatchedSessionOrClass(t *testing.T) {
	f := fixture(t)
	b, err := json.Marshal(goalactor.Content{Owner: "alice", SessionUID: "other-session", ClassUID: "class-uid", InboundRef: "r1"})
	require.NoError(t, err)
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:channelsd"), "system:channelsd")
	_, err = f.signed.Put(ctx, memory.Entry{Scope: memory.Scope{Kind: "session", ID: "team/session"}, Kind: goalactor.KindName, ID: "goalactor-stale", CreatedAt: time.Now().UTC(), Content: b})
	require.NoError(t, err)
	_, err = f.s.actorForTurn(operatorCtx(), "team", "session", "r1")
	assert.ErrorIs(t, err, core.ErrDenied)
}

func TestActorForTurnRefusesForgedAttestation(t *testing.T) {
	f := fixture(t)
	// A store with no write verifier, so a forged channelsd envelope lands
	// and the server's own signature check is what must refuse it.
	raw := memory.NewLocal(meminmem.NewBackend())
	f.s.Memory = raw
	b, err := json.Marshal(goalactor.Content{Owner: "mallory", SessionUID: "session-uid", ClassUID: "class-uid", InboundRef: "r1"})
	require.NoError(t, err)
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:channelsd"), "system:channelsd")
	var keyID string
	for ref := range f.keys {
		keyID = ref.KeyID
	}
	_, err = raw.Put(ctx, memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: "team/session"}, Kind: goalactor.KindName, ID: "goalactor-forged",
		CreatedAt: time.Now().UTC(), Content: b,
		Provenance: &memory.Provenance{Publisher: "system:channelsd", KeyID: keyID, Seq: 1, Sig: []byte("forged")},
	})
	require.NoError(t, err)
	_, err = f.s.actorForTurn(operatorCtx(), "team", "session", "r1")
	require.Error(t, err)
	assert.ErrorIs(t, err, memory.ErrBadProvenance, "refused by the signature check, not merely unmatched")
	assert.Contains(t, err.Error(), "signature invalid")
}

func TestPrivateActorUnchangedByInboundRefs(t *testing.T) {
	f := fixture(t)
	f.turnProof(t, "alice", "r1")
	f.turnProof(t, "alice", "r2")
	a, err := f.s.actor(operatorCtx(), "team", "session")
	require.NoError(t, err)
	assert.Equal(t, "alice", a.Domain.Owner)
	f.turnProof(t, "bob", "r3")
	_, err = f.s.actor(operatorCtx(), "team", "session")
	assert.ErrorIs(t, err, core.ErrDenied, "private scope still pins the first human")
}
