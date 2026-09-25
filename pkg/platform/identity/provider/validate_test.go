package provider_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
)

func TestValidateToken(t *testing.T) {
	anthropic, ok := provider.ByID("anthropic-oauth")
	require.True(t, ok, "anthropic-oauth provider must be embedded")
	github, ok := provider.ByID("github-pat")
	require.True(t, ok, "github-pat provider must be embedded")

	// A provider that declares NO format — validation must stay permissive.
	formatless := provider.Provider{ID: "no-format"}
	// A provider declaring an empty pattern — also permissive.
	emptyPattern := provider.Provider{ID: "empty", TokenShape: &provider.TokenShape{Pattern: ""}}

	cases := []struct {
		name    string
		prov    provider.Provider
		token   string
		wantErr bool
	}{
		{name: "anthropic-oauth: sk-ant-oat token accepted", prov: *anthropic, token: "sk-ant-oat01-AbCdEf", wantErr: false},
		{name: "anthropic-oauth: trailing newline trimmed then accepted", prov: *anthropic, token: "sk-ant-oat01-AbCdEf\n", wantErr: false},
		{name: "anthropic-oauth: surrounding whitespace trimmed then accepted", prov: *anthropic, token: "  sk-ant-oat01-AbCdEf  ", wantErr: false},
		{name: "anthropic-oauth: wrong-prefix 92-byte value rejected (the live bug)", prov: *anthropic, token: "zkeI" + repeat("x", 88), wantErr: true},
		{name: "anthropic-oauth: API key (sk-ant-api) rejected, not an OAuth token", prov: *anthropic, token: "sk-ant-api03-XXXXXXXX", wantErr: true},
		{name: "github-pat: classic ghp_ accepted", prov: *github, token: "ghp_AbCdEf0123456789", wantErr: false},
		{name: "github-pat: fine-grained github_pat_ accepted", prov: *github, token: "github_pat_AbCdEf0123", wantErr: false},
		{name: "github-pat: random value rejected", prov: *github, token: "random-not-a-token", wantErr: true},
		{name: "formatless provider: any value permitted", prov: formatless, token: "literally-anything", wantErr: false},
		{name: "empty-pattern provider: any value permitted", prov: emptyPattern, token: "literally-anything", wantErr: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := provider.ValidateToken(tc.prov, tc.token)
			if tc.wantErr {
				require.Error(t, err, "expected a format rejection")
				var fe *provider.TokenFormatError
				assert.ErrorAs(t, err, &fe, "a mismatch must be a *TokenFormatError so callers can show the hint")
				assert.NotEmpty(t, fe.Hint, "the rejection must carry the expected-format hint")
				return
			}
			assert.NoError(t, err, "a correctly-formatted (or formatless) token must be accepted")
		})
	}
}

// repeat avoids pulling strings.Repeat into the table just for one row.
func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
