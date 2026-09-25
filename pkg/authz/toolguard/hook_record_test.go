package toolguard

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// newTestGuardRecord builds a GuardRecord from a single class rule matching
// every tool ("*") with the supplied DataLimit, so UI-vs-model ingress
// ceiling behavior can be exercised without breaker/rate noise.
func newTestGuardRecord(t *testing.T, dl *v1.DataLimitSpec) *GuardRecord {
	t.Helper()
	p, err := ResolvePolicy(Tiers{Class: &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
		{Match: v1.ToolGuardMatch{Tool: "*"}, DataLimit: dl},
	}}})
	require.NoError(t, err)
	return NewGuardRecord(RecordDeps{
		Policy:   p,
		Registry: NewRegistry(nil),
		LookupTool: func(string) (string, string) {
			return "mcp", "mcpserver/crm"
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func recordFixture(t *testing.T, clk *fakeClock) (*GuardRecord, *Registry, *[]Event, *[][]OpenBreakerInfo) {
	t.Helper()
	reg := NewRegistry(clk.now)
	pol, err := ResolvePolicy(Tiers{})
	require.NoError(t, err)
	var events []Event
	var patches [][]OpenBreakerInfo
	h := NewGuardRecord(RecordDeps{
		Policy:   pol,
		Registry: reg,
		LookupTool: func(string) (string, string) {
			return "mcp", "mcpserver/github"
		},
		RecordAudit: func(_ context.Context, ev Event) { events = append(events, ev) },
		PatchStatus: func(_ context.Context, snap []OpenBreakerInfo) { patches = append(patches, snap) },
	})
	return h, reg, &events, &patches
}

func postInput(name string, isErr bool) pipeline.Input {
	return pipeline.Input{
		Point:   pipeline.PostToolCall,
		Session: pipeline.SessionRef{Namespace: "ns", Name: "s", Class: "c"},
		Tool:    &pipeline.ToolCallInfo{Name: name, UseID: "tu_1", IsError: isErr},
	}
}

func TestGuardRecordCountsFailuresAndPatchesStatusOnTrip(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	h, _, events, patches := recordFixture(t, clk)
	// Builtin threshold is 5: four failures, no transitions yet.
	for i := 0; i < 4; i++ {
		dec := h.Eval(context.Background(), postInput("github_search", true))
		assert.Equal(t, pipeline.Allow, dec.Verdict, "recording never blocks")
	}
	assert.Empty(t, *events)
	assert.Empty(t, *patches)
	// Fifth failure trips the tool breaker.
	h.Eval(context.Background(), postInput("github_search", true))
	require.NotEmpty(t, *events)
	assert.Equal(t, "breaker_opened", (*events)[0].Event)
	assert.Equal(t, "tool/github_search", (*events)[0].Key)
	require.Len(t, *patches, 1)
	require.Len(t, (*patches)[0], 1)
	assert.Equal(t, "tool/github_search", (*patches)[0][0].Key)
}

func TestGuardRecordSuccessClosesAndPatches(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	h, _, events, patches := recordFixture(t, clk)
	for i := 0; i < 5; i++ {
		h.Eval(context.Background(), postInput("github_search", true))
	}
	clk.t = clk.t.Add(time.Minute)
	h.Eval(context.Background(), postInput("github_search", false))
	last := (*events)[len(*events)-1]
	assert.Equal(t, "breaker_closed", last.Event)
	// Status patched empty after close.
	assert.Empty(t, (*patches)[len(*patches)-1])
}

func TestHookLevelHalfOpenProbeCycle(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	reg := NewRegistry(clk.now)
	pol, err := ResolvePolicy(Tiers{})
	require.NoError(t, err)
	lookup := func(string) (string, string) { return "mcp", "mcpserver/github" }
	guard := NewGuard(GuardDeps{Policy: pol, Registry: reg, LookupTool: lookup,
		TurnIndex: func(context.Context) int { return 0 }})
	var events []Event
	rec := NewGuardRecord(RecordDeps{Policy: pol, Registry: reg, LookupTool: lookup,
		RecordAudit: func(_ context.Context, ev Event) { events = append(events, ev) }})

	// Trip the builtin tool breaker (threshold 5) through the hooks.
	for i := 0; i < 5; i++ {
		require.Equal(t, pipeline.Allow, guard.Eval(context.Background(), toolInput("github_search")).Verdict)
		rec.Eval(context.Background(), postInput("github_search", true))
	}
	// Open: denied.
	require.Equal(t, pipeline.Deny, guard.Eval(context.Background(), toolInput("github_search")).Verdict)

	// Cool-off elapses: exactly one probe admitted; a concurrent call denied.
	clk.t = clk.t.Add(31 * time.Second)
	require.Equal(t, pipeline.Allow, guard.Eval(context.Background(), toolInput("github_search")).Verdict)
	assert.Equal(t, pipeline.Deny, guard.Eval(context.Background(), toolInput("github_search")).Verdict,
		"second call during probe must be denied")

	// Probe succeeds → closed; next call admitted.
	rec.Eval(context.Background(), postInput("github_search", false))
	assert.Equal(t, pipeline.Allow, guard.Eval(context.Background(), toolInput("github_search")).Verdict)
	last := events[len(events)-1]
	assert.Equal(t, "breaker_closed", last.Event)
}

func TestGuardRecordSkipsUnknownToolsAndNonToolInput(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	reg := NewRegistry(clk.now)
	pol, err := ResolvePolicy(Tiers{})
	require.NoError(t, err)
	h := NewGuardRecord(RecordDeps{
		Policy: pol, Registry: reg,
		LookupTool: func(string) (string, string) { return "", "" },
	})
	assert.Equal(t, pipeline.Allow, h.Eval(context.Background(), postInput("nope", true)).Verdict)
	assert.Equal(t, pipeline.Allow, h.Eval(context.Background(), pipeline.Input{Point: pipeline.PostToolCall}).Verdict)
}

func postInputWithResult(name string, isErr bool, resultBytes int) pipeline.Input {
	res := make([]byte, resultBytes)
	for i := range res {
		res[i] = 'y'
	}
	return pipeline.Input{
		Point:   pipeline.PostToolCall,
		Session: pipeline.SessionRef{Namespace: "ns", Name: "s", Class: "c"},
		Tool:    &pipeline.ToolCallInfo{Name: name, UseID: "tu_1", IsError: isErr, Result: string(res)},
	}
}

// ingressRecord: a GuardRecord whose policy sets ONLY an ingress limit on "*"
// (no breaker), so the widened early-return path is exercised.
func ingressRecord(t *testing.T, maxIngress int64, action string, events *[]Event) *GuardRecord {
	t.Helper()
	p, err := ResolvePolicy(Tiers{Class: &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
		{Match: v1.ToolGuardMatch{Tool: "*"}, DataLimit: &v1.DataLimitSpec{MaxIngressBytes: maxIngress, Action: action}},
	}}})
	require.NoError(t, err)
	return NewGuardRecord(RecordDeps{
		Policy: p, Registry: NewRegistry(nil),
		LookupTool:  func(string) (string, string) { return "mcp", "mcpserver/github" },
		RecordAudit: func(_ context.Context, ev Event) { *events = append(*events, ev) },
	})
}

func TestGuardRecordIngressUnderLimitAllows(t *testing.T) {
	var events []Event
	h := ingressRecord(t, 100, "deny", &events)
	dec := h.Eval(context.Background(), postInputWithResult("github_read", false, 50))
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.Empty(t, events)
}

func TestGuardRecordIngressOverLimitWithholdsAndAudits(t *testing.T) {
	var events []Event
	h := ingressRecord(t, 100, "deny", &events)
	dec := h.Eval(context.Background(), postInputWithResult("github_read", false, 500))
	require.Equal(t, pipeline.Deny, dec.Verdict)
	assert.Contains(t, dec.Reason, "github_read")
	assert.Contains(t, dec.Reason, "withheld")
	require.Len(t, events, 1)
	assert.Equal(t, "ingress_limit_hit", events[0].Event)
	assert.Equal(t, "ingress", events[0].Limit)
	assert.Equal(t, int64(500), events[0].ObservedBytes)
}

func TestGuardRecordIngressCapsErrorResultsToo(t *testing.T) {
	var events []Event
	h := ingressRecord(t, 100, "deny", &events)
	// A server can mark an unbounded exfil payload IsError, so error results are
	// NOT exempt — an oversized IsError result is still withheld + audited.
	dec := h.Eval(context.Background(), postInputWithResult("github_read", true, 500))
	require.Equal(t, pipeline.Deny, dec.Verdict)
	require.Len(t, events, 1)
	assert.Equal(t, "ingress_limit_hit", events[0].Event)
	assert.Equal(t, int64(500), events[0].ObservedBytes)
}

func TestGuardRecordIngressWarnAllowsFullResult(t *testing.T) {
	var events []Event
	h := ingressRecord(t, 100, "warn", &events)
	dec := h.Eval(context.Background(), postInputWithResult("github_read", false, 500))
	assert.Equal(t, pipeline.Allow, dec.Verdict, "warn lets the full result through")
	require.Len(t, events, 1)
	assert.Equal(t, "guard_warn", events[0].Event)
}

// TestGuardRecordIngressWarnCannotEscapeTheCeiling is the enforcement half of
// TestCeilingFloorsTheVolumeActionItBounds: a class author (namespace-writable
// AgentClass.spec.toolGuard) writing `action: warn` must not turn a cluster
// admin's ingress ceiling into a log line. Warn returns pipeline.Decision{} —
// the oversized third-party payload reaches the model — which is precisely what
// an admin capping maxIngressBytes is trying to stop.
func TestGuardRecordIngressWarnCannotEscapeTheCeiling(t *testing.T) {
	p, err := ResolvePolicy(Tiers{
		Class: &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
			{Match: v1.ToolGuardMatch{Tool: "*"}, DataLimit: &v1.DataLimitSpec{MaxIngressBytes: 1 << 20, Action: "warn"}},
		}},
		Ceiling: &v1.ToolGuardCeiling{MaxIngressBytes: i64(100)},
	})
	require.NoError(t, err)
	var events []Event
	h := NewGuardRecord(RecordDeps{
		Policy: p, Registry: NewRegistry(nil),
		LookupTool:  func(string) (string, string) { return "mcp", "mcpserver/github" },
		RecordAudit: func(_ context.Context, ev Event) { events = append(events, ev) },
	})

	dec := h.Eval(context.Background(), postInputWithResult("github_read", false, 500))
	assert.Equal(t, pipeline.Deny, dec.Verdict, "the ceiling's byte bound must withhold the result, not warn about it")
	require.Len(t, events, 1)
	assert.Equal(t, "ingress_limit_hit", events[0].Event)
	assert.Equal(t, "deny", events[0].Action)
}

// Breaker recording must observe the TRUE outcome before the byte verdict:
// a successful-but-oversized result still closes/keeps-closed the breaker.
func TestGuardRecordIngressDenyStillRecordsTrueOutcome(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	reg := NewRegistry(clk.now)
	// Rule with BOTH a breaker (threshold 5) and an ingress limit.
	p, err := ResolvePolicy(Tiers{Class: &v1.ToolGuardPolicy{Rules: []v1.ToolGuardRule{
		{
			Match:     v1.ToolGuardMatch{Tool: "*"},
			Breaker:   &v1.BreakerSpec{FailureThreshold: 5},
			DataLimit: &v1.DataLimitSpec{MaxIngressBytes: 100, Action: "deny"},
		},
	}}})
	require.NoError(t, err)
	var events []Event
	h := NewGuardRecord(RecordDeps{
		Policy: p, Registry: reg,
		LookupTool:  func(string) (string, string) { return "mcp", "mcpserver/github" },
		RecordAudit: func(_ context.Context, ev Event) { events = append(events, ev) },
	})
	// First, 4 real failures accumulate (no trip yet).
	for i := 0; i < 4; i++ {
		h.Eval(context.Background(), postInput("x", true))
	}
	// A successful-but-oversized call: ingress denies, but the breaker sees a
	// SUCCESS and resets consecutive failures (no trip on the next failure alone).
	dec := h.Eval(context.Background(), postInputWithResult("x", false, 500))
	require.Equal(t, pipeline.Deny, dec.Verdict)
	// Prove the success reset the counter: one more failure must NOT trip (needs 5).
	h.Eval(context.Background(), postInput("x", true))
	assert.Empty(t, reg.OpenBreakers(), "success between failures reset the breaker counter")
}

func TestGuardRecordUIIngressCeiling(t *testing.T) {
	big := strings.Repeat("x", 5000)
	cases := []struct {
		name          string
		dataLimit     *v1.DataLimitSpec
		uiDataBinding bool
		wantVerdict   pipeline.Verdict
	}{
		{name: "model path under a 4096-byte model cap: denied",
			dataLimit: &v1.DataLimitSpec{MaxIngressBytes: 4096}, wantVerdict: pipeline.Deny},
		{name: "UI path is not bound by the model cap: allowed",
			dataLimit: &v1.DataLimitSpec{MaxIngressBytes: 4096}, uiDataBinding: true,
			wantVerdict: pipeline.Allow},
		{name: "UI path IS bound by its own cap: denied",
			dataLimit:     &v1.DataLimitSpec{MaxIngressBytes: 4096, MaxUIIngressBytes: 1024},
			uiDataBinding: true, wantVerdict: pipeline.Deny},
		{name: "UI path with no configured cap falls to the platform default: allowed at 5000 bytes",
			dataLimit: nil, uiDataBinding: true, wantVerdict: pipeline.Allow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestGuardRecord(t, tc.dataLimit)
			dec := h.Eval(context.Background(), pipeline.Input{
				Point:         pipeline.PostToolCall,
				UIDataBinding: tc.uiDataBinding,
				Tool:          &pipeline.ToolCallInfo{Name: "crm_list_leads", Result: big},
			})
			assert.Equal(t, tc.wantVerdict, dec.Verdict)
		})
	}
}
