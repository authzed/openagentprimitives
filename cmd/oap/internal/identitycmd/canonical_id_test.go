package identitycmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func TestIdentityCanonicalIDCmd(t *testing.T) {
	cmd := newIdentityCanonicalIDCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{"alice@example.com"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(t, cmd.Execute(), "execute canonical-id")

	expected, err := identity.EmailReference("alice@example.com").Canonical()
	require.NoError(t, err)
	assert.Equal(t, expected.String(), strings.TrimSpace(out.String()), "canonical id should match identity.Principal.Canonical")
}

func TestIdentityCanonicalIDCmd_RequiresArg(t *testing.T) {
	cmd := newIdentityCanonicalIDCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{})
	assert.Error(t, cmd.Execute(), "missing email arg should error")
}
