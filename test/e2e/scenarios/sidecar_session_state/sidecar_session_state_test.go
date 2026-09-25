//go:build e2e

package sidecar_session_state_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	mcptest "github.com/authzed/openagentprimitives/pkg/tools/mcp/testing"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	sidecarRef     = "state-tb"
	sidecarLLMName = "k8s"
	selectTool     = "k8s_select"   // synthesized LLM-facing name
	selectedTool   = "k8s_selected" // synthesized LLM-facing name
	selectedValue  = "adobe"
)

// TestSidecarSessionState_SelectionSurvivesAcrossToolCalls is the end-to-end
// regression test for "PS selection isn't persisting across calls".
//
// A stateful MCP sidecar keys its per-request selection by the MCP session id.
// The runner owns ONE probe.SessionCache per AgentSession so every tool call to
// a given server rides the same MCP session; sidecar-toolbox tools were
// synthesized without it, so each call opened a fresh session and the agent's
// selection vanished between calls.
//
// This drives the whole loop — session create → mid-session sidecar readiness →
// ToolRefresher synthesis → two separate tool calls — and asserts the value
// selected by the first call is visible to the second, plus that the sidecar
// served exactly ONE MCP session for the entire AgentSession.
func TestSidecarSessionState_SelectionSurvivesAcrossToolCalls(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	// The sidecar stub: a REAL MCP server whose select/selected tools store and
	// read a value keyed by the MCP session id — the dedicated-mcp shape. It
	// stands in for the separate sidecar pod, so the runner reaches it exactly
	// as it would a pod IP.
	srv, sessionCount := mcptest.NewSelectionServer()
	// CloseClientConnections before Close: a cached MCP session holds a
	// standalone SSE stream open for the life of the AgentSession, and
	// httptest.Server.Close blocks indefinitely waiting for it. (Registered
	// before e2e.Start, so LIFO cleanup runs this AFTER the harness shuts the
	// session down — the force-close only catches what shutdown left behind.)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})

	h := e2e.Start(t, e2e.Options{ExtraManifests: []string{string(manifests)}})

	// MCP-shaped tools wrap real args in {operation_id, _reason, args}; the
	// harness factory does not wire SessionContext.Operations, so any non-empty
	// operation_id passes the registered-id check.
	selectCall := e2e.ToolUse(selectTool, map[string]any{
		"operation_id": "op-select",
		"_reason":      "select the value for this session",
		"args":         map[string]any{"value": selectedValue},
	})
	selectedCall := e2e.ToolUse(selectedTool, map[string]any{
		"operation_id": "op-selected",
		"_reason":      "read back what this session selected",
		"args":         map[string]any{},
	})

	// selectSucceeded distinguishes the real sidecar answer ("selected adobe")
	// from the "unknown tool" result returned while the refresher has not yet
	// synthesized the sidecar's tools.
	selectSucceeded := func(content any) bool {
		return strings.Contains(fmt.Sprintf("%v", content), "selected "+selectedValue)
	}

	// Script (one live loop):
	//   user "go"                        → tool_use(k8s_select)
	//   tool_result(select, unknown)     → tool_use(k8s_select)   [retry until synthesized]
	//   tool_result(select, "selected …")→ tool_use(k8s_selected) [the read-back]
	//   tool_result(selected, anything)  → respond_to_user("done")
	//   tool_result(respond_to_user, _)  → EndTurn
	//
	// The read-back result is deliberately NOT branched on: the assertions below
	// inspect what the sidecar actually returned, so a lost selection fails on a
	// concrete assertion rather than by starving the reply expectation.
	//
	// Registration order matters: rules are matched first-wins, and
	// OnUserMessage("go") matches EVERY request (it walks back to the original
	// human text, which never leaves the conversation). It must be registered
	// LAST so the tool_result rules get first refusal.
	h.LLM.OnToolResult(selectTool, selectSucceeded).Reply(selectedCall)
	h.LLM.OnToolResult(selectTool, func(content any) bool {
		return !selectSucceeded(content)
	}).Reply(selectCall).Repeating()
	h.LLM.OnToolResult(selectedTool, e2e.AnyResult()).Reply(e2e.RespondToUser("done"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())
	h.LLM.OnUserMessage("go").Reply(selectCall).Repeating()

	h.WaitForAgentClassValid("ac-sidecar-session-state", 30*time.Second)

	// Drive the session. The first inbound creates the AgentSession; then the
	// harness stamps the sidecar Ready (as the operator does once its pod is up)
	// pointing its dispatch+probe endpoint at the stateful stub.
	h.SendUserMessage("go")
	sessName := sessionNameAfterFirstReply(t, h)
	tbSpec := getSidecarToolboxSpec(t, h, "default", sidecarRef)
	h.MarkSidecarReady("default", sessName, sidecarLLMName, sidecarRef, srv.URL, tbSpec)

	h.ExpectAgentReply(e2e.Contains("done"))

	reqs := h.LLM.Requests()
	require.NotEmpty(t, reqs, "scripted LLM must have observed at least one request")

	// The sidecar's tools were synthesized mid-session and both were called.
	require.True(t, anyReqAdvertises(reqs, selectTool),
		"the mid-session-synthesized sidecar select tool must be advertised")
	require.True(t, anyReqAdvertises(reqs, selectedTool),
		"the mid-session-synthesized sidecar selected tool must be advertised")

	// THE assertion: the read-back call landed on the SAME MCP session as the
	// select, so the server still had the selection. A per-call session yields
	// "no selection for this session" here.
	readBack, found := findToolResultFor(reqs, selectedTool)
	require.True(t, found, "the read-back tool_result must appear in some LLM request")
	assert.NotContains(t, readBack, "no selection for this session",
		"the sidecar lost this session's selection between two tool calls — the runner opened a fresh MCP session per call")
	assert.Contains(t, readBack, selectedValue,
		"the value selected by the first tool call must be visible to the second")

	// And structurally: one MCP session served the whole AgentSession.
	assert.Equal(t, 1, sessionCount(),
		"every sidecar tool call must ride ONE MCP session per AgentSession")
}

// findToolResultFor returns the content of the first tool_result produced by a
// call to `toolName`, located by matching the tool_use that preceded it.
func findToolResultFor(reqs []llm.Request, toolName string) (content string, found bool) {
	useIDs := map[string]bool{}
	for _, req := range reqs {
		for _, m := range req.Messages {
			for _, cb := range m.Content {
				if cb.ToolUse != nil && cb.ToolUse.Name == toolName {
					useIDs[cb.ToolUse.ID] = true
				}
				if cb.ToolResult != nil && useIDs[cb.ToolResult.ToolUseID] {
					return cb.ToolResult.Content, true
				}
			}
		}
	}
	return "", false
}

// anyReqAdvertises reports whether any request in reqs advertises name.
func anyReqAdvertises(reqs []llm.Request, name string) bool {
	for _, req := range reqs {
		for _, td := range req.Tools {
			if td.Name == name {
				return true
			}
		}
	}
	return false
}

// getSidecarToolboxSpec fetches the SidecarToolbox CR's spec so MarkSidecarReady
// can snapshot it onto the resolved status entry (the synthesizer reads its
// Tools allowlist from there).
func getSidecarToolboxSpec(t *testing.T, h *e2e.Harness, ns, name string) spiceboxv1alpha1.SidecarToolboxSpec {
	t.Helper()
	var tb spiceboxv1alpha1.SidecarToolbox
	require.Eventually(t, func() bool {
		return h.K8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &tb) == nil
	}, 10*time.Second, 100*time.Millisecond, "SidecarToolbox %s/%s must exist", ns, name)
	return tb.Spec
}

// sessionNameAfterFirstReply polls until exactly one AgentSession exists and
// returns its name — MarkSidecarReady needs the session before it can patch
// status.
func sessionNameAfterFirstReply(t *testing.T, h *e2e.Harness) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var list spiceboxv1alpha1.AgentSessionList
		if err := h.K8s.List(context.Background(), &list); err == nil && len(list.Items) == 1 {
			return list.Items[0].Name
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("sessionNameAfterFirstReply: no single AgentSession within deadline")
	return ""
}
