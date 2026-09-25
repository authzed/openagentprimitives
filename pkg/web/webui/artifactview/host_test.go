package artifactview

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostPage_NestedNoJSArtifactFrame(t *testing.T) {
	body, csp, err := hostPage("TOK123", "https://shell.example", true)
	require.NoError(t, err)
	s := string(body)

	// The inner artifact frame MUST be allow-same-origin with NO allow-scripts —
	// the structural no-JS guarantee (spike-verified). Assert the exact attr.
	require.Contains(t, s, `id="ap-artifact"`)
	assert.Contains(t, s, `sandbox="allow-same-origin"`, "inner artifact frame must be script-disabled")
	assert.NotContains(t, s, `allow-scripts`, "the artifact frame must NEVER get allow-scripts")
	assert.Contains(t, s, `src="/content?ct=TOK123"`, "host self-embeds /content from its own token")

	// The bridge is origin-pinned to the trusted shell and same-origin to the artifact.
	assert.Contains(t, s, `"https://shell.example"`, "bridge must pin the trusted shell origin")
	assert.Contains(t, s, "scrollTo", "host preserves scroll by same-origin access")
	assert.NotContains(t, s, `postMessage({ap:"scroll"`, "no postMessage to the artifact — host reads it directly")

	// The annotation renderer bundle (Plan D2): a nonce'd config global followed
	// by a nonce'd external <script> pointing at the sandbox-origin bundle route.
	assert.Contains(t, s, `window.__AP_ANNOT={shell:"https://shell.example"};`, "config global must carry the sanitized shell origin")
	assert.Contains(t, s, `src="/artifact-host.js"`, "host page must load the annotation bundle from the sandbox-origin route")

	// CSP: nonce'd script, no connect-src, framed only by the shell.
	assert.Contains(t, csp, "script-src 'nonce-", "host script runs under a nonce")
	assert.NotContains(t, csp, "connect-src", "host has no network exit")
	assert.Contains(t, csp, "frame-ancestors https://shell.example", "only the trusted shell may frame the host")

	// The nonce in the CSP must match the nonce on every nonce'd tag: the base
	// <style>, the annotator-chrome <style>, the bridge <script>, the config-global
	// <script>, and the external bundle <script src="/artifact-host.js">.
	cspNonce := between(t, csp, "script-src 'nonce-", "'")
	require.NotEmpty(t, cspNonce)
	assert.Equal(t, 5, strings.Count(s, `nonce="`+cspNonce+`"`), "both <style> tags and all three <script> tags must carry the CSP nonce")

	// style-src's nonce and script-src's nonce must be the identical token, not
	// merely both present — a regression that gave them independent nonces
	// would still pass the count-based assertion above but break the <style>
	// tag (whose nonce attribute would no longer match style-src).
	scriptNonce := between(t, csp, "script-src 'nonce-", "'")
	styleNonce := between(t, csp, "style-src 'nonce-", "'")
	require.NotEmpty(t, styleNonce)
	assert.Equal(t, scriptNonce, styleNonce, "style-src and script-src must carry the SAME nonce")
}

// TestHostPage_AnnotateFalse_OmitsAnnotatorConfigAndBundle is the review-finding
// regression: when the session's class does not grant annotation_batch, the
// host page must NOT ship the annotator config global or bundle script — the
// browser would otherwise render a working-looking annotator whose Send then
// fails closed (403) at /interact. The D1 swap bridge and the inner read-only
// artifact frame must be unaffected — the live-view still works.
func TestHostPage_AnnotateFalse_OmitsAnnotatorConfigAndBundle(t *testing.T) {
	body, csp, err := hostPage("TOK123", "https://shell.example", false)
	require.NoError(t, err)
	s := string(body)

	assert.NotContains(t, s, "__AP_ANNOT", "annotate=false must omit the config global entirely")
	assert.NotContains(t, s, `src="/artifact-host.js"`, "annotate=false must omit the annotator bundle script")

	// The swap bridge (D1) and the inner no-JS artifact frame stay regardless —
	// the read-only live-view (revision swaps, scroll preservation) is not
	// gated by the annotation capability.
	assert.Contains(t, s, `sandbox="allow-same-origin"`, "inner artifact frame must be script-disabled")
	assert.NotContains(t, s, `allow-scripts`, "the artifact frame must NEVER get allow-scripts")
	assert.Contains(t, s, `src="/content?ct=TOK123"`, "host self-embeds /content from its own token")
	assert.Contains(t, s, "scrollTo", "host preserves scroll by same-origin access — unaffected by annotate")

	// CSP is unchanged by annotate.
	assert.Contains(t, csp, "script-src 'nonce-", "host script runs under a nonce")
	assert.Contains(t, csp, "frame-ancestors https://shell.example", "only the trusted shell may frame the host")

	// Now only two nonce'd tags remain: the <style> and the bridge <script>.
	cspNonce := between(t, csp, "script-src 'nonce-", "'")
	require.NotEmpty(t, cspNonce)
	assert.Equal(t, 2, strings.Count(s, `nonce="`+cspNonce+`"`), "only the <style> and bridge <script> carry the CSP nonce when annotate is false")
}

// TestHostPage_UnconfiguredShellOrigin_FrameAncestorsNone is the fail-CLOSED
// regression for the one directive that protects this page from being framed.
//
// shellOrigin is av.TrustedOrigin() — in webd that is an externalurl
// Provider.Get(), which returns "" until its first successful ConfigMap poll.
// The trusted and sandbox URLs are two independent keys with two independent
// pollers, and webui/server.go deliberately keeps serving while only one of
// them has landed, so this page can genuinely be rendered with no shell origin.
// A configured-but-malformed value that sanitizes away reaches the same state.
//
// Registering frame-ancestors with an empty source list emitted
// "frame-ancestors ;" — an EMPTY ancestor-source-list is invalid CSP, so the UA
// discards the whole directive as a parse error and the sandbox-origin page
// that frames agent-generated HTML becomes embeddable by ANY origin. Dropping
// the directive is the identical failure: frame-ancestors does not fall back to
// default-src. The only fail-closed answer is an explicit 'none'.
func TestHostPage_UnconfiguredShellOrigin_FrameAncestorsNone(t *testing.T) {
	cases := []struct {
		name        string
		shellOrigin string
	}{
		{name: "trusted origin not yet polled: frame-ancestors 'none'", shellOrigin: ""},
		{name: "configured origin sanitizes to empty: frame-ancestors 'none'", shellOrigin: `"};alert(1);//`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, csp, err := hostPage("TOK123", tc.shellOrigin, true)
			require.NoError(t, err)

			assert.Contains(t, csp, "frame-ancestors 'none'",
				"an absent shell origin must DENY all framing, never emit a directive the UA discards")
			assert.NotContains(t, csp, "frame-ancestors ;",
				"a valueless frame-ancestors is a CSP parse error — the UA drops the directive entirely")
			assert.Equal(t, 1, strings.Count(csp, "frame-ancestors "),
				"exactly one frame-ancestors directive")
		})
	}
}

// TestHostPage_ShellOriginInjection is adversarial: shellOrigin is normally a
// trusted, configured value, but a regression that dropped sanitization would
// let it inject CSP directives or widen frame-ancestors to another host. Each
// case asserts the sanitized origin is the ONLY thing that lands in
// frame-ancestors, and that the function's own trailing directives
// (base-uri 'none'; form-action 'none') are not preceded by an attacker
// controlled directive/host.
func TestHostPage_ShellOriginInjection(t *testing.T) {
	cases := []struct {
		name       string
		shellOrig  string
		wantOrigin string // exact sanitized value; also the bridge's JS literal
		// wantAncestors is the exact frame-ancestors source list expected. It
		// tracks wantOrigin EXCEPT when sanitization leaves nothing: a valueless
		// source list is invalid CSP (the UA discards the directive, removing
		// framing protection), so the empty case must deny rather than emit it —
		// see TestHostPage_UnconfiguredShellOrigin_FrameAncestorsNone.
		wantAncestors string
	}{
		{
			name:          "semicolon directive injection",
			shellOrig:     "https://legit.example; base-uri *",
			wantOrigin:    "https://legit.example",
			wantAncestors: "https://legit.example",
		},
		{
			name:          "space host-widening injection",
			shellOrig:     "https://legit.example evil.example",
			wantOrigin:    "https://legit.example",
			wantAncestors: "https://legit.example",
		},
		{
			name:          "quote/JS-breakout attempt: sanitizes empty, so framing is DENIED",
			shellOrig:     `"};alert(1);//`,
			wantOrigin:    "",
			wantAncestors: "'none'",
		},
		// Control bytes reach the header value intact unless truncated here:
		// url.Parse REJECTS them, so the sanitizer's parse-and-reconstruct
		// branch is skipped and the raw string would fall through. Go's header
		// writer then rewrites "\n"/"\r" to a SPACE — and a space is exactly
		// what separates two hosts in a frame-ancestors source list, so the
		// widened policy is emitted verbatim to the browser.
		{
			name:          "newline host-widening injection",
			shellOrig:     "https://legit.example\nevil.example",
			wantOrigin:    "https://legit.example",
			wantAncestors: "https://legit.example",
		},
		{
			name:          "CRLF directive injection",
			shellOrig:     "https://legit.example\r\nframe-src *",
			wantOrigin:    "https://legit.example",
			wantAncestors: "https://legit.example",
		},
		{
			name:          "tab host-widening injection",
			shellOrig:     "https://legit.example\tevil.example",
			wantOrigin:    "https://legit.example",
			wantAncestors: "https://legit.example",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, csp, err := hostPage(`"><script>alert(1)</script>`, tc.shellOrig, true)
			require.NoError(t, err)
			s := string(body)

			// The artifact frame must still be script-disabled: no allow-scripts,
			// and the malicious ct must not have produced a live <script> tag.
			assert.NotContains(t, s, "allow-scripts", "the artifact frame must NEVER get allow-scripts")
			assert.NotContains(t, s, "<script>alert(1)</script>", "ct must not break out of its HTML attribute")

			// frame-ancestors must contain exactly the sanitized origin as its
			// only source, immediately followed by the function's own
			// base-uri/form-action directives — proving no extra directive or
			// host was smuggled in ahead of them.
			// This is the precise anti-injection check: frame-ancestors' value is
			// EXACTLY the sanitized origin and is immediately followed by the
			// function's own base-uri/form-action directives. If shellOrigin's
			// ';' or ' ' survived sanitization, an extra directive or host would
			// appear between "frame-ancestors <origin>" and "; base-uri 'none'",
			// and this exact-adjacency match would fail.
			want := "frame-ancestors " + tc.wantAncestors + "; base-uri 'none'; form-action 'none'"
			assert.Contains(t, csp, want, "frame-ancestors must carry only the sanitized origin, with no injected directive")
			assert.Equal(t, 1, strings.Count(csp, "frame-ancestors "), "exactly one frame-ancestors directive — no injected duplicate")
			// No control byte may survive into the header value at all: Go's
			// header writer rewrites "\n"/"\r" to a space, which would widen the
			// source list into a second host after this string was built.
			assert.NotContains(t, csp, "evil.example", "no smuggled second host may reach the CSP")
			assert.Equal(t, -1, strings.IndexFunc(csp, func(r rune) bool { return r < 0x20 || r == 0x7f }),
				"the CSP header value must contain no control byte")

			// The JS string literal for the bridge's trusted-origin constant must
			// be exactly the sanitized origin, safely quoted — never an
			// unescaped break-out of the JS string.
			jsLit := `var T=` + mustJSONString(t, tc.wantOrigin) + `;`
			assert.Contains(t, s, jsLit, "bridge origin literal must be the sanitized origin, safely quoted")
		})
	}
}

// TestHostPage_CtAttributeBreakoutAttempt is adversarial from the other
// injection point: ct lands inside the inner iframe's src="..." attribute
// value. A ct payload that breaks out of that attribute could append its own
// attribute to the SAME <iframe> tag — e.g. sandbox="allow-scripts" —
// which would defeat the structural no-JS guarantee without ever touching
// shellOrigin. html.EscapeString must keep it inert.
func TestHostPage_CtAttributeBreakoutAttempt(t *testing.T) {
	ct := `x" sandbox="allow-scripts`
	body, csp, err := hostPage(ct, "https://shell.example", true)
	require.NoError(t, err)
	s := string(body)

	// ct's quote must be HTML-escaped rather than closing the src="..."
	// attribute early — if it weren't, ct would add a SECOND, live
	// sandbox="allow-scripts" attribute to the <iframe> tag. The escaped
	// text "allow-scripts" is expected to still appear as inert characters
	// inside the src value; what must NOT happen is a second real
	// sandbox="..." attribute on the tag.
	assert.Equal(t, 1, strings.Count(s, `sandbox="`), "ct must not be able to add a second sandbox attribute to the iframe")
	assert.Contains(t, s, `sandbox="allow-same-origin"`, "iframe sandbox attribute must be untouched by ct")
	assert.NotContains(t, s, `<script>alert(1)</script>`, "no unescaped script tag from ct")
	assert.Contains(t, csp, "frame-ancestors https://shell.example; base-uri 'none'; form-action 'none'", "ct must not affect the CSP")
}

// mustJSONString returns the exact JSON string-literal encoding hostPage
// produces for s (matching json.Marshal's escaping), so tests can assert on
// it without duplicating escaping logic.
func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	require.NoError(t, err)
	return string(b)
}

// between returns the substring of s between the first occurrence of a and the
// next occurrence of b after it.
func between(t *testing.T, s, a, b string) string {
	t.Helper()
	i := strings.Index(s, a)
	require.GreaterOrEqual(t, i, 0, "prefix %q not found", a)
	i += len(a)
	j := strings.Index(s[i:], b)
	require.GreaterOrEqual(t, j, 0, "suffix %q not found", b)
	return s[i : i+j]
}
