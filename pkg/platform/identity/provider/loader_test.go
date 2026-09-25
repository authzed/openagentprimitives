package provider_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
)

func TestAllIncludesGithubPat(t *testing.T) {
	all := provider.All()
	require.NotEmpty(t, all, "All() empty; expected at least one provider")
	p, ok := provider.ByID("github-pat")
	require.True(t, ok, "github-pat not found")
	assert.Equal(t, "bearer", p.Shape)
	assert.Equal(t, "github-pat", p.Builtin)
	require.NotNil(t, p.TokenShape, "TokenShape missing")
	assert.NotEmpty(t, p.TokenShape.Pattern)
}

func TestByIDMiss(t *testing.T) {
	_, ok := provider.ByID("does-not-exist")
	assert.False(t, ok, "expected miss")
}

func TestOAuthMCP(t *testing.T) {
	p, ok := provider.ByID("oauth-mcp")
	require.True(t, ok, "oauth-mcp not found")
	assert.Equal(t, "oauth", p.Shape)
	require.NotNil(t, p.OAuth)
	assert.True(t, p.OAuth.DynamicClientRegistration, "DynamicClientRegistration should be true")
}

func TestKubectlKubeconfig(t *testing.T) {
	p, ok := provider.ByID("kubectl-kubeconfig")
	require.True(t, ok, "kubectl-kubeconfig not found")
	assert.Equal(t, "kubeconfig", p.Shape)
	assert.Equal(t, "kubectl-kubeconfig", p.Builtin)
	require.NotNil(t, p.TokenShape, "TokenShape missing")
	assert.NotEmpty(t, p.TokenShape.Pattern)
}

func TestAllReturnsSortedByID(t *testing.T) {
	all := provider.All()
	for i := 1; i < len(all); i++ {
		assert.LessOrEqual(t, all[i-1].ID, all[i].ID,
			"All() not sorted: [%d]=%q > [%d]=%q", i-1, all[i-1].ID, i, all[i].ID)
	}
}
