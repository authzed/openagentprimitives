package steelthread_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// generateOne resolves a single bare original and returns the stand-in chosen
// for it.
func generateOne(t *testing.T, old string) string {
	t.Helper()
	got, err := steelthread.ResolveRedactions([]steelthread.Redaction{{Old: old}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, old, got[0].Old, "resolution must not rewrite the original")
	return got[0].New
}

// TestResolveRedactions_AGeneratedStandInIsTheOriginalsByteLength is the
// property the whole mechanism exists for.
//
// A length-changing replacement desynchronises every recorded value derived
// from a byte count — an artifact's size, a content length — which the replay
// recomputes from the redacted bytes while the recorded number was computed
// from the original. The divergence then surfaces at the artifact, many turns
// from the redaction, with nothing naming a rule.
//
// BYTES, not runes, and the multi-byte rows are what pin that. A byte count is
// what the bundle records, so a UTF-8 original is matched by its encoded
// length: the stand-in for a 14-byte, 12-rune name is 14 ASCII characters, and
// the rune count deliberately does not survive.
func TestResolveRedactions_AGeneratedStandInIsTheOriginalsByteLength(t *testing.T) {
	cases := []struct {
		name string
		old  string
	}{
		{name: "one byte", old: "x"},
		{name: "two bytes", old: "xy"},
		{name: "three bytes, below where the stem fits", old: "xyz"},
		{name: "four bytes, the shortest legible token", old: "acme"},
		{name: "eight bytes, exactly the stem's own width", old: "acmecorp"},
		{name: "twelve bytes, the width that failed a real replay", old: "demoowneracc"},
		{name: "twenty bytes", old: "acme-corporation-ltd"},
		{name: "longer than one digest, so the digest has to be blocked", old: strings.Repeat("a-long-host.", 8)},
		{name: "multi-byte: 14 bytes across 12 runes", old: "café-münchen"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := generateOne(t, tc.old)
			assert.Equal(t, len(tc.old), len(got),
				"a stand-in of a different byte length desyncs every recorded value derived from a byte count")
			assert.NotContains(t, got, tc.old, "a stand-in that contains the original removes nothing")
		})
	}

	t.Run("the multi-byte trade is byte count over rune count, deliberately", func(t *testing.T) {
		const old = "café-münchen"
		require.Equal(t, 14, len(old), "the fixture must actually be multi-byte or this proves nothing")
		require.Equal(t, 12, utf8.RuneCountInString(old))

		got := generateOne(t, old)
		assert.Equal(t, 14, len(got), "byte length is what the recorded counts measure")
		assert.Equal(t, 14, utf8.RuneCountInString(got),
			"the ASCII stand-in has more runes than the original; that is the accepted cost of matching bytes")
	})
}

// TestResolveRedactions_AGeneratedStandInIsDeterministic pins that the token is
// a function of its inputs and nothing else — no clock, no randomness, no map
// iteration order.
//
// Without it a bundle stops being reproducible: every re-capture of the same
// session with the same rules rewrites every redacted occurrence to a new
// token, and the diff a reviewer is shown is entirely churn.
func TestResolveRedactions_AGeneratedStandInIsDeterministic(t *testing.T) {
	rules := []steelthread.Redaction{
		{Old: "acme-corporation"},
		{Old: "beta-labs", New: "COMPANY-B"},
		{Old: "internal-host.example"},
	}

	first, err := steelthread.ResolveRedactions(rules)
	require.NoError(t, err)
	second, err := steelthread.ResolveRedactions(rules)
	require.NoError(t, err)
	assert.Equal(t, first, second, "two resolutions of the same rule set must agree exactly")

	// A separately-constructed set with the same content, so the agreement is
	// about the inputs rather than about the slice being reused.
	third, err := steelthread.ResolveRedactions([]steelthread.Redaction{
		{Old: "acme-corporation"},
		{Old: "beta-labs", New: "COMPANY-B"},
		{Old: "internal-host.example"},
	})
	require.NoError(t, err)
	assert.Equal(t, first, third, "the token must depend on the rules' content, not on their identity")

	// And resolving an already-resolved set changes nothing, which is what lets
	// the CLI resolve early to report what it will do and Capture resolve again
	// for the callers that did not.
	fourth, err := steelthread.ResolveRedactions(first)
	require.NoError(t, err)
	assert.Equal(t, first, fourth, "ResolveRedactions must be idempotent")
}

// TestResolveRedactions_AGeneratedStandInIsLegible pins that a reader of a
// committed fixture can tell a stand-in from data.
//
// The token leads with as much of "redacted" as the length allows and spends
// the rest on the digest. Below four bytes there is no room for both and the
// digest wins — a token nobody recognises as a placeholder is a cosmetic
// problem, and two identities sharing one token is a correctness problem.
func TestResolveRedactions_AGeneratedStandInIsLegible(t *testing.T) {
	cases := []struct {
		name       string
		old        string
		wantPrefix string
	}{
		{name: "twelve bytes carries the whole word", old: "demoowneracc", wantPrefix: "redacted"},
		{name: "nine bytes carries the whole word", old: "acme-corp", wantPrefix: "redacted"},
		{name: "eight bytes gives one character to the digest", old: "acmecorp", wantPrefix: "redacte"},
		{name: "four bytes still reads as a redaction", old: "acme", wantPrefix: "red"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := generateOne(t, tc.old)
			assert.True(t, strings.HasPrefix(got, tc.wantPrefix),
				"%q must lead with %q so a reader of the fixture sees a placeholder", got, tc.wantPrefix)
		})
	}

	// Lowercase alphanumeric throughout, and that is a correctness property
	// rather than a style one: a redacted value still goes through whatever
	// validated it the first time, and an upper-case stand-in for a lowercase
	// slug made a replayed toolspec refuse its own call once already.
	t.Run("every generated token is lowercase alphanumeric", func(t *testing.T) {
		for _, old := range []string{"x", "xyz", "acme", "acmecorp", "demoowneracc", "café-münchen"} {
			got := generateOne(t, old)
			for _, r := range got {
				assert.True(t, (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'),
					"stand-in %q for %q carries %q, which is not legal everywhere a slug is", got, old, r)
			}
		}
	})
}

// TestResolveRedactions_AGeneratedStandInIsUniqueAcrossTheSet pins the property
// that has to hold by construction rather than by probability.
//
// A digest prefix short enough to fit a short original is short enough to
// collide, and a collision silently merges two identities into one token: the
// reader of the fixture sees one company where the session named two, and
// nothing anywhere says so.
func TestResolveRedactions_AGeneratedStandInIsUniqueAcrossTheSet(t *testing.T) {
	t.Run("two same-length originals never share a token", func(t *testing.T) {
		got, err := steelthread.ResolveRedactions([]steelthread.Redaction{
			{Old: "acme-corp"}, {Old: "beta-labs"}, {Old: "gamma-inc"},
		})
		require.NoError(t, err)
		seen := map[string]string{}
		for _, r := range got {
			require.NotContains(t, seen, r.New, "%q and %q were given the same stand-in %q", seen[r.New], r.Old, r.New)
			seen[r.New] = r.Old
		}
	})

	// The deterministic collision. The token generation would produce for
	// "acme-corp" is claimed FIRST by a supplied rule of the same width, so the
	// generator has to notice and choose again — which is the branch a merely
	// improbable collision would never reach in a test.
	t.Run("a taken token forces the generator to choose another", func(t *testing.T) {
		wouldGenerate := generateOne(t, "acme-corp")
		require.Len(t, wouldGenerate, len("acme-corp"))

		got, err := steelthread.ResolveRedactions([]steelthread.Redaction{
			{Old: "beta-labs", New: wouldGenerate},
			{Old: "acme-corp"},
		})
		require.NoError(t, err)
		assert.Equal(t, wouldGenerate, got[0].New, "a supplied token is never taken away from its rule")
		assert.NotEqual(t, wouldGenerate, got[1].New,
			"the generated token must differ from the one already claimed, or two originals share a stand-in")
		assert.Len(t, got[1].New, len("acme-corp"), "choosing again must not change the length")
	})

	// applyRedactions runs the rules in ORDER over the same text, so a later
	// rule whose original appears inside an earlier rule's stand-in would
	// rewrite part of that stand-in — and the bundle would then record a token
	// that is nowhere in the files it describes.
	t.Run("a generated token never contains another rule's original", func(t *testing.T) {
		got, err := steelthread.ResolveRedactions([]steelthread.Redaction{
			{Old: "redacted", New: "ELIDEDXX"},
			{Old: "acme-corp"},
		})
		require.NoError(t, err)
		assert.NotContains(t, got[1].New, "redacted",
			"the stem itself is a later rule's original here, so the generator has to give it up")
		assert.Len(t, got[1].New, len("acme-corp"))
	})
}

// TestResolveRedactions_RefusesWhenNoUniqueStandInFits pins the fail-closed
// end of generation.
//
// Sixteen one-byte originals that are exactly the sixteen hex digits: every
// candidate token at that length IS one of them, so there is no token left to
// choose. Refusing names the length and points at the explicit form; reusing a
// token would merge two identities silently, which is the failure the whole
// uniqueness rule exists to prevent.
func TestResolveRedactions_RefusesWhenNoUniqueStandInFits(t *testing.T) {
	var rules []steelthread.Redaction
	for _, c := range "0123456789abcdef" {
		rules = append(rules, steelthread.Redaction{Old: string(c)})
	}

	_, err := steelthread.ResolveRedactions(rules)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not generate a 1-byte stand-in")
	assert.Contains(t, err.Error(), "Name the replacement explicitly",
		"the refusal has to name the way out, or an operator is simply stuck")
}

// TestResolveRedactions_ValidatesSuppliedRulesFromAProgrammaticCaller pins that
// the checks are not the parser's alone.
//
// Every test in this package builds Redactions in code rather than through
// ParseRedaction, and so does anything embedding the capture. A check that
// lived only in the parser would be a second, weaker contract for exactly the
// callers most likely to get it wrong.
func TestResolveRedactions_ValidatesSuppliedRulesFromAProgrammaticCaller(t *testing.T) {
	cases := []struct {
		name    string
		rule    steelthread.Redaction
		wantErr string
	}{
		{
			name:    "a shorter replacement is refused, naming the required length",
			rule:    steelthread.Redaction{Old: "acme-corporation", New: "COMPANY-A"},
			wantErr: "exactly 16 byte(s)",
		},
		{
			name:    "a longer replacement is refused too",
			rule:    steelthread.Redaction{Old: "acme", New: "COMPANY-A"},
			wantErr: "exactly 4 byte(s)",
		},
		{
			// SAME length, so the length check cannot be what refuses it —
			// which is the only way to prove the containment check is still
			// there. At equal lengths "contains" means "equals", and a rule
			// replacing a value with itself is a no-op that reports a hit count
			// for every occurrence it left in place.
			name:    "a replacement containing the original is refused on its own account",
			rule:    steelthread.Redaction{Old: "acme", New: "acme"},
			wantErr: "still contains the value it replaces",
		},
		{
			name:    "an empty original is refused",
			rule:    steelthread.Redaction{New: "COMPANY-A"},
			wantErr: "empty original",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := steelthread.ResolveRedactions([]steelthread.Redaction{tc.rule})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestCapture_AGeneratedStandInReachesEverySurfaceAndIsRecorded is the
// end-to-end half: a rule that named no replacement removes the original from
// every emitted file, and the bundle records the token the capture chose —
// still without the original, which is the contract that has not changed.
func TestCapture_AGeneratedStandInReachesEverySurfaceAndIsRecorded(t *testing.T) {
	recs := recordsNamingACustomer(t)
	res, _ := captureWith(t, recs, "acme-corp")

	require.NotNil(t, res.Bundle.Capture)
	require.Len(t, res.Bundle.Capture.Redactions, 1)
	token := res.Bundle.Capture.Redactions[0].Replacement
	require.Len(t, token, len("acme-corp"))
	assert.Positive(t, res.Bundle.Capture.Redactions[0].Count)

	require.NotEmpty(t, res.Emitted)
	for _, f := range res.Emitted {
		assert.NotContains(t, string(f.Bytes), "acme-corp", "%s was not redacted", f.Name)
	}
	assert.Contains(t, string(bytesOf(t, res, "bundle.json")), token,
		"the recorded token has to be the one actually written, or the record describes nothing")

	// Two captures of the same records under the same rule produce the same
	// bytes. A generated stand-in that churned would make every re-capture an
	// unreadable diff.
	again, _ := captureWith(t, recs, "acme-corp")
	require.Len(t, again.Emitted, len(res.Emitted))
	for i := range res.Emitted {
		assert.Equal(t, res.Emitted[i].Name, again.Emitted[i].Name)
		assert.Equal(t, string(res.Emitted[i].Bytes), string(again.Emitted[i].Bytes),
			"%s differs between two captures of the same session under the same rules", res.Emitted[i].Name)
	}
}
