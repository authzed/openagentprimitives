package agentcmd

import (
	"io"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
)

func TestRegistryOptions(t *testing.T) {
	cases := []struct {
		name     string
		user     string
		pass     string
		host     string
		wantErr  string
		wantCred bool

		plainCreds     string
		wantPlainCreds bool
	}{
		{name: "none set: anonymous, no credential"},
		{name: "all set: credential bound to host", user: "ci-bot", pass: "token", host: "ghcr.io", wantCred: true},
		{name: "host:port accepted", user: "ci-bot", pass: "token", host: "localhost:5000", wantCred: true},
		{name: "only username set: error naming all vars", user: "ci-bot", wantErr: registryHostEnv},
		{name: "only password set: error naming all vars", pass: "token", wantErr: registryHostEnv},
		{name: "username+password without host: error naming all vars", user: "ci-bot", pass: "token", wantErr: registryHostEnv},
		{name: "host alone: error naming all vars", host: "ghcr.io", wantErr: registryUsernameEnv},
		{name: "host with scheme: rejected as not a bare host", user: "ci-bot", pass: "token", host: "https://ghcr.io", wantErr: "bare registry host"},
		{name: "host with path: rejected as not a bare host", user: "ci-bot", pass: "token", host: "ghcr.io/org", wantErr: "bare registry host"},
		{name: "host with trailing space: rejected as not a bare host", user: "ci-bot", pass: "token", host: "ghcr.io ", wantErr: "bare registry host"},
		{name: "host with empty port: rejected", user: "ci-bot", pass: "token", host: "ghcr.io:", wantErr: "empty port"},
		{name: "plain-HTTP opt-in not a bool: error naming the var", user: "ci-bot", pass: "token", host: "registry:5000", plainCreds: "yes please", wantErr: registryPlainHTTPCredsEnv},
		{name: "plain-HTTP opt-in true: carried to opts", user: "ci-bot", pass: "token", host: "registry:5000", plainCreds: "true", wantCred: true, wantPlainCreds: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(registryUsernameEnv, tc.user)
			t.Setenv(registryPasswordEnv, tc.pass)
			t.Setenv(registryHostEnv, tc.host)
			t.Setenv(registryPlainHTTPCredsEnv, tc.plainCreds)

			opts, err := registryOptions(true)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.NotContains(t, err.Error(), "token", "error must not echo the secret")
				return
			}
			require.NoError(t, err)
			assert.True(t, opts.PlainHTTP)
			if !tc.wantCred {
				assert.Nil(t, opts.Credential)
				assert.Empty(t, opts.CredentialHost)
				return
			}
			require.NotNil(t, opts.Credential)
			assert.Equal(t, tc.user, opts.Credential.Username)
			assert.Equal(t, tc.pass, opts.Credential.Password)
			assert.Equal(t, tc.host, opts.CredentialHost)
			assert.Equal(t, tc.wantPlainCreds, opts.AllowPlainHTTPCredential)
			assert.False(t, opts.RequireCredentialHost, "only push and sign opt in to strict host matching")
		})
	}
}

// TestRegistryAuth_PushAndSignRefuseOtherHost: <ref> is the user's own choice
// for push and sign, so a credential host that doesn't match it is a typo and
// must fail naming both hosts, before anything is dialed, rather than going
// out anonymously and coming back as a bare 401.
func TestRegistryAuth_PushAndSignRefuseOtherHost(t *testing.T) {
	t.Setenv(registryUsernameEnv, "ci-bot")
	t.Setenv(registryPasswordEnv, "token")
	t.Setenv(registryHostEnv, "ghcr.io")

	dir := t.TempDir()
	keyPath, _ := writeECDSAPrivateKeyPEM(t, dir, "key.pem", true)
	oapPath := aptest.WriteFile(t, dir, "demo-agent.oap", "never read: the host check runs first")
	const ref = "registry.example/team/demo-agent:v1"

	cases := []struct {
		name string
		cmd  *cobra.Command
		args []string
	}{
		{name: "push to another host: ErrCredentialHostMismatch", cmd: newAgentPushCmd(&apcmd.Globals{}), args: []string{ref, oapPath}},
		{name: "sign on another host: ErrCredentialHostMismatch", cmd: newAgentSignCmd(&apcmd.Globals{}), args: []string{ref, "--key", keyPath}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.cmd.SetArgs(tc.args)
			tc.cmd.SetOut(io.Discard)
			tc.cmd.SetErr(io.Discard)
			err := tc.cmd.Execute()
			require.ErrorIs(t, err, oci.ErrCredentialHostMismatch)
			assert.Contains(t, err.Error(), "registry.example")
			assert.Contains(t, err.Error(), "ghcr.io")
			assert.NotContains(t, err.Error(), "token", "error must not echo the secret")
		})
	}
}
