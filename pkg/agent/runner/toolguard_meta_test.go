package runner

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// countingMetaTool is a Stateless meta tool — the shape of every meta tool that
// surfaces third-party content (read_channel_history, query_memory, …). Its
// trivial permission is what makes the dispatcher skip executeToolContained,
// and with it every toolguard hook.
type countingMetaTool struct {
	name    string
	content string
	isError bool

	mu    sync.Mutex
	calls int
}

func (m *countingMetaTool) Name() string                 { return m.name }
func (m *countingMetaTool) Kind() tool.Kind              { return tool.KindMeta }
func (m *countingMetaTool) Description() string          { return "recalls third-party content" }
func (m *countingMetaTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (m *countingMetaTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (m *countingMetaTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (m *countingMetaTool) Execute(_ context.Context, _ json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return tool.Result{Content: m.content, IsError: m.isError}, nil
}
func (m *countingMetaTool) executed() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// metaGuardLoop builds a dispatch-ready Loop over one meta tool and a policy
// resolved from the given tiers. It takes the whole Tiers rather than a rule
// list because rules and the Limits ceiling are two independent admin surfaces,
// and either one alone must be able to govern this tool.
func metaGuardLoop(t *testing.T, mt *countingMetaTool, tiers toolguard.Tiers) *Loop {
	t.Helper()
	pol, err := toolguard.ResolvePolicy(tiers)
	require.NoError(t, err, "ResolvePolicy must accept the fixture tiers")
	return &Loop{
		Tools:           []tool.Tool{mt},
		Mem:             memory.NewLocal(inmem.NewBackend()),
		SessionKey:      memory.NamespacedName{Namespace: "default", Name: "tg-meta"},
		ToolGuardPolicy: pol,
		ToolAuthMode:    ToolAuthModeDisabled,
	}
}

// classRules is the class-tier (AgentClass.spec.toolGuard) shape of Tiers —
// the "an admin authored a rule" surface.
func classRules(rules ...v1.ToolGuardRule) toolguard.Tiers {
	return toolguard.Tiers{Class: &v1.ToolGuardPolicy{Rules: rules}}
}

// ceiling is the Limits-tier (settings.limits.toolGuard) shape of Tiers — the
// OTHER admin surface, folded independently of any rule by
// pkg/platform/settings.effectiveToolGuard.
func ceiling(c *v1.ToolGuardCeiling) toolguard.Tiers {
	return toolguard.Tiers{Ceiling: c}
}

// dispatchMeta dispatches one tool_use for mt through the real dispatch path.
func dispatchMeta(t *testing.T, l *Loop, name, useID string, turn int32) tool.Result {
	t.Helper()
	sess := &tool.SessionContext{Namespace: l.SessionKey.Namespace, Name: l.SessionKey.Name}
	uses := []llm.ToolUseBlock{{ID: useID, Name: name, Input: json.RawMessage(`{}`)}}
	results, _ := l.dispatchToolUses(
		memory.WithSystemApproval(context.Background(), "test"), uses, sess, turn, 0, nil, nil)
	require.Len(t, results, 1, "one tool_use → one result")
	return results[0]
}

// TestToolGuardAppliesToMetaTools covers a rule that an admin writes, the CRD
// accepts (ToolGuardMatch.Kind's enum advertises "meta"), and that used to do
// nothing at all: toolguard's hooks run only inside executeToolContained, which
// the dispatcher skips for every Stateless/Passthrough meta tool. Nothing
// rejected such a rule at admission, nothing reported it inert in status, and
// nothing logged it — the admin believed a cap was enforced when it was not, on
// exactly the meta tools that surface third-party content.
func TestToolGuardAppliesToMetaTools(t *testing.T) {
	t.Run("rateLimit on kind=meta: second call in the turn is denied without executing", func(t *testing.T) {
		mt := &countingMetaTool{name: "recall_history", content: "third-party text"}
		l := metaGuardLoop(t, mt, classRules(v1.ToolGuardRule{
			Match:     v1.ToolGuardMatch{Kind: "meta", Tool: "recall_history"},
			RateLimit: &v1.RateLimitSpec{MaxCallsPerTurn: 1},
		}))

		first := dispatchMeta(t, l, "recall_history", "tu-1", 0)
		assert.False(t, first.IsError, "the first call is within budget")
		assert.Equal(t, "third-party text", first.Content)

		second := dispatchMeta(t, l, "recall_history", "tu-2", 0)
		assert.True(t, second.IsError, "the second call exceeds maxCallsPerTurn and must be denied")
		assert.Contains(t, second.Content, "call budget", "the deny text must say the budget is exhausted")
		assert.Equal(t, 1, mt.executed(), "a rate deny must NOT invoke the tool")
	})

	t.Run("breaker on kind=meta: opens after the authored threshold and stops calling", func(t *testing.T) {
		mt := &countingMetaTool{name: "recall_history", content: "boom", isError: true}
		l := metaGuardLoop(t, mt, classRules(v1.ToolGuardRule{
			Match:   v1.ToolGuardMatch{Kind: "meta", Tool: "recall_*"},
			Breaker: &v1.BreakerSpec{FailureThreshold: 2, Action: v1.ToolGuardActionDeny},
		}))

		// Each dispatch is its own turn, so the per-turn rate counter (unset
		// here) plays no part — only consecutive failures do.
		for i := int32(0); i < 2; i++ {
			res := dispatchMeta(t, l, "recall_history", "tu", i)
			assert.Contains(t, res.Content, "boom", "call %d is admitted and surfaces the tool error", i+1)
		}
		require.Equal(t, 2, mt.executed(), "the breaker admitted both failing calls")

		third := dispatchMeta(t, l, "recall_history", "tu", 2)
		assert.True(t, third.IsError, "the breaker is open; the third call is denied")
		assert.Contains(t, third.Content, "circuit breaker open", "the deny text must name the breaker state")
		assert.Equal(t, 2, mt.executed(), "a breaker-open deny must NOT invoke the tool")
	})

	t.Run("dataLimit on kind=meta: oversized result is withheld from the model", func(t *testing.T) {
		big := strings.Repeat("x", 4096)
		mt := &countingMetaTool{name: "recall_history", content: big}
		l := metaGuardLoop(t, mt, classRules(v1.ToolGuardRule{
			Match:     v1.ToolGuardMatch{Kind: "meta", Tool: "recall_history"},
			DataLimit: &v1.DataLimitSpec{MaxIngressBytes: 1024},
		}))

		res := dispatchMeta(t, l, "recall_history", "tu-1", 0)
		assert.True(t, res.IsError, "a result over maxIngressBytes must be withheld")
		assert.NotContains(t, res.Content, big, "the oversized payload must never reach the model")
		assert.Equal(t, 1, mt.executed(), "an ingress deny happens after Execute, so the tool did run")
	})
}

// TestToolGuardCeilingAppliesToMetaTools covers the OTHER admin surface. A
// ToolGuardCeiling is folded from settings.limits.toolGuard independently of any
// rule (pkg/platform/settings.effectiveToolGuard), and its CRD doc promises the rate and
// byte ceilings bind "even when no lower-tier rule configures" one — which is
// how gated sandbox/mcp tools behave, since RuleFor clamps its Builtin fallback.
// A predicate that asks only "did a rule match?" answers no for every name in a
// rule-less policy, so an admin who caps ingress bytes precisely to bound how
// much third-party content enters model context got nothing on the ungated meta
// tools that fetch it.
func TestToolGuardCeilingAppliesToMetaTools(t *testing.T) {
	t.Run("ceiling-only maxIngressBytes: oversized meta result is withheld from the model", func(t *testing.T) {
		big := strings.Repeat("x", 4096)
		mt := &countingMetaTool{name: "recall_history", content: big}
		l := metaGuardLoop(t, mt, ceiling(&v1.ToolGuardCeiling{MaxIngressBytes: ptr.To(int64(1024))}))

		res := dispatchMeta(t, l, "recall_history", "tu-1", 0)
		assert.True(t, res.IsError, "a result over the ceiling's maxIngressBytes must be withheld")
		assert.NotContains(t, res.Content, big, "the oversized payload must never reach the model")
		assert.Equal(t, 1, mt.executed(), "an ingress deny happens after Execute, so the tool did run")
	})

	t.Run("ceiling-only maxCallsPerTurn: second call in the turn is denied without executing", func(t *testing.T) {
		mt := &countingMetaTool{name: "recall_history", content: "third-party text"}
		l := metaGuardLoop(t, mt, ceiling(&v1.ToolGuardCeiling{MaxCallsPerTurn: ptr.To(int32(1))}))

		first := dispatchMeta(t, l, "recall_history", "tu-1", 0)
		assert.False(t, first.IsError, "the first call is within the ceiling's budget")

		second := dispatchMeta(t, l, "recall_history", "tu-2", 0)
		assert.True(t, second.IsError, "the second call exceeds the ceiling and must be denied")
		assert.Contains(t, second.Content, "call budget", "the deny text must say the budget is exhausted")
		assert.Equal(t, 1, mt.executed(), "a rate deny must NOT invoke the tool")
	})
}

// TestToolGuardBuiltinDoesNotReachUngatedMetaTools pins the deliberate scope of
// the two tests above: what governs an ungated meta tool is an authored rule or
// an admin-set rate/byte ceiling — never the built-in fallback. Builtin (breaker
// on, 5 consecutive failures, action deny) is calibrated for external
// dependencies; applying it here would let five consecutive
// agent_work_complete / respond_to_user errors open a breaker that wedges the
// session rather than protecting it.
//
// The ceiling cases are the load-bearing ones: honoring a ceiling by simply
// resolving the full policy would drag Builtin's bare breaker in with it,
// because RuleFor's no-match fallback IS clamp(Builtin).
func TestToolGuardBuiltinDoesNotReachUngatedMetaTools(t *testing.T) {
	cases := []struct {
		name  string
		tiers toolguard.Tiers
	}{
		{
			name:  "no rules and no ceiling: every call reaches Execute",
			tiers: toolguard.Tiers{},
		},
		{
			name: "byte ceiling set (calls stay under it): the ceiling binds but Builtin's breaker does not",
			// A generous ingress ceiling turns guarding ON for this tool without
			// ever tripping; only Builtin's breaker could deny here, and it must not.
			tiers: ceiling(&v1.ToolGuardCeiling{MaxIngressBytes: ptr.To(int64(1 << 20))}),
		},
		{
			name: "breaker-knobs-only ceiling: minAction must not re-arm Builtin's breaker",
			// MinAction is a severity FLOOR on a rule's breaker action. With no
			// rule and Builtin deliberately out of reach, there is nothing to
			// floor — it must not conjure a breaker onto a meta tool.
			tiers: ceiling(&v1.ToolGuardCeiling{
				MinAction:           ptr.To(v1.ToolGuardActionHalt),
				MaxFailureThreshold: ptr.To(int32(2)),
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mt := &countingMetaTool{name: "recall_history", content: "boom", isError: true}
			l := metaGuardLoop(t, mt, tc.tiers)

			for i := 0; i < 8; i++ {
				res := dispatchMeta(t, l, "recall_history", "tu", 0)
				assert.True(t, res.IsError, "call %d surfaces the tool's own error", i+1)
				assert.Contains(t, res.Content, "boom", "call %d must carry the tool's error text, not a breaker deny", i+1)
			}
			assert.Equal(t, 8, mt.executed(),
				"the builtin breaker must not gate a meta tool: every call reached Execute")
		})
	}
}
