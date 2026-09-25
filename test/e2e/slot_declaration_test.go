//go:build e2e

package e2e_test

import (
	"testing"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// slotClassOverride renders a centerdot AgentClass whose single slot carries the
// given instance-axis field block.
//
// The WHOLE spec is restated because ExtraManifests REPLACES the object rather
// than patching it — a partial spec drops systemPrompt and the class then fails
// Valid for a reason that has nothing to do with slots.
func slotClassOverride(slotFields string) string {
	return `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: centerdot-companies
  namespace: default
spec:
  displayName: CenterdotBot
  description: "E2E fixture agent. Do not deploy to a real cluster."
  model:
    provider: test
    name: scripted
    apiKey:
      name: centerdot-placeholder
      key: api-key
  systemPrompt:
    inline: |
      You are a fixture agent for the agentprimitives e2e harness.
      DO NOT deploy to a real cluster.
  agentIdentity: centerdot-identity
  mcpServers:
    - name: centerdot
      ref: centerdot-companies
  budget:
    maxTurns: 10
    maxTokens: 50000
    maxDuration: 5m
  authz:
    slots:
      - resourceType: crm_company
        description: "A CRM company record"
        permission: contact_access
` + slotFields
}

// autoGrantFrom with no channel_thread source: the trust policy governs a fill
// source the slot never uses, so it can never take effect.
const slotFieldsInertPolicy = `        fillFrom: [ask]
        autoGrantFrom: [owner]
`

// The same fields in a combination that is coherent.
const slotFieldsWellFormed = `        fillFrom: [channel_thread, ask]
        autoGrantFrom: [owner]
        membership: frozen
`

// A slot declaring a trust policy that can never take effect must stop the
// class becoming runnable.
//
// The controller test already asserts the reason is computed. What this adds is
// the end of that chain: in a running system with every controller live, the
// class never reaches Valid=True, so no session starts on it. That is the
// property the rule exists for, and no unit or controller test observes it.
//
// The rule matters because of WHICH direction it fails. autoGrantFrom names
// whose thread contributions bind a slot without approval; with no
// channel_thread source it governs nothing. Admitting the class would leave a
// field in the YAML that reads as a constraint while authorizing nothing —
// invisible in exactly the way a missing field is not.
func TestE2E_slotDeclaration_inertTrustPolicyBlocksTheClass(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       "testdata/agent-centerdot-companies",
		ExtraManifests: []string{slotClassOverride(slotFieldsInertPolicy)},
	})

	h.WaitForAgentClassInvalid("centerdot-companies",
		spiceboxv1alpha1.ReasonSlotDeclarationInvalid, 30*time.Second)
}

// The companion, and the one that keeps the rule honest: the SAME fields, in a
// coherent combination, must leave the class fully runnable. Without this, a
// validator that rejected every slot carrying the new fields would pass the
// test above and look correct.
func TestE2E_slotDeclaration_wellFormedPolicyLeavesTheClassValid(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       "testdata/agent-centerdot-companies",
		ExtraManifests: []string{slotClassOverride(slotFieldsWellFormed)},
	})

	// The MCP stub must serve the fixture's tools BEFORE validity is awaited:
	// with no handlers registered the served tool list is empty, the MCPServer
	// flips Valid=False/AllowlistDrift, and the class follows it down for a
	// reason that has nothing to do with slots.
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})

	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)
}
