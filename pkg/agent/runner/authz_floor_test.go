package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
)

// AdvanceAuthzFloor stamps the per-session ZedToken cache for every resource a
// grant write touched, so the next ToolCallAuthz check reads at-least-as-fresh
// as the write and sees the just-granted slot. This is the runner half of the
// plan-gate amendment race fix.
func TestAdvanceAuthzFloor_StampsCachePerGrantResource(t *testing.T) {
	cache := toolcheck.NewZedTokenCache()
	l := &Loop{AuthzCache: cache}

	l.AdvanceAuthzFloor([]authz.Relation{
		{ResourceType: "workshop_draft", ResourceID: "draft", Relation: "slot_grant_remove"},
		{ResourceType: "git_repo", ResourceID: "workspace", Relation: "slot_grant_write"},
	}, "zt-after-grant")

	assert.Equal(t, "zt-after-grant", cache.Get("workshop_draft", "draft"), "floor advanced for the amended resource")
	assert.Equal(t, "zt-after-grant", cache.Get("git_repo", "workspace"))
	assert.Equal(t, "zt-after-grant", cache.Latest())
}

func TestAdvanceAuthzFloor_NoOpOnEmptyTokenOrNilCache(t *testing.T) {
	cache := toolcheck.NewZedTokenCache()
	(&Loop{AuthzCache: cache}).AdvanceAuthzFloor(
		[]authz.Relation{{ResourceType: "workshop_draft", ResourceID: "draft"}}, "")
	assert.Equal(t, "", cache.Get("workshop_draft", "draft"), "an empty token advances nothing")

	// A nil cache (test/e2e wiring may leave it unset) must never panic.
	(&Loop{AuthzCache: nil}).AdvanceAuthzFloor(
		[]authz.Relation{{ResourceType: "x", ResourceID: "y"}}, "zt")
}
