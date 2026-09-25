//go:build e2e

package artifact_test

import (
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestConversation_PingPong is the canonical T9 roundtrip test: the
// harness boots envtest + controllers + channelsd pipeline + outbound
// relay + fake Channel listener; the test injects "ping" via
// SendUserMessage, the scripted LLM matches OnUserMessage("ping") and
// emits a respond_to_user("pong") tool call, the meta tool publishes the
// envelope on NATS, the outbound relay dispatches it to the fake
// sender, and ExpectAgentReply asserts on the captured outbound.
//
// If this passes the framework is wired end-to-end and richer scenarios
// (T13+) become "write a script, write an Expect" exercises. If it fails
// the failure message names which assertion blew up — usually either
// AgentClass-Valid timing out (controller wiring), no outbound seen
// (respond_to_user not registered / NATS not connected / relay not
// subscribed), or one outbound but text mismatch (LLM script wrong).
func TestConversation_PingPong(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: e2e.TestdataDir("agent-centerdot-companies"),
	})

	// Seed the MCPStub with the centerdot tools so the AgentClass
	// reaches Valid=True (without this the MCPServer controller stamps
	// AllowlistDrift and the AgentSession never spawns). The handlers
	// are placeholders — the ping/pong script never invokes them, but
	// the names must match the AgentClass's mcpServer allowlist
	// (centerdot/02-mcpserver.yaml).
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})

	// The AgentClass must be Valid before the AgentSession controller
	// will let the pipeline-created session through. Without this
	// explicit wait the first SendUserMessage races the controller
	// chain (AgentClass→MCPServer→Guardian→Channel) and can land before
	// any of them have observed the applied CRs.
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)

	// Script the LLM: on any inbound user message containing "ping",
	// reply with a respond_to_user("pong") and end the turn. .Repeating
	// is added so a runner that re-asks (e.g. after spec.prompt cold-
	// start replays the user message AND the in-flight fake-injected
	// message both reach the LLM) doesn't trip "no rule matched."
	h.LLM.OnUserMessage("ping").Reply(e2e.RespondToUser("pong")).Repeating()

	h.SendUserMessage("ping")
	got := h.ExpectAgentReply(e2e.Contains("pong"))
	t.Logf("agent replied: %q", got.Text)
}
