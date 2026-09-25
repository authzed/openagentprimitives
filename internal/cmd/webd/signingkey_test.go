package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// writeKeyFile materializes a signing-key file with exactly the given contents
// and returns its path — mirroring what a Kubernetes Secret volume mount hands
// webd at startup.
func writeKeyFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

// TestReadHexKeyRejectsUnsafeKeys pins the startup floor on the HMAC signing
// key webd derives its entire authenticated surface from: the idd_session
// cookie signer, the /content capability token, and the artifact view minter.
//
// The empty case is the one that shipped: the Secret's volume is optional, so
// an ABSENT Secret fails closed, but an externally-provisioned Secret (ESO,
// GitOps) whose "key" is the empty string does not — hex.DecodeString("")
// returns ([]byte{}, nil), and HMAC-SHA256 under a zero-length key is publicly
// computable by anyone.
func TestReadHexKeyRejectsUnsafeKeys(t *testing.T) {
	full := hex.EncodeToString([]byte(strings.Repeat("a", passthroughlink.MinKeyLen)))
	cases := []struct {
		name    string
		file    string
		wantErr string // substring the error must mention; "" means expect success
	}{
		{
			name:    "empty file: rejected, so a zero-length HMAC key can never load",
			file:    "",
			wantErr: "empty",
		},
		{
			name:    "whitespace-only file: rejected, trimming must not yield a zero-length key",
			file:    "  \n\t ",
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
			file:    "not-hex-at-all",
			wantErr: "not valid hex",
		},
		{
			name: "32-byte key: accepted, the shipped install-generated size",
			file: full,
		},
		{
			name: "32-byte key with trailing newline: accepted, mounts often add one",
			file: full + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readHexKey(writeKeyFile(t, tc.file))
			if tc.wantErr == "" {
				require.NoError(t, err, "a well-formed key of the shipped size must load")
				assert.Len(t, got, passthroughlink.MinKeyLen)
				return
			}
			require.Error(t, err, "unsafe key material must not load")
			assert.Contains(t, err.Error(), tc.wantErr, "error should name why the key was refused")
			assert.Nil(t, got, "no key bytes may be returned alongside an error")
		})
	}
}

// TestZeroLengthKeyWouldHaveMadeCookiesForgeable demonstrates the consequence
// the length floor prevents, using only passthroughlink's exported API.
//
// Under a zero-length key, an attacker who knows nothing but the (public)
// payload shape can mint an idd_session cookie naming an arbitrary Subject and
// have webd's own verifier accept it — authenticating as any user at every
// AuthAuthenticated/AuthAuthorized route. The test asserts both halves: that
// the forgery genuinely works when the key is empty (so the floor is load
// bearing, not decoration), and that readHexKey refuses to hand webd such a key.
func TestZeroLengthKeyWouldHaveMadeCookiesForgeable(t *testing.T) {
	// The attacker's signer: no stolen material, just the empty key an
	// unguarded empty Secret decodes to.
	attacker := passthroughlink.New([]byte{},
		passthroughlink.WithIssuer(passthroughlink.IssuerIdentityd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd),
	)
	forged, err := attacker.Mint(passthroughlink.Payload{
		Subject:   "victim",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err, "minting under a zero-length key is arithmetically possible")

	// webd's own cookie signer, constructed exactly as main.go does, from the
	// same empty key — it accepts the forgery.
	victimSide := passthroughlink.New([]byte{},
		passthroughlink.WithIssuer(passthroughlink.IssuerIdentityd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd),
	)
	p, err := victimSide.Verify(forged)
	require.NoError(t, err, "a zero-length key makes the HMAC publicly computable")
	require.Equal(t, "victim", string(p.Subject), "the attacker chose the authenticated subject")

	// That key cannot reach webd, because the loader refuses it.
	_, err = readHexKey(writeKeyFile(t, ""))
	require.Error(t, err, "readHexKey must refuse the key that enables the forgery above")
}
