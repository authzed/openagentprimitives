package slack

import (
	"encoding/json"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

func registerNoticeCat(t *testing.T, name string, tone channelinteractions.Tone, terminal bool, g channelinteractions.Glyph) {
	t.Helper()
	channelinteractions.Reset()
	t.Cleanup(channelinteractions.Reset)
	channelinteractions.Register(channelinteractions.Category{
		Name: name, Notice: true, Tone: tone, Terminal: terminal, Glyph: g,
		Resurface: channelinteractions.ResurfaceNone,
	})
}

func noticeReq(category string) channelevents.InteractionRequestPayload {
	return channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: "agents", Name: "sre-bot-4f2c"},
		Category:        category,
		RequestRef:      "req-1",
		Lead:            "Agent stopped",
		Body:            "It ran out of memory while running `analyze_logs`.",
		NextStep:        "Start a new thread to try again.",
		Audience:        channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	}
}

// firstContainer asserts the block set is exactly one container and returns it.
func firstContainer(t *testing.T, blocks []slackapi.Block) containerBlock {
	t.Helper()
	require.Len(t, blocks, 1, "a notice renders as a single container block")
	c, ok := blocks[0].(containerBlock)
	require.True(t, ok, "expected containerBlock, got %T", blocks[0])
	return c
}

// childTypes lists the child block types in order — the shape assertion.
func childTypes(c containerBlock) []slackapi.MessageBlockType {
	out := make([]slackapi.MessageBlockType, 0, len(c.ChildBlocks))
	for _, ch := range c.ChildBlocks {
		out = append(out, ch.BlockType())
	}
	return out
}

// titleElements returns the rich_text_title's section elements.
func titleElements(t *testing.T, c containerBlock) []slackapi.RichTextSectionElement {
	t.Helper()
	require.NotNil(t, c.RichTextTitle, "title must be rich_text_title: the plain_text title does not render emoji")
	require.Len(t, c.RichTextTitle.Elements, 1)
	sec, ok := c.RichTextTitle.Elements[0].(*slackapi.RichTextSection)
	require.True(t, ok, "expected *RichTextSection, got %T", c.RichTextTitle.Elements[0])
	return sec.Elements
}

func TestBuildNoticeBlocks_HouseStyleShape(t *testing.T) {
	registerNoticeCat(t, "agent_failed", channelinteractions.ToneCritical, false, channelinteractions.GlyphDefault)
	blocks := buildInteractionRequestBlocks(noticeReq("agent_failed"), "agents/sre-bot-4f2c")
	c := firstContainer(t, blocks)

	require.NoError(t, c.validate())
	assert.False(t, c.IsCollapsible, "a notice is not collapsible")
	assert.True(t, c.HasHeaderDivider, "which is what makes the header divider legal")
	assert.Equal(t, []slackapi.MessageBlockType{
		slackapi.MBTSection, slackapi.MBTDivider, slackapi.MBTContext,
	}, childTypes(c), "body, rule, provenance")
}

// The chip is the severity signal. It must be an emoji ELEMENT — a literal
// unicode square in a text element renders as `:large_red_square:` in a
// container title, which is the trap this shape exists to avoid.
func TestBuildNoticeBlocks_ChipComesFromSeverity(t *testing.T) {
	cases := []struct {
		name     string
		tone     channelinteractions.Tone
		wantChip string
	}{
		{name: "critical, non-terminal → red circle", tone: channelinteractions.ToneCritical, wantChip: "red_circle"},
		{name: "degraded → orange circle", tone: channelinteractions.ToneDegraded, wantChip: "large_orange_circle"},
		{name: "routine → blue circle", tone: channelinteractions.ToneRoutine, wantChip: "large_blue_circle"},
		{name: "resolved → green circle", tone: channelinteractions.ToneResolved, wantChip: "large_green_circle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registerNoticeCat(t, "demo_notice", tc.tone, false, channelinteractions.GlyphDefault)
			c := firstContainer(t, buildInteractionRequestBlocks(noticeReq("demo_notice"), "s"))
			els := titleElements(t, c)

			require.Len(t, els, 3, "chip, spacer, bold lead")
			emoji, ok := els[0].(*slackapi.RichTextSectionEmojiElement)
			require.True(t, ok, "first title element must be an emoji element, got %T", els[0])
			assert.Equal(t, tc.wantChip, emoji.Name)
		})
	}
}

// A glyph override replaces the severity chip; severity still drives
// everything else. This is the "rare circumstance" escape hatch.
func TestBuildNoticeBlocks_GlyphOverridesTheChip(t *testing.T) {
	registerNoticeCat(t, "leak_blocked", channelinteractions.ToneCritical, false, channelinteractions.GlyphMoney)
	c := firstContainer(t, buildInteractionRequestBlocks(noticeReq("leak_blocked"), "s"))

	emoji, ok := titleElements(t, c)[0].(*slackapi.RichTextSectionEmojiElement)
	require.True(t, ok)
	assert.Equal(t, "moneybag", emoji.Name, "the category's glyph wins over its severity chip")
}

// The spacer must be its own element AND non-breaking. Slack trims leading
// whitespace inside the lead's element outright, and collapses a run of
// ordinary spaces to one even in a element of its own — so a plain "  " and a
// plain " " render identically flush.
func TestBuildNoticeBlocks_SpacerIsItsOwnElement(t *testing.T) {
	registerNoticeCat(t, "demo_notice", channelinteractions.ToneRoutine, false, channelinteractions.GlyphDefault)
	els := titleElements(t, firstContainer(t, buildInteractionRequestBlocks(noticeReq("demo_notice"), "s")))

	require.Len(t, els, 3)
	spacer, ok := els[1].(*slackapi.RichTextSectionTextElement)
	require.True(t, ok, "second element must be a text spacer, got %T", els[1])
	assert.Equal(t, chipSpacer, spacer.Text)
	assert.Equal(t, "\u00a0\u00a0", spacer.Text,
		"ordinary spaces collapse to one in a rich_text element; only U+00A0 holds the gap")

	lead, ok := els[2].(*slackapi.RichTextSectionTextElement)
	require.True(t, ok)
	assert.Equal(t, "Agent stopped", lead.Text, "the lead carries no leading whitespace of its own")
	require.NotNil(t, lead.Style)
	assert.True(t, lead.Style.Bold)
}

func TestBuildNoticeBlocks_NextStepIsEmphasisedInTheBody(t *testing.T) {
	registerNoticeCat(t, "demo_notice", channelinteractions.ToneCritical, false, channelinteractions.GlyphDefault)
	c := firstContainer(t, buildInteractionRequestBlocks(noticeReq("demo_notice"), "s"))

	sec, ok := c.ChildBlocks[0].(*slackapi.SectionBlock)
	require.True(t, ok)
	assert.Contains(t, sec.Text.Text, "*Start a new thread to try again.*")
	assert.Contains(t, sec.Text.Text, "It ran out of memory")
}

// Provenance goes in a trailing context child, NOT the container subtitle:
// subtitle renders emoji but silently drops mrkdwn code spans, so a session
// ref there would show visible backticks.
func TestBuildNoticeBlocks_ProvenanceIsATrailingContextNotSubtitle(t *testing.T) {
	registerNoticeCat(t, "demo_notice", channelinteractions.ToneRoutine, false, channelinteractions.GlyphDefault)
	c := firstContainer(t, buildInteractionRequestBlocks(noticeReq("demo_notice"), "s"))

	assert.Nil(t, c.Subtitle, "subtitle drops code spans; provenance must not live there")
	last, ok := c.ChildBlocks[len(c.ChildBlocks)-1].(*slackapi.ContextBlock)
	require.True(t, ok, "last child must be the provenance context")
	require.Len(t, last.ContextElements.Elements, 1)
	txt, ok := last.ContextElements.Elements[0].(*slackapi.TextBlockObject)
	require.True(t, ok)
	assert.Contains(t, txt.Text, "`agents/sre-bot-4f2c`")
}

// Show Details appears only when there is something to show.
func TestBuildNoticeBlocks_DetailsButtonGatedOnDetails(t *testing.T) {
	registerNoticeCat(t, "demo_notice", channelinteractions.ToneCritical, false, channelinteractions.GlyphDefault)

	without := firstContainer(t, buildInteractionRequestBlocks(noticeReq("demo_notice"), "s"))
	assert.NotContains(t, childTypes(without), slackapi.MBTAction, "no Details ⇒ no button")

	p := noticeReq("demo_notice")
	p.Details = json.RawMessage(`{"reason":"OOMKilled"}`)
	with := firstContainer(t, buildInteractionRequestBlocks(p, "s"))
	assert.Contains(t, childTypes(with), slackapi.MBTAction, "Details ⇒ a button to show them")
}

// An untrusted excerpt stays fenced inside the container, same contract the
// prompt renderer upholds.
func TestBuildNoticeBlocks_ExcerptIsInert(t *testing.T) {
	registerNoticeCat(t, "demo_notice", channelinteractions.ToneDegraded, false, channelinteractions.GlyphDefault)
	p := noticeReq("demo_notice")
	p.Excerpt = &channelevents.InteractionExcerpt{Label: "Detail", Content: "```\nescape attempt\n```"}

	c := firstContainer(t, buildInteractionRequestBlocks(p, "s"))
	var found bool
	for _, ch := range c.ChildBlocks {
		if sec, ok := ch.(*slackapi.SectionBlock); ok && sec.Text != nil {
			if assert.ObjectsAreEqual(true, len(sec.Text.Text) > 0) && contains(sec.Text.Text, "escape attempt") {
				found = true
				assert.NotContains(t, sec.Text.Text, "<!channel>")
			}
		}
	}
	assert.True(t, found, "excerpt content must be rendered somewhere, fenced")
}

// A prompt renders in the same shape as a notice: one container, tone chip,
// buttons inside.
func TestBuildInteractionRequestBlocks_PromptSharesTheNoticeShape(t *testing.T) {
	channelinteractions.Reset()
	t.Cleanup(channelinteractions.Reset)
	channelinteractions.Register(channelinteractions.Category{
		Name: "tool_approval", Deciders: channelinteractions.DecideOwner,
		Tone:      channelinteractions.ToneRoutine,
		Resurface: channelinteractions.ResurfaceNone,
	})
	p := noticeReq("tool_approval")
	p.Actions = []channelevents.InteractionAction{
		{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision},
	}
	blocks := buildInteractionRequestBlocks(p, "s")

	// A prompt renders in the SAME container shape a notice does. A reader
	// scanning a thread should not have to learn two visual languages; what
	// distinguishes a prompt is that it has buttons, which is visible without
	// a second idiom.
	c := firstContainer(t, blocks)
	require.NoError(t, c.validate())
	els := titleElements(t, c)
	_, isEmoji := els[0].(*slackapi.RichTextSectionEmojiElement)
	assert.True(t, isEmoji, "a prompt leads with its tone chip, like a notice")
	assert.NotNil(t, interactionActionBlock(t, blocks), "and carries its buttons inside the container")
}

// The degrade target must carry the same facts, since it is what a user sees
// if Slack ever rejects the container.
func TestBuildNoticeFallbackBlocks_CarriesLeadNextStepAndSeverity(t *testing.T) {
	p := noticeReq("demo_notice")
	blocks := buildNoticeFallbackBlocks(p, channelinteractions.ToneCritical, true, "s")

	require.NotEmpty(t, blocks)
	sec, ok := blocks[0].(*slackapi.SectionBlock)
	require.True(t, ok)
	assert.Contains(t, sec.Text.Text, ":red_circle:", "severity survives the degrade")
	assert.Contains(t, sec.Text.Text, "*Agent stopped*")
	assert.Contains(t, sec.Text.Text, "*Start a new thread to try again.*")
	for _, b := range blocks {
		_, isContainer := b.(containerBlock)
		assert.False(t, isContainer, "the fallback must use only long-supported blocks")
	}
}

// The push-notification preview has no blocks to lean on, so it must stand
// alone and stay actionable.
func TestNoticeNotifyText(t *testing.T) {
	assert.Equal(t, "Agent stopped — Start a new thread to try again.",
		noticeNotifyText(noticeReq("demo_notice")))

	p := noticeReq("demo_notice")
	p.NextStep = ""
	assert.Equal(t, "Agent stopped", noticeNotifyText(p))
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// interactionChildren returns the child blocks of the single container an
// interaction renders as.
//
// Prompts and notices share one shape, so a test that wants the section or the
// action row reaches through the container rather than indexing the top level.
func interactionChildren(t *testing.T, blocks []slackapi.Block) []slackapi.Block {
	t.Helper()
	require.Len(t, blocks, 1, "an interaction renders as a single container block")
	c, ok := blocks[0].(containerBlock)
	require.True(t, ok, "expected containerBlock, got %T", blocks[0])
	return c.ChildBlocks
}

// interactionActionBlock returns the container's action row, or nil when the
// interaction has no buttons.
func interactionActionBlock(t *testing.T, blocks []slackapi.Block) *slackapi.ActionBlock {
	t.Helper()
	for _, ch := range interactionChildren(t, blocks) {
		if ab, ok := ch.(*slackapi.ActionBlock); ok {
			return ab
		}
	}
	return nil
}

// interactionText concatenates every section body in the container, for tests
// asserting that some copy reached the user without caring which block holds it.
func interactionText(t *testing.T, blocks []slackapi.Block) string {
	t.Helper()
	var b strings.Builder
	for _, ch := range interactionChildren(t, blocks) {
		if sec, ok := ch.(*slackapi.SectionBlock); ok && sec.Text != nil {
			b.WriteString(sec.Text.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}
