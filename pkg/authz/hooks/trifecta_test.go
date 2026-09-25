package hooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/trifecta"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// standingAB is a session already holding untrusted input and sensitive
// access. Leg C is left false: the hook decides it from the call.
var standingAB = trifecta.Legs{Untrusted: true, Sensitive: true}

func trifectaHook(mode string, legs trifecta.Legs, impact authz.StateImpact) *hooks.Trifecta {
	return hooks.NewTrifecta(hooks.TrifectaDeps{
		Mode:         mode,
		StandingLegs: func(context.Context) (trifecta.Legs, error) { return legs, nil },
		CallImpact: func(string, map[string]any) (authz.StateImpact, error) {
			return impact, nil
		},
	})
}

func toolCall(name string) pipeline.Input {
	return pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: name, Args: json.RawMessage(`{}`)},
	}
}

// TestEnforcingDeniesTheCallThatCompletesTheTrifecta.
//
// Legs A and B stand on the session's bound data; the write in front of the
// hook is C. That is the moment the trifecta completes, and the last moment
// refusing it is still cheap.
func TestEnforcingDeniesTheCallThatCompletesTheTrifecta(t *testing.T) {
	h := trifectaHook("enforcing", standingAB, authz.External)

	dec := h.Eval(context.Background(), toolCall("open_pr"))
	assert.Equal(t, pipeline.Deny, dec.Verdict)
	for _, want := range []string{"untrusted", "sensitive", "consequential"} {
		assert.Contains(t, dec.Reason, want, "the refusal must name the legs so an operator knows what to change")
	}
}

// TestAReadDoesNotCompleteTheTrifecta pins that C comes from the CALL.
//
// A session whose surface contains a writing tool has not completed anything by
// making a read. Deciding C from the session's whole surface instead would
// refuse every call a capable child makes, including the harmless ones.
func TestAReadDoesNotCompleteTheTrifecta(t *testing.T) {
	h := trifectaHook("enforcing", standingAB, authz.Readonly)

	dec := h.Eval(context.Background(), toolCall("read_issue"))
	assert.NotEqual(t, pipeline.Deny, dec.Verdict,
		"reading is leg B's territory; a read must not be treated as consequential action")
}

// TestTwoLegsNeverRefuses walks the near-misses at the hook level, not just in
// Evaluate, because the hook is where a future edit would be tempted to
// tighten it.
func TestTwoLegsNeverRefuses(t *testing.T) {
	cases := []struct {
		name   string
		legs   trifecta.Legs
		impact authz.StateImpact
	}{
		{name: "untrusted + consequential", legs: trifecta.Legs{Untrusted: true}, impact: authz.External},
		{name: "sensitive + consequential", legs: trifecta.Legs{Sensitive: true}, impact: authz.External},
		{name: "untrusted + sensitive, only reading", legs: standingAB, impact: authz.Readonly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := trifectaHook("enforcing", tc.legs, tc.impact).Eval(context.Background(), toolCall("t"))
			assert.NotEqual(t, pipeline.Deny, dec.Verdict)
		})
	}
}

// TestLoggingRecordsAndDoesNotDeny — and records the near-misses too, which is
// the dataset the mode exists for.
func TestLoggingRecordsAndDoesNotDeny(t *testing.T) {
	h := trifectaHook("logging", standingAB, authz.External)

	dec := h.Eval(context.Background(), toolCall("open_pr"))
	assert.NotEqual(t, pipeline.Deny, dec.Verdict)
	require.NotEmpty(t, dec.Audit)
	assert.Equal(t, "trifecta", dec.Audit[0].Kind)
	assert.Equal(t, true, dec.Audit[0].Fields["refused"],
		"logging must record that it WOULD have refused, or the mode gathers nothing usable")
}

func TestDisabledIsInert(t *testing.T) {
	h := trifectaHook("disabled", standingAB, authz.External)
	assert.Equal(t, pipeline.Decision{}, h.Eval(context.Background(), toolCall("open_pr")))

	unset := trifectaHook("", standingAB, authz.External)
	assert.Equal(t, pipeline.Decision{}, unset.Eval(context.Background(), toolCall("open_pr")))
}

// TestTrifectaEnforcesWithToolCallAuthzDisabled is the test the whole design
// argument rests on.
//
// The spec's case against routing this through ToolCallAuthzDeps.ForceApproval
// is that Eval RETURNS before consulting it when toolCalls.mode is "disabled" —
// so the control would vanish exactly when an operator turned off a DIFFERENT
// check. The danger is that a later refactor "reuses" that gate because it
// looks like consolidation.
//
// Asserted directly rather than left to hook ordering. Ordering is a fact about
// today's wiring; this is a fact about the requirement, and it fails loudly if
// anyone rewires the trifecta behind another gate's mode.
func TestTrifectaEnforcesWithToolCallAuthzDisabled(t *testing.T) {
	// The trifecta hook holds its OWN mode and takes no toolCalls mode at all
	// — there is no field here through which that gate could switch it off.
	h := hooks.NewTrifecta(hooks.TrifectaDeps{
		Mode:         "enforcing",
		StandingLegs: func(context.Context) (trifecta.Legs, error) { return standingAB, nil },
		CallImpact: func(string, map[string]any) (authz.StateImpact, error) {
			return authz.External, nil
		},
	})

	// A sibling ToolCallAuthz with mode disabled, proving the two are wired
	// independently: it no-ops, and the trifecta still denies.
	tca := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{Mode: "disabled"})
	assert.Equal(t, pipeline.Decision{}, tca.Eval(context.Background(), toolCall("open_pr")),
		"precondition: tool-call authz is genuinely off")

	dec := h.Eval(context.Background(), toolCall("open_pr"))
	assert.Equal(t, pipeline.Deny, dec.Verdict,
		"the trifecta must still enforce when tool-call authz is disabled; an operator turning off a permission "+
			"check has not decided that untrusted content may drive a write")
}

// TestClosureDenialSetRefusesInEveryMode follows requirePlan's precedent.
//
// A denied closure is a structural fact — this delegation has already been
// judged to have gone wrong — not a policy judgement about the call in front of
// the hook. So it refuses even when the mode is disabled, which is the one
// place this hook deliberately ignores its own dial.
func TestClosureDenialSetRefusesInEveryMode(t *testing.T) {
	for _, mode := range []string{"disabled", "logging", "enforcing"} {
		t.Run(mode, func(t *testing.T) {
			h := hooks.NewTrifecta(hooks.TrifectaDeps{
				Mode:               mode,
				InClosureDenialSet: func(context.Context) (bool, error) { return true, nil },
			})
			dec := h.Eval(context.Background(), toolCall("anything"))
			assert.Equal(t, pipeline.Deny, dec.Verdict,
				"a denied closure is structural and must refuse in %q too", mode)
		})
	}
}

// TestAnUnresolvableLegRefusesOnlyWhenEnforcing.
//
// Unknown must not read as clean under enforcement — the alternative is running
// a call whose closure nobody can characterise. Under logging it is recorded
// instead, because failing the session would stop the mode gathering the
// evidence it exists for.
func TestAnUnresolvableLegRefusesOnlyWhenEnforcing(t *testing.T) {
	deps := func(mode string) hooks.TrifectaDeps {
		return hooks.TrifectaDeps{
			Mode:         mode,
			StandingLegs: func(context.Context) (trifecta.Legs, error) { return trifecta.Legs{}, errors.New("spicedb down") },
			CallImpact: func(string, map[string]any) (authz.StateImpact, error) {
				return authz.External, nil
			},
		}
	}
	assert.Equal(t, pipeline.Deny,
		hooks.NewTrifecta(deps("enforcing")).Eval(context.Background(), toolCall("t")).Verdict)

	logged := hooks.NewTrifecta(deps("logging")).Eval(context.Background(), toolCall("t"))
	assert.NotEqual(t, pipeline.Deny, logged.Verdict)
	require.NotEmpty(t, logged.Audit)
	assert.Equal(t, "trifecta_unresolvable", logged.Audit[0].Kind)
}
