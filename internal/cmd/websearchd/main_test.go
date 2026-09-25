package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/websearch/bravesearch"
)

// TestLoadFromEnv covers loadFromEnv's fail-closed env contract: run() ends
// in ListenAndServe, so without this extraction none of these refusal
// branches would ever be reachable from a test — mirrors
// internal/cmd/apiadapter's TestLoadFromEnv.
func TestLoadFromEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(t *testing.T, cfg envConfig)
	}{
		{
			name:    "missing WEBSEARCH_API_KEY: err names it",
			env:     map[string]string{"MCP_PORT": "8080"},
			wantErr: "WEBSEARCH_API_KEY is required",
		},
		{
			name: "missing MCP_PORT: err names it",
			env: map[string]string{
				"WEBSEARCH_API_KEY": "k",
			},
			wantErr: "MCP_PORT is required",
		},
		{
			name: "defaults: backend defaults to bravesearch, path defaults to /mcp, artifact store unset",
			env: map[string]string{
				"WEBSEARCH_API_KEY": "k",
				"MCP_PORT":          "8080",
			},
			check: func(t *testing.T, cfg envConfig) {
				assert.Equal(t, bravesearch.KindName, cfg.backendName, "unset WEBSEARCH_BACKEND must default to the one production backend")
				assert.Equal(t, "8080", cfg.port)
				assert.Equal(t, "/mcp", cfg.path)
				assert.Empty(t, cfg.artifactStoreURL, "unset ARTIFACT_STORE_URL is a valid, if degraded, configuration — not a startup failure")
			},
		},
		{
			name: "explicit overrides are all honored",
			env: map[string]string{
				"WEBSEARCH_BACKEND":  "some-other-backend",
				"WEBSEARCH_API_KEY":  "k",
				"MCP_PORT":           "9090",
				"MCP_PATH":           "/custom",
				"ARTIFACT_STORE_URL": "mem://",
			},
			check: func(t *testing.T, cfg envConfig) {
				assert.Equal(t, "some-other-backend", cfg.backendName, "loadFromEnv does not validate the backend name — that is run()'s registry.Get job")
				assert.Equal(t, "9090", cfg.port)
				assert.Equal(t, "/custom", cfg.path)
				assert.Equal(t, "mem://", cfg.artifactStoreURL)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"WEBSEARCH_BACKEND", "WEBSEARCH_API_KEY", "MCP_PORT", "MCP_PATH", "ARTIFACT_STORE_URL"} {
				t.Setenv(k, tc.env[k])
			}
			cfg, err := loadFromEnv()
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			tc.check(t, cfg)
		})
	}
}

// TestRun_UnknownBackend_FailsClosedBeforeServing pins that run() refuses an
// unregistered WEBSEARCH_BACKEND name rather than falling back to whatever
// the registry happens to have (registry.Get's own fail-closed contract —
// see pkg/tools/websearch/registry). This does not need to touch the
// registry at all: an unregistered name is refused whether or not anything
// else is ever registered, so this test never risks another test's
// registration.
func TestRun_UnknownBackend_FailsClosedBeforeServing(t *testing.T) {
	t.Setenv("WEBSEARCH_BACKEND", "no-such-backend-registered-anywhere")
	t.Setenv("WEBSEARCH_API_KEY", "k")
	t.Setenv("MCP_PORT", "0")
	err := run()
	require.Error(t, err, "run() must fail before ever reaching ListenAndServe")
	assert.Contains(t, err.Error(), "no websearch backend registered")
}

// TestRun_MalformedArtifactStoreURL_FailsClosedBeforeServing pins that a
// NON-empty but malformed ARTIFACT_STORE_URL is refused at startup, unlike
// its absence (which loadFromEnv treats as a valid, if degraded,
// configuration — see TestLoadFromEnv's "defaults" case). blob.Open refuses
// a URL with no "://" scheme synchronously, before any network call, so this
// returns well before ListenAndServe with no risk of hanging the test.
func TestRun_MalformedArtifactStoreURL_FailsClosedBeforeServing(t *testing.T) {
	t.Setenv("WEBSEARCH_API_KEY", "k")
	t.Setenv("MCP_PORT", "0")
	t.Setenv("ARTIFACT_STORE_URL", "not-a-well-formed-url")
	err := run()
	require.Error(t, err, "run() must fail before ever reaching ListenAndServe")
	assert.Contains(t, err.Error(), "open artifact store")
}
