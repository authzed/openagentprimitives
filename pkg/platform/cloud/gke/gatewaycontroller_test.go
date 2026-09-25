package gke

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// nodeWithProviderID returns a typed fake holding one node carrying the given
// gce:// providerID, so identity derivation has something to parse.
func nodeWithProviderID(t *testing.T, providerID string) *fake.Clientset {
	t.Helper()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
	node.Spec.ProviderID = providerID
	return fake.NewSimpleClientset(node)
}

func TestGKEEnsureGatewayController(t *testing.T) {
	const providerID = "gce://my-project/us-central1-a/gke-my-cluster-pool-abc123"

	// stubIdentity makes describeClusterIdentity return a fixed cluster/location
	// without shelling out to gcloud.
	stubIdentity := func(cluster, location string) func() {
		orig := describeClusterIdentity
		describeClusterIdentity = func(context.Context, cloud.Reporter, string, string, string) (string, string, error) {
			return cluster, location, nil
		}
		return func() { describeClusterIdentity = orig }
	}

	t.Run("already served: returns managed class, no gcloud", func(t *testing.T) {
		origServed := gatewayAPIServed
		gatewayAPIServed = func(*rest.Config) (bool, error) { return true, nil }
		t.Cleanup(func() { gatewayAPIServed = origServed })

		called := false
		origEnable := enableGatewayAPI
		enableGatewayAPI = func(context.Context, cloud.Reporter, string, string, string) error { called = true; return nil }
		t.Cleanup(func() { enableGatewayAPI = origEnable })

		res, err := Strategy{}.EnsureGatewayController(context.Background(), cloud.GatewayControllerParams{
			Clients: cloud.Clients{Typed: nodeWithProviderID(t, providerID)}, Reporter: cloud.NopReporter{},
		})
		require.NoError(t, err)
		assert.True(t, res.Proceed)
		assert.Equal(t, "gke-l7-global-external-managed", res.GatewayClass)
		assert.False(t, called, "must not enable when already served")
	})

	t.Run("not served, declined: skip, no gcloud", func(t *testing.T) {
		origServed := gatewayAPIServed
		gatewayAPIServed = func(*rest.Config) (bool, error) { return false, nil }
		t.Cleanup(func() { gatewayAPIServed = origServed })
		t.Cleanup(stubIdentity("my-cluster", "us-central1"))

		called := false
		origEnable := enableGatewayAPI
		enableGatewayAPI = func(context.Context, cloud.Reporter, string, string, string) error { called = true; return nil }
		t.Cleanup(func() { enableGatewayAPI = origEnable })

		res, err := Strategy{}.EnsureGatewayController(context.Background(), cloud.GatewayControllerParams{
			Clients: cloud.Clients{Typed: nodeWithProviderID(t, providerID)}, Reporter: cloud.NopReporter{},
			AssumeYes: false, // non-interactive testRep → declines
		})
		require.NoError(t, err)
		assert.False(t, res.Proceed)
		assert.False(t, called, "declined: must not run gcloud")
	})

	t.Run("not served, accepted: enables derived cluster, monitors to served", func(t *testing.T) {
		// served flips false→true after enable runs.
		enabled := false
		origServed := gatewayAPIServed
		gatewayAPIServed = func(*rest.Config) (bool, error) { return enabled, nil }
		t.Cleanup(func() { gatewayAPIServed = origServed })
		t.Cleanup(stubIdentity("my-cluster", "us-central1"))

		var gotProject, gotCluster, gotLoc string
		origEnable := enableGatewayAPI
		enableGatewayAPI = func(_ context.Context, _ cloud.Reporter, project, cluster, location string) error {
			gotProject, gotCluster, gotLoc = project, cluster, location
			enabled = true
			return nil
		}
		t.Cleanup(func() { enableGatewayAPI = origEnable })

		origPoll := gatewayServedPoll
		gatewayServedPoll = 0
		t.Cleanup(func() { gatewayServedPoll = origPoll })

		res, err := Strategy{}.EnsureGatewayController(context.Background(), cloud.GatewayControllerParams{
			Clients: cloud.Clients{Typed: nodeWithProviderID(t, providerID)}, Reporter: cloud.NopReporter{},
			AssumeYes: true,
		})
		require.NoError(t, err)
		assert.True(t, res.Proceed)
		assert.Equal(t, "gke-l7-global-external-managed", res.GatewayClass)
		assert.Equal(t, "my-project", gotProject, "must enable in the derived project (providerID project)")
		assert.Equal(t, "my-cluster", gotCluster, "must enable the derived cluster")
		assert.Equal(t, "us-central1", gotLoc, "must enable in the derived location")
	})

	t.Run("not served, identity underivable: skip, no gcloud", func(t *testing.T) {
		origServed := gatewayAPIServed
		gatewayAPIServed = func(*rest.Config) (bool, error) { return false, nil }
		t.Cleanup(func() { gatewayAPIServed = origServed })

		called := false
		origEnable := enableGatewayAPI
		enableGatewayAPI = func(context.Context, cloud.Reporter, string, string, string) error { called = true; return nil }
		t.Cleanup(func() { enableGatewayAPI = origEnable })

		res, err := Strategy{}.EnsureGatewayController(context.Background(), cloud.GatewayControllerParams{
			Clients: cloud.Clients{Typed: fake.NewSimpleClientset()}, Reporter: cloud.NopReporter{}, AssumeYes: true,
		})
		require.NoError(t, err)
		assert.False(t, res.Proceed, "no node → cannot derive identity → skip")
		assert.False(t, called, "must never run gcloud against a guessed cluster")
	})

	t.Run("override + already served: returns override class, no gcloud", func(t *testing.T) {
		origServed := gatewayAPIServed
		gatewayAPIServed = func(*rest.Config) (bool, error) { return true, nil }
		t.Cleanup(func() { gatewayAPIServed = origServed })

		called := false
		origEnable := enableGatewayAPI
		enableGatewayAPI = func(context.Context, cloud.Reporter, string, string, string) error { called = true; return nil }
		t.Cleanup(func() { enableGatewayAPI = origEnable })

		res, err := Strategy{}.EnsureGatewayController(context.Background(), cloud.GatewayControllerParams{
			Clients: cloud.Clients{Typed: nodeWithProviderID(t, providerID)}, Reporter: cloud.NopReporter{},
			GatewayClassOverride: "custom-class",
		})
		require.NoError(t, err)
		assert.True(t, res.Proceed)
		assert.Equal(t, "custom-class", res.GatewayClass, "override must be returned as the class")
		assert.False(t, called, "must not enable when already served")
	})

	t.Run("override + not served: surface + skip, no gcloud", func(t *testing.T) {
		origServed := gatewayAPIServed
		gatewayAPIServed = func(*rest.Config) (bool, error) { return false, nil }
		t.Cleanup(func() { gatewayAPIServed = origServed })

		called := false
		origEnable := enableGatewayAPI
		enableGatewayAPI = func(context.Context, cloud.Reporter, string, string, string) error { called = true; return nil }
		t.Cleanup(func() { enableGatewayAPI = origEnable })

		// No node / identity needed — the override path must short-circuit before
		// deriveClusterIdentity is ever called.
		res, err := Strategy{}.EnsureGatewayController(context.Background(), cloud.GatewayControllerParams{
			Clients: cloud.Clients{Typed: fake.NewSimpleClientset()}, Reporter: cloud.NopReporter{},
			GatewayClassOverride: "custom-class",
		})
		require.NoError(t, err)
		assert.False(t, res.Proceed, "must not proceed when override set but Gateway API not served")
		assert.False(t, called, "must not run gcloud when a custom class is requested")
	})

	t.Run("not served, accepted, discovery always errors: timeout folds last error", func(t *testing.T) {
		// The first gatewayAPIServed call (the pre-enable served check) must return
		// (false, nil) so EnsureGatewayController proceeds past the initial check and
		// into the enable+wait path. Subsequent calls (from the waitGatewayServed poll
		// loop) always return (false, errors.New("boom")) — the poll loop must fold
		// that last error into the timeout message rather than dropping it silently.
		origServed := gatewayAPIServed
		calls := 0
		gatewayAPIServed = func(*rest.Config) (bool, error) {
			calls++
			if calls == 1 {
				return false, nil // pre-enable check: not served, no error → enter enable path
			}
			return false, errors.New("boom") // wait-loop checks: always fail
		}
		t.Cleanup(func() { gatewayAPIServed = origServed })
		t.Cleanup(stubIdentity("my-cluster", "us-central1"))

		origEnable := enableGatewayAPI
		enableGatewayAPI = func(context.Context, cloud.Reporter, string, string, string) error { return nil }
		t.Cleanup(func() { enableGatewayAPI = origEnable })

		origPoll := gatewayServedPoll
		gatewayServedPoll = 0
		t.Cleanup(func() { gatewayServedPoll = origPoll })

		origDeadline := gatewayServedDeadline
		gatewayServedDeadline = 10 * time.Millisecond
		t.Cleanup(func() { gatewayServedDeadline = origDeadline })

		_, err := Strategy{}.EnsureGatewayController(context.Background(), cloud.GatewayControllerParams{
			Clients:   cloud.Clients{Typed: nodeWithProviderID(t, providerID)},
			Reporter:  cloud.NopReporter{},
			AssumeYes: true,
		})
		require.Error(t, err, "must error: discovery always fails so Gateway API never becomes served")
		assert.Contains(t, err.Error(), "did not become served", "timeout message must be present")
		assert.Contains(t, err.Error(), "boom", "last discovery error must be folded into the timeout message")
	})
}
