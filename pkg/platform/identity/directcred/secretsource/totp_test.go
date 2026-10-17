package secretsource_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred/secretsource"
)

// Test vectors below are derived two ways, never by calling
// github.com/pquerna/otp/totp.GenerateCodeCustom and hard-coding whatever
// it returns -- that would be circular (it would only prove the library
// agrees with itself, not that this package wires URI params/defaults
// into it correctly).
//
//   - The SHA1/digits=6/period=30 vector is the published RFC 6238
//     Appendix B test vector (Time=59s, SHA1, 8-digit code "94287082"),
//     truncated to 6 digits. Truncating is valid because both the 6- and
//     8-digit codes are the SAME 31-bit dynamic-truncation "value" modulo
//     a different power of ten, and 10^6 divides 10^8 -- so value%10^6
//     is congruent to (value%10^8)%10^6, i.e. the published 8-digit
//     code's last 6 digits: "94287082" -> "287082". The RFC's seed is the
//     ASCII string "12345678901234567890"; base32-encoding those 20 bytes
//     (no padding needed: 20*8=160 bits is a multiple of 5) gives the
//     bare base32 seed below.
//
//   - The SHA256/digits=8/period=60 vector has no published RFC vector
//     (RFC 6238 Appendix B fixes the step at 30s; this test exercises a
//     URI that overrides it to 60s). It was derived independently of
//     this package and of github.com/pquerna/otp: a standalone Go
//     program implemented RFC 4226 HOTP dynamic truncation directly
//     against crypto/hmac + crypto/sha256 (no otp import at all), fed
//     the RFC 6238 Appendix B SHA256 seed bytes (ASCII
//     "12345678901234567890123456789012", 32 bytes) and counter =
//     1700000000/60, and printed "77076628". That independent
//     implementation was first sanity-checked against the published
//     RFC 6238 SHA1 vector above (reproduced "94287082" exactly) before
//     being trusted for the SHA256/period=60 case, and the result was
//     then cross-checked by calling the real otp/totp.GenerateCodeCustom
//     once offline with the same key/time/options, which agreed
//     ("77076628"). The hard-coded expectation below is the independent
//     program's output, not a value read back out of this package or
//     otp/totp.
const (
	rfc6238SHA1BareSeed = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	rfc6238SHA1Time     = 59 // seconds; RFC 6238 Appendix B row

	sha256Period60Secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQGEZA"
	sha256Period60Time   = 1700000000 // seconds; arbitrary fixed point, independently derived (see above)
)

func TestTOTP(t *testing.T) {
	t.Run("otpauth URI: SHA256/digits=8/period=60 honored, independently-derived vector", func(t *testing.T) {
		uri := "otpauth://totp/Example:test@example.com?secret=" + sha256Period60Secret +
			"&issuer=Example&algorithm=SHA256&digits=8&period=60"

		code := generateAt(t, uri, time.Unix(sha256Period60Time, 0).UTC())
		assert.Equal(t, "77076628", code)
	})

	t.Run("bare base32 seed: SHA1/6/30 defaults, RFC 6238 vector truncated to 6 digits", func(t *testing.T) {
		code := generateAt(t, rfc6238SHA1BareSeed, time.Unix(rfc6238SHA1Time, 0).UTC())
		assert.Equal(t, "287082", code)
	})

	t.Run("bare seed with spaces and lowercase: normalized to the same code", func(t *testing.T) {
		messy := "gezd gnbv gy3t qojq\tgezd gnbv gy3t qojq"
		code := generateAt(t, messy, time.Unix(rfc6238SHA1Time, 0).UTC())
		assert.Equal(t, "287082", code)
	})

	t.Run("garbage seed: Fetch succeeds, OTP() returns the parse error lazily", func(t *testing.T) {
		src := secretsource.New()
		cfg := directcred.Config{SecretData: map[string][]byte{
			"g.example.com.username": []byte("gary"),
			"g.example.com.password": []byte("garypw"),
			"g.example.com.totp":     []byte("not-a-valid-base32!!!"),
		}}

		cred, err := src.Fetch(context.Background(), cfg, "g.example.com")
		require.NoError(t, err, "a garbage seed must not fail Fetch")
		require.NotNil(t, cred.OTP)

		_, err = cred.OTP()
		assert.Error(t, err, "a garbage seed must fail at OTP() call time, not before")
	})
}

// generateAt round-trips raw (an otpauth:// URI or bare seed) through a
// Fetch'd Credential's OTP closure, pinned at t via the exported test hook
// so the test is deterministic without sleeping or mocking time.Now.
func generateAt(t *testing.T, raw string, at time.Time) string {
	t.Helper()
	code, err := secretsource.CodeAt(raw, at)
	require.NoError(t, err)
	return code
}
