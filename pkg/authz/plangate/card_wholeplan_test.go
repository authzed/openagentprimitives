package plangate

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

// readEditShipPlan is the codebot shape: read, edit, ship — the last carrying the
// irreversible step.
func readEditShipPlan(t *testing.T, lastSlots []Slot) Plan {
	t.Helper()
	const repo = "https://github.com/acme/app"
	return Plan{Phases: []Phase{
		{
			Label: "Read the repository", Why: "to see what needs fixing",
			Permissions: []permsurface.Handle{handle(t, "fetch", "git_repo")},
			Slots:       []Slot{{Type: "git_repo", ID: repo, Why: "the repo the user named"}},
		},
		{
			Label: "Edit the files", Why: "apply the fix",
			Permissions: []permsurface.Handle{handle(t, "read", "git_repo")},
		},
		{
			Label: "Push and open the PR", Why: "deliver the change",
			Permissions: []permsurface.Handle{handle(t, "push", "git_repo")},
			Slots:       lastSlots,
		},
	}}
}

func wholePlanCard(t *testing.T, p Plan) Card {
	t.Helper()
	return BuildCard(CardInput{
		Plan: p, WholePlan: true,
		Surface: []permsurface.Descriptor{
			{Handle: handle(t, "push", "git_repo"), StateImpact: authz.External},
			{Handle: handle(t, "fetch", "git_repo"), StateImpact: authz.Readwrite},
			{Handle: handle(t, "read", "git_repo"), StateImpact: authz.Readonly},
		},
	})
}

// The whole point of moving the decision: the approver sees the ENTIRE plan at
// once and answers once. Per-phase cards asked serially, and at the moment of
// the first decision said nothing about the push coming two phases later — so a
// human cleared "read-only recon" with no idea what they were starting.
func TestBuildCard_wholePlanShowsEveryPhase(t *testing.T) {
	const repo = "https://github.com/acme/app"
	card := wholePlanCard(t, readEditShipPlan(t, []Slot{{Type: "git_repo", ID: repo, Why: "same repo"}}))

	for _, want := range []string{"Phase 1", "Phase 2", "Phase 3"} {
		assert.Contains(t, card.What, want, "every phase must be visible in one card")
	}
	assert.Contains(t, card.What, repo, "and the resource each one acts on")
	assert.Contains(t, card.What, "leaves this session",
		"the irreversible step is the reason to read the card; it must be marked where it occurs")
}

// Phases are identified by INDEX, never by the agent's label.
//
// Phase.Label is agent-authored and untrusted. A whole-plan card is the first
// thing that would have reason to print three of them in the computed half, and
// doing so would hand a prompt-injected agent three free lines inside the
// section the approver is told to trust.
func TestBuildCard_wholePlanNamesPhasesByIndexNotByAgentLabel(t *testing.T) {
	card := wholePlanCard(t, readEditShipPlan(t, nil))

	assert.NotContains(t, card.What, "Read the repository",
		"an agent-authored label must not appear in the computed half")
	assert.NotContains(t, card.What, "Push and open the PR")
	assert.Contains(t, card.Why, "Read the repository",
		"the labels are still useful — attributed, in the agent's own section")
}

// Severity is the MAX across the phases being approved, not the first phase's.
// Approving this card authorizes the push, so it must be priced as a push —
// otherwise the most consequential thing a single click ever does would render
// as routine because phase 1 happens to be a read.
func TestBuildCard_wholePlanSeverityIsTheWorstPhase(t *testing.T) {
	card := wholePlanCard(t, readEditShipPlan(t, []Slot{{Type: "git_repo", ID: "https://github.com/acme/app"}}))

	assert.NotEqual(t, string(Routine), card.Severity,
		"a plan containing an external phase cannot be a routine approval")
	assert.Contains(t, card.Lead, Severity(card.Severity).Marker(),
		"the marker rides the lead so a channel with no styling still shows the signal")
}

// What one click actually buys. A phase that named its target is covered; a
// phase that could not is not, and saying so is what keeps the card honest
// about being the LAST prompt or merely the first.
func TestBuildCard_wholePlanStatesWhichPhasesAreCovered(t *testing.T) {
	t.Run("every phase named: this is the only prompt", func(t *testing.T) {
		card := wholePlanCard(t, readEditShipPlan(t, []Slot{{Type: "git_repo", ID: "https://github.com/acme/app"}}))
		assert.Regexp(t, `(?i)covers (all|every|phases)`, card.What)
		assert.NotRegexp(t, `(?i)will ask again`, card.What,
			"promising a further prompt that never comes is its own failure")
	})

	t.Run("a phase with no target named: that one asks again", func(t *testing.T) {
		card := wholePlanCard(t, readEditShipPlan(t, []Slot{{Type: "git_repo"}}))
		assert.Regexp(t, `(?i)will ask again|asks again`, card.What)
		assert.Contains(t, card.What, "Phase 3",
			"name WHICH phase is not covered, or the approver cannot tell what they still owe")
	})
}

// The trust boundary again, over the whole-plan renderer specifically: it is the
// first thing that walks every phase, so it is the first that could leak three
// labels and three justifications into the computed half at once.
func TestBuildCard_wholePlanLeaksNoAgentTextIntoWhat(t *testing.T) {
	rng := rand.New(rand.NewSource(20260814))
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789 \n\t:*_`~[]{}()<>|&$#/\\\"'"
	token := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteByte(alphabet[rng.Intn(len(alphabet))])
		}
		return b.String()
	}

	for i := 0; i < 200; i++ {
		marker := fmt.Sprintf("MARKER%dZ%s", i, token(12))
		p := readEditShipPlan(t, []Slot{{Type: "git_repo", ID: "https://github.com/acme/app", Why: marker}})
		for j := range p.Phases {
			p.Phases[j].Label = token(10) + marker
			p.Phases[j].Why = marker + token(10)
		}

		card := wholePlanCard(t, p)

		require.NotContains(t, card.What, marker, "agent text reached the computed half (iteration %d)", i)
		require.NotContains(t, card.When, marker, "and When is computed (iteration %d)", i)
	}
}

// Being fully specified is what decides whether a plan-scoped approval covers a
// phase. A phase whose target is unnamed has nothing stable to key an approval
// to, and pre-approving it would authorize a resource the human never saw — so
// it is excluded, and the card says it will ask again.
func TestPhaseFullySpecified_aDeferredTargetIsNotCovered(t *testing.T) {
	deferring := readEditShipPlan(t, []Slot{{Type: "git_repo"}})
	assert.True(t, PhaseFullySpecified(deferring.Phases[0]), "phase 1 names its repo")
	assert.True(t, PhaseFullySpecified(deferring.Phases[1]),
		"phase 2 requests no slot at all, so it constrains the instance axis not at all")
	assert.False(t, PhaseFullySpecified(deferring.Phases[2]),
		"phase 3 declared the type and named no instance")

	named := readEditShipPlan(t, []Slot{{Type: "git_repo", ID: "https://github.com/acme/app"}})
	assert.True(t, PhaseFullySpecified(named.Phases[2]),
		"with every target named, one approval can cover the whole plan")
}
