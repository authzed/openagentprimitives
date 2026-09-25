package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

func authored(id, label string, handles ...string) AuthoredPhase {
	ph := AuthoredPhase{ID: id, Label: label, Why: "because " + id}
	for _, h := range handles {
		ph.Permissions = append(ph.Permissions, AuthoredPermission{Handle: h, Why: "need " + h})
	}
	return ph
}

func surfaceWith(t *testing.T, handles ...string) []permsurface.Descriptor {
	t.Helper()
	out := make([]permsurface.Descriptor, 0, len(handles))
	for _, s := range handles {
		h, err := permsurface.ParseHandle(s)
		require.NoError(t, err, "fixture handle %q must parse", s)
		out = append(out, permsurface.Descriptor{Handle: h, StateImpact: authz.Readonly})
	}
	return out
}

// Order is authority: requires-edges name indices, so a permuted re-submit is
// a different plan and must not be able to move the agent's position.
func TestFreezeFrom_preservesDeclarationOrder(t *testing.T) {
	surface := surfaceWith(t, "perm:read:tracker_issue", "perm:write:tracker_issue")

	got, probs := FreezeFrom([]AuthoredPhase{
		authored("recon", "Read", "perm:read:tracker_issue"),
		authored("write", "Write", "perm:write:tracker_issue"),
	}, surface, nil)

	require.Empty(t, probs)
	require.Len(t, got.Phases, 2)
	assert.Equal(t, "Read", got.Phases[0].Label)
	assert.Equal(t, "Write", got.Phases[1].Label)
}

// The frozen phase carries NO agent-chosen id, by construction. Identity is
// (digest, index); dropping the field means no future code CAN key on the
// agent's name for a phase, rather than merely being told not to.
func TestFreezeFrom_dropsTheAgentChosenID(t *testing.T) {
	got, _ := FreezeFrom([]AuthoredPhase{authored("recon", "Read")}, nil, nil)
	require.Len(t, got.Phases, 1)

	// Label survives for display; there is no ID field to read.
	assert.Equal(t, "Read", got.Phases[0].Label)
}

// The surface is the authority on what exists. A handle the agent names that
// is not on it is DROPPED with a problem reported — never minted, because a
// handle absent from the surface is one the dispatcher will never check.
func TestFreezeFrom_dropsHandlesNotOnTheSurface(t *testing.T) {
	surface := surfaceWith(t, "perm:read:tracker_issue")

	got, probs := FreezeFrom([]AuthoredPhase{
		authored("p", "P", "perm:read:tracker_issue", "perm:write:secret_vault"),
	}, surface, nil)

	require.Len(t, got.Phases, 1)
	assert.Len(t, got.Phases[0].Permissions, 1, "only the on-surface handle survives")
	assert.Equal(t, "perm:read:tracker_issue", got.Phases[0].Permissions[0].String())

	require.Len(t, probs, 1)
	assert.Contains(t, probs[0].Detail, "perm:write:secret_vault")
	assert.Contains(t, probs[0].Detail, "surface")
}

// A syntactically invalid handle is rejected the same way — never sanitized
// into something adjacent, which is the collision permsurface exists to stop.
func TestFreezeFrom_dropsUnparseableHandles(t *testing.T) {
	got, probs := FreezeFrom([]AuthoredPhase{
		authored("p", "P", "not a handle", "perm:BAD:Type"),
	}, surfaceWith(t, "perm:read:tracker_issue"), nil)

	require.Len(t, got.Phases, 1)
	assert.Empty(t, got.Phases[0].Permissions)
	assert.Len(t, probs, 2, "each bad handle reports separately so the agent can fix both")
}

// Requires ids resolve to INDICES exactly once, at freeze time, so the gate
// never resolves an agent-supplied name at decision time.
func TestFreezeFrom_resolvesRequiresIDsToIndices(t *testing.T) {
	a := authored("recon", "Read")
	b := authored("write", "Write")
	b.Requires = []AuthoredRequires{{Phase: "recon", Why: "must read first"}}

	got, probs := FreezeFrom([]AuthoredPhase{a, b}, nil, nil)

	require.Empty(t, probs)
	require.Len(t, got.Phases[1].Requires, 1)
	assert.Equal(t, 0, got.Phases[1].Requires[0].Phase, "resolved to recon's index")
	assert.Equal(t, "must read first", got.Phases[1].Requires[0].Why)
}

// Forward references are legal — a phase may require one declared later.
func TestFreezeFrom_resolvesAForwardRequires(t *testing.T) {
	a := authored("first", "First")
	a.Requires = []AuthoredRequires{{Phase: "second", Why: "odd but legal"}}

	got, probs := FreezeFrom([]AuthoredPhase{a, authored("second", "Second")}, nil, nil)

	require.Empty(t, probs)
	require.Len(t, got.Phases[0].Requires, 1)
	assert.Equal(t, 1, got.Phases[0].Requires[0].Phase)
}

// update_plan already refuses an unresolvable edge, but freezing must not
// TRUST that: it runs on whatever reaches it, and a dangling edge silently
// dropped would quietly remove an ordering constraint the human approved.
func TestFreezeFrom_reportsAnUnresolvableRequiresEdge(t *testing.T) {
	a := authored("p", "P")
	a.Requires = []AuthoredRequires{{Phase: "ghost", Why: "x"}}

	got, probs := FreezeFrom([]AuthoredPhase{a}, nil, nil)

	require.Len(t, probs, 1)
	assert.Contains(t, probs[0].Detail, "ghost")
	assert.Empty(t, got.Phases[0].Requires, "an unresolvable edge is dropped, not guessed at")
}

// Max defaults to 1 when the agent declares nothing, so one-shot is the norm
// and anything else is an argued exception.
func TestFreezeFrom_maxDefaultsToOne(t *testing.T) {
	got, _ := FreezeFrom([]AuthoredPhase{authored("p", "P")}, nil, nil)
	assert.Equal(t, 1, got.Phases[0].Max.Count)
}

func TestFreezeFrom_maxCarriesAnExplicitCount(t *testing.T) {
	a := authored("p", "P")
	a.Max = &AuthoredMax{Count: 3, Why: "retries expected"}

	got, _ := FreezeFrom([]AuthoredPhase{a}, nil, nil)

	assert.Equal(t, 3, got.Phases[0].Max.Count)
	assert.Equal(t, "retries expected", got.Phases[0].Max.Why)
}

// A PhaseRef is (digest, index). An approval for one plan must never resolve
// against another, or an approval could be transplanted by re-submitting a
// differently-shaped plan whose phase happens to sit at the same index.
func TestPhaseRef_isNotTransplantableBetweenPlans(t *testing.T) {
	surface := surfaceWith(t, "perm:read:tracker_issue", "perm:write:tracker_issue")

	narrow, _ := FreezeFrom([]AuthoredPhase{authored("p", "P", "perm:read:tracker_issue")}, surface, nil)
	wide, _ := FreezeFrom([]AuthoredPhase{
		authored("p", "P", "perm:read:tracker_issue", "perm:write:tracker_issue"),
	}, surface, nil)

	a := PhaseRef{PlanDigest: narrow.Digest(), Index: 0}
	b := PhaseRef{PlanDigest: wide.Digest(), Index: 0}

	assert.NotEqual(t, a, b, "same index, different authority — must not be the same ref")
	assert.False(t, a.SamePlan(b))
}

func TestPhaseRef_samePlanSameIndexIsEqual(t *testing.T) {
	p, _ := FreezeFrom([]AuthoredPhase{authored("p", "P")}, nil, nil)
	assert.Equal(t, PhaseRef{p.Digest(), 0}, PhaseRef{p.Digest(), 0})
}

// Freezing the same authored plan twice yields the same digest — otherwise
// every re-approval would read as a supersede.
func TestFreezeFrom_isDeterministic(t *testing.T) {
	in := []AuthoredPhase{authored("a", "A", "perm:read:tracker_issue"), authored("b", "B")}
	surface := surfaceWith(t, "perm:read:tracker_issue")

	first, _ := FreezeFrom(in, surface, nil)
	second, _ := FreezeFrom(in, surface, nil)

	assert.Equal(t, first.Digest(), second.Digest())
}

// Re-wording a justification must NOT change the digest, or an agent tidying
// its own prose would fire a spurious supersede card and train approvers to
// click through them.
func TestFreezeFrom_rewordingWhyDoesNotChangeTheDigest(t *testing.T) {
	surface := surfaceWith(t, "perm:read:tracker_issue")

	a := authored("p", "P", "perm:read:tracker_issue")
	b := authored("p", "P", "perm:read:tracker_issue")
	b.Why = "a completely different explanation"
	b.Permissions[0].Why = "and a different one here too"

	fa, _ := FreezeFrom([]AuthoredPhase{a}, surface, nil)
	fb, _ := FreezeFrom([]AuthoredPhase{b}, surface, nil)

	assert.Equal(t, fa.Digest(), fb.Digest())
}

func TestFreezeFrom_emptyInputYieldsAnEmptyPlan(t *testing.T) {
	got, probs := FreezeFrom(nil, nil, nil)
	assert.Empty(t, got.Phases)
	assert.Empty(t, probs)
}

// Label is display, not authority, so it does not move the digest — the same
// reasoning as Why. A relabel with an identical ceiling grants nothing, and
// making it read as a supersede would fire a card for a cosmetic edit.
//
// The cost is real and worth stating: an agent CAN relabel a phase the human
// already approved without a fresh approval, so the label in the audit trail
// is the agent's latest wording rather than the wording that was approved.
// That is acceptable because the label cannot widen anything — but a reader of
// the trail should know the ceiling is the durable part, not the prose.
func TestFreezeFrom_relabellingDoesNotChangeTheDigest(t *testing.T) {
	surface := surfaceWith(t, "perm:read:tracker_issue")

	a, _ := FreezeFrom([]AuthoredPhase{authored("p", "Read the issue", "perm:read:tracker_issue")}, surface, nil)
	b, _ := FreezeFrom([]AuthoredPhase{authored("p", "Something else entirely", "perm:read:tracker_issue")}, surface, nil)

	assert.Equal(t, a.Digest(), b.Digest())
	assert.NotEqual(t, a.Phases[0].Label, b.Phases[0].Label, "the label itself did change")
}

// Changing a phase's ID must not change the digest either — the id is dropped
// at freeze time, so two plans differing only in what the agent called its
// phases are the same plan.
func TestFreezeFrom_renamingAPhaseIDDoesNotChangeTheDigest(t *testing.T) {
	surface := surfaceWith(t, "perm:read:tracker_issue")

	a, _ := FreezeFrom([]AuthoredPhase{authored("recon", "Read", "perm:read:tracker_issue")}, surface, nil)
	b, _ := FreezeFrom([]AuthoredPhase{authored("investigate", "Read", "perm:read:tracker_issue")}, surface, nil)

	assert.Equal(t, a.Digest(), b.Digest())
}

// But a requires-edge that RESOLVES DIFFERENTLY does change the digest, even
// though the ids look similar — because the edge is stored as an index and the
// index is authority.
func TestFreezeFrom_aDifferentlyResolvedRequiresChangesTheDigest(t *testing.T) {
	x, y := authored("a", "A"), authored("b", "B")
	y.Requires = []AuthoredRequires{{Phase: "a", Why: "w"}}
	forward, _ := FreezeFrom([]AuthoredPhase{x, y}, nil, nil)

	x2, y2 := authored("a", "A"), authored("b", "B")
	x2.Requires = []AuthoredRequires{{Phase: "b", Why: "w"}}
	backward, _ := FreezeFrom([]AuthoredPhase{x2, y2}, nil, nil)

	assert.NotEqual(t, forward.Digest(), backward.Digest(),
		"which phase requires which is authority and must move the digest")
}
