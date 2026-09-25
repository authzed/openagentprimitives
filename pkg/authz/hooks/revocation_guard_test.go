package hooks_test

import (
	"context"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/toolorigin"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/stretchr/testify/assert"
)

func toolInput(name string) pipeline.Input {
	return pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: name},
	}
}

func TestRevocationGuardDeniesRevokedOrigin(t *testing.T) {
	set := toolorigin.New()
	_ = set.Invalidate("mcpserver/linear")
	g := hooks.NewRevocationGuard(hooks.RevocationGuardDeps{
		Set:          set,
		LookupOrigin: func(string) string { return "mcpserver/linear" },
	})
	d := g.Eval(context.Background(), toolInput("some_tool"))
	assert.Equal(t, pipeline.Deny, d.Verdict)
	assert.Contains(t, d.Reason, "mcpserver/linear")
}

func TestRevocationGuardAllowsLiveOrigin(t *testing.T) {
	g := hooks.NewRevocationGuard(hooks.RevocationGuardDeps{
		Set:          toolorigin.New(),
		LookupOrigin: func(string) string { return "mcpserver/linear" },
	})
	d := g.Eval(context.Background(), toolInput("some_tool"))
	assert.Equal(t, pipeline.Decision{}, d)
}

func TestRevocationGuardAllowsOriginlessTools(t *testing.T) {
	set := toolorigin.New()
	_ = set.Invalidate("mcpserver/linear")
	g := hooks.NewRevocationGuard(hooks.RevocationGuardDeps{
		Set:          set,
		LookupOrigin: func(string) string { return "" }, // origin-less tool
	})
	d := g.Eval(context.Background(), toolInput("sandbox_exec"))
	assert.Equal(t, pipeline.Decision{}, d)
}
