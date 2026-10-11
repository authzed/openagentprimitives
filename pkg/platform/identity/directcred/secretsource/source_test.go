package secretsource_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred/secretsource"
)

func TestKind(t *testing.T) {
	assert.Equal(t, directcred.SourceSecret, secretsource.New().Kind())
}

func TestSecretSourceList(t *testing.T) {
	cases := []struct {
		name     string
		data     map[string][]byte
		wantRefs []directcred.Ref
		wantErr  string // substring expected in the error; empty means no error
	}{
		{
			name: "clean data: two sorted Refs, hosts canonical",
			data: map[string][]byte{
				"b.example.com.username": []byte("bob"),
				"b.example.com.password": []byte("bobpw"),
				"a.example.com.username": []byte("alice"),
				"a.example.com.password": []byte("alicepw"),
			},
			wantRefs: []directcred.Ref{
				{
					ID:    "a.example.com",
					Label: "a.example.com",
					Hosts: []directcred.Host{{Name: "a.example.com", Exact: false}},
				},
				{
					ID:    "b.example.com",
					Label: "b.example.com",
					Hosts: []directcred.Host{{Name: "b.example.com", Exact: false}},
				},
			},
		},
		{
			name: "orphan username key with no matching password: error names the orphan",
			data: map[string][]byte{
				"a.example.com.username":      []byte("alice"),
				"a.example.com.password":      []byte("alicepw"),
				"orphan.example.com.username": []byte("nopass"),
			},
			wantErr: `"orphan.example.com.username"`,
		},
		{
			name: "orphan password key with no matching username: error names the orphan",
			data: map[string][]byte{
				"orphan.example.com.password": []byte("nouser"),
			},
			wantErr: `"orphan.example.com.password"`,
		},
		{
			name: "key with unrecognized suffix: error names the key",
			data: map[string][]byte{
				"a.example.com.username": []byte("alice"),
				"a.example.com.password": []byte("alicepw"),
				"a.example.com.nickname": []byte("al"),
			},
			wantErr: `"a.example.com.nickname"`,
		},
		{
			name: "key with no dot at all: error names the key",
			data: map[string][]byte{
				"nodothere": []byte("x"),
			},
			wantErr: `"nodothere"`,
		},
		{
			name: "non-canonical (mixed-case) host key: fail closed, not silently lowercased",
			data: map[string][]byte{
				"AA.com.username": []byte("alice"),
				"AA.com.password": []byte("alicepw"),
			},
			wantErr: `"AA.com"`,
		},
		{
			name: "host key that fails CanonicalHost outright (contains a scheme): error names the key",
			data: map[string][]byte{
				"https://evil.com.username": []byte("alice"),
				"https://evil.com.password": []byte("alicepw"),
			},
			wantErr: `"https://evil.com"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := secretsource.New()
			cfg := directcred.Config{SecretData: tc.data}

			got, err := src.List(context.Background(), cfg)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantRefs, got)
		})
	}
}

func TestSecretSourceVerify(t *testing.T) {
	t.Run("clean data: zero Pin, no error", func(t *testing.T) {
		src := secretsource.New()
		cfg := directcred.Config{SecretData: map[string][]byte{
			"a.example.com.username": []byte("alice"),
			"a.example.com.password": []byte("alicepw"),
		}}

		pin, err := src.Verify(context.Background(), cfg)
		require.NoError(t, err)
		assert.Equal(t, directcred.Pin{}, pin)
	})

	t.Run("malformed data: orphan key errors, same as List", func(t *testing.T) {
		src := secretsource.New()
		cfg := directcred.Config{SecretData: map[string][]byte{
			"a.example.com.username": []byte("alice"),
		}}

		_, err := src.Verify(context.Background(), cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"a.example.com.username"`)
	})
}

func TestSecretSourceFetch(t *testing.T) {
	data := map[string][]byte{
		"a.example.com.username": []byte("alice"),
		"a.example.com.password": []byte("alicepw"),
		"b.example.com.username": []byte("bob"),
		"b.example.com.password": []byte("bobpw"),
		"b.example.com.totp":     []byte("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"),
	}
	cfg := directcred.Config{SecretData: data}

	t.Run("round-trip: values match, no .totp means nil OTP", func(t *testing.T) {
		src := secretsource.New()
		cred, err := src.Fetch(context.Background(), cfg, "a.example.com")
		require.NoError(t, err)
		assert.Equal(t, "alice", cred.Username)
		assert.Equal(t, "alicepw", cred.Password)
		assert.Nil(t, cred.OTP, "no .totp key present: OTP must be nil")
	})

	t.Run("round-trip: .totp present means non-nil OTP", func(t *testing.T) {
		src := secretsource.New()
		cred, err := src.Fetch(context.Background(), cfg, "b.example.com")
		require.NoError(t, err)
		assert.Equal(t, "bob", cred.Username)
		assert.Equal(t, "bobpw", cred.Password)
		require.NotNil(t, cred.OTP, ".totp key present: OTP must be non-nil")

		code, err := cred.OTP()
		require.NoError(t, err)
		assert.Len(t, code, 6, "bare seed defaults to 6 digits")
	})

	t.Run("unknown id: error", func(t *testing.T) {
		src := secretsource.New()
		_, err := src.Fetch(context.Background(), cfg, "nope.example.com")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nope.example.com")
	})
}
