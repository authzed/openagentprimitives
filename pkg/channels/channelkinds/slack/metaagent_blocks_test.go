// pkg/channels/channelkinds/slack/metaagent_blocks_test.go
//
// Rendering tests for the metaagent scope-approval Block Kit, alongside the
// code they cover — Block Kit is the channel kind's business, not channelsd's.
package slack

import (
	"encoding/json"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
)

// ---------------------------------------------------------------------------
// F2: buildMetaagentScopeApprovalBlocks rendering
// ---------------------------------------------------------------------------

// TestBuildMetaagentScopeApprovalBlocks_ThreeRegions verifies that a full
// payload (verbatim, summary, skipped + caveat explains) renders all three
// regions plus the action block and footer.
func TestBuildMetaagentScopeApprovalBlocks_ThreeRegions(t *testing.T) {
	pl := scope.MetaagentApprovalPayload{
		RequestID:       "req-abc",
		Requester:       "U_ALICE",
		Verbatim:        "allow read on linear issues",
		ApproverSummary: "Alice wants to read Linear issues. This adds the 'linear:read' scope.",
		SkippedExplain:  "github:write was skipped because it's too broad.",
		CaveatExplain:   "linear:read only covers public projects.",
	}
	blocks := buildMetaagentScopeApprovalBlocks(pl, "default/sess1")

	// Flatten the block text for assertion.
	allText := extractBlockText(blocks)

	// Region 1: verbatim
	assert.Contains(t, allText, "<@U_ALICE>", "region 1 must mention requester")
	assert.Contains(t, allText, "allow read on linear issues", "region 1 must include verbatim text")

	// Region 2: approver summary
	assert.Contains(t, allText, "Alice wants to read Linear issues", "region 2 must include approver summary")
	assert.Contains(t, allText, "linear:read", "region 2 must include scope name")

	// Region 3: warnings
	assert.Contains(t, allText, "github:write was skipped", "region 3 must include skipped explain")
	assert.Contains(t, allText, "linear:read only covers public projects", "region 3 must include caveat explain")
	assert.Contains(t, allText, "⚠", "region 3 must have warning icon")

	// Buttons must be present.
	assert.True(t, hasActionBlock(blocks), "action block with Approve/Deny/Show Details must be present")
	assert.True(t, hasButton(blocks, "Approve"), "Approve button must be present")
	assert.True(t, hasButton(blocks, "Deny"), "Deny button must be present")
	assert.True(t, hasButton(blocks, "Show Details"), "Show Details button must be present")

	// Footer must include session ref and request id.
	assert.Contains(t, allText, "default/sess1", "footer must include session ref")
	assert.Contains(t, allText, "req-abc", "footer must include request id")
}

// TestBuildMetaagentScopeApprovalBlocks_TwoRegions verifies that when
// SkippedExplain and CaveatExplain are both empty, region 3 is omitted.
func TestBuildMetaagentScopeApprovalBlocks_TwoRegions(t *testing.T) {
	pl := scope.MetaagentApprovalPayload{
		RequestID:       "req-xyz",
		Requester:       "U_BOB",
		Verbatim:        "allow write on github",
		ApproverSummary: "Bob wants to write to GitHub. This adds the 'github:write' scope.",
		// SkippedExplain and CaveatExplain intentionally empty.
	}
	blocks := buildMetaagentScopeApprovalBlocks(pl, "ns2/sess2")

	allText := extractBlockText(blocks)

	// Region 1 present.
	assert.Contains(t, allText, "<@U_BOB>", "region 1 must mention requester")
	assert.Contains(t, allText, "allow write on github", "region 1 must include verbatim")

	// Region 2 present.
	assert.Contains(t, allText, "Bob wants to write to GitHub", "region 2 must include summary")

	// Region 3 must be absent.
	assert.NotContains(t, allText, "⚠", "region 3 must be absent when no warnings")
	assert.NotContains(t, allText, "Heads up", "region 3 must be absent when no warnings")

	// Buttons still present.
	assert.True(t, hasButton(blocks, "Approve"), "Approve button must be present")
	assert.True(t, hasButton(blocks, "Deny"), "Deny button must be present")
}

// TestBuildMetaagentScopeApprovalBlocks_ButtonValues verifies that Approve and
// Deny button values contain the correct "metaagent_approval" sentinel and that
// the action_ids use the requestId.
func TestBuildMetaagentScopeApprovalBlocks_ButtonValues(t *testing.T) {
	pl := scope.MetaagentApprovalPayload{
		RequestID: "req-123",
		Requester: "U_CAROL",
		Verbatim:  "some request",
	}
	blocks := buildMetaagentScopeApprovalBlocks(pl, "ns/sess")

	// The action block contains three buttons; check their action_ids and values.
	for _, block := range blocks {
		ab, ok := block.(*slackapi.ActionBlock)
		if !ok {
			continue
		}
		require.Len(t, ab.Elements.ElementSet, 3, "action block must have 3 buttons")
		for _, elem := range ab.Elements.ElementSet {
			btn, ok := elem.(*slackapi.ButtonBlockElement)
			require.True(t, ok, "action block element must be a button")
			// Value must be parseable JSON with v:"metaagent_approval".
			assert.Contains(t, btn.Value, "metaagent_approval",
				"button value must contain metaagent_approval sentinel")
			assert.Contains(t, btn.Value, "req-123",
				"button value must contain requestId")
			assert.Contains(t, btn.Value, "ns/sess",
				"button value must contain session ref")
		}
		// Action IDs must be prefixed "metaagent_".
		for _, elem := range ab.Elements.ElementSet {
			btn := elem.(*slackapi.ButtonBlockElement)
			assert.True(t, strings.HasPrefix(btn.ActionID, "metaagent_"),
				"action_id %q must have metaagent_ prefix", btn.ActionID)
		}
		break
	}
}

// TestBuildMetaagentScopeApprovalBlocks_ColdStart_CleanedTaskAndFiveButtons
// verifies a cold-start payload renders the cleaned-task region plus the five
// action buttons carrying the correct decision strings.
func TestBuildMetaagentScopeApprovalBlocks_ColdStart_CleanedTaskAndFiveButtons(t *testing.T) {
	pl := scope.MetaagentApprovalPayload{
		RequestID:       "req-cs",
		Requester:       "U_DANA",
		Verbatim:        "summarize L-140, don't read ENG, my SSN is 123",
		ApproverSummary: "Dana wants to summarize L-140 with a hard-deny on ENG.",
		ColdStart:       true,
		CleanedTask:     "summarize L-140",
	}
	blocks := buildMetaagentScopeApprovalBlocks(pl, "default/coldsess")
	allText := extractBlockText(blocks)

	// Verbatim request still shown, but WITHOUT the requester mention — the
	// approver is the person who just started the session.
	assert.Contains(t, allText, "summarize L-140, don't read ENG", "cold-start must still show the verbatim request")
	assert.NotContains(t, allText, "<@U_DANA>", "cold-start must NOT prefix the requester mention")

	// Cleaned-task region present as a blockquote.
	assert.Contains(t, allText, "the agent will be asked to", "cold-start cleaned-task region must be present")
	assert.Contains(t, allText, "> summarize L-140", "cleaned task must render as a blockquote")

	// Five labelled buttons.
	for _, label := range []string{"Approve", "Approve w/ original", "Run w/o scope", "Deny", "Show Details"} {
		assert.True(t, hasButton(blocks, label), "cold-start button %q must be present", label)
	}

	// Decision strings carried in the button values.
	gotDecisions := buttonDecisions(t, blocks)
	assert.ElementsMatch(t,
		[]string{"approve_cleaned", "approve_original", "run_without_scope", "deny", "show_details"},
		gotDecisions, "cold-start buttons must carry the five decision strings")
}

// TestBuildMetaagentScopeApprovalBlocks_ColdStart_EmptyTask verifies the "no
// task" line renders when CleanedTask is empty.
func TestBuildMetaagentScopeApprovalBlocks_ColdStart_EmptyTask(t *testing.T) {
	pl := scope.MetaagentApprovalPayload{
		RequestID:       "req-cs2",
		Requester:       "U_ERIN",
		Verbatim:        "allow read on linear",
		ApproverSummary: "Erin wants read on linear.",
		ColdStart:       true,
		CleanedTask:     "",
	}
	blocks := buildMetaagentScopeApprovalBlocks(pl, "default/coldsess2")
	allText := extractBlockText(blocks)
	assert.Contains(t, allText, "no task", "empty cleaned task must render the no-task line")
	assert.Contains(t, allText, "only sets permissions", "no-task line wording")
}

// TestBuildMetaagentScopeApprovalBlocks_NotColdStart_ThreeButtonsUnchanged
// verifies a non-cold-start payload still renders exactly three mid-session
// buttons (approve/deny/show_details) and no cleaned-task region.
func TestBuildMetaagentScopeApprovalBlocks_NotColdStart_ThreeButtonsUnchanged(t *testing.T) {
	pl := scope.MetaagentApprovalPayload{
		RequestID:       "req-mid",
		Requester:       "U_FRED",
		Verbatim:        "allow write on github",
		ApproverSummary: "Fred wants github write.",
		CleanedTask:     "should be ignored when not cold-start",
	}
	blocks := buildMetaagentScopeApprovalBlocks(pl, "default/midsess")
	allText := extractBlockText(blocks)

	assert.NotContains(t, allText, "the agent will be asked to", "non-cold-start must omit cleaned-task region")
	assert.NotContains(t, allText, "should be ignored", "CleanedTask must not render when not cold-start")

	gotDecisions := buttonDecisions(t, blocks)
	assert.ElementsMatch(t,
		[]string{"approve", "deny", "show_details"},
		gotDecisions, "non-cold-start must carry the three mid-session decisions")
}

// buttonDecisions extracts the "d" decision field from every button value JSON
// across all action blocks.
func buttonDecisions(t *testing.T, blocks []slackapi.Block) []string {
	t.Helper()
	var out []string
	for _, b := range blocks {
		ab, ok := b.(*slackapi.ActionBlock)
		if !ok {
			continue
		}
		for _, el := range ab.Elements.ElementSet {
			btn, ok := el.(*slackapi.ButtonBlockElement)
			require.True(t, ok, "action element must be a button")
			var v struct {
				D string `json:"d"`
			}
			require.NoError(t, json.Unmarshal([]byte(btn.Value), &v), "button value JSON")
			out = append(out, v.D)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// extractBlockText joins all text found in a block set for easy contains checks.
func extractBlockText(blocks []slackapi.Block) string {
	var sb strings.Builder
	for _, b := range blocks {
		switch blk := b.(type) {
		case *slackapi.SectionBlock:
			if blk.Text != nil {
				sb.WriteString(blk.Text.Text)
				sb.WriteString("\n")
			}
		case *slackapi.HeaderBlock:
			if blk.Text != nil {
				sb.WriteString(blk.Text.Text)
				sb.WriteString("\n")
			}
		case *slackapi.ContextBlock:
			for _, el := range blk.ContextElements.Elements {
				if t, ok := el.(*slackapi.TextBlockObject); ok {
					sb.WriteString(t.Text)
					sb.WriteString("\n")
				}
			}
		case *slackapi.ActionBlock:
			for _, el := range blk.Elements.ElementSet {
				if btn, ok := el.(*slackapi.ButtonBlockElement); ok {
					if btn.Text != nil {
						sb.WriteString(btn.Text.Text)
						sb.WriteString("\n")
					}
				}
			}
		}
	}
	return sb.String()
}

// hasActionBlock reports whether any ActionBlock exists in the slice.
func hasActionBlock(blocks []slackapi.Block) bool {
	for _, b := range blocks {
		if _, ok := b.(*slackapi.ActionBlock); ok {
			return true
		}
	}
	return false
}

// hasButton reports whether any button in any ActionBlock has the given label.
func hasButton(blocks []slackapi.Block, label string) bool {
	for _, b := range blocks {
		ab, ok := b.(*slackapi.ActionBlock)
		if !ok {
			continue
		}
		for _, el := range ab.Elements.ElementSet {
			if btn, ok := el.(*slackapi.ButtonBlockElement); ok {
				if btn.Text != nil && btn.Text.Text == label {
					return true
				}
			}
		}
	}
	return false
}
