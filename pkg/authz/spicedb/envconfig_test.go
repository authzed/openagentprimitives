package spicedb_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
)

// TestLoadEnvConfig sweeps the env-var combinations the operator,
// runner, and channelsd binaries all rely on to find SpiceDB.
// Endpoint + Token are both REQUIRED; Insecure is a non-fatal bool.
// Missing required vars must produce an error naming them — the
// runner's startup gate uses that error to refuse to start, so silent
// fallbacks would re-introduce the "per-tool authz disabled" footgun.
func TestLoadEnvConfig(t *testing.T) {
	cases := []struct {
		name        string
		endpoint    string
		token       string
		insecure    string
		wantErr     bool
		wantInclude []string // substrings the error must mention
		wantCfg     spicedb.EnvConfig
	}{
		{
			name:     "happy: all set with insecure=true",
			endpoint: "spicedb.svc:50051", token: "shhh", insecure: "true",
			wantCfg: spicedb.EnvConfig{Endpoint: "spicedb.svc:50051", Token: "shhh", Insecure: true},
		},
		{
			name:     "happy: insecure unset defaults to false",
			endpoint: "spicedb.svc:50051", token: "shhh", insecure: "",
			wantCfg: spicedb.EnvConfig{Endpoint: "spicedb.svc:50051", Token: "shhh", Insecure: false},
		},
		{
			name:     "happy: insecure=anything-but-true is false",
			endpoint: "spicedb.svc:50051", token: "shhh", insecure: "yes",
			wantCfg: spicedb.EnvConfig{Endpoint: "spicedb.svc:50051", Token: "shhh", Insecure: false},
		},
		{
			name:     "error: endpoint missing",
			endpoint: "", token: "shhh", insecure: "true",
			wantErr:     true,
			wantInclude: []string{"SPICEDB_ENDPOINT"},
		},
		{
			name:     "error: token missing",
			endpoint: "spicedb.svc:50051", token: "", insecure: "true",
			wantErr:     true,
			wantInclude: []string{"SPICEDB_TOKEN"},
		},
		{
			name:     "error: both missing lists both",
			endpoint: "", token: "", insecure: "",
			wantErr:     true,
			wantInclude: []string{"SPICEDB_ENDPOINT", "SPICEDB_TOKEN"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(spicedb.EnvEndpoint, tc.endpoint)
			t.Setenv(spicedb.EnvToken, tc.token)
			t.Setenv(spicedb.EnvTokenPath, "")
			t.Setenv(spicedb.EnvInsecure, tc.insecure)

			got, err := spicedb.LoadEnvConfig()
			if tc.wantErr {
				require.Error(t, err, "expected an error")
				for _, sub := range tc.wantInclude {
					assert.Contains(t, err.Error(), sub, "error must mention the missing var")
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantCfg, got)
		})
	}
}

// TestLoadEnvConfigTokenPath covers SPICEDB_TOKEN_PATH: when set and
// non-empty it overrides SPICEDB_TOKEN with the (trimmed) file contents;
// a missing file is a hard error; SPICEDB_TOKEN remains the fallback
// when no path is set. This keeps the runner from carrying the preshared
// token as a plaintext env var — it reads it from a mounted file instead.
func TestLoadEnvConfigTokenPath(t *testing.T) {
	const endpoint = "spicedb.svc:50051"

	writeTokenFile := func(t *testing.T, contents string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "spicedb-token")
		require.NoError(t, os.WriteFile(p, []byte(contents), 0o600))
		return p
	}

	cases := []struct {
		name        string
		token       string // SPICEDB_TOKEN
		tokenPath   func(t *testing.T) string
		wantToken   string
		wantErr     bool
		wantInclude []string
	}{
		{
			name:      "only SPICEDB_TOKEN set: that value is used",
			token:     "env-token",
			tokenPath: func(*testing.T) string { return "" },
			wantToken: "env-token",
		},
		{
			name:      "SPICEDB_TOKEN_PATH set: trimmed file contents override env token",
			token:     "env-token",
			tokenPath: func(t *testing.T) string { return writeTokenFile(t, "  file-token\n") },
			wantToken: "file-token",
		},
		{
			name:        "SPICEDB_TOKEN_PATH points at a missing file: error",
			token:       "env-token",
			tokenPath:   func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope") },
			wantErr:     true,
			wantInclude: []string{"SPICEDB_TOKEN_PATH"},
		},
		{
			name:        "neither token nor path set: required error",
			token:       "",
			tokenPath:   func(*testing.T) string { return "" },
			wantErr:     true,
			wantInclude: []string{"SPICEDB_TOKEN"},
		},
		{
			name:  "SPICEDB_TOKEN_PATH set but file is whitespace-only: error names path, not SPICEDB_TOKEN",
			token: "",
			tokenPath: func(t *testing.T) string {
				return writeTokenFile(t, "  \n\t")
			},
			wantErr: true,
			// Error must name the file path and say "empty"; it must NOT
			// instruct the operator to set SPICEDB_TOKEN (runner pods use
			// SPICEDB_TOKEN_PATH, never the env-var form).
			wantInclude: []string{"empty", "spicedb-token"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(spicedb.EnvEndpoint, endpoint)
			t.Setenv(spicedb.EnvToken, tc.token)
			t.Setenv(spicedb.EnvTokenPath, tc.tokenPath(t))

			got, err := spicedb.LoadEnvConfig()
			if tc.wantErr {
				require.Error(t, err)
				for _, sub := range tc.wantInclude {
					assert.Contains(t, err.Error(), sub)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantToken, got.Token, "resolved token")
			assert.Equal(t, endpoint, got.Endpoint)
		})
	}
}

// TestSharedResourceNames pins the install-time ConfigMap + Secret
// names so the agentsession podspec, the operator deployment, and the
// channelsd token mount all reference the same constants. Drift in
// the names is exactly what creates accidentally-disabled SpiceDB.
func TestSharedResourceNames(t *testing.T) {
	assert.Equal(t, "spicebox-spicedb-config", spicedb.SharedConfigMapName)
	assert.Equal(t, "endpoint", spicedb.SharedConfigMapEndpointKey)
	assert.Equal(t, "spicebox-spicedb-token", spicedb.SharedTokenSecretName)
	assert.Equal(t, "token", spicedb.SharedTokenSecretKey)
	assert.Equal(t, "SPICEDB_ENDPOINT", spicedb.EnvEndpoint)
	assert.Equal(t, "SPICEDB_TOKEN", spicedb.EnvToken)
	assert.Equal(t, "SPICEDB_INSECURE", spicedb.EnvInsecure)
}
