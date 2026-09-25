//go:build integration

// pkg/apis/v1alpha1/subagentrequest_validation_test.go
//
// Integration test for the SubagentRequestSpec.Mode enum marker
// (single_turn|task|chat|attended). The marker is CRD-level and only real once
// installed: a Go-level check that spec.Mode round-trips through the struct
// proves nothing about what the apiserver actually accepts. This runs
// against a real envtest apiserver (via pkg/controllers/testenv) so the
// rejected-value case exercises the generated CRD's enum validation
// directly, not a Go-level pre-check that could drift from what's actually
// installed.
package v1alpha1_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// newModeRequest builds a minimally-valid SubagentRequest (parent, class and
// task are the only spec-required fields) with the given name and mode, for
// exercising the mode enum in isolation.
func newModeRequest(name, mode string) *v1alpha1.SubagentRequest {
	return &v1alpha1.SubagentRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.SubagentRequestSpec{
			Parent: v1alpha1.NamespacedRef{Namespace: "default", Name: "demo-parent"},
			Class:  "demo-coder",
			Task:   "do the thing",
			Mode:   mode,
		},
	}
}

func TestSubagentRequestMode(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()

	t.Run("omitted: accepted, EffectiveMode is single_turn", func(t *testing.T) {
		sr := newModeRequest("mode-omitted", "")
		require.NoError(t, env.Client.Create(ctx, sr))
		assert.Equal(t, v1alpha1.SubagentModeSingleTurn, sr.EffectiveMode())
	})

	t.Run("single_turn: accepted", func(t *testing.T) {
		sr := newModeRequest("mode-single-turn", v1alpha1.SubagentModeSingleTurn)
		assert.NoError(t, env.Client.Create(ctx, sr))
	})

	t.Run("task: accepted", func(t *testing.T) {
		sr := newModeRequest("mode-task", v1alpha1.SubagentModeTask)
		assert.NoError(t, env.Client.Create(ctx, sr))
	})

	t.Run("chat: accepted", func(t *testing.T) {
		sr := newModeRequest("mode-chat", v1alpha1.SubagentModeChat)
		assert.NoError(t, env.Client.Create(ctx, sr))
	})

	t.Run("attended: accepted", func(t *testing.T) {
		sr := newModeRequest("mode-attended", v1alpha1.SubagentModeAttended)
		assert.NoError(t, env.Client.Create(ctx, sr))
	})

	t.Run("conversation: rejected by the apiserver", func(t *testing.T) {
		sr := newModeRequest("mode-conversation", "conversation")
		err := env.Client.Create(ctx, sr)
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
	})
}
