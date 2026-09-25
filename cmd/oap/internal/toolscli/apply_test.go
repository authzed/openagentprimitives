package toolscli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/toolscli"

	// Side-effect imports register the kinds.
	_ "github.com/authzed/openagentprimitives/pkg/tools/kinds/mcp"
	_ "github.com/authzed/openagentprimitives/pkg/tools/kinds/sandbox"
)

const multiDoc = `
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

func TestApplyStream_BothKinds(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	results, err := toolscli.ApplyStream(context.Background(), c, "ns", strings.NewReader(multiDoc))
	require.NoError(t, err, "ApplyStream must succeed")
	require.Len(t, results, 2, "two docs should produce two results")

	gotKinds := map[string]bool{}
	for _, r := range results {
		assert.NoError(t, r.Err, "result %s/%s err", r.Kind, r.Name)
		gotKinds[r.Kind] = true
	}
	assert.True(t, gotKinds["MCPServer"], "MCPServer applied")
	assert.True(t, gotKinds["SpiceboxToolspec"], "SpiceboxToolspec applied")
}

func TestApplyStream_BadDocContinues(t *testing.T) {
	const stream = `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: MCPServer
metadata:
  name: linear
spec:
  server: { url: https://x, transport: streamable-http }
  tools: []
---
not yaml: : :  :
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SpiceboxToolspec
metadata:
  name: git
spec:
  toolkit: { name: git, revision: r1 }
  allowSubcommands: []
`
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	results, err := toolscli.ApplyStream(context.Background(), c, "ns", strings.NewReader(stream))
	require.NoError(t, err, "ApplyStream itself should not error on a bad doc")
	require.Len(t, results, 3, "three docs (including the malformed one) should produce three results")

	assert.NoError(t, results[0].Err, "first doc applies successfully")
	assert.Error(t, results[1].Err, "malformed doc must produce an error")
	assert.NoError(t, results[2].Err, "third doc still applies after the bad one")
}

func TestApplyStream_Empty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	results, err := toolscli.ApplyStream(context.Background(), c, "ns", strings.NewReader(""))
	require.NoError(t, err, "ApplyStream on empty input must succeed")
	assert.Empty(t, results, "empty input yields no results")
}
