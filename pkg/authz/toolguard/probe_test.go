package toolguard

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// The hooks below stand in for the PreToolCall gates that run AFTER tool_guard
// (content_guard 18, tool_call_authz 20, scope 30). The executor returns at the
// first non-Allow verdict, so when one of them fires, the contained call never
// reaches PostToolCall — and nothing records an outcome for the half-open probe
// tool_guard just admitted.

// approvalDeny asks for approval and is refused (nopHost.AwaitDecision answers
// "not approved"), which the executor turns into a Deny. This is the audit's
// most reachable trigger: a human-denied or timed-out approval on the one call
// holding the probe claim.
type approvalDeny struct{}

func (approvalDeny) Name() string             { return "downstream_approval" }
func (approvalDeny) Points() []pipeline.Point { return []pipeline.Point{pipeline.PreToolCall} }
func (approvalDeny) Eval(context.Context, pipeline.Input) pipeline.Decision {
	return pipeline.Decision{Approval: &pipeline.ApprovalAsk{Kind: "tool_call", Summary: "run it?"}}
}

// downstreamHalt is the Pre Halt twin (loop.go's containPreHalt exit).
type downstreamHalt struct{}

func (downstreamHalt) Name() string             { return "downstream_halt" }
func (downstreamHalt) Points() []pipeline.Point { return []pipeline.Point{pipeline.PreToolCall} }
func (downstreamHalt) Eval(context.Context, pipeline.Input) pipeline.Decision {
	return pipeline.Decision{Verdict: pipeline.Halt, Reason: "content guard halt"}
}

// containedCall mirrors pkg/agent/runner.executeToolContained: exactly one
// probe ledger per contained call, released however the call exits. It stops
// after PreToolCall, which is what every outcome-less exit does — a Pre Deny, a
// Pre Halt, and a user interrupt after execute all return before the
// PostToolCall run that is Registry.Record's only production caller.
func containedCall(t *testing.T, exec *pipeline.Executor, in pipeline.Input) pipeline.Outcome {
	t.Helper()
	ctx, releaseProbes := WithProbeLedger(context.Background())
	defer releaseProbes()
	out, err := exec.Run(ctx, pipeline.PreToolCall, in, &nopHost{})
	require.NoError(t, err, "PreToolCall executor must reach a verdict")
	return out
}

// failCalls drives n admitted-then-failed calls for one tool key, so callers
// can walk a tool breaker (or, spread across tools, an origin breaker) toward
// its threshold.
func failCalls(t *testing.T, reg *Registry, rule ResolvedRule, tool, origin string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		adm, _ := reg.Admit(context.Background(), ToolKey(tool), OriginKey(origin), rule, 0)
		require.True(t, adm.Allowed, "setup call %d on %s must admit", i, tool)
		reg.Record(ToolKey(tool), OriginKey(origin), rule, true)
	}
}

// halfOpenGuard returns an executor whose github_search tool breaker has just
// re-opened its probe slot: tripped (builtin threshold 5) and cooled off, so
// the next admitted call claims the half-open probe. downstream, when non-nil,
// is registered after tool_guard.
func halfOpenGuard(t *testing.T, downstream pipeline.Hook, logger *slog.Logger) *pipeline.Executor {
	t.Helper()
	clk := &fakeClock{t: time.Unix(1000, 0)}
	reg := NewRegistry(clk.now)
	pol, err := ResolvePolicy(Tiers{})
	require.NoError(t, err)
	g := NewGuard(GuardDeps{
		Policy:     pol,
		Registry:   reg,
		LookupTool: func(string) (string, string) { return "mcp", "mcpserver/github" },
		TurnIndex:  func(context.Context) int { return 0 },
		Logger:     logger,
	})
	preg := pipeline.NewRegistry()
	preg.Register(g, 15)
	if downstream != nil {
		preg.Register(downstream, 20)
	}
	failCalls(t, reg, Builtin, "github_search", "mcpserver/github", 5)
	clk.t = clk.t.Add(31 * time.Second) // cool-off elapsed → next call probes
	return pipeline.NewExecutor(preg)
}

// TestProbeClaimHandedBackOnEveryOutcomelessExit covers the three ways
// executeToolContained returns without ever running PostToolCall. In each, the
// call that took the half-open probe reports no outcome, so the claim must die
// with the call — otherwise checkBreaker's stateHalfOpen branch denies every
// later call on that key with deniedBy "probe", and half-open has no cool-off
// timer to ever undo it.
func TestProbeClaimHandedBackOnEveryOutcomelessExit(t *testing.T) {
	cases := []struct {
		name          string
		downstream    pipeline.Hook
		wantVerdict   pipeline.Verdict
		wantFiredHook string
	}{
		{
			name:          "pre-deny (approval refused): the next call still reaches the approval gate, not a stale probe denial",
			downstream:    approvalDeny{},
			wantVerdict:   pipeline.Deny,
			wantFiredHook: "downstream_approval",
		},
		{
			name:          "pre-halt: the next call still reaches the halting gate, not a stale probe denial",
			downstream:    downstreamHalt{},
			wantVerdict:   pipeline.Halt,
			wantFiredHook: "downstream_halt",
		},
		{
			name:          "user interrupt after execute (no PostToolCall): the next call is still admitted",
			downstream:    nil,
			wantVerdict:   pipeline.Allow,
			wantFiredHook: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := halfOpenGuard(t, tc.downstream, discardLogger())
			in := toolInput("github_search")

			// The probe call: tool_guard admits it (claiming the half-open
			// probe) and the call ends without an outcome.
			out := containedCall(t, exec, in)
			require.Equal(t, tc.wantVerdict, out.Verdict)
			require.Equal(t, tc.wantFiredHook, out.FiredHook, "tool_guard must have admitted the probe call")

			// The claim died with that call.
			out = containedCall(t, exec, in)
			assert.NotEqual(t, "tool_guard", out.FiredHook,
				"a stranded probe claim wedges the tool for the rest of the session")
			assert.Equal(t, tc.wantVerdict, out.Verdict)
			assert.Equal(t, tc.wantFiredHook, out.FiredHook)
		})
	}
}

func TestStrandedOriginProbeWouldKillEveryToolOfThatOrigin(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	g, reg, _ := guardFixture(t, clk)
	preg := pipeline.NewRegistry()
	preg.Register(g, 15)
	preg.Register(approvalDeny{}, 20)
	exec := pipeline.NewExecutor(preg)

	// Trip the ORIGIN breaker (builtin threshold 10) with failures spread thin
	// enough that no single tool reaches its own threshold (builtin 5).
	failCalls(t, reg, Builtin, "github_search", "mcpserver/github", 4)
	failCalls(t, reg, Builtin, "github_push", "mcpserver/github", 4)
	failCalls(t, reg, Builtin, "github_issues", "mcpserver/github", 2)
	clk.t = clk.t.Add(31 * time.Second)

	out := containedCall(t, exec, toolInput("github_search"))
	require.Equal(t, "downstream_approval", out.FiredHook, "tool_guard must have admitted the origin probe call")

	// A sibling tool of the same origin must still be reachable: the origin
	// probe was claimed by a call that never reported an outcome.
	out = containedCall(t, exec, toolInput("github_issues"))
	assert.Equal(t, "downstream_approval", out.FiredHook,
		"a stranded origin probe claim kills every tool of that MCP server")
}

func TestReleaseAfterRecordDoesNotStealANewerProbe(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	reg := NewRegistry(clk.now)
	rule := testRule() // threshold 3, initial cool-off 30s
	const tk = "tool/x"
	for i := 0; i < 3; i++ {
		_, _ = reg.Admit(context.Background(), tk, "", rule, 0)
		reg.Record(tk, "", rule, true)
	}

	// Call A takes the probe and reports a failure: Record resolves the claim
	// and re-opens the breaker.
	clk.t = clk.t.Add(31 * time.Second)
	ctxA, releaseA := WithProbeLedger(context.Background())
	admA, _ := reg.Admit(ctxA, tk, "", rule, 0)
	require.True(t, admA.Probe)
	reg.Record(tk, "", rule, true)

	// The doubled cool-off elapses and call B legitimately takes the next probe.
	clk.t = clk.t.Add(61 * time.Second)
	ctxB, releaseB := WithProbeLedger(context.Background())
	admB, _ := reg.Admit(ctxB, tk, "", rule, 0)
	require.True(t, admB.Probe)

	// A's deferred release runs late. It must not hand back B's live slot.
	releaseA()
	admC, _ := reg.Admit(context.Background(), tk, "", rule, 0)
	assert.False(t, admC.Allowed, "B's probe is still in flight")
	assert.Equal(t, "probe", admC.DeniedBy)

	// B's own release does free it.
	releaseB()
	admD, _ := reg.Admit(context.Background(), tk, "", rule, 0)
	assert.True(t, admD.Allowed)
	assert.True(t, admD.Probe)
}

func TestReleaseAfterSuccessfulProbeLeavesBreakerClosed(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	reg := NewRegistry(clk.now)
	rule := testRule()
	const tk = "tool/x"
	for i := 0; i < 3; i++ {
		_, _ = reg.Admit(context.Background(), tk, "", rule, 0)
		reg.Record(tk, "", rule, true)
	}
	clk.t = clk.t.Add(31 * time.Second)

	ctx, release := WithProbeLedger(context.Background())
	adm, _ := reg.Admit(ctx, tk, "", rule, 0)
	require.True(t, adm.Probe)
	trans := reg.Record(tk, "", rule, false) // probe succeeded → closed
	require.Len(t, trans, 1)
	require.Equal(t, "breaker_closed", trans[0].Event)

	// The contained call's deferred release still runs on the success path; it
	// must not drag the closed breaker back to half-open.
	release()
	adm, _ = reg.Admit(context.Background(), tk, "", rule, 0)
	assert.True(t, adm.Allowed)
	assert.False(t, adm.Probe, "a closed breaker admits normally, it does not probe")
}

func TestConcurrentContainedCallsNeverStrandTheProbe(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	reg := NewRegistry(clk.now)
	rule := testRule()
	const tk = "tool/x"
	for i := 0; i < 3; i++ {
		_, _ = reg.Admit(context.Background(), tk, "", rule, 0)
		reg.Record(tk, "", rule, true)
	}
	clk.t = clk.t.Add(31 * time.Second)

	// Every call abandons its claim (the denied/halted/interrupted shape).
	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	probes := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, release := WithProbeLedger(context.Background())
			defer release()
			if adm, _ := reg.Admit(ctx, tk, "", rule, 0); adm.Probe {
				mu.Lock()
				probes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	assert.GreaterOrEqual(t, probes, 1, "at least one call should have taken the probe")
	adm, _ := reg.Admit(context.Background(), tk, "", rule, 0)
	assert.True(t, adm.Allowed, "no abandoned claim may survive its call")
	assert.True(t, adm.Probe)
}

func TestUnledgeredProbeClaimIsLoggedNotSwallowed(t *testing.T) {
	// A call site that forgot WithProbeLedger reintroduces the wedge, so the
	// guard must say so rather than hand out an unreleasable claim in silence
	// (AGENTS.md: never silently drop errors).
	var buf bytes.Buffer
	exec := halfOpenGuard(t, approvalDeny{}, slog.New(slog.NewTextHandler(&buf, nil)))

	out, err := exec.Run(context.Background(), pipeline.PreToolCall, toolInput("github_search"), &nopHost{})
	require.NoError(t, err)
	require.Equal(t, "downstream_approval", out.FiredHook, "tool_guard must have admitted the probe call")

	assert.Contains(t, buf.String(), "no probe ledger",
		"an unledgered probe claim must be surfaced, not swallowed")
}

// discardLogger keeps the guard's INFO chatter out of test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
