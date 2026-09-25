package hooks_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// findHook returns the descriptor with the given name, or fails the test.
func findHook(t *testing.T, descs []hooks.HookDescriptor, name string) hooks.HookDescriptor {
	t.Helper()
	for _, d := range descs {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("hook %q not found in %v", name, descs)
	return hooks.HookDescriptor{}
}

func TestActiveHooks_Order(t *testing.T) {
	// All-on config: every hook should be present and in canonical order.
	got := hooks.ActiveHooks(hooks.ActivationConfig{
		ToolCallMode:       "enforcing",
		ScopeEnabled:       true,
		ColdStart:          "extractAndApprove",
		LeakageMode:        "enforcing",
		InteractPermission: "user:owner",
		BoundEntityCount:   2,
	})

	// Canonical order across all points (the table is a flat, point-ordered list).
	wantOrder := []string{
		"cold_start_scope",   // SessionStart
		"interact",           // InboundTurn (channelsd)
		"entity_bind",        // InboundTurn (authzd)
		"mcp_trust",          // PreToolCall
		"tool_call_authz",    // PreToolCall
		"scope",              // PreToolCall/PostToolCall
		"info_leak_read",     // PostToolCall/PreResponse
		"info_leak_audience", // PostToolCall/PreResponse
		"session_cleanup",    // SessionEnd
	}
	var gotNames []string
	for _, d := range got {
		gotNames = append(gotNames, d.Name)
	}
	assert.Equal(t, wantOrder, gotNames, "ActiveHooks must list hooks in canonical point+order")
}

func TestActiveHooks_AllOn_Activation(t *testing.T) {
	got := hooks.ActiveHooks(hooks.ActivationConfig{
		ToolCallMode:       "enforcing",
		ScopeEnabled:       true,
		ColdStart:          "extractAndApprove",
		LeakageMode:        "enforcing",
		InteractPermission: "user:owner",
		BoundEntityCount:   2,
	})

	assert.Equal(t, hooks.ActiveYes, findHook(t, got, "tool_call_authz").Active)
	assert.Equal(t, hooks.ActiveYes, findHook(t, got, "scope").Active)
	assert.Equal(t, hooks.ActiveYes, findHook(t, got, "cold_start_scope").Active)
	assert.Equal(t, hooks.ActiveYes, findHook(t, got, "info_leak_read").Active)
	assert.Equal(t, hooks.ActiveYes, findHook(t, got, "info_leak_audience").Active)
	assert.Equal(t, hooks.ActiveYes, findHook(t, got, "interact").Active)
	assert.Equal(t, hooks.ActiveYes, findHook(t, got, "entity_bind").Active)
	// SessionCleanup is always active.
	assert.Equal(t, hooks.ActiveYes, findHook(t, got, "session_cleanup").Active)
	// McpTrust is session-tool-dependent → conditional at the class level.
	assert.Equal(t, hooks.ActiveConditional, findHook(t, got, "mcp_trust").Active,
		"McpTrust is session-tool-dependent; class-level view renders it conditional")

	// Points are reported per hook (sample a couple).
	assert.Equal(t, []pipeline.Point{pipeline.PreToolCall}, findHook(t, got, "tool_call_authz").Points)
	assert.ElementsMatch(t, []pipeline.Point{pipeline.PreToolCall, pipeline.PostToolCall}, findHook(t, got, "scope").Points)
	assert.Equal(t, []pipeline.Point{pipeline.SessionEnd}, findHook(t, got, "session_cleanup").Points)
}

func TestActiveHooks_ScopeOnly(t *testing.T) {
	got := hooks.ActiveHooks(hooks.ActivationConfig{
		ScopeEnabled: true,
		ColdStart:    "off",
		ToolCallMode: "disabled",
		LeakageMode:  "disabled",
	})
	assert.Equal(t, hooks.ActiveYes, findHook(t, got, "scope").Active)
	assert.Equal(t, hooks.ActiveNo, findHook(t, got, "tool_call_authz").Active)
	assert.Equal(t, hooks.ActiveNo, findHook(t, got, "info_leak_read").Active)
	assert.Equal(t, hooks.ActiveNo, findHook(t, got, "info_leak_audience").Active)
	// coldStart=off ⇒ cold_start_scope inactive even though scope enabled.
	assert.Equal(t, hooks.ActiveNo, findHook(t, got, "cold_start_scope").Active)
}

func TestActiveHooks_LeakageDisabled(t *testing.T) {
	got := hooks.ActiveHooks(hooks.ActivationConfig{
		LeakageMode:  "disabled",
		ToolCallMode: "enforcing",
	})
	assert.Equal(t, hooks.ActiveNo, findHook(t, got, "info_leak_read").Active)
	assert.Equal(t, hooks.ActiveNo, findHook(t, got, "info_leak_audience").Active)
	assert.Equal(t, hooks.ActiveYes, findHook(t, got, "tool_call_authz").Active)
}

// TestActiveHooks_ToolCallsDisabledButScopeOn pins that Scope renders ACTIVE
// even when ToolCallAuthz is disabled — the two are independent gates.
func TestActiveHooks_ToolCallsDisabledButScopeOn(t *testing.T) {
	got := hooks.ActiveHooks(hooks.ActivationConfig{
		ToolCallMode: "disabled",
		ScopeEnabled: true,
	})
	require.Equal(t, hooks.ActiveNo, findHook(t, got, "tool_call_authz").Active,
		"tool_call_authz inactive when toolCalls.mode=disabled")
	assert.Equal(t, hooks.ActiveYes, findHook(t, got, "scope").Active,
		"Scope must stay ACTIVE even when ToolCallAuthz is disabled (spec §8 gap)")
}
