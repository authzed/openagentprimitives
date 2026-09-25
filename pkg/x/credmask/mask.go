// Package credmask masks credential material for operator-visible output —
// ToolCall.status.agent.injected[*].masked and the setup engine's "stored as
// <masked>" line.
//
// Both are defence-in-depth, not security boundaries: anyone who can read a
// ToolCall can already read the same namespace's Secrets. The goal is to
// reduce accidental exposure in log aggregators and screen recordings while
// keeping values identifiable — an operator must be able to tell the injected
// credential is the GitHub PAT they intended and not something else, which a
// fixed **** cannot.
//
// What is shown is the vendor prefix, never the raw first four bytes: vendors
// deliberately end token prefixes with a delimiter (_ or -) so secret scanners
// can recognise token families, making the prefix already public. Tokens with
// no recognisable prefix get ****+last4, which echoes no leading secret bytes.
//
// Residual case: an all-lowercase secret carrying '_' or '-' within its first
// 12 chars (a snake_case passphrase) has that run treated as a vendor prefix
// and shown. Deliberate — the ≥8-hidden-chars floor bounds the exposure, and
// tightening the rule would cost identifiability for real vendor prefixes.
package credmask

// Mask returns a redacted form of v: vendor prefix + "****" + last 4, falling
// back to "****"+last4 when no vendor prefix is recognised, and to a bare
// "****" for values under 12 chars or containing non-printable-ASCII. See the
// package doc for why the prefix is safe to show.
func Mask(v string) string {
	stripped := stripNewlines(v)

	// Bytes ≥ 0x80 are rejected alongside control characters because the final
	// byte-slice (stripped[len-4:]) must never split a multi-byte UTF-8 rune and
	// emit invalid UTF-8 into a status field. Real credentials are ASCII.
	for i := 0; i < len(stripped); i++ {
		c := stripped[i]
		if c < 0x20 || c >= 0x7f {
			return "****"
		}
	}

	// Too short to show anything useful without exposing most of it.
	if len(stripped) < 12 {
		return "****"
	}

	last4 := stripped[len(stripped)-4:]

	// The length floor above guarantees index 11 exists, so scanning 11 down to
	// 0 for the delimiter needs no bounds clamp.
	delimIdx := -1
	for i := 11; i >= 0; i-- {
		if stripped[i] == '_' || stripped[i] == '-' {
			delimIdx = i
			break
		}
	}

	if delimIdx >= 0 {
		candidate := stripped[:delimIdx+1]
		if isLowercaseVendorPrefix(candidate) {
			// Enough secret must stay hidden once last4 is also shown.
			remainder := len(stripped) - len(candidate)
			if remainder >= 8 {
				return candidate + "****" + last4
			}
		}
	}

	return "****" + last4
}

// stripNewlines removes \r and \n characters from s.
func stripNewlines(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\r' && c != '\n' {
			out = append(out, c)
		}
	}
	return string(out)
}

// isLowercaseVendorPrefix reports whether every byte of s is in [a-z0-9_-].
// Vendor prefixes are lowercase by convention while random base62 token
// material is ~50% uppercase, so this rejects most accidental delimiters
// falling inside real secret bytes.
func isLowercaseVendorPrefix(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
