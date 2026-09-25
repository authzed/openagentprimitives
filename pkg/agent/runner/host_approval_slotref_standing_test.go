package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// The fallback branch of requestedSlotRefs is what approvedPhaseRecords'
// non-covered branch feeds — a plain plan_phase ask, or a plan_amendment —
// since that synthesized record never carries SlotRefs of its own. Before
// this test, that branch built a SlotRef from rec.Slots and pg.slotValues but
// left Standing at its zero value, so "this was session-only" and "this
// record predates the Standing field" were indistinguishable downstream.
func TestRequestedSlotRefs_FallbackPopulatesStandingFromTheMap(t *testing.T) {
	rec := plangateaudit.Content{Slots: []string{"crm_contact", "crm_company"}}
	pg := &pendingPlanGate{
		slotValues: map[string]string{"crm_contact": "acme-corp-hash"},
	}
	standing := map[string]string{
		"crm_contact": spiceboxv1alpha1.StandingRequired,
		"crm_company": spiceboxv1alpha1.StandingSessionOnly,
	}

	got := requestedSlotRefs(rec, pg, standing, nil)

	assert.Equal(t, []plangateaudit.SlotRef{
		{Type: "crm_contact", ID: "acme-corp-hash", Standing: spiceboxv1alpha1.StandingRequired},
		{Type: "crm_company", Standing: spiceboxv1alpha1.StandingSessionOnly},
	}, got, "each fallback ref must carry the same Standing recordPlanGateDecision used to decide it")
}

// A type absent from the standing map (status has not published resolvedSlots
// yet, or the class declares no standing for it) must read back as "" —
// session-only by the same default the live decision path applies at
// host_approval.go's PlanGateSlotStanding lookup — not panic on a nil map and
// not fabricate a value.
func TestRequestedSlotRefs_FallbackUnmappedTypeGetsEmptyStanding(t *testing.T) {
	rec := plangateaudit.Content{Slots: []string{"crm_contact"}}
	pg := &pendingPlanGate{}

	got := requestedSlotRefs(rec, pg, nil, nil)

	assert.Equal(t, []plangateaudit.SlotRef{{Type: "crm_contact"}}, got)
}

// When the record already carries SlotRefs (the covered path — frozen by
// plangate.PhaseAuthorityRecord, which stamps Standing at freeze time from
// the same per-type map), requestedSlotRefs must pass them through untouched.
// The standing map argument here deliberately disagrees with what the record
// already carries, so a test that silently prefers the fresh map over the
// frozen record would be caught.
func TestRequestedSlotRefs_CoveredRecordPassesThroughUnchanged(t *testing.T) {
	frozen := []plangateaudit.SlotRef{
		{Type: "crm_contact", ID: "acme-corp-hash", Standing: spiceboxv1alpha1.StandingRequired},
	}
	rec := plangateaudit.Content{SlotRefs: frozen}
	pg := &pendingPlanGate{}
	standing := map[string]string{"crm_contact": spiceboxv1alpha1.StandingSessionOnly}

	got := requestedSlotRefs(rec, pg, standing, nil)

	assert.Equal(t, frozen, got, "a record that already named its refs must not be re-derived from a live map")
}

// unionSlotRefs is what approverCanDelegateSlots is asked about, built from
// the same fallback path — so its output has to carry Standing too, or the
// SAME missing-Standing defect would show up one layer up as well.
func TestUnionSlotRefs_FallbackCarriesStanding(t *testing.T) {
	pg := &pendingPlanGate{
		slots:      []string{"crm_contact"},
		slotValues: map[string]string{"crm_contact": "acme-corp-hash"},
	}
	standing := map[string]string{"crm_contact": spiceboxv1alpha1.StandingRequired}

	got := unionSlotRefs(pg, standing, nil)

	assert.Equal(t, []plangateaudit.SlotRef{
		{Type: "crm_contact", ID: "acme-corp-hash", Standing: spiceboxv1alpha1.StandingRequired},
	}, got)
}
