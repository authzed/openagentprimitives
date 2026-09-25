package identityadvisor

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildUserPromptTruncatesTranscriptRuneSafely verifies that an over-long
// thread transcript is truncated on rune boundaries, not byte boundaries. A
// naive byte-slice cut ([:MaxTranscriptChars]) lands mid-rune for multi-byte
// UTF-8 and produces an invalid string in the model request; the rune-safe cut
// keeps the output valid. (This truncation only becomes reachable once
// ThreadTranscript is wired, so the correctness is pinned here proactively.)
func TestBuildUserPromptTruncatesTranscriptRuneSafely(t *testing.T) {
	// "世" is 3 bytes / 1 rune. A leading ASCII byte offsets the byte boundary so
	// a byte-slice cut at MaxTranscriptChars would fall mid-rune. The transcript
	// has MaxTranscriptChars+100 runes, so it exceeds the rune budget and IS
	// truncated.
	transcript := "x" + strings.Repeat("世", MaxTranscriptChars+100)
	out := buildUserPrompt(Request{ThreadTranscript: transcript})

	assert.True(t, utf8.ValidString(out), "prompt must stay valid UTF-8 after truncation")
	assert.Contains(t, out, "… (truncated)", "an over-long transcript is marked truncated")
	assert.LessOrEqual(t, strings.Count(out, "世"), MaxTranscriptChars,
		"no more than the rune budget of transcript characters is retained")
}

// TestBuildUserPromptShortTranscriptNotTruncated verifies a within-budget
// transcript is passed through verbatim (no truncation marker).
func TestBuildUserPromptShortTranscriptNotTruncated(t *testing.T) {
	out := buildUserPrompt(Request{ThreadTranscript: "a short thread"})
	assert.Contains(t, out, "a short thread")
	assert.NotContains(t, out, "… (truncated)")
}

// TestParseAndValidate pins the model-output contract: a single JSON
// object with `mode` and `reason` fields, optionally wrapped in
// markdown fences. mode must be one of "agent"/"userPassthrough" and
// reason must be non-empty; anything else is a hard error so the
// caller can fall back to a deterministic default rather than bind an
// identity based on garbage model output.
func TestParseAndValidate(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantMode string
		wantErr  bool
	}{
		{"agent ok", `{"mode":"agent","reason":"busy thread, four people"}`, "agent", false},
		{"passthrough ok", "```json\n{\"mode\":\"userPassthrough\",\"reason\":\"solo fresh DM\"}\n```", "userPassthrough", false},
		{"bad mode rejected", `{"mode":"root","reason":"x"}`, "", true},
		{"empty reason rejected", `{"mode":"agent","reason":""}`, "", true},
		{"not json rejected", `sure! agent`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAndValidate(tc.raw)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantMode, got.Mode)
		})
	}
}
