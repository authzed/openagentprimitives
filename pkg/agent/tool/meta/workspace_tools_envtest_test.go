//go:build integration

// pkg/agent/tool/meta/workspace_tools_envtest_test.go
package meta_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	// Registers the "git" workspace-source driver into
	// pkg/platform/workspacekinds/registry — runWorkspaceReconcile resolves the tool's
	// kind string ("git") through that registry, so without this import
	// Execute would fail closed with "no driver registered for kind git".
	_ "github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/git"
)

// waitAndSucceedJob polls until the reconcile Job named by key appears, then
// marks it Succeeded=1. envtest boots no kubelet/Job-controller, so the Job
// created by Execute would otherwise sit Active forever and waitJobDone would
// time out; this fakes the "reconcile pod finished" signal the real cluster
// would eventually produce. Runs in the background because Execute's
// waitJobDone poll blocks the calling goroutine until the Job reaches a
// terminal state — the caller must have already started this before calling
// Execute. Errors are reported over the returned channel rather than via
// require/assert, which must only be called from the test's own goroutine.
func waitAndSucceedJob(ctx context.Context, c client.Client, key client.ObjectKey) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				errCh <- fmt.Errorf("context done waiting for job %s: %w", key, ctx.Err())
				return
			case <-tick.C:
				var job batchv1.Job
				if err := c.Get(ctx, key, &job); err != nil {
					continue
				}
				job.Status.Succeeded = 1
				if err := c.Status().Update(ctx, &job); err != nil {
					errCh <- fmt.Errorf("update job %s status: %w", key, err)
					return
				}
				errCh <- nil
				return
			}
		}
	}()
	return errCh
}

// TestApplyWorkspace_CreatesReconcileJob exercises apply_workspace's full
// Execute path against a real (envtest) apiserver: it must create the
// reconcile Job with the overlay PVC mounted, the scoped push credential
// wired in via secretKeyRef (never a literal), and an owner ref back to the
// AgentSession — then, once the Job is faked to Succeeded, return a
// non-error Result.
func TestApplyWorkspace_CreatesReconcileJob(t *testing.T) {
	env := testenv.Shared(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := &tool.SessionContext{
		Namespace:       "default",
		Name:            "sess1",
		AgentSessionUID: types.UID("uid-1"),
		K8sClient:       env.Client,
	}
	tl := meta.NewApplyWorkspace("git", "https://example.com/o/r.git", "main", "/workspace", "sess1-workspace", "sess1-creds", "my-registry/git@sha256:pinned", "custom-ws-sa")

	key := client.ObjectKey{Namespace: "default", Name: "ws-apply-sess1"}
	fakeErrCh := waitAndSucceedJob(ctx, env.Client, key)

	res, err := tl.Execute(ctx, nil, sess)
	require.NoError(t, err, "Execute must not return a Go error")
	require.NoError(t, <-fakeErrCh, "faking reconcile Job success")
	assert.False(t, res.IsError, "Execute result: %s", res.Content)

	var job batchv1.Job
	require.NoError(t, env.Client.Get(ctx, key, &job), "reconcile Job must exist")

	require.Len(t, job.Spec.Template.Spec.Volumes, 1, "overlay PVC volume")
	require.NotNil(t, job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim)
	assert.Equal(t, "sess1-workspace", job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName,
		"Job must mount the session's overlay PVC")

	require.NotEmpty(t, job.Spec.Template.Spec.InitContainers, "reconcile commands run as initContainers")
	ic := job.Spec.Template.Spec.InitContainers[0]
	require.Len(t, ic.VolumeMounts, 1)
	assert.Equal(t, "/workspace", ic.VolumeMounts[0].MountPath, "initContainer must mount the overlay at /workspace")

	// The operator-resolved image + SA (not the package defaults) must reach the
	// Job — this is what makes the reconcile image digest-pinnable per cluster.
	assert.Equal(t, "my-registry/git@sha256:pinned", ic.Image, "reconcile Job must run the operator-configured image, not the hard-coded default")
	assert.Equal(t, "custom-ws-sa", job.Spec.Template.Spec.ServiceAccountName, "reconcile Job must run as the operator-configured ServiceAccount")

	var gotCredEnv, gotCredKey, gotCredSecret string
	for _, e := range ic.Env {
		if e.Name != "WORKSPACE_GIT_TOKEN" {
			continue
		}
		gotCredEnv = e.Name
		require.NotNil(t, e.ValueFrom, "cred env must be sourced from the Secret, not a literal value")
		require.NotNil(t, e.ValueFrom.SecretKeyRef)
		gotCredSecret = e.ValueFrom.SecretKeyRef.Name
		gotCredKey = e.ValueFrom.SecretKeyRef.Key
	}
	assert.Equal(t, "WORKSPACE_GIT_TOKEN", gotCredEnv, "apply Job must inject the git push credential env var")
	assert.Equal(t, "sess1-creds", gotCredSecret, "cred env must be sourced from the session's credential Secret")
	assert.Equal(t, "git-token", gotCredKey, "cred env must read the git-token key")

	require.Len(t, job.OwnerReferences, 1, "reconcile Job must be owned by the AgentSession")
	assert.Equal(t, "AgentSession", job.OwnerReferences[0].Kind)
	assert.Equal(t, sess.Name, job.OwnerReferences[0].Name)
	assert.Equal(t, types.UID("uid-1"), job.OwnerReferences[0].UID)

	require.NotNil(t, job.Spec.ActiveDeadlineSeconds, "reconcile Job must bound a stuck run")
	assert.Equal(t, int64(600), *job.Spec.ActiveDeadlineSeconds)
	require.NotNil(t, job.Spec.TTLSecondsAfterFinished, "finished reconcile Jobs must self-clean")
	assert.Equal(t, int32(3600), *job.Spec.TTLSecondsAfterFinished)
}

// TestSyncWorkspace_CreatesReconcileJob mirrors
// TestApplyWorkspace_CreatesReconcileJob for sync_workspace: no credential is
// ever injected (sync never authenticates as a write-back principal), and the
// Job is named ws-sync-<session> rather than ws-apply-<session>.
func TestSyncWorkspace_CreatesReconcileJob(t *testing.T) {
	env := testenv.Shared(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := &tool.SessionContext{
		Namespace:       "default",
		Name:            "sess1",
		AgentSessionUID: types.UID("uid-1"),
		K8sClient:       env.Client,
	}
	// Empty reconcile image/SA ⇒ the tool falls back to its built-in defaults.
	tl := meta.NewSyncWorkspace("git", "https://example.com/o/r.git", "main", "/workspace", "sess1-workspace", "", "")

	key := client.ObjectKey{Namespace: "default", Name: "ws-sync-sess1"}
	fakeErrCh := waitAndSucceedJob(ctx, env.Client, key)

	res, err := tl.Execute(ctx, nil, sess)
	require.NoError(t, err, "Execute must not return a Go error")
	require.NoError(t, <-fakeErrCh, "faking reconcile Job success")
	assert.False(t, res.IsError, "Execute result: %s", res.Content)

	var job batchv1.Job
	require.NoError(t, env.Client.Get(ctx, key, &job), "reconcile Job must exist")
	require.NotEmpty(t, job.Spec.Template.Spec.InitContainers)
	for _, e := range job.Spec.Template.Spec.InitContainers[0].Env {
		assert.NotEqual(t, "WORKSPACE_GIT_TOKEN", e.Name, "sync must never inject the push credential")
	}
	assert.Equal(t, "alpine/git:latest", job.Spec.Template.Spec.InitContainers[0].Image,
		"empty reconcile image must fall back to the built-in default")
	assert.Equal(t, "ap-snapshotter", job.Spec.Template.Spec.ServiceAccountName,
		"empty reconcile SA must fall back to the built-in default")
}
