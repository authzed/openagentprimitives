package factcontent_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
)

func TestEntryID(t *testing.T) {
	// The id is what makes a fact write-once: two writes about the same
	// (subject, name) MUST land on the same entry so the append-only door
	// compares them, and two different ones MUST NOT collide.
	a := factcontent.EntryID("github_pr", "demo-org/demo-repo#6", "is_cross_repository")
	b := factcontent.EntryID("github_pr", "demo-org/demo-repo#6", "is_cross_repository")
	assert.Equal(t, a, b, "same subject + name must be the same entry")

	assert.NotEqual(t, a,
		factcontent.EntryID("github_pr", "demo-org/demo-repo#5", "is_cross_repository"),
		"a different PR is a different fact")
	assert.NotEqual(t, a,
		factcontent.EntryID("github_pr", "demo-org/demo-repo#6", "is_draft"),
		"a different name is a different fact")
	assert.NotEqual(t, a,
		factcontent.EntryID("git_commit", "demo-org/demo-repo#6", "is_cross_repository"),
		"a different resource type is a different fact")

	// Field-boundary confusion: ("ab","c") and ("a","bc") must not collide.
	assert.NotEqual(t,
		factcontent.EntryID("t", "ab", "c"),
		factcontent.EntryID("t", "a", "bc"),
		"concatenation without a separator would collide these")
}
