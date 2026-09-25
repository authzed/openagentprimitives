package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEmbeddedGithubPatHasVerifyConfig pins the catalog contract Task 2+
// depend on: github-pat declares a live-verification probe.
func TestEmbeddedGithubPatHasVerifyConfig(t *testing.T) {
	p, ok := ByID("github-pat")
	require.True(t, ok, "github-pat provider missing from embedded catalog")
	require.NotNil(t, p.Verify, "github-pat must declare a verify: block")
	assert.Equal(t, "https://api.github.com/user", p.Verify.Endpoint)
	assert.Equal(t, "token", p.Verify.AuthScheme)
	assert.Equal(t, "login", p.Verify.SubjectField)
}

// TestValidateVerifyConfig covers the load-time validation rules for a
// verify: block. Table-driven: each case is one way the config can be wrong.
func TestValidateVerifyConfig(t *testing.T) {
	cases := []struct {
		name    string
		cfg     VerifyConfig
		wantErr string // "" = valid
	}{
		{name: "valid minimal: https endpoint only", cfg: VerifyConfig{Endpoint: "https://api.example.com/me"}},
		{name: "valid full: method+scheme+statuses+subject", cfg: VerifyConfig{Endpoint: "https://api.example.com/me", Method: "GET", AuthScheme: "Bearer", SuccessStatuses: []int{200, 204}, SubjectField: "login"}},
		{name: "http endpoint rejected: must be https", cfg: VerifyConfig{Endpoint: "http://api.example.com/me"}, wantErr: "must be https"},
		{name: "empty endpoint rejected", cfg: VerifyConfig{}, wantErr: "endpoint is required"},
		{name: "unparseable endpoint rejected", cfg: VerifyConfig{Endpoint: "https://%zz"}, wantErr: "endpoint"},
		{name: "unknown authScheme rejected", cfg: VerifyConfig{Endpoint: "https://a.example.com", AuthScheme: "basic"}, wantErr: "authScheme"},
		{name: "unknown method rejected", cfg: VerifyConfig{Endpoint: "https://a.example.com", Method: "DELETE"}, wantErr: "method"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVerifyConfig(&tc.cfg)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
