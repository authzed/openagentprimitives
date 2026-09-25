package slack

import (
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
)

// conformanceArgs is the copy every registered notice category is driven with.
// Deliberately generic: this suite asserts SHAPE, not wording — wording is the
// publisher's, and pinning it here would make every copy edit a test edit.
func conformanceArgs() notice.Args {
	return notice.Args{
		Lead:     "Something happened",
		Body:     "A short explanation of what happened.",
		NextStep: "Do this about it.",
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	}
}

// registeredNotices returns every notice category, reading the live registry
// rather than a hand-maintained list so a new row is covered by this suite the
// moment it is added — the property that makes this a conformance test rather
// than a spot check.
//
// It re-registers first because sibling tests in this package call
// channelinteractions.Reset to install their own fixtures, and the registry is
// process-wide. Depending on ambient state would make this suite's result a
// function of test ORDER: green when run alone, red in the full package, which
// is the most expensive kind of failure to diagnose.
func registeredNotices(t *testing.T) []channelinteractions.Category {
	t.Helper()
	withRealCategories(t)

	var out []channelinteractions.Category
	for _, c := range channelinteractions.All() {
		if c.Notice {
			out = append(out, c)
		}
	}
	require.NotEmpty(t, out, "no notice categories are registered")
	return out
}

// Every registered notice must render as the house shape: one container,
// non-collapsible, header divider, chip-led rich_text_title, provenance last.
func TestEveryNoticeCategoryRendersTheHouseShape(t *testing.T) {
	for _, cat := range registeredNotices(t) {
		t.Run(cat.Name, func(t *testing.T) {
			pl, err := notice.New(cat.Name, conformanceArgs()).
				Payload(channelevents.SessionRef{Namespace: "agents", Name: "demo"}, "req-1")
			require.NoError(t, err, "every registered notice must build a valid payload")

			blocks := buildInteractionRequestBlocks(pl, "agents/demo")
			require.Len(t, blocks, 1, "a notice renders as exactly one container block")
			c, ok := blocks[0].(containerBlock)
			require.True(t, ok, "expected containerBlock, got %T", blocks[0])

			require.NoError(t, c.validate(), "the rendered container must be one Slack accepts")
			assert.False(t, c.IsCollapsible)
			assert.True(t, c.HasHeaderDivider)
			assert.Nil(t, c.Subtitle, "subtitle drops code spans; nothing may render there")

			// Title leads with the severity chip.
			require.NotNil(t, c.RichTextTitle)
			sec := c.RichTextTitle.Elements[0].(*slackapi.RichTextSection)
			_, isEmoji := sec.Elements[0].(*slackapi.RichTextSectionEmojiElement)
			assert.True(t, isEmoji, "the title must lead with the tone chip")

			// Provenance is the last child, behind a rule.
			last := c.ChildBlocks[len(c.ChildBlocks)-1]
			assert.Equal(t, slackapi.MBTContext, last.BlockType(),
				"the provenance context closes every notice")
			assert.Equal(t, slackapi.MBTDivider, c.ChildBlocks[len(c.ChildBlocks)-2].BlockType(),
				"a rule separates the body from the provenance footer")
		})
	}
}

// Every RESOLVED outcome must render as the house shape too.
//
// This sweep is the hole the bare-section applied card slipped through: the
// conformance suite covered only buildInteractionRequestBlocks, so the
// rendering that REPLACES a prompt on resolution was never asserted to be a
// container at all, and a resolved approval rendered as one line of text.
func TestEveryOutcomeRendersTheAppliedHouseShape(t *testing.T) {
	req := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: "agents", Name: "demo"},
		Category:        "tool_approval",
		RequestRef:      "req-1",
		Lead:            "Approve this tool call?",
		Fields:          []channelevents.InteractionField{{Label: "Tool", Value: "git_push"}},
	}
	outcomes := []string{
		channelevents.OutcomeApproved, channelevents.OutcomeDenied,
		channelevents.OutcomeExpired, channelevents.OutcomeResolved,
	}
	for _, outcome := range outcomes {
		t.Run(outcome, func(t *testing.T) {
			p := channelevents.InteractionAppliedPayload{
				AgentSessionRef: req.AgentSessionRef,
				Category:        req.Category,
				RequestRef:      req.RequestRef,
				Outcome:         outcome,
			}
			for _, tc := range []struct {
				name string
				req  *channelevents.InteractionRequestPayload
			}{
				{"with the cached prompt", &req},
				{"degraded, prompt not cached", nil},
			} {
				t.Run(tc.name, func(t *testing.T) {
					blocks := buildInteractionAppliedBlocks(p, tc.req)
					require.Len(t, blocks, 1, "a resolved interaction renders as exactly one container")
					c, ok := blocks[0].(containerBlock)
					require.True(t, ok, "expected containerBlock, got %T", blocks[0])
					require.NoError(t, c.validate(), "the rendered container must be one Slack accepts")

					require.NotNil(t, c.RichTextTitle)
					sec := c.RichTextTitle.Elements[0].(*slackapi.RichTextSection)
					_, isEmoji := sec.Elements[0].(*slackapi.RichTextSectionEmojiElement)
					assert.True(t, isEmoji, "the title must lead with the tone chip")

					assert.NotContains(t, concatBlockText(blocks), outcome,
						"the lowercase wire constant %q must never reach a reader", outcome)
				})
			}
		})
	}
}

// The plain-text notification preview renders on a lock screen and in the
// channel list, where no block does — so it needs the same human outcome the
// card got, not the enum value behind it.
func TestInteractionOutcomeText_EmptyOutcomeTextUsesTheHeadline(t *testing.T) {
	assert.Equal(t, ":large_green_circle: Approved",
		interactionOutcomeText(channelevents.InteractionAppliedPayload{
			Category: "tool_approval", Outcome: channelevents.OutcomeApproved,
		}),
		"tool_approval supplies no OutcomeText, so the headline is all the preview has")
}

// A warning or danger notice that names no action leaves the user stuck. This
// is the structural guarantee that motivates NextStep being its own field
// rather than a sentence someone might forget to write.
func TestToneRequiringNextStepIsEnforcedAtBuildTime(t *testing.T) {
	for _, cat := range registeredNotices(t) {
		t.Run(cat.Name, func(t *testing.T) {
			args := conformanceArgs()
			args.NextStep = ""
			_, err := notice.New(cat.Name, args).
				Payload(channelevents.SessionRef{Namespace: "agents", Name: "demo"}, "req-1")

			if cat.Tone.RequiresNextStep() {
				require.Error(t, err, "a %s notice must not build without a nextStep", cat.Tone)
				assert.Contains(t, err.Error(), "nextStep")
				return
			}
			assert.NoError(t, err, "%s severity has nothing for the user to do", cat.Tone)
		})
	}
}

// Every tone must map to a DISTINCT chip in both shapes, and every glyph to a
// chip of its own. An unmapped value falls through to the default and would
// paint a critical message as routine.
func TestEveryToneAndGlyphMapsToAChip(t *testing.T) {
	// Derived, never transcribed: a hand-written copy of the vocabulary stops
	// covering a tone the moment one is added, and an uncovered tone falls
	// through toneChip's default to the routine chip.
	tones := channelinteractions.AllTones()
	require.Len(t, tones, 8,
		"AllTones shrank; a tone dropped from the vocabulary silently stops being chip-checked")
	for _, terminal := range []bool{false, true} {
		seen := map[string]channelinteractions.Tone{}
		for _, tone := range tones {
			chip := toneChip(tone, terminal)
			require.NotEmpty(t, chip, "tone %q (terminal=%v) has no chip", tone, terminal)
			prev, dup := seen[chip]
			assert.False(t, dup,
				"tones %q and %q share chip %q at terminal=%v; they must be distinguishable",
				prev, tone, chip, terminal)
			seen[chip] = tone
		}
	}

	// The two shapes must never collide, or "is this over?" becomes unreadable.
	for _, tone := range tones {
		assert.NotEqual(t, toneChip(tone, false), toneChip(tone, true),
			"tone %q draws the same chip whether terminal or not", tone)
	}

	for _, g := range channelinteractions.AllGlyphs() {
		assert.NotEmpty(t, glyphChip(g), "glyph %q has no chip, so it would silently fall back to tone", g)
	}
	assert.Empty(t, glyphChip(channelinteractions.GlyphDefault),
		"the default glyph must defer to the tone chip")
}

// The degrade target has to carry the same facts as the container: it is what
// a user sees if Slack ever rejects the newer block.
func TestFallbackRenderingPreservesEveryNoticesFacts(t *testing.T) {
	for _, cat := range registeredNotices(t) {
		t.Run(cat.Name, func(t *testing.T) {
			pl, err := notice.New(cat.Name, conformanceArgs()).
				Payload(channelevents.SessionRef{Namespace: "agents", Name: "demo"}, "req-1")
			require.NoError(t, err)

			blocks := buildNoticeFallbackBlocks(pl, cat.Tone, cat.Terminal, "agents/demo")
			require.NotEmpty(t, blocks)
			for _, b := range blocks {
				_, isContainer := b.(containerBlock)
				require.False(t, isContainer, "the fallback must not use the block being fallen back from")
			}
			sec, ok := blocks[0].(*slackapi.SectionBlock)
			require.True(t, ok)
			assert.Contains(t, sec.Text.Text, pl.Lead)
			if pl.NextStep != "" {
				assert.Contains(t, sec.Text.Text, pl.NextStep,
					"the action must survive a degraded rendering")
			}
		})
	}
}

// withRealCategories installs the production category registry for the
// duration of a test, and restores it afterwards.
//
// The registry is process-wide and sibling tests Reset it to install their own
// fixtures, so any test that READS the live registry must put it back first.
// Without this, such a test passes alone and fails only under certain
// orderings — the most expensive kind of failure to diagnose, because it does
// not reproduce when you run the test that reported it.
func withRealCategories(t *testing.T) {
	t.Helper()
	channelinteractions.Reset()
	categories.RegisterAll()
	t.Cleanup(func() {
		channelinteractions.Reset()
		categories.RegisterAll()
	})
}
