package memory_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

func TestKGEntity_JSONRoundTrip(t *testing.T) {
	e := memory.KGEntity{
		UUID: "abc-123", Name: "Bob", Summary: "Software engineer",
		Attributes: map[string]string{"role": "engineer"},
	}
	b, err := json.Marshal(e)
	require.NoError(t, err)
	var got memory.KGEntity
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, e.Name, got.Name)
	assert.Equal(t, e.Attributes["role"], got.Attributes["role"])
}

func TestKGFact_JSONRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := memory.KGFact{
		UUID: "fact-1", Name: "WORKS_AT", Fact: "Bob works at TechCorp",
		FromEntity: "ent-1", ToEntity: "ent-2", ValidAt: &now,
	}
	b, err := json.Marshal(f)
	require.NoError(t, err)
	var got memory.KGFact
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, f.Fact, got.Fact)
	assert.Equal(t, f.FromEntity, got.FromEntity)
	assert.Nil(t, got.InvalidAt)
}

func TestKGCommunity_JSONRoundTrip(t *testing.T) {
	c := memory.KGCommunity{
		UUID: "comm-1", Name: "Engineering", Summary: "The engineering team",
		Members: []string{"ent-1", "ent-2"},
	}
	b, err := json.Marshal(c)
	require.NoError(t, err)
	var got memory.KGCommunity
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, c.Name, got.Name)
	assert.Len(t, got.Members, 2)
}
