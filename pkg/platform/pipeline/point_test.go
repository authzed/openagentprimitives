package pipeline_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

func TestPoints_AllDistinctAndStable(t *testing.T) {
	all := []pipeline.Point{
		pipeline.SessionStart, pipeline.InboundTurn, pipeline.PreToolCall,
		pipeline.PostToolCall, pipeline.PreResponse, pipeline.SessionEnd,
		pipeline.MetaagentReceived, pipeline.MetaagentExtract,
		pipeline.MetaagentDecide, pipeline.MetaagentApply,
	}
	seen := map[pipeline.Point]bool{}
	for _, p := range all {
		assert.NotEmpty(t, string(p), "point must have a stable string value")
		assert.False(t, seen[p], "points must be distinct: %q repeated", p)
		seen[p] = true
	}
	// Stable wire values (persisted in audit/config) — guard against rename.
	assert.Equal(t, "session_start", string(pipeline.SessionStart))
	assert.Equal(t, "inbound_turn", string(pipeline.InboundTurn))
	assert.Equal(t, "pre_tool_call", string(pipeline.PreToolCall))
	assert.Equal(t, "post_tool_call", string(pipeline.PostToolCall))
	assert.Equal(t, "pre_response", string(pipeline.PreResponse))
	assert.Equal(t, "session_end", string(pipeline.SessionEnd))
	// The four metaagent control-plane points (distinct from the data-plane).
	assert.Equal(t, "metaagent_received", string(pipeline.MetaagentReceived))
	assert.Equal(t, "metaagent_extract", string(pipeline.MetaagentExtract))
	assert.Equal(t, "metaagent_decide", string(pipeline.MetaagentDecide))
	assert.Equal(t, "metaagent_apply", string(pipeline.MetaagentApply))
}
