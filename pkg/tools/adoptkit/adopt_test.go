//go:build integration

package adoptkit_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
)

func TestAdoptSecret_LabelsAndAnnotates(t *testing.T) {
	env := testenv.Shared(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Pre-create the referenced secret (user-provided creds), unlabeled.
	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "creds"},
		Data:       map[string][]byte{"token": []byte("x")},
	}))
	objRef := types.NamespacedName{Namespace: "default", Name: "creds"}

	// Owner A adopts it. envtest's live client serves as both reader + writer.
	require.NoError(t, adoptkit.AdoptSecret(ctx, env.Client, env.Client, objRef, types.NamespacedName{Namespace: "default", Name: "chan-a"}, "Channel"))
	// Owner B co-adopts it.
	require.NoError(t, adoptkit.AdoptSecret(ctx, env.Client, env.Client, objRef, types.NamespacedName{Namespace: "default", Name: "chan-b"}, "Channel"))

	var got corev1.Secret
	require.NoError(t, env.Client.Get(ctx, objRef, &got))
	assert.Contains(t, got.Labels, adoptguard.AdoptedLabel, "adopted label set")
	assert.Equal(t, "default/chan-a", got.Annotations["agentprimitives.authzed.com/owner-Channel-chan-a"])
	assert.Equal(t, "default/chan-b", got.Annotations["agentprimitives.authzed.com/owner-Channel-chan-b"], "co-ownership: B's annotation does not clobber A's")
	// .data untouched (SSA was metadata-only).
	assert.Equal(t, []byte("x"), got.Data["token"], "adopt must not touch .data")
	_ = client.IgnoreNotFound
}

func TestAdoptSecret_MissingSecretIsNotFound_DoesNotCreate(t *testing.T) {
	env := testenv.Shared(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	missing := types.NamespacedName{Namespace: "default", Name: "no-such-secret"}

	// Adopting a missing reference must return NotFound and must NOT mint an
	// empty Secret (SSA Apply would otherwise create it).
	err := adoptkit.AdoptSecret(ctx, env.Client, env.Client, missing, types.NamespacedName{Namespace: "default", Name: "chan-a"}, "Channel")
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err), "missing referenced secret ⇒ NotFound, got %v", err)

	var got corev1.Secret
	getErr := env.Client.Get(ctx, missing, &got)
	assert.True(t, apierrors.IsNotFound(getErr), "adopt must not have created the secret")
}
