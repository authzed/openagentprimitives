package plangate

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

func handlesOf(t *testing.T, si authz.StateImpact, n int) []HandleImpact {
	t.Helper()
	out := make([]HandleImpact, 0, n)
	for i := 0; i < n; i++ {
		h, err := permsurface.NewPermHandle(fmt.Sprintf("op%d", i), "tracker_issue")
		require.NoError(t, err)
		out = append(out, HandleImpact{Handle: h, StateImpact: si})
	}
	return out
}

func TestComputeTier(t *testing.T) {
	cases := []struct {
		name string
		in   TierInput
		want Tier
	}{
		{
			name: "all readonly, within budget: tier 0 auto-approves",
			in:   TierInput{Handles: handlesOf(t, authz.Readonly, 3), MaxAutoApproveHandles: 8},
			want: Tier0,
		},
		{
			// A phase declaring nothing has no ceiling to auto-approve. Letting
			// it through at tier 0 would make "declare nothing" the cheapest
			// possible plan, which is the opposite of the incentive.
			name: "ZERO handles declared: never tier 0",
			in:   TierInput{Handles: nil, MaxAutoApproveHandles: 8},
			want: Tier1,
		},
		{
			name: "any readwrite: tier 1",
			in:   TierInput{Handles: handlesOf(t, authz.Readwrite, 1), MaxAutoApproveHandles: 8},
			want: Tier1,
		},
		{
			name: "any external: tier 2",
			in:   TierInput{Handles: handlesOf(t, authz.External, 1), MaxAutoApproveHandles: 8},
			want: Tier2,
		},
		{
			name: "readonly but past maxAutoApproveHandles: not tier 0",
			in:   TierInput{Handles: handlesOf(t, authz.Readonly, 9), MaxAutoApproveHandles: 8},
			want: Tier1,
		},
		{
			name: "readonly, but the SESSION budget is already spent: not tier 0",
			in: TierInput{
				Handles:               handlesOf(t, authz.Readonly, 2),
				MaxAutoApproveHandles: 8,
				AlreadyAutoApproved:   8,
			},
			want: Tier1,
		},
		{
			name: "readonly, and this phase would exactly exhaust the budget: still tier 0",
			in: TierInput{
				Handles:               handlesOf(t, authz.Readonly, 3),
				MaxAutoApproveHandles: 8,
				AlreadyAutoApproved:   5,
			},
			want: Tier0,
		},
		{
			name: "readonly, and this phase would overrun the budget by one: not tier 0",
			in: TierInput{
				Handles:               handlesOf(t, authz.Readonly, 4),
				MaxAutoApproveHandles: 8,
				AlreadyAutoApproved:   5,
			},
			want: Tier1,
		},
		{
			// Severity dominates: a single external handle in an otherwise
			// readonly phase makes the whole phase tier 2.
			name: "mostly readonly with one external: tier 2",
			in: TierInput{
				Handles:               append(handlesOf(t, authz.Readonly, 3), handlesOf(t, authz.External, 1)...),
				MaxAutoApproveHandles: 8,
			},
			want: Tier2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ComputeTier(tc.in))
		})
	}
}

// THE inversion audit found, pinned.
//
// Tiering by max(StateImpact) alone makes one phase with forty readwrite
// handles and one phase with a single handle both "one card, one click". Since
// phase-level ceilings cost the agent something real (it holds one phase at a
// time) and each extra plan costs the human another card, the dominant strategy
// for ANY agent — cooperative or hostile — becomes a single phase containing
// the entire surface. After that the active phase never changes, "one phase at
// a time" is vacuously satisfied, and the effective authorization is identical
// to today's session-wide scope plus one card.
//
// A design whose own incentives push toward its degenerate case is not a
// control. So breadth escalates independently of severity.
func TestComputeTier_breadthEscalatesIndependentlyOfSeverity(t *testing.T) {
	narrow := TierInput{Handles: handlesOf(t, authz.Readonly, 3), MaxAutoApproveHandles: 8, MaxSingleCardHandles: 16}
	wide := TierInput{Handles: handlesOf(t, authz.Readonly, 40), MaxAutoApproveHandles: 8, MaxSingleCardHandles: 16}

	assert.Equal(t, Tier0, ComputeTier(narrow), "a narrow readonly phase is the cheap path")
	assert.NotEqual(t, Tier0, ComputeTier(wide), "a wide phase must cost more, even at the same severity")

	assert.False(t, RendersPerHandle(narrow), "a narrow phase may be approved from a summary")
	assert.True(t, RendersPerHandle(wide),
		"a phase past maxSingleCardHandles must render per-handle so breadth is acknowledged explicitly")
}

// The gradient the incentive depends on: narrow is cheaper than wide at every
// severity. This is what stops a wide plan from being the rational choice.
func TestComputeTier_narrowIsNeverMoreExpensiveThanWide(t *testing.T) {
	for _, si := range []authz.StateImpact{authz.Readonly, authz.Readwrite, authz.External} {
		t.Run(string(si)+": narrow ≤ wide", func(t *testing.T) {
			narrow := ComputeTier(TierInput{Handles: handlesOf(t, si, 2), MaxAutoApproveHandles: 8})
			wide := ComputeTier(TierInput{Handles: handlesOf(t, si, 40), MaxAutoApproveHandles: 8})
			assert.LessOrEqual(t, int(narrow), int(wide))
		})
	}
}

// Tier 2 is not a shortcut. `external` means "re-approve per call,
// irreversible side effects"; approving the phase never consumes that.
func TestTier2_doesNotConsumeThePerCallExternalApproval(t *testing.T) {
	in := TierInput{Handles: handlesOf(t, authz.External, 1), MaxAutoApproveHandles: 8}

	require.Equal(t, Tier2, ComputeTier(in))
	assert.True(t, StillNeedsPerCallApproval(in),
		"a tier-2 phase approval must not stand in for the per-call external approval")
}

func TestTier_autoApprovesOnlyAtTierZero(t *testing.T) {
	assert.True(t, Tier0.AutoApproves())
	assert.False(t, Tier1.AutoApproves())
	assert.False(t, Tier2.AutoApproves())
}

// A zero budget must not read as "unlimited". Fail toward asking a human.
func TestComputeTier_zeroBudgetMeansNoAutoApproval(t *testing.T) {
	got := ComputeTier(TierInput{Handles: handlesOf(t, authz.Readonly, 1), MaxAutoApproveHandles: 0})
	assert.NotEqual(t, Tier0, got, "an unset budget must not silently enable auto-approval")
}

// An unrecognized StateImpact is treated as the most severe, never the least:
// a new impact level added upstream must not silently auto-approve until
// somebody remembers to update this table.
func TestComputeTier_unknownStateImpactFailsClosed(t *testing.T) {
	h, err := permsurface.NewPermHandle("op", "tracker_issue")
	require.NoError(t, err)

	got := ComputeTier(TierInput{
		Handles:               []HandleImpact{{Handle: h, StateImpact: authz.StateImpact("teleport")}},
		MaxAutoApproveHandles: 8,
	})
	assert.Equal(t, Tier2, got, "an unknown impact must price as the most severe")
}
