//go:build integration

package adoptkit_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
)

// createSecret creates an unadopted Secret with one data key and returns its
// reference. Names are per test: the envtest API server is shared.
func createSecret(t *testing.T, env *testenv.Env, name string) types.NamespacedName {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Data:       map[string][]byte{"token": []byte("x")},
	}), "create Secret %s", name)
	return types.NamespacedName{Namespace: "default", Name: name}
}

func getSecret(t *testing.T, env *testenv.Env, ref types.NamespacedName) *corev1.Secret {
	t.Helper()
	var s corev1.Secret
	require.NoError(t, env.Client.Get(context.Background(), ref, &s), "get Secret %s", ref)
	return &s
}

func managerNames(s *corev1.Secret) []string {
	out := []string{}
	for _, mf := range s.ManagedFields {
		out = append(out, mf.Manager)
	}
	return out
}

func session(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "default", Name: name}
}

func TestAdopt_RepeatAdoptionByTheSameOwnerDoesNotWrite(t *testing.T) {
	env := testenv.Shared(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ref := createSecret(t, env, "adopt-repeat")

	require.NoError(t, adoptkit.AdoptSecret(ctx, env.Client, env.Client, ref, session("sess-a"), "AgentSession"))
	before := getSecret(t, env, ref)

	require.NoError(t, adoptkit.AdoptSecret(ctx, env.Client, env.Client, ref, session("sess-a"), "AgentSession"))
	after := getSecret(t, env, ref)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "an owner already recorded is not applied again")

	require.NoError(t, adoptkit.AdoptSecret(ctx, env.Client, env.Client, ref, session("sess-b"), "AgentSession"))
	assert.Equal(t, "default/sess-b", getSecret(t, env, ref).Annotations["agentprimitives.authzed.com/owner-AgentSession-sess-b"],
		"a NEW owner is still recorded")
}

func TestAdoptSecretIfUnadopted(t *testing.T) {
	env := testenv.Shared(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("unadopted Secret: adopted, reports true", func(t *testing.T) {
		ref := createSecret(t, env, "ifunadopted-fresh")
		did, err := adoptkit.AdoptSecretIfUnadopted(ctx, env.Client, env.Client, ref, session("sess-a"), "AgentSession")
		require.NoError(t, err)
		assert.True(t, did, "adopts when nothing has")
		got := getSecret(t, env, ref)
		assert.Contains(t, got.Labels, adoptguard.AdoptedLabel)
		assert.Equal(t, "default/sess-a", got.Annotations["agentprimitives.authzed.com/owner-AgentSession-sess-a"])
	})

	t.Run("already adopted by another owner: untouched, reports false", func(t *testing.T) {
		ref := createSecret(t, env, "ifunadopted-owned")
		require.NoError(t, adoptkit.AdoptSecret(ctx, env.Client, env.Client, ref, session("ident"), "AgentIdentity"))
		before := getSecret(t, env, ref)

		did, err := adoptkit.AdoptSecretIfUnadopted(ctx, env.Client, env.Client, ref, session("sess-a"), "AgentSession")
		require.NoError(t, err)
		assert.False(t, did, "the label is all the guard needs, so nothing is written")
		after := getSecret(t, env, ref)
		assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "no write")
		assert.NotContains(t, after.Annotations, "agentprimitives.authzed.com/owner-AgentSession-sess-a", "no session owner recorded")
	})
}

func TestPruneOwners(t *testing.T) {
	env := testenv.Shared(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	staleOnly := func(names ...string) func(types.NamespacedName) bool {
		return func(o types.NamespacedName) bool {
			for _, n := range names {
				if o.Name == n {
					return true
				}
			}
			return false
		}
	}

	t.Run("stale owners removed in one write; live owners, other kinds, label and data kept", func(t *testing.T) {
		ref := createSecret(t, env, "prune-mixed")
		require.NoError(t, adoptkit.AdoptSecret(ctx, env.Client, env.Client, ref, session("ident"), "AgentIdentity"))
		for _, n := range []string{"sess-a", "sess-b", "sess-c"} {
			require.NoError(t, adoptkit.AdoptSecret(ctx, env.Client, env.Client, ref, session(n), "AgentSession"))
		}
		before := getSecret(t, env, ref)

		removed, err := adoptkit.PruneOwners(ctx, env.Client, before, "AgentSession", staleOnly("sess-a", "sess-b"))
		require.NoError(t, err)
		assert.Equal(t, 2, removed)

		got := getSecret(t, env, ref)
		assert.NotContains(t, got.Annotations, "agentprimitives.authzed.com/owner-AgentSession-sess-a")
		assert.NotContains(t, got.Annotations, "agentprimitives.authzed.com/owner-AgentSession-sess-b")
		assert.Equal(t, "default/sess-c", got.Annotations["agentprimitives.authzed.com/owner-AgentSession-sess-c"], "live owner kept")
		assert.Equal(t, "default/ident", got.Annotations["agentprimitives.authzed.com/owner-AgentIdentity-ident"], "other kinds untouched")
		assert.Contains(t, got.Labels, adoptguard.AdoptedLabel, "label kept")
		assert.Equal(t, []byte("x"), got.Data["token"], "data untouched")

		managers := strings.Join(managerNames(got), ",")
		assert.NotContains(t, managers, "agentprimitives-adopt-AgentSession-sess-a", "stale manager dropped")
		assert.NotContains(t, managers, "agentprimitives-adopt-AgentSession-sess-b", "stale manager dropped")
		assert.Contains(t, managers, "agentprimitives-adopt-AgentSession-sess-c", "live manager kept")
		assert.Contains(t, managers, "agentprimitives-adopt-AgentIdentity-ident", "other kind's manager kept")
	})

	t.Run("the only owner of the label is stale: label stays on the object", func(t *testing.T) {
		ref := createSecret(t, env, "prune-sole")
		require.NoError(t, adoptkit.AdoptSecret(ctx, env.Client, env.Client, ref, session("sess-a"), "AgentSession"))

		removed, err := adoptkit.PruneOwners(ctx, env.Client, getSecret(t, env, ref), "AgentSession", staleOnly("sess-a"))
		require.NoError(t, err)
		assert.Equal(t, 1, removed)
		got := getSecret(t, env, ref)
		assert.Contains(t, got.Labels, adoptguard.AdoptedLabel, "pruning never un-adopts")
		assert.NotContains(t, got.Annotations, "agentprimitives.authzed.com/owner-AgentSession-sess-a")
	})

	t.Run("object changed since it was read: refused, nothing removed", func(t *testing.T) {
		ref := createSecret(t, env, "prune-stale-read")
		for _, n := range []string{"sess-a", "sess-b"} {
			require.NoError(t, adoptkit.AdoptSecret(ctx, env.Client, env.Client, ref, session(n), "AgentSession"))
		}
		read := getSecret(t, env, ref)
		require.NoError(t, adoptkit.AdoptSecret(ctx, env.Client, env.Client, ref, session("sess-c"), "AgentSession"), "a newer write")

		_, err := adoptkit.PruneOwners(ctx, env.Client, read, "AgentSession", staleOnly("sess-a"))
		require.Error(t, err, "the resourceVersion test must fail")
		assert.Contains(t, getSecret(t, env, ref).Annotations, "agentprimitives.authzed.com/owner-AgentSession-sess-a", "nothing removed")
	})

	t.Run("nothing stale: no write", func(t *testing.T) {
		ref := createSecret(t, env, "prune-noop")
		require.NoError(t, adoptkit.AdoptSecret(ctx, env.Client, env.Client, ref, session("sess-a"), "AgentSession"))
		before := getSecret(t, env, ref)
		removed, err := adoptkit.PruneOwners(ctx, env.Client, before, "AgentSession", staleOnly())
		require.NoError(t, err)
		assert.Zero(t, removed)
		assert.Equal(t, before.ResourceVersion, getSecret(t, env, ref).ResourceVersion)
	})
}
