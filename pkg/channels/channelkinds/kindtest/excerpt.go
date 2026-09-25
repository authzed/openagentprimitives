package kindtest

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// dangerousExcerptPayload builds an InteractionRequestPayload whose Excerpt
// combines a fence-breaking attempt (a raw ``` triplet inside Content) with
// a channel-wide-ping attempt (<!channel> in both Label and Content) — the
// two live-markup escapes every kind's Excerpt rendering must neutralize
// per channelevents.InteractionExcerpt's CONTRACT (Label and Content alike
// must render inert).
func dangerousExcerptPayload() channelevents.InteractionRequestPayload {
	return channelevents.InteractionRequestPayload{
		Category:   "kindtest-excerpt-inertness",
		RequestRef: "req-inert",
		Lead:       "Sample prompt",
		Excerpt: &channelevents.InteractionExcerpt{
			Label:   "Flagged content <!channel>",
			Content: "```escape attempt``` <!channel> <@U12345> ping",
		},
	}
}

// AssertExcerptInert feeds a fence-breaking, channel-ping-attempting Excerpt
// through kindRender — a closure that renders an InteractionRequestPayload the
// way one kind's dialect would — and asserts the output never lets the excerpt
// fire live: no surviving fence-breaker, no live "<!channel>" mention.
//
// This is the conformance check for mrkdwn-style dialects, whose fenced code
// blocks do NOT block special-sequence interpretation the way CommonMark's do;
// they must escape live sequences outright rather than merely widen the fence.
// It is deliberately NOT run against the CommonMark text floor, whose
// inert-fence strategy widens the fence past any backtick run in the content —
// CommonMark-safe, but the raw bytes remain present by design.
// TestExcerptInertness_TextFloor asserts that contract on its own terms.
func AssertExcerptInert(t *testing.T, kindRender func(channelevents.InteractionRequestPayload) string) {
	t.Helper()
	out := kindRender(dangerousExcerptPayload())
	assert.NotContains(t, out, "```escape attempt```", "raw fence-breaking excerpt must not survive rendering live")
	assert.NotContains(t, out, "<!channel>", "excerpt must not fire a live channel-wide ping")
}
