package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// freezeLoop is a Loop wired with just enough to freeze a plan and record it.
func freezeLoop(t *testing.T) (*Loop, *memory.Local) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	readH, err := permsurface.ParseHandle("perm:read:tracker_issue")
	require.NoError(t, err)

	return &Loop{
		Mem:          mem,
		SessionKey:   memory.NamespacedName{Namespace: "ns", Name: "s"},
		PlanGateMode: "enforcing",
		PlanGateSurface: []permsurface.Descriptor{
			{Handle: readH, StateImpact: authz.Readonly},
		},
		PlanGateMaxAutoApprove: 8,
	}, mem
}

// A tier-0 phase clears itself, so its phase_approved record is a GRANT like
// any other — and carry-over now reads the granted subset off that record.
// Written without a ceiling it falls back to reconstructing the declaration,
// which keeps a legacy reading alive on a record produced today.
func TestFreezeAndRecordPhases_tier0ApprovalCarriesItsGrantedSubset(t *testing.T) {
	l, mem := freezeLoop(t)

	_, freezeErr := l.FreezeAndRecordPhases(context.Background(), []plangate.AuthoredPhase{
		{ID: "recon", Label: "Recon", Why: "read first", Permissions: []plangate.AuthoredPermission{
			{Handle: "perm:read:tracker_issue", Why: "to read"},
		}},
	})
	require.NoError(t, freezeErr)

	recs, err := plangateaudit.List(
		memory.WithSystemApproval(context.Background(), "plan_gate"),
		mem, memory.Scope{Kind: "session", ID: "ns/s"})
	require.NoError(t, err)

	var approvals []plangateaudit.Content
	for _, r := range recs {
		if r.Event == plangateaudit.EventPhaseApproved {
			approvals = append(approvals, r)
		}
	}
	require.Len(t, approvals, 1, "an all-readonly phase auto-approves")

	assert.Equal(t, []string{"perm:read:tracker_issue"}, approvals[0].Ceiling,
		"every approval written from here on states what it granted, so the "+
			"declaration fallback shrinks to history rather than staying live")
	assert.Equal(t, 1, approvals[0].MaxCount)
}

// The wiring a plangate-package test cannot see: the runner must actually PASS
// the phase's slot requests into the tier computation.
//
// The rule and its consumption are separate facts. ComputeTier refusing tier 0
// on an ungranted slot is worth nothing if FreezeAndRecordPhases never tells it
// there are any — the phase would auto-approve, write its own phase_approved
// record, and a later grant write would land with no human in the loop.
func TestFreezeAndRecordPhases_aSlotRequestBlocksTier0AutoApproval(t *testing.T) {
	l, mem := freezeLoop(t)
	l.PlanGateSlotTypes = []string{"crm_company"}

	_, freezeErr := l.FreezeAndRecordPhases(context.Background(), []plangate.AuthoredPhase{{
		ID: "recon", Label: "Recon", Why: "read first",
		Permissions: []plangate.AuthoredPermission{
			{Handle: "perm:read:tracker_issue", Why: "to read"},
		},
		Slots: []plangate.AuthoredSlot{{Type: "crm_company", Why: "to reach the company"}},
	}})
	require.NoError(t, freezeErr)

	recs, err := plangateaudit.List(
		memory.WithSystemApproval(context.Background(), "plan_gate"),
		mem, memory.Scope{Kind: "session", ID: "ns/s"})
	require.NoError(t, err)

	var declared, approved int
	for _, r := range recs {
		switch r.Event {
		case plangateaudit.EventPlanApproved:
			declared++
			assert.Equal(t, []string{"crm_company"}, r.Slots,
				"the frozen plan must record its slot requests or it cannot be rebuilt")
		case plangateaudit.EventPhaseApproved:
			approved++
		}
	}
	assert.Equal(t, 1, declared, "the phase freezes")
	assert.Zero(t, approved,
		"an all-readonly phase would auto-approve, but approving this one writes "+
			"a grant on somebody's resource — so it waits for a human")
}

// The whole path, through the REAL projection the runner uses: an agent asks
// for a call budget in update_plan and it must reach the frozen plan. A budget
// dropped anywhere in that chain is silent — the phase simply never runs out,
// and the focus mechanism is inert while looking wired.
func TestAuthoredPhasesFrom_carriesTheCallBudgetIntoTheFrozenPlan(t *testing.T) {
	authored := AuthoredPhasesFrom([]plans.Phase{{
		ID: "recon", Label: "Recon", Why: "read first",
		Permissions: []plans.PhasePermission{{Handle: "perm:read:tracker_issue", Why: "to read"}},
		Budget:      &plans.PhaseBudget{Calls: 12, Why: "the repo is large and needs a broad sweep"},
	}})

	require.Len(t, authored, 1)
	require.NotNil(t, authored[0].Budget, "a declared budget must survive the projection")
	assert.Equal(t, 12, authored[0].Budget.Calls)

	frozen, probs := plangate.FreezeFrom(authored, nil, nil)
	require.Empty(t, probs)
	assert.Equal(t, 12, frozen.Phases[0].Budget.Calls,
		"the frozen plan is what the gate reads; a budget that stops short of it does nothing")
	assert.Equal(t, "the repo is large and needs a broad sweep", frozen.Phases[0].Budget.Why,
		"the justification rides along for the approver, even though it never re-keys the plan")
}

// A phase that declares no budget must freeze as unbounded, not as zero-calls.
func TestAuthoredPhasesFrom_anUndeclaredBudgetStaysUnbounded(t *testing.T) {
	authored := AuthoredPhasesFrom([]plans.Phase{{ID: "recon", Label: "Recon", Why: "read"}})
	require.Len(t, authored, 1)
	assert.Nil(t, authored[0].Budget)

	frozen, _ := plangate.FreezeFrom(authored, nil, nil)
	assert.Zero(t, frozen.Phases[0].Budget.Calls, "zero is unbounded; see BudgetSpec")
}

// The tier-0 auto-approve budget is SESSION-CUMULATIVE — the union of handles
// auto-approved without a human across the whole session — not per-phase. With
// it unwired, a prompt-injected agent declares many all-readonly phases in ONE
// update_plan and every one prices to Tier0, auto-clearing unbounded readonly
// breadth with nobody asked. The accumulator must carry across the freeze loop,
// so once the cumulative count crosses the budget the remaining phases wait for
// a person.
func TestFreezeAndRecordPhases_tier0BudgetIsSessionCumulativeNotPerPhase(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	// 12 distinct readonly handles; budget 8.
	var surface []permsurface.Descriptor
	var mk func(i int) string
	mk = func(i int) string { return "perm:read:res" + string(rune('a'+i)) }
	for i := 0; i < 12; i++ {
		h, err := permsurface.ParseHandle(mk(i))
		require.NoError(t, err)
		surface = append(surface, permsurface.Descriptor{Handle: h, StateImpact: authz.Readonly})
	}
	l := &Loop{
		Mem: mem, SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"},
		PlanGateMode: "enforcing", PlanGateSurface: surface, PlanGateMaxAutoApprove: 8,
	}

	// Three phases, 4 distinct readonly handles each: cumulative 4, 8, 12.
	// Phases 0 and 1 fit the budget (4, then 8); phase 2 (12) must not auto-clear.
	phase := func(id string, from int) plangate.AuthoredPhase {
		p := plangate.AuthoredPhase{ID: id, Label: id, Why: "w"}
		for i := from; i < from+4; i++ {
			p.Permissions = append(p.Permissions, plangate.AuthoredPermission{Handle: mk(i), Why: "to read"})
		}
		return p
	}
	_, err := l.FreezeAndRecordPhases(context.Background(),
		[]plangate.AuthoredPhase{phase("a", 0), phase("b", 4), phase("c", 8)})
	require.NoError(t, err)

	recs, err := plangateaudit.List(memory.WithSystemApproval(context.Background(), "plan_gate"),
		mem, memory.Scope{Kind: "session", ID: "ns/s"})
	require.NoError(t, err)

	tier0 := 0
	for _, r := range recs {
		if r.Event == plangateaudit.EventPhaseApproved && r.Provenance == "plan_gate:tier0" {
			tier0++
		}
	}
	assert.Equal(t, 2, tier0,
		"the first two phases fit the cumulative budget and auto-clear; the third exceeds it and must wait for a human")
}

// Re-planning must not RESET the session-cumulative budget: prior tier-0
// clearances are seeded from the log, so a second update_plan declaring fresh
// readonly breadth after the budget is already spent does not auto-clear.
func TestFreezeAndRecordPhases_priorClearancesSeedTheBudgetAcrossReplans(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	var surface []permsurface.Descriptor
	mk := func(i int) string { return "perm:read:res" + string(rune('a'+i)) }
	for i := 0; i < 12; i++ {
		h, err := permsurface.ParseHandle(mk(i))
		require.NoError(t, err)
		surface = append(surface, permsurface.Descriptor{Handle: h, StateImpact: authz.Readonly})
	}
	l := &Loop{
		Mem: mem, SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"},
		PlanGateMode: "enforcing", PlanGateSurface: surface, PlanGateMaxAutoApprove: 8,
	}
	phase := func(id string, from, n int) plangate.AuthoredPhase {
		p := plangate.AuthoredPhase{ID: id, Label: id, Why: "w"}
		for i := from; i < from+n; i++ {
			p.Permissions = append(p.Permissions, plangate.AuthoredPermission{Handle: mk(i), Why: "to read"})
		}
		return p
	}

	// First plan: one phase with 6 readonly handles → auto-clears (6 ≤ 8).
	_, err := l.FreezeAndRecordPhases(context.Background(), []plangate.AuthoredPhase{phase("a", 0, 6)})
	require.NoError(t, err)

	// Re-plan: a phase with 4 fresh handles. Per-phase it would fit (4 ≤ 8), but
	// 6 already spent + 4 = 10 > 8, so it must NOT auto-clear.
	_, err = l.FreezeAndRecordPhases(context.Background(), []plangate.AuthoredPhase{phase("b", 6, 4)})
	require.NoError(t, err)

	recs, err := plangateaudit.List(memory.WithSystemApproval(context.Background(), "plan_gate"),
		mem, memory.Scope{Kind: "session", ID: "ns/s"})
	require.NoError(t, err)
	tier0 := 0
	for _, r := range recs {
		if r.Event == plangateaudit.EventPhaseApproved && r.Provenance == "plan_gate:tier0" {
			tier0++
		}
	}
	assert.Equal(t, 1, tier0, "only the first plan's phase auto-cleared; the re-plan exceeded the seeded budget")
}

// The budget is the UNION of auto-approved handles, not a running sum. An agent
// re-planning the SAME readonly reach (the normal update_plan flow) must keep
// auto-approving — the seeded prior handles and the re-declared ones are the
// same set, so |A ∪ P| = |A|, well inside the budget. Pricing it as |A|+|P|
// double-counts and forces a human click for reach the session already holds.
func TestFreezeAndRecordPhases_tier0BudgetPricesTheUnionNotTheSum(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	var surface []permsurface.Descriptor
	mk := func(i int) string { return "perm:read:res" + string(rune('a'+i)) }
	for i := 0; i < 12; i++ {
		h, err := permsurface.ParseHandle(mk(i))
		require.NoError(t, err)
		surface = append(surface, permsurface.Descriptor{Handle: h, StateImpact: authz.Readonly})
	}
	l := &Loop{
		Mem: mem, SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"},
		PlanGateMode: "enforcing", PlanGateSurface: surface, PlanGateMaxAutoApprove: 8,
	}
	phase := func(id string, from, n int) plangate.AuthoredPhase {
		p := plangate.AuthoredPhase{ID: id, Label: id, Why: "w"}
		for i := from; i < from+n; i++ {
			p.Permissions = append(p.Permissions, plangate.AuthoredPermission{Handle: mk(i), Why: "to read"})
		}
		return p
	}
	countTier0 := func() int {
		recs, err := plangateaudit.List(memory.WithSystemApproval(context.Background(), "plan_gate"),
			mem, memory.Scope{Kind: "session", ID: "ns/s"})
		require.NoError(t, err)
		n := 0
		for _, r := range recs {
			if r.Event == plangateaudit.EventPhaseApproved && r.Provenance == "plan_gate:tier0" {
				n++
			}
		}
		return n
	}

	// First plan: 6 readonly handles → auto-clears (6 ≤ 8).
	_, err := l.FreezeAndRecordPhases(context.Background(), []plangate.AuthoredPhase{phase("a", 0, 6)})
	require.NoError(t, err)
	require.Equal(t, 1, countTier0())

	// Re-plan the SAME 6 handles. Union is still 6 ≤ 8 → must auto-clear again.
	// A sum would see 6 (seed) + 6 = 12 > 8 and wrongly force a click.
	_, err = l.FreezeAndRecordPhases(context.Background(), []plangate.AuthoredPhase{phase("a", 0, 6)})
	require.NoError(t, err)
	assert.Equal(t, 2, countTier0(),
		"re-planning identical readonly reach stays auto-approved — the budget prices the union, not the sum")
}
