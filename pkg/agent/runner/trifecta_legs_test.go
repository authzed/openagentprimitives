package runner

// The trifecta hook is only as good as what it is told. These pin the two
// derivations the runner supplies — the session's standing legs, and the
// impact of the call in front of the gate — because a wrong answer here is a
// control that runs and concludes nothing.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/trifecta"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// legsLoop builds a Loop with the trifecta inputs wired to fixtures.
func legsLoop(tags []string, tagErr error, surface []permsurface.Descriptor) *Loop {
	l := &Loop{}
	l.SessionKey.Namespace = "demo"
	l.SessionKey.Name = "worker-7"
	l.PlanGateSurface = surface
	l.TrifectaBoundTags = func(context.Context) ([]string, error) {
		if tagErr != nil {
			return nil, tagErr
		}
		return tags, nil
	}
	l.TrifectaDeps = trifecta.Deps{
		TagCarriesUntrusted: func(context.Context, string) (bool, error) { return false, nil },
		TagReaders:          func(context.Context, string) ([]string, error) { return []string{"alice"}, nil },
		ChildAudience:       func(context.Context, authz.SessionRef) ([]string, error) { return []string{"alice"}, nil },
	}
	return l
}

func actingSurface() []permsurface.Descriptor {
	return []permsurface.Descriptor{{StateImpact: authz.Readwrite}}
}

// TestTheTrifectaHookIsBuiltOnItsOwnModeAndRunsBeforeToolCallAuthz.
//
// Two facts in one test because they are the same decision. It activates on
// trifecta.mode ALONE — a control that switches off when an operator disables
// tool-call authz is not a control — and it sits at 19, ahead of tool_call_authz
// at 20, beside the plan gate. A three-leg call is refused before the
// authorization machinery spends anything on it.
func TestTheTrifectaHookIsBuiltOnItsOwnModeAndRunsBeforeToolCallAuthz(t *testing.T) {
	build := func(mode, toolAuth string) []string {
		l := &Loop{AgentClass: &spiceboxv1alpha1.AgentClass{}}
		l.ToolAuthMode = toolAuth
		l.TrifectaMode = mode
		return hookNamesAt(l.buildPipelineRegistry(), pipeline.PreToolCall)
	}

	t.Run("enforcing: present, and ahead of tool_call_authz", func(t *testing.T) {
		pre := build("enforcing", "enforcing")
		require.Contains(t, pre, "trifecta")
		assert.Less(t, indexOfHook(pre, "trifecta"), indexOfHook(pre, "tool_call_authz"),
			"the trifecta must refuse before authorization work is spent on the call")
	})

	t.Run("its own mode: on even when tool-call authz is off", func(t *testing.T) {
		pre := build("enforcing", "disabled")
		assert.Contains(t, pre, "trifecta",
			"disabling an unrelated permission check must not disable this one")
	})

	t.Run("disabled and unset: not built at all", func(t *testing.T) {
		assert.NotContains(t, build("disabled", "enforcing"), "trifecta")
		assert.NotContains(t, build("", "enforcing"), "trifecta",
			"an unset mode is off, not on — the safe reading of a class that declared nothing")
	})
}

func indexOfHook(names []string, want string) int {
	for i, n := range names {
		if n == want {
			return i
		}
	}
	return -1
}

// TestBoundTagsAreReadLIVE.
//
// A tag can be bound into a session mid-run — a parent answering ask_parent
// hands data over after the runner started — so legs derived from a
// start-time snapshot would judge the session on data it no longer holds. The
// lookup is a function for that reason, and this asserts it is consulted at
// derivation time rather than captured.
func TestBoundTagsAreReadLIVE(t *testing.T) {
	calls := 0
	l := legsLoop(nil, nil, actingSurface())
	l.TrifectaBoundTags = func(context.Context) ([]string, error) {
		calls++
		return []string{"ptt-1"}, nil
	}

	_, err := l.trifectaStandingLegs(context.Background())
	require.NoError(t, err)
	_, err = l.trifectaStandingLegs(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 2, calls, "the bound-tag set must be read per derivation, not captured once")
}

// TestAnUnreadableBoundTagSetIsAnERROR, never empty legs.
//
// A leg that silently read false would be the trifecta failing to fire — no
// denial, no hold, and nothing anywhere to notice. The hook's own mode switch
// decides what an error means; this only refuses to invent an answer.
func TestAnUnreadableBoundTagSetIsAnERROR(t *testing.T) {
	l := legsLoop(nil, errors.New("spicedb unavailable"), actingSurface())

	_, err := l.trifectaStandingLegs(context.Background())

	require.Error(t, err, "an unresolvable input must not read as a clean leg")
	assert.Contains(t, err.Error(), "bound tags")
}

// TestAnUnwiredLookupIsAnErrorNotACleanVerdict: the same rule for the wiring
// itself, so a half-built Loop cannot present as a session holding nothing.
func TestAnUnwiredLookupIsAnErrorNotACleanVerdict(t *testing.T) {
	l := legsLoop(nil, nil, actingSurface())
	l.TrifectaBoundTags = nil

	_, err := l.trifectaStandingLegs(context.Background())
	require.Error(t, err)
}

// TestASessionHoldingNothingDerivesCleanLegs — the ordinary case, and it must
// cost no SpiceDB lookups: Derive returns early when nothing is bound, so a
// session with no delegated data pays nothing for the check being enabled.
func TestASessionHoldingNothingDerivesCleanLegs(t *testing.T) {
	l := legsLoop(nil, nil, actingSurface())
	l.TrifectaDeps = trifecta.Deps{
		TagCarriesUntrusted: func(context.Context, string) (bool, error) {
			t.Fatal("no tags are bound; no lookup should happen")
			return false, nil
		},
	}

	legs, err := l.trifectaStandingLegs(context.Background())

	require.NoError(t, err)
	assert.False(t, legs.Untrusted, "nothing bound means nothing untrusted, as a FACT not a default")
	assert.False(t, legs.Sensitive)
}

// TestCallImpactComesFromTheCALLNotTheSurface is the distinction that keeps
// the gate from firing on every call.
//
// The surface's StateImpact is the MAX across every producer of a handle —
// right for "could this session ever act", wrong for "is this particular call
// an action". Reading it here would make a read-only call by a session that
// holds one write tool look consequential, and leg C would be permanently true.
func TestCallImpactComesFromTheCALLNotTheSurface(t *testing.T) {
	l := legsLoop(nil, nil, actingSurface()) // surface says readwrite

	_, err := l.trifectaCallImpact("not_in_catalog", nil)

	require.Error(t, err,
		"an unknown tool must not inherit the surface's impact; it must be unresolvable")
	assert.Contains(t, err.Error(), "catalog")
}

// TestAnUnresolvableToolIsAnErrorNotReadonly.
//
// Defaulting to readonly would hand the hook a clean leg C for a call nobody
// could classify — the quiet direction, and the wrong one. Enforcing mode fails
// closed on the error instead.
func TestAnUnresolvableToolIsAnErrorNotReadonly(t *testing.T) {
	l := legsLoop(nil, nil, nil)

	impact, err := l.trifectaCallImpact("ghost_tool", map[string]any{})

	require.Error(t, err)
	assert.NotEqual(t, authz.Readonly, impact,
		"an unclassifiable call must not present as read-only, which is the leg the trifecta needs to be false")
}
