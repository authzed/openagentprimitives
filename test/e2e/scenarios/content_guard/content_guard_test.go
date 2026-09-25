//go:build e2e

// Package content_guard_test exercises the url-allowlist content-guard plugin
// wired through the in-process harness. Two sub-cases:
//
//  1. Block: a tool returns a disallowed URL → the runner withholds the result
//     (IsError=true, Content contains the deny reason). The secret URL never
//     reaches the model-visible tool_result.
//
//  2. Pass: a tool returns only an allowlisted URL → the model sees real content
//     (IsError=false, Content contains the safe URL).
//
// No real names: alice@example.com, trusted.example.com, evil.example.invalid
// are fictional per AGENTS.md.
package content_guard_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/urlallowlist"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// newURLAllowlistInstance builds the url-allowlist inspector for the test
// config: allow *.trusted.example.com, deny everything else, inspect results only.
func newURLAllowlistInstance(t *testing.T) contentguard.Instance {
	t.Helper()
	inst, err := urlallowlist.New().Configure(json.RawMessage(`{
		"rules": [{"domain": "*.trusted.example.com", "action": "allow"}],
		"defaultAction": "deny",
		"points": ["result"]
	}`))
	require.NoError(t, err, "configure url-allowlist inspector")
	return inst
}

// TestURLAllowlist_BlocksDisallowedURL proves that a tool_result carrying
// a URL outside the allowlist is withheld from the LLM (IsError=true) and
// the raw disallowed URL is absent from the model-visible content.
func TestURLAllowlist_BlocksDisallowedURL(t *testing.T) {
	// Non-MCPServer manifests: secrets + AgentClass + Channel. The MCPServer
	// is applied after Start so {{MCP_URL}} can be substituted with the live
	// MCPStub URL, mirroring the federated_mcp_test pattern.
	h := e2e.Start(t, e2e.Options{
		ExtraManifests: []string{nonMCPManifests},
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    "alice@example.com",
	})

	// Apply the MCPServer doc with the live stub URL substituted.
	h.ApplyManifest(strings.ReplaceAll(mcpServerManifest, "{{MCP_URL}}", h.MCP.URL()))

	// Wire BEFORE the session spawns (the Loop is built per-session).
	h.SetContentInspectors(
		[]contentguard.Instance{newURLAllowlistInstance(t)},
		[]string{"url-allowlist"},
	)

	// The stub tool returns a result containing a disallowed URL.
	const disallowedURL = "https://evil.example.invalid/steal?token=s3cr3t"
	h.MCP.OnTool("fetch_data", func(args map[string]any) any {
		return map[string]any{"result": "fetched from " + disallowedURL}
	})

	h.WaitForAgentClassValid("ac-contentguard", 30*time.Second)

	// LLM script:
	//   1. User message → LLM emits tool_use for cg_fetch_data.
	//   2. Content guard blocks the result → tool_result (IsError=true, deny reason).
	//      The predicate asserts the disallowed URL is NOT in the LLM-visible content.
	//   3. LLM handles the blocked result → respond_to_user.
	//   4. respond_to_user tool_result → EndTurn.
	const userMsg = "fetch the data"
	h.LLM.OnUserMessage(userMsg).
		Reply(e2e.ToolUse("cg_fetch_data", map[string]any{
			"operation_id": "op-block-1",
			"_reason":      "user requested data fetch",
			"args":         map[string]any{},
		}))
	h.LLM.OnToolResult("cg_fetch_data", func(result any) bool {
		// The withheld result is a plain string (the deny reason), not JSON.
		text, ok := result.(string)
		if !ok {
			return false
		}
		// Must mention the guard (deny reason from url-allowlist).
		hasGuardReason := strings.Contains(text, "url-allowlist")
		// Must NOT expose the disallowed URL to the model.
		noDisallowedURL := !strings.Contains(text, "evil.example.invalid")
		return hasGuardReason && noDisallowedURL
	}).Reply(e2e.RespondToUser("The fetch was blocked by the URL policy."))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.SendUserMessage(userMsg)
	h.ExpectAgentReply(e2e.Contains("blocked by the URL policy"))

	// The stub DID receive the call (blocking is post-call, not pre-call).
	calls := h.MCP.Calls()
	assert.Len(t, calls, 1, "MCPStub must have received exactly one call")
	if len(calls) > 0 {
		assert.Equal(t, "fetch_data", calls[0].Name)
	}

	h.AssertAllRulesConsumed()
}

// TestURLAllowlist_PassesAllowlistedURL proves that a tool_result carrying
// only an allowlisted URL is passed through unchanged — the model sees the
// real content and no IsError is set.
func TestURLAllowlist_PassesAllowlistedURL(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		ExtraManifests: []string{nonMCPManifests},
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    "alice@example.com",
	})

	h.ApplyManifest(strings.ReplaceAll(mcpServerManifest, "{{MCP_URL}}", h.MCP.URL()))

	h.SetContentInspectors(
		[]contentguard.Instance{newURLAllowlistInstance(t)},
		[]string{"url-allowlist"},
	)

	// The stub tool returns only an allowlisted URL — guard must pass through.
	const allowedURL = "https://api.trusted.example.com/v1/resource"
	h.MCP.OnTool("fetch_data", func(args map[string]any) any {
		return map[string]any{"result": "fetched from " + allowedURL}
	})

	h.WaitForAgentClassValid("ac-contentguard", 30*time.Second)

	// LLM script:
	//   1. User message → LLM emits tool_use for cg_fetch_data.
	//   2. Content guard passes the result → tool_result carries real content.
	//      Predicate asserts the allowlisted domain IS visible to the model.
	//   3. LLM replies → respond_to_user.
	//   4. respond_to_user tool_result → EndTurn.
	const userMsg = "fetch from the trusted source"
	h.LLM.OnUserMessage(userMsg).
		Reply(e2e.ToolUse("cg_fetch_data", map[string]any{
			"operation_id": "op-pass-1",
			"_reason":      "user requested trusted data",
			"args":         map[string]any{},
		}))
	h.LLM.OnToolResult("cg_fetch_data", func(result any) bool {
		// matchToolResult parses the content as JSON: {"result": "fetched from https://..."}.
		m, ok := result.(map[string]any)
		if !ok {
			// Fallback: raw string comparison.
			text, isStr := result.(string)
			return isStr && strings.Contains(text, "trusted.example.com")
		}
		resultStr, _ := m["result"].(string)
		return strings.Contains(resultStr, "trusted.example.com")
	}).Reply(e2e.RespondToUser("Fetched data from trusted source successfully."))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.SendUserMessage(userMsg)
	h.ExpectAgentReply(e2e.Contains("trusted source"))

	// The stub must have received the call.
	calls := h.MCP.Calls()
	assert.Len(t, calls, 1, "MCPStub must have received exactly one call for the pass case")
	if len(calls) > 0 {
		assert.Equal(t, "fetch_data", calls[0].Name)
	}

	h.AssertAllRulesConsumed()
}

// nonMCPManifests holds secrets + AgentClass + Channel. Applied at Start time
// so the MCPServer — whose URL isn't known until the stub starts — can be
// applied separately after Start with the live URL substituted.
const nonMCPManifests = `
apiVersion: v1
kind: Secret
metadata:
  name: cg-llm-creds
  namespace: default
type: Opaque
stringData:
  api-key: "unused-by-the-scripted-llm"
---
apiVersion: v1
kind: Secret
metadata:
  name: cg-fake-creds
  namespace: default
type: Opaque
stringData:
  placeholder: "unused-by-the-fake-channel"
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: ac-contentguard
  namespace: default
spec:
  displayName: "Content Guard Test Agent"
  identityMode: agent
  model:
    provider: test
    name: scripted
    apiKey:
      name: cg-llm-creds
      key: api-key
  systemPrompt:
    inline: "you are a content-guard test agent"
  authz:
    toolCalls:
      mode: disabled
  budget:
    maxTurns: 50
    maxTokens: 100000
    maxDuration: "1h"
  mcpServers:
    - name: cg
      ref: cg-fetcher
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: Channel
metadata:
  name: ac-contentguard-fake
  namespace: default
spec:
  kind: fake
  role: both
  agentClass: ac-contentguard
  credentialsRef:
    secretName: cg-fake-creds
  fake:
    echo: false
`

// mcpServerManifest holds the MCPServer doc with {{MCP_URL}} sentinel.
// Applied after Start so the live MCPStub URL can be substituted.
const mcpServerManifest = `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: MCPServer
metadata:
  name: cg-fetcher
  namespace: default
spec:
  name: cg
  version: v1
  intent: "Content-guard e2e fixture: proves url-allowlist block and pass."
  server:
    url: "{{MCP_URL}}"
    transport: http
  tools:
    - name: fetch_data
      intent: "Fetch data from a URL and return the result."
      args:
        allowedFields: []
      permission:
        stateImpact: stateless
      effects:
        readOnly: true
        idempotent: true
`
