package toolcall

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSecretLikeKey(t *testing.T) {
	cases := []struct {
		name string
		keys []string
		want bool
	}{
		{
			name: "secret-like keys → IsSecretLikeKey=true",
			keys: []string{
				"GH_TOKEN", "MY_SECRET", "DB_PASSWORD", "passwd", "API_KEY", "api_key", "API-KEY",
				"PRIVATE_KEY", "ACCESS_KEY", "client_secret", "BEARER", "jwt_value", "MY_OAUTH",
				"PassPhrase", "PIN", "session_cookie", "AUTH_KEY", "credential",
			},
			want: true,
		},
		{
			name: "non-secret keys → IsSecretLikeKey=false",
			keys: []string{
				"LOG_LEVEL", "DEBUG", "GH_HOST", "HTTP_PROXY", "HOME", "PATH", "USER",
				"REGION", "ENVIRONMENT", "TZ", "LANG",
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range tc.keys {
				assert.Equal(t, tc.want, IsSecretLikeKey(k), "IsSecretLikeKey(%q)", k)
			}
		})
	}
}

func TestIsHighEntropyValue(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"32-char hex UUID → not flagged (UUID/hash exemption)", "0123456789abcdef0123456789abcdef", false},
		{"short value → not flagged (length exemption)", "short", false},
		{"GitHub PAT-like (mixed case, len>16) → flagged", "ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ12345", true},
		{"JWT-like (base64url with dots) → flagged", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTYifQ.abcdef0123456789", true},
		{"English text → not flagged (low entropy)", "the quick brown fox jumps over the lazy dog repeatedly aaa", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsHighEntropyValue(tc.value), "IsHighEntropyValue(%q)", tc.value)
		})
	}
}

func TestEnvKeyValid(t *testing.T) {
	cases := []struct {
		name string
		keys []string
		want bool
	}{
		{"shell-safe identifiers → IsValidEnvName=true", []string{"FOO", "FOO_BAR", "_X", "X1", "x_y_2"}, true},
		{"invalid env names → IsValidEnvName=false", []string{"", "1FOO", "FOO-BAR", "FOO BAR", "FOO.BAR"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range tc.keys {
				assert.Equal(t, tc.want, IsValidEnvName(k), "IsValidEnvName(%q)", k)
			}
		})
	}
}
