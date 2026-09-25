//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// "seen 0 prompt(s)" when a tool_approval prompt DID arrive and simply failed
// the predicates is the most expensive message in this harness: it sends the
// reader looking for a publication bug that is not there. ForResource's own doc
// already promises the timeout message is where a mismatch gets spotted — this
// is that message.
func TestDescribeApprovalCandidates_showsWhyEachPromptWasRejected(t *testing.T) {
	got := describeApprovalCandidates([]ApprovalPrompt{
		{Tool: "centerdot_list_contacts_for_company", ResourceType: "crm_company", ResourceID: "any", Permission: "contact_access"},
	})

	assert.Contains(t, got, "centerdot_list_contacts_for_company")
	assert.Contains(t, got, "crm_company:any",
		"the resource is the field that most often differs; it must be readable at a glance")
	assert.Contains(t, got, "contact_access")
}

// The distinction the count alone destroys. "No prompt arrived" and "a prompt
// arrived that you did not match" have completely different causes, and the
// message must not let them look alike.
func TestDescribeApprovalCandidates_saysNothingArrivedWhenNothingDid(t *testing.T) {
	got := describeApprovalCandidates(nil)
	assert.Contains(t, got, "none",
		"an empty candidate list must say so explicitly rather than render as blank")
}

func TestDescribeApprovalCandidates_listsEveryCandidate(t *testing.T) {
	got := describeApprovalCandidates([]ApprovalPrompt{
		{Tool: "a_tool", ResourceType: "t", ResourceID: "1"},
		{Tool: "b_tool", ResourceType: "t", ResourceID: "2"},
	})
	assert.Contains(t, got, "a_tool")
	assert.Contains(t, got, "b_tool", "a later candidate must not be truncated away")
}
