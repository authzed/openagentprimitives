package channelevents

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The Outcome* constants are wire enum values — what one program tells
// another. Every surface that shows a resolved interaction needs the human
// word, and several categories (both tool-approval decision handlers) resolve
// with no OutcomeText at all, so this fallback is the only thing standing
// between a reader and the raw enum.
func TestOutcomeHeadline(t *testing.T) {
	cases := []struct {
		outcome string
		want    string
	}{
		{OutcomeApproved, "Approved"},
		{OutcomeDenied, "Denied"},
		{OutcomeExpired, "Expired"},
		{OutcomeResolved, "Resolved"},
	}
	for _, tc := range cases {
		t.Run(tc.outcome, func(t *testing.T) {
			assert.Equal(t, tc.want, OutcomeHeadline(tc.outcome))
		})
	}
}

// An outcome this build does not know about is still better shown than
// swallowed — a renderer must never be the reason a resolution goes silent.
func TestOutcomeHeadline_UnknownOutcomePassesThrough(t *testing.T) {
	assert.Equal(t, "some_future_outcome", OutcomeHeadline("some_future_outcome"))
	assert.Empty(t, OutcomeHeadline(""), "an empty outcome has no word to invent")
}
