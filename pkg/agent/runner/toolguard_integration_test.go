package runner

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolguardaudit"
)

// wedgedTool is a non-meta (sandbox-kind, origin-less) tool whose Execute
// always reports a tool-side error, counting its invocations so the test can
// prove the circuit breaker stops calling it once open.
type wedgedTool struct {
	mu    sync.Mutex
	calls int
}

func (w *wedgedTool) Name() string                                  { return "wedged" }
func (w *wedgedTool) Kind() tool.Kind                               { return tool.KindSandbox }
func (w *wedgedTool) Description() string                           { return "always fails" }
func (w *wedgedTool) InputSchema() json.RawMessage                  { return json.RawMessage(`{"type":"object"}`) }
func (w *wedgedTool) Permission() authz.Permission                  { return authz.Permission{} }
func (w *wedgedTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (w *wedgedTool) Execute(_ context.Context, _ json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	w.mu.Lock()
	w.calls++
	w.mu.Unlock()
	return tool.Result{Content: "boom", IsError: true}, nil
}
func (w *wedgedTool) executed() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// TestToolGuardDispatchTripsDefaultBreaker drives the REAL dispatch pipeline
// (no injected executor — the toolguard Guard/GuardRecord hooks self-register
// from Loop.ToolGuardPolicy via buildPipelineRegistry) and proves the default,
// builtin circuit breaker trips after 5 failures and then denies further calls
// without re-invoking the wedged tool.
//
// ToolCallAuthz is neutralized by ToolAuthMode="disabled" so the only
// PreToolCall hook in play is tool_guard — isolating the breaker behavior.
func TestToolGuardDispatchTripsDefaultBreaker(t *testing.T) {
	// Builtin rule (no tiers): FailureThreshold 5, InitialCoolOff 30s, action deny.
	pol, err := toolguard.ResolvePolicy(toolguard.Tiers{})
	require.NoError(t, err, "ResolvePolicy(empty) must yield the builtin rule")

	mem := memory.NewLocal(inmem.NewBackend())
	key := memory.NamespacedName{Namespace: "default", Name: "tg-disp"}
	l := &Loop{
		Tools:           []tool.Tool{&wedgedTool{}},
		Mem:             mem,
		SessionKey:      key,
		ToolGuardPolicy: pol,
		// Disabled mode keeps tool_call_authz out of the registry so the only
		// active PreToolCall hook is tool_guard (real-registry isolation).
		ToolAuthMode: ToolAuthModeDisabled,
	}
	// The fake tool instance the dispatch path will resolve by name.
	wedged := l.Tools[0].(*wedgedTool)

	sess := &tool.SessionContext{Namespace: key.Namespace, Name: key.Name}
	auditScope := memory.Scope{Kind: "session", ID: key.Namespace + "/" + key.Name}

	// dispatchOne dispatches a single "wedged" tool_use (one dispatchToolUses
	// call per admission so the PostToolCall outcome is recorded between
	// admissions, matching real turn-by-turn dispatch).
	dispatchOne := func(useID string) tool.Result {
		t.Helper()
		uses := []llm.ToolUseBlock{{ID: useID, Name: "wedged", Input: json.RawMessage(`{"args":{}}`)}}
		results, _ := l.dispatchToolUses(memory.WithSystemApproval(context.Background(), "test"), uses, sess, 0, 0, nil, nil)
		require.Len(t, results, 1, "one tool_use → one result")
		return results[0]
	}

	// 1. Five sequential admitted calls — all run Execute, all surface "boom".
	for i := 0; i < 5; i++ {
		res := dispatchOne("tu-admit")
		assert.True(t, res.IsError, "admitted call %d must surface the tool error", i+1)
		assert.Contains(t, res.Content, "boom", "admitted call %d must carry the tool's error text", i+1)
	}
	require.Equal(t, 5, wedged.executed(), "the breaker admitted all 5 failing calls (Execute ran 5×)")

	// 2. Sixth call — breaker is open; denied without invoking Execute.
	sixth := dispatchOne("tu-open")
	assert.True(t, sixth.IsError, "6th call must be denied (IsError)")
	assert.Contains(t, sixth.Content, "circuit breaker open", "deny text must name the breaker state")
	assert.Contains(t, sixth.Content, "wedged", "deny text must name the gated tool")
	assert.Equal(t, 5, wedged.executed(), "a breaker-open deny must NOT invoke the tool")

	// 3. Audit trail: exactly one breaker_opened (Trips=1), ≥1 guard_deny.
	entries, err := toolguardaudit.List(memory.WithSystemApproval(context.Background(), "test"), mem, auditScope)
	require.NoError(t, err, "audit List over the session scope")

	var opened, denies int
	for _, e := range entries {
		switch e.Event {
		case "breaker_opened":
			opened++
			assert.Equal(t, int32(1), e.Trips, "first trip records Trips=1")
			assert.True(t, strings.Contains(e.Tool, "wedged"), "breaker_opened names the tool")
		case "guard_deny":
			denies++
		}
	}
	assert.Equal(t, 1, opened, "exactly one breaker_opened transition")
	assert.GreaterOrEqual(t, denies, 1, "at least one guard_deny while the breaker is open")

	// 4. Seventh immediate call — still open, still denied, count unchanged.
	seventh := dispatchOne("tu-still-open")
	assert.True(t, seventh.IsError, "7th call must still be denied")
	assert.Contains(t, seventh.Content, "circuit breaker open", "still-open deny names the breaker state")
	assert.Equal(t, 5, wedged.executed(), "still-open deny must NOT invoke the tool")

	// 5. Status patching: this Loop has a nil Status patcher, so
	// patchToolGuardStatus is a no-op (it early-returns on l.Status == nil).
	// The status path is unit-tested in the toolguard hook tests; this
	// dispatch-level test deliberately avoids standing up an envtest.
	assert.Nil(t, l.Status, "no Status patcher wired — status patch path is unit-tested elsewhere")
}

// bigTool succeeds but returns a result larger than the configured ingress
// budget, so the PostToolCall guard must withhold it.
type bigTool struct {
	mu    sync.Mutex
	calls int
	size  int
}

func (b *bigTool) Name() string                                  { return "bigread" }
func (b *bigTool) Kind() tool.Kind                               { return tool.KindSandbox }
func (b *bigTool) Description() string                           { return "returns a big payload" }
func (b *bigTool) InputSchema() json.RawMessage                  { return json.RawMessage(`{"type":"object"}`) }
func (b *bigTool) Permission() authz.Permission                  { return authz.Permission{} }
func (b *bigTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (b *bigTool) Execute(_ context.Context, _ json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	payload := make([]byte, b.size)
	for i := range payload {
		payload[i] = 'Z'
	}
	return tool.Result{Content: string(payload)}, nil
}

// TestToolGuardDispatchWithholdsOversizedResult drives the real dispatch
// pipeline with a per-tool ingress byte budget and proves the oversized
// successful result is replaced by an IsError that never carries the payload.
func TestToolGuardDispatchWithholdsOversizedResult(t *testing.T) {
	pol, err := toolguard.ResolvePolicy(toolguard.Tiers{Class: &v1alpha1.ToolGuardPolicy{
		Rules: []v1alpha1.ToolGuardRule{{
			Match:     v1alpha1.ToolGuardMatch{Tool: "bigread"},
			DataLimit: &v1alpha1.DataLimitSpec{MaxIngressBytes: 64, Action: "deny"},
		}},
	}})
	require.NoError(t, err)

	mem := memory.NewLocal(inmem.NewBackend())
	key := memory.NamespacedName{Namespace: "default", Name: "tg-ingress"}
	l := &Loop{
		Tools:           []tool.Tool{&bigTool{size: 4096}},
		Mem:             mem,
		SessionKey:      key,
		ToolGuardPolicy: pol,
		ToolAuthMode:    ToolAuthModeDisabled,
	}
	big := l.Tools[0].(*bigTool)
	sess := &tool.SessionContext{Namespace: key.Namespace, Name: key.Name}
	auditScope := memory.Scope{Kind: "session", ID: key.Namespace + "/" + key.Name}

	uses := []llm.ToolUseBlock{{ID: "tu-big", Name: "bigread", Input: json.RawMessage(`{}`)}}
	results, _ := l.dispatchToolUses(memory.WithSystemApproval(context.Background(), "test"), uses, sess, 0, 0, nil, nil)
	require.Len(t, results, 1)

	assert.Equal(t, 1, big.calls, "the tool DID execute (ingress is a post-hook)")
	assert.True(t, results[0].IsError, "oversized result must be withheld as IsError")
	assert.Contains(t, results[0].Content, "withheld")
	assert.NotContains(t, results[0].Content, "ZZZ", "the oversized payload must NOT reach the model")

	entries, err := toolguardaudit.List(memory.WithSystemApproval(context.Background(), "test"), mem, auditScope)
	require.NoError(t, err)
	var ingress int
	for _, e := range entries {
		if e.Event == "ingress_limit_hit" {
			ingress++
			assert.Equal(t, "ingress", e.Limit)
			assert.Equal(t, int64(4096), e.ObservedBytes)
		}
	}
	assert.Equal(t, 1, ingress, "exactly one ingress_limit_hit recorded")
}
