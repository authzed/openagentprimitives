// pkg/controllers/settings/revoke_durability_test.go
//
// Controller-level tests for the durable half of the tool-origin revocation
// trigger state. The publisher unit tests drive Observe directly; these drive
// the whole Reconcile against a fake API server, so they also prove the piece
// Observe cannot prove on its own: that the record it stamps survives the
// reconciler's status write, and is therefore there for a successor process.
package settings

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
)

// casWithMCPAllowlist builds the singleton ClusterAgentSettings with the given
// allowedMCPServers names.
func casWithMCPAllowlist(names ...string) *spiceboxv1alpha1.ClusterAgentSettings {
	return &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			Limits: &spiceboxv1alpha1.SettingsLimits{AllowedMCPServers: mcpList(names...)},
		},
	}
}

func clusterClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.ClusterAgentSettings{}).
		Build()
}

func reconcileCAS(t *testing.T, r *ClusterReconciler) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
	})
	return err
}

func getCAS(t *testing.T, c client.Client) spiceboxv1alpha1.ClusterAgentSettings {
	t.Helper()
	var got spiceboxv1alpha1.ClusterAgentSettings
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Name: spiceboxv1alpha1.ClusterAgentSettingsName}, &got))
	return got
}

func TestClusterReconcile_RecordsTheObservedAllowlistOnStatus(t *testing.T) {
	c := clusterClient(t, casWithMCPAllowlist("linear", "github"))
	bus := &capBus{}
	r := &ClusterReconciler{Client: c, RevokePublisher: NewRevokePublisher(revocation.NewPublisher(bus))}
	require.NoError(t, reconcileCAS(t, r))
	require.Empty(t, bus.snapshot(), "first reconcile primes without emitting")

	rec := getCAS(t, c).Status.ObservedLimits
	require.NotNil(t, rec, "the trigger state must be persisted where a successor process can read it")
	assert.Equal(t, &[]string{"github", "linear"}, rec.AllowedMCPServers)
}

func TestClusterReconcile_FreshOperatorProcessEmitsAWithdrawalCommittedWhileItWasDown(t *testing.T) {
	ctx := context.Background()
	c := clusterClient(t, casWithMCPAllowlist("linear", "github"))

	// Operator #1 observes the CR once and records what it saw.
	bus1 := &capBus{}
	require.NoError(t, reconcileCAS(t, &ClusterReconciler{
		Client: c, RevokePublisher: NewRevokePublisher(revocation.NewPublisher(bus1)),
	}))
	require.Empty(t, bus1.snapshot(), "first reconcile primes without emitting")

	// The operator goes down. An admin withdraws "github" from the allowlist.
	cas := getCAS(t, c)
	cas.Spec.Limits.AllowedMCPServers = mcpList("linear")
	require.NoError(t, c.Update(ctx, &cas))

	// Operator #2 comes up against the same API server with an empty cache.
	bus2 := &capBus{}
	require.NoError(t, reconcileCAS(t, &ClusterReconciler{
		Client: c, RevokePublisher: NewRevokePublisher(revocation.NewPublisher(bus2)),
	}))

	envs := bus2.snapshot()
	require.Len(t, envs, 1, "a restarted operator must still revoke an origin withdrawn while it was down")
	assert.Equal(t, "mcpserver/github", decodeRevoked(t, envs[0]).Key)
}

func TestClusterReconcile_FailedPublishSurfacesAsAReconcileErrorAndIsRetried(t *testing.T) {
	ctx := context.Background()
	c := clusterClient(t, casWithMCPAllowlist("linear", "github"))
	bus := &capBus{}
	r := &ClusterReconciler{Client: c, RevokePublisher: NewRevokePublisher(revocation.NewPublisher(bus))}
	require.NoError(t, reconcileCAS(t, r))

	cas := getCAS(t, c)
	cas.Spec.Limits.AllowedMCPServers = mcpList("linear")
	require.NoError(t, c.Update(ctx, &cas))

	bus.setFailure(errors.New("nats: connection closed"))
	require.Error(t, reconcileCAS(t, r),
		"a revoke that did not publish must fail the reconcile so it is requeued")

	rec := getCAS(t, c).Status.ObservedLimits
	require.NotNil(t, rec)
	assert.Equal(t, &[]string{"github", "linear"}, rec.AllowedMCPServers,
		"the record must still name the origin whose revoke failed")

	// The requeue lands on a healthy bus.
	bus.setFailure(nil)
	require.NoError(t, reconcileCAS(t, r))
	envs := bus.snapshot()
	require.Len(t, envs, 1, "the retry must re-derive and re-emit the withdrawal")
	assert.Equal(t, "mcpserver/github", decodeRevoked(t, envs[0]).Key)
}
