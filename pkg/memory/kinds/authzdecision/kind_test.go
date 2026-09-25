package authzdecision_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
)

func TestAuthzDecision_Registered(t *testing.T) {
	k, ok := memory.LookupKind("authz_decision")
	require.True(t, ok)
	assert.Equal(t, "authzd-", k.IDPrefix())
}

func TestAuthzDecision_RecordAndQuery(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, authzdecision.Record(ctx, m, scope, "tu-1", authzdecision.Decision{
		Outcome: "denied", Subject: "alice@example.com",
		ResourceType: "github_repo", ResourceID: "authzed/spicedb",
		Permission: "write", Message: "no write perm",
	}))
	require.NoError(t, authzdecision.Record(ctx, m, scope, "tu-2", authzdecision.Decision{
		Outcome: "allowed", Subject: "alice@example.com",
		ResourceType: "github_repo", ResourceID: "authzed/spicedb",
		Permission: "read",
	}))

	denies, err := authzdecision.DeniesForResource(ctx, m, scope, "github_repo", "authzed/spicedb")
	require.NoError(t, err)
	require.Len(t, denies, 1)
	assert.Equal(t, "denied", denies[0].Outcome)
	assert.Equal(t, "no write perm", denies[0].Message)
}

// TestDeniesForResource_CorruptEntrySurfacesError asserts that a denial row
// whose content won't decode is NOT silently dropped from the deny set.
// DeniesForResource gates the asked-but-denied replay guard, so a
// corrupt/tampered append-only entry must surface as a hard error — if it
// silently vanished, the guard would re-open the very call it denied. The
// corrupt entry is written straight to the backend, bypassing the facade's
// Put validation, to mimic on-disk tampering of an already-persisted row.
func TestDeniesForResource_CorruptEntrySurfacesError(t *testing.T) {
	backend := inmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, backend.Put(ctx, memory.Entry{
		Scope: scope,
		Kind:  authzdecision.Kind{}.Name(),
		ID:    memory.NewID(authzdecision.Kind{}),
		Links: []memory.Link{
			{Relation: "for_resource", Kind: "github_repo", ID: "authzed/spicedb"},
		},
		Tags:    []string{"outcome:denied"},
		Content: []byte(`{"outcome":"denied", this is not valid json`),
	}), "seed corrupt denial row directly into backend")

	m := memory.NewLocal(backend)
	_, err := authzdecision.DeniesForResource(ctx, m, scope, "github_repo", "authzed/spicedb")
	require.Error(t, err, "a corrupt denial row must surface as an error, not silently drop")
	assert.ErrorContains(t, err, "DeniesForResource")
}

// TestAuthzDecision_PinFieldsRoundTrip verifies that the pin-provenance fields
// on Decision survive a JSON marshal/unmarshal cycle (the memory layer stores
// them as JSON).
func TestAuthzDecision_PinFieldsRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		d    authzdecision.Decision
	}{
		{
			name: "all pin fields set",
			d: authzdecision.Decision{
				Outcome:         "allowed",
				Subject:         "alice@example.com",
				ResourceType:    "github_repo",
				Permission:      "read",
				PinKind:         "mcp",
				PinName:         "my-mcp-server",
				PinStrength:     "frozen",
				PinDigest:       "sha256:deadbeef",
				PinVersion:      "v1.0.0",
				PinDrifted:      true,
				PinBypassReason: "dev exemption",
			},
		},
		{
			name: "no pin fields: omitempty serializes cleanly",
			d: authzdecision.Decision{
				Outcome:      "denied",
				Subject:      "bob@example.com",
				ResourceType: "github_repo",
				Permission:   "write",
				Message:      "no write perm",
			},
		},
		{
			name: "image pin, no drift",
			d: authzdecision.Decision{
				Outcome:     "allowed",
				Subject:     "carol@example.com",
				Permission:  "exec",
				PinKind:     "image",
				PinName:     "my-sidecar",
				PinStrength: "named",
				PinDigest:   "sha256:imgabc",
				PinVersion:  "latest",
				PinDrifted:  false,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.d)
			require.NoError(t, err, "marshal")

			var got authzdecision.Decision
			require.NoError(t, json.Unmarshal(b, &got), "unmarshal")

			assert.Equal(t, tc.d, got)

			// When no pin fields are set, the JSON must NOT contain any "pin" keys.
			if tc.d.PinKind == "" {
				assert.NotContains(t, string(b), "pinKind")
				assert.NotContains(t, string(b), "pinDrifted")
			}
		})
	}
}
