package plangate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// A root written BEFORE DelegatedFrom existed carries the same fact only in its
// Provenance sentence. This log is append-only, so those records are permanent
// — and reading one as "not inherited" would leave exactly the sessions that
// were already delegated when the field landed ungated, which is the bug rather
// than a tidy-up of it.
//
// The field stays authoritative for everything written from here on; this is a
// floor under records that predate it, not a second spelling. Same shape as the
// fold's PhaseKey → (PlanDigest, PhaseIndex) fallback.
func TestHasInheritedCeiling_TrueForAPreFieldRootViaProvenance(t *testing.T) {
	mem := memory.NewLocal(memoryinmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	scope := memory.Scope{Kind: "session", ID: "demo/legacy-child"}

	require.NoError(t, plangateaudit.Record(ctx, mem, scope, plangateaudit.Content{
		Event:      plangateaudit.EventPlanApproved,
		Provenance: "delegated from demo/parent phase(s) [0]",
		PlanDigest: "sha256:whatever",
	}))

	got, err := HasInheritedCeiling(ctx, mem, scope)
	require.NoError(t, err)
	assert.True(t, got, "a pre-field root still names itself delegated in prose")
}

// A session's own approval carries no such sentence, so the fallback does not
// widen into "any record with a provenance string".
func TestHasInheritedCeiling_ProvenanceFallbackDoesNotMatchAnOwnApproval(t *testing.T) {
	mem := memory.NewLocal(memoryinmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	scope := memory.Scope{Kind: "session", ID: "demo/own-with-provenance"}

	require.NoError(t, plangateaudit.Record(ctx, mem, scope, plangateaudit.Content{
		Event:      plangateaudit.EventPlanApproved,
		Provenance: "approved by alice@example.com",
		PlanDigest: "sha256:whatever",
	}))

	got, err := HasInheritedCeiling(ctx, mem, scope)
	require.NoError(t, err)
	assert.False(t, got)
}
