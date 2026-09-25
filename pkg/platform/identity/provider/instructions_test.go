package provider_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
)

func TestAnthropicOAuthProviderLoads(t *testing.T) {
	p, ok := provider.ByID("anthropic-oauth")
	require.True(t, ok, "anthropic-oauth provider must be embedded")
	assert.Equal(t, "bearer", p.Shape, "anthropic-oauth is a user-supplied token, not an AP-driven OAuth flow")
	assert.Equal(t, "anthropic-oauth", p.Builtin, "must dispatch to the anthropic-oauth builtin flow")
	assert.Contains(t, p.Instructions, "setup-token",
		"instructions must tell the user to run `claude setup-token`")
	assert.NotEmpty(t, p.DocsURL, "docs link is shown next to the instructions")
}

func TestGithubPatHasInstructions(t *testing.T) {
	p, ok := provider.ByID("github-pat")
	require.True(t, ok)
	assert.NotEmpty(t, p.Instructions, "github-pat must carry user-facing instructions for parity")
}
