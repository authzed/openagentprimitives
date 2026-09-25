//go:build integration

// pkg/apis/v1alpha1/agentsession_validation_test.go
//
// Integration test for the AgentSessionSpec CEL validation gating
// spec.parent immutability:
//   - unset -> set is legal (a delegated child names its parent once).
//   - set -> the same value is legal (a re-apply must not be rejected).
//   - set -> a different value is REJECTED (repointing would silently move a
//     running session's approval and transcript-read standing to a
//     different set of humans).
//   - set -> unset is REJECTED (removing parent must not silently detach
//     standing either).
//
// Runs against a real envtest apiserver (via pkg/controllers/testenv) so the
// assertion exercises the generated CRD's XValidation rule directly, not a
// Go-level pre-check that could drift from what's actually installed. This
// is the repo's first XValidation marker to use oldSelf (transition
// semantics) and whole-object `==`, so nothing else proves this CEL shape
// survives the apiserver's type-checker — see the sibling pattern in
// agentclass_validation_test.go, which this mirrors.
package v1alpha1_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// newParentTestSession builds a minimally-valid AgentSession (class + prompt
// are the only spec-required fields) with the given name, for exercising the
// spec.parent immutability CEL rule in isolation.
func newParentTestSession(name string) *v1alpha1.AgentSession {
	return &v1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.AgentSessionSpec{
			Class:  "demo-agent",
			Prompt: v1alpha1.PromptSource{Inline: "hello"},
		},
	}
}

func TestAgentSessionParentImmutableCEL(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()

	parentA := v1alpha1.NamespacedRef{Namespace: "default", Name: "session-a"}
	parentB := v1alpha1.NamespacedRef{Namespace: "default", Name: "session-b"}

	t.Run("unset to set: allowed (first set is legal)", func(t *testing.T) {
		sess := newParentTestSession("parent-unset-to-set")
		require.NoError(t, env.Client.Create(ctx, sess), "creating with no parent")

		sess.Spec.Parent = parentA.DeepCopy()
		assert.NoError(t, env.Client.Update(ctx, sess), "setting parent for the first time must be allowed")
	})

	t.Run("A to A: allowed (a re-apply must not be rejected)", func(t *testing.T) {
		sess := newParentTestSession("parent-noop-update")
		sess.Spec.Parent = parentA.DeepCopy()
		require.NoError(t, env.Client.Create(ctx, sess), "creating with parent already set")

		var fresh v1alpha1.AgentSession
		require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &fresh), "Get before re-apply")
		fresh.Spec.Parent = parentA.DeepCopy()
		assert.NoError(t, env.Client.Update(ctx, &fresh), "re-applying the same parent value must be a no-op, not a rejection")
	})

	// This is the load-bearing case: a rule that only refuses REMOVING parent
	// would still pass every other case here and fail only here.
	t.Run("A to B: rejected (repointing to a different session)", func(t *testing.T) {
		sess := newParentTestSession("parent-repoint-rejected")
		sess.Spec.Parent = parentA.DeepCopy()
		require.NoError(t, env.Client.Create(ctx, sess), "creating with parent already set")

		var fresh v1alpha1.AgentSession
		require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &fresh), "Get before repoint attempt")
		fresh.Spec.Parent = parentB.DeepCopy()
		err := env.Client.Update(ctx, &fresh)
		require.Error(t, err, "repointing parent to a different session must be rejected")
		assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
		assert.Contains(t, err.Error(), "spec.parent is immutable once set")
	})

	t.Run("A to unset: rejected (clearing parent once set)", func(t *testing.T) {
		sess := newParentTestSession("parent-remove-rejected")
		sess.Spec.Parent = parentA.DeepCopy()
		require.NoError(t, env.Client.Create(ctx, sess), "creating with parent already set")

		var fresh v1alpha1.AgentSession
		require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &fresh), "Get before removal attempt")
		fresh.Spec.Parent = nil
		err := env.Client.Update(ctx, &fresh)
		require.Error(t, err, "removing parent once set must be rejected")
		assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
		assert.Contains(t, err.Error(), "spec.parent is immutable once set")
	})
}
