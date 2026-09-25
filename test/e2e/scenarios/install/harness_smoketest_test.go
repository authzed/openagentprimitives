//go:build e2e

package install_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestHarness_StartReturnsCleanly: empty Options, just verify the
// fields are populated and the MCP stub URL is reachable. Doesn't
// apply any AgentDir or wait for any condition.
func TestHarness_StartReturnsCleanly(t *testing.T) {
	h := e2e.Start(t, e2e.Options{})
	require.NotNil(t, h.LLM)
	require.NotNil(t, h.MCP)
	require.NotNil(t, h.SpiceDB)
	require.NotNil(t, h.K8s)
	require.NotEmpty(t, h.MCP.URL())
}

// TestHarness_AppliesCenterdotAgentDir applies the centerdot YAML
// directory and verifies the CRs land. Does NOT wait for AgentClass
// Valid=True (no controllers running in T7).
func TestHarness_AppliesCenterdotAgentDir(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: e2e.TestdataDir("agent-centerdot-companies"),
	})

	// Proof the apply pass actually wrote to the apiserver: Get the
	// AgentClass back. We don't assert on .Status because controllers
	// aren't running yet (T8).
	var ac spiceboxv1alpha1.AgentClass
	require.NoError(t,
		h.K8s.Get(context.Background(),
			client.ObjectKey{Namespace: "default", Name: "centerdot-companies"},
			&ac),
		"AgentClass centerdot-companies should exist after applyAgentDir")
	require.Equal(t, "CenterdotBot", ac.Spec.DisplayName,
		"AgentClass.Spec.DisplayName round-trips through the typed decode path")
}

// TestHarness_StartsCenterdotAgentValid is the foundational T8 test:
// the harness boots envtest + the controller manager + InProcessRunnerFactory,
// applies the centerdot AgentDir, and verifies the AgentClass converges
// to Valid=True within 30s.
//
// If this passes, every T13+ conversation-driving test has a working
// agent to drive against. If it fails, the WaitForAgentClassValid
// timeout message includes the last observed Valid condition's reason
// + message — the four likely culprits are documented in the T8 plan
// (binding-coverage, MCPServer invalid, SpiceDB schema conflict,
// TestProviderNotAllowed).
func TestHarness_StartsCenterdotAgentValid(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: e2e.TestdataDir("agent-centerdot-companies"),
	})

	// Pre-seed the MCPStub with the two tools the centerdot
	// MCPServer allowlists. Without this the mcpserver controller's
	// tools/list probe sees an empty stub and stamps Valid=False
	// reason=AllowlistDrift, blocking the AgentClass binding-coverage
	// check. The handlers are placeholders — this smoke test never
	// invokes a tool — but the names MUST match the AgentClass spec
	// (centerdot/02-mcpserver.yaml: list_companies +
	// list_contacts_for_company).
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})

	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)
}
