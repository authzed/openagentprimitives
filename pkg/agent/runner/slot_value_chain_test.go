package runner_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
)

// The slot's concrete resource crosses three hops on its way to the frozen
// plan — updatePlanArgSlot -> plans.PhaseSlot -> plangate.AuthoredSlot -> Slot —
// and every hop is a place a future edit can silently drop it.
//
// This is not hypothetical for THIS field. The comment on the projection records
// that slots were accepted by update_plan's schema from the start and "every
// projection dropped them, so a phase could request a slot and the frozen plan
// would never know". The same silence would swallow the value, and the symptom
// would be subtle: plans that look approved-in-advance but still prompt, because
// the frozen phase names no target.
//
// The value is authority — it enters the digest — so losing it does not fail
// loudly. It just widens what an approval covers.
func TestAuthoredPhasesFrom_carriesTheSlotValue(t *testing.T) {
	got := runner.AuthoredPhasesFrom([]plans.Phase{{
		ID: "read", Label: "Read", Why: "the user named this repo",
		Slots: []plans.PhaseSlot{
			{Type: "github_repo", ID: "foo/bar", Why: "the repo the user named"},
			{Type: "tracker_issue", Why: "I will know which once I look"},
		},
	}})

	require.Len(t, got, 1)
	require.Len(t, got[0].Slots, 2)

	assert.Equal(t, "foo/bar", got[0].Slots[0].ID,
		"a named target must survive the projection, or the plan cannot be pre-approved")
	assert.Empty(t, got[0].Slots[1].ID,
		"and an unnamed one must stay unnamed rather than acquiring a value")
	assert.Equal(t, "the repo the user named", got[0].Slots[0].Why,
		"the agent's reason rides along as display-only")
}

// TestSlotTransformsOf_MatchesResolvedSlotsByResourceType pins the one place
// this repo derives the per-type transform-chain map, and is what stands in
// for a cmd/runner <-> pkg/e2e parity test.
//
// A true black-box parity test — build a Loop the way cmd/runner's run()
// does and the way the e2e InProcessRunnerFactory does, on the same
// AgentClass, and diff PlanGateSlotTransforms — is impractical here: cmd/
// runner's construction lives inline in run() (cmd/runner/main.go), which
// requires a live NATS connection, a SpiceDB client and a real Kubernetes
// client to reach, with no isolated "build a Loop from a class" function to
// call from a test. What makes that impracticality safe to accept is that
// runner.SlotTransformsOf is now the ONLY place either caller computes this
// map — cmd/runner/main.go and pkg/e2e/inprocess_runner_factory.go both call
// it directly rather than each hand-rolling their own copy (which is what
// let them drift before: the e2e copy carried a comment naming the exact
// failure it did not prevent). Given that, a focused correctness test on the
// single shared function, run once here, covers both call sites; the
// structural guarantee against drift is that there is only one
// implementation left to test.
func TestSlotTransformsOf_MatchesResolvedSlotsByResourceType(t *testing.T) {
	class := &spiceboxv1alpha1.AgentClass{
		Status: spiceboxv1alpha1.AgentClassStatus{
			ResolvedSlots: []spiceboxv1alpha1.ResolvedSlot{
				{ResourceType: "git_repo", Permission: "push", ValueTransforms: []string{"normalize_url", "spicedb_escape"}},
				{ResourceType: "tracker_issue", Permission: "read"}, // no chain: not value-keyed
			},
		},
	}

	got := runner.SlotTransformsOf(class)

	assert.Equal(t, map[string][]string{"git_repo": {"normalize_url", "spicedb_escape"}}, got,
		"a slot published with no ValueTransforms must be ABSENT from the map, not present with a nil/empty chain — "+
			"callers key on presence to decide whether a value is value-keyed at all")
}

// A class that publishes no resolved slots at all (never reconciled, or a
// class with no authz.slots) must produce an empty map, not a nil one that
// happens to work today only because every consumer ranges over it.
func TestSlotTransformsOf_NoResolvedSlots_ReturnsEmptyMap(t *testing.T) {
	got := runner.SlotTransformsOf(&spiceboxv1alpha1.AgentClass{})

	assert.NotNil(t, got, "an unpopulated status must still return a usable (non-nil) map")
	assert.Empty(t, got)
}

// TestSlotStandingOf_MatchesResolvedSlotsByResourceType mirrors
// TestSlotTransformsOf_MatchesResolvedSlotsByResourceType — same derivation
// shape (loop through ResolvedSlots, key by ResourceType), same reason it is
// centralized here rather than hand-rolled at each of the two Loop-construction
// call sites (cmd/runner/main.go, pkg/e2e/inprocess_runner_factory.go).
func TestSlotStandingOf_MatchesResolvedSlotsByResourceType(t *testing.T) {
	class := &spiceboxv1alpha1.AgentClass{
		Status: spiceboxv1alpha1.AgentClassStatus{
			ResolvedSlots: []spiceboxv1alpha1.ResolvedSlot{
				{ResourceType: "crm_company", Permission: "contact_access", Standing: spiceboxv1alpha1.StandingRequired},
				{ResourceType: "git_repo", Permission: "push", Standing: spiceboxv1alpha1.StandingSessionOnly},
				{ResourceType: "tracker_issue", Permission: "read"}, // no standing published
			},
		},
	}

	got := runner.SlotStandingOf(class)

	assert.Equal(t, map[string]string{
		"crm_company": spiceboxv1alpha1.StandingRequired,
		"git_repo":    spiceboxv1alpha1.StandingSessionOnly,
	}, got, "a slot published with no Standing must be ABSENT from the map — "+
		"approverCanDelegateSlots treats absence as session-only by reading a missing key, "+
		"not by finding an explicit empty-string entry")
}

// A class that publishes no resolved slots at all must produce an empty map,
// not a nil one — mirrors TestSlotTransformsOf_NoResolvedSlots_ReturnsEmptyMap.
func TestSlotStandingOf_NoResolvedSlots_ReturnsEmptyMap(t *testing.T) {
	got := runner.SlotStandingOf(&spiceboxv1alpha1.AgentClass{})

	assert.NotNil(t, got, "an unpopulated status must still return a usable (non-nil) map")
	assert.Empty(t, got)
}

// TestPermissionTitlesOf_MatchesResolvedPermissionTitlesByKey pins the one
// place this repo derives the card's title lookup, mirroring
// TestSlotTransformsOf_MatchesResolvedSlotsByResourceType — same reason it is
// centralized here rather than hand-rolled at either of the two
// Loop-construction call sites (cmd/runner/main.go,
// pkg/e2e/inprocess_runner_factory.go): a nil map at either site would make
// every declared title silently fall back to the detokenized handle.
func TestPermissionTitlesOf_MatchesResolvedPermissionTitlesByKey(t *testing.T) {
	class := &spiceboxv1alpha1.AgentClass{
		Status: spiceboxv1alpha1.AgentClassStatus{
			ResolvedPermissionTitles: []spiceboxv1alpha1.ResolvedPermissionTitle{
				{ResourceType: "git_repo", Permission: "push", Title: "Push commits to the repository"},
				{ResourceType: "tracker_issue", Permission: "read", Title: "Read an issue"},
			},
		},
	}

	got := runner.PermissionTitlesOf(class)

	assert.Equal(t, map[string]string{
		"git_repo/push":      "Push commits to the repository",
		"tracker_issue/read": "Read an issue",
	}, got, `keyed "<resourceType>/<permission>", matching CardInput.PermissionTitles and the card's own lookup`)
}

// A class that publishes no resolved permission titles at all must produce an
// empty map, not a nil one — mirrors
// TestSlotTransformsOf_NoResolvedSlots_ReturnsEmptyMap.
func TestPermissionTitlesOf_NoResolvedPermissionTitles_ReturnsEmptyMap(t *testing.T) {
	got := runner.PermissionTitlesOf(&spiceboxv1alpha1.AgentClass{})

	assert.NotNil(t, got, "an unpopulated status must still return a usable (non-nil) map")
	assert.Empty(t, got)
}

// TestResourceDisplaysOf_MatchesResolvedResourceDisplaysByType is the display
// sibling of TestPermissionTitlesOf_MatchesResolvedPermissionTitlesByKey — the
// one place this repo derives the card's per-type display lookup, so neither
// Loop-construction site (internal/cmd/runner/main.go,
// test/e2e/inprocess_runner_factory.go) can hand-roll a copy that drifts or
// forgets it, leaving every declared display silently falling back to the
// wire type name.
func TestResourceDisplaysOf_MatchesResolvedResourceDisplaysByType(t *testing.T) {
	class := &spiceboxv1alpha1.AgentClass{
		Status: spiceboxv1alpha1.AgentClassStatus{
			ResolvedResourceDisplays: []spiceboxv1alpha1.ResolvedResourceDisplay{
				{ResourceType: "git_repo", Name: "Git repository", Icon: "repository", Label: "url_path"},
				{ResourceType: "tracker_issue", Name: "Tracker issue"},
			},
		},
	}

	got := runner.ResourceDisplaysOf(class)

	assert.Equal(t, map[string]plangate.ResourceDisplay{
		"git_repo":      {Name: "Git repository", Icon: "repository", Label: "url_path"},
		"tracker_issue": {Name: "Tracker issue"},
	}, got, "keyed by resourceType, matching CardInput.ResourceDisplays and the card's own lookup")
}

// A class that publishes no resolved resource displays at all must produce an
// empty map, not a nil one — mirrors
// TestPermissionTitlesOf_NoResolvedPermissionTitles_ReturnsEmptyMap.
func TestResourceDisplaysOf_NoResolvedResourceDisplays_ReturnsEmptyMap(t *testing.T) {
	got := runner.ResourceDisplaysOf(&spiceboxv1alpha1.AgentClass{})

	assert.NotNil(t, got, "an unpopulated status must still return a usable (non-nil) map")
	assert.Empty(t, got)
}
