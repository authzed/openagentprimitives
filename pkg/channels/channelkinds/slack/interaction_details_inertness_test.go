// pkg/channels/channelkinds/slack/interaction_details_inertness_test.go
//
// Markup inertness for the Show-Details MODAL, the third surface in the same
// family as interaction_inertness_test.go (the pending card) and
// interaction_applied_test.go (the resolved card).
//
// The modal is the highest-stakes of the three and was the last one open. It is
// the ground-truth view an approver opens BEFORE clicking Approve, and it
// renders the SAME strings the card already neutralises: tool_approval's
// justification is both a Field.Value on the card (escaped by
// escapePublisherPayload) and ToolApprovalDetails.Justification here. One
// escaped rendering beside one live one, for the same payload, is the shape
// inert.go names as worse than either alone.
//
// Neither of the two slots is publisher copy. Justification is the summarizer
// LLM's prose and ToolDescription is an UPSTREAM MCP server's tool description
// (both filled in pkg/agent/runner/host_approval.go's ToolApprovalDetails), and
// ArgsJSON is the model's own tool-call arguments.
package slack

import (
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// slackTextObjectLimit is Slack's own limit on a single mrkdwn text object,
// stated here rather than read from the production code on purpose: a test that
// tracked whatever budget the renderer picked could not tell whether that
// budget was right.
const slackTextObjectLimit = 3000

// detailsModalText renders the modal for d and concatenates every block's
// text — section bodies and the footer context alike, since all of them are
// mrkdwn the approver reads.
func detailsModalText(t *testing.T, d channelevents.ToolApprovalDetails) string {
	t.Helper()
	return concatBlockText(renderInteractionDetailsModal(d, "req-details-inert", "default/sess-inert"))
}

// TestRenderInteractionDetailsModal_PublisherSlotsAreInert covers every slot
// the modal renders into a mrkdwn surface. Each row puts the forged action in
// exactly one slot, so dropping any one field from the sweep fails exactly one
// row.
func TestRenderInteractionDetailsModal_PublisherSlotsAreInert(t *testing.T) {
	cases := []struct {
		name    string
		details channelevents.ToolApprovalDetails
	}{
		{
			name: "an upstream MCP tool description cannot forge a link",
			details: channelevents.ToolApprovalDetails{
				ToolDescription: "Creates an issue. " + lureLink,
			},
		},
		{
			name: "the summarizer's justification cannot forge a link — the card already makes this string inert",
			details: channelevents.ToolApprovalDetails{
				Justification: "The agent needs this because " + lureLink,
			},
		},
		{
			name:    "resourceType cannot forge a link",
			details: channelevents.ToolApprovalDetails{ResourceType: "repo " + lureLink},
		},
		{
			name:    "resourceID cannot forge a link",
			details: channelevents.ToolApprovalDetails{ResourceID: "org/x " + lureLink},
		},
		{
			name:    "permission cannot forge a link",
			details: channelevents.ToolApprovalDetails{Permission: "repo_write " + lureLink},
		},
		{
			name: "stateImpact cannot forge a link in the footer",
			details: channelevents.ToolApprovalDetails{
				Permission: "repo_write", StateImpact: "external " + lureLink,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detailsModalText(t, tc.details)
			assert.NotContains(t, got, lureLink,
				"a forged <url|label> must not render live in the modal an approver opens to decide")
			assert.Contains(t, got, lureLinkVisible,
				"inert is not deleted — the approver must still see what was attempted")
		})
	}
}

// TestRenderInteractionDetailsModal_ArgsCannotBreakTheFence is the args half.
// summarizeArgsForContext only trims and truncates, so a ``` inside a
// model-authored argument value CLOSES the fence at interaction_details.go's
// "```\n"+argsText+"\n```" and everything after it renders as live mrkdwn —
// a channel-wide ping, or a forged action line, in the approver's modal.
// inertExcerpt is what every other fenced untrusted region in this package
// already uses (interaction_notice.go's two Excerpt renderings).
func TestRenderInteractionDetailsModal_ArgsCannotBreakTheFence(t *testing.T) {
	got := detailsModalText(t, channelevents.ToolApprovalDetails{
		ArgsJSON: "{\"body\":\"x\"}\n```\n<!channel> " + lureLink,
	})

	assert.NotContains(t, got, "<!channel>",
		"an argument value that closes the fence must not open a channel-wide ping")
	assert.NotContains(t, got, lureLink, "...nor a forged platform action")
	assert.Contains(t, got, "&lt;!channel&gt;", "...and the attempt must stay visible, not be deleted")
	assert.Equal(t, 2, strings.Count(got, "```"),
		"exactly the renderer's own opening and closing fence survive")
}

// TestRenderInteractionDetailsModal_NoBlockExceedsSlacksSectionLimit is the
// other half of escapeToolApprovalDetails' own reasoning, which it applied to
// only three of its seven slots.
//
// The sweep runs before summarizeForSlackSection precisely because escaping
// AFTER a 2800-rune cap would let 2800 runes of "<" expand to 11200 characters
// and blow Slack's 3000-character limit, failing the whole modal with an opaque
// invalid_blocks. But ResourceType / ResourceID / Permission / StateImpact /
// ArgsHash were never capped at all — and ResourceID is bound from the MODEL's
// own tool-call arguments and is length-unbounded, so the agent decided, per
// request, whether the approver could open the ground-truth view before
// clicking Approve. Denial of the review step is the interesting half: an
// approver who cannot open Details is an approver deciding blind.
func TestRenderInteractionDetailsModal_NoBlockExceedsSlacksSectionLimit(t *testing.T) {
	// ~750 "<" is enough on its own: each becomes "&lt;", so the Resource
	// section alone renders past 3000 characters.
	long := strings.Repeat("<", 900)

	cases := []struct {
		name    string
		details channelevents.ToolApprovalDetails
	}{
		{
			name:    "a model-bound resourceID cannot overflow the Resource section",
			details: channelevents.ToolApprovalDetails{ResourceType: "repo", ResourceID: long, Permission: "write"},
		},
		{
			name:    "nor a resourceType",
			details: channelevents.ToolApprovalDetails{ResourceType: long, ResourceID: "org/x", Permission: "write"},
		},
		{
			name:    "nor a permission",
			details: channelevents.ToolApprovalDetails{ResourceType: "repo", ResourceID: "org/x", Permission: long},
		},
		{
			name:    "nor a stateImpact in the footer",
			details: channelevents.ToolApprovalDetails{StateImpact: long},
		},
		{
			name:    "nor an args hash in the footer",
			details: channelevents.ToolApprovalDetails{ArgsHash: long},
		},
		{
			name: "nor all of them at once",
			details: channelevents.ToolApprovalDetails{
				ResourceType: long, ResourceID: long, Permission: long,
				StateImpact: long, ArgsHash: long,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks := renderInteractionDetailsModal(tc.details, "req-1", "default/sess-1")
			require.NotEmpty(t, blocks, "the modal must still render")
			for i, text := range blockTexts(blocks) {
				assert.LessOrEqual(t, len([]rune(text)), slackTextObjectLimit,
					"block %d is %d runes: Slack rejects the whole view with invalid_blocks above %d",
					i, len([]rune(text)), slackTextObjectLimit)
			}
		})
	}
}

// A cap is only correct if it leaves the short, ordinary values alone — an
// approver reads these to decide, and a truncated resource id is a worse
// ground-truth view than a long one.
func TestRenderInteractionDetailsModal_OrdinaryFieldsAreNotTruncated(t *testing.T) {
	got := detailsModalText(t, channelevents.ToolApprovalDetails{
		ResourceType: "repository", ResourceID: "acme/widgets", Permission: "contents_write",
		StateImpact: "external", ArgsHash: "sha256:0f1e2d3c4b5a69788796a5b4c3d2e1f0",
	})
	for _, want := range []string{
		"repository", "acme/widgets", "contents_write", "external",
		"sha256:0f1e2d3c4b5a69788796a5b4c3d2e1f0",
	} {
		assert.Contains(t, got, want, "an ordinary value must render whole")
	}
	assert.NotContains(t, got, "…", "nothing short enough to fit should be marked truncated")
}

// blockTexts returns each block's mrkdwn text separately — the unit Slack
// actually limits, which concatBlockText deliberately loses.
func blockTexts(blocks []slackapi.Block) []string {
	var out []string
	for _, b := range blocks {
		switch v := b.(type) {
		case *slackapi.SectionBlock:
			if v.Text != nil {
				out = append(out, v.Text.Text)
			}
		case *slackapi.ContextBlock:
			for _, e := range v.ContextElements.Elements {
				if t, ok := e.(*slackapi.TextBlockObject); ok {
					out = append(out, t.Text)
				}
			}
		}
	}
	return out
}

// The args fence must still carry the args a reader is gating on: escaping is
// not an excuse to mangle ordinary JSON. Quotes, braces and backslashes are not
// Slack-active and must survive byte-for-byte.
func TestRenderInteractionDetailsModal_OrdinaryArgsRenderVerbatim(t *testing.T) {
	got := detailsModalText(t, channelevents.ToolApprovalDetails{
		ArgsJSON: `{"title":"hello","path":"a/b.go","re":"^x\\d+$"}`,
	})
	assert.Contains(t, got, `{"title":"hello","path":"a/b.go","re":"^x\\d+$"}`,
		"ordinary args JSON must render unchanged")
}
