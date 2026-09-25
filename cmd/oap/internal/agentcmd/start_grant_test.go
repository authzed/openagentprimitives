package agentcmd

// The standing override for the session-start gate: `oap agent grant-start`
// writes agentclass#starter, which satisfies agentclass#start_session
// without a per-session platform-admin approval.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

func TestStartGrantCommandTree(t *testing.T) {
	root := NewCmd(&apcmd.Globals{})
	names := map[string]bool{}
	for _, c := range root.Commands() {
		names[c.Name()] = true
	}
	require.NotEmpty(t, names, "oap agent must register subcommands")
	assert.True(t, names["grant-start"], "grant-start subcommand")
	assert.True(t, names["revoke-start"], "revoke-start subcommand")
	assert.True(t, names["list-starters"], "list-starters subcommand")
}
