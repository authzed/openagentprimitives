package toolguard

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// nopHost satisfies pipeline.Host; Halt records the reason.
type nopHost struct{ haltReason string }

func (h *nopHost) PublishApproval(context.Context, pipeline.ApprovalAsk) (string, error) {
	return "", nil
}
func (h *nopHost) AwaitDecision(context.Context, string, time.Duration) (bool, string, bool, error) {
	return false, "", false, nil
}
func (h *nopHost) Notify(context.Context, pipeline.Notice) error          { return nil }
func (h *nopHost) SetStatus(context.Context, pipeline.StatusUpdate) error { return nil }
func (h *nopHost) Halt(_ context.Context, reason string) error {
	h.haltReason = reason
	return nil
}
func (h *nopHost) Audit(context.Context, []pipeline.AuditRecord) error { return nil }

// guardFixture builds a Guard backed by an empty-Tiers policy (builtin rules
// apply) and a recording audit sink.
func guardFixture(t *testing.T, clk *fakeClock) (*Guard, *Registry, *[]Event) {
	t.Helper()
	reg := NewRegistry(clk.now)
	var events []Event
	pol, err := ResolvePolicy(Tiers{})
	require.NoError(t, err)
	g := NewGuard(GuardDeps{
		Policy:   pol,
		Registry: reg,
		LookupTool: func(name string) (string, string) {
			return "mcp", "mcpserver/github"
		},
		TurnIndex:   func(context.Context) int { return 0 },
		RecordAudit: func(_ context.Context, ev Event) { events = append(events, ev) },
	})
	return g, reg, &events
}

func toolInput(name string) pipeline.Input {
	return pipeline.Input{
		Point:   pipeline.PreToolCall,
		Session: pipeline.SessionRef{Namespace: "ns", Name: "s", Class: "c"},
		Tool:    &pipeline.ToolCallInfo{Name: name, Args: json.RawMessage(`{}`), UseID: "tu_1"},
	}
}

func TestGuardAllowsWhenClosed(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	g, _, _ := guardFixture(t, clk)
	dec := g.Eval(context.Background(), toolInput("github_search"))
	assert.Equal(t, pipeline.Allow, dec.Verdict)
}

func TestGuardDeniesWhenOpenWithRetryAfterText(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	g, reg, events := guardFixture(t, clk)
	rule := Builtin
	// Trip the tool breaker (builtin threshold 5).
	for i := 0; i < 5; i++ {
		_, _ = reg.Admit(context.Background(), ToolKey("github_search"), OriginKey("mcpserver/github"), rule, 0)
		reg.Record(ToolKey("github_search"), OriginKey("mcpserver/github"), rule, true)
	}
	dec := g.Eval(context.Background(), toolInput("github_search"))
	require.Equal(t, pipeline.Deny, dec.Verdict)
	assert.Contains(t, dec.Reason, "github_search")
	assert.Contains(t, dec.Reason, "circuit breaker open")
	assert.Contains(t, dec.Reason, "30s")
	// Denial audited.
	require.NotEmpty(t, *events)
	last := (*events)[len(*events)-1]
	assert.Equal(t, "guard_deny", last.Event)
	assert.Equal(t, "github_search", last.Tool)
	assert.Equal(t, "tu_1", last.UseID)
}

func TestGuardEvalThroughExecutor(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	g, reg, _ := guardFixture(t, clk)
	preg := pipeline.NewRegistry()
	preg.Register(g, 15)
	exec := pipeline.NewExecutor(preg)

	out, err := exec.Run(context.Background(), pipeline.PreToolCall, toolInput("github_search"), &nopHost{})
	require.NoError(t, err)
	assert.Equal(t, pipeline.Allow, out.Verdict)

	rule := Builtin
	for i := 0; i < 5; i++ {
		_, _ = reg.Admit(context.Background(), ToolKey("github_search"), OriginKey("mcpserver/github"), rule, 0)
		reg.Record(ToolKey("github_search"), OriginKey("mcpserver/github"), rule, true)
	}
	out, err = exec.Run(context.Background(), pipeline.PreToolCall, toolInput("github_search"), &nopHost{})
	require.NoError(t, err)
	assert.Equal(t, pipeline.Deny, out.Verdict)
	assert.Equal(t, "tool_guard", out.FiredHook)
}

func TestGuardNonToolInputIsNoop(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	g, _, _ := guardFixture(t, clk)
	dec := g.Eval(context.Background(), pipeline.Input{Point: pipeline.PreToolCall})
	assert.Equal(t, pipeline.Allow, dec.Verdict)
}

func policyWithAction(t *testing.T, action string) *ResolvedPolicy {
	t.Helper()
	p, err := ResolvePolicy(Tiers{Class: &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
		{
			Match:   v1.ToolGuardMatch{Tool: "*"},
			Breaker: &v1.BreakerSpec{FailureThreshold: 1, Action: action},
		},
	}}})
	require.NoError(t, err)
	return p
}

func TestGuardHaltActionReturnsHaltVerdict(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	reg := NewRegistry(clk.now)
	g := NewGuard(GuardDeps{
		Policy: policyWithAction(t, "halt"), Registry: reg,
		LookupTool: func(string) (string, string) { return "mcp", "" },
		TurnIndex:  func(context.Context) int { return 0 },
	})
	rule := g.d.Policy.RuleFor("mcp", "x", "")
	_, _ = reg.Admit(context.Background(), ToolKey("x"), "", rule, 0)
	reg.Record(ToolKey("x"), "", rule, true) // threshold 1 → open
	dec := g.Eval(context.Background(), toolInput("x"))
	assert.Equal(t, pipeline.Halt, dec.Verdict)
	assert.Contains(t, dec.Reason, "tool_guard:")
}

func TestGuardWarnActionAllowsButAudits(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	reg := NewRegistry(clk.now)
	var events []Event
	g := NewGuard(GuardDeps{
		Policy: policyWithAction(t, "warn"), Registry: reg,
		LookupTool:  func(string) (string, string) { return "mcp", "" },
		TurnIndex:   func(context.Context) int { return 0 },
		RecordAudit: func(_ context.Context, ev Event) { events = append(events, ev) },
	})
	rule := g.d.Policy.RuleFor("mcp", "x", "")
	_, _ = reg.Admit(context.Background(), ToolKey("x"), "", rule, 0)
	reg.Record(ToolKey("x"), "", rule, true)
	dec := g.Eval(context.Background(), toolInput("x"))
	assert.Equal(t, pipeline.Allow, dec.Verdict, "warn never blocks")
	require.NotEmpty(t, events)
	assert.Equal(t, "guard_warn", events[len(events)-1].Event)
}

// egressInput builds a PreToolCall input whose serialized args are exactly n bytes.
func egressInput(name string, n int) pipeline.Input {
	args := make([]byte, n)
	for i := range args {
		args[i] = 'x'
	}
	return pipeline.Input{
		Point:   pipeline.PreToolCall,
		Session: pipeline.SessionRef{Namespace: "ns", Name: "s", Class: "c"},
		Tool:    &pipeline.ToolCallInfo{Name: name, Args: args, UseID: "tu_1"},
	}
}

// egressPolicy: a "*" rule with only a DataLimit (no breaker, no rate).
func egressPolicy(t *testing.T, maxEgress int64, action string) *ResolvedPolicy {
	t.Helper()
	p, err := ResolvePolicy(Tiers{Class: &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
		{Match: v1.ToolGuardMatch{Tool: "*"}, DataLimit: &v1.DataLimitSpec{MaxEgressBytes: maxEgress, Action: action}},
	}}})
	require.NoError(t, err)
	return p
}

func newEgressGuard(t *testing.T, clk *fakeClock, pol *ResolvedPolicy, events *[]Event) (*Guard, *Registry) {
	t.Helper()
	reg := NewRegistry(clk.now)
	g := NewGuard(GuardDeps{
		Policy: pol, Registry: reg,
		LookupTool:  func(string) (string, string) { return "mcp", "mcpserver/github" },
		TurnIndex:   func(context.Context) int { return 0 },
		RecordAudit: func(_ context.Context, ev Event) { *events = append(*events, ev) },
	})
	return g, reg
}

func TestGuardEgressUnderLimitAllows(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	var events []Event
	g, _ := newEgressGuard(t, clk, egressPolicy(t, 100, "deny"), &events)
	dec := g.Eval(context.Background(), egressInput("github_push", 50))
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.Empty(t, events)
}

func TestGuardEgressOverLimitDeniesAndAudits(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	var events []Event
	g, reg := newEgressGuard(t, clk, egressPolicy(t, 100, "deny"), &events)
	dec := g.Eval(context.Background(), egressInput("github_push", 250))
	require.Equal(t, pipeline.Deny, dec.Verdict)
	assert.Contains(t, dec.Reason, "github_push")
	assert.Contains(t, dec.Reason, "outbound limit")
	require.Len(t, events, 1)
	assert.Equal(t, "egress_limit_hit", events[0].Event)
	assert.Equal(t, "egress", events[0].Limit)
	assert.Equal(t, int64(250), events[0].ObservedBytes)
	// Egress denial must NOT consume a rate/breaker slot.
	assert.Empty(t, reg.OpenBreakers())
}

func TestGuardEgressWarnAllowsButAudits(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	var events []Event
	g, _ := newEgressGuard(t, clk, egressPolicy(t, 100, "warn"), &events)
	dec := g.Eval(context.Background(), egressInput("github_push", 250))
	assert.Equal(t, pipeline.Allow, dec.Verdict, "warn never blocks")
	require.Len(t, events, 1)
	assert.Equal(t, "guard_warn", events[0].Event)
	assert.Equal(t, int64(250), events[0].ObservedBytes)
}

func TestGuardEgressHaltReturnsHaltVerdict(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	var events []Event
	g, _ := newEgressGuard(t, clk, egressPolicy(t, 100, "halt"), &events)
	dec := g.Eval(context.Background(), egressInput("github_push", 250))
	assert.Equal(t, pipeline.Halt, dec.Verdict)
	assert.Contains(t, dec.Reason, "tool_guard:")
}
