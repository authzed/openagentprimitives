package capability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryOptInDefaultOff(t *testing.T) {
	c, ok := Lookup("memory")
	require.True(t, ok)
	assert.False(t, c.DefaultOn(), "memory must be opt-in")
}

func TestMemoryOfferMatrix(t *testing.T) {
	c, _ := Lookup("memory")
	cases := []struct {
		name      string
		env       RunnerEnv
		wantTools []string
		wantSkip  bool
	}{
		{name: "backend+search → all three", env: RunnerEnv{MemoryAvailable: true, SearchAvailable: true}, wantTools: []string{"query_memory", "search_memory", "record_observation"}},
		{name: "backend only → query + record, no search", env: RunnerEnv{MemoryAvailable: true, SearchAvailable: false}, wantTools: []string{"query_memory", "record_observation"}},
		{name: "no backend → skip", env: RunnerEnv{MemoryAvailable: false}, wantSkip: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools, skip := c.Offer(OfferContext{Ctx: context.Background(), Granted: true, Enabled: true, Env: tc.env})
			if tc.wantSkip {
				require.NotNil(t, skip)
				assert.Empty(t, tools)
				return
			}
			assert.Nil(t, skip)
			assert.Equal(t, tc.wantTools, toolNames(tools))
		})
	}
}

// TestMemoryOffersRecordObservationButStaysOptIn pins the two halves of "a
// write tool must not become ambient": the capability contributes
// record_observation once a memory backend is wired (so a session actually
// granted the capability can call it), and the capability itself remains
// DefaultOn() == false regardless (so no AgentClass gets it without an
// explicit grant).
func TestMemoryOffersRecordObservationButStaysOptIn(t *testing.T) {
	c, ok := Lookup("memory")
	require.True(t, ok)
	assert.False(t, c.DefaultOn(), "a write tool must not become ambient")

	tools, skip := c.Offer(OfferContext{Ctx: context.Background(), Granted: true, Enabled: true, Env: RunnerEnv{MemoryAvailable: true}})
	assert.Nil(t, skip)
	assert.Contains(t, toolNames(tools), "record_observation")
}

func TestKnowledgeOfferMatrix(t *testing.T) {
	c, ok := Lookup("knowledge")
	require.True(t, ok)
	assert.False(t, c.DefaultOn())
	tools, skip := c.Offer(OfferContext{Ctx: context.Background(), Granted: true, Enabled: true, Env: RunnerEnv{KGAvailable: true}})
	assert.Nil(t, skip)
	assert.Equal(t, []string{"query_knowledge"}, toolNames(tools))

	_, skip2 := c.Offer(OfferContext{Ctx: context.Background(), Granted: true, Enabled: true, Env: RunnerEnv{KGAvailable: false}})
	assert.NotNil(t, skip2, "no KG endpoint → skip")
}
