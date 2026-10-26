package agentcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistryOptions(t *testing.T) {
	cases := []struct {
		name     string
		user     string
		pass     string
		wantErr  bool
		wantCred bool
	}{
		{name: "neither set: anonymous, no credential"},
		{name: "both set: credential carried", user: "ci-bot", pass: "token", wantCred: true},
		{name: "only username set: error naming both vars", user: "ci-bot", wantErr: true},
		{name: "only password set: error naming both vars", pass: "token", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(registryUsernameEnv, tc.user)
			t.Setenv(registryPasswordEnv, tc.pass)

			opts, err := registryOptions(true)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), registryUsernameEnv)
				assert.Contains(t, err.Error(), registryPasswordEnv)
				assert.NotContains(t, err.Error(), "token", "error must not echo the secret")
				return
			}
			require.NoError(t, err)
			assert.True(t, opts.PlainHTTP)
			if !tc.wantCred {
				assert.Nil(t, opts.Credential)
				return
			}
			require.NotNil(t, opts.Credential)
			assert.Equal(t, tc.user, opts.Credential.Username)
			assert.Equal(t, tc.pass, opts.Credential.Password)
		})
	}
}
