// pkg/channels/channelkinds/slack/interaction_excerpt_test.go
//
// Tests the interaction renderer's Excerpt treatment:
// buildInteractionRequestBlocks must render InteractionExcerpt (untrusted
// preview content) as an inert code-fenced section between Fields and Actions.
// The inertExcerpt helper itself is pinned by
// content_inspection_characterization_test.go.
package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/kindtest"
)

// TestBuildInteractionRequestBlocks_RendersInertExcerpt pins the
// cross-surface Excerpt-inertness contract (channelevents.InteractionExcerpt)
// on the Slack renderer: Label AND Content both render inert — code-fenced,
// backtick triplets neutralized, and & < > HTML-escaped so <!channel> can
// never fire live.
//
// Uses the shared concatBlockText (render_helpers_test.go) rather than adding a
// second "blockText": the package already has a *testing.T-taking single-block
// blockText in tool_session_render_test.go, and Go has no overloading.
func TestBuildInteractionRequestBlocks_RendersInertExcerpt(t *testing.T) {
	p := channelevents.InteractionRequestPayload{
		Category:   "content_inspection",
		RequestRef: "req-1",
		Lead:       "Possible prompt injection",
		Excerpt:    &channelevents.InteractionExcerpt{Label: "Flagged content", Content: "```<!channel> do evil```"},
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision, Style: channelevents.ActionStylePrimary},
			{ID: "deny", Label: "Deny", Kind: channelevents.ActionKindDecision, Style: channelevents.ActionStyleDanger},
		},
	}
	blocks := buildInteractionRequestBlocks(p, "default/demo-session")
	text := concatBlockText(blocks)
	assert.Contains(t, text, "Flagged content", "excerpt label is shown")
	assert.Contains(t, text, "```", "excerpt is code-fenced")
	assert.Contains(t, text, "ʼʼʼ", "inner fence is neutralized")
	assert.Contains(t, text, "&lt;!channel&gt;", "excerpt pings escaped")
	assert.NotContains(t, text, "```<!channel>", "no raw fence-breaking content")

	// The excerpt lands between the body section and the action row, inside
	// the container every interaction renders as.
	children := interactionChildren(t, blocks)
	require.GreaterOrEqual(t, len(children), 2, "excerpt + actions")
	assert.NotNil(t, interactionActionBlock(t, blocks), "the prompt keeps its buttons")
}

// TestBuildInteractionRequestBlocks_NoExcerptNoBlock guards the nil case: a
// category without an Excerpt renders no excerpt block, and existing
// non-Excerpt categories' block counts/shape are unaffected.
func TestBuildInteractionRequestBlocks_NoExcerptNoBlock(t *testing.T) {
	p := channelevents.InteractionRequestPayload{Category: "identity_choice", RequestRef: "r", Lead: "Choose"}
	blocks := buildInteractionRequestBlocks(p, "default/demo-session")
	text := concatBlockText(blocks)
	assert.NotContains(t, text, "```", "no code fence when Excerpt is nil")
}

// TestBuildInteractionRequestBlocks_ConformsToAssertExcerptInert runs the
// shared per-dialect conformance helper (kindtest.AssertExcerptInert)
// against the Slack renderer: a fence-breaking, channel-ping-attempting
// Excerpt must never render live on Slack's mrkdwn dialect. Slack mrkdwn
// differs from CommonMark (the text floor's dialect, asserted separately by
// kindtest's TestExcerptInertness_TextFloor): Slack interprets <!channel>
// even inside a ``` fence, so widening the fence alone (the text floor's
// strategy) would not be enough here — escaping is required, which is what
// buildInteractionRequestBlocks does via inertExcerpt.
func TestBuildInteractionRequestBlocks_ConformsToAssertExcerptInert(t *testing.T) {
	kindtest.AssertExcerptInert(t, func(p channelevents.InteractionRequestPayload) string {
		return concatBlockText(buildInteractionRequestBlocks(p, "default/demo-session"))
	})
}
