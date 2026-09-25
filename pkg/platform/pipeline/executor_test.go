package pipeline_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

func denyHook(name string) *fakeHook {
	return &fakeHook{name: name, points: []pipeline.Point{pipeline.PreToolCall},
		decide: func(pipeline.Input) pipeline.Decision {
			return pipeline.Decision{Verdict: pipeline.Deny, Reason: name + " says no"}
		}}
}

func TestExecutor_AllAllow_RunsAllInOrder_ReturnsAllow(t *testing.T) {
	r := pipeline.NewRegistry()
	a := &fakeHook{name: "a", points: []pipeline.Point{pipeline.PreToolCall}}
	b := &fakeHook{name: "b", points: []pipeline.Point{pipeline.PreToolCall}}
	r.Register(a, 10)
	r.Register(b, 20)
	host := &fakeHost{}
	out, err := pipeline.NewExecutor(r).Run(context.Background(), pipeline.PreToolCall, pipeline.Input{}, host)
	require.NoError(t, err)
	assert.Equal(t, pipeline.Allow, out.Verdict)
	assert.True(t, a.ran && b.ran, "both hooks ran")
	assert.False(t, host.halted)
}

func TestExecutor_Deny_ShortCircuits_SkipsLaterHooks(t *testing.T) {
	r := pipeline.NewRegistry()
	first := denyHook("first")
	later := &fakeHook{name: "later", points: []pipeline.Point{pipeline.PreToolCall}}
	r.Register(first, 10)
	r.Register(later, 20)
	out, err := pipeline.NewExecutor(r).Run(context.Background(), pipeline.PreToolCall, pipeline.Input{}, &fakeHost{})
	require.NoError(t, err)
	assert.Equal(t, pipeline.Deny, out.Verdict)
	assert.Equal(t, "first says no", out.Reason)
	assert.Equal(t, "first", out.FiredHook)
	assert.False(t, later.ran, "hooks after a Deny must not run")
}

func TestExecutor_DeliversNoticesAndAudit(t *testing.T) {
	r := pipeline.NewRegistry()
	r.Register(&fakeHook{name: "n", points: []pipeline.Point{pipeline.PreToolCall},
		decide: func(pipeline.Input) pipeline.Decision {
			return pipeline.Decision{
				Notices: []pipeline.Notice{{Notice: testNotice("heads up")}},
				Audit:   []pipeline.AuditRecord{{Kind: "ran"}},
			}
		}}, 10)
	host := &fakeHost{}
	_, err := pipeline.NewExecutor(r).Run(context.Background(), pipeline.PreToolCall, pipeline.Input{}, host)
	require.NoError(t, err)
	require.Len(t, host.notices, 1)
	assert.Equal(t, "heads up — Send it again.", host.notices[0].Text(),
		"a text-only Host renders lead plus next step")
	require.Len(t, host.audits, 1)
	assert.Equal(t, "ran", host.audits[0].Kind)
}

func TestExecutor_SetsInputPoint(t *testing.T) {
	r := pipeline.NewRegistry()
	var seen pipeline.Point
	r.Register(&fakeHook{name: "p", points: []pipeline.Point{pipeline.PostToolCall},
		decide: func(in pipeline.Input) pipeline.Decision { seen = in.Point; return pipeline.Decision{} }}, 10)
	_, err := pipeline.NewExecutor(r).Run(context.Background(), pipeline.PostToolCall, pipeline.Input{}, &fakeHost{})
	require.NoError(t, err)
	assert.Equal(t, pipeline.PostToolCall, seen, "executor stamps Input.Point")
}

func TestExecutor_Halt_CallsHostHaltAndStops(t *testing.T) {
	r := pipeline.NewRegistry()
	r.Register(&fakeHook{name: "halter", points: []pipeline.Point{pipeline.PreToolCall},
		decide: func(pipeline.Input) pipeline.Decision {
			return pipeline.Decision{Verdict: pipeline.Halt, Reason: "scope review unavailable"}
		}}, 10)
	later := &fakeHook{name: "later", points: []pipeline.Point{pipeline.PreToolCall}}
	r.Register(later, 20)
	host := &fakeHost{}
	out, err := pipeline.NewExecutor(r).Run(context.Background(), pipeline.PreToolCall, pipeline.Input{}, host)
	require.NoError(t, err)
	assert.Equal(t, pipeline.Halt, out.Verdict)
	assert.True(t, host.halted)
	assert.Equal(t, "scope review unavailable", host.haltReason)
	assert.False(t, later.ran)
}

func TestExecutor_HookPanic_FailsClosedToHalt(t *testing.T) {
	r := pipeline.NewRegistry()
	r.Register(&fakeHook{name: "boom", points: []pipeline.Point{pipeline.PreToolCall},
		decide: func(pipeline.Input) pipeline.Decision { panic("kaboom") }}, 10)
	host := &fakeHost{}
	out, err := pipeline.NewExecutor(r).Run(context.Background(), pipeline.PreToolCall, pipeline.Input{}, host)
	require.NoError(t, err, "a hook panic must not propagate; it fails closed")
	assert.Equal(t, pipeline.Halt, out.Verdict, "panic ⇒ fail closed (Halt)")
	assert.Equal(t, "boom", out.FiredHook)
	assert.True(t, host.halted)
	assert.Contains(t, host.haltReason, "panic")
}

func approvalHook(name string) *fakeHook {
	return &fakeHook{name: name, points: []pipeline.Point{pipeline.PreToolCall},
		decide: func(pipeline.Input) pipeline.Decision {
			return pipeline.Decision{
				Status:   &pipeline.StatusUpdate{Text: "awaiting approval…"},
				Approval: &pipeline.ApprovalAsk{Kind: "tool_call", Summary: "run X"},
			}
		}}
}

// approvalHookWithTimeout is approvalHook with an explicit OnTimeout policy.
func approvalHookWithTimeout(name string, pol pipeline.TimeoutPolicy) *fakeHook {
	return &fakeHook{name: name, points: []pipeline.Point{pipeline.PreToolCall}, decide: func(pipeline.Input) pipeline.Decision {
		return pipeline.Decision{
			Status:   &pipeline.StatusUpdate{Text: "awaiting approval…"},
			Approval: &pipeline.ApprovalAsk{Kind: "tool_call", Summary: "run X", OnTimeout: pol},
		}
	}}
}

func TestExecutor_Approval_Approved_Continues(t *testing.T) {
	r := pipeline.NewRegistry()
	r.Register(approvalHook("ask"), 10)
	after := &fakeHook{name: "after", points: []pipeline.Point{pipeline.PreToolCall}}
	r.Register(after, 20)
	host := &fakeHost{approveResult: true}
	out, err := pipeline.NewExecutor(r).Run(context.Background(), pipeline.PreToolCall, pipeline.Input{}, host)
	require.NoError(t, err)
	assert.Equal(t, pipeline.Allow, out.Verdict)
	require.Len(t, host.approvals, 1, "approval was published")
	require.Len(t, host.statuses, 1, "status delivered before the blocking await")
	assert.True(t, after.ran, "approved ⇒ pipeline continues to later hooks")
}

func TestExecutor_Approval_Denied_ShortCircuitsToDeny(t *testing.T) {
	r := pipeline.NewRegistry()
	r.Register(approvalHook("ask"), 10)
	after := &fakeHook{name: "after", points: []pipeline.Point{pipeline.PreToolCall}}
	r.Register(after, 20)
	host := &fakeHost{approveResult: false} // denied / timeout
	out, err := pipeline.NewExecutor(r).Run(context.Background(), pipeline.PreToolCall, pipeline.Input{}, host)
	require.NoError(t, err)
	assert.Equal(t, pipeline.Deny, out.Verdict)
	assert.Equal(t, "ask", out.FiredHook)
	assert.False(t, after.ran)
}

func TestExecutor_Approval_PublishError_FailsClosedToHalt(t *testing.T) {
	r := pipeline.NewRegistry()
	r.Register(approvalHook("ask"), 10)
	host := &fakeHost{approveErr: assert.AnError}
	out, err := pipeline.NewExecutor(r).Run(context.Background(), pipeline.PreToolCall, pipeline.Input{}, host)
	require.NoError(t, err)
	assert.Equal(t, pipeline.Halt, out.Verdict, "cannot reach approver ⇒ fail closed")
	assert.True(t, host.halted)
}

func TestExecutor_Approval_AwaitError_FailsClosedToHalt(t *testing.T) {
	r := pipeline.NewRegistry()
	r.Register(approvalHook("ask"), 10)
	host := &fakeHost{approveResult: false, awaitErr: assert.AnError}
	out, err := pipeline.NewExecutor(r).Run(context.Background(), pipeline.PreToolCall, pipeline.Input{}, host)
	require.NoError(t, err)
	assert.Equal(t, pipeline.Halt, out.Verdict, "await infra error ⇒ fail closed (Halt), not Deny")
	assert.True(t, host.halted)
}

func TestExecutor_Approval_Approved_RecordsApprover(t *testing.T) {
	r := pipeline.NewRegistry()
	r.Register(approvalHook("ask"), 10)
	// fakeHost.AwaitDecision returns by="approver" when approveResult=true
	host := &fakeHost{approveResult: true}
	_, err := pipeline.NewExecutor(r).Run(context.Background(), pipeline.PreToolCall, pipeline.Input{}, host)
	require.NoError(t, err)
	// Locate the approval_resolved audit record emitted by the executor.
	var resolved *pipeline.AuditRecord
	for i := range host.audits {
		if host.audits[i].Kind == "approval_resolved" {
			resolved = &host.audits[i]
			break
		}
	}
	require.NotNil(t, resolved, "executor must emit an approval_resolved audit record")
	assert.Equal(t, "approver", resolved.Fields["approver"])
	assert.Equal(t, true, resolved.Fields["approved"])
}

func TestExecutor_Approval_Timeout_DefaultPolicy_DeniesNotHalt(t *testing.T) {
	r := pipeline.NewRegistry()
	r.Register(approvalHook("ask"), 10) // default OnTimeout == TimeoutDeny
	after := &fakeHook{name: "after", points: []pipeline.Point{pipeline.PreToolCall}}
	r.Register(after, 20)
	host := &fakeHost{timedOut: true} // deadline lapsed, err == nil
	out, err := pipeline.NewExecutor(r).Run(context.Background(), pipeline.PreToolCall, pipeline.Input{}, host)
	require.NoError(t, err)
	assert.Equal(t, pipeline.Deny, out.Verdict, "a timeout under TimeoutDeny is a sticky-deny, never a Halt")
	assert.False(t, host.halted, "timeout must NOT crash the session")
	assert.False(t, after.ran, "deny short-circuits later hooks")
}

func TestExecutor_Approval_Timeout_HaltPolicy_FailsClosed(t *testing.T) {
	r := pipeline.NewRegistry()
	r.Register(approvalHookWithTimeout("ask", pipeline.TimeoutHalt), 10)
	host := &fakeHost{timedOut: true}
	out, err := pipeline.NewExecutor(r).Run(context.Background(), pipeline.PreToolCall, pipeline.Input{}, host)
	require.NoError(t, err)
	assert.Equal(t, pipeline.Halt, out.Verdict, "TimeoutHalt fails the session closed")
	assert.True(t, host.halted)
}

// testNotice builds a real notice for a test that only cares that SOME
// user-facing message was produced. It uses a registered category so the
// notice is valid end-to-end rather than a hand-built struct that could drift
// from what production can actually construct.
func testNotice(lead string) *notice.Notice {
	return notice.New(categories.InternalError, notice.Args{
		Lead:     lead,
		NextStep: "Send it again.",
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	})
}
