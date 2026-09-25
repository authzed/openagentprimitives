package capability_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/metaagent"
	"github.com/authzed/openagentprimitives/pkg/metaagent/capability"
)

func surfaceOf(cap string, names ...string) []metaagent.Action {
	var out []metaagent.Action
	for _, n := range names {
		dir := metaagent.Narrowing
		if n == "widen" {
			dir = metaagent.Widening
		}
		out = append(out, metaagent.Action{Capability: cap, Name: n, Direction: dir})
	}
	return out
}

// THE bypass this closes. The surface is standing-gated: SurfaceFor already
// excluded every capability the speaker lacks permission for. If a decision
// naming an action that is NOT on the surface were applied, the standing gate
// would be bypassed entirely — a classifier that hallucinated (or was steered
// into) "lifecycle/cancel_session" would cancel a session for a speaker who was
// never offered it.
//
// The model picks FROM the surface; it does not get to name something outside
// it.
func TestResolve_refusesAnActionTheSpeakerWasNeverOffered(t *testing.T) {
	registerFake(t, "res_offside")

	in := metaagent.Input{
		Turn:    "cancel everything",
		Surface: surfaceOf("res_offside", "narrow"), // widen NOT offered
	}

	out, err := capability.Resolve(in, metaagent.Decision{
		Capability: "res_offside", Action: "widen",
	})
	require.NoError(t, err)

	assert.False(t, out.Apply)
	assert.NotEmpty(t, out.Refused, "an off-surface action must be refused with a reason")
	assert.Contains(t, out.Refused, "not offered")
}

// A narrowing that WAS offered applies with no approval.
func TestResolve_anOfferedNarrowingApplies(t *testing.T) {
	registerFake(t, "res_narrow")

	out, err := capability.Resolve(
		metaagent.Input{Surface: surfaceOf("res_narrow", "narrow")},
		metaagent.Decision{Capability: "res_narrow", Action: "narrow"})
	require.NoError(t, err)

	assert.True(t, out.Apply)
	assert.False(t, out.RequiresApproval)
	assert.Empty(t, out.Refused)
}

// A widening that was offered still needs a human.
func TestResolve_anOfferedWideningNeedsApproval(t *testing.T) {
	registerFake(t, "res_widen")

	out, err := capability.Resolve(
		metaagent.Input{Surface: surfaceOf("res_widen", "narrow", "widen")},
		metaagent.Decision{Capability: "res_widen", Action: "widen"})
	require.NoError(t, err)

	assert.True(t, out.RequiresApproval)
	assert.False(t, out.Apply, "a widening does not apply until somebody approves it")
}

// The surface entry's own Direction is NOT trusted either — it is re-resolved
// from the registry. The surface is runtime-built today, but it travels in
// Input alongside the untrusted turn, and a single place deciding direction is
// worth more than an assumption about who built which field.
func TestResolve_takesDirectionFromTheRegistryNotTheSurfaceEntry(t *testing.T) {
	registerFake(t, "res_lying")

	// A surface entry that claims the widening action is narrowing.
	lying := []metaagent.Action{
		{Capability: "res_lying", Name: "widen", Direction: metaagent.Narrowing},
	}

	out, err := capability.Resolve(
		metaagent.Input{Surface: lying},
		metaagent.Decision{Capability: "res_lying", Action: "widen"})
	require.NoError(t, err)

	assert.True(t, out.RequiresApproval,
		"the registry says widen; a surface entry claiming otherwise is not an input")
}

// An action ON the surface but absent from the registry errors rather than
// resolving to anything.
//
// That is an inconsistent state — the surface is built FROM the registry — so
// it means the capability was unregistered between building the surface and
// resolving against it. Erroring is right: there is no direction to look up,
// and defaulting to narrowing would apply an unknown action with no approval.
//
// Note the ORDERING this documents: the surface check runs first, so an action
// that is neither offered nor registered is REFUSED (a user-facing no-op with a
// reason) rather than erroring. The standing gate is the stronger statement and
// the one worth telling the speaker about.
func TestResolve_anOnSurfaceButUnregisteredActionErrors(t *testing.T) {
	surface := []metaagent.Action{
		{Capability: "res_vanished", Name: "narrow", Direction: metaagent.Narrowing},
	}

	_, err := capability.Resolve(
		metaagent.Input{Surface: surface},
		metaagent.Decision{Capability: "res_vanished", Action: "narrow"})

	require.Error(t, err, "no registry entry means no direction; it must not default")
}

// An empty surface offers nothing, so nothing resolves — the shape a speaker
// with no standing at all produces.
func TestResolve_anEmptySurfaceRefusesEverything(t *testing.T) {
	registerFake(t, "res_empty")

	out, err := capability.Resolve(metaagent.Input{},
		metaagent.Decision{Capability: "res_empty", Action: "narrow"})
	require.NoError(t, err)

	assert.False(t, out.Apply)
	assert.NotEmpty(t, out.Refused)
}
