package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// The approver line on a plan-gate card was always EMPTY.
//
// PlanGateDeps.Approvers is read in three places to render "who can say yes",
// and no binary ever set it — so every card shipped with the field blank while
// the four-level rendering ladder in channelkinds.DescribeApprovers sat
// complete and uncalled. An approval card that does not say who can approve it
// is a card the requester cannot chase.
func TestPlanGateApprovers_describesTheApproverPopulation(t *testing.T) {
	l := &Loop{
		SpiceDBLookupSubjects: func(context.Context, string, string) ([]string, error) {
			return []string{"user:alice@example.com"}, nil
		},
		SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"},
	}

	got := l.planGateApprovers(context.Background())

	assert.Equal(t, "user:alice@example.com", got,
		"one approver is named outright — the most useful rendering, so it wins")
}

// Past the naming threshold the card must NOT list everyone: a long list is
// worse than useless and the enumeration itself costs a lookup. It degrades to
// a count.
func TestPlanGateApprovers_aLargePopulationDegradesToACount(t *testing.T) {
	many := make([]string, 12)
	for i := range many {
		many[i] = "user:person" + string(rune('a'+i)) + "@example.com"
	}
	l := &Loop{
		SpiceDBLookupSubjects: func(context.Context, string, string) ([]string, error) {
			return many, nil
		},
		SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"},
	}

	got := l.planGateApprovers(context.Background())

	assert.Contains(t, got, "12 people")
	assert.NotContains(t, got, "persona@example.com", "a 12-name list is not a card line")
}

// THE degradation that matters. A lookup failure must produce the honest floor,
// never a blank and never a guess: "anyone with approve on this session" is
// correct on its own terms, so a card still tells the requester where to go.
func TestPlanGateApprovers_aFailedLookupFallsToTheHonestFloor(t *testing.T) {
	l := &Loop{
		SpiceDBLookupSubjects: func(context.Context, string, string) ([]string, error) {
			return nil, assert.AnError
		},
		SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"},
	}

	got := l.planGateApprovers(context.Background())

	assert.Equal(t, "anyone with approve on this session", got)
	assert.NotEmpty(t, got, "a blank approver line is the one outcome that helps nobody")
}

// Unwired lookup: same floor. A kubectl-driven session has no SpiceDB handle
// and must still render a card.
func TestPlanGateApprovers_withNoLookupWiredStillRendersTheFloor(t *testing.T) {
	l := &Loop{SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"}}

	assert.Equal(t, "anyone with approve on this session",
		l.planGateApprovers(context.Background()))
}

// The wiring, which the tests above cannot see: the hook must actually RECEIVE
// the description. planGateApprovers being correct is worth nothing if
// PlanGateDeps.Approvers stays empty — which is exactly the state this closed,
// where the renderer and the reader both existed and nothing joined them.
func TestPlanGateHook_receivesTheApproverDescription(t *testing.T) {
	l := &Loop{
		PlanGateMode: "enforcing",
		SessionKey:   memory.NamespacedName{Namespace: "ns", Name: "s"},
		SpiceDBLookupSubjects: func(context.Context, string, string) ([]string, error) {
			return []string{"user:alice@example.com"}, nil
		},
	}

	var built []pipeline.Hook
	for _, f := range hookFactories {
		if f.Name == "plan_gate" {
			built = f.Build(l)
		}
	}
	require.Len(t, built, 1, "the plan gate must be registered under enforcing")

	gate, ok := built[0].(*hooks.PlanGate)
	require.True(t, ok)
	assert.Equal(t, "user:alice@example.com", gate.ApproversForTest(),
		"the hook renders this onto every card; empty means the seam is still unjoined")
}

// The approver line rendered the RAW SpiceDB subject, which for a user is the
// base64 canonical id — a live card read "Who can approve:
// YWRtaW5AYXAubG9jYWw". That is unreadable, and worse, unactionable: the one
// thing this line exists for is telling a blocked requester who to go ask.
//
// The existing tests could not catch it because their fixtures return
// "user:alice@example.com" — an email, which SpiceDBLookupSubjects never
// returns in production. A fixture in a shape the real path cannot produce is
// how a display bug survives a green suite.
func TestPlanGateApprovers_namesArePresentedAsEmailsNotCanonicalIDs(t *testing.T) {
	canonical, err := identity.EmailReference(identity.Email("admin@ap.loc")).Canonical()
	require.NoError(t, err)
	require.NotContains(t, canonical.String(), "@",
		"precondition: the canonical form is opaque, which is the whole problem")

	l := &Loop{
		SpiceDBLookupSubjects: func(context.Context, string, string) ([]string, error) {
			return []string{"user:" + canonical.String()}, nil
		},
		SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"},
	}

	got := l.planGateApprovers(context.Background())

	assert.Equal(t, "admin@ap.loc", got,
		"a requester has to be able to read who to chase")
	assert.NotContains(t, got, canonical.String(),
		"the canonical id must not reach a human-facing surface")
}
