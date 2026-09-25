package authz_session_config_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	asc "github.com/authzed/openagentprimitives/pkg/memory/kinds/authz_session_config"
)

func TestAuthzSessionConfig_Registered(t *testing.T) {
	k, ok := memory.LookupKind("authz_session_config")
	require.True(t, ok)
	assert.Equal(t, "asc-", k.IDPrefix())
}

func TestAuthzSessionConfig_SnapshotAndGet(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	cfg := asc.Content{
		BoundEntities: []spiceboxv1alpha1.BoundEntityType{
			{ResourceType: "github_repo", Description: "repos", Permission: "read"},
		},
		Subject: "alice",
	}
	require.NoError(t, asc.Snapshot(ctx, m, scope, cfg))
	got, ok, err := asc.Get(ctx, m, scope)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "alice", got.Subject)
	require.Len(t, got.BoundEntities, 1)
	assert.Equal(t, "github_repo", got.BoundEntities[0].ResourceType)
}

// TestAuthzSessionConfig_MultiEntityAndPromptsRoundTrip covers the parts of
// Content that a single-entity snapshot cannot: that BoundEntities survives
// with more than one element in the order it was written, and that
// PerToolPrompts — a map[string][]string, the only non-scalar shape in the
// record — round-trips key and value.
//
// PerToolPrompts has no production writer today (the operator leaves it unset;
// see reconcileAuthzSessionConfig), but authzd's extraction pipeline reads it
// off this record, so the serialization must keep working for whoever populates
// it. Nothing else in the repo asserts either property.
func TestAuthzSessionConfig_MultiEntityAndPromptsRoundTrip(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/multi"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, asc.Snapshot(ctx, m, scope, asc.Content{
		BoundEntities: []spiceboxv1alpha1.BoundEntityType{
			{ResourceType: "github_repo", Description: "GitHub repos", Permission: "read"},
			{ResourceType: "slack_channel", Description: "Slack channels", Permission: "write"},
		},
		PerToolPrompts: map[string][]string{
			"gh_pr_view": {"Search for pull requests by their number."},
		},
		Subject: "bob",
	}))

	got, ok, err := asc.Get(ctx, m, scope)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, got.BoundEntities, 2)
	assert.Equal(t, "github_repo", got.BoundEntities[0].ResourceType, "element order preserved")
	assert.Equal(t, "slack_channel", got.BoundEntities[1].ResourceType, "element order preserved")
	require.Contains(t, got.PerToolPrompts, "gh_pr_view")
	assert.Equal(t, []string{"Search for pull requests by their number."}, got.PerToolPrompts["gh_pr_view"])
}

// TestAuthzSessionConfig_OverwriteReplacesListsRatherThanAppending pins the
// half of stable-ID upsert that the scalar overwrite tests cannot see: a slice
// field must be REPLACED by the second write, not accumulated across both. A
// record that appended would grow the session's bound-entity set on every
// reconcile.
func TestAuthzSessionConfig_OverwriteReplacesListsRatherThanAppending(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/overwrite"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, asc.Snapshot(ctx, m, scope, asc.Content{
		BoundEntities: []spiceboxv1alpha1.BoundEntityType{{ResourceType: "github_repo", Permission: "read"}},
		Subject:       "alice",
	}))
	require.NoError(t, asc.Snapshot(ctx, m, scope, asc.Content{
		BoundEntities: []spiceboxv1alpha1.BoundEntityType{{ResourceType: "slack_channel", Permission: "write"}},
		Subject:       "carol",
	}))

	got, ok, err := asc.Get(ctx, m, scope)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "carol", got.Subject)
	require.Len(t, got.BoundEntities, 1, "the second snapshot replaces the list; it must not append to it")
	assert.Equal(t, "slack_channel", got.BoundEntities[0].ResourceType)
}

func TestAuthzSessionConfig_Idempotent(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")
	require.NoError(t, asc.Snapshot(ctx, m, scope, asc.Content{Subject: "alice"}))
	require.NoError(t, asc.Snapshot(ctx, m, scope, asc.Content{Subject: "bob"}))
	got, ok, err := asc.Get(ctx, m, scope)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "bob", got.Subject, "second Snapshot replaces first")
}

func TestAuthzSessionConfig_Absent(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	_, ok, err := asc.Get(memory.WithSystemApproval(context.Background(), "test"), m, scope)
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestAuthzSessionConfig_SessionCredentialMayNotAuthorIt is the security
// property this Kind exists to hold.
//
// authzd derives the session's cold-start authorization policy from this record
// (internal/cmd/authzd/cold_start_policy.go reads scopeEnabled + coldStart). If the
// session's own bearer can author it, a compromised runner chooses the gates
// that are applied to it — so the per-session credential must be refused at the
// memory facade's per-kind write door, and the operator must be the writer.
//
// memory.WithTokenSession is exactly the capability the memory HTTP handler
// attaches for a per-session bearer, so this is the same door a runner's write
// arrives at. Component tokens (authzd, channelsd) and the operator's
// in-process facade do not carry it and are unaffected.
func TestAuthzSessionConfig_SessionCredentialMayNotAuthorIt(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	sc := memory.Scope{Kind: "session", ID: "ns/a"}

	ctx := memory.WithSystemApproval(context.Background(), "test")
	ctx = memory.WithTokenSession(ctx, memory.NamespacedName{Namespace: "ns", Name: "a"})

	err := asc.Snapshot(ctx, m, sc, asc.Content{
		Subject: "alice", ScopeEnabled: true, ColdStart: "extractAndAutoApply",
	})
	require.ErrorIs(t, err, memory.ErrKindNotSessionWritable,
		"a per-session credential must not author the record its own cold-start gate is derived from")

	_, found, gerr := asc.Get(memory.WithSystemApproval(context.Background(), "test"), m, sc)
	require.NoError(t, gerr)
	assert.False(t, found, "the refused write must not have landed")
}

// TestAuthzSessionConfig_OperatorInProcessWriteStillWorks is the other half:
// tightening the door must not lock out the legitimate writer. The operator
// writes through the facade in-process, with no token-session capability.
func TestAuthzSessionConfig_OperatorInProcessWriteStillWorks(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	sc := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "operator:agentsession-controller")

	require.NoError(t, asc.Snapshot(ctx, m, sc, asc.Content{ScopeEnabled: true, ColdStart: "extractAndApprove"}),
		"the operator's in-process write must still be permitted")

	got, found, err := asc.Get(ctx, m, sc)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "extractAndApprove", got.ColdStart)
}

func TestAuthzSessionConfig_ColdStartFields(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sc := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, asc.Snapshot(ctx, m, sc, asc.Content{
		Subject: "alice", ScopeEnabled: true, ColdStart: "extractAndApprove",
	}))
	got, ok, err := asc.Get(ctx, m, sc)
	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, got.ScopeEnabled)
	assert.Equal(t, "extractAndApprove", got.ColdStart)
}
