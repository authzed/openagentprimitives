package agentcmd

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
)

func TestAgentSign_KeyFlagRequired(t *testing.T) {
	cmd := newAgentSignCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{"registry.example/team/demo-agent:v1"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key")
}

func TestAgentSign_MissingKeyFile(t *testing.T) {
	cmd := newAgentSignCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{
		"registry.example/team/demo-agent:v1",
		"--key", filepath.Join(t.TempDir(), "does-not-exist.pem"),
	})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist.pem")
}

func TestAgentSign_GarbageKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := aptest.WriteFile(t, dir, "garbage.pem", "not a real PEM key")

	cmd := newAgentSignCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{"registry.example/team/demo-agent:v1", "--key", path})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a PEM file")
}

func TestAgentSign_MalformedRef(t *testing.T) {
	dir := t.TempDir()
	path, _ := writeECDSAPrivateKeyPEM(t, dir, "key.pem", true)

	cmd := newAgentSignCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{"not-a-ref", "--key", path})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-a-ref")
}

func TestAgentSign_WrongArgCount(t *testing.T) {
	cmd := newAgentSignCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{})
	assert.Error(t, cmd.Execute())
}

func TestAgentSign_PlainHTTPFlagWiring(t *testing.T) {
	cmd := newAgentSignCmd(&apcmd.Globals{})
	flag := cmd.Flags().Lookup("plain-http")
	require.NotNil(t, flag, "sign must register a --plain-http flag")
	assert.Equal(t, "false", flag.DefValue)
}

func TestAgentSign_KeyFlagWiring(t *testing.T) {
	cmd := newAgentSignCmd(&apcmd.Globals{})
	flag := cmd.Flags().Lookup("key")
	require.NotNil(t, flag, "sign must register a --key flag")
	assert.Equal(t, "", flag.DefValue)
}
