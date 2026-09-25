package toolcall

import (
	"math"
	"regexp"
	"strings"
)

// secretLikeKey matches env-var names that smell like secrets. Hard-rejected
// by the ToolCall validator. No allowlist override — if a user has a
// legitimate config key with one of these substrings, they rename it.
var secretLikeKey = regexp.MustCompile(`(?i)(token|secret|password|passwd|credential|api[_-]?key|private[_-]?key|auth[_-]?key|access[_-]?key|client[_-]?secret|bearer|oauth|jwt|cookie|passphrase|pin)`)

// envName is a valid POSIX env-var name.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// IsSecretLikeKey reports whether the env-var key matches the secret-name
// pattern. Used to hard-reject ToolCall.spec.env keys that look like secrets.
func IsSecretLikeKey(k string) bool {
	return secretLikeKey.MatchString(k)
}

// IsValidEnvName reports whether the key is a valid POSIX env-var name.
func IsValidEnvName(k string) bool {
	return envName.MatchString(k)
}

// IsHighEntropyValue reports whether v is "long enough" + "high entropy" + not
// all-hex. A heuristic for catching tokens that slipped through the name
// blacklist. False positives are tolerated (warn-only, not fail).
//
// Threshold: len(stripped) >= 16, Shannon entropy >= 4.5 bits/char, and the
// value is not entirely [0-9a-fA-F] (which catches UUIDs and hex hashes).
func IsHighEntropyValue(v string) bool {
	stripped := strings.ReplaceAll(v, "\n", "")
	stripped = strings.ReplaceAll(stripped, "\r", "")
	if len(stripped) < 16 {
		return false
	}
	if isAllHex(stripped) {
		return false
	}
	return shannon(stripped) >= 4.5
}

func isAllHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// shannon returns the Shannon entropy of s in bits/char.
func shannon(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
