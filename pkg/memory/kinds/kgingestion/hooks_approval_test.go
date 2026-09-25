package kgingestion_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/kgingestion"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// recordingMemory is a minimal memory.Memory fake whose Query captures the
// ctx it was called with, so the test can assert on what approvals that ctx
// carries. Put/Search/SendSignal are unused by kgingestion's hooks and are
// stubbed out.
type recordingMemory struct {
	queryCtx context.Context
}

func (r *recordingMemory) Put(_ context.Context, e memory.Entry) (memory.Entry, error) {
	return e, nil
}

func (r *recordingMemory) Query(ctx context.Context, _ memory.Query) (memory.QueryResult, error) {
	r.queryCtx = ctx
	return memory.QueryResult{}, nil
}

func (r *recordingMemory) Search(_ context.Context, _ memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{}, nil
}

func (r *recordingMemory) SendSignal(_ context.Context, _ memory.Signal) error {
	return nil
}

// TestOnSignal_InjectsSystemApproval_BeforeQuery drives the turn.completed
// handler directly against a hooks value built from a recording fake
// memory.Memory, and asserts that by the time the handler reaches
// h.mem.Query (via turnContent), the ctx it received already carries a
// system approval for ReadMemory. Without the injection in OnSignal, the
// captured ctx carries no approval and EnsureApproval fails closed.
func TestOnSignal_InjectsSystemApproval_BeforeQuery(t *testing.T) {
	memory.ResetRegistryForTest()
	t.Cleanup(kgingestion.Teardown)
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(kgingestion.Kind{})

	rec := &recordingMemory{}
	kg := &fakeKGProvider{}
	kgingestion.Setup(rec, kg, kgingestion.IngestionConfig{})

	scope := memory.Scope{Kind: "session", ID: "ns/test"}
	scopeHooks := kgingestion.Kind{}.NewScopeHooks(scope)

	payload, err := json.Marshal(map[string]string{"turnIndex": "0", "role": "assistant"})
	require.NoError(t, err)

	err = scopeHooks.OnSignal(context.Background(), memory.Signal{
		Kind: lifecycle.SigTurnCompleted, Scope: scope, At: time.Now(), Payload: payload,
	})
	require.NoError(t, err)

	require.NotNil(t, rec.queryCtx, "handleTurn must call h.mem.Query")
	assert.NoError(t, memory.EnsureApproval(rec.queryCtx, memory.ReadMemory, scope.ID),
		"kgingestion handler must inject a system approval before Query")
}
