package secretsource

import (
	"fmt"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// newOTPFunc returns a Credential.OTP closure over raw (an otpauth:// URI
// or a bare base32 seed, as found under a host's .totp key). Parsing raw
// is deliberately deferred to the first call: a malformed .totp value
// must not fail Fetch -- it is Phase C's tool-call boundary that turns a
// call-time OTP() error into a user-visible failure, not Fetch itself.
func newOTPFunc(raw string) func() (string, error) {
	return func() (string, error) {
		return CodeAt(raw, time.Now())
	}
}

// CodeAt computes the one-time code for raw at the instant t. raw is
// either an otpauth:// URI -- whose algorithm/digits/period query
// parameters are honored -- or a bare base32 seed, which defaults to
// SHA1/6-digit/30-second (RFC 6238 / Google-Authenticator compatible).
//
// Exported so tests can assert against a fixed, independently-derived
// vector without sleeping or mocking time.Now: newOTPFunc's closure (what
// Credential.OTP actually is) is a thin time.Now() wrapper around this.
func CodeAt(raw string, t time.Time) (string, error) {
	secret, opts, err := parseTOTPConfig(raw)
	if err != nil {
		return "", err
	}

	code, err := totp.GenerateCodeCustom(secret, t, opts)
	if err != nil {
		// totp/hotp's errors here are static sentinels (e.g. "Decoding of
		// secret as base32 failed.") that never embed the secret itself,
		// so wrapping is safe -- unlike the otpauth-URI parse error below.
		return "", fmt.Errorf("secretsource: generate OTP code: %w", err)
	}
	return code, nil
}

// parseTOTPConfig splits raw into the base32 secret and the
// algorithm/digits/period to generate with.
func parseTOTPConfig(raw string) (string, totp.ValidateOpts, error) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "otpauth://") {
		key, err := otp.NewKeyFromURL(trimmed)
		if err != nil {
			// Do NOT wrap err directly: a url.Parse failure's Error() text
			// embeds the entire input URL -- including the secret query
			// parameter -- verbatim. Name only that parsing failed.
			return "", totp.ValidateOpts{}, fmt.Errorf("secretsource: malformed otpauth URI")
		}
		return normalizeSeed(key.Secret()), totp.ValidateOpts{
			Period:    uint(key.Period()),
			Digits:    key.Digits(),
			Algorithm: key.Algorithm(),
		}, nil
	}

	// Not an otpauth:// URI: treat raw as a bare base32 seed with the
	// defaults every common authenticator app assumes.
	return normalizeSeed(trimmed), totp.ValidateOpts{
		Period:    30,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	}, nil
}

// normalizeSeed uppercases a base32 seed and strips ALL whitespace.
// Operators commonly copy a seed grouped with spaces (an enrollment QR's
// "XXXX XXXX XXXX" display form) or in lowercase; the underlying library
// only trims LEADING/TRAILING whitespace before decoding, so internal
// whitespace and case need handling here.
func normalizeSeed(seed string) string {
	return strings.ToUpper(strings.Join(strings.Fields(seed), ""))
}
