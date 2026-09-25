package capability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

func TestIntrospectionResolvesOverAllToolsSoFar(t *testing.T) {
	c, ok := Lookup("introspection")
	require.True(t, ok)
	assert.True(t, c.Infrastructural())
	tools, skip := c.Offer(OfferContext{
		Ctx: context.Background(),
		Env: RunnerEnv{AllToolsSoFar: []tool.Tool{stubTool{"query_memory"}, stubTool{"sandbox_run"}}},
	})
	assert.Nil(t, skip)
	require.Len(t, tools, 1)
	assert.Equal(t, "introspect_tool", tools[0].Name())
	// The introspect tool must NOT resolve itself: AllToolsSoFar was captured
	// before introspect_tool was appended, so its name index excludes itself.
}
