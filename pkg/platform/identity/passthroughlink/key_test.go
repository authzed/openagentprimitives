package passthroughlink_test

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// TestDecodeHexKey pins the shared loader contract every reader of the
// spicebox-passthrough-link-key Secret depends on. The empty case is the one
// that matters most: hex.DecodeString("") returns ([]byte{}, nil), so a reader
// that decodes without a length check silently accepts a zero-length HMAC key
// — which anyone can compute signatures under.
func TestDecodeHexKey(t *testing.T) {
	full := hex.EncodeToString([]byte(strings.Repeat("a", passthroughlink.MinKeyLen)))
	cases := []struct {
		name    string
		raw     string
		wantErr string // substring the error must mention; "" means expect success
	}{
		{
			name:    "empty input: rejected rather than decoding to a zero-length key",
			raw:     "",
			wantErr: "empty",
		},
		{
			name:    "whitespace-only input: rejected, trimming must not yield a zero-length key",
			raw:     " \n\t",
			wantErr: "empty",
		},
		{
			name:    "one byte: rejected as below the floor",
			raw:     "ff",
			wantErr: "too short",
		},
		{
			name:    "one byte under the floor: rejected, the boundary is not off by one",
			raw:     hex.EncodeToString([]byte(strings.Repeat("a", passthroughlink.MinKeyLen-1))),
			wantErr: "too short",
		},
		{
			name:    "odd-length hex: rejected as not valid hex",
			raw:     "abc",
			wantErr: "not valid hex",
		},
		{
			name:    "non-hex characters: rejected as not valid hex",
			raw:     "zzzzzzzz",
			wantErr: "not valid hex",
		},
		{
			name: "exactly the floor: accepted, the size oap install generates",
			raw:  full,
		},
		{
			name: "floor with surrounding whitespace: accepted, Secret mounts add newlines",
			raw:  "\n" + full + "\n",
		},
		{
			name: "above the floor: accepted, a longer key is stronger not weaker",
			raw:  hex.EncodeToString([]byte(strings.Repeat("a", passthroughlink.MinKeyLen*2))),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := passthroughlink.DecodeHexKey([]byte(tc.raw))
			if tc.wantErr == "" {
				require.NoError(t, err)
				assert.GreaterOrEqual(t, len(got), passthroughlink.MinKeyLen,
					"an accepted key must meet the floor it was checked against")
				return
			}
			require.Error(t, err, "unsafe key material must be refused")
			assert.Contains(t, err.Error(), tc.wantErr, "error should name why the key was refused")
			assert.Nil(t, got, "no key bytes may be returned alongside an error")
		})
	}
}

// TestDecodeHexKeyErrorsCarryNoKeyMaterial pins that a rejection message never
// echoes any part of the key it rejected.
//
// The hex decoder's own error is the trap: hex.InvalidByteError stringifies as
// "encoding/hex: invalid byte: U+0041 'A'", so wrapping it would print one
// character of the signing key into every log that records the failure.
func TestDecodeHexKeyErrorsCarryNoKeyMaterial(t *testing.T) {
	// Each input is malformed, and every character in it is distinctive enough
	// that its presence in the error would be unambiguous.
	cases := []struct {
		name string
		raw  string
	}{
		{name: "invalid byte mid-key: offending character withheld", raw: strings.Repeat("a", 40) + "QQ" + strings.Repeat("b", 22)},
		{name: "wholly non-hex key: no character echoed", raw: "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"},
		{name: "odd-length key: no character echoed", raw: strings.Repeat("c", 65)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := passthroughlink.DecodeHexKey([]byte(tc.raw))
			require.Error(t, err, "malformed key material must be refused")

			msg := err.Error()
			for _, ch := range []string{"Q", "Z", "U+"} {
				if strings.Contains(tc.raw, ch) || ch == "U+" {
					assert.NotContains(t, msg, ch,
						"the rejection message must not echo key material or its position")
				}
			}
			assert.Contains(t, msg, "not valid hex", "the error must still say why it was refused")
		})
	}
}

// TestDecodeHexKeyRoundTripsInstallGeneratedKey proves the floor admits exactly
// what `oap install` writes: hex.EncodeToString of 32 random bytes.
func TestDecodeHexKeyRoundTripsInstallGeneratedKey(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	got, err := passthroughlink.DecodeHexKey([]byte(hex.EncodeToString(raw)))
	require.NoError(t, err, "the install-generated key shape must load")
	assert.Equal(t, raw, got, "decoding must recover the exact key bytes")
}
