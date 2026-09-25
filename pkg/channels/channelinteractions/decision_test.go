package channelinteractions

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func bindableCategory(t *testing.T) Category {
	t.Helper()
	Reset()
	ResetBindings()
	t.Cleanup(func() { Reset(); ResetBindings() })
	c := Category{Name: "fixture_cat", Tone: ToneRoutine, Park: v1alpha1.AgentSessionPhaseAwaitingDecision,
		Deciders: DecideApprovers, Resurface: ResurfaceCached}
	Register(c)
	return c
}

func TestBindAndHandlerFor(t *testing.T) {
	c := bindableCategory(t)
	called := false
	Bind(c.Name, func(ctx context.Context, d Decision) (Outcome, error) {
		called = true
		return Outcome{Result: channelevents.OutcomeApproved}, nil
	})

	h, ok := HandlerFor(c.Name)
	require.True(t, ok)
	out, err := h(context.Background(), Decision{})
	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, channelevents.OutcomeApproved, out.Result)

	_, ok = HandlerFor("unbound_cat")
	assert.False(t, ok)
}

func TestBindPanics(t *testing.T) {
	c := bindableCategory(t)
	noop := func(ctx context.Context, d Decision) (Outcome, error) {
		return Outcome{Result: channelevents.OutcomeDenied}, nil
	}

	assert.Panics(t, func() { Bind("never_registered", noop) }, "binding an unregistered category is a programmer error")
	assert.Panics(t, func() { Bind(c.Name, nil) }, "nil handler is a programmer error")
	Bind(c.Name, noop)
	assert.Panics(t, func() { Bind(c.Name, noop) }, "double bind is a programmer error")
}

func TestOutcomeValidate(t *testing.T) {
	cases := []struct {
		name    string
		outcome Outcome
		wantErr string
	}{
		{name: "approved valid", outcome: Outcome{Result: channelevents.OutcomeApproved}, wantErr: ""},
		{name: "resolved with minted url valid", outcome: Outcome{Result: channelevents.OutcomeResolved, MintedURL: "https://example.com/v"}, wantErr: ""},
		{name: "empty result rejected", outcome: Outcome{}, wantErr: "result"},
		{name: "unknown result rejected", outcome: Outcome{Result: "shrug"}, wantErr: "result"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.outcome.Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}
