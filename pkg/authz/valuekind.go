package authz

import (
	"net/url"
	"strings"
)

// ValueKind is the shape of the values a slot holds — what a seeder must look
// for in a thread to find candidates for it.
//
// DERIVED from the slot's transform chain rather than declared. A canonicalising
// transform exists precisely because the values have a known shape, so
// normalize_url in the chain IS the statement "this slot holds URLs". Deriving
// it keeps one source of truth, the same reason the chain itself is derived from
// the tools rather than restated on the slot; a declared kind could disagree
// with the transforms and the disagreement would be silent.
type ValueKind string

const (
	// ValueKindUnknown means nothing can be extracted for this slot. A slot
	// whose values have no recognised shape is simply not thread-seedable —
	// fail closed, rather than guess at what its ids look like and grant.
	ValueKindUnknown ValueKind = ""
	// ValueKindURL marks a slot whose values are http(s) URLs.
	ValueKindURL ValueKind = "url"
)

// valueKindMarkers maps a transform to the value shape its presence implies.
// A new canonicalising transform (normalize_email, …) makes its slots seedable
// by adding a row here, not by touching any consumer.
var valueKindMarkers = map[string]ValueKind{
	"normalize_url": ValueKindURL,
}

// ValueKindOf derives the shape of a slot's values from its transform chain.
// Returns ValueKindUnknown when nothing in the chain identifies a shape.
func ValueKindOf(transforms []string) ValueKind {
	for _, t := range transforms {
		if k, ok := valueKindMarkers[t]; ok {
			return k
		}
	}
	return ValueKindUnknown
}

// ExtractCandidates finds the raw values of the given kind in a piece of text.
//
// Deliberately conservative. Every value this returns can become a GRANT, so
// over-extraction is over-granting: a string that merely looks URL-ish becoming
// a reachable target is the failure mode, and it is silent. Under-extraction
// costs an approval prompt, which is recoverable.
//
// Returns values in first-seen order, deduplicated.
func ExtractCandidates(kind ValueKind, text string) []string {
	if kind != ValueKindURL || text == "" {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	for _, raw := range scanURLs(text) {
		if _, dup := seen[raw]; dup {
			continue
		}
		seen[raw] = struct{}{}
		out = append(out, raw)
	}
	return out
}

// urlDelimiters end a URL token. The angle bracket and pipe are here because
// chat transports wrap links (Slack writes <https://x.example|label>); stopping
// at them is plain tokenisation, not knowledge of any one transport, so this
// stays out of the channel kinds.
const urlDelimiters = " \t\n\r\"'`<>|(){}[]\\"

// urlTrailing is punctuation that commonly ends a sentence rather than a URL.
const urlTrailing = ".,;:!?"

// scanURLs pulls http(s) URLs out of free text.
//
// Scheme matching is case-INSENSITIVE. A scheme is case-insensitive per RFC
// 3986 and normalize_url lowercases it, so matching only the lowercase spelling
// would make "HTTPS://x" un-seedable while the very same URL typed in lowercase
// seeded fine. That direction is a fail-closed inconsistency rather than a hole
// — the value just finds no grant and routes to an approval — but it is still
// wrong, and a caller cannot tell why it happened.
func scanURLs(text string) []string {
	// Lower a single copy for searching. asciiLower preserves byte length, so
	// offsets into it index the ORIGINAL text unchanged — the value returned is
	// the author's own spelling, which normalize_url then canonicalises.
	lowered := asciiLower(text)
	var out []string
	for i := 0; i < len(text); {
		idx := indexScheme(lowered[i:])
		if idx < 0 {
			break
		}
		start := i + idx
		end := start
		for end < len(text) && !strings.ContainsRune(urlDelimiters, rune(text[end])) {
			end++
		}
		tok := strings.TrimRight(text[start:end], urlTrailing)
		if isAddressableURL(tok) {
			out = append(out, tok)
		}
		i = end
		if i == start {
			i++ // no progress; avoid spinning on a degenerate match
		}
	}
	return out
}

// indexScheme returns the offset of the next http:// or https:// in s, which
// the caller has already lower-cased.
func indexScheme(s string) int {
	h := strings.Index(s, "http://")
	s2 := strings.Index(s, "https://")
	switch {
	case h < 0:
		return s2
	case s2 < 0:
		return h
	case h < s2:
		return h
	default:
		return s2
	}
}

// isAddressableURL keeps only what actually names a host to reach. A bare
// "https://" or a scheme with no authority grants nothing and cannot be revoked
// by id afterwards.
func isAddressableURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}
