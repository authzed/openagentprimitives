package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

func permDesc(t *testing.T, permission, resourceType string, si authz.StateImpact) permsurface.Descriptor {
	t.Helper()
	h, err := permsurface.NewPermHandle(permission, resourceType)
	require.NoError(t, err)
	return permsurface.Descriptor{
		Handle:       h,
		Permission:   permission,
		ResourceType: resourceType,
		StateImpact:  si,
	}
}

func toolDesc(t *testing.T, name string, si authz.StateImpact) permsurface.Descriptor {
	t.Helper()
	h, err := permsurface.NewToolHandle(name)
	require.NoError(t, err)
	return permsurface.Descriptor{Handle: h, ToolName: name, StateImpact: si}
}

func handle(t *testing.T, permission, resourceType string) permsurface.Handle {
	t.Helper()
	h, err := permsurface.NewPermHandle(permission, resourceType)
	require.NoError(t, err)
	return h
}

func phaseWith(t *testing.T, permissions ...string) Phase {
	t.Helper()
	p := Phase{Why: "because", Max: MaxSpec{Count: 1}}
	for _, perm := range permissions {
		p.Permissions = append(p.Permissions, handle(t, perm, "tracker_issue"))
	}
	return p
}

func demoSurface(t *testing.T) []permsurface.Descriptor {
	t.Helper()
	return []permsurface.Descriptor{
		permDesc(t, "read", "tracker_issue", authz.Readonly),
		permDesc(t, "write", "tracker_issue", authz.Readwrite),
		toolDesc(t, "apply_workspace", authz.External),
	}
}

// Slice 1's entire safety argument: the synthesized session plan's phase 0
// ceiling is the whole surface, so membership can never deny and behavior is
// provably unchanged.
func TestSessionPlan_ceilingIsTheWholeSurface(t *testing.T) {
	surface := demoSurface(t)

	p := SessionPlan(surface)
	require.Len(t, p.Phases, 1, "the seed plan synthesizes exactly one phase")

	ceiling, err := p.Ceiling(0)
	require.NoError(t, err)
	assert.Len(t, ceiling, len(surface))
	for _, d := range surface {
		assert.Contains(t, ceiling, d.Handle,
			"every surface handle must be in phase 0's ceiling — else enabling the gate changes behavior")
	}
}

func TestSessionPlan_emptySurfaceStillYieldsOnePhase(t *testing.T) {
	p := SessionPlan(nil)
	require.Len(t, p.Phases, 1)

	ceiling, err := p.Ceiling(0)
	require.NoError(t, err)
	assert.Empty(t, ceiling, "an agent with no permissioned tools has an empty ceiling, not no phase")
}

// A zero-value Plan has no phases, so every index is out of range. The gate
// must get an error it can log, never a silent empty ceiling that would read
// as "deny everything" under enforcing.
func TestPlan_ceilingRejectsOutOfRangeIndex(t *testing.T) {
	cases := []struct {
		name  string
		plan  Plan
		index int
	}{
		{"negative index on a real plan", SessionPlan(nil), -1},
		{"index past the end", SessionPlan(nil), 1},
		{"far past the end", SessionPlan(nil), 99},
		{"any index on an empty plan", Plan{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name+": returns an error", func(t *testing.T) {
			_, err := tc.plan.Ceiling(tc.index)
			assert.Error(t, err)
		})
	}
}

// Index IS phase identity, and `requires` edges name indices, so reordering
// two phases produces a genuinely different plan.
func TestPlan_digestIsOrderSensitive(t *testing.T) {
	a := Plan{Phases: []Phase{phaseWith(t, "read"), phaseWith(t, "write")}}
	b := Plan{Phases: []Phase{phaseWith(t, "write"), phaseWith(t, "read")}}
	assert.NotEqual(t, a.Digest(), b.Digest())
}

// The digest covers AUTHORITY, not narration. `why` is agent-authored text
// with zero authz weight; if it perturbed the digest, re-wording a
// justification would read as a new plan and fire a spurious supersede card.
func TestPlan_digestIgnoresEveryWhy(t *testing.T) {
	base := Plan{Phases: []Phase{{
		Why:         "original phase reason",
		Permissions: []permsurface.Handle{handle(t, "read", "tracker_issue")},
		Max:         MaxSpec{Count: 1, Why: "original max reason"},
		Requires:    []RequiresEdge{{Phase: 0, Why: "original edge reason"}},
	}}}

	reworded := Plan{Phases: []Phase{{
		Why:         "COMPLETELY different phase reason",
		Permissions: []permsurface.Handle{handle(t, "read", "tracker_issue")},
		Max:         MaxSpec{Count: 1, Why: "COMPLETELY different max reason"},
		Requires:    []RequiresEdge{{Phase: 0, Why: "COMPLETELY different edge reason"}},
	}}}

	assert.Equal(t, base.Digest(), reworded.Digest(),
		"all three why fields are narration and must not move the digest")
}

// The other half of the same contract: everything that IS authority must move
// it. A digest blind to a budget or a prerequisite would let an agent widen
// its authority under an unchanged digest.
func TestPlan_digestCoversEveryAuthorityField(t *testing.T) {
	base := Plan{Phases: []Phase{{
		Permissions: []permsurface.Handle{handle(t, "read", "tracker_issue")},
		Max:         MaxSpec{Count: 1},
		Requires:    []RequiresEdge{{Phase: 0}},
	}}}

	cases := []struct {
		name string
		mut  func(p *Plan)
	}{
		{"adding a permission", func(p *Plan) {
			p.Phases[0].Permissions = append(p.Phases[0].Permissions, handle(t, "write", "tracker_issue"))
		}},
		{"removing a permission", func(p *Plan) {
			p.Phases[0].Permissions = nil
		}},
		{"raising the max count", func(p *Plan) {
			p.Phases[0].Max.Count = 5
		}},
		{"adding a requires edge", func(p *Plan) {
			p.Phases[0].Requires = append(p.Phases[0].Requires, RequiresEdge{Phase: 1})
		}},
		{"removing a requires edge", func(p *Plan) {
			p.Phases[0].Requires = nil
		}},
		{"adding a phase", func(p *Plan) {
			p.Phases = append(p.Phases, phaseWith(t, "write"))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name+": changes the digest", func(t *testing.T) {
			mutated := clonePlan(t, base)
			tc.mut(&mutated)
			assert.NotEqual(t, base.Digest(), mutated.Digest())
		})
	}
}

// Permission ORDER within a phase is not authority — the ceiling is a set.
// Two plans differing only in declaration order authorize identically and must
// digest identically, or a cosmetic re-sort would read as a supersede.
func TestPlan_digestIgnoresPermissionOrderWithinAPhase(t *testing.T) {
	a := Plan{Phases: []Phase{{Permissions: []permsurface.Handle{
		handle(t, "read", "tracker_issue"), handle(t, "write", "tracker_issue"),
	}}}}
	b := Plan{Phases: []Phase{{Permissions: []permsurface.Handle{
		handle(t, "write", "tracker_issue"), handle(t, "read", "tracker_issue"),
	}}}}
	assert.Equal(t, a.Digest(), b.Digest())
}

func TestPlan_digestIsStableAcrossCalls(t *testing.T) {
	p := SessionPlan(demoSurface(t))
	assert.Equal(t, p.Digest(), p.Digest())
}

func TestPlan_emptyPlanDigestsWithoutPanicking(t *testing.T) {
	assert.NotEmpty(t, Plan{}.Digest())
	assert.NotEqual(t, Plan{}.Digest(), SessionPlan(demoSurface(t)).Digest())
}

func clonePlan(t *testing.T, p Plan) Plan {
	t.Helper()
	out := Plan{Phases: make([]Phase, len(p.Phases))}
	for i, ph := range p.Phases {
		cp := ph
		cp.Permissions = append([]permsurface.Handle(nil), ph.Permissions...)
		cp.Requires = append([]RequiresEdge(nil), ph.Requires...)
		out.Phases[i] = cp
	}
	return out
}
