package lifecycle_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/metaagent"
	"github.com/authzed/openagentprimitives/pkg/metaagent/capability"
	_ "github.com/authzed/openagentprimitives/pkg/metaagent/capability/lifecycle"
)

func lifecycleCap(t *testing.T) capability.Capability {
	t.Helper()
	c, ok := capability.Get("lifecycle")
	require.True(t, ok, "the blank import must register it")
	return c
}

// EVERY lifecycle action is subtractive, so every one is Narrowing and applies
// with no approval. That is the fail-safe direction: a user saying "stop"
// during an incident must not be made to wait on a second click.
func TestLifecycle_everyActionIsNarrowing(t *testing.T) {
	for _, a := range lifecycleCap(t).Actions() {
		assert.Equal(t, metaagent.Narrowing, a.Direction,
			"action %q is not subtractive; lifecycle may only ever take authority away", a.Name)

		need, err := capability.RequiresApproval(metaagent.Decision{
			Capability: "lifecycle", Action: a.Name,
		})
		require.NoError(t, err)
		assert.False(t, need, "action %q must apply immediately", a.Name)
	}
}

// Gated on `approve`, not `interact`. Without approver standing a hostile
// thread participant could cancel everyone's work — subtractive is fail-SAFE,
// not harmless.
func TestLifecycle_isGatedOnApproverStanding(t *testing.T) {
	assert.Equal(t, "approve", lifecycleCap(t).Permission())
}

func TestLifecycle_cancelEndsTheSessionAndSaysSo(t *testing.T) {
	var cancelled, notified int
	env := capability.Env{
		Session:       capability.SessionRef{Namespace: "ns", Name: "s"},
		CancelSession: func(context.Context, string) error { cancelled++; return nil },
		Notify:        func(context.Context, string) { notified++ },
	}

	err := lifecycleCap(t).Apply(context.Background(), env,
		metaagent.Decision{Capability: "lifecycle", Action: "cancel_session"})

	require.NoError(t, err)
	assert.Equal(t, 1, cancelled)
	assert.Equal(t, 1, notified,
		"an auto-applied narrowing must be visible; a session that quietly stopped "+
			"is indistinguishable from a broken one")
}

func TestLifecycle_unapprovePhaseRevokesTheNamedPhase(t *testing.T) {
	var got []int
	env := capability.Env{
		UnapprovePhase: func(_ context.Context, i int) error { got = append(got, i); return nil },
		Notify:         func(context.Context, string) {},
	}

	err := lifecycleCap(t).Apply(context.Background(), env,
		metaagent.Decision{Capability: "lifecycle", Action: "unapprove_phase", TargetIndex: 2})

	require.NoError(t, err)
	assert.Equal(t, []int{2}, got)
}

// A negative index names no phase. Refuse rather than pass it through — an
// effector handed -1 would either error obscurely or, worse, do something.
func TestLifecycle_refusesANegativePhaseIndex(t *testing.T) {
	var called int
	env := capability.Env{
		UnapprovePhase: func(context.Context, int) error { called++; return nil },
	}

	err := lifecycleCap(t).Apply(context.Background(), env,
		metaagent.Decision{Capability: "lifecycle", Action: "unapprove_phase", TargetIndex: -1})

	require.Error(t, err)
	assert.Zero(t, called)
}

// An unwired effector must be REPORTED, never silently treated as success.
// Losing an effect the user asked for and saying nothing is exactly what the
// no-silent-errors rule exists to prevent.
func TestLifecycle_anUnwiredEffectorIsReportedNotSwallowed(t *testing.T) {
	err := lifecycleCap(t).Apply(context.Background(), capability.Env{},
		metaagent.Decision{Capability: "lifecycle", Action: "cancel_session"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not wired")
}

// An effector that fails must surface its cause.
func TestLifecycle_anEffectorFailureKeepsItsCause(t *testing.T) {
	env := capability.Env{
		CancelSession: func(context.Context, string) error { return errors.New("nats unreachable") },
	}

	err := lifecycleCap(t).Apply(context.Background(), env,
		metaagent.Decision{Capability: "lifecycle", Action: "cancel_session"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "nats unreachable")
}

// An action outside the registered vocabulary reaches Apply only through a bug,
// and must not fall through to a default effect.
func TestLifecycle_anUnknownActionIsRefused(t *testing.T) {
	err := lifecycleCap(t).Apply(context.Background(), capability.Env{},
		metaagent.Decision{Capability: "lifecycle", Action: "delete_everything"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete_everything")
}

// Subtractive capabilities have no administrative ceiling to move within —
// there is no "too much stopping". Permitted must still be true, or the
// capability would be inert.
func TestLifecycle_ceilingIsPermittedAndUnbounded(t *testing.T) {
	b := lifecycleCap(t).Ceiling(metaagent.StateSnapshot{})

	assert.True(t, b.Permitted, "lifecycle is always available to someone with standing")
	assert.Zero(t, b.Max, "subtractive actions have no numeric ceiling")
}
