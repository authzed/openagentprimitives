package agentcmd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
)

func TestAgentVerify_KeyFlagRequired(t *testing.T) {
	cmd := newAgentVerifyCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{"registry.example/team/demo-agent:v1"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key")
}

func TestAgentVerify_MissingKeyFile(t *testing.T) {
	cmd := newAgentVerifyCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{
		"registry.example/team/demo-agent:v1",
		"--key", filepath.Join(t.TempDir(), "does-not-exist.pub"),
	})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist.pub")
}

func TestAgentVerify_GarbageKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := aptest.WriteFile(t, dir, "garbage.pub", "not a real PEM key")

	cmd := newAgentVerifyCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{"registry.example/team/demo-agent:v1", "--key", path})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a PEM file")
}

func TestAgentVerify_MalformedRef(t *testing.T) {
	dir := t.TempDir()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	path := writeECDSAPublicKeyPEM(t, dir, "key.pub", &priv.PublicKey)

	cmd := newAgentVerifyCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{"not-a-ref", "--key", path})
	err = cmd.Execute()
	require.Error(t, err, "a verify failure must surface as a non-nil RunE error so cobra exits non-zero")
	assert.Contains(t, err.Error(), "not-a-ref")
}

func TestAgentVerify_WrongArgCount(t *testing.T) {
	cmd := newAgentVerifyCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{})
	assert.Error(t, cmd.Execute())
}

func TestAgentVerify_PlainHTTPFlagWiring(t *testing.T) {
	cmd := newAgentVerifyCmd(&apcmd.Globals{})
	flag := cmd.Flags().Lookup("plain-http")
	require.NotNil(t, flag, "verify must register a --plain-http flag")
	assert.Equal(t, "false", flag.DefValue)
}

func TestAgentVerify_KeyFlagWiring(t *testing.T) {
	cmd := newAgentVerifyCmd(&apcmd.Globals{})
	flag := cmd.Flags().Lookup("key")
	require.NotNil(t, flag, "verify must register a --key flag")
	assert.Equal(t, "", flag.DefValue)
}
