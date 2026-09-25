package toolscmd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestToolsApply_BothKindsInOneStream(t *testing.T) {
	body := `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: MCPServer
metadata:
  name: linear
spec:
  server: { url: https://x, transport: streamable-http }
  tools: []
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SpiceboxToolspec
metadata:
  name: git
spec:
  toolkit: { name: git, revision: r1 }
  allowSubcommands: [status]
`
	path := aptest.WriteTempYAML(t, body)
	c := aptest.ClientBuilder(t).Build()
	prev := toolsListClientFactory
	defer func() { toolsListClientFactory = prev }()
	toolsListClientFactory = func(_ *apcmd.Globals) (client.Client, string, error) {
		return c, "ns", nil
	}

	out, err := runTools(t, "apply", "-f", path)
	require.NoErrorf(t, err, "tools apply; out=%s", out)
	for _, want := range []string{"MCPServer", "linear", "SpiceboxToolspec", "git"} {
		assert.Containsf(t, out, want, "out missing %q", want)
	}

	// Verify both objects were created in the fake cluster.
	var srv spiceboxv1alpha1.MCPServer
	assert.NoError(t,
		c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "linear"}, &srv),
		"MCPServer should exist post-apply")
	var tsp spiceboxv1alpha1.SpiceboxToolspec
	assert.NoError(t,
		c.Get(context.Background(), client.ObjectKey{Name: "git"}, &tsp),
		"SpiceboxToolspec should exist post-apply")
}

func TestToolsApply_MalformedDocStillContinues(t *testing.T) {
	body := `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: MCPServer
metadata:
  name: linear
spec:
  server: { url: https://x, transport: streamable-http }
  tools: []
---
:: not yaml at all : :
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SpiceboxToolspec
metadata:
  name: git
spec:
  toolkit: { name: git, revision: r1 }
  allowSubcommands: []
`
	path := aptest.WriteTempYAML(t, body)
	c := aptest.ClientBuilder(t).Build()
	prev := toolsListClientFactory
	defer func() { toolsListClientFactory = prev }()
	toolsListClientFactory = func(_ *apcmd.Globals) (client.Client, string, error) {
		return c, "ns", nil
	}

	out, err := runTools(t, "apply", "-f", path)
	require.Error(t, err, "malformed doc should yield non-nil error")
	// The two valid docs should still be reported applied.
	assert.Contains(t, out, "MCPServer", "valid MCPServer should still apply")
	assert.Contains(t, out, "SpiceboxToolspec", "valid SpiceboxToolspec should still apply")
}

func TestToolsApply_StdinDash(t *testing.T) {
	body := `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: MCPServer
metadata: {name: x}
spec:
  server: { url: https://y, transport: streamable-http }
  tools: []
`
	c := aptest.ClientBuilder(t).Build()
	prev := toolsListClientFactory
	defer func() { toolsListClientFactory = prev }()
	toolsListClientFactory = func(_ *apcmd.Globals) (client.Client, string, error) {
		return c, "ns", nil
	}
	root := newToolsCmd(t)
	root.SetIn(strings.NewReader(body))
	var buf strings.Builder
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"apply", "-f", "-"})
	require.NoErrorf(t, root.Execute(), "apply via stdin; out=%s", buf.String())
	assert.Contains(t, buf.String(), "MCPServer", "stdin apply should report MCPServer")
	assert.Contains(t, buf.String(), "x", "stdin apply should name resource 'x'")
}
