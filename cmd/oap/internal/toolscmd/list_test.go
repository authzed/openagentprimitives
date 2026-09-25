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

func TestToolsList_BothKinds(t *testing.T) {
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "ns"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://x", Transport: "streamable-http"},
			Tools: []spiceboxv1alpha1.MCPServerTool{
				{Name: "t1", Permission: &authz.Permission{StateImpact: authz.Passthrough}},
				{Name: "t2", Permission: &authz.Permission{StateImpact: authz.Passthrough}},
			},
		},
	}
	tsp := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "git"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "git", Revision: "r1"},
			AllowSubcommands: []string{"status"},
		},
	}
	c := aptest.ClientBuilder(t).WithObjects(mcp, tsp).Build()

	prev := toolsListClientFactory
	defer func() { toolsListClientFactory = prev }()
	toolsListClientFactory = func(_ *apcmd.Globals) (client.Client, string, error) {
		return c, "ns", nil
	}

	out, err := runTools(t, "list", "-A")
	require.NoErrorf(t, err, "tools list; out=%s", out)
	for _, want := range []string{"MCPServer", "linear", "https://x", "SpiceboxToolspec", "git"} {
		assert.Containsf(t, out, want, "out missing %q", want)
	}
}

func TestToolsList_Empty(t *testing.T) {
	c := aptest.ClientBuilder(t).Build()
	prev := toolsListClientFactory
	defer func() { toolsListClientFactory = prev }()
	toolsListClientFactory = func(_ *apcmd.Globals) (client.Client, string, error) {
		return c, "ns", nil
	}
	out, err := runTools(t, "list")
	require.NoErrorf(t, err, "tools list; out=%s", out)
	assert.Contains(t, out, "no tools", "list output should report 'no tools'")
}
