package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The JIT tool-approval card must not print wire identifiers.
//
// It rendered "Tool: `gitlike_git`" and "Permission: `push`" — the internal
// tool id and the SpiceDB permission name — on a card a person decides from.
// The plan-gate card beside it resolves the same permission to its declared
// title ("Push commits to the remote repository") and the same resource to its
// display name, so one approval surface spoke English and the other spoke
// schema, for the same underlying call.
func TestToolApprovalFields_UseHumanCopyNotWireNames(t *testing.T) {
	fields := toolApprovalFields(toolApprovalCopy{
		ToolName:        "gitlike_git",
		ToolDescription: "Run git against the session's checkout.",
		Permission:      "push",
		ResourceType:    "git_repo",
		PermissionTitle: "Push commits to the remote repository",
		Justification:   "the user asked me to push",
		What:            "Push the fix branch.",
	})

	by := map[string]string{}
	for _, f := range fields {
		by[f.Label] = f.Value
	}

	assert.Equal(t, "Push commits to the remote repository", by["Permission"],
		"the declared title is what a human reads, exactly as the plan-gate card shows it")
	assert.NotContains(t, by["Tool"], "gitlike_git",
		"the internal tool id has no business on an approval surface")
	assert.Equal(t, "gitlike git", by["Tool"],
		"the Tool row is the humanized tool NAME, not the tool's own description")
	for label, v := range by {
		assert.NotContains(t, v, "`", "no backtick-wrapped wire values remain (%s)", label)
	}
}

// The Tool row must stay a concise identity even when the tool carries a
// long, multi-sentence LLM-facing description — that description is
// authoring prose for a model, not a one-line summary for a human approver,
// and the Why/What/Permission rows already say what the call does and why.
// A desktop review caught this: the card was dumping the entire description
// (sometimes several sentences) into the Tool row.
func TestToolApprovalFields_ToolRowIgnoresLongDescription(t *testing.T) {
	longDescription := "Type text into a focused element on the current page. " +
		"Use this when a form field, search box, or editable area is already " +
		"focused and you need to enter text; it does not click or focus the " +
		"element for you, so call web_click first if nothing is focused yet. " +
		"Supports an optional delay between keystrokes to mimic human typing " +
		"and avoid tripping bot-detection heuristics on the target page."

	fields := toolApprovalFields(toolApprovalCopy{
		ToolName:        "web_type",
		ToolDescription: longDescription,
		Permission:      "mutate",
		ResourceType:    "web_action",
		Justification:   "filling in the search box",
		What:            "Type the search query.",
	})

	by := map[string]string{}
	for _, f := range fields {
		by[f.Label] = f.Value
	}

	assert.Equal(t, "web type", by["Tool"],
		"the Tool row is the humanized name, never the tool's own description")
	assert.NotContains(t, by["Tool"], "Supports an optional delay",
		"the full description must not leak into the Tool row")
	assert.Less(t, len(by["Tool"]), len(longDescription),
		"the Tool row must be far shorter than the tool's full description")
}

// With no declared title, the permission still must not appear raw — it is
// detokenized against the resource type the way the plan-gate card's fallback
// does, so an undeclared title degrades to plain English rather than schema.
func TestToolApprovalFields_UndeclaredTitleDegradesToEnglish(t *testing.T) {
	fields := toolApprovalFields(toolApprovalCopy{
		ToolName:     "centerdot_update",
		Permission:   "update",
		ResourceType: "crm_company",
		What:         "Update the record.",
	})

	by := map[string]string{}
	for _, f := range fields {
		by[f.Label] = f.Value
	}
	assert.NotEqual(t, "update", by["Permission"], "the bare permission name is a wire value")
	assert.Contains(t, by["Permission"], "update")
	assert.Contains(t, by["Permission"], "crm company",
		"detokenized: the type reads as words, not as an identifier")
}

// A tool with no description falls back to a humanized form of its name rather
// than the raw id — an underscored identifier is still an identifier.
func TestToolApprovalFields_NoDescriptionHumanizesTheName(t *testing.T) {
	fields := toolApprovalFields(toolApprovalCopy{
		ToolName: "gitlike_git", Permission: "push", ResourceType: "git_repo", What: "x",
	})
	by := map[string]string{}
	for _, f := range fields {
		by[f.Label] = f.Value
	}
	require.NotEmpty(t, by["Tool"])
	assert.NotContains(t, by["Tool"], "_", "underscores are wire spelling")
	assert.NotContains(t, by["Tool"], "`")
}

// The deterministic What fallback (no summarizer) must not print the escaped
// object id either — that is the form SpiceDB stores, not one anybody reads.
func TestWhatLine_FallbackAvoidsWireIdentifiers(t *testing.T) {
	got := whatLine("", toolApprovalCopy{
		Permission:      "push",
		ResourceType:    "git_repo",
		ResourceID:      "https=3A//github=2Ecom/demo-org/demo-repo",
		PermissionTitle: "Push commits to the remote repository",
		ResourceLabel:   "demo-org/demo-repo",
	})

	assert.Contains(t, got, "Push commits to the remote repository")
	assert.Contains(t, got, "demo-org/demo-repo")
	assert.NotContains(t, got, "=3A", "the escaped id is storage spelling")
	assert.NotContains(t, got, "git_repo", "and the wire type name is an identifier")
}
