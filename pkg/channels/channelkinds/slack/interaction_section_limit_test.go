// pkg/channelkinds/slack/interaction_section_limit_test.go
//
// A section block's mrkdwn is capped at 3000 characters, and exceeding it makes
// Slack reject the WHOLE message with an opaque invalid_blocks error. The reply
// path has split on that limit since chunkForSlackSection was written; the
// interaction path built one unbounded section from Body + every Field and did
// not.
//
// For an approval that failure is not a truncated message, it is a session that
// stops. The card never posts, nobody is asked, and the runner waits out its
// approval timeout on a decision no human was ever shown.
//
// It is reachable by ordinary input rather than by a hostile one: a plan-gate
// card's What lists a phase's whole ceiling plus one line per requested
// resource, update_plan admits 128 slots per phase, and a slot id may be 512
// characters — tens of thousands of characters from a plan the agent is
// entitled to declare.
package slack

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	slackapi "github.com/slack-go/slack"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// sectionTexts returns every section block's mrkdwn, descending into the
// container every interaction renders as.
func sectionTexts(blocks []slackapi.Block) []string {
	var out []string
	for _, b := range blocks {
		switch v := b.(type) {
		case containerBlock:
			out = append(out, sectionTexts(v.ChildBlocks)...)
		case *slackapi.SectionBlock:
			if v.Text != nil {
				out = append(out, v.Text.Text)
			}
		}
	}
	return out
}

func TestBuildInteractionRequestBlocks_noSectionExceedsSlacksLimit(t *testing.T) {
	// A plan card shaped like one the gate really produces: a ceiling, then a
	// line per requested resource. Sized past the limit with realistic content
	// rather than filler, so the failure mode matches the production one.
	var what strings.Builder
	what.WriteString("perm:push:git_repo\nperm:create_repo:github_repo\n\nAlso asks to reach:")
	for i := 0; i < 120; i++ {
		what.WriteString("\n  git_repo — https://github.example.invalid/some-organization/repository-number-")
		what.WriteString(strings.Repeat("x", 40))
	}
	what.WriteString("\nEvery resource above is named, so this approval covers them.")
	require.Greater(t, len([]rune(what.String())), 3000, "the fixture must actually exceed the limit")

	p := channelevents.InteractionRequestPayload{
		Category:   "plan_phase",
		RequestRef: "req-1",
		Lead:       "Plan approval",
		Fields: []channelevents.InteractionField{
			{Label: "What", Value: what.String()},
			{Label: "Why (the agent's words)", Value: "the ticket names these repositories"},
		},
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Kind: channelevents.ActionKindDecision},
		},
	}

	blocks := buildInteractionRequestBlocks(p, "default/demo-session")

	for i, s := range sectionTexts(blocks) {
		assert.LessOrEqual(t, len([]rune(s)), 3000,
			"section %d is over Slack's limit; chat.postMessage rejects the whole message "+
				"and the approval is never shown to anyone", i)
	}
}

// Splitting must not become dropping. The resource is the single fact the
// approver most needs, and it is the part that lands last — so a splitter that
// truncated instead of chunking would remove exactly it while leaving a card
// that still looks complete.
func TestBuildInteractionRequestBlocks_splittingPreservesEveryResource(t *testing.T) {
	const needle = "repository-that-must-survive-the-split"

	var what strings.Builder
	what.WriteString("perm:push:git_repo\n\nAlso asks to reach:")
	for i := 0; i < 120; i++ {
		what.WriteString("\n  git_repo — https://github.example.invalid/org/filler-" + strings.Repeat("y", 40))
	}
	// Last, where a truncating implementation would lose it.
	what.WriteString("\n  git_repo — https://github.example.invalid/org/" + needle)

	p := channelevents.InteractionRequestPayload{
		Category:   "plan_phase",
		RequestRef: "req-1",
		Lead:       "Plan approval",
		Fields:     []channelevents.InteractionField{{Label: "What", Value: what.String()}},
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision},
		},
	}

	text := concatBlockText(buildInteractionRequestBlocks(p, "default/demo-session"))
	assert.Contains(t, text, needle,
		"a resource dropped by the renderer is a resource the approver consents to without seeing")
}

// The ordinary case must not change shape. Splitting a card that fits would
// turn one section into several for every approval in the system.
func TestBuildInteractionRequestBlocks_aShortCardStaysOneSection(t *testing.T) {
	p := channelevents.InteractionRequestPayload{
		Category:   "plan_phase",
		RequestRef: "req-1",
		Lead:       "Plan approval",
		Fields: []channelevents.InteractionField{
			{Label: "What", Value: "perm:read:tracker_issue"},
			{Label: "When", Value: "phase 1 of 2"},
		},
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision},
		},
	}

	assert.Len(t, sectionTexts(buildInteractionRequestBlocks(p, "default/demo-session")), 1,
		"a card that fits must render exactly as it did before")
}
