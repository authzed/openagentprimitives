package toolscmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

func TestToolsGet_MCPServer(t *testing.T) {
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "ns"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://x", Transport: "streamable-http"},
			Auth:   spiceboxv1alpha1.MCPServerAuth{Provider: "linear-provider"},
			Tools:  []spiceboxv1alpha1.MCPServerTool{{Name: "search", Permission: &authz.Permission{StateImpact: authz.Passthrough}}},
		},
	}
	c := aptest.ClientBuilder(t).WithObjects(mcp).Build()

	prev := toolsListClientFactory
	defer func() { toolsListClientFactory = prev }()
	toolsListClientFactory = func(_ *apcmd.Globals) (client.Client, string, error) {
		return c, "ns", nil
	}

	out, err := runTools(t, "get", "linear")
	require.NoErrorf(t, err, "tools get; out=%s", out)
	for _, want := range []string{"MCPServer/linear", "https://x", "provider:", "linear-provider", "search"} {
		assert.Containsf(t, out, want, "out missing %q", want)
	}
}

func TestToolsGet_NotFound(t *testing.T) {
	c := aptest.ClientBuilder(t).Build()
	prev := toolsListClientFactory
	defer func() { toolsListClientFactory = prev }()
	toolsListClientFactory = func(_ *apcmd.Globals) (client.Client, string, error) {
		return c, "ns", nil
	}
	_, err := runTools(t, "get", "missing")
	require.Error(t, err, "expected not-found error")
	assert.Contains(t, err.Error(), "not found", "error should mention 'not found'")
}

func TestToolsGet_Ambiguous(t *testing.T) {
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "ns"},
	}
	// SpiceboxToolspec is cluster-scoped — Resolve looks it up at "".
	tsp := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "linear"},
	}
	c := aptest.ClientBuilder(t).WithObjects(mcp, tsp).Build()
	prev := toolsListClientFactory
	defer func() { toolsListClientFactory = prev }()
	toolsListClientFactory = func(_ *apcmd.Globals) (client.Client, string, error) {
		return c, "ns", nil
	}
	_, err := runTools(t, "get", "linear")
	require.Error(t, err, "expected ambiguous error")
	assert.Contains(t, err.Error(), "ambiguous", "error should mention ambiguity")
}
