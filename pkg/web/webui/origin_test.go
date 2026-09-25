package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSanitizeOrigin covers the injection-defeat contract SanitizeOrigin
// promises: truncate at the first dangerous character, then normalize to a
// bare scheme://host when what remains parses as one. Every sandbox-origin
// host page that stamps a configured origin into its own CSP or a JS string
// literal (artifactview's /artifact-host, sessionview's /mcpui-host) depends
// on this — see their own adversarial tests (host_test.go's
// TestHostPage_ShellOriginInjection) for the end-to-end proof.
func TestSanitizeOrigin(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "well-formed https origin: unchanged", in: "https://legit.example", want: "https://legit.example"},
		{name: "well-formed http origin: unchanged", in: "http://legit.example:8080", want: "http://legit.example:8080"},
		{name: "path/query/fragment stripped", in: "https://legit.example/a/b?x=1#y", want: "https://legit.example"},
		{name: "semicolon directive injection: truncated", in: "https://legit.example; base-uri *", want: "https://legit.example"},
		{name: "space host-widening injection: truncated", in: "https://legit.example evil.example", want: "https://legit.example"},
		{name: "quote/JS-breakout attempt: truncated to unparseable prefix", in: `"};alert(1);//`, want: ""},
		{name: "angle bracket breakout: truncated", in: "https://legit.example<script>", want: "https://legit.example"},
		{name: "empty input: empty output", in: "", want: ""},
		{name: "malformed (no scheme): returned as-is once clean", in: "not-a-url", want: "not-a-url"},
		// Control bytes are the case url.Parse cannot save us from: it REJECTS
		// any ASCII control byte, so the reconstruct branch is skipped and the
		// raw string would fall through unchanged. Go's header writer then
		// rewrites "\n"/"\r" to a space, turning one host into two in the
		// frame-src/frame-ancestors directive this value is joined into — the
		// exact host-widening truncation exists to stop. A tab is a CSP
		// source-list separator in its own right and needs no rewriting at all.
		{name: "newline host-widening: truncated", in: "https://legit.example\nevil.example", want: "https://legit.example"},
		{name: "carriage return host-widening: truncated", in: "https://legit.example\revil.example", want: "https://legit.example"},
		{name: "tab host-widening: truncated", in: "https://legit.example\tevil.example", want: "https://legit.example"},
		{name: "CRLF directive injection: truncated", in: "https://legit.example\r\nframe-ancestors *", want: "https://legit.example"},
		{name: "other control byte (NUL): truncated", in: "https://legit.example\x00evil.example", want: "https://legit.example"},
		{name: "DEL: truncated", in: "https://legit.example\x7fevil.example", want: "https://legit.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, SanitizeOrigin(tc.in))
		})
	}
}

// TestStrictOrigin covers the fail-closed sibling: same truncation and same
// scheme://host reconstruction as SanitizeOrigin, but "" instead of a verbatim
// passthrough when the value is not an http(s) origin.
//
// The rows that matter are the ones where the two functions DIVERGE — they are
// exactly the values that must never reach a navigable sink. SanitizeOrigin
// hands "javascript:alert(1)" back unchanged (no character in it is in the
// blocked set and url.Parse gives it no host), which is harmless in a CSP
// source list and fatal on an iframe src. See artifactview's urlsFor.
func TestStrictOrigin(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "well-formed https origin: unchanged", in: "https://legit.example", want: "https://legit.example"},
		{name: "well-formed http origin with port: unchanged", in: "http://legit.example:8080", want: "http://legit.example:8080"},
		{name: "path/query/fragment stripped", in: "https://legit.example/a/b?x=1#y", want: "https://legit.example"},
		{name: "semicolon directive injection: truncated to the clean origin", in: "https://legit.example; base-uri *", want: "https://legit.example"},
		{name: "space host-widening injection: truncated to the clean origin", in: "https://legit.example evil.example", want: "https://legit.example"},
		// Divergence from SanitizeOrigin starts here: each of these returns the
		// input verbatim there, and "" here.
		{name: "javascript: URL: refused (SanitizeOrigin returns it verbatim)", in: "javascript:alert(1)", want: ""},
		{name: "data: URL: refused", in: "data:text/html,<x>", want: ""},
		{name: "file: URL: refused", in: "file:///etc/passwd", want: ""},
		{name: "scheme-less host: refused", in: "legit.example", want: ""},
		{name: "scheme with no host: refused", in: "https://", want: ""},
		{name: "empty input: refused", in: "", want: ""},
		{name: "relative path: refused", in: "/artifact-host", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, StrictOrigin(tc.in))
		})
	}
}

// TestSanitizeCSPSourceToken covers the sibling contract to SanitizeOrigin:
// a CSP source-list token (used for widget-declared connect-src/img-src/
// frame-src entries) rejects any token carrying a directive-injection or
// JS-breakout character, but — unlike SanitizeOrigin — does NOT url.Parse
// and reconstruct the token, so a wildcard subdomain source like
// "https://*.cdn.example.test" (a valid CSP source-list token that isn't a
// parseable origin) survives unchanged. See buildWidgetCSP in
// pkg/web/webui/sessionview/widgets.go, which maps each widget-declared domain
// through this before joining into a directive value.
func TestSanitizeCSPSourceToken(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{name: "clean https origin: kept unchanged", in: "https://api.example.test", want: "https://api.example.test", wantOK: true},
		{name: "wildcard subdomain source: survives (not url.Parse'd)", in: "https://*.cdn.example.test", want: "https://*.cdn.example.test", wantOK: true},
		{name: "semicolon directive injection: dropped", in: "https://evil.test; script-src *", wantOK: false},
		{name: "embedded space: dropped", in: "a b", wantOK: false},
		{name: "empty input: dropped", in: "", wantOK: false},
		{name: "angle bracket breakout: dropped", in: "https://evil.test<script>", wantOK: false},
		{name: "single quote breakout: dropped", in: "https://evil.test'onload", wantOK: false},
		{name: "double quote breakout: dropped", in: `https://evil.test"onload`, wantOK: false},
		{name: "embedded newline: dropped", in: "https://evil.test\nscript-src *", wantOK: false},
		{name: "embedded carriage return: dropped", in: "https://evil.test\rscript-src *", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := SanitizeCSPSourceToken(tc.in)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

// TestTrustedOriginMatch pins the CSRF pin every browser-only mutating route
// shares. The row that matters most is the blank trusted origin: written
// inline as `r.Header.Get("Origin") != trusted`, an unset trusted URL makes
// `"" != ""` false and the guard silently passes for every request that sends
// no Origin — a CSRF check that becomes a no-op precisely when the deployment
// is misconfigured. Four call sites had that shape before this helper existed.
func TestTrustedOriginMatch(t *testing.T) {
	cases := []struct {
		name    string
		origin  string // the request's Origin header; "" means absent
		trusted string
		want    bool
	}{
		{name: "exact match: allowed", origin: "https://ui.example", trusted: "https://ui.example", want: true},
		{name: "trailing slash on the trusted value is trimmed: allowed",
			origin: "https://ui.example", trusted: "https://ui.example/", want: true},
		{name: "a different origin: refused", origin: "https://evil.example", trusted: "https://ui.example", want: false},
		{name: "no Origin header at all: refused (these routes are browser-only)",
			origin: "", trusted: "https://ui.example", want: false},
		{name: "blank trusted origin with a matching-looking absent Origin: REFUSED, not allowed",
			origin: "", trusted: "", want: false},
		{name: "blank trusted origin with a real Origin: refused",
			origin: "https://ui.example", trusted: "", want: false},
		{name: "blank trusted origin that is only a slash: refused",
			origin: "", trusted: "/", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/anything", nil)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			assert.Equal(t, tc.want, TrustedOriginMatch(r, tc.trusted))
		})
	}
}
