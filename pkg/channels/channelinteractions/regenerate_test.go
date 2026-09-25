package channelinteractions

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// bindableRegenCategory registers a fresh ResurfaceRegenerate category parked
// at AwaitingCredentials — the shape credential_link uses — and resets both the
// category registry and the regenerator bindings around the test. Its fixture
// name differs from decision_test.go's bindableCategory so the two files cannot
// collide through the shared package-level registry.
func bindableRegenCategory(t *testing.T) Category {
	t.Helper()
	Reset()
	ResetRegenerators()
	t.Cleanup(func() { Reset(); ResetRegenerators() })
	c := Category{Name: "fixture_regen_cat", Tone: ToneRoutine, Park: v1alpha1.AgentSessionPhaseAwaitingCredentials,
		Deciders: DecideRequester, Resurface: ResurfaceRegenerate}
	Register(c)
	return c
}

func TestBindRegeneratorAndRegeneratorFor(t *testing.T) {
	c := bindableRegenCategory(t)
	var gotSess *v1alpha1.AgentSession
	var gotReq *channelevents.InteractionRequestPayload
	sess := &v1alpha1.AgentSession{}
	req := &channelevents.InteractionRequestPayload{Category: c.Name}
	BindRegenerator(c.Name, func(ctx context.Context, s *v1alpha1.AgentSession, r *channelevents.InteractionRequestPayload) error {
		gotSess = s
		gotReq = r
		return nil
	})

	r, ok := RegeneratorFor(c.Name)
	require.True(t, ok)
	err := r(context.Background(), sess, req)
	require.NoError(t, err)
	assert.Same(t, sess, gotSess)
	assert.Same(t, req, gotReq)

	_, ok = RegeneratorFor("unbound_cat")
	assert.False(t, ok)
}

func TestBindRegeneratorPanics(t *testing.T) {
	c := bindableRegenCategory(t)
	noop := func(ctx context.Context, s *v1alpha1.AgentSession, r *channelevents.InteractionRequestPayload) error {
		return nil
	}

	assert.Panics(t, func() { BindRegenerator("never_registered", noop) }, "binding an unregistered category is a programmer error")
	assert.Panics(t, func() { BindRegenerator(c.Name, nil) }, "nil regenerator is a programmer error")
	BindRegenerator(c.Name, noop)
	assert.Panics(t, func() { BindRegenerator(c.Name, noop) }, "double bind is a programmer error")
}

func TestResetRegenerators(t *testing.T) {
	c := bindableRegenCategory(t)
	BindRegenerator(c.Name, func(context.Context, *v1alpha1.AgentSession, *channelevents.InteractionRequestPayload) error {
		return nil
	})
	_, ok := RegeneratorFor(c.Name)
	require.True(t, ok)

	ResetRegenerators()

	_, ok = RegeneratorFor(c.Name)
	assert.False(t, ok, "ResetRegenerators must clear all bindings")
}
