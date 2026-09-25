package bronzethread_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

func idx(i int32) *int32 { return &i }

// A trace is the authorization-relevant shape of a run: which gate events fired
// against which phase, and which checks resolved which way. It is what a golden
// pins, so it must say enough to catch a behavioral change and little enough to
// stay stable.
func TestTraceLines_rendersGateEventsAndDecisions(t *testing.T) {
	got := bt.TraceLines(
		[]plangateaudit.Content{
			{Event: plangateaudit.EventPlanApproved, PhaseIndex: idx(0), Mode: "enforcing"},
			{Event: plangateaudit.EventGateAllowed, PhaseIndex: idx(0),
				Handle: "perm:list:crm_company", Tool: "centerdot_list_companies", Mode: "enforcing"},
		},
		[]authzdecision.Decision{
			{ResourceType: "crm_company", ResourceID: "*", Permission: "list", Outcome: "allowed"},
		},
	)

	// Sorted, so "authz" precedes "gate" — the lines are a set of facts about
	// the run, not a timeline. See TraceLines on why order is deliberately not
	// captured here.
	assert.Equal(t, []string{
		"authz crm_company:*#list allowed",
		"gate  gate_allowed    phase=0 handle=perm:list:crm_company tool=centerdot_list_companies",
		"gate  plan_approved   phase=0",
	}, got)
}

// Stability is the whole contract. Two logs carrying the same facts in a
// different write order must produce the same golden, or the golden fails on
// scheduling noise and gets regenerated blindly until it asserts nothing.
func TestTraceLines_isIndependentOfRecordOrder(t *testing.T) {
	a := plangateaudit.Content{Event: plangateaudit.EventPlanApproved, PhaseIndex: idx(0)}
	b := plangateaudit.Content{Event: plangateaudit.EventPhaseApproved, PhaseIndex: idx(1)}
	x := authzdecision.Decision{ResourceType: "t", ResourceID: "1", Permission: "read", Outcome: "allowed"}
	y := authzdecision.Decision{ResourceType: "t", ResourceID: "2", Permission: "read", Outcome: "denied"}

	assert.Equal(t,
		bt.TraceLines([]plangateaudit.Content{a, b}, []authzdecision.Decision{x, y}),
		bt.TraceLines([]plangateaudit.Content{b, a}, []authzdecision.Decision{y, x}),
	)
}

// A duplicate decision on the same key with the same outcome is one fact, not
// two: the count varies with retries and re-dispatch, which is exactly the
// scheduling noise a golden must not encode.
func TestTraceLines_collapsesRepeatedIdenticalDecisions(t *testing.T) {
	d := authzdecision.Decision{ResourceType: "t", ResourceID: "1", Permission: "read", Outcome: "allowed"}

	assert.Equal(t,
		bt.TraceLines(nil, []authzdecision.Decision{d}),
		bt.TraceLines(nil, []authzdecision.Decision{d, d, d}),
	)
}

// A denial and an allow on the SAME key are two different facts and must both
// survive — that pair is "denied, then allowed after approval", the single most
// important sequence this feature has.
func TestTraceLines_keepsBothOutcomesForOneKey(t *testing.T) {
	got := bt.TraceLines(nil, []authzdecision.Decision{
		{ResourceType: "t", ResourceID: "1", Permission: "read", Outcome: "denied"},
		{ResourceType: "t", ResourceID: "1", Permission: "read", Outcome: "allowed"},
	})
	assert.Len(t, got, 2, "collapsing these would erase the approval flow entirely")
}

func TestTraceLines_emptyRunTracesToNothing(t *testing.T) {
	assert.Empty(t, bt.TraceLines(nil, nil))
}
