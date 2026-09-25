//go:build e2e

package list_companies_test

import (
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestCenterdot_ListCompanies_HappyPath exercises the full
// no-approval-needed flow:
//
//	user "companies created in the last 2 weeks"
//	→ LLM emits tool_use centerdot_list_companies(sinceDays=14)
//	→ MCP stub returns 3 companies
//	→ LLM emits respond_to_user("Found 3 companies: Acme, Beta, Centerdot")
//	→ harness sees the outbound text.
//
// Tool naming: the centerdot AgentClass declares
// mcpServers[0].name: centerdot, so the runner synthesizes LLM-facing
// tool names as <prefix>_<tool> (see pkg/agent/tool/mcp/mcp_tool.go:
// llmName = <serverName>_<toolName>). The ScriptedLLM matchers below
// use the prefixed name `centerdot_list_companies`; the MCPStub's
// OnTool uses the bare upstream name `list_companies` (the runner's
// MCP dispatcher strips the prefix before forwarding to the server).
func TestCenterdot_ListCompanies_HappyPath(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: "../../../testdata/agent-centerdot-companies",
	})

	// MCP backing. Register before WaitForAgentClassValid so the
	// MCPServer controller's tools/list probe sees both allowlisted
	// tools and the AgentClass binding-coverage check passes
	// (otherwise: Valid=False reason=AllowlistDrift).
	// list_contacts_for_company is unused in this scenario but must
	// be registered to satisfy the allowlist.
	h.MCP.OnTool("list_companies", func(args map[string]any) any {
		return map[string]any{
			"results": []map[string]any{
				{"id": "acme-id", "name": "Acme", "ownerId": "owner-1"},
				{"id": "beta-id", "name": "Beta", "ownerId": "owner-2"},
				{"id": "centerdot-id", "name": "Centerdot", "ownerId": "owner-1"},
			},
		}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})

	// Block until the AgentClass converges; otherwise the first
	// SendUserMessage races the controller chain and the synthetic
	// inbound can land before the AgentSession controller is ready.
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)
	// Also block on SpiceDBBootstrap: a barrier that the whole fixture
	// (not just the AgentClass) is applied before traffic is driven.
	// AgentClass Valid does NOT imply it — they are independent
	// reconcile loops with a 5s guardian debounce between them.
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	// LLM script. The first rule fires on the human inbound and asks
	// the runner to dispatch centerdot_list_companies. The second
	// rule fires after the tool_result comes back and emits the final
	// respond_to_user reply. The third rule handles respond_to_user's
	// "delivered" tool_result: the runner asks the LLM what to do
	// next, and we end the turn — otherwise the runner would loop
	// indefinitely (or until "no rule matched" trips t.Fatalf from
	// the runner goroutine while ExpectAgentReply is still polling).
	// MCP tools require operation_id + _reason audit fields wrapping the
	// actual args; SessionContext.Operations isn't wired in the harness
	// factory, so any non-empty operation_id passes the registered-id
	// check (see mcp/dispatch.go Execute).
	h.LLM.OnUserMessage("companies created in the last 2 weeks").
		Reply(e2e.ToolUse("centerdot_list_companies", map[string]any{
			"operation_id": "op-1",
			"_reason":      "user asked for recent companies",
			"args":         map[string]any{"sinceDays": 14},
		}))
	h.LLM.OnToolResult("centerdot_list_companies", e2e.AnyResult()).
		Reply(e2e.RespondToUser("Found 3 companies: Acme, Beta, Centerdot"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.EndTurn())

	// Drive.
	h.SendUserMessage("companies created in the last 2 weeks")
	h.ExpectAgentReply(e2e.Contains("Acme", "Beta", "Centerdot"))
	h.AssertAllRulesConsumed()
}
