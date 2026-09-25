package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const noAuthConfig = `
baseURL: https://api.example.test
auth: {type: none}
operations:
  - {name: get_thing, method: GET, path: /x}
`

const bearerAuthConfig = `
baseURL: https://api.example.test
auth: {type: bearer, envVar: API_TOKEN}
operations:
  - {name: get_thing, method: GET, path: /x}
`

// TestLoadFromEnv covers loadFromEnv's fail-closed env contract: run() ends
// in ListenAndServe, so without this extraction none of these refusal
// branches would ever be reachable from a test.
func TestLoadFromEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(t *testing.T, credential, port, path string)
	}{
		{
			name:    "missing config: err names AP_SIDECAR_CONFIG",
			wantErr: "AP_SIDECAR_CONFIG is required",
		},
		{
			name:    "bad config: err names the parse failure",
			env:     map[string]string{"AP_SIDECAR_CONFIG": "not: [valid: yaml: at: all"},
			wantErr: "parse AP_SIDECAR_CONFIG",
		},
		{
			name: "configured-but-empty credential env: err names the envVar",
			env: map[string]string{
				"AP_SIDECAR_CONFIG": bearerAuthConfig,
				"MCP_PORT":          "8080",
				// API_TOKEN deliberately unset: auth.type is "bearer" but the
				// environment never populated it.
			},
			wantErr: `auth.envVar "API_TOKEN" is set in the config but empty in the environment`,
		},
		{
			name: "missing port: err names MCP_PORT",
			env: map[string]string{
				"AP_SIDECAR_CONFIG": noAuthConfig,
			},
			wantErr: "MCP_PORT is required",
		},
		{
			name: "defaulted path: MCP_PATH unset defaults to /mcp",
			env: map[string]string{
				"AP_SIDECAR_CONFIG": noAuthConfig,
				"MCP_PORT":          "8080",
			},
			check: func(t *testing.T, credential, port, path string) {
				assert.Equal(t, "", credential)
				assert.Equal(t, "8080", port)
				assert.Equal(t, "/mcp", path)
			},
		},
		{
			name: "happy path: the credential is trimmed of a trailing newline",
			env: map[string]string{
				"AP_SIDECAR_CONFIG": bearerAuthConfig,
				"API_TOKEN":         "sekret\n",
				"MCP_PORT":          "8080",
				"MCP_PATH":          "/custom",
			},
			check: func(t *testing.T, credential, port, path string) {
				assert.Equal(t, "sekret", credential, "a --from-file Secret's trailing newline must not reach the Bearer header")
				assert.Equal(t, "8080", port)
				assert.Equal(t, "/custom", path)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"AP_SIDECAR_CONFIG", "API_TOKEN", "MCP_PORT", "MCP_PATH"} {
				t.Setenv(k, tc.env[k])
			}
			_, credential, port, path, err := loadFromEnv()
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			tc.check(t, credential, port, path)
		})
	}
}
