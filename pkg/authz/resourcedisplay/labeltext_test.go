package resourcedisplay_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/resourcedisplay"
)

// spiceDBObjectIDMax is SpiceDB's own limit on an object id. Restated here
// rather than imported because the point of the assertion below is that this
// package's cap keeps every id it mints under a limit it does not control.
const spiceDBObjectIDMax = 1024

// spiceDBObjectIDAlphabet is the character set SpiceDB admits in an object id.
// A name that survives encoding must be spelled entirely from it, or the write
// that carries it is rejected — which is the failure this encoding exists to
// make unreachable by naming a channel badly.
const spiceDBObjectIDAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/_|-=+"

// encodeRawForTest encodes text WITHOUT sanitizing it — deliberately not
// EncodeB64Text, so a test can build the id an older build (or anything that
// skipped the encoder) would have stored, and prove the read does not trust it.
func encodeRawForTest(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

// The round trip is the contract: whatever a sync stored, a console shows.
// Names chosen for the characters a SpiceDB object id CANNOT hold directly —
// spaces, punctuation, non-ASCII, emoji — because a bare-string encoding would
// pass a test using only "demo-channel".
func TestEncodeDecodeB64Text_RoundTripsNamesAnObjectIDCannotHoldDirectly(t *testing.T) {
	for _, name := range []string{
		"demo-channel",
		"Demo Engineers",
		"acme / platform (on-call)",
		"Ingeniería",
		"team 🚀 demo",
	} {
		t.Run(name, func(t *testing.T) {
			id, ok := resourcedisplay.EncodeB64Text(name)
			require.True(t, ok, "a renderable name must encode")

			for _, r := range id {
				assert.Containsf(t, spiceDBObjectIDAlphabet, string(r),
					"encoded id must be spelled only from SpiceDB's object-id alphabet; %q is not", string(r))
			}

			title, href := resourcedisplay.DecoderB64Text.Decode(id)
			assert.Equal(t, name, title, "the name a sync stored is the name a console shows")
			assert.Empty(t, href, "a display name is never a link target")
		})
	}
}

// The hostile half. Each of these renders as something OTHER than what it
// says when passed through unchanged: a bidi override reorders the text around
// it, so a channel a person reads as one name is stored as another; zero-width
// characters make two distinct rows look identical; a control character can
// reach an operator's terminal through a log line.
//
// Dropped, never escaped — see SanitizeLabelText — so the assertion is on what
// SURVIVES, not on how it was quoted.
func TestSanitizeLabelText_DropsTheCharactersThatMakeANameLieAboutItself(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "right-to-left override: reordering text around it is a renamed row",
			raw:  "demo-‮gnp.exe",
			want: "demo-gnp.exe",
		},
		{
			name: "bidi isolate pair: same reordering, newer codepoints",
			raw:  "⁦demo⁩-channel",
			want: "demo-channel",
		},
		{
			name: "zero-width space: two distinct rows rendering identically",
			raw:  "demo​-channel",
			want: "demo-channel",
		},
		{
			name: "C0 controls: a name that reaches a terminal through a log line",
			raw:  "demo\x1b[31m-channel\x07",
			want: "demo[31m-channel",
		},
		{
			name: "newline and tab: a one-line row is not a place for either",
			raw:  "demo\n\t-channel",
			want: "demo-channel",
		},
		{
			name: "surrounding whitespace is trimmed, interior is kept",
			raw:  "  demo channel  ",
			want: "demo channel",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resourcedisplay.SanitizeLabelText(tc.raw))
		})
	}
}

// Invalid UTF-8 derives NOTHING rather than being salvaged: replacement
// characters would render as a name the directory never had.
func TestSanitizeLabelText_InvalidUTF8DerivesNothing(t *testing.T) {
	assert.Empty(t, resourcedisplay.SanitizeLabelText("demo\xff\xfechannel"))

	_, ok := resourcedisplay.EncodeB64Text("demo\xff\xfechannel")
	assert.False(t, ok, "a name that sanitizes to nothing must not be stored as a label")
}

// A name with nothing renderable left is not a label. The caller must fall
// back to the raw id, which is strictly better than a blank row.
func TestEncodeB64Text_RefusesANameWithNothingRenderableLeft(t *testing.T) {
	for _, raw := range []string{"", "   ", "​​", "\x00\x01\x02"} {
		id, ok := resourcedisplay.EncodeB64Text(raw)
		assert.False(t, ok, "%q has no renderable content", raw)
		assert.Empty(t, id)
	}
}

// The cap is a rune-boundary cut, and getting it wrong is not cosmetic: a
// byte-boundary cut produces invalid UTF-8, which SanitizeLabelText then
// rejects outright — so the long name would decode to NOTHING rather than to
// a shortened name.
func TestSanitizeLabelText_TruncatesOnARuneBoundaryAndSaysItDidSo(t *testing.T) {
	// Multi-byte throughout, so any byte-boundary cut lands mid-sequence.
	long := strings.Repeat("é", 400)

	got := resourcedisplay.SanitizeLabelText(long)

	assert.True(t, utf8.ValidString(got), "a truncated name must still be valid UTF-8")
	assert.LessOrEqual(t, len(got), resourcedisplay.MaxLabelTextBytes, "the cap must actually bound the result")
	assert.True(t, strings.HasSuffix(got, "…"), "a shortened name must not present itself as a whole one")
	assert.True(t, strings.HasPrefix(got, "éé"), "the surviving prefix is the start of the real name")
}

// The cap's storage half: whatever a directory names a group, the id this
// package mints fits in a SpiceDB object id. A name over the cap is the case
// that would otherwise fail the WRITE — long after the sanitizing code ran and
// nowhere near it.
func TestEncodeB64Text_StaysWithinSpiceDBsObjectIDLimit(t *testing.T) {
	id, ok := resourcedisplay.EncodeB64Text(strings.Repeat("é", 4000))

	require.True(t, ok)
	assert.LessOrEqual(t, len(id), spiceDBObjectIDMax,
		"an encoded name must never be the reason a relationship write is rejected")
}

// Idempotence is what lets the same function run on the way IN and on the way
// OUT. Without it, decoding would keep mutating a stored name and a row's
// title would drift from what the sync wrote.
func TestSanitizeLabelText_IsIdempotent(t *testing.T) {
	for _, raw := range []string{
		"demo-channel",
		"  demo ‮channel  ",
		strings.Repeat("é", 400),
	} {
		once := resourcedisplay.SanitizeLabelText(raw)
		assert.Equal(t, once, resourcedisplay.SanitizeLabelText(once), "sanitizing twice must change nothing")
	}
}

// The guard that matters most on this page, and the reason a display name gets
// its OWN decoder instead of reusing DecoderB64URL: a channel someone named
// after a URL is TEXT. Pairing an attacker-chosen label with an
// attacker-chosen target is the whole shape of a spoofed link, and the only
// way to be sure it cannot happen is for this decoder to have no path that
// returns an href at all.
func TestDecoderB64Text_NeverDerivesAnHref(t *testing.T) {
	for _, name := range []string{
		"https://evil.example/demo-org/widgets",
		"http://evil.example",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
	} {
		t.Run(name, func(t *testing.T) {
			id, ok := resourcedisplay.EncodeB64Text(name)
			require.True(t, ok)

			title, href := resourcedisplay.DecoderB64Text.Decode(id)
			assert.NotEmpty(t, title, "the name is still shown — as text")
			assert.Empty(t, href, "a directory-authored name must never become a link target")
		})
	}
}

// An id that is not this encoding derives nothing, so the caller renders the
// raw id — the same fallback every other deriver in this package has.
func TestDecoderB64Text_UndecodableIDDerivesNothing(t *testing.T) {
	for _, raw := range []string{"not base64!", "", "***"} {
		title, href := resourcedisplay.DecoderB64Text.Decode(raw)
		assert.Empty(t, title)
		assert.Empty(t, href)
	}
}

// Sanitizing runs again on the way OUT, so a label written by an older build —
// or by anything that skipped the encoder — cannot put a bidi override on the
// page. The fixture is a hand-built id, deliberately NOT produced by
// EncodeB64Text, because what is being proven is that the read does not trust
// the write.
func TestDecoderB64Text_SanitizesWhatItReadsBack(t *testing.T) {
	// Unsanitized payload, encoded directly: exactly what a stored row written
	// before this sanitizing existed would look like.
	raw := encodeRawForTest("demo-‮gnp.exe")

	title, href := resourcedisplay.DecoderB64Text.Decode(raw)
	assert.Equal(t, "demo-gnp.exe", title, "a stored override must not survive the read either")
	assert.Empty(t, href)
}
