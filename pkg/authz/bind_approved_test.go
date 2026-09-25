package authz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

type conflictingScopeMemory struct {
	memory.Memory
	scope              memory.Scope
	conflictsRemaining int
	queryCalls         int
	putCalls           int
	putErr             error
}

type approvedRecordingWriter struct{ wrote []Relation }

func (w *approvedRecordingWriter) WriteRelationships(_ context.Context, rels []Relation) error {
	w.wrote = append(w.wrote, rels...)
	return nil
}

func (*approvedRecordingWriter) DeleteRelationships(context.Context, []Relation) error { return nil }

func (m *conflictingScopeMemory) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	m.queryCalls++
	// bindSlots reads once to compute its delta, then PutIfVersion reads again
	// to compare. Change the document immediately before that comparison so the
	// caller observes the same optimistic-concurrency loss as two live writers.
	if m.conflictsRemaining > 0 && m.queryCalls%2 == 0 {
		cur, found, err := sessionscope.Get(ctx, m.Memory, m.scope)
		if err != nil {
			return memory.QueryResult{}, err
		}
		if !found {
			cur = scope.Scope{}
		}
		cur = scope.ApplyDelta(cur, scope.ScopeDelta{
			HardDeny: scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "git_repo", ID: "blocked/repo"}}},
		}, scope.SourceApproverDenyConv, time.Unix(1700000000, 0).UTC())
		if err := sessionscope.Put(ctx, m.Memory, m.scope, cur); err != nil {
			return memory.QueryResult{}, err
		}
		m.conflictsRemaining--
	}
	return m.Memory.Query(ctx, q)
}

func (m *conflictingScopeMemory) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	m.putCalls++
	if m.putErr != nil {
		return memory.Entry{}, m.putErr
	}
	return m.Memory.Put(ctx, e)
}

// A human approving a slot must NARROW the session, not merely permit one more
// instance. Approving acme/app excludes every other git_repo — the same
// property a class `default` has had all along, and the reason the weaker
// config path was deterministic while the stronger human one was not.
func TestBindApproved_narrowsScopeToTheApprovedInstance(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/s"}
	sess := SessionRef{Namespace: "ns", Name: "s"}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	err := BindApproved(ctx, mem, memScope, sess, nil,
		[]SlotBinding{{ResourceType: "git_repo", ResourceID: TrustedObjectID("acme/app"), Permission: "push"}},
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now)
	require.NoError(t, err, "a nil writer narrows scope and grants nothing; it must not error")

	got, _, err := sessionscope.Get(memory.WithSystemApproval(ctx, "test"), mem, memScope)
	require.NoError(t, err)

	var ids []string
	for _, r := range got.Resources {
		if r.ResourceType == "git_repo" {
			ids = append(ids, r.IDs...)
		}
	}
	assert.Equal(t, []string{"acme/app"}, ids,
		"once a type is in scope.Resources every ref of that type must match one, "+
			"so this is what makes a second repository denied rather than merely ungranted")
}

// The source is recorded, and it is its own value. Recording an approval as
// `default` or `extracted` would misattribute a human decision to config or to
// text the agent read — the audit trail is the only place that distinction
// survives.
func TestBindApproved_recordsApprovedAsItsOwnSource(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/s"}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	require.NoError(t, BindApproved(ctx, mem, memScope,
		SessionRef{Namespace: "ns", Name: "s"}, nil,
		[]SlotBinding{{ResourceType: "git_repo", ResourceID: TrustedObjectID("acme/app"), Permission: "push"}},
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now))

	got, _, err := sessionscope.Get(memory.WithSystemApproval(ctx, "test"), mem, memScope)
	require.NoError(t, err)
	require.NotEmpty(t, got.Resources)
	assert.Equal(t, scope.SourceApproved, got.Resources[0].Source)
}

// Empty is a no-op, not an error: a decision that granted nothing (every slot
// held back for its owner) must not fail the turn.
func TestBindApproved_emptyBindingsIsANoOp(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	assert.NoError(t, BindApproved(context.Background(), mem, memory.Scope{Kind: "session", ID: "ns/s"},
		SessionRef{Namespace: "ns", Name: "s"}, nil, nil,
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now))
}

func TestBindApproved_retriesScopeVersionConflictAndMergesConcurrentDeny(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	memScope := memory.Scope{Kind: "session", ID: "ns/s"}
	mem := &conflictingScopeMemory{
		Memory:             memory.NewLocal(inmem.NewBackend()),
		scope:              memScope,
		conflictsRemaining: 1,
	}
	w := &approvedRecordingWriter{}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	require.NoError(t, BindApproved(ctx, mem, memScope,
		SessionRef{Namespace: "ns", Name: "s"}, w,
		[]SlotBinding{{ResourceType: "git_repo", ResourceID: TrustedObjectID("acme/app"), Permission: "push"}},
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now))

	got, _, err := sessionscope.Get(ctx, mem.Memory, memScope)
	require.NoError(t, err)
	assert.True(t, got.ResourceDisallowed("git_repo", "blocked/repo"),
		"the retry must re-read and preserve the concurrent hard deny")
	assert.Equal(t, 4, mem.queryCalls, "one lost compare requires exactly one re-read/recompute")
	assert.Len(t, w.wrote, 1, "grant only after the merged scope write succeeds")
}

func TestBindApproved_exhaustsScopeVersionConflictsWithoutGranting(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	memScope := memory.Scope{Kind: "session", ID: "ns/s"}
	mem := &conflictingScopeMemory{
		Memory:             memory.NewLocal(inmem.NewBackend()),
		scope:              memScope,
		conflictsRemaining: 100,
	}
	w := &approvedRecordingWriter{}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	err := BindApproved(ctx, mem, memScope,
		SessionRef{Namespace: "ns", Name: "s"}, w,
		[]SlotBinding{{ResourceType: "git_repo", ResourceID: TrustedObjectID("acme/app"), Permission: "push"}},
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now)

	assert.ErrorIs(t, err, sessionscope.ErrVersionConflict)
	assert.Equal(t, 6, mem.queryCalls, "the retry bound is three read/compare attempts")
	assert.Equal(t, 97, mem.conflictsRemaining)
	assert.Empty(t, w.wrote, "a scope write that never wins must never create a grant")
}

func TestBindApproved_doesNotRetryUnrelatedScopeWriteError(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	memScope := memory.Scope{Kind: "session", ID: "ns/s"}
	errStore := errors.New("memory unavailable")
	mem := &conflictingScopeMemory{
		Memory: memory.NewLocal(inmem.NewBackend()),
		scope:  memScope,
		putErr: errStore,
	}
	w := &approvedRecordingWriter{}
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	err := BindApproved(ctx, mem, memScope,
		SessionRef{Namespace: "ns", Name: "s"}, w,
		[]SlotBinding{{ResourceType: "git_repo", ResourceID: TrustedObjectID("acme/app"), Permission: "push"}},
		EnforcePreconditions, now().Add(time.Hour), logr.Discard(), now)

	assert.ErrorIs(t, err, errStore)
	assert.Equal(t, 1, mem.putCalls, "only version conflicts are retryable")
	assert.Empty(t, w.wrote, "a failed scope write must never create a grant")
}
