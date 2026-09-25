package webui

import (
	"net/url"
	"strings"
)

// SanitizeOrigin reduces raw to a bare "scheme://host" origin free of the
// characters that would let it break out of a CSP directive token
// (frame-ancestors/frame-src) or a JS string literal it's paired with:
// whitespace, ';', quotes, angle brackets, and every ASCII control byte. A
// well-formed origin contains none of them, so:
//
//  1. Truncate at the first such character. That alone collapses
//     directive-injection ("https://legit.example; base-uri *") and
//     host-widening ("https://legit.example evil.example") payloads back to
//     their clean prefix.
//
//     The control bytes are NOT redundant with step 2's url.Parse: net/url
//     REJECTS any ASCII control byte outright, so a raw string carrying one
//     skips the reconstruct branch and falls through to step 3 unchanged. Go's
//     header writer then rewrites "\n"/"\r" to a SPACE — exactly what separates
//     two hosts in a CSP source list — so "https://ok.example\nevil.example"
//     would emit "frame-src https://ok.example evil.example", reintroducing
//     downstream the host-widening this function exists to stop. A tab is a
//     source-list separator already, needing no rewrite.
//
//  2. If what remains parses as an http(s) URL with a host, reconstruct it as
//     exactly scheme://host — dropping path, query, and fragment so the result
//     is a true origin, not an arbitrary URL.
//
//  3. Otherwise return the truncated string as-is: already free of the
//     dangerous characters, though possibly not a usable origin — a malformed
//     configured origin is a misconfiguration, not an injection.
//
// Shared by every sandbox-origin host page that stamps a configured origin into
// its own CSP + bridge script (artifactview/host.go, sessionview/widgets.go).
// Step 3 is what makes this the WRONG function for a value the browser will
// navigate to or frame: see StrictOrigin.
func SanitizeOrigin(raw string) string {
	raw = truncateAtBreakout(raw)
	if origin := parseHTTPOrigin(raw); origin != "" {
		return origin
	}
	return raw
}

// StrictOrigin is SanitizeOrigin without step 3: the bare "scheme://host"
// origin, or "" when raw is not an http(s) origin at all.
//
// Use this — not SanitizeOrigin — wherever the value becomes a URL the browser
// NAVIGATES to or frames. SanitizeOrigin's lenient fallback is safe for a CSP
// source or postMessage target (a malformed one simply never matches or never
// delivers), but for a navigable URL it would hand back "javascript:alert(1)"
// verbatim — every character of it is outside the blocked set — to be assigned
// to an iframe src.
//
// "" is likewise a real input, not just a malformed one: webd's external-URL
// ConfigMap is seeded EMPTY whenever `oap install` manages external access, and
// webd keeps serving until the value lands. Concatenating an empty base yields
// a shell-RELATIVE URL, loading sandbox content in the trusted origin — the
// same-origin collapse the two-origin split exists to prevent. Returning ""
// lets the caller fail closed on both cases with one check.
func StrictOrigin(raw string) string {
	return parseHTTPOrigin(truncateAtBreakout(raw))
}

// truncateAtBreakout implements step 1: cut at the first character that could
// break out of a CSP directive token or a JS string literal.
func truncateAtBreakout(raw string) string {
	if i := strings.IndexFunc(raw, isOriginBreakout); i >= 0 {
		return raw[:i]
	}
	return raw
}

// parseHTTPOrigin implements step 2: reconstruct raw as exactly scheme://host
// when it parses as an http(s) URL with a host, else "". Shared so the strict
// and lenient entry points can never disagree about what counts as an origin.
func parseHTTPOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// SanitizeCSPSourceToken validates a single CSP source-list token — a
// widget-declared connect-src/img-src/script-src/style-src/frame-src entry
// (WidgetCSPMeta in sessionview/widgets.go) — which is MCP-server-authored and
// therefore untrusted.
//
// Unlike SanitizeOrigin it does NOT truncate-and-reconstruct via url.Parse: a
// valid token can be a wildcard host source like "https://*.cdn.example.com",
// which is not a parseable origin and would be silently mangled or dropped by
// the scheme://host reconstruction. Widgets plausibly need wildcard subdomain
// sources, so this sibling only rejects tokens carrying a character that breaks
// out of the directive they're joined into: whitespace widens a token into a
// second host — or, since the caller space-joins tokens, injects what looks like
// a second directive keyword; ';' splices in a literal new directive; quotes and
// angle brackets are JS-string/HTML breakout characters this value may also
// travel through (document.go's bootstrap embedding).
//
// Returns (raw, true) when raw is clean and non-empty; ("", false) when raw is
// empty or carries a blocked character, which buildWidgetCSP drops rather than
// joins.
func SanitizeCSPSourceToken(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	if strings.IndexFunc(raw, isOriginBreakout) >= 0 {
		return "", false
	}
	return raw, true
}

// isOriginBreakout reports whether r may not appear in a value destined for a
// CSP directive token or a JS string literal — one predicate on purpose, so a
// character blocked for SanitizeOrigin can never be allowed by
// SanitizeCSPSourceToken. Separate literal character sets drift, and an origin
// that permits tab/CR/LF widens a frame-src/frame-ancestors source list into a
// second host.
//
//   - space and tab separate sources within one CSP directive value, so
//     either widens a single-host directive into two.
//   - CR and LF are rewritten to a space by Go's header writer, arriving at
//     the browser as that same widening; they are also classic header-splitting
//     bytes.
//   - ';' terminates a CSP directive, splicing in a literal new one.
//   - quotes and angle brackets are JS-string / HTML breakout characters,
//     since these values also travel through bootstrap script embedding.
//   - every other ASCII control byte (and DEL) has no legitimate place in an
//     origin or source token, and net/url rejects them anyway — blocking them
//     here keeps the truncation, not the parse, the thing that guarantees it.
func isOriginBreakout(r rune) bool {
	switch r {
	case ' ', ';', '\'', '"', '<', '>':
		return true
	}
	return r < 0x20 || r == 0x7f
}
