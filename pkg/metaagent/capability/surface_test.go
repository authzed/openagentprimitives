package capability_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/metaagent"
	"github.com/authzed/openagentprimitives/pkg/metaagent/capability"
)

// offCap is default-OFF and gated on a permission the speaker holds.
type offCap struct{ fakeCap }

func (c *offCap) DefaultOn() bool { return false }

// impermissibleCap is on and permitted-by-standing, but an admin switched it off.
type impermissibleCap struct{ fakeCap }

func (c *impermissibleCap) Ceiling(metaagent.StateSnapshot) capability.Bound {
	return capability.Bound{Permitted: false}
}

func holds(perms ...string) capability.PermissionChecker {
	set := map[string]struct{}{}
	for _, p := range perms {
		set[p] = struct{}{}
	}
	return func(_ context.Context, p string) (bool, error) {
		_, ok := set[p]
		return ok, nil
	}
}

// A capability the speaker holds standing for is offered, with its actions.
func TestSurfaceFor_offersWhatTheSpeakerHoldsStandingFor(t *testing.T) {
	registerFake(t, "surf_ok")
	// Its Ceiling is permitted by default in fakeCap? No — assert explicitly.
	got, skipped, err := capability.SurfaceFor(context.Background(),
		metaagent.StateSnapshot{}, holds("manage_scope"), []string{"surf_ok"})
	require.NoError(t, err)

	var names []string
	for _, a := range got {
		if a.Capability == "surf_ok" {
			names = append(names, a.Name)
		}
	}
	assert.ElementsMatch(t, []string{"narrow", "widen"}, names)
	assert.Empty(t, skippedFor(skipped, "surf_ok"))
}

// A speaker WITHOUT the capability's permission is not offered it — and the
// exclusion is RECORDED, not silently dropped. Their utterance becomes a no-op
// with a notice, so they learn why nothing happened instead of concluding the
// feature is broken.
func TestSurfaceFor_recordsWhyAStandingFailureExcludedACapability(t *testing.T) {
	registerFake(t, "surf_nostanding")

	got, skipped, err := capability.SurfaceFor(context.Background(),
		metaagent.StateSnapshot{}, holds(), []string{"surf_nostanding"})
	require.NoError(t, err)

	assert.Empty(t, actionsFor(got, "surf_nostanding"))
	s := skippedFor(skipped, "surf_nostanding")
	require.NotEmpty(t, s, "an exclusion must be explainable, never silent")
	assert.Contains(t, s[0].Reason, "manage_scope", "name the permission they lack")
}

// An admin switch-off is refused regardless of standing, and is a DIFFERENT
// reason from lacking standing — telling a user "you don't have permission"
// when an administrator disabled the feature sends them to the wrong person.
func TestSurfaceFor_distinguishesAnAdminSwitchOffFromAStandingFailure(t *testing.T) {
	c := &impermissibleCap{fakeCap{name: "surf_off"}}
	capability.Register(c)
	t.Cleanup(func() { capability.Unregister("surf_off") })

	got, skipped, err := capability.SurfaceFor(context.Background(),
		metaagent.StateSnapshot{}, holds("manage_scope"), []string{"surf_off"})
	require.NoError(t, err)

	assert.Empty(t, actionsFor(got, "surf_off"))
	s := skippedFor(skipped, "surf_off")
	require.NotEmpty(t, s)
	assert.Contains(t, s[0].Reason, "administrator")
	assert.NotContains(t, s[0].Reason, "manage_scope",
		"this is not a standing problem; saying so would send the user to the wrong person")
}

// A default-off capability is offered only when explicitly enabled.
func TestSurfaceFor_defaultOffRequiresExplicitEnabling(t *testing.T) {
	c := &offCap{fakeCap{name: "surf_defoff"}}
	capability.Register(c)
	t.Cleanup(func() { capability.Unregister("surf_defoff") })

	got, _, err := capability.SurfaceFor(context.Background(),
		metaagent.StateSnapshot{}, holds("manage_scope"), nil)
	require.NoError(t, err)
	assert.Empty(t, actionsFor(got, "surf_defoff"), "default-off stays off when unlisted")

	got, _, err = capability.SurfaceFor(context.Background(),
		metaagent.StateSnapshot{}, holds("manage_scope"), []string{"surf_defoff"})
	require.NoError(t, err)
	assert.NotEmpty(t, actionsFor(got, "surf_defoff"))
}

// A standing check that ERRORS excludes the capability — it does not guess.
// Treating an unreachable SpiceDB as "they probably hold it" would offer
// actions nobody verified they may invoke.
func TestSurfaceFor_aFailedStandingCheckExcludesRatherThanAssumes(t *testing.T) {
	registerFake(t, "surf_err")
	check := func(context.Context, string) (bool, error) {
		return false, errors.New("spicedb unreachable")
	}

	got, skipped, err := capability.SurfaceFor(context.Background(),
		metaagent.StateSnapshot{}, check, []string{"surf_err"})
	require.NoError(t, err, "one capability's lookup failing must not fail the whole surface")

	assert.Empty(t, actionsFor(got, "surf_err"))
	s := skippedFor(skipped, "surf_err")
	require.NotEmpty(t, s)
	assert.Contains(t, s[0].Reason, "spicedb unreachable", "the cause must not be swallowed")
}

func actionsFor(as []metaagent.Action, cap string) []metaagent.Action {
	var out []metaagent.Action
	for _, a := range as {
		if a.Capability == cap {
			out = append(out, a)
		}
	}
	return out
}

func skippedFor(ss []capability.Skipped, cap string) []capability.Skipped {
	var out []capability.Skipped
	for _, s := range ss {
		if s.Capability == cap {
			out = append(out, s)
		}
	}
	return out
}
