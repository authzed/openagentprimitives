//go:build e2e

package sidecars_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestSidecar_AllowlistEnforcement_EchoCallableSecretToolAbsent exercises
// sidecar-toolbox tool synthesis through the in-process harness end to end:
//
//	user "echo this"
//	→ LLM emits tool_use echo_echo(text=...)
//	→ the synthesized sidecar tool dispatches to the in-process MCP stub
//	→ stub returns the echo result
//	→ LLM emits respond_to_user("echoed: ...")
//	→ harness sees the outbound text.
//
// Two facts are asserted:
//  1. Allowlist enforcement at the agent's tool surface: the synthesized
//     sidecar tool `echo_echo` is advertised to the LLM and is callable,
//     while `echo_secret_tool` (which the stub ALSO serves) is NOT
//     synthesized — the SidecarToolbox allowlist exposes only `echo`.
//  2. The stub recorded the underlying `echo` call (proof the synthesized
//     tool dispatched to the sidecar, not a no-op).
//
// Tool naming mirrors MCP: the AgentClass declares
// sidecarToolboxes[0].name: echo, so the runner synthesizes the LLM-facing
// name as <prefix>_<tool> = echo_echo. The stub's OnTool uses the bare
// upstream name `echo`; the dispatcher strips the prefix before forwarding.
func TestSidecar_AllowlistEnforcement_EchoCallableSecretToolAbsent(t *testing.T) {
	h := e2e.Start(t, e2e.Options{})
	applySidecarFixtures(t, h)

	// Redirect the runner's sidecar probe at the in-process MCP stub. The
	// stub listens on a random httptest port, not the controller-allocated
	// rt.Port, so without this the probe can't reach it. Set before the
	// AgentSession is spawned (SendUserMessage below).
	h.SetSidecarProbeURL(func(spiceboxv1alpha1.ResolvedSidecarToolbox) string { return h.MCP.URL() })

	// The stub advertises BOTH echo and secret_tool on tools/list so
	// synthesis sees both candidates; the SidecarToolbox allowlist (echo
	// only) must cause only echo to be synthesized.
	h.MCP.AnnounceTools([]map[string]any{
		{"name": "echo", "description": "Echoes its input.", "inputSchema": map[string]any{"type": "object"}},
		{"name": "secret_tool", "description": "Should NOT be synthesized.", "inputSchema": map[string]any{"type": "object"}},
	})
	h.MCP.OnTool("echo", func(args map[string]any) any {
		return map[string]any{"echoed": args["text"]}
	})
	h.MCP.OnTool("secret_tool", func(_ map[string]any) any {
		return map[string]any{"secret": "leaked"}
	})

	// Block until the AgentClass converges. The AgentClass controller
	// validates sidecarToolboxes[].ref against the SidecarToolbox's
	// Valid=True condition, which applySidecarFixtures stamped.
	h.WaitForAgentClassValid("sidecar-echo", 30*time.Second)

	// LLM script: call echo_echo once, then reply with the echoed text.
	// MCP-shaped tools wrap real args in {operation_id, _reason, args};
	// SessionContext.Operations isn't wired in the harness factory, so any
	// non-empty operation_id passes the registered-id check.
	h.LLM.OnUserMessage("echo this").
		Reply(e2e.ToolUse("echo_echo", map[string]any{
			"operation_id": "op-1",
			"_reason":      "user asked to echo",
			"args":         map[string]any{"text": "hello sidecar"},
		}))
	h.LLM.OnToolResult("echo_echo", e2e.AnyResult()).
		Reply(e2e.RespondToUser("echoed: hello sidecar"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.EndTurn())

	h.SendUserMessage("echo this")
	h.ExpectAgentReply(e2e.Contains("echoed: hello sidecar"))
	h.AssertAllRulesConsumed()

	// Assertion 1: the stub recorded the underlying echo call (the
	// synthesized tool dispatched to the sidecar).
	var echoCalls int
	for _, c := range h.MCP.Calls() {
		if c.Name == "echo" {
			echoCalls++
		}
		assert.NotEqual(t, "secret_tool", c.Name,
			"secret_tool must never be dispatched (allowlist omits it)")
	}
	assert.Equal(t, 1, echoCalls, "echo dispatched exactly once")

	// Assertion 2: allowlist enforcement at the agent's tool surface. The
	// runner advertises echo_echo to the LLM but NOT echo_secret_tool,
	// because the SidecarToolbox allowlist exposes only echo.
	advertised := advertisedToolNames(h)
	assert.Contains(t, advertised, "echo_echo",
		"the allowlisted sidecar tool is synthesized + advertised")
	assert.NotContains(t, advertised, "echo_secret_tool",
		"the non-allowlisted sidecar tool is NOT synthesized")
}

// TestSidecar_OriginBreakerOpens_AfterThreeTransportFailures drives the echo
// sidecar tool through three consecutive transport failures (injected via the
// stub's FailTransport) and asserts toolguard's ORIGIN-level circuit breaker
// opens once the per-origin threshold (3, set by the AgentClass's toolGuard
// rule for origin "sidecartoolbox/*") is crossed — denying the 4th call
// pre-dispatch.
//
// This replaces the former sidecar degrade-tracker scenario (degrade.go,
// removed): circuit-breaking now goes through toolguard. The behavioral
// deltas from the old tracker:
//   - The 3rd failing tool_result is now a PLAIN failure (the old tracker
//     annotated it "...marked Degraded]"); the breaker fires on the NEXT
//     (4th) call, denying it pre-Execute with a "circuit breaker open" text.
//   - ALL errored tool_results count toward the breaker (the old tracker
//     counted only transport-class IsError via isTransportFailure, which is
//     gone); the spec-approved semantics are "every errored result counts".
//   - The 4th call is DENIED before it reaches the wire, so the stub records
//     exactly 3 calls (the old tracker also never dispatched the 4th, but as
//     an inline short-circuit on the synthesized tool, not a pre-dispatch
//     toolguard deny).
//
// The scripted LLM re-calls echo_echo on each plain failure result, then
// stops and replies once it observes a result containing "circuit breaker
// open" (the toolguard deny text, which also names the origin
// "sidecartoolbox/echo-tb").
func TestSidecar_OriginBreakerOpens_AfterThreeTransportFailures(t *testing.T) {
	h := e2e.Start(t, e2e.Options{})
	applySidecarFixtures(t, h)
	h.SetSidecarProbeURL(func(spiceboxv1alpha1.ResolvedSidecarToolbox) string { return h.MCP.URL() })

	h.MCP.AnnounceTools([]map[string]any{
		{"name": "echo", "description": "Echoes its input.", "inputSchema": map[string]any{"type": "object"}},
	})
	// echo handler is registered, but FailTransport makes the first three
	// tools/call requests return HTTP 500 (transport-class) regardless. The
	// probe (tools/list) is unaffected, so synthesis still succeeds.
	h.MCP.OnTool("echo", func(args map[string]any) any {
		return map[string]any{"echoed": args["text"]}
	})

	h.WaitForAgentClassValid("sidecar-echo", 30*time.Second)

	// Inject a generous budget of consecutive transport failures for echo. One
	// failed CALL now costs exactly ONE wire attempt: the session cache
	// re-issues a tools/call only on a 404 (the server terminated the session
	// and refused BEFORE dispatch, so the tool provably did not run). A
	// transport failure is an UNDETERMINED outcome — the call may have
	// executed — so it surfaces to the agent instead of being silently
	// replayed. The budget stays above the expected 3 so that if
	// replay-on-blip ever returns, the dispatch assertion below climbs to 6
	// and fails loudly rather than passing on a different mechanism.
	// The breaker's threshold is 3 failed calls; the 3rd crosses it and the
	// 4th call is denied pre-dispatch by toolguard.
	h.MCP.FailTransport("echo", 6)

	echoToolUse := e2e.ToolUse("echo_echo", map[string]any{
		"operation_id": "op-breaker",
		"_reason":      "exercise origin breaker",
		"args":         map[string]any{"text": "ping"},
	})

	// First call kicks off the sequence.
	h.LLM.OnUserMessage("trigger breaker").Reply(echoToolUse)
	// Higher priority: once the result carries the toolguard deny text, stop
	// and reply. Registered BEFORE the re-call rule so it wins on the
	// breaker-open result.
	h.LLM.OnToolResult("echo_echo", func(content any) bool {
		s, ok := content.(string)
		return ok && strings.Contains(s, "circuit breaker open")
	}).Reply(e2e.RespondToUser("sidecar circuit open; stopping"))
	// Lower priority: any result NOT carrying the breaker deny (i.e. a plain
	// transport failure) → call echo_echo again. Repeating so it fires for
	// each of the pre-threshold failures.
	h.LLM.OnToolResult("echo_echo", func(content any) bool {
		s, ok := content.(string)
		return ok && !strings.Contains(s, "circuit breaker open")
	}).Reply(echoToolUse).Repeating()
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.SendUserMessage("trigger breaker")
	h.ExpectAgentReply(e2e.Contains("sidecar circuit open"))

	// The stub must have observed EXACTLY three wire dispatches: the three
	// failed tool calls that drive the breaker open, one attempt each. The 4th
	// call is denied pre-dispatch by toolguard, so it never reaches the wire.
	//
	// This asserted 6 while the session cache replayed a tools/call after any
	// transport error. It no longer does: a transport failure leaves the
	// outcome undetermined — the tool may already have run — so re-issuing it
	// could double a side effect the agent never asked to repeat. Only a 404,
	// which proves the server refused before dispatch, is re-issued. A count of
	// 6 here now means that replay came back.
	var echoCalls int
	for _, c := range h.MCP.Calls() {
		if c.Name == "echo" {
			echoCalls++
		}
	}
	assert.Equal(t, 3, echoCalls,
		"echo dispatched 3 failed calls, one attempt each; the 4th call is denied pre-dispatch by the origin breaker")

	// The LLM observed a tool_result carrying the toolguard origin-breaker
	// deny — naming both the breaker state and the sidecar origin.
	assert.True(t, sawBreakerOpenToolResult(h),
		"a tool_result for echo_echo carried the origin circuit-breaker deny (origin sidecartoolbox/echo-tb)")
}

// applySidecarFixtures applies manifests.yaml and stamps Valid=True on the
// echo-tb SidecarToolbox. The harness does not run the SidecarToolbox
// controller (which would require a probe Pod); the AgentClass controller
// only reads the CR's Valid condition, so stamping it directly is
// sufficient — mirroring how sandbox scenarios stamp SpiceboxSession
// status the harness does not reconcile.
func applySidecarFixtures(t *testing.T, h *e2e.Harness) {
	t.Helper()
	h.ApplyManifest(readManifests(t))
	// The REAL sidecartoolbox controller now runs in this harness, so echo-tb's
	// Valid condition is EARNED rather than stamped: its classCheck phase
	// requires spec.sandbox.class to be Valid=True first, and nothing else sets
	// that (the harness deliberately omits the SpiceboxClass controller). The
	// old hand-stamp of the toolbox is gone — it would only have raced the
	// controller, which promptly re-reconciled it back to
	// Valid=False/ClassInvalid.
	h.StampSpiceboxClassesValid()
}

// readManifests loads manifests.yaml next to this test file.
func readManifests(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller(0) must resolve this test file")
	path := filepath.Join(filepath.Dir(thisFile), "manifests.yaml")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "read manifests.yaml")
	return string(raw)
}

// advertisedToolNames returns the set of tool names the runner advertised
// to the scripted LLM across all requests. Used to assert allowlist
// enforcement at the agent's tool surface.
func advertisedToolNames(h *e2e.Harness) map[string]bool {
	names := map[string]bool{}
	for _, req := range h.LLM.Requests() {
		for _, td := range req.Tools {
			names[td.Name] = true
		}
	}
	return names
}

// sawBreakerOpenToolResult reports whether any request the LLM observed
// carried a tool_result whose content is the toolguard origin-breaker deny:
// it must name both the breaker state ("circuit breaker open") and the
// sidecar origin ("origin sidecartoolbox/echo-tb") so the model can tell
// WHICH sidecar tripped.
func sawBreakerOpenToolResult(h *e2e.Harness) bool {
	for _, req := range h.LLM.Requests() {
		for _, m := range req.Messages {
			for _, c := range m.Content {
				if c.Type == "tool_result" && c.ToolResult != nil &&
					strings.Contains(c.ToolResult.Content, "circuit breaker open") &&
					strings.Contains(c.ToolResult.Content, "origin sidecartoolbox/echo-tb") {
					return true
				}
			}
		}
	}
	return false
}
