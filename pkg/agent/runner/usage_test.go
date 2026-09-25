package runner

import (
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/stretchr/testify/assert"
)

func TestLoopUsageSnapshot_Accumulates(t *testing.T) {
	var l Loop
	l.addUsage(llm.Usage{InputTokens: 100, OutputTokens: 10, CacheCreationTokens: 5, CacheReadTokens: 50})
	l.addUsage(llm.Usage{InputTokens: 200, OutputTokens: 20, CacheCreationTokens: 0, CacheReadTokens: 70})
	s := l.usageSnapshot()
	assert.Equal(t, int64(300), s.InputTokens)
	assert.Equal(t, int64(30), s.OutputTokens)
	assert.Equal(t, int64(5), s.CacheCreationTokens)
	assert.Equal(t, int64(120), s.CacheReadTokens)
}
