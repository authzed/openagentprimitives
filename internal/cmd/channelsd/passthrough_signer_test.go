package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// TestLoadPassthroughSignerRejectsUnsafeKeys pins the same startup floor webd
// enforces on the shared spicebox-passthrough-link-key Secret. channelsd is
// the *minter* of credential deep-links; a guessable key here lets anyone
// forge a link that identityd will honour.
//
// channelsd deliberately does not crash on a bad key — the caller logs and
// runs with the passthrough watcher disabled — so the contract under test is
// that an unsafe key produces an error and a nil Signer, never a usable
// Signer built on weak material.
func TestLoadPassthroughSignerRejectsUnsafeKeys(t *testing.T) {
	full := hex.EncodeToString([]byte(strings.Repeat("a", passthroughlink.MinKeyLen)))
	cases := []struct {
		name    string
		file    string
		wantErr string // substring the error must mention; "" means expect success
	}{
		{
			name:    "empty file: rejected, so a zero-length HMAC key can never sign a deep-link",
			file:    "",
			wantErr: "empty",
		},
		{
			name:    "whitespace-only file: rejected, trimming must not yield a zero-length key",
			file:    "  \n",
			wantErr: "empty",
		},
		{
			name:    "one-byte key: rejected as brute-forceable, below the 32-byte floor",
			file:    "ff",
			wantErr: "too short",
		},
		{
			name:    "31-byte key: rejected, the floor is not off by one",
			file:    hex.EncodeToString([]byte(strings.Repeat("a", passthroughlink.MinKeyLen-1))),
			wantErr: "too short",
		},
		{
			name:    "non-hex file: rejected as not valid hex",
			file:    "zzzz",
			wantErr: "not valid hex",
		},
		{
			name: "32-byte key: accepted, the shipped install-generated size",
			file: full,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key")
			require.NoError(t, os.WriteFile(path, []byte(tc.file), 0o600))

			signer, err := loadPassthroughSigner(path)
			if tc.wantErr == "" {
				require.NoError(t, err, "a well-formed key of the shipped size must load")
				assert.NotNil(t, signer)
				return
			}
			require.Error(t, err, "unsafe key material must not produce a Signer")
			assert.Contains(t, err.Error(), tc.wantErr, "error should name why the key was refused")
			assert.Nil(t, signer, "no Signer may be returned alongside an error")
		})
	}
}
