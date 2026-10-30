package oap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const capabilitiesYAML = `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: MCPServer
metadata:
  name: tracker
spec:
  server:
    url: https://tracker.example.com/mcp
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SidecarToolbox
metadata:
  name: browser
spec:
  source:
    image: ghcr.io/example/browser:1
  sandbox:
    class: default
    network:
      allowedHosts: [docs.example.com]
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SidecarToolbox
metadata:
  name: scripted
spec:
  source:
    inline:
      baseImage: python:3
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SpiceboxToolspec
metadata:
  name: gh
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SpiceboxClass
metadata:
  name: open
spec:
  network:
    mode: allowlist
    allowedHosts: [api.example.com]
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SpiceboxClass
metadata:
  name: sealed
spec:
  network:
    mode: none
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentIdentity
metadata:
  name: bot
spec:
  credentials:
  - name: tracker-token
    type: static
    allowedHosts: [tracker.example.com]
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
`

func TestSummarizeCapabilities(t *testing.T) {
	child := dependencyBundle(t, "helper")
	child.Manifest.Requires.Secrets = []RequiredSecret{{Name: "helper-key"}}
	root := dependencyBundle(t, "root", child)
	root.Manifests = append(root.Manifests, []byte("---\n"+capabilitiesYAML)...)
	root.Manifest.Requires.Secrets = []RequiredSecret{{Name: "tracker-api"}}
	root.Manifest.Requires.ClusterDeps = []RequiredClusterDep{{Kind: "SpiceboxClass", Name: "default"}}

	c, err := SummarizeCapabilities(root)
	require.NoError(t, err)
	assert.False(t, c.Empty())
	assert.Equal(t, []string{"tracker (https://tracker.example.com/mcp)"}, c.MCPServers)
	assert.Equal(t, []string{"browser (ghcr.io/example/browser:1)", "scripted (built from an inline script)"}, c.SidecarToolboxes)
	assert.Equal(t, []string{"gh"}, c.Toolspecs)
	assert.Equal(t, []string{
		"toolbox browser: docs.example.com",
		"sandbox open: allowlist (api.example.com)",
		"credential bot/tracker-token: tracker.example.com",
	}, c.Network, "a sandbox class with mode none grants no egress and is not listed")
	assert.Equal(t, []string{"bot/tracker-token (static)"}, c.Credentials)
	assert.Equal(t, []string{"tracker-api", "helper-key"}, c.Secrets, "the root's grants lead, then each dependency's")
	assert.Equal(t, []string{"SpiceboxClass/default"}, c.ClusterDeps)
	assert.Equal(t, []string{"helper v1.0.0"}, c.Agents)
}

func TestSummarizeCapabilities_BareAgentIsEmpty(t *testing.T) {
	c, err := SummarizeCapabilities(dependencyBundle(t, "solo"))
	require.NoError(t, err)
	assert.True(t, c.Empty())
}

func TestSummarizeCapabilities_UndecodableCRIsAnError(t *testing.T) {
	b := dependencyBundle(t, "root")
	b.Manifests = append(b.Manifests, []byte("---\napiVersion: agentprimitives.authzed.com/v1alpha1\nkind: SpiceboxClass\nmetadata:\n  name: odd\nspec:\n  network: 7\n")...)
	_, err := SummarizeCapabilities(b)
	require.Error(t, err, "a CR the summary cannot read must not be silently left out of the prompt")
	assert.Contains(t, err.Error(), `SpiceboxClass "odd"`)
}
