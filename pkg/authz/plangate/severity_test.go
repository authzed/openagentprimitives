package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		req  Request
		want Severity
	}{
		{"first plan, nothing external: Routine",
			Request{Kind: KindPlanApproval, EnvelopeRecorded: true}, Routine},

		{"supersede changing no ceiling and resetting nothing: Routine",
			Request{Kind: KindSupersede, EnvelopeRecorded: true}, Routine},

		{"any external handle: Elevated",
			Request{Kind: KindPlanApproval, HasExternalHandle: true, EnvelopeRecorded: true}, Elevated},

		{"extra entry against a spent max: Elevated",
			Request{Kind: KindExtraEntry, BudgetSpent: true, EnvelopeRecorded: true}, Elevated},

		{"supersede changing a ceiling: Elevated",
			Request{Kind: KindSupersede, CeilingChanged: true, EnvelopeRecorded: true}, Elevated},

		// Restoring budgets and rewinding are the closest thing the model has
		// to a rewind of a human's decision, so they price like one. Before
		// this, a supersede that reset every budget in the plan was Routine
		// while re-entering one phase once was Elevated — inverted.
		{"supersede restoring a spent budget: Elevated",
			Request{Kind: KindSupersede, RestoresBudget: true, EnvelopeRecorded: true}, Elevated},

		{"supersede rewinding the active phase: Elevated",
			Request{Kind: KindSupersede, RewindsPhase: true, EnvelopeRecorded: true}, Elevated},

		{"supersede leaving the envelope: Severe",
			Request{Kind: KindSupersede, LeavesEnvelope: true, EnvelopeRecorded: true}, Severe},

		{"post-exposure widening that adds external reach: Severe",
			Request{Kind: KindAmendment, AfterFirstToolResult: true, AddsExternalReach: true, EnvelopeRecorded: true}, Severe},

		// The post-exposure rule needs external reach to fire; without it this
		// is Elevated from the amendment rule alone, NOT Severe. Pins that the
		// two rules are independent rather than one implying the other.
		{"amendment after exposure WITHOUT external reach: Elevated, not Severe",
			Request{Kind: KindAmendment, AfterFirstToolResult: true, EnvelopeRecorded: true}, Elevated},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Classify(tc.req))
		})
	}
}

// No envelope means no pre-exposure baseline, so there is nothing to compare a
// supersede against and every supersede is treated as leaving it. This makes
// recording an envelope strictly better than not, with no switch to forget.
func TestClassify_noEnvelopeMakesEverySupersedeSevere(t *testing.T) {
	assert.Equal(t, Severe, Classify(Request{Kind: KindSupersede, EnvelopeRecorded: false}))
	assert.Equal(t, Severe, Classify(Request{Kind: KindSupersede, EnvelopeRecorded: false, CeilingChanged: true}))
}

// The missing-envelope rule is about SUPERSEDING a pre-commitment. A first plan
// has nothing to have left, so it must not be swept up by it — otherwise every
// session's opening card is Severe and the signal dies immediately.
func TestClassify_noEnvelopeDoesNotEscalateAFirstPlan(t *testing.T) {
	assert.Equal(t, Routine, Classify(Request{Kind: KindPlanApproval, EnvelopeRecorded: false}))
}

// Severity is the MAX across every applicable rule, never the first match.
func TestClassify_takesTheMaximum(t *testing.T) {
	cases := []struct {
		name string
		req  Request
		want Severity
	}{
		{"ceiling change (Elevated) plus envelope exit (Severe)",
			Request{Kind: KindSupersede, CeilingChanged: true, LeavesEnvelope: true, EnvelopeRecorded: true}, Severe},
		{"external handle (Elevated) plus a spent budget (Elevated)",
			Request{Kind: KindExtraEntry, HasExternalHandle: true, BudgetSpent: true, EnvelopeRecorded: true}, Elevated},
		{"every rule at once",
			Request{Kind: KindSupersede, HasExternalHandle: true, BudgetSpent: true,
				CeilingChanged: true, RestoresBudget: true, RewindsPhase: true,
				LeavesEnvelope: true, EnvelopeRecorded: true}, Severe},
	}
	for _, tc := range cases {
		t.Run(tc.name+": "+string(tc.want), func(t *testing.T) {
			assert.Equal(t, tc.want, Classify(tc.req))
		})
	}
}

// An approval is a question, not a failure, at every severity: Approve must
// never render danger-styled. Elevation is still signalled — by the card's
// rail, marker and external lines — just not by recolouring the button that
// answers "yes".
func TestSeverityStyles_ApprovalIsNeverRed(t *testing.T) {
	for _, s := range []Severity{Routine, Elevated, Severe} {
		t.Run(string(s)+": Approve is primary, Deny is not danger-styled", func(t *testing.T) {
			assert.Equal(t, channelevents.ActionStylePrimary, s.ApproveStyle(),
				"an approval is a question, not a failure: elevation is carried by the card's amber, not by the confirm button")
			assert.NotEqual(t, channelevents.ActionStyleDanger, s.DenyStyle(),
				"a red Deny trains the reflex that refusing is the dangerous act")
		})
	}
}

// The destructive act is granting, so Deny must never be the red button on a
// dangerous card — that trains exactly the wrong reflex.
func TestSeverity_denyIsNeverDangerStyled(t *testing.T) {
	for _, s := range []Severity{Routine, Elevated, Severe} {
		t.Run(string(s)+": Deny is not danger-styled", func(t *testing.T) {
			assert.NotEqual(t, channelevents.ActionStyleDanger, s.DenyStyle())
		})
	}
}

func TestSeverity_marker(t *testing.T) {
	assert.Empty(t, Routine.Marker(), "a routine card carries no marker")
	assert.Equal(t, "⚠️", Elevated.Marker())
	assert.Equal(t, "🛑", Severe.Marker())
}

// A kind with no styling affordance must still get the marker and the computed
// sentence, so the signal degrades to text rather than disappearing.
func TestSeverity_rendersTextEvenWithoutStyling(t *testing.T) {
	for _, s := range []Severity{Elevated, Severe} {
		t.Run(string(s)+": has a non-empty marker", func(t *testing.T) {
			assert.NotEmpty(t, s.Marker())
		})
	}
}

func TestSeverity_ordering(t *testing.T) {
	assert.Less(t, Routine.rank(), Elevated.rank())
	assert.Less(t, Elevated.rank(), Severe.rank())
}

// An amendment is a widening by definition — the agent wants reach its approved
// plan does not hold — so it is never Routine, however mild the handle.
func TestClassify_amendmentIsNeverRoutine(t *testing.T) {
	got := Classify(Request{Kind: KindAmendment, EnvelopeRecorded: true})
	assert.NotEqual(t, Routine, got)
	assert.Equal(t, Elevated, got)
}
