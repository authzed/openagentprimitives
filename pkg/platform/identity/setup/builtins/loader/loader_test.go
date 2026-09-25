package loader_test

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/loader"
)

func TestAllBuiltinsRegistered(t *testing.T) {
	names := []string{}
	for _, f := range builtins.All() {
		names = append(names, f.Name())
	}
	sort.Strings(names)
	assert.Equal(t, []string{
		"anthropic-oauth", "github-pat", "kubectl-kubeconfig", "oauth-mcp",
		"onepassword-scim", "slack-bot-token", "tailscale-authkey",
	}, names)
}
