package runner_test

import (
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/stretchr/testify/assert"
)

func TestPinDriftState(t *testing.T) {
	t.Run("mark then drifted returns summary and ok=true", func(t *testing.T) {
		p := runner.NewPinDriftState()
		p.MarkDrifted("gh_get_issue", "MCPServer/gh drifted sha256:old -> sha256:new")
		summary, ok := p.Drifted("gh_get_issue")
		assert.True(t, ok, "Drifted must return ok=true after MarkDrifted")
		assert.Equal(t, "MCPServer/gh drifted sha256:old -> sha256:new", summary)
	})

	t.Run("clear then drifted returns ok=false", func(t *testing.T) {
		p := runner.NewPinDriftState()
		p.MarkDrifted("gh_get_issue", "some summary")
		p.Clear("gh_get_issue")
		_, ok := p.Drifted("gh_get_issue")
		assert.False(t, ok, "Drifted must return ok=false after Clear")
	})

	t.Run("unknown tool returns ok=false", func(t *testing.T) {
		p := runner.NewPinDriftState()
		_, ok := p.Drifted("no_such_tool")
		assert.False(t, ok, "Drifted must return ok=false for a tool that was never marked")
	})
}
