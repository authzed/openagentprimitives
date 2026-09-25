package runner

import "github.com/authzed/openagentprimitives/pkg/authz"

// AdvanceAuthzFloor stamps the per-session ZedToken cache for every resource a
// grant write just touched, using that write's WrittenAt token, so the next
// ToolCallAuthz check reads at-least-as-fresh as the grant and sees it.
//
// Wired as the SlotBinder's floor hook (internal/cmd/runner). The race it
// closes: a plan-gate approval (or amendment) writes a session-only slot grant
// SYNCHRONOUSLY, in-process, and the very next dispatch is the check the grant
// exists to authorize. That check floats on this cache
// (toolcheck.check_tool_call): a WARM cache already holds a token from an
// earlier check on the same resource, taken BEFORE the grant, so an
// AtLeastAsFresh read on it can be served a snapshot that predates the grant and
// deny the call. (The cold-cache case already reads FullyConsistent; this is the
// warm-cache gap.) Advancing the floor to the grant's own token removes the
// race at any SpiceDB version.
//
// Keyed by each written relation's RESOURCE (type/id) — the same key the check
// looks the floor up under (Cache.Get(check.ResourceType, resolvedID)); a slot
// grant's resource is the slot instance itself (e.g. workshop_draft:draft).
// Set also advances Latest(), the fallback floor for resources with no
// per-resource token, so a check on any resource benefits. No-op on a nil cache
// (test/e2e wiring may leave it unset) or an empty token.
func (l *Loop) AdvanceAuthzFloor(rels []authz.Relation, token string) {
	if l == nil || l.AuthzCache == nil || token == "" {
		return
	}
	for _, r := range rels {
		l.AuthzCache.Set(r.ResourceType, r.ResourceID, token)
	}
}
