package plans_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestStatus_IsValid(t *testing.T) {
	require.True(t, plans.StatusPending.IsValid())
	require.True(t, plans.StatusInProgress.IsValid())
	require.True(t, plans.StatusDone.IsValid())
	require.True(t, plans.StatusStopped.IsValid(), "stopped must be valid for replay")
	require.False(t, plans.Status("nope").IsValid())
	require.False(t, plans.Status("").IsValid())
}

func TestPlan_FindItem(t *testing.T) {
	p := plans.Plan{
		Name: "main",
		Items: []plans.Item{
			{ID: "a", Label: "A", Status: plans.StatusPending},
			{ID: "b", Label: "B", Status: plans.StatusInProgress},
		},
	}
	got, ok := p.FindItem("b")
	require.True(t, ok)
	require.Equal(t, plans.StatusInProgress, got.Status)

	_, ok = p.FindItem("nope")
	require.False(t, ok)
}

func TestStatusError_IsValid(t *testing.T) {
	if !plans.StatusError.IsValid() {
		t.Fatalf("StatusError must be a valid status")
	}
	for _, s := range []plans.Status{plans.StatusPending, plans.StatusInProgress, plans.StatusDone, plans.StatusError} {
		if !s.IsValid() {
			t.Errorf("%q should be valid", s)
		}
	}
	if plans.Status("nope").IsValid() {
		t.Errorf("unknown status must not be valid")
	}
}

func TestItem_DetailsOutputFields(t *testing.T) {
	it := plans.Item{
		ID:      "step1",
		Label:   "do a thing",
		Status:  plans.StatusDone,
		Details: "why we do it",
		Output:  "what we found",
	}
	if it.Details != "why we do it" || it.Output != "what we found" {
		t.Fatalf("Item fields not assignable: %+v", it)
	}
}

func TestPlanToEnvelopePayload(t *testing.T) {
	p := plans.Plan{
		Name:       "trip",
		ParentPlan: "root",
		ParentItem: "leg1",
		UpdatedAt:  time.Unix(7, 0).UTC(),
		Items: []plans.Item{
			{ID: "a", Label: "pack", Status: plans.StatusDone, OperationID: "op1"},
			{ID: "b", Label: "go", Status: plans.StatusInProgress, Details: "d", Output: "o"},
		},
	}
	got := p.ToEnvelopePayload()
	assert.Equal(t, channelevents.PlanUpdatePayload{
		PlanName:   "trip",
		ParentPlan: "root",
		ParentItem: "leg1",
		UpdatedAt:  time.Unix(7, 0).UTC(),
		Items: []channelevents.PlanItemRef{
			{ID: "a", Label: "pack", Status: "done", OperationID: "op1"},
			{ID: "b", Label: "go", Status: "in_progress", Details: "d", Output: "o"},
		},
	}, got)
}

// A plan's phases and its items' phase grouping must REACH the channel, or a
// renderer cannot show the staging the agent declared — which is how the web
// chat ended up drawing a flat checklist beside an approval card that spoke in
// phases, describing one plan as two unrelated things.
func TestPlanToEnvelopePayload_CarriesPhases(t *testing.T) {
	p := plans.Plan{
		Name:      "ship",
		UpdatedAt: time.Unix(7, 0).UTC(),
		Items: []plans.Item{
			{ID: "a", Label: "read the repo", Status: plans.StatusDone, Phase: "recon"},
			{ID: "b", Label: "push the fix", Status: plans.StatusPending, Phase: "write"},
			{ID: "c", Label: "tidy up", Status: plans.StatusPending},
		},
		Phases: []plans.Phase{
			{ID: "recon", Label: "Read the repository"},
			{ID: "write", Label: "Push the fix"},
		},
	}

	got := p.ToEnvelopePayload()

	assert.Equal(t, []channelevents.PlanPhaseRef{
		{ID: "recon", Label: "Read the repository"},
		{ID: "write", Label: "Push the fix"},
	}, got.Phases, "the declared phase list travels in order")

	assert.Equal(t, []string{"recon", "write", ""}, []string{
		got.Items[0].Phase, got.Items[1].Phase, got.Items[2].Phase,
	}, "each item keeps its phase grouping, and an ungrouped item stays ungrouped")
}

// A plan with no declared phases must emit none rather than an empty list, so
// "this plan declares no staging" and "this plan declares zero phases" cannot
// be told apart by a renderer that only checks presence.
func TestPlanToEnvelopePayload_NoPhasesEmitsNone(t *testing.T) {
	p := plans.Plan{
		Name:      "simple",
		UpdatedAt: time.Unix(7, 0).UTC(),
		Items:     []plans.Item{{ID: "a", Label: "do it", Status: plans.StatusPending}},
	}

	got := p.ToEnvelopePayload()

	assert.Nil(t, got.Phases, "no declared phases means no phase list on the wire")
	assert.Empty(t, got.Items[0].Phase, "an item with no phase carries none")
}
