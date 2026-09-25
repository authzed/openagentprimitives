package runner

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/synthesize"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// appToolFixture selects which single app tool newAppToolFixture registers.
type appToolFixture struct {
	// readonly registers "demo_readonly" (StateImpact Readonly, server hint
	// true — satisfies the auto-run predicate) when true, or "demo_mutating"
	// (StateImpact Readwrite) when false, so the readonly-gate denial tests
	// have a genuinely side-effecting granted tool to deny.
	readonly bool

	// The fields below are consumed only by newUIActionFixture
	// (uiactionrecord_test.go), which builds a full approval flow on top of
	// this same shape. newAppToolFixture ignores them; their zero values keep
	// every existing appToolFixture{readonly: ...} literal unchanged.

	// approve is the decision a background watcher goroutine delivers once a
	// side-effecting call's approval is pending. Ignored when
	// approvalTimesOut is true.
	approve bool
	// approvalTimesOut, when true, means no decision is ever delivered: the
	// fixture's (short) approval timeout is left to lapse on its own.
	approvalTimesOut bool
	// approverIsRequester puts the fixture's authorized viewer ("user:viewer")
	// into the resolved approver set, so the approval is ADDRESSED TO that
	// viewer rather than to someone else.
	approverIsRequester bool
	// memoryWriteFails forces every ui_action memory write to fail, for the
	// write-before-push ordering test.
	memoryWriteFails bool
	// noPublisher leaves Loop.UIPublish nil, for the memory-only
	// degrade-gracefully test.
	noPublisher bool
	// resultText overrides the fixture's fakeAppTool result content ("body"
	// when empty), for the never-leaks-the-raw-result test.
	resultText string
}

// uiFixtureRecorder is the pipeline.Hook newAppToolFixture registers at BOTH
// containment points; it backs observedPipelineInputsForTest. Registering at
// both PreToolCall and PostToolCall means the one synchronous readonly
// auto-run call these tests drive (executeToolContained runs both points
// before returning) is observed twice, once per point.
type uiFixtureRecorder struct {
	mu   sync.Mutex
	seen []pipeline.Input
}

func (r *uiFixtureRecorder) Name() string { return "ui_fixture_recorder" }
func (r *uiFixtureRecorder) Points() []pipeline.Point {
	return []pipeline.Point{pipeline.PreToolCall, pipeline.PostToolCall}
}
func (r *uiFixtureRecorder) Eval(_ context.Context, in pipeline.Input) pipeline.Decision {
	r.mu.Lock()
	r.seen = append(r.seen, in)
	r.mu.Unlock()
	return pipeline.Decision{}
}
func (r *uiFixtureRecorder) inputs() []pipeline.Input {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]pipeline.Input(nil), r.seen...)
}

// fakeUIMemory is a MemoryAppender that records every appended turn, so
// transcriptLenForTest can prove a data binding never writes one (D-B6: the
// result reaches the browser over the wire only, never the LLM transcript).
type fakeUIMemory struct {
	mu    sync.Mutex
	turns []memory.Turn
}

func (m *fakeUIMemory) ReadAll(context.Context) ([]memory.Turn, error) { return nil, nil }
func (m *fakeUIMemory) ReadAfter(context.Context, int) ([]memory.Turn, error) {
	return nil, nil
}
func (m *fakeUIMemory) Append(_ context.Context, t memory.Turn) error {
	m.mu.Lock()
	m.turns = append(m.turns, t)
	m.mu.Unlock()
	return nil
}
func (m *fakeUIMemory) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.turns)
}

// uiFixtureRig backs the …ForTest accessors below for one fixture Loop.
// approvalAsks counts calls to Loop.approvalAskSpawnHookForTest, which
// handleAppToolCallReq invokes SYNCHRONOUSLY — on the caller's own goroutine,
// before it spawns the detached side-effecting goroutine — so the count is
// race-free by construction (an assertion made immediately after
// HandleUIDataBinding returns needs no Eventually).
type uiFixtureRig struct {
	mu           sync.Mutex
	approvalAsks int
	recorder     *uiFixtureRecorder
	mem          *fakeUIMemory
}

// uiFixtureRigs indexes a fixture Loop to its rig. The …ForTest accessors
// are methods on *Loop (so call sites read l.approvalAsksForTest()
// naturally), but the recording state is test-only and does not belong on
// the production Loop struct — this keyed side-table is the seam instead.
var (
	uiFixtureRigsMu sync.Mutex
	uiFixtureRigs   = map[*Loop]*uiFixtureRig{}
)

// approvalAsksForTest reports how many times this fixture Loop parked a
// side-effecting call for approval (handleAppToolCallReq's detached-spawn
// branch). Zero for a Loop never built via newAppToolFixture.
func (l *Loop) approvalAsksForTest() int {
	uiFixtureRigsMu.Lock()
	rig := uiFixtureRigs[l]
	uiFixtureRigsMu.Unlock()
	if rig == nil {
		return 0
	}
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return rig.approvalAsks
}

// transcriptLenForTest reports how many turns this fixture Loop's Memory has
// recorded. Zero for a Loop never built via newAppToolFixture.
func (l *Loop) transcriptLenForTest() int {
	uiFixtureRigsMu.Lock()
	rig := uiFixtureRigs[l]
	uiFixtureRigsMu.Unlock()
	if rig == nil || rig.mem == nil {
		return 0
	}
	return rig.mem.len()
}

// observedPipelineInputsForTest returns every pipeline.Input this fixture
// Loop's containment pipeline has evaluated so far, in order. Nil for a Loop
// never built via newAppToolFixture.
func (l *Loop) observedPipelineInputsForTest() []pipeline.Input {
	uiFixtureRigsMu.Lock()
	rig := uiFixtureRigs[l]
	uiFixtureRigsMu.Unlock()
	if rig == nil || rig.recorder == nil {
		return nil
	}
	return rig.recorder.inputs()
}

// newAppToolFixture builds a Loop wired with ONE granted app tool (selected
// by appToolFixture.readonly), an allowing InteractChecker, and the
// recording seams the …ForTest accessors above read. This is the shared
// shape TestHandleUIDataBinding's cases need; TestHandleAppToolCall's own
// per-scenario setups (denial modes, rate limiting, pipeline hooks) stay as
// they are in apptoolcall_test.go, since those vary too much per case to
// collapse into this one fixture without losing what each subtest pins.
func newAppToolFixture(t *testing.T, f appToolFixture) (*Loop, InteractChecker, context.Context) {
	t.Helper()
	const ns, name = "demo-ns", "demo-session"

	toolName := "demo_readonly"
	perm := authz.Permission{StateImpact: authz.Readonly}
	if !f.readonly {
		toolName = "demo_mutating"
		perm = authz.Permission{StateImpact: authz.Readwrite}
	}
	ft := &fakeAppTool{name: toolName, perm: perm, roHint: true, result: tool.Result{Content: "body"}}

	mem := &fakeUIMemory{}
	l := &Loop{
		SessionKey: memory.NamespacedName{Namespace: ns, Name: name},
		AppTools:   map[string]tool.Tool{toolName: ft},
		Memory:     mem,
	}
	rec := &uiFixtureRecorder{}
	reg := pipeline.NewRegistry()
	reg.Register(rec, 10)
	loopWithInjectedExecutor(t, l, reg)

	rig := &uiFixtureRig{recorder: rec, mem: mem}
	l.approvalAskSpawnHookForTest = func() {
		rig.mu.Lock()
		rig.approvalAsks++
		rig.mu.Unlock()
	}
	uiFixtureRigsMu.Lock()
	uiFixtureRigs[l] = rig
	uiFixtureRigsMu.Unlock()
	t.Cleanup(func() {
		uiFixtureRigsMu.Lock()
		delete(uiFixtureRigs, l)
		uiFixtureRigsMu.Unlock()
	})

	ia := &fakeInteract{allow: true}
	execCtx := memory.WithSystemApproval(context.Background(), "test")
	return l, ia, execCtx
}

// dataBindingEnvelope builds the wire bytes for a data-binding / app-tool
// request naming toolName, with the fixture's authorized viewer as
// Requester. Named distinctly from apptoolcall_test.go's own
// appToolEnvelope (which threads an explicit requester per call, since
// TestHandleAppToolCall's cases vary it): every case here uses the SAME
// fixture-authorized viewer, so the shorter 2-arg form drops that
// repetition without touching the existing helper's signature or any of its
// many call sites.
func dataBindingEnvelope(t *testing.T, toolName, argsJSON string) []byte {
	t.Helper()
	env, err := channelevents.BuildEnvelope("demo-ns", "demo-session", channelevents.KindUIDataBinding,
		channelevents.AppToolCallRequest{
			ToolName:  toolName,
			Args:      json.RawMessage(argsJSON),
			Requester: "user:viewer",
			RequestID: "req-1",
		})
	require.NoError(t, err)
	b, err := json.Marshal(env)
	require.NoError(t, err)
	return b
}

func TestHandleUIDataBinding(t *testing.T) {
	const ns, name = "demo-ns", "demo-session"

	t.Run("a readonly granted tool runs and returns its result", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: true})
		resp := l.HandleUIDataBinding(execCtx, ia, ns, name,
			dataBindingEnvelope(t, "demo_readonly", `{"span":"30d"}`))
		assert.Equal(t, channelevents.AppToolCallStatusOK, resp.Status)
		assert.False(t, resp.IsError)
	})

	t.Run("a side-effecting granted tool is DENIED — a write is never a data binding", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: false})
		resp := l.HandleUIDataBinding(execCtx, ia, ns, name,
			dataBindingEnvelope(t, "demo_mutating", `{}`))
		assert.Equal(t, channelevents.AppToolCallStatusDenied, resp.Status)
		assert.Contains(t, resp.Message, "only read")
	})

	t.Run("a side-effecting tool spawns NO detached approval", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: false})
		_ = l.HandleUIDataBinding(execCtx, ia, ns, name, dataBindingEnvelope(t, "demo_mutating", `{}`))
		assert.Zero(t, l.approvalAsksForTest(),
			"a data binding must never fire an approval; that is an action binding's job")
	})

	t.Run("a nil interact checker denies BEFORE the readonly verdict leaks", func(t *testing.T) {
		l, _, execCtx := newAppToolFixture(t, appToolFixture{readonly: false})
		resp := l.HandleUIDataBinding(execCtx, nil, ns, name, dataBindingEnvelope(t, "demo_mutating", `{}`))
		assert.Equal(t, channelevents.AppToolCallStatusDenied, resp.Status)
		assert.NotContains(t, resp.Message, "only read",
			"an unauthorized caller must not learn whether the tool is readonly")
	})

	t.Run("an ungranted tool is not_found, same fail-closed lookup as the widget path", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: true})
		resp := l.HandleUIDataBinding(execCtx, ia, ns, name, dataBindingEnvelope(t, "not_granted", `{}`))
		assert.Equal(t, channelevents.AppToolCallStatusNotFound, resp.Status)
	})

	t.Run("malformed wire bytes are an error, never a panic", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: true})
		resp := l.HandleUIDataBinding(execCtx, ia, ns, name, []byte("{not json"))
		assert.Equal(t, channelevents.AppToolCallStatusError, resp.Status)
	})

	t.Run("the result never reaches the transcript", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: true})
		before := l.transcriptLenForTest()
		_ = l.HandleUIDataBinding(execCtx, ia, ns, name, dataBindingEnvelope(t, "demo_readonly", `{}`))
		assert.Equal(t, before, l.transcriptLenForTest(), "D-B6: bypass the agent, not the platform")
	})

	t.Run("the pipeline sees UIDataBinding=true", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: true})
		_ = l.HandleUIDataBinding(execCtx, ia, ns, name, dataBindingEnvelope(t, "demo_readonly", `{}`))
		require.NotEmpty(t, l.observedPipelineInputsForTest())
		for _, in := range l.observedPipelineInputsForTest() {
			assert.True(t, in.UIDataBinding, "every point of the contained call is marked browser-bound")
		}
	})

	t.Run("an ordinary app-tool call is NOT marked as a data binding", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: true})
		_ = l.HandleAppToolCall(execCtx, ia, ns, name, dataBindingEnvelope(t, "demo_readonly", `{}`))
		require.NotEmpty(t, l.observedPipelineInputsForTest())
		for _, in := range l.observedPipelineInputsForTest() {
			assert.False(t, in.UIDataBinding)
		}
	})
}

// TestHandleUIDataBinding_NormalizesToolNameForLookup pins requirement 4: a
// Binding.Ref carries no CRD pattern (unlike AgentUI.spec.tools /
// AgentClassUIGrant.grantedTools, which do — see apptools_grant.go's
// MaterializeAppTools doc comment), so a ref reaching the wire un-normalized
// must still resolve against Loop.AppTools' normalized keys, or a validly
// granted tool would look not_found for the wrong reason.
func TestHandleUIDataBinding_NormalizesToolNameForLookup(t *testing.T) {
	const ns, name = "demo-ns", "demo-session"
	registeredKey := synthesize.NormalizeName("Demo.Readonly") // == "demo-readonly"
	require.NotEqual(t, "Demo.Readonly", registeredKey, "the fixture needs a name NormalizeName actually rewrites")

	ft := &fakeAppTool{name: registeredKey, perm: authz.Permission{StateImpact: authz.Readonly}, roHint: true, result: tool.Result{Content: "body"}}
	l := &Loop{
		SessionKey: memory.NamespacedName{Namespace: ns, Name: name},
		AppTools:   map[string]tool.Tool{registeredKey: ft},
	}
	reg := pipeline.NewRegistry()
	loopWithInjectedExecutor(t, l, reg)
	ia := &fakeInteract{allow: true}
	execCtx := memory.WithSystemApproval(context.Background(), "test")

	resp := l.HandleUIDataBinding(execCtx, ia, ns, name,
		dataBindingEnvelope(t, "Demo.Readonly", `{}`)) // raw, un-normalized ref on the wire

	assert.Equal(t, channelevents.AppToolCallStatusOK, resp.Status,
		"an un-normalized wire ref must still resolve to the normalized registry key")
}
