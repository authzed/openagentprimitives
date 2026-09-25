package runner

import (
	"context"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// endCapturingRunner is a pipelineRunner that records the Input it was called
// with, so a test can assert what fireSessionEnd built.
type endCapturingRunner struct{ in pipeline.Input }

func (c *endCapturingRunner) Run(_ context.Context, _ pipeline.Point, in pipeline.Input, _ pipeline.Host) (pipeline.Outcome, error) {
	c.in = in
	return pipeline.Outcome{Verdict: pipeline.Allow}, nil
}

func TestFireSessionEnd_PopulatesUsage(t *testing.T) {
	cr := &endCapturingRunner{}
	l := &Loop{Model: "claude-opus-4-8"}
	// Prime the executor seam so executor() returns our capturing runner.
	l.pipelineExec = cr
	l.pipelineOnce.Do(func() {})

	l.addUsage(llm.Usage{InputTokens: 100, OutputTokens: 10, CacheCreationTokens: 5, CacheReadTokens: 50})

	l.fireSessionEnd(context.Background(), "completed")

	require.NotNil(t, cr.in.End)
	assert.Equal(t, "completed", cr.in.End.Reason)
	assert.Equal(t, "claude-opus-4-8", cr.in.End.Model)
	assert.Equal(t, int64(100), cr.in.End.InputTokens)
	assert.Equal(t, int64(10), cr.in.End.OutputTokens)
	assert.Equal(t, int64(5), cr.in.End.CacheCreationTokens)
	assert.Equal(t, int64(50), cr.in.End.CacheReadTokens)
}
