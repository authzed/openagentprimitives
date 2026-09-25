package meta_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

type fakeKG struct {
	facts    []memory.KGFact
	entities []memory.KGEntity
	comms    []memory.KGCommunity
}

func (f *fakeKG) Ingest(_ context.Context, _ memory.KGInput) error { return nil }
func (f *fakeKG) SearchFacts(_ context.Context, _ string, _ int) ([]memory.KGFact, error) {
	return f.facts, nil
}
func (f *fakeKG) GetEntity(_ context.Context, _ string) (*memory.KGEntity, error) { return nil, nil }
func (f *fakeKG) EntityFacts(_ context.Context, _ string) ([]memory.KGFact, error) {
	return f.facts, nil
}
func (f *fakeKG) RelatedEntities(_ context.Context, _ string, _ int) ([]memory.KGEntity, error) {
	return f.entities, nil
}
func (f *fakeKG) Communities(_ context.Context, _ string) ([]memory.KGCommunity, error) {
	return f.comms, nil
}

func TestQueryKnowledge_Name(t *testing.T) {
	assert.Equal(t, "query_knowledge", meta.NewQueryKnowledge().Name())
}

func TestQueryKnowledge_SearchMode(t *testing.T) {
	kg := &fakeKG{facts: []memory.KGFact{
		{UUID: "f1", Name: "WORKS_AT", Fact: "Bob works at TechCorp"},
	}}
	sess := &tool.SessionContext{Namespace: "default", Name: "test", KG: kg}
	res, err := meta.NewQueryKnowledge().Execute(memory.WithSystemApproval(context.Background(), "test"),
		json.RawMessage(`{"text":"Bob","mode":"search"}`), sess)
	require.NoError(t, err)
	assert.False(t, res.IsError)
	// Corrected: this used to assert Trusted (the blanket "framework meta tool"
	// sweep). Graph facts are extracted by an external service from stored
	// turns, so the payload is third-party and must be inspected. See
	// TestMemoryRecallTools_ThirdPartyContent_IsUntrusted.
	assert.False(t, res.Trusted, "query_knowledge relays externally extracted facts and must be inspected")
	var out map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(res.Content), &out))
	assert.Equal(t, float64(1), out["count"])
}

func TestQueryKnowledge_NoKG(t *testing.T) {
	sess := &tool.SessionContext{Namespace: "default", Name: "test"}
	res, err := meta.NewQueryKnowledge().Execute(memory.WithSystemApproval(context.Background(), "test"),
		json.RawMessage(`{"text":"test"}`), sess)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "not available")
}

func TestQueryKnowledge_RelatedMode(t *testing.T) {
	kg := &fakeKG{entities: []memory.KGEntity{
		{UUID: "e1", Name: "TechCorp"},
	}}
	sess := &tool.SessionContext{Namespace: "default", Name: "test", KG: kg}
	res, err := meta.NewQueryKnowledge().Execute(memory.WithSystemApproval(context.Background(), "test"),
		json.RawMessage(`{"entity_uuid":"e0","mode":"related"}`), sess)
	require.NoError(t, err)
	assert.False(t, res.IsError)
}

func TestQueryKnowledge_CommunitiesMode(t *testing.T) {
	kg := &fakeKG{comms: []memory.KGCommunity{
		{UUID: "c1", Name: "Engineering"},
	}}
	sess := &tool.SessionContext{Namespace: "default", Name: "test", KG: kg}
	res, err := meta.NewQueryKnowledge().Execute(memory.WithSystemApproval(context.Background(), "test"),
		json.RawMessage(`{"mode":"communities"}`), sess)
	require.NoError(t, err)
	assert.False(t, res.IsError)
}
