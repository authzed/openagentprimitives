// pkg/channels/channelkinds/slack/block_container.go
//
// The Slack "container" block, hand-rolled.
//
// Slack shipped it in June 2026 but slack-go does not carry it — checked
// through v0.27.0 (latest), whose MessageBlockType constants stop at alert /
// card / carousel. slackapi.Block is just BlockType() + ID() over a
// JSON-marshalled struct, so a local implementation is enough and drops out
// the moment upstream adds one.
//
// Why the container and not a coloured attachment: an attachment is the only
// way to get a severity COLOUR in a Slack message, but every attachment also
// carries an "Added by <app>" attribution footer, cannot hold a container
// (Slack rejects it: "Unsupported block type: container"), and cannot combine
// `footer` with `blocks` (invalid_keys). Top-level blocks carry no attribution
// line.
package slack

import (
	"fmt"

	slackapi "github.com/slack-go/slack"
)

// mbtContainer is the block type discriminator. Not in slackapi's
// MessageBlockType constant set; the type is a string, so this composes.
const mbtContainer slackapi.MessageBlockType = "container"

// containerWidth controls the rendered width. Verified: narrow renders at
// roughly 60% and the others are accepted.
type containerWidth string

const (
	containerWidthStandard containerWidth = "" // Slack's default
	containerWidthNarrow   containerWidth = "narrow"
)

// containerBlock groups child blocks under a header. Hand-rolled because
// slack-go does not carry the `container` block.
//
// Two undocumented Slack constraints shape it (see NOTES.md — both were found
// by posting, not by reading the reference):
//
//  1. `has_header_divider` may only be set when `is_collapsible` is false;
//     setting both fails the whole message with invalid_blocks. Made
//     unrepresentable by construction — newContainerBlock owns the pair and no
//     caller can set them directly.
//  2. `title` does NOT render emoji (it prints `:large_red_square:` literally,
//     even with emoji:true), and `subtitle` renders emoji but drops mrkdwn code
//     spans. Hence rich_text_title for glyphs, and provenance in a trailing
//     context child rather than in subtitle.
type containerBlock struct {
	// Type is always the "container" block type; set by the constructors.
	Type slackapi.MessageBlockType `json:"type"`
	// BlockID is the caller's handle for a later chat.update; omitted when unset.
	BlockID string `json:"block_id,omitempty"`

	// RichTextTitle is the only title form that renders an emoji element.
	RichTextTitle *slackapi.RichTextBlock `json:"rich_text_title,omitempty"`
	// Title is the plain_text alternative. Mutually exclusive with
	// RichTextTitle in practice; kept for callers with no glyph to draw.
	Title *slackapi.TextBlockObject `json:"title,omitempty"`
	// Subtitle renders emoji but not code spans — avoid it for anything
	// containing markup.
	Subtitle *slackapi.TextBlockObject `json:"subtitle,omitempty"`

	// ChildBlocks is the container's body; Slack allows at most 10 and no
	// nested container. Always emitted, even empty, since Slack requires the key.
	ChildBlocks []slackapi.Block `json:"child_blocks"`

	// Width picks the container's rendered width; empty means Slack's default.
	Width containerWidth `json:"width,omitempty"`
	// IsCollapsible lets the reader fold the container; forces
	// HasHeaderDivider false (constraint 1 above).
	IsCollapsible bool `json:"is_collapsible,omitempty"`
	// DefaultCollapsed starts a collapsible container folded; ignored otherwise.
	DefaultCollapsed bool `json:"default_collapsed,omitempty"`
	// HasHeaderDivider draws a rule under the title; valid only when
	// IsCollapsible is false.
	HasHeaderDivider bool `json:"has_header_divider,omitempty"`
}

// BlockType satisfies slackapi.Block.
func (c containerBlock) BlockType() slackapi.MessageBlockType { return c.Type }

// ID satisfies slackapi.Block.
func (c containerBlock) ID() string { return c.BlockID }

// containerMaxChildren is Slack's documented cap on child_blocks.
const containerMaxChildren = 10

// newContainerBlock builds a non-collapsible container with a header divider —
// the notice shape.
//
// It owns the collapsible/divider pair rather than exposing both, so the
// mutually-exclusive combination that fails the entire chat.postMessage cannot
// be expressed. A caller wanting a collapsible container uses
// newCollapsibleContainerBlock, which sets no divider.
func newContainerBlock(title *slackapi.RichTextBlock, children ...slackapi.Block) containerBlock {
	return containerBlock{
		Type:             mbtContainer,
		RichTextTitle:    title,
		ChildBlocks:      children,
		HasHeaderDivider: true,
		IsCollapsible:    false,
	}
}

// newCollapsibleContainerBlock builds a collapsible container. It sets NO
// header divider — Slack rejects the combination (see containerBlock).
func newCollapsibleContainerBlock(title *slackapi.RichTextBlock, collapsed bool, children ...slackapi.Block) containerBlock {
	return containerBlock{
		Type:             mbtContainer,
		RichTextTitle:    title,
		ChildBlocks:      children,
		IsCollapsible:    true,
		DefaultCollapsed: collapsed,
		HasHeaderDivider: false,
	}
}

// validate reports whether the block is one Slack will accept. It exists so
// the invariants are assertable in a unit test without a live workspace —
// every rule here corresponds to a rejection observed from the real API.
func (c containerBlock) validate() error {
	if c.Type != mbtContainer {
		return fmt.Errorf("container block: type must be %q, got %q", mbtContainer, c.Type)
	}
	if c.IsCollapsible && c.HasHeaderDivider {
		return fmt.Errorf("container block: has_header_divider can only be set when is_collapsible is false")
	}
	if c.RichTextTitle == nil && c.Title == nil {
		return fmt.Errorf("container block: needs a title or a rich_text_title")
	}
	if len(c.ChildBlocks) == 0 {
		return fmt.Errorf("container block: needs at least one child block")
	}
	if len(c.ChildBlocks) > containerMaxChildren {
		return fmt.Errorf("container block: %d child blocks exceeds Slack's cap of %d",
			len(c.ChildBlocks), containerMaxChildren)
	}
	for i, ch := range c.ChildBlocks {
		if !containerAllowsChild(ch.BlockType()) {
			return fmt.Errorf("container block: child[%d] type %q is not permitted inside a container",
				i, ch.BlockType())
		}
	}
	return nil
}

// containerAllowedChildren is Slack's documented allowlist for child_blocks.
// Notably absent: alert (modal-only anyway), card, carousel, and container
// itself — a container cannot nest.
var containerAllowedChildren = map[slackapi.MessageBlockType]bool{
	slackapi.MBTAction:   true,
	slackapi.MBTContext:  true,
	slackapi.MBTDivider:  true,
	slackapi.MBTFile:     true,
	slackapi.MBTHeader:   true,
	slackapi.MBTImage:    true,
	slackapi.MBTInput:    true,
	slackapi.MBTRichText: true,
	slackapi.MBTSection:  true,
	slackapi.MBTTable:    true,
	slackapi.MBTVideo:    true,
}

func containerAllowsChild(t slackapi.MessageBlockType) bool { return containerAllowedChildren[t] }
