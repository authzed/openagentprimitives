package resourcedisplay

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Table over link eligibility. https only — no javascript:, data:, file:, and
// explicitly no http either: the design admits exactly one scheme.
func TestEligibleHref(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "https is eligible", raw: "https://github.com/demo-org/demo-repo", want: "https://github.com/demo-org/demo-repo"},
		{name: "http is NOT eligible: https only", raw: "http://github.com/demo-org/demo-repo", want: ""},
		{name: "javascript: is never eligible", raw: "javascript:alert(1)", want: ""},
		{name: "data: is never eligible", raw: "data:text/html,hi", want: ""},
		{name: "file: is never eligible", raw: "file:///etc/passwd", want: ""},
		{name: "a non-URL value is not eligible", raw: "not a url", want: ""},
		{name: "an https URL with no host is not eligible", raw: "https:///no-host", want: ""},
		{name: "empty is not eligible", raw: "", want: ""},
		{
			name: "a lookalike host is STILL eligible — it derives to the raw value, not blocked",
			raw:  "https://github.co/demo-org/demo-repo",
			want: "https://github.co/demo-org/demo-repo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, EligibleHref(tc.raw))
		})
	}
}

// The href, whenever non-empty, MUST be byte-identical to the raw value it
// was derived from — the text == href invariant made structural rather than
// merely documented: EligibleHref never transforms its input, only accepts
// or rejects it, so there is no code path that could return a different
// string than what will be displayed.
func TestEligibleHref_NeverTransformsItsInput(t *testing.T) {
	raw := "https://github.com/demo-org/demo-repo?ref=main#L10"
	assert.Equal(t, raw, EligibleHref(raw), "an eligible href is returned byte-identical to the raw value")
}

// The decoder registry is CLOSED: a name nobody registered derives nothing
// at all, so a declaration elsewhere can select a presentation but never
// invent one. Both halves must be empty — a title with no value behind it is
// a guess.
func TestObjectIDDecoder_Decode(t *testing.T) {
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	cases := []struct {
		name      string
		decoder   ObjectIDDecoder
		raw       string
		wantTitle string
		wantHref  string
	}{
		{
			name:      "b64url: an encoded https repo URL names its path and links the URL",
			decoder:   DecoderB64URL,
			raw:       b64("https://github.com/demo-org/widgets"),
			wantTitle: "demo-org/widgets",
			wantHref:  "https://github.com/demo-org/widgets",
		},
		{
			name:      "b64url: an http URL is nameable but NOT linkable",
			decoder:   DecoderB64URL,
			raw:       b64("http://ghe.internal.example/demo-org/widgets"),
			wantTitle: "demo-org/widgets",
			wantHref:  "",
		},
		{
			name:      "b64url: an id that is not base64 derives nothing",
			decoder:   DecoderB64URL,
			raw:       "!!!not-base64!!!",
			wantTitle: "",
			wantHref:  "",
		},
		{
			name:      "b64url: decoded-but-pathless derives nothing, never a bare host as a label",
			decoder:   DecoderB64URL,
			raw:       b64("https://github.com"),
			wantTitle: "",
			wantHref:  "",
		},
		{
			name:      "b64url: decoded-but-not-a-URL derives nothing",
			decoder:   DecoderB64URL,
			raw:       b64("just some bytes"),
			wantTitle: "",
			wantHref:  "",
		},
		{
			name:      "an unrecognized decoder name derives nothing",
			decoder:   ObjectIDDecoder("regex_magic"),
			raw:       b64("https://github.com/demo-org/widgets"),
			wantTitle: "",
			wantHref:  "",
		},
		{
			name:      "the empty decoder — a kind that declared none — derives nothing",
			decoder:   "",
			raw:       b64("https://github.com/demo-org/widgets"),
			wantTitle: "",
			wantHref:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, href := tc.decoder.Decode(tc.raw)
			assert.Equal(t, tc.wantTitle, title)
			assert.Equal(t, tc.wantHref, href)
		})
	}
}

// A decoded id can carry a javascript: payload just as easily as a URL, and
// the title is rendered as text while the href is rendered as a real link —
// so the scheme guard has to survive the decode step, not only the raw one.
func TestObjectIDDecoder_Decode_NeverLinksAHostileScheme(t *testing.T) {
	raw := base64.RawURLEncoding.EncodeToString([]byte("javascript:alert(1)"))
	title, href := DecoderB64URL.Decode(raw)
	assert.Empty(t, href, "a non-https decoded value must never become a link target")
	assert.Empty(t, title, "and it has no path to name either")
}

// DeriveB64URLPath and the b64url decoder's TITLE must agree: they are the
// same derivation, and plangate's approval card and admind's Scopes panel
// showing two different names for one repository is the drift this shared
// package exists to prevent.
func TestDeriveB64URLPath_AgreesWithTheDecoderTitle(t *testing.T) {
	raw := base64.RawURLEncoding.EncodeToString([]byte("https://github.com/demo-org/widgets"))
	title, _ := DecoderB64URL.Decode(raw)
	assert.Equal(t, DeriveB64URLPath(raw), title)
	assert.Equal(t, "demo-org/widgets", title)
}

// The three path derivers, kept here because plangate's label registry now
// dispatches into them: a non-URL value derives nothing from the URL-shaped
// two, and the segment deriver works on bare tokens by design.
func TestPathDerivers(t *testing.T) {
	assert.Equal(t, "demo-org/demo-repo", DeriveURLPath("https://github.com/demo-org/demo-repo/"))
	assert.Empty(t, DeriveURLPath("not a url"))
	assert.Empty(t, DeriveURLPath("https://github.com"))
	assert.Equal(t, "42", DeriveLastSegment("org/proj/issues/42/"))
	assert.Equal(t, "TICKET-42", DeriveLastSegment("TICKET-42"))
	assert.Empty(t, DeriveLastSegment(""))
	assert.Empty(t, DeriveB64URLPath("!!!not-base64!!!"))
}
