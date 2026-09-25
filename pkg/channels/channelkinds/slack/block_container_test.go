package slack

import (
	"encoding/json"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func demoTitle() *slackapi.RichTextBlock {
	return noticeTitle("large_red_square", "Agent stopped")
}

func demoSection() slackapi.Block {
	return slackapi.NewSectionBlock(
		slackapi.NewTextBlockObject(slackapi.MarkdownType, "body", false, false), nil, nil)
}

// Slack rejects `has_header_divider` unless `is_collapsible` is false, and
// fails the ENTIRE chat.postMessage with invalid_blocks when both are set.
// The constructors own that pair so the combination cannot be written; this
// pins that, because the failure mode is losing a whole message — including
// the "your session died" ones.
func TestContainerBlock_ConstructorsCannotProduceTheRejectedCombination(t *testing.T) {
	notice := newContainerBlock(demoTitle(), demoSection())
	assert.True(t, notice.HasHeaderDivider)
	assert.False(t, notice.IsCollapsible)
	assert.NoError(t, notice.validate())

	collapsible := newCollapsibleContainerBlock(demoTitle(), true, demoSection())
	assert.True(t, collapsible.IsCollapsible)
	assert.False(t, collapsible.HasHeaderDivider, "a collapsible container must not ask for a header divider")
	assert.NoError(t, collapsible.validate())
}

func TestContainerBlock_Validate(t *testing.T) {
	cases := []struct {
		name    string
		block   containerBlock
		wantErr string
	}{
		{
			name:  "notice shape: valid",
			block: newContainerBlock(demoTitle(), demoSection()),
		},
		{
			name: "collapsible AND header divider: rejected (Slack fails the whole message)",
			block: containerBlock{
				Type: mbtContainer, RichTextTitle: demoTitle(),
				ChildBlocks:   []slackapi.Block{demoSection()},
				IsCollapsible: true, HasHeaderDivider: true,
			},
			wantErr: "has_header_divider",
		},
		{
			name: "no title of either kind: rejected",
			block: containerBlock{
				Type: mbtContainer, ChildBlocks: []slackapi.Block{demoSection()},
			},
			wantErr: "title",
		},
		{
			name: "no children: rejected",
			block: containerBlock{
				Type: mbtContainer, RichTextTitle: demoTitle(),
			},
			wantErr: "at least one child",
		},
		{
			name: "more than ten children: rejected",
			block: func() containerBlock {
				kids := make([]slackapi.Block, 0, containerMaxChildren+1)
				for i := 0; i <= containerMaxChildren; i++ {
					kids = append(kids, demoSection())
				}
				return newContainerBlock(demoTitle(), kids...)
			}(),
			wantErr: "exceeds Slack's cap",
		},
		{
			name: "a nested container child: rejected (containers do not nest)",
			block: newContainerBlock(demoTitle(),
				newContainerBlock(demoTitle(), demoSection())),
			wantErr: "not permitted inside a container",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.block.validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// The wire shape is what Slack actually parses, so pin the JSON: the field
// names and the discriminator are the whole contract for a hand-rolled block.
func TestContainerBlock_MarshalsToTheVerifiedWireShape(t *testing.T) {
	b := newContainerBlock(demoTitle(),
		demoSection(),
		slackapi.NewDividerBlock(),
		slackapi.NewContextBlock("", slackapi.NewTextBlockObject(
			slackapi.MarkdownType, "session `agents/sre-bot-4f2c`", false, false)),
	)
	raw, err := json.Marshal(b)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))

	assert.Equal(t, "container", got["type"])
	assert.Equal(t, true, got["has_header_divider"])
	assert.NotContains(t, got, "is_collapsible", "false is omitted, which is what Slack wants here")
	require.Contains(t, got, "rich_text_title")
	require.Contains(t, got, "child_blocks")
	assert.Len(t, got["child_blocks"], 3)

	// The chip must serialise as an emoji ELEMENT. A literal unicode square in
	// a text element is normalised by Slack to `:large_red_square:` and then
	// printed verbatim in a container header — the exact defect this shape
	// works around.
	title := got["rich_text_title"].(map[string]any)
	elements := title["elements"].([]any)
	section := elements[0].(map[string]any)
	first := section["elements"].([]any)[0].(map[string]any)
	assert.Equal(t, "emoji", first["type"])
	assert.Equal(t, "large_red_square", first["name"])
}

func TestContainerBlock_SatisfiesTheSlackBlockInterface(t *testing.T) {
	var b slackapi.Block = newContainerBlock(demoTitle(), demoSection())
	assert.Equal(t, mbtContainer, b.BlockType())
	assert.Empty(t, b.ID())
}
