package hooks

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

const shipRepo = "https://github.com/demo-org/demo-repo"

// shipSurface is the codebot shape: a read, a working read, and an
// irreversible push. The push is what makes the severity claim testable — a
// plan-scoped card authorizes it, so it must be priced as one.
func shipSurface(t *testing.T) []permsurface.Descriptor {
	t.Helper()
	return []permsurface.Descriptor{
		permDesc(t, "fetch", "git_repo", "git_fetch", authz.Readwrite),
		permDesc(t, "read", "git_repo", "git_read", authz.Readonly),
		permDesc(t, "push", "git_repo", "git_push", authz.External),
	}
}

// shipGate builds an enforcing gate over read → edit → ship. lastSlotID is the
// target the ship phase names; empty means it genuinely cannot say yet.
func shipGate(
	t *testing.T, rec *fakeRecorder, lastSlotID string, records []plangateaudit.Content,
) (*PlanGate, plangate.Plan) {
	t.Helper()
	surface := shipSurface(t)

	plan, probs := plangate.FreezeFrom([]plangate.AuthoredPhase{
		{ID: "recon", Label: "Read the repository", Why: "to see what needs fixing",
			Permissions: []plangate.AuthoredPermission{{Handle: "perm:fetch:git_repo", Why: "to read the remote"}},
			Slots:       []plangate.AuthoredSlot{{Type: "git_repo", ID: shipRepo, Why: "the repo the user named"}}},
		{ID: "edit", Label: "Fix the typos", Why: "apply the corrections",
			Permissions: []plangate.AuthoredPermission{{Handle: "perm:read:git_repo", Why: "to re-read as I edit"}},
			Slots:       []plangate.AuthoredSlot{{Type: "git_repo", ID: shipRepo, Why: "the same repo"}}},
		{ID: "ship", Label: "Open the pull request", Why: "deliver the change",
			Permissions: []plangate.AuthoredPermission{{Handle: "perm:push:git_repo", Why: "to push the branch"}},
			Slots:       []plangate.AuthoredSlot{{Type: "git_repo", ID: lastSlotID, Why: "where the PR goes"}}},
	}, surface, []string{"git_repo"})
	require.Empty(t, probs)

	h := NewPlanGate(PlanGateDeps{
		Mode:        "enforcing",
		Plan:        plangate.SessionPlan(surface),
		CurrentPlan: func() (plangate.Plan, bool) { return plan, true },
		Surface:     surface,
		Resolve:     surfaceResolver(t, surface),
		Records:     func() []plangateaudit.Content { return records },
		Recorder:    rec,
		Logger:      &fakeLogger{},
	})
	return h, plan
}

func selectedRec(p plangate.Plan, idx int32) plangateaudit.Content {
	return plangateaudit.Content{
		Event: plangateaudit.EventPhaseSelected, PlanDigest: p.Digest(), PhaseIndex: &idx,
	}
}

func coveredOf(t *testing.T, payload map[string]any) []plangateaudit.Content {
	t.Helper()
	c, ok := payload["covered"].([]plangateaudit.Content)
	require.True(t, ok, "the ask must carry the phases its answer clears; payload was %#v", payload)
	return c
}

func coveredKeys(c []plangateaudit.Content) []string {
	out := make([]string, 0, len(c))
	for _, p := range c {
		out = append(out, p.PhaseKey)
	}
	return out
}

// THE move: one card, the whole plan.
//
// Per-phase cards asked serially, so a codebot run cost two decisions — the
// clone is readwrite, the push is external — and the first card said nothing
// about the push coming two phases later. The human cleared "read-only recon"
// with no account of what they were starting.
func TestPlanScopedApproval_oneCardShowsAndClearsEveryPhase(t *testing.T) {
	rec := &fakeRecorder{}
	h, plan := shipGate(t, rec, shipRepo, nil)

	d := evalTool(t, h, "git_fetch")

	require.NotNil(t, d.Approval, "an unapproved plan must reach a human")
	card, ok := d.Approval.Payload["card"].(plangate.Card)
	require.True(t, ok, "the card is what the approver reads")

	for _, want := range []string{"Phase 1", "Phase 2", "Phase 3", shipRepo} {
		assert.Contains(t, card.What, want,
			"the card must show every phase and the resource each one touches")
	}
	assert.Contains(t, card.When, "whole plan",
		`"phase 1 of 3" would be a false statement about what is being decided`)

	assert.Equal(t,
		[]string{plan.Phases[0].AuthorityKey(), plan.Phases[1].AuthorityKey(), plan.Phases[2].AuthorityKey()},
		coveredKeys(coveredOf(t, d.Approval.Payload)),
		"one yes must clear every phase the card showed, or the card lied about what it bought")
}

// Each covered phase is recorded against ITS OWN authority, not the active
// phase's. A card that cleared three phases under one ceiling would read back,
// at the next carry-over, as "every phase was granted the push".
func TestPlanScopedApproval_eachCoveredPhaseCarriesItsOwnGrant(t *testing.T) {
	rec := &fakeRecorder{}
	h, plan := shipGate(t, rec, shipRepo, nil)

	d := evalTool(t, h, "git_fetch")
	require.NotNil(t, d.Approval)

	covered := coveredOf(t, d.Approval.Payload)
	require.Len(t, covered, 3)
	for i, want := range []string{"perm:fetch:git_repo", "perm:read:git_repo", "perm:push:git_repo"} {
		require.NotNil(t, covered[i].PhaseIndex)
		assert.Equal(t, int32(i), *covered[i].PhaseIndex,
			"a record naming the wrong phase grants the wrong reach")
		assert.Equal(t, []string{want}, covered[i].Ceiling)
		assert.Equal(t, []string{"git_repo"}, covered[i].Slots,
			"a phase whose slot request never reaches the ask records a grant narrower than the card showed")
		assert.Equal(t, plan.Phases[i].AuthorityKey(), covered[i].PhaseKey)
	}
}

// Priced at the WORST phase, not the first. Approving this authorizes the push,
// so a plan containing one is an external decision even though the phase that
// triggered the card is a read — and that being a read is the common case,
// which is exactly how this would otherwise have rendered routine.
func TestPlanScopedApproval_isPricedAtTheWorstPhaseNotTheFirst(t *testing.T) {
	rec := &fakeRecorder{}
	h, _ := shipGate(t, rec, shipRepo, nil)

	d := evalTool(t, h, "git_fetch")
	require.NotNil(t, d.Approval)

	card := d.Approval.Payload["card"].(plangate.Card)
	assert.NotEqual(t, string(plangate.Routine), card.Severity,
		"a plan whose last phase pushes cannot be a routine approval")
	assert.Equal(t, card.Severity, d.Approval.Payload["severity"],
		"the severity the channel styles on must be the one the card was priced at")
	assert.Equal(t, card.Severity, rec.last(t).Severity,
		"and the audit log must record the same price")
}

// ONE decision per PLAN. A turn's tool calls dispatch concurrently, so several
// can reach the same unapproved plan at once; keyed per phase, a plan-scoped
// card would publish once per phase index and the human would answer the same
// question twice.
func TestPlanScopedApproval_coalescesOnThePlanNotThePhase(t *testing.T) {
	rec := &fakeRecorder{}
	h, plan := shipGate(t, rec, shipRepo, nil)

	d := evalTool(t, h, "git_fetch")

	require.NotNil(t, d.Approval)
	assert.Equal(t, fmt.Sprintf("plan:%s", plan.Digest()), d.Approval.CoalesceKey)
}

// THE anti-loop invariant. An ask that does not clear the phase that raised it
// is answered and then raised again by the very next call, forever — the human
// clicks approve and nothing happens.
//
// It bites on the deferred phase specifically: a phase that names no target is
// deliberately excluded from a plan-scoped card's coverage, which is right for
// a phase two steps away and fatal for the one running now.
func TestPlanScopedApproval_alwaysClearsThePhaseThatRaisedIt(t *testing.T) {
	cases := []struct {
		name       string
		lastSlotID string
		active     int32
		tool       string
	}{
		{"every target named, first phase active", shipRepo, 0, "git_fetch"},
		{"every target named, last phase active", shipRepo, 2, "git_push"},
		{"the active phase names no target", "", 2, "git_push"},
	}
	for _, tc := range cases {
		t.Run(tc.name+": the ask clears the active phase", func(t *testing.T) {
			rec := &fakeRecorder{}
			// Built twice: a selection record has to name the plan's digest,
			// which is only knowable once the plan is frozen.
			_, plan := shipGate(t, rec, tc.lastSlotID, nil)
			h, _ := shipGate(t, rec, tc.lastSlotID, []plangateaudit.Content{selectedRec(plan, tc.active)})

			d := evalTool(t, h, tc.tool)

			require.NotNil(t, d.Approval, "an unapproved phase must reach a human")
			assert.Contains(t, coveredKeys(coveredOf(t, d.Approval.Payload)),
				plan.Phases[tc.active].AuthorityKey(),
				"the answer must clear the phase that raised the card, or approving changes nothing")
		})
	}
}

// A phase whose target is unnamed is NOT pre-approved. Its authority key covers
// the slot type and not the instance, so clearing it in advance would authorize
// a resource nobody was shown — and the card says, in as many words, that it
// will ask again.
func TestPlanScopedApproval_doesNotPreApproveAPhaseWithNoTarget(t *testing.T) {
	rec := &fakeRecorder{}
	h, plan := shipGate(t, rec, "", nil) // the ship phase defers

	d := evalTool(t, h, "git_fetch")

	require.NotNil(t, d.Approval)
	assert.NotContains(t, coveredKeys(coveredOf(t, d.Approval.Payload)),
		plan.Phases[2].AuthorityKey(),
		"pre-approving an unnamed target grants a resource the approver never saw")

	card := d.Approval.Payload["card"].(plangate.Card)
	assert.Contains(t, card.What, "Phase 3",
		"name WHICH phase is not covered, or the approver cannot tell what they still owe")
}

// The join neither half can see on its own: the records the ask carries have to
// be records the FOLD reads back as approval.
//
// The gate projects them and the runner writes them; nothing in either package's
// own tests says the fold then agrees. Get it wrong — a missing key, a phase
// index the fold cannot place — and the human clicks approve, the fold still
// reports the phase unapproved, and the next call raises the same card. Working
// halves, broken contract.
//
// Stamped here the way the runner stamps them; the bronze bundle
// `plangate-plan-scoped-approval` is what proves the runner really does.
func TestPlanScopedApproval_theRecordsItCarriesFoldAsApproved(t *testing.T) {
	rec := &fakeRecorder{}
	h, plan := shipGate(t, rec, shipRepo, nil)

	d := evalTool(t, h, "git_fetch")
	require.NotNil(t, d.Approval)

	var written []plangateaudit.Content
	for _, c := range coveredOf(t, d.Approval.Payload) {
		c.Event = plangateaudit.EventPhaseApproved
		c.PlanDigest = plan.Digest()
		written = append(written, c)
	}

	st, err := plangate.Fold(plan, written)
	require.NoError(t, err)
	for i := range plan.Phases {
		assert.True(t, st.PhaseApproved(i),
			"phase %d was on the card, so one yes must clear it", i)
	}
}

// priorShipPlan is a plan structurally identical to shipGate's except the
// ship phase names no permission at all — so diffing the two plans' shared
// phase index shows a genuine widening (a pure handle addition, no slot
// riding along, since the slot is named identically on both sides).
func priorShipPlan(t *testing.T) plangate.Plan {
	t.Helper()
	surface := shipSurface(t)
	p, probs := plangate.FreezeFrom([]plangate.AuthoredPhase{
		{ID: "recon", Label: "Read the repository", Why: "to see what needs fixing",
			Permissions: []plangate.AuthoredPermission{{Handle: "perm:fetch:git_repo", Why: "to read the remote"}},
			Slots:       []plangate.AuthoredSlot{{Type: "git_repo", ID: shipRepo, Why: "the repo the user named"}}},
		{ID: "edit", Label: "Fix the typos", Why: "apply the corrections",
			Permissions: []plangate.AuthoredPermission{{Handle: "perm:read:git_repo", Why: "to re-read as I edit"}},
			Slots:       []plangate.AuthoredSlot{{Type: "git_repo", ID: shipRepo, Why: "the same repo"}}},
		{ID: "ship", Label: "Open the pull request", Why: "deliver the change",
			Slots: []plangate.AuthoredSlot{{Type: "git_repo", ID: shipRepo, Why: "where the PR goes"}}},
	}, surface, []string{"git_repo"})
	require.Empty(t, probs)
	return p
}

// THE blocker this slice fixes: a widening on the WORST phase of a
// plan-scoped card must not understate itself. Approving this authorizes the
// push, so the lead must name the external tier even though the card's What
// is a delta ("Add to the approved plan: …") rather than the full ceiling —
// which is exactly the shape whose Phases BuildCard leaves empty, and which a
// lead computed from `card` instead of a synthesized fallback reads as if the
// plan only reads.
func TestPlanScopedApproval_aWideningOnTheWorstPhaseNamesTheExternalTierInTheSummary(t *testing.T) {
	rec := &fakeRecorder{}
	prior := priorShipPlan(t)

	// Built twice, same reason as TestPlanScopedApproval_alwaysClearsThePhaseThatRaisedIt:
	// a selection record has to name the plan's digest, which is only knowable
	// once the plan is frozen.
	_, plan := shipGate(t, rec, shipRepo, nil)

	idx0, idx1, idx2 := int32(0), int32(1), int32(2)
	records := append(planApprovedRecs(prior),
		plangateaudit.Content{Event: plangateaudit.EventPhaseApproved, PlanDigest: prior.Digest(), PhaseIndex: &idx0},
		plangateaudit.Content{Event: plangateaudit.EventPhaseApproved, PlanDigest: prior.Digest(), PhaseIndex: &idx1},
		plangateaudit.Content{Event: plangateaudit.EventPhaseApproved, PlanDigest: prior.Digest(), PhaseIndex: &idx2},
		selectedRec(plan, 2),
	)

	h, _ := shipGate(t, rec, shipRepo, records)

	d := evalTool(t, h, "git_push")

	require.NotNil(t, d.Approval, "the ship phase widens what was approved, so it must ask")
	require.Equal(t, "plan_phase", d.Approval.Kind)

	summary := d.Approval.Summary
	assert.Contains(t, summary, "leaves this session and cannot be undone",
		"the plan's worst phase pushes; the lead must name that tier even though the "+
			"card's What states a delta, not the full ceiling")
	assert.NotContains(t, summary, "only reads",
		`the What immediately below reads "Add to the approved plan: … push … `+
			`(leaves this session)"; a lead saying the plan only reads contradicts it`)
}
