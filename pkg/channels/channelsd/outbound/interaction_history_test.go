package outbound

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/interactionhistory"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/stretchr/testify/require"
)

func TestInteractionHistorySurvivesRestartAndNeverStoresCredentialLinks(t *testing.T) {
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:channelsd"), "system:channelsd")
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := memsqlite.NewClient(path)
	require.NoError(t, err)
	require.NoError(t, db.Migrate(ctx))
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	signer := provenance.NewSigner(priv, "system:channelsd")
	keys := provenance.MapKeyLookup{{Publisher: signer.Publisher(), KeyID: signer.KeyID()}: priv.Public().(ed25519.PublicKey)}
	facade := func(db *memsqlite.Client) memory.Memory {
		return memory.NewLocal(memsqlite.NewBackend(db), memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
	}
	signed := provenance.NewSigningMemory(facade(db), signer)
	source := channelevents.SessionRef{Namespace: "demo", Name: "child"}
	destination := memory.Scope{Kind: "session", ID: "demo/parent"}
	request := channelevents.InteractionRequestPayload{AgentSessionRef: source, Category: "plan_phase", RequestRef: "review", Lead: "Exact original approval", Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants}, Actions: []channelevents.InteractionAction{{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision}}}
	env, err := channelevents.BuildEnvelope(source.Namespace, source.Name, channelevents.KindInteractionRequest, request)
	require.NoError(t, err)
	require.NoError(t, noteInteractionHistory(ctx, signed, destination, "child-uid", env))
	require.NoError(t, noteInteractionHistory(ctx, signed, destination, "child-uid", env), "identical replay is append-only idempotent")
	applied := channelevents.InteractionAppliedPayload{AgentSessionRef: source, Category: request.Category, RequestRef: request.RequestRef, Outcome: channelevents.OutcomeApproved, Reason: "Exact original reason", ResponseRef: "https://secret.example.test/callback", MintedURL: "https://secret.example.test/minted"}
	env, err = channelevents.BuildEnvelope(source.Namespace, source.Name, channelevents.KindInteractionApplied, applied)
	require.NoError(t, err)
	require.NoError(t, noteInteractionHistory(ctx, signed, destination, "child-uid", env))
	request.Category = "credential_link"
	request.RequestRef = "credential"
	env, err = channelevents.BuildEnvelope(source.Namespace, source.Name, channelevents.KindInteractionRequest, request)
	require.NoError(t, err)
	require.NoError(t, noteInteractionHistory(ctx, signed, destination, "child-uid", env))
	require.NoError(t, db.Close())
	db, err = memsqlite.NewClient(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	result, err := facade(db).Query(ctx, memory.Query{Scope: destination, Kinds: []string{interactionhistory.KindName}})
	require.NoError(t, err)
	require.Len(t, result.Entries, 2)
	for _, e := range result.Entries {
		require.Equal(t, "system:channelsd", e.Provenance.Publisher)
		var c interactionhistory.Content
		require.NoError(t, json.Unmarshal(e.Content, &c))
		require.Equal(t, source, c.Source)
		if c.Applied != nil {
			require.Equal(t, "approved", c.Applied.Outcome)
			require.Equal(t, applied.Reason, c.Applied.Reason)
			require.Empty(t, c.Applied.ResponseRef)
			require.Empty(t, c.Applied.MintedURL)
		}
	}
}
