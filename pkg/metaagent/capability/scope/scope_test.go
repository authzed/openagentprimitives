package scope_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/metaagent"
	"github.com/authzed/openagentprimitives/pkg/metaagent/capability"
	_ "github.com/authzed/openagentprimitives/pkg/metaagent/capability/scope"
)

func scopeCap(t *testing.T) capability.Capability {
	t.Helper()
	c, ok := capability.Get("scope")
	require.True(t, ok, "the blank import must register it")
	return c
}

// Narrowing auto-applies; widening always needs approval. The registry is what
// enforces that, but the DECLARATION is the security-critical part — a widen
// mislabelled narrowing would bypass approval entirely.
func TestScope_declaresHonestDirections(t *testing.T) {
	byName := map[string]metaagent.Direction{}
	for _, a := range scopeCap(t).Actions() {
		byName[a.Name] = a.Direction
	}

	assert.Equal(t, metaagent.Narrowing, byName["narrow"])
	assert.Equal(t, metaagent.Widening, byName["widen"],
		"widening scope hands the session reach it did not have; it can never auto-apply")
}

func TestScope_isGatedOnManageScope(t *testing.T) {
	assert.Equal(t, "manage_scope", scopeCap(t).Permission())
}

// A narrowing runs the existing chain and touches NOTHING else. Narrowing the
// session must not supersede the plan: the phase ceiling is already a narrowing
// gate, and rewriting the plan to remove reach the agent may still legitimately
// hold in a later phase would break work nobody asked to stop.
func TestScope_narrowingRunsTheChainAndLeavesThePlanAlone(t *testing.T) {
	var ran, widened int
	env := capability.Env{
		ApplyScope: func(context.Context) ([]string, error) { ran++; return nil, nil },
		WidenActivePhase: func(context.Context, []string) error {
			widened++
			return nil
		},
		Notify: func(context.Context, string) {},
	}

	err := scopeCap(t).Apply(context.Background(), env,
		metaagent.Decision{Capability: "scope", Action: "narrow"})

	require.NoError(t, err)
	assert.Equal(t, 1, ran)
	assert.Zero(t, widened, "a narrowing has no second bound to carry")
}

// One intent, one approval: an approved widening applies to BOTH bounds — the
// session scope and the active phase — so the user is not asked again the
// moment the agent tries to use what they just granted.
func TestScope_anApprovedWideningCarriesToTheActivePhase(t *testing.T) {
	var gotTypes []string
	env := capability.Env{
		ApplyScope: func(context.Context) ([]string, error) {
			return []string{"deploy_log"}, nil
		},
		WidenActivePhase: func(_ context.Context, ts []string) error { gotTypes = ts; return nil },
		Notify:           func(context.Context, string) {},
	}

	err := scopeCap(t).Apply(context.Background(), env,
		metaagent.Decision{Capability: "scope", Action: "widen"})

	require.NoError(t, err)
	assert.Equal(t, []string{"deploy_log"}, gotTypes,
		"the phase bound must receive exactly the types the scope bound accepted")
}

// A widening the chain ended up NOT applying (every ref skipped as out of
// envelope, say) must not widen the phase either. The two bounds move together
// or not at all — widening a phase for reach the session never got would hand
// the agent a ceiling entry nothing backs.
func TestScope_aWideningThatAppliedNothingDoesNotTouchThePhase(t *testing.T) {
	var widened int
	env := capability.Env{
		ApplyScope:       func(context.Context) ([]string, error) { return nil, nil },
		WidenActivePhase: func(context.Context, []string) error { widened++; return nil },
		Notify:           func(context.Context, string) {},
	}

	err := scopeCap(t).Apply(context.Background(), env,
		metaagent.Decision{Capability: "scope", Action: "widen"})

	require.NoError(t, err)
	assert.Zero(t, widened)
}

// If the phase half fails after the scope half succeeded, say so. The session
// is now in a split state — scope widened, phase not — and silence would leave
// the user believing one approval covered both when the agent is about to be
// denied.
func TestScope_aFailedPhaseCarryIsReportedNotSwallowed(t *testing.T) {
	env := capability.Env{
		ApplyScope: func(context.Context) ([]string, error) { return []string{"deploy_log"}, nil },
		WidenActivePhase: func(context.Context, []string) error {
			return errors.New("plan supersede rejected")
		},
	}

	err := scopeCap(t).Apply(context.Background(), env,
		metaagent.Decision{Capability: "scope", Action: "widen"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "plan supersede rejected")
	assert.Contains(t, err.Error(), "scope was widened",
		"the message must state the split so the user is not told nothing happened")
}

func TestScope_anUnwiredChainIsReportedNotSwallowed(t *testing.T) {
	err := scopeCap(t).Apply(context.Background(), capability.Env{},
		metaagent.Decision{Capability: "scope", Action: "narrow"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not wired")
}

// With no plan gate in the session there is no second bound, and a widening
// must still succeed — the phase half is an addition to the scope half, not a
// precondition for it.
func TestScope_widensScopeEvenWithNoPlanGateWired(t *testing.T) {
	env := capability.Env{
		ApplyScope: func(context.Context) ([]string, error) { return []string{"deploy_log"}, nil },
		Notify:     func(context.Context, string) {},
	}

	err := scopeCap(t).Apply(context.Background(), env,
		metaagent.Decision{Capability: "scope", Action: "widen"})

	require.NoError(t, err, "a session without the plan gate has only one bound to move")
}
