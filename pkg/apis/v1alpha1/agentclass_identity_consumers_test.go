package v1alpha1_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// The runner reads the class AgentIdentity, and the per-session Role grants
// that read, only under ResolvesCredentialsInRunner. A sidecar-only class must
// answer false there — its credential is resolved controller-side — while still
// counting as a consumer for the AgentIdentityInUse warning.
func TestAgentClassSpec_IdentityConsumers(t *testing.T) {
	mcp := []v1.AgentClassMCPServerRef{{Name: "gh", Ref: "gh"}}
	bundles := []v1.ToolBundle{{Name: "sh", Class: "sh"}}
	sidecars := []v1.AgentClassSidecarToolboxRef{{Name: "box", Ref: "box"}}

	cases := []struct {
		name         string
		spec         *v1.AgentClassSpec
		wantInRunner bool
		wantConsumes bool
	}{
		{name: "nil spec: neither", spec: nil},
		{name: "no tools: neither", spec: &v1.AgentClassSpec{}},
		{name: "MCPServers: runner-side and consumes", spec: &v1.AgentClassSpec{MCPServers: mcp}, wantInRunner: true, wantConsumes: true},
		{name: "ToolBundles: runner-side and consumes", spec: &v1.AgentClassSpec{ToolBundles: bundles}, wantInRunner: true, wantConsumes: true},
		{name: "SidecarToolboxes only: consumes but NOT runner-side", spec: &v1.AgentClassSpec{SidecarToolboxes: sidecars}, wantConsumes: true},
		{name: "sidecar plus MCP: runner-side and consumes", spec: &v1.AgentClassSpec{SidecarToolboxes: sidecars, MCPServers: mcp}, wantInRunner: true, wantConsumes: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantInRunner, tc.spec.ResolvesCredentialsInRunner(), "ResolvesCredentialsInRunner")
			assert.Equal(t, tc.wantConsumes, tc.spec.ConsumesAgentIdentity(), "ConsumesAgentIdentity")
		})
	}
}
