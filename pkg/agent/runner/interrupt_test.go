package runner

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTool is a minimal tool.Tool with a settable name + stateImpact and an
// optional Cancel. cancellable=false omits Cancel (via the non-cancellable
// variant below).
type fakeTool struct {
	name   string
	impact authz.StateImpact
}

func (f fakeTool) Name() string                                  { return f.name }
func (f fakeTool) Kind() tool.Kind                               { return tool.KindMCP }
func (f fakeTool) Description() string                           { return "" }
func (f fakeTool) InputSchema() json.RawMessage                  { return json.RawMessage(`{}`) }
func (f fakeTool) Permission() authz.Permission                  { return authz.Permission{StateImpact: f.impact} }
func (f fakeTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (f fakeTool) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	return tool.Result{}, nil
}

type cancellableFakeTool struct{ fakeTool }

func (cancellableFakeTool) Cancel(context.Context) error { return nil }

func ro(name string) tool.Tool { return cancellableFakeTool{fakeTool{name, authz.Readonly}} }
func rw(name string) tool.Tool { return cancellableFakeTool{fakeTool{name, authz.Readwrite}} }
func nc(name string) tool.Tool { return fakeTool{name, authz.Readonly} } // readonly but NOT Cancellable

// spyCancelTool is a cancellableFakeTool that records whether Cancel was
// invoked, so a test can assert the teardown loop's isInterruptibleTool
// guard actually prevented a call rather than just trusting the shared
// interruptibleReason predicate in isolation.
type spyCancelTool struct {
	fakeTool
	called *bool
}

func (s spyCancelTool) Cancel(context.Context) error {
	*s.called = true
	return nil
}

func TestIsInterruptibleTool(t *testing.T) {
	cases := []struct {
		name string
		tool tool.Tool
		want bool
	}{
		{"readonly+cancellable → interruptible", ro("read_file"), true},
		{"readwrite+cancellable → not interruptible", rw("terraform_apply"), false},
		{"readonly-not-cancellable → not interruptible", nc("run_shell"), false},
		{"stateless+cancellable → interruptible", cancellableFakeTool{fakeTool{"noop", authz.Stateless}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isInterruptibleTool(tc.tool))
		})
	}
}

func TestInterruptRegistry_FiresRegisteredCancels(t *testing.T) {
	var reg interruptRegistry

	ctxA, cancelA := context.WithCancelCause(context.Background())
	ctxB, cancelB := context.WithCancelCause(context.Background())
	reg.registerTool("tu_a", ro("tool_a"), cancelA)
	reg.registerTool("tu_b", ro("tool_b"), cancelB)

	reg.fire()

	assert.ErrorIs(t, context.Cause(ctxA), errInterruptedByUser)
	assert.ErrorIs(t, context.Cause(ctxB), errInterruptedByUser)
	assert.True(t, reg.takeRequested(), "fire sets requested")
	assert.False(t, reg.takeRequested(), "takeRequested clears the flag")
}

func TestInterruptRegistry_DeregisterStopsCancel(t *testing.T) {
	var reg interruptRegistry
	ctxA, cancelA := context.WithCancelCause(context.Background())
	reg.registerTool("tu_a", ro("tool_a"), cancelA)
	reg.deregisterTool("tu_a")
	reg.fire()
	assert.NotErrorIs(t, context.Cause(ctxA), errInterruptedByUser,
		"a deregistered tool is not cancelled by a later fire")
	cancelA(nil) // avoid ctx leak
}

func TestInterruptRegistry_CancelForBudget(t *testing.T) {
	var reg interruptRegistry

	roCtx, roCancel := context.WithCancelCause(context.Background())
	rwCtx, rwCancel := context.WithCancelCause(context.Background())
	provCtx, provCancel := context.WithCancelCause(context.Background())
	reg.registerTool("tu_ro", ro("read_file"), roCancel)
	reg.registerTool("tu_rw", rw("terraform_apply"), rwCancel)
	reg.setProvider(provCancel)

	assert.Empty(t, reg.cancelRemainingForBudget(), "the drain cut-off is a no-op before the budget is spent")
	assert.NoError(t, rwCtx.Err())

	draining := reg.cancelForBudget(errMaxDurationExceeded)

	assert.ErrorIs(t, context.Cause(roCtx), errMaxDurationExceeded, "readonly tool cancelled at the cap")
	assert.ErrorIs(t, context.Cause(provCtx), errMaxDurationExceeded, "provider call cancelled at the cap")
	assert.NoError(t, rwCtx.Err(), "stateful tool is left to drain, not cut mid-mutation")
	assert.Equal(t, []string{"terraform_apply"}, draining, "the draining stateful tools are named for the log")
	assert.False(t, reg.takeRequested(), "a budget cancel is not a user interrupt")

	// Registrations after the budget is spent are cancelled on arrival,
	// stateful ones included: a call that has not started has nothing to drain.
	lateTool, lateToolCancel := context.WithCancelCause(context.Background())
	lateProv, lateProvCancel := context.WithCancelCause(context.Background())
	reg.registerTool("tu_late", rw("git_push"), lateToolCancel)
	reg.setProvider(lateProvCancel)
	assert.ErrorIs(t, context.Cause(lateTool), errMaxDurationExceeded, "late stateful tool cancelled on register")
	assert.ErrorIs(t, context.Cause(lateProv), errMaxDurationExceeded, "late provider call cancelled on set")

	// Once the drain window runs out, whatever is still in flight is cut.
	reg.deregisterTool("tu_ro")
	reg.deregisterTool("tu_late")
	assert.Equal(t, []string{"terraform_apply"}, reg.cancelRemainingForBudget())
	assert.ErrorIs(t, context.Cause(rwCtx), errMaxDurationExceeded, "stateful tool cancelled after the drain")
}

// regTool registers t under id in l's live interrupt registry and returns
// its cancel func, mirroring what the dispatch fan-out does in loop_dispatch.go.
func regTool(l *Loop, id string, t tool.Tool) context.CancelCauseFunc {
	_, cancel := context.WithCancelCause(context.Background())
	l.interrupts.registerTool(id, t, cancel)
	return cancel
}

func TestInterrupt_StatefulInflight_Rejects(t *testing.T) {
	l := &Loop{}
	regTool(l, "tu_1", rw("terraform_apply")) // cancellable + readwrite → not interruptible
	out := l.Interrupt(context.Background())
	assert.False(t, out.Interrupted)
	assert.Contains(t, out.Reason, "terraform_apply")
}

func TestInterrupt_ReadonlyInflight_FiresAndReports(t *testing.T) {
	l := &Loop{}
	ctxA, cancelA := context.WithCancelCause(context.Background())
	l.interrupts.registerTool("tu_a", ro("read_file"), cancelA)
	out := l.Interrupt(context.Background())
	assert.True(t, out.Interrupted)
	assert.ErrorIs(t, context.Cause(ctxA), errInterruptedByUser)
}

func TestInterrupt_PendingApprovalOnly_IsInterruptibleAndDenied(t *testing.T) {
	l := &Loop{Approval: approval.New()}
	got := make(chan approval.Decision, 1)
	go func() {
		d, _ := l.Approval.Await(context.Background(), approval.Request{RequestID: "r1", SessionRef: l.sessionRef()})
		got <- d
	}()
	require.Eventually(t, func() bool { return l.Approval.PendingForSession(l.sessionRef()) == 1 }, time.Second, time.Millisecond)
	out := l.Interrupt(context.Background())
	assert.True(t, out.Interrupted, "a pending approval makes the run interruptible even with no tools")
	select {
	case d := <-got:
		assert.False(t, d.Approved, "the pending approval was denied by the interrupt")
	case <-time.After(time.Second):
		t.Fatal("pending approval not resolved by Interrupt")
	}
}

func TestInterrupt_NothingRunning_Rejects(t *testing.T) {
	l := &Loop{}
	out := l.Interrupt(context.Background())
	assert.False(t, out.Interrupted)
	assert.Contains(t, out.Reason, "nothing")
}

func TestFire_ExcludesStatefulEntry(t *testing.T) {
	l := &Loop{}
	ctxRO, cancelRO := context.WithCancelCause(context.Background())
	ctxRW, cancelRW := context.WithCancelCause(context.Background())
	l.interrupts.registerTool("ro", ro("read_file"), cancelRO)
	l.interrupts.registerTool("rw", rw("terraform_apply"), cancelRW)
	l.interrupts.fire()
	assert.ErrorIs(t, context.Cause(ctxRO), errInterruptedByUser, "readonly entry cancelled")
	assert.NotErrorIs(t, context.Cause(ctxRW), errInterruptedByUser, "stateful entry NOT cancelled by fire")
	cancelRW(nil)
}

// TestCancelInterruptibleTools_SkipsStatefulCallsReadonly proves the
// isInterruptibleTool guard in the Cancel-teardown loop that Loop.Interrupt
// runs (extracted as cancelInterruptibleTools): a stateful/readwrite tool's
// Cancel must never be invoked, even though it implements tool.Cancellable,
// while a readonly interruptible tool's Cancel is invoked. This is the
// defense-in-depth path for the within-batch race the guard closes — a
// stateful sibling registering between Interrupt's currentInterruptibility
// check and this loop's own currentTools() snapshot — mirroring the
// existing fire() guard covered by TestFire_ExcludesStatefulEntry.
func TestCancelInterruptibleTools_SkipsStatefulCallsReadonly(t *testing.T) {
	var roCalled, rwCalled bool
	roTool := spyCancelTool{fakeTool{"read_file", authz.Readonly}, &roCalled}
	rwTool := spyCancelTool{fakeTool{"terraform_apply", authz.Readwrite}, &rwCalled}

	cancelInterruptibleTools(context.Background(), []tool.Tool{roTool, rwTool}, "ns/sess")

	assert.True(t, roCalled, "Cancel called on the readonly interruptible tool")
	assert.False(t, rwCalled, "Cancel NOT called on the stateful tool even though it implements Cancellable")
}
