package runner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// widgetCallEnvelope is appToolEnvelope plus the calling widget's origin — the
// field webd fills in from the session's own status.activeWidgets entry.
func widgetCallEnvelope(t *testing.T, toolName, requester, widgetOrigin string, args json.RawMessage) []byte {
	t.Helper()
	env, err := channelevents.BuildEnvelope("default", "disp", channelevents.KindAppToolCall,
		channelevents.AppToolCallRequest{
			ToolName:     toolName,
			Args:         args,
			Requester:    requester,
			RequestID:    "req-1",
			WidgetOrigin: widgetOrigin,
		})
	require.NoError(t, err)
	b, err := json.Marshal(env)
	require.NoError(t, err)
	return b
}

// A widget is HTML an MCP server authored, running its own script. The app-tool
// registry is a single flat map across every origin the session has, so a
// lookup by name alone let a widget from server A invoke server B's tool — and
// A needs no app-tool grant of its own to get a widget rendered, only B's tool
// needs to have earned one.
//
// The origin was already on the resource spec and was dropped on the way to the
// widget reference. Threading it through and comparing it here is the pin: a
// widget calls its OWN server's tools and nothing else.
func TestHandleAppToolCall_WidgetOriginPin(t *testing.T) {
	const ns, name = "default", "disp"
	roPerm := authz.Permission{StateImpact: authz.Readonly}
	execCtx := memory.WithSystemApproval(context.Background(), "test")

	t.Run("cross-origin call: not_found, tool never executes", func(t *testing.T) {
		ia := &fakeInteract{allow: true}
		victim := &fakeAppTool{
			name: "billing_charge", perm: roPerm, roHint: true,
			origin: "mcpserver/billing", result: tool.Result{Content: "body"},
		}
		l := newAppLoop(t, pipeline.NewRegistry(), map[string]tool.Tool{"billing_charge": victim})

		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			widgetCallEnvelope(t, "billing_charge", "user:viewer", "mcpserver/evil", json.RawMessage(`{}`)))

		assert.Equal(t, channelevents.AppToolCallStatusNotFound, resp.Status,
			"a cross-origin callee is indistinguishable from an absent one, so the refusal is not an enumeration oracle")
		assert.Equal(t, 0, victim.executed(),
			"a widget must never reach another server's tool")
	})

	t.Run("same-origin call: executes", func(t *testing.T) {
		ia := &fakeInteract{allow: true}
		own := &fakeAppTool{
			name: "billing_read", perm: roPerm, roHint: true,
			origin: "mcpserver/billing", result: tool.Result{Content: "body"},
		}
		l := newAppLoop(t, pipeline.NewRegistry(), map[string]tool.Tool{"billing_read": own})

		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			widgetCallEnvelope(t, "billing_read", "user:viewer", "mcpserver/billing", json.RawMessage(`{}`)))

		assert.Equal(t, channelevents.AppToolCallStatusOK, resp.Status)
		assert.Equal(t, 1, own.executed(), "a widget calling its own server's tool is the supported case")
	})

	// An origin-less tool is a non-MCP tool (Origin() returns ""). It belongs to
	// no server, so no widget can claim it as its own.
	t.Run("origin-less callee: not_found for a widget caller", func(t *testing.T) {
		ia := &fakeInteract{allow: true}
		bare := &fakeAppTool{name: "local_read", perm: roPerm, roHint: true, result: tool.Result{Content: "body"}}
		l := newAppLoop(t, pipeline.NewRegistry(), map[string]tool.Tool{"local_read": bare})

		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			widgetCallEnvelope(t, "local_read", "user:viewer", "mcpserver/evil", json.RawMessage(`{}`)))

		assert.Equal(t, channelevents.AppToolCallStatusNotFound, resp.Status)
		assert.Equal(t, 0, bare.executed())
	})

	// The counterweight: an agent-declared UI binding is not a widget and sends
	// no origin, so it keeps reaching every tool the three-way grant admitted.
	// Without this the pin would read as "empty origin means deny" and silently
	// break every agent-defined UI.
	t.Run("no widget origin: unpinned, still executes", func(t *testing.T) {
		ia := &fakeInteract{allow: true}
		ft := &fakeAppTool{
			name: "billing_read", perm: roPerm, roHint: true,
			origin: "mcpserver/billing", result: tool.Result{Content: "body"},
		}
		l := newAppLoop(t, pipeline.NewRegistry(), map[string]tool.Tool{"billing_read": ft})

		resp := l.HandleAppToolCall(execCtx, ia, ns, name,
			appToolEnvelope(t, "billing_read", "user:viewer", json.RawMessage(`{}`)))

		assert.Equal(t, channelevents.AppToolCallStatusOK, resp.Status)
		assert.Equal(t, 1, ft.executed())
	})
}
