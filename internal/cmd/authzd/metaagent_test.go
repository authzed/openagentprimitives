package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/metaagentthread"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/scopeaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"

	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

// failingPutBackend wraps a memory.Backend and fails every Put, so a test can
// exercise the memory-write-failure error path of ApplyScopeChange.
type failingPutBackend struct{ memory.Backend }

func (failingPutBackend) Put(_ context.Context, _ memory.Entry) error {
	return errors.New("backend put unavailable")
}

// captureSlog installs a capturing slog handler as the process-wide default
// for the duration of the test and restores the previous default on cleanup.
// Tests using it must not run in parallel with each other or with anything
// else touching the slog default — no test in this package calls t.Parallel(),
// so Go's default sequential execution is what keeps this safe.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestApplyScopeChange_EmptyDelta_NoOp(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{Memory: m}

	scopeRef := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, mg.ApplyScopeChange(approvedCtx(), scopeRef, "ns/a", scope.ScopeDelta{}))

	got, found, err := sessionscope.Get(approvedCtx(), m, scopeRef)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, scope.Scope{}, got)
}

func TestApplyScopeChange_HardDenyResources_LandsInMemoryDisallow(t *testing.T) {
	// Hard-deny lives ENTIRELY in the Layer-2 session_scope memory Disallow set,
	// which the Scope hook reads at dispatch. This pins that ApplyScopeChange
	// records the deny in memory.
	m := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{Memory: m}

	scopeRef := memory.Scope{Kind: "session", ID: "ns/a"}
	d := scope.ScopeDelta{
		HardDeny: scope.ScopePartial{
			Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "bad/repo"}},
		},
	}
	require.NoError(t, mg.ApplyScopeChange(approvedCtx(), scopeRef, "ns/a", d))

	got, found, err := sessionscope.Get(approvedCtx(), m, scopeRef)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, int64(1), got.ScopeVersion)
	assert.True(t, got.ResourceDisallowed("github_repo", "bad/repo"),
		"hard-deny must land in the Layer-2 memory Disallow set")
}

func TestApplyScopeChange_HardDenyGlobID_LandsInMemoryDisallow(t *testing.T) {
	// Both glob and concrete IDs are enforced at Layer 2 (memory Disallow set).
	// There is no longer a SpiceDB tuple for either.
	m := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{Memory: m}

	scopeRef := memory.Scope{Kind: "session", ID: "ns/a"}
	d := scope.ScopeDelta{
		HardDeny: scope.ScopePartial{
			Resources: []scope.ResourceRef{
				{ResourceType: "linear_issue", ID: "ENG-*"},   // glob
				{ResourceType: "linear_issue", ID: "SEC-123"}, // concrete
			},
		},
	}
	require.NoError(t, mg.ApplyScopeChange(approvedCtx(), scopeRef, "ns/a", d))

	got, found, err := sessionscope.Get(approvedCtx(), m, scopeRef)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, got.ResourceDisallowed("linear_issue", "ENG-369"), "glob enforced at Layer 2")
	assert.True(t, got.ResourceDisallowed("linear_issue", "SEC-123"), "concrete enforced at Layer 2")
}

func TestApplyScopeChange_HardDenyTools_LandsInMemoryToolDeny(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{Memory: m}

	scopeRef := memory.Scope{Kind: "session", ID: "ns/a"}
	d := scope.ScopeDelta{
		HardDeny: scope.ScopePartial{Tools: []string{"github.delete_repo"}},
	}
	require.NoError(t, mg.ApplyScopeChange(approvedCtx(), scopeRef, "ns/a", d))

	got, found, err := sessionscope.Get(approvedCtx(), m, scopeRef)
	require.NoError(t, err)
	require.True(t, found)
	assert.Contains(t, got.Tools.Deny, "github.delete_repo",
		"hard-deny tool must land in the Layer-2 memory tool-deny set")
}

func TestApplyScopeChange_Widening_AdvancesMemoryScope(t *testing.T) {
	// Widening (Add) advances the in-scope Resources set in memory. There is no
	// longer a SpiceDB revoke step — there are no disallow tuples to revoke.
	m := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{Memory: m}

	scopeRef := memory.Scope{Kind: "session", ID: "ns/a"}
	d := scope.ScopeDelta{
		Add: scope.ScopePartial{
			Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}},
		},
	}
	require.NoError(t, mg.ApplyScopeChange(approvedCtx(), scopeRef, "ns/a", d))

	got, found, err := sessionscope.Get(approvedCtx(), m, scopeRef)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, got.Resources, 1)
	assert.Equal(t, []string{"foo/bar"}, got.Resources[0].IDs)
}

func TestApplyScopeChange_ConservativeOrdering_MemoryWriteFailureSurfacesError(t *testing.T) {
	// Memory is the only store, so a memory write failure must surface as an
	// error rather than a silent partial apply. A read-only backend fails the
	// sessionscope.Put.
	m := memory.NewLocal(failingPutBackend{inmem.NewBackend()})
	mg := &Metaagent{Memory: m}

	scopeRef := memory.Scope{Kind: "session", ID: "ns/a"}
	d := scope.ScopeDelta{
		HardDeny: scope.ScopePartial{
			Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "bad/repo"}},
		},
	}
	err := mg.ApplyScopeChange(approvedCtx(), scopeRef, "ns/a", d)
	require.Error(t, err, "a memory write failure must surface as an error")
	assert.Contains(t, err.Error(), "write scope")
}

func TestApplyScopeChange_Idempotent(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{Memory: m}

	scopeRef := memory.Scope{Kind: "session", ID: "ns/a"}
	d := scope.ScopeDelta{
		Add: scope.ScopePartial{
			Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}},
		},
	}
	require.NoError(t, mg.ApplyScopeChange(approvedCtx(), scopeRef, "ns/a", d))
	require.NoError(t, mg.ApplyScopeChange(approvedCtx(), scopeRef, "ns/a", d))

	got, _, _ := sessionscope.Get(approvedCtx(), m, scopeRef)
	require.Len(t, got.Resources, 1)
	assert.Equal(t, []string{"foo/bar"}, got.Resources[0].IDs, "duplicate adds collapse")
	assert.Equal(t, int64(2), got.ScopeVersion, "version bumps each apply")
}

func TestApplyScopeChange_WritesAuditRecord(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{Memory: m}

	scopeRef := memory.Scope{Kind: "session", ID: "ns/a"}
	d := scope.ScopeDelta{
		Add: scope.ScopePartial{
			Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}},
		},
	}
	require.NoError(t, mg.ApplyScopeChange(approvedCtx(), scopeRef, "ns/a", d))

	records, err := scopeaudit.List(approvedCtx(), m, scopeRef)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, d, records[0].Delta)
}

// Fake LLM providers shared across the metaagent test files (the staged
// lifecycle tests, the worker round-trips, and the e2e scope scenarios).
type fakeMetaExtractor struct {
	result scope.ScopeDelta
	err    error
}

func (f *fakeMetaExtractor) Extract(_ context.Context, _ ExtractorInput) (scope.ScopeDelta, error) {
	if f.err != nil {
		return scope.ScopeDelta{}, f.err
	}
	return f.result, nil
}

type fakeMetaComposer struct {
	out ComposerOutput
	err error
}

func (f *fakeMetaComposer) Compose(_ context.Context, _ ComposerInput) (ComposerOutput, error) {
	if f.err != nil {
		return ComposerOutput{}, f.err
	}
	return f.out, nil
}

func TestMetaagentNotify_DeliversAndRecords(t *testing.T) {
	scopeRef := memory.Scope{Kind: "session", ID: "ns/a"}

	t.Run("NoticePublish set: invoked with requester+body and memory append happens", func(t *testing.T) {
		m := memory.NewLocal(inmem.NewBackend())
		var gotRequester, gotBody string
		var gotScope memory.Scope
		called := 0
		mg := &Metaagent{
			Memory: m,
			NoticePublish: func(_ context.Context, sr memory.Scope, requester, body string) error {
				called++
				gotScope, gotRequester, gotBody = sr, requester, body
				return nil
			},
		}

		mg.notify(approvedCtx(), scopeRef, "user:alice", "Scope change denied.")

		assert.Equal(t, 1, called, "NoticePublish must be invoked once")
		assert.Equal(t, "user:alice", gotRequester)
		assert.Equal(t, "Scope change denied.", gotBody)
		assert.Equal(t, scopeRef, gotScope)

		// Memory append still happens.
		msgs, err := metaagentthread.List(approvedCtx(), m, scopeRef)
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		assert.Equal(t, metaagentthread.RoleMetaagentNotice, msgs[0].Role)
		assert.Equal(t, "Scope change denied.", msgs[0].Body)
	})

	t.Run("append fails: the failure is logged with the session (no silent errors)", func(t *testing.T) {
		// The metaagent_thread append is the durable record that a notice was
		// produced. metaagentthread.Append -> auditaccessor.Append returns the
		// error and logs nothing of its own, so if notify drops it a failed
		// (or, in production, rejected-unsigned) audit write is invisible.
		logs := captureSlog(t)
		m := memory.NewLocal(failingPutBackend{inmem.NewBackend()})
		mg := &Metaagent{Memory: m} // NoticePublish nil: isolate the append

		mg.notify(approvedCtx(), scopeRef, "user:alice", "Scope change denied.")

		assert.Contains(t, logs.String(), "metaagent: notice thread append failed",
			"a failed metaagent_thread append must be logged, not dropped")
		assert.Contains(t, logs.String(), scopeRef.ID,
			"the log must name the session so an operator can locate the failure")
	})

	t.Run("NoticePublish nil: notify still appends to memory and does not panic", func(t *testing.T) {
		m := memory.NewLocal(inmem.NewBackend())
		mg := &Metaagent{Memory: m} // NoticePublish nil

		assert.NotPanics(t, func() {
			mg.notify(approvedCtx(), scopeRef, "user:alice", "memory only")
		})

		msgs, err := metaagentthread.List(approvedCtx(), m, scopeRef)
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		assert.Equal(t, "memory only", msgs[0].Body)
	})
}
