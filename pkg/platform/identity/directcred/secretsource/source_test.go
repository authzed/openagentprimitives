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

// TestSecretSourceListMultiFaultErrorIsDeterministic pins the Phase B
// final-review hardening fix: parseEntries and entriesByCanonicalHost
// (source.go) now sort their map keys before iterating, so the FIRST fault
// reported from data carrying more than one malformed key is always the
// SAME one across repeated calls -- not whichever key Go's randomized map
// iteration happened to visit first. Each subtest runs List many times over
// the SAME ≥2-fault data and asserts every call returns byte-identical error
// text naming the lexicographically-first offending key.
func TestSecretSourceListMultiFaultErrorIsDeterministic(t *testing.T) {
	const repeats = 25

	cases := []struct {
		name    string
		data    map[string][]byte
		wantErr string // exact error message every repeat must match
	}{
		{
			// Both keys fault in parseEntries (unrecognized suffix) -- the map
			// ranged there is cfg.SecretData itself.
			name: "two keys with unrecognized suffix: always names the lexicographically-first",
			data: map[string][]byte{
				"z.example.com.nickname": []byte("y"),
				"a.example.com.nickname": []byte("x"),
			},
			wantErr: `secretsource: key "a.example.com.nickname" has no recognized suffix`,
		},
		{
			// Both keys parse cleanly in parseEntries; the fault is in
			// entriesByCanonicalHost's cross-validation -- the map ranged there
			// is parseEntries' per-host result, keyed by host (no suffix).
			name: "two orphan hosts: always names the lexicographically-first host",
			data: map[string][]byte{
				"orphanz.example.com.password": []byte("y"),
				"orphana.example.com.username": []byte("x"),
			},
			wantErr: `secretsource: key "orphana.example.com.username" has no matching "orphana.example.com.password" key`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := secretsource.New()
			cfg := directcred.Config{SecretData: tc.data}

			for i := 0; i < repeats; i++ {
				_, err := src.List(context.Background(), cfg)
				require.Error(t, err)
				require.Equal(t, tc.wantErr, err.Error(), "call %d must report the same fault as every other call", i)
			}
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
