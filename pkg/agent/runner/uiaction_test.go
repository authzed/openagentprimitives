package runner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/synthesize"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiaction"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// uiActionEnvelope builds the wire bytes for an agent-UI action invoke
// request naming action/toolName, with the fixture's authorized viewer as
// Requester. A THIRD wire shape (UIActionRequest, not AppToolCallRequest) —
// sibling to apptoolcall_test.go's appToolEnvelope and
// uidatabinding_test.go's dataBindingEnvelope, not an overload of either
// (the arity collision Plan 4's review caught).
func uiActionEnvelope(t *testing.T, action, toolName, argsJSON string) []byte {
	t.Helper()
	env, err := channelevents.BuildEnvelope("demo-ns", "demo-session", channelevents.KindUIAction,
		channelevents.UIActionRequest{
			RequestID: "req-1",
			Action:    action,
			ToolName:  toolName,
			Args:      json.RawMessage(argsJSON),
			Requester: "user:viewer",
		})
	require.NoError(t, err)
	b, err := json.Marshal(env)
	require.NoError(t, err)
	return b
}

func TestHandleUIAction(t *testing.T) {
	const ns, name = "demo-ns", "demo-session"

	t.Run("a side-effecting granted tool is ACCEPTED — unlike a data binding, an action may write", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: false})
		resp := l.HandleUIAction(execCtx, ia, ns, name,
			uiActionEnvelope(t, "advance", "demo_mutating", `{}`))
		assert.Equal(t, string(uiaction.StateSubmitted), resp.State,
			"a side-effecting action detaches and reports submitted, never denied")
		assert.NotEmpty(t, resp.RequestID)
	})

	t.Run("a side-effecting action DOES fire the approval a data binding must never fire", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: false})
		_ = l.HandleUIAction(execCtx, ia, ns, name, uiActionEnvelope(t, "advance", "demo_mutating", `{}`))
		assert.Equal(t, 1, l.approvalAsksForTest(),
			"this is the exact inverse of TestHandleUIDataBinding's spawns-no-approval case")
	})

	t.Run("a readonly granted tool auto-runs and settles synchronously", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: true})
		resp := l.HandleUIAction(execCtx, ia, ns, name, uiActionEnvelope(t, "refresh", "demo_readonly", `{}`))
		assert.Equal(t, string(uiaction.StateSucceeded), resp.State)
	})

	t.Run("a nil interact checker denies before anything is spawned or recorded", func(t *testing.T) {
		l, _, execCtx := newAppToolFixture(t, appToolFixture{readonly: false})
		resp := l.HandleUIAction(execCtx, nil, ns, name, uiActionEnvelope(t, "advance", "demo_mutating", `{}`))
		assert.Equal(t, string(uiaction.StateDenied), resp.State)
		assert.Zero(t, l.approvalAsksForTest(), "an unauthorized caller must not be able to fire an approval ask")
	})

	t.Run("an ungranted tool is a failed action, and reports no tool vocabulary to the browser", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: true})
		resp := l.HandleUIAction(execCtx, ia, ns, name, uiActionEnvelope(t, "advance", "not_granted", `{}`))
		assert.Equal(t, string(uiaction.StateFailed), resp.State)
		assert.NotContains(t, resp.Message, "not_granted")
	})

	t.Run("malformed wire bytes are a failure, never a panic", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: true})
		resp := l.HandleUIAction(execCtx, ia, ns, name, []byte("{not json"))
		assert.Equal(t, string(uiaction.StateFailed), resp.State)
	})

	t.Run("the result never reaches the transcript — D-B6 holds for the write half too", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: true})
		before := l.transcriptLenForTest()
		_ = l.HandleUIAction(execCtx, ia, ns, name, uiActionEnvelope(t, "refresh", "demo_readonly", `{}`))
		assert.Equal(t, before, l.transcriptLenForTest(), "addressable to the browser, not to the model")
	})

	t.Run("an action is NOT metered as a data binding", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: true})
		_ = l.HandleUIAction(execCtx, ia, ns, name, uiActionEnvelope(t, "refresh", "demo_readonly", `{}`))
		require.NotEmpty(t, l.observedPipelineInputsForTest())
		for _, in := range l.observedPipelineInputsForTest() {
			assert.False(t, in.UIDataBinding,
				"the browser-sized ingress ceiling is the READ half's rule; an action is metered normally")
		}
	})
}

// TestHandleUIAction_NormalizesToolNameForLookup pins the same requirement
// TestHandleUIDataBinding_NormalizesToolNameForLookup pins for the read
// half: a Binding.Ref (here, an Action's ToolName) carries no CRD pattern,
// so a ref reaching the wire un-normalized must still resolve against
// Loop.AppTools' normalized keys via the SAME synthesize.NormalizeName call
// inside handleAppToolCallReq — there is no action-specific lookup path to
// forget the normalization on.
func TestHandleUIAction_NormalizesToolNameForLookup(t *testing.T) {
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

	resp := l.HandleUIAction(execCtx, ia, ns, name,
		uiActionEnvelope(t, "advance", "Demo.Readonly", `{}`)) // raw, un-normalized ref on the wire

	assert.Equal(t, string(uiaction.StateSucceeded), resp.State,
		"an un-normalized wire ref must still resolve to the normalized registry key")
}

// TestSurfaceRefactorLeavesTheOtherTwoSurfacesUnchanged is a regression guard
// on the appToolSurface change itself: the bool it replaced gated shipped
// behavior on two paths this task does not otherwise touch.
func TestSurfaceRefactorLeavesTheOtherTwoSurfacesUnchanged(t *testing.T) {
	const ns, name = "demo-ns", "demo-session"

	t.Run("a widget app-tool call still spawns a detached approval", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: false})
		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			appToolEnvelope(t, "demo_mutating", "user:viewer", json.RawMessage(`{}`)))
		assert.Equal(t, channelevents.AppToolCallStatusRequiresApproval, resp.Status)
		assert.Equal(t, 1, l.approvalAsksForTest())
	})

	t.Run("a data binding to a side-effecting tool is still denied", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: false})
		resp := l.HandleUIDataBinding(execCtx, ia, ns, name, dataBindingEnvelope(t, "demo_mutating", `{}`))
		assert.Equal(t, channelevents.AppToolCallStatusDenied, resp.Status)
	})

	t.Run("a data binding still marks the pipeline UIDataBinding=true", func(t *testing.T) {
		l, ia, execCtx := newAppToolFixture(t, appToolFixture{readonly: true})
		_ = l.HandleUIDataBinding(execCtx, ia, ns, name, dataBindingEnvelope(t, "demo_readonly", `{}`))
		require.NotEmpty(t, l.observedPipelineInputsForTest())
		for _, in := range l.observedPipelineInputsForTest() {
			assert.True(t, in.UIDataBinding)
		}
	})
}
