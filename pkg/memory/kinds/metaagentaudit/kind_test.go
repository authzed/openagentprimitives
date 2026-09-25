package metaagentaudit_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/metaagentaudit"
)

func TestMetaagentAudit_Registered(t *testing.T) {
	k, ok := memory.LookupKind("metaagent_audit")
	require.True(t, ok)
	assert.Equal(t, "maud-", k.IDPrefix())
	assert.Equal(t, reflect.TypeOf(metaagentaudit.Content{}), k.ContentSchema())
}

func TestMetaagentAudit_RetentionSignals(t *testing.T) {
	k, _ := memory.LookupKind("metaagent_audit")
	r := k.Retention()
	assert.Contains(t, r.ArchiveOn, lifecycle.SigSessionCompleted)
	assert.Contains(t, r.ArchiveOn, lifecycle.SigSessionFailed)
	assert.True(t, r.TTLAfterArchive > 0)
}

func TestMetaagentAudit_RecordAndList(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sc := memory.Scope{Kind: "session", ID: "ns/a"}

	delta := scope.ScopeDelta{
		Add: scope.ScopePartial{Tools: []string{"github.list_issues"}},
	}
	c := metaagentaudit.Content{
		Ts:                 time.Now(),
		Requester:          "user:alice",
		Approver:           "user:bob",
		RequestText:        "please add github issue access",
		ExtractorLLMModel:  "gemini-2.5-pro",
		ExtractorLatencyMs: 312,
		ProposedDelta:      delta,
		Classification: metaagentaudit.Classification{
			Applied: delta,
			Skipped: []scope.SkippedItem{{RequestFragment: "tool x", Reason: scope.ReasonOutOfEnvelopeTool}},
			Caveats: []scope.CaveatItem{{AppliedFragment: "disallow read of repo:foo", Category: scope.CaveatSearchToolCanReturn}},
		},
		ComposerLLMModel:  "gemini-2.5-flash",
		ComposerLatencyMs: 140,
		ComposerOutput: metaagentaudit.ComposerOutput{
			ApproverSummary: "Grant access to github issues",
			SkippedExplain:  "Tool x is out of envelope",
		},
		ApproverDecision: "approved",
		AppliedDelta:     &delta,
	}
	require.NoError(t, metaagentaudit.Record(ctx, m, sc, c))

	records, err := metaagentaudit.List(ctx, m, sc)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "user:alice", records[0].Requester)
	assert.Equal(t, "user:bob", records[0].Approver)
	assert.Equal(t, "approved", records[0].ApproverDecision)
	assert.Equal(t, "gemini-2.5-pro", records[0].ExtractorLLMModel)
	assert.Len(t, records[0].Classification.Skipped, 1)
	assert.Len(t, records[0].Classification.Caveats, 1)
	assert.NotNil(t, records[0].AppliedDelta)
}

func TestMetaagentAudit_EmptyListReturnsEmpty(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	sc := memory.Scope{Kind: "session", ID: "fresh"}
	records, err := metaagentaudit.List(memory.WithSystemApproval(context.Background(), "test"), m, sc)
	require.NoError(t, err)
	assert.Empty(t, records)
	assert.NotNil(t, records)
}

func TestMetaagentAudit_AutoTs(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sc := memory.Scope{Kind: "session", ID: "ns/auto"}
	// Ts zero → auto-set to now.
	require.NoError(t, metaagentaudit.Record(ctx, m, sc, metaagentaudit.Content{Requester: "user:alice"}))
	records, err := metaagentaudit.List(ctx, m, sc)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.False(t, records[0].Ts.IsZero())
}

func TestMetaagentAudit_RequestByID_RoundTrip(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, metaagentaudit.RecordRequested(ctx, m, scope, metaagentaudit.Content{
		RequestID:   "metaagent-scope-ns/a-123",
		Requester:   "U_ALICE",
		RequestText: "please widen scope to repo X",
		ColdStart:   true,
		CleanedTask: "summarize repo X",
		ComposerOutput: metaagentaudit.ComposerOutput{
			ApproverSummary: "adds read access to repo X",
		},
	}), "RecordRequested")

	got, err := metaagentaudit.RequestByID(ctx, m, scope, "metaagent-scope-ns/a-123")
	require.NoError(t, err)
	require.NotNil(t, got, "request record must be found by request id")
	assert.Equal(t, "U_ALICE", got.Requester)
	assert.Equal(t, "please widen scope to repo X", got.RequestText)
	assert.True(t, got.ColdStart)
	assert.Equal(t, "summarize repo X", got.CleanedTask)
	assert.Equal(t, "adds read access to repo X", got.ComposerOutput.ApproverSummary)
}

func TestMetaagentAudit_RequestByID_Miss(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	got, err := metaagentaudit.RequestByID(memory.WithSystemApproval(context.Background(), "test"), m,
		memory.Scope{Kind: "session", ID: "ns/a"}, "nope")
	require.NoError(t, err, "a miss is not an error")
	assert.Nil(t, got)
}
