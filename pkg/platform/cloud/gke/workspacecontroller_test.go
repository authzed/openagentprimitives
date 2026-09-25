package gke

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// nodeAndClasses builds a typed fake holding one node with a gce:// providerID
// (so identity derivation has something to parse) plus any storage classes.
func nodeAndClasses(providerID string, classes ...*storagev1.StorageClass) *fake.Clientset {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
	node.Spec.ProviderID = providerID
	objs := []runtime.Object{node}
	for _, c := range classes {
		objs = append(objs, c)
	}
	return fake.NewSimpleClientset(objs...)
}

func TestGKEEnsureWorkspaceStorage(t *testing.T) {
	const providerID = "gce://my-project/us-central1-a/gke-my-cluster-pool-abc123"

	stubIdentity := func(cluster, location string) func() {
		orig := describeClusterIdentity
		describeClusterIdentity = func(context.Context, cloud.Reporter, string, string, string) (string, string, error) {
			return cluster, location, nil
		}
		return func() { describeClusterIdentity = orig }
	}

	t.Run("filestore class already exists: no-op, no gcloud", func(t *testing.T) {
		called := false
		orig := enableFilestoreCSI
		enableFilestoreCSI = func(context.Context, cloud.Reporter, string, string, string) error { called = true; return nil }
		t.Cleanup(func() { enableFilestoreCSI = orig })

		kc := fake.NewSimpleClientset(multishareFilestoreClass("enterprise-multishare-rwx"))
		err := workspaceStorage{}.EnsureWorkspaceStorage(context.Background(), cloud.WorkspaceEnableParams{
			Clients: cloud.Clients{Typed: kc}, Reporter: cloud.NopReporter{},
		})
		require.NoError(t, err)
		assert.False(t, called, "a durable class already exists — must not enable anything")
	})

	t.Run("no class, identity + consent: enables the derived cluster's addon", func(t *testing.T) {
		t.Cleanup(stubIdentity("my-cluster", "us-central1"))
		origPoll := filestoreClassPoll
		filestoreClassPoll = 0
		t.Cleanup(func() { filestoreClassPoll = origPoll })

		kc := nodeAndClasses(providerID)
		var gotProject, gotCluster, gotLoc string
		orig := enableFilestoreCSI
		enableFilestoreCSI = func(_ context.Context, _ cloud.Reporter, project, cluster, location string) error {
			gotProject, gotCluster, gotLoc = project, cluster, location
			// GKE creates the managed *-rwx classes once the addon is on.
			_, e := kc.StorageV1().StorageClasses().Create(context.Background(),
				multishareFilestoreClass("enterprise-multishare-rwx"), metav1.CreateOptions{})
			return e
		}
		t.Cleanup(func() { enableFilestoreCSI = orig })

		err := workspaceStorage{}.EnsureWorkspaceStorage(context.Background(), cloud.WorkspaceEnableParams{
			Clients: cloud.Clients{Typed: kc}, Reporter: cloud.NopReporter{}, AssumeYes: true,
		})
		require.NoError(t, err)
		assert.Equal(t, "my-project", gotProject)
		assert.Equal(t, "my-cluster", gotCluster)
		assert.Equal(t, "us-central1", gotLoc)
	})

	t.Run("no class, declined: no gcloud, falls back", func(t *testing.T) {
		t.Cleanup(stubIdentity("my-cluster", "us-central1"))
		called := false
		orig := enableFilestoreCSI
		enableFilestoreCSI = func(context.Context, cloud.Reporter, string, string, string) error { called = true; return nil }
		t.Cleanup(func() { enableFilestoreCSI = orig })

		err := workspaceStorage{}.EnsureWorkspaceStorage(context.Background(), cloud.WorkspaceEnableParams{
			Clients: cloud.Clients{Typed: nodeAndClasses(providerID)}, Reporter: cloud.NopReporter{},
			AssumeYes: false, // non-interactive → declines
		})
		require.NoError(t, err, "a decline is not an error — Resolve falls back")
		assert.False(t, called, "declined: must not run gcloud")
	})

	t.Run("no class, identity underivable: no gcloud, falls back", func(t *testing.T) {
		called := false
		orig := enableFilestoreCSI
		enableFilestoreCSI = func(context.Context, cloud.Reporter, string, string, string) error { called = true; return nil }
		t.Cleanup(func() { enableFilestoreCSI = orig })

		// A node with no gce:// providerID → identity cannot be derived.
		kc := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
		err := workspaceStorage{}.EnsureWorkspaceStorage(context.Background(), cloud.WorkspaceEnableParams{
			Clients: cloud.Clients{Typed: kc}, Reporter: cloud.NopReporter{}, AssumeYes: true,
		})
		require.NoError(t, err, "underivable identity is not an error — never target a guessed cluster")
		assert.False(t, called, "underivable identity: must not run gcloud")
	})
}
