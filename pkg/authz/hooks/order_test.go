package hooks_test

import (
	"testing"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/stretchr/testify/assert"
)

// The code-owned per-point order (spec §6):
//
//	PreToolCall:  McpTrust → ToolCallAuthz → Scope
//	PostToolCall: Scope → InfoLeakRead → InfoLeakAudience
//
// We encode order as ascending ints; lower runs first.
func TestHookOrder_PreToolCall(t *testing.T) {
	assert.Less(t, hooks.OrderMcpTrust, hooks.OrderToolCallAuthz, "McpTrust before ToolCallAuthz")
	assert.Less(t, hooks.OrderToolCallAuthz, hooks.OrderScope, "ToolCallAuthz before Scope")
}

func TestHookOrder_PostToolCall(t *testing.T) {
	assert.Less(t, hooks.OrderScope, hooks.OrderInfoLeakRead, "Scope before InfoLeakRead")
	assert.Less(t, hooks.OrderInfoLeakRead, hooks.OrderInfoLeakAudience, "InfoLeakRead before InfoLeakAudience")
}

// SessionStart in the runner's per-call registry carries two hooks: the
// IdentityChoiceGate (identityMode=ask|dynamic) and ColdStartScope. The gate MUST
// run first — identity resolves before scope review, so a userPassthrough handoff
// halts before any scope work and an agent choice is settled before scope
// narrowing. (EntityBind slots at SessionStart in authzd's SEPARATE registry, so
// it is not compared against these here.)
func TestHookOrder_SessionStart(t *testing.T) {
	assert.Positive(t, hooks.OrderIdentityChoiceGate, "IdentityChoiceGate order must be positive")
	assert.Positive(t, hooks.OrderColdStartScope, "ColdStartScope order must be positive")
	assert.Less(t, hooks.OrderIdentityChoiceGate, hooks.OrderColdStartScope, "IdentityChoiceGate before ColdStartScope")
}

func TestHookOrder_SessionEnd(t *testing.T) {
	assert.Positive(t, hooks.OrderSessionCleanup, "SessionCleanup is the sole SessionEnd hook today")
}

// InboundTurn is served by hooks in two SEPARATE per-host registries: channelsd
// runs Interact; authzd runs EntityBind. They never share a registry, so the two
// order ints are not compared at runtime — we only assert they exist and are
// positive (each is the sole InboundTurn hook in its own host).
func TestHookOrder_InboundTurn_TwoHostSplit(t *testing.T) {
	assert.Positive(t, hooks.OrderInteract, "Interact is the sole InboundTurn hook in channelsd's host")
	assert.Positive(t, hooks.OrderEntityBind, "EntityBind is the sole InboundTurn hook in authzd's host")
}
