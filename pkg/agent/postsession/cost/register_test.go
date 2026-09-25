package cost_test

import (
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	_ "github.com/authzed/openagentprimitives/pkg/agent/postsession/cost" // pull in init()
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func costFactory(t *testing.T) runner.HookFactory {
	t.Helper()
	for _, f := range runner.RegisteredHookFactories() {
		if f.Name == "session_cost" {
			return f
		}
	}
	t.Fatal("session_cost factory not registered")
	return runner.HookFactory{}
}

func TestCostHook_GatedBySetting(t *testing.T) {
	f := costFactory(t)

	on := f.Build(&runner.Loop{ReportSessionCost: true, Provider: fake.New(nil)})
	require.Len(t, on, 1)
	assert.Equal(t, "session_cost", on[0].Name())

	off := f.Build(&runner.Loop{ReportSessionCost: false, Provider: fake.New(nil)})
	assert.Empty(t, off, "setting off ⇒ no cost hook")

	nilProvider := f.Build(&runner.Loop{ReportSessionCost: true, Provider: nil})
	assert.Empty(t, nilProvider, "nil provider ⇒ no cost hook (no pricing source)")
}
