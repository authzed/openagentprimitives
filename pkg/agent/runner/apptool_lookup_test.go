package runner

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// These tests pin the Phase-D prerequisite: the runner's per-call pipeline
// lookups must resolve tools across BOTH l.Tools (LLM-visible) AND l.AppTools
// (MCP-UI app-visible). Before the fix every lookup iterated l.Tools only, so an
// app-tool's real Permission (StateImpact + Check) / kind / origin were invisible
// to the contained pipeline — making the ToolCallAuthz Check + StateImpact
// routing + toolguard breaker a silent no-op for app-tools.

// TestLoop_lookupTool pins the shared resolver: Tools first (LLM path stays
// byte-identical), then AppTools; miss → (nil,false).
func TestLoop_lookupTool(t *testing.T) {
	llmTool := &fakeAppTool{name: "llm_tool", perm: authz.Permission{StateImpact: authz.Readonly}}
	appTool := &fakeAppTool{name: "app_tool", perm: authz.Permission{StateImpact: authz.Readwrite}}
	l := &Loop{
		SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"},
		Tools:      []tool.Tool{llmTool},
		AppTools:   map[string]tool.Tool{"app_tool": appTool},
	}

	t.Run("resolves an LLM tool from l.Tools", func(t *testing.T) {
		got, ok := l.lookupTool("llm_tool")
		require.True(t, ok, "an LLM-visible tool must resolve")
		assert.Same(t, tool.Tool(llmTool), got)
	})

	t.Run("resolves an app-tool from l.AppTools", func(t *testing.T) {
		got, ok := l.lookupTool("app_tool")
		require.True(t, ok, "an app-visible tool must resolve from the separate registry")
		assert.Same(t, tool.Tool(appTool), got)
	})

	t.Run("miss returns (nil,false)", func(t *testing.T) {
		got, ok := l.lookupTool("nope")
		assert.False(t, ok)
		assert.Nil(t, got)
	})
}

// TestLookupTool_ToolsReassignRace is the -race proof for the l.Tools guard:
// lookupTool is reached from the NATS app-tool handler goroutine, which races
// ToolRefresher's mid-turn `l.Tools = applyToolRefresh(...)` reassignment on the
// Run goroutine. lookupTool snapshots l.Tools under toolsMu.RLock, so it is
// race-clean against the guarded writer. Stripping lookupTool's RLock makes this
// FAIL under -race (unsynchronized concurrent read/write of the slice field).
func TestLookupTool_ToolsReassignRace(t *testing.T) {
	appTool := &fakeAppTool{name: "app_tool", perm: authz.Permission{StateImpact: authz.Readonly}}
	l := &Loop{
		SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"},
		Tools:      []tool.Tool{&fakeAppTool{name: "t0"}},
		AppTools:   map[string]tool.Tool{"app_tool": appTool},
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				// Mirror ToolRefresher's guarded mid-turn reassignment.
				l.toolsMu.Lock()
				l.Tools = []tool.Tool{&fakeAppTool{name: fmt.Sprintf("t%d", i)}}
				l.toolsMu.Unlock()
			}
		}
	}()

	// "app_tool" lives only in AppTools, so each lookup ranges the entire l.Tools
	// snapshot (never matching) before resolving from AppTools — exercising the
	// guarded read against the concurrent writer.
	for i := 0; i < 500; i++ {
		got, ok := l.lookupTool("app_tool")
		require.True(t, ok)
		assert.Same(t, tool.Tool(appTool), got)
	}
	close(stop)
	<-done
}

// TestResolvePermission_AcrossRegistries is the critical assertion: the
// ToolCallAuthz ResolvePermission closure must return an app-tool's REAL
// Permission (with its Check + StateImpact), not the zero Permission it returned
// when the app-tool was invisible to the l.Tools-only scan.
func TestResolvePermission_AcrossRegistries(t *testing.T) {
	appTool := &fakeAppTool{name: "app_write", perm: checkPerm()} // Readonly + Check
	llmTool := &fakeDispatchTool{name: "llm_read", kind: tool.KindMCP, perm: authz.Permission{StateImpact: authz.Readwrite, Check: &authz.PermissionCheck{ResourceType: "doc", Permission: "write"}}}
	l := &Loop{
		SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"},
		Tools:      []tool.Tool{llmTool},
		AppTools:   map[string]tool.Tool{"app_write": appTool},
	}
	resolve := l.toolCallAuthzDeps().ResolvePermission

	t.Run("app-tool resolves its real Permission (Check + StateImpact), not zero", func(t *testing.T) {
		perm, err := resolve("app_write", map[string]any{})
		require.NoError(t, err)
		assert.Equal(t, authz.Readonly, perm.StateImpact, "the app-tool's StateImpact must survive the lookup")
		require.NotNil(t, perm.Check, "the app-tool's per-resource Check must be resolved, not dropped to zero")
		assert.Equal(t, "repo", perm.Check.ResourceType)
	})

	t.Run("LLM tool resolves from l.Tools (byte-identical)", func(t *testing.T) {
		perm, err := resolve("llm_read", map[string]any{})
		require.NoError(t, err)
		assert.Equal(t, authz.Readwrite, perm.StateImpact)
		require.NotNil(t, perm.Check)
		assert.Equal(t, "doc", perm.Check.ResourceType)
	})

	t.Run("unknown tool resolves the zero Permission", func(t *testing.T) {
		perm, err := resolve("nope", map[string]any{})
		require.NoError(t, err)
		assert.Equal(t, authz.Permission{}, perm)
	})
}

// TestToolGuardLookup_AcrossRegistries pins that the toolguard circuit-breaker's
// (kind, origin) resolution keys on an app-tool's real origin, so a breaker can
// trip on the app-tool's MCPServer origin.
func TestToolGuardLookup_AcrossRegistries(t *testing.T) {
	appTool := &fakeAppTool{name: "app_tool", perm: authz.Permission{StateImpact: authz.Readonly}, origin: "mcpserver/widgets"}
	llmTool := &fakeAppTool{name: "llm_tool", perm: authz.Permission{StateImpact: authz.Readonly}, origin: "mcpserver/backend"}
	l := &Loop{
		SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"},
		Tools:      []tool.Tool{llmTool},
		AppTools:   map[string]tool.Tool{"app_tool": appTool},
	}
	lookup := l.toolGuardLookup()

	t.Run("app-tool yields its (kind, origin)", func(t *testing.T) {
		kind, origin := lookup("app_tool")
		assert.Equal(t, "mcp", kind)
		assert.Equal(t, "mcpserver/widgets", origin, "the breaker must key on the app-tool's origin")
	})

	t.Run("LLM tool yields its (kind, origin)", func(t *testing.T) {
		kind, origin := lookup("llm_tool")
		assert.Equal(t, "mcp", kind)
		assert.Equal(t, "mcpserver/backend", origin)
	})

	t.Run("unknown tool yields empty", func(t *testing.T) {
		kind, origin := lookup("nope")
		assert.Empty(t, kind)
		assert.Empty(t, origin)
	})
}
