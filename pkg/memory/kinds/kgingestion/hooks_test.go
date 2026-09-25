package kgingestion_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/kgingestion"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	turnkind "github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

type fakeKGProvider struct {
	ingested []memory.KGInput
}

func (f *fakeKGProvider) Ingest(_ context.Context, input memory.KGInput) error {
	f.ingested = append(f.ingested, input)
	return nil
}
func (f *fakeKGProvider) SearchFacts(_ context.Context, _ string, _ int) ([]memory.KGFact, error) {
	return nil, nil
}
func (f *fakeKGProvider) GetEntity(_ context.Context, _ string) (*memory.KGEntity, error) {
	return nil, nil
}
func (f *fakeKGProvider) EntityFacts(_ context.Context, _ string) ([]memory.KGFact, error) {
	return nil, nil
}
func (f *fakeKGProvider) RelatedEntities(_ context.Context, _ string, _ int) ([]memory.KGEntity, error) {
	return nil, nil
}
func (f *fakeKGProvider) Communities(_ context.Context, _ string) ([]memory.KGCommunity, error) {
	return nil, nil
}

func setupTest(t *testing.T, strategy kgingestion.IngestionStrategy, minTokens int) (*memory.Local, *fakeKGProvider, memory.Scope) {
	t.Helper()
	// Reset the registry first so init()-registered Kinds are cleared,
	// then re-register the subset this test needs.
	memory.ResetRegistryForTest()
	t.Cleanup(kgingestion.Teardown)
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(kgingestion.Kind{})
	memory.RegisterKind(turnkind.Kind{})

	b := inmem.NewBackend()
	m := memory.NewLocal(b)
	kg := &fakeKGProvider{}
	kgingestion.Setup(m, kg, kgingestion.IngestionConfig{
		Strategy: strategy, MinContentLength: minTokens,
	})
	return m, kg, memory.Scope{Kind: "session", ID: "ns/test"}
}

func putTurn(t *testing.T, m *memory.Local, scope memory.Scope, index int, text string) {
	t.Helper()
	turnContent, err := json.Marshal(memory.Turn{
		Index: index, Role: "assistant",
		Content:   []memory.ContentBlock{{Type: "text", Text: text}},
		CreatedAt: time.Now(),
	})
	require.NoError(t, err)
	turnID := fmt.Sprintf("turn-%06d-assistant", index)
	_, err = m.Put(memory.WithSystemApproval(context.Background(), "test"), memory.Entry{
		Scope: scope, Kind: "turn", ID: turnID,
		Content: turnContent, CreatedAt: time.Now(),
	})
	require.NoError(t, err)
}

func emitTurnSignal(t *testing.T, m *memory.Local, scope memory.Scope, index int) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"turnIndex": strconv.Itoa(index), "role": "assistant",
	})
	require.NoError(t, err)
	err = m.SendSignal(memory.WithSystemApproval(context.Background(), "test"), memory.Signal{
		Kind: lifecycle.SigTurnCompleted, Scope: scope,
		At: time.Now(), Payload: payload,
	})
	require.NoError(t, err)
}

func TestEveryTurnStrategy(t *testing.T) {
	m, kg, scope := setupTest(t, kgingestion.StrategyEveryTurn, 0)
	putTurn(t, m, scope, 0, "Hello Bob works at TechCorp")
	emitTurnSignal(t, m, scope, 0)
	require.Len(t, kg.ingested, 1)
	assert.Contains(t, kg.ingested[0].Content, "Hello Bob")
	assert.Equal(t, "ns/test", kg.ingested[0].GroupID)
}

func TestContentGatedStrategy_SkipsShort(t *testing.T) {
	m, kg, scope := setupTest(t, kgingestion.StrategyContentGated, 100)
	putTurn(t, m, scope, 0, "ok")
	emitTurnSignal(t, m, scope, 0)
	assert.Empty(t, kg.ingested, "short content should be skipped")
}

func TestContentGatedStrategy_PassesLong(t *testing.T) {
	m, kg, scope := setupTest(t, kgingestion.StrategyContentGated, 5)
	putTurn(t, m, scope, 0, "Hello Bob works at TechCorp as an engineer")
	emitTurnSignal(t, m, scope, 0)
	require.Len(t, kg.ingested, 1)
}

func TestBatchStrategy_FlushesOnComplete(t *testing.T) {
	m, kg, scope := setupTest(t, kgingestion.StrategyBatch, 0)
	putTurn(t, m, scope, 0, "Hello Bob")
	emitTurnSignal(t, m, scope, 0)
	putTurn(t, m, scope, 1, "He works at TechCorp")
	emitTurnSignal(t, m, scope, 1)
	assert.Empty(t, kg.ingested, "batch should not ingest on turn signals")

	err := m.SendSignal(memory.WithSystemApproval(context.Background(), "test"), memory.Signal{
		Kind: lifecycle.SigSessionCompleted, Scope: scope, At: time.Now(),
	})
	require.NoError(t, err)
	require.Len(t, kg.ingested, 1, "batch should flush on session completed")
	assert.Contains(t, kg.ingested[0].Content, "Hello Bob")
	assert.Contains(t, kg.ingested[0].Content, "TechCorp")
}
