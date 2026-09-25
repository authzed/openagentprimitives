package hooks

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// --- fakes -------------------------------------------------------------

type fakeRecorder struct{ recs []plangateaudit.Content }

func (f *fakeRecorder) Record(_ context.Context, c plangateaudit.Content) error {
	f.recs = append(f.recs, c)
	return nil
}

func (f *fakeRecorder) last(t *testing.T) plangateaudit.Content {
	t.Helper()
	require.NotEmpty(t, f.recs, "expected at least one recorded plan-gate event")
	return f.recs[len(f.recs)-1]
}

type fakeLogger struct{ msgs []string }

func (f *fakeLogger) Info(msg string, _ ...any) { f.msgs = append(f.msgs, msg) }

// --- helpers -----------------------------------------------------------

func permDesc(t *testing.T, permission, resourceType, tool string, si authz.StateImpact) permsurface.Descriptor {
	t.Helper()
	h, err := permsurface.NewPermHandle(permission, resourceType)
	require.NoError(t, err)
	return permsurface.Descriptor{
		Handle: h, Permission: permission, ResourceType: resourceType, StateImpact: si,
		Via: []permsurface.Provenance{{Tool: tool}},
	}
}

// demoSurface is a two-tool surface. resolveHandle below maps a tool name back
// to its handle, standing in for the runner's real projection.
func demoSurface(t *testing.T) []permsurface.Descriptor {
	t.Helper()
	return []permsurface.Descriptor{
		permDesc(t, "read", "tracker_issue", "read_issue", authz.Readonly),
		permDesc(t, "write", "tracker_issue", "update_issue", authz.Readwrite),
	}
}

func surfaceResolver(t *testing.T, surface []permsurface.Descriptor) HandleResolver {
	t.Helper()
	byTool := map[string]permsurface.Handle{}
	for _, d := range surface {
		for _, v := range d.Via {
			byTool[v.Tool] = d.Handle
		}
	}
	return func(toolName string, _ map[string]any) (permsurface.Handle, bool) {
		h, ok := byTool[toolName]
		return h, ok
	}
}

func evalTool(t *testing.T, h pipeline.Hook, toolName string) pipeline.Decision {
	t.Helper()
	return h.Eval(context.Background(), pipeline.Input{
		Point:   pipeline.PreToolCall,
		Session: pipeline.SessionRef{Namespace: "ns", Name: "demo-agent-1", Class: "demo-agent"},
		Tool:    &pipeline.ToolCallInfo{Name: toolName, UseID: "toolu_01"},
	})
}

func loggingGate(t *testing.T, rec *fakeRecorder, logs *fakeLogger) pipeline.Hook {
	t.Helper()
	surface := demoSurface(t)
	return NewPlanGate(PlanGateDeps{
		Mode:     "logging",
		Plan:     plangate.SessionPlan(surface),
		Resolve:  surfaceResolver(t, surface),
		Recorder: rec,
		Logger:   logs,
	})
}

// --- tests -------------------------------------------------------------

func TestPlanGate_pointsAndName(t *testing.T) {
	h := loggingGate(t, &fakeRecorder{}, &fakeLogger{})
	assert.Equal(t, "plan_gate", h.Name())
	assert.Equal(t, []pipeline.Point{pipeline.PreToolCall}, h.Points())
}

// THE slice-1 acceptance criterion: logging mode records what enforcing would
// have refused, and lets the call through regardless.
//
// Note the fixture. Producing a would-deny needs a tool that DOES resolve to a
// handle whose handle is NOT in the ceiling — a tool that resolves to no handle
// at all is a different case entirely (see the unhandleable test below), and
// conflating the two would leave this branch untested.
func TestPlanGate_loggingRecordsWouldDenyButNeverHalts(t *testing.T) {
	rec := &fakeRecorder{}
	inCeiling := demoSurface(t)
	outsideCeiling := permDesc(t, "delete", "tracker_issue", "delete_issue", authz.External)

	h := NewPlanGate(PlanGateDeps{
		Mode: "logging",
		// The plan authorizes only the demo surface...
		Plan: plangate.SessionPlan(inCeiling),
		// ...but the resolver can also name a handle outside it.
		Resolve:  surfaceResolver(t, append(append([]permsurface.Descriptor(nil), inCeiling...), outsideCeiling)),
		Recorder: rec,
		Logger:   &fakeLogger{},
	})

	d := evalTool(t, h, "delete_issue")

	assert.Equal(t, pipeline.Allow, d.Verdict, "logging mode must never halt a call")
	got := rec.last(t)
	assert.Equal(t, plangateaudit.OutcomeWouldDeny, got.Outcome,
		"but it must RECORD that enforcing would have denied")
	assert.Equal(t, "logging", got.Mode)
	assert.Equal(t, outsideCeiling.Handle.String(), got.Handle,
		"the record must name the handle that fell outside the ceiling")
}

// The zero-behavior-change proof at the unit level: every tool on the surface
// is inside the synthesized session ceiling.
func TestPlanGate_sessionPlanAdmitsEverySurfaceTool(t *testing.T) {
	surface := demoSurface(t)
	rec := &fakeRecorder{}
	h := NewPlanGate(PlanGateDeps{
		Mode: "logging", Plan: plangate.SessionPlan(surface),
		Resolve: surfaceResolver(t, surface), Recorder: rec,
	})

	for _, d := range surface {
		for _, v := range d.Via {
			t.Run(v.Tool+": allowed and recorded as allow", func(t *testing.T) {
				dec := evalTool(t, h, v.Tool)
				assert.Equal(t, pipeline.Allow, dec.Verdict)
				assert.Equal(t, plangateaudit.OutcomeAllow, rec.last(t).Outcome)
			})
		}
	}
}

func TestPlanGate_disabledIsOffEntirely(t *testing.T) {
	rec, logs := &fakeRecorder{}, &fakeLogger{}
	h := NewPlanGate(PlanGateDeps{Mode: "disabled", Recorder: rec, Logger: logs})

	d := evalTool(t, h, "anything")

	assert.Equal(t, pipeline.Allow, d.Verdict)
	assert.Empty(t, rec.recs, "disabled means no records and no overhead")
	assert.Empty(t, logs.msgs, "disabled must not log either")
}

// An empty mode string reaching the hook means the settings fold did not run.
// It must behave as disabled rather than as enforcing — the hook is registered
// by the wiring only when the mode is non-disabled, so a mode it cannot parse
// is a wiring bug, not a policy.
func TestPlanGate_emptyModeBehavesAsDisabled(t *testing.T) {
	rec := &fakeRecorder{}
	h := NewPlanGate(PlanGateDeps{Mode: "", Recorder: rec})

	assert.Equal(t, pipeline.Allow, evalTool(t, h, "anything").Verdict)
	assert.Empty(t, rec.recs)
}

// The hook attaches only at PreToolCall, but Input.Tool is nil at every other
// point. A nil deref here would panic the runner mid-session.
func TestPlanGate_nilToolIsIgnored(t *testing.T) {
	rec := &fakeRecorder{}
	h := loggingGate(t, rec, &fakeLogger{})

	d := h.Eval(context.Background(), pipeline.Input{Point: pipeline.PreToolCall})

	assert.Equal(t, pipeline.Allow, d.Verdict)
	assert.Empty(t, rec.recs)
}

// A gate that cannot resolve its ceiling must be LOUD, and in logging must
// still proceed. Silence in either direction is the failure mode.
func TestPlanGate_unresolvableCeilingLogsAndProceeds(t *testing.T) {
	rec, logs := &fakeRecorder{}, &fakeLogger{}
	h := NewPlanGate(PlanGateDeps{
		Mode:     "logging",
		Plan:     plangate.Plan{}, // no phases: Ceiling(0) errors
		Resolve:  surfaceResolver(t, demoSurface(t)),
		Recorder: rec,
		Logger:   logs,
	})

	d := evalTool(t, h, "read_issue")

	assert.Equal(t, pipeline.Allow, d.Verdict, "logging must not halt even on an internal error")
	assert.NotEmpty(t, logs.msgs, "an unresolvable ceiling must never fail silently")
}

// A tool with no handle at all (a meta tool, a passthrough) is not a plan-gate
// concern. It must record the fact rather than pretend the call had a handle.
func TestPlanGate_unhandleableToolRecordsWithoutAHandle(t *testing.T) {
	rec := &fakeRecorder{}
	h := loggingGate(t, rec, &fakeLogger{})

	evalTool(t, h, "update_plan")

	got := rec.last(t)
	assert.Empty(t, got.Handle, "no handle resolved, so none may be claimed")
	assert.Equal(t, "update_plan", got.Tool)
}

// Every record must name the active phase, and the seed plan's is always 0 —
// which is exactly why PhaseIndex is a pointer.
func TestPlanGate_recordsTheActivePhase(t *testing.T) {
	rec := &fakeRecorder{}
	h := loggingGate(t, rec, &fakeLogger{})

	evalTool(t, h, "read_issue")

	got := rec.last(t)
	require.NotNil(t, got.PhaseIndex, "a per-call record must name its phase")
	assert.Equal(t, int32(0), *got.PhaseIndex)
	assert.NotEmpty(t, got.PlanDigest, "and must pin which plan it was gated against")
}

// A recorder failure must not take the call down in logging mode, but must not
// be swallowed either.
func TestPlanGate_recorderFailureIsLoggedNotFatal(t *testing.T) {
	logs := &fakeLogger{}
	surface := demoSurface(t)
	h := NewPlanGate(PlanGateDeps{
		Mode: "logging", Plan: plangate.SessionPlan(surface),
		Resolve: surfaceResolver(t, surface), Recorder: failingRecorder{}, Logger: logs,
	})

	d := evalTool(t, h, "read_issue")

	assert.Equal(t, pipeline.Allow, d.Verdict)
	assert.NotEmpty(t, logs.msgs, "a dropped audit write must be logged")
}

type failingRecorder struct{}

func (failingRecorder) Record(context.Context, plangateaudit.Content) error {
	return assertErr{}
}

type assertErr struct{}

func (assertErr) Error() string { return "recorder unavailable" }

// No card is published, so no human approves, so no grant is written. The
// property is structural — there is no publisher and no grant writer wired into
// this hook at all.
func TestPlanGate_loggingPublishesNothing(t *testing.T) {
	rec := &fakeRecorder{}
	h := loggingGate(t, rec, &fakeLogger{})

	d := evalTool(t, h, "unknown_tool")

	assert.Nil(t, d.Approval, "logging must never raise an approval ask")
	assert.Empty(t, d.Notices, "and must not notify anyone")
}

// The lead is the first thing a human reads. It used to join raw handles
// ("perm:fetch:git_repo, perm:push:git_repo, …"); it must instead name the
// strongest tier the card carries, COMPUTED from the card's own structure —
// never a wire-format join.
func TestPlanGateLead_NamesTheAskNotTheHandles(t *testing.T) {
	lead := planGateLead("", 2, plangate.Card{Phases: []plangate.CardPhase{
		{Permissions: []plangate.CardLine{{Text: "Read the repository", Impact: "readonly"}}},
		{Permissions: []plangate.CardLine{{Text: "Push commits to the repository", Impact: "external"}}},
	}})

	assert.NotContains(t, lead, "perm:", "no wire format in the lead")
	assert.Contains(t, lead, "2-phase plan")
	assert.Contains(t, lead, "leaves this session",
		"the lead names the strongest tier present, so it cannot understate the ask")
}

func TestPlanGateLead_ReadOnlyPlanSaysSo(t *testing.T) {
	lead := planGateLead("this phase", 1, plangate.Card{Phases: []plangate.CardPhase{
		{Permissions: []plangate.CardLine{{Text: "Read the repository", Impact: "readonly"}}},
	}})
	assert.NotContains(t, lead, "leaves this session")
}
