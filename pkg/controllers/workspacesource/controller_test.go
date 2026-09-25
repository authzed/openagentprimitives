//go:build integration

package workspacesource_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/controllers/workspacesource"
	_ "github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/git"
)

func startWSManager(t *testing.T, env *testenv.Env) {
	t.Helper()
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:     env.Scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	require.NoError(t, err)
	require.NoError(t, (&workspacesource.Reconciler{
		Client:                    mgr.GetClient(),
		BaseStorageClass:          "standard",
		MaterializeImage:          "busybox:1.36",
		MaterializeServiceAccount: "ap-snapshotter",
	}).SetupWithManager(mgr))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
}

func TestWorkspaceSource_MaterializesAndReports(t *testing.T) {
	env := testenv.Shared(t)
	startWSManager(t, env)
	ctx := context.Background()

	ws := &spiceboxv1alpha1.WorkspaceSource{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-source", Namespace: "default"},
		Spec: spiceboxv1alpha1.WorkspaceSourceSpec{
			Source: spiceboxv1alpha1.WorkspaceSourceRef{Kind: "git", Locator: "https://example.com/o/r.git", Ref: "main"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ws))

	// base PVC + materialize Job appear, Ready=False/Materializing
	var job batchv1.Job
	eventually(t, 10*time.Second, func() bool {
		var pvc corev1.PersistentVolumeClaim
		if err := env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-base-demo-source"}, &pvc); err != nil {
			return false
		}
		return env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-mat-demo-source"}, &job) == nil
	})

	var readying spiceboxv1alpha1.WorkspaceSource
	eventually(t, 10*time.Second, func() bool {
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(ws), &readying); err != nil {
			return false
		}
		c := meta.FindStatusCondition(readying.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionReady)
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == spiceboxv1alpha1.ReasonWorkspaceSourceMaterializing
	})

	// simulate the Job succeeding (no kubelet in envtest)
	job.Status.Succeeded = 1
	require.NoError(t, env.Client.Status().Update(ctx, &job))

	// Ready flips true
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.WorkspaceSource
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(ws), &got); err != nil {
			return false
		}
		c := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionReady)
		return c != nil && c.Status == metav1.ConditionTrue
	})
}

// TestWorkspaceSource_HonorsBaseSize is the regression guard for a Task-3 bug
// where spec.base.size was ignored and the base PVC was always sized off the
// operator default. It asserts the materialized base PVC's storage request
// matches the WorkspaceSource's spec.base.size verbatim.
func TestWorkspaceSource_HonorsBaseSize(t *testing.T) {
	env := testenv.Shared(t)
	startWSManager(t, env)
	ctx := context.Background()

	ws := &spiceboxv1alpha1.WorkspaceSource{
		ObjectMeta: metav1.ObjectMeta{Name: "sized-source", Namespace: "default"},
		Spec: spiceboxv1alpha1.WorkspaceSourceSpec{
			Source: spiceboxv1alpha1.WorkspaceSourceRef{Kind: "git", Locator: "https://example.com/o/r.git", Ref: "main"},
			Base:   spiceboxv1alpha1.WorkspaceBase{Size: "5Gi"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ws))

	var pvc corev1.PersistentVolumeClaim
	eventually(t, 10*time.Second, func() bool {
		return env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-base-sized-source"}, &pvc) == nil
	})

	got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	want := resource.MustParse("5Gi")
	assert.Zero(t, got.Cmp(want), "base PVC storage request must honor spec.base.size: got %s, want %s", got.String(), want.String())
}

// TestWorkspaceSource_SpecChangeDoesNotLatchReady is the regression guard for
// the whole-phase-review Fix 1 bug: the materialize Job is created once by
// stable name, so after a spec edit the pre-fix controller kept reporting
// Ready=True for a generation it never actually materialized. It asserts
// that editing spec.source.ref (which bumps metadata.generation) after the
// Job has already succeeded flips Ready to False/SpecChanged rather than
// leaving the stale Ready=True in place.
func TestWorkspaceSource_SpecChangeDoesNotLatchReady(t *testing.T) {
	env := testenv.Shared(t)
	startWSManager(t, env)
	ctx := context.Background()

	ws := &spiceboxv1alpha1.WorkspaceSource{
		ObjectMeta: metav1.ObjectMeta{Name: "changed-source", Namespace: "default"},
		Spec: spiceboxv1alpha1.WorkspaceSourceSpec{
			Source: spiceboxv1alpha1.WorkspaceSourceRef{Kind: "git", Locator: "https://example.com/o/r.git", Ref: "main"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ws))

	// base PVC + materialize Job appear
	var job batchv1.Job
	eventually(t, 10*time.Second, func() bool {
		var pvc corev1.PersistentVolumeClaim
		if err := env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-base-changed-source"}, &pvc); err != nil {
			return false
		}
		return env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-mat-changed-source"}, &job) == nil
	})

	// simulate the Job succeeding (no kubelet in envtest)
	job.Status.Succeeded = 1
	require.NoError(t, env.Client.Status().Update(ctx, &job))

	// Ready flips true for the generation the Job was materialized at
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.WorkspaceSource
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(ws), &got); err != nil {
			return false
		}
		c := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionReady)
		return c != nil && c.Status == metav1.ConditionTrue
	})

	// edit the spec — bumps metadata.generation past what the Job was labeled with
	var toUpdate spiceboxv1alpha1.WorkspaceSource
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(ws), &toUpdate))
	toUpdate.Spec.Source.Ref = "v2"
	require.NoError(t, env.Client.Update(ctx, &toUpdate))

	// Ready must NOT stay latched True — the base still reflects the old spec
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.WorkspaceSource
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(ws), &got); err != nil {
			return false
		}
		c := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionReady)
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == spiceboxv1alpha1.ReasonWorkspaceSourceSpecChanged
	})
}

// TestWorkspaceSource_ScheduledRefresh drives a WorkspaceSource with
// base.refresh set to the minimum allowed interval ("1m") through:
// materialize → Ready=True (which baselines status.lastRefreshedAt to "now"
// rather than treating a freshly-materialized base as instantly due for
// refresh) → the base is pre-aged past the interval (rather than waiting a
// full minute in real time) → a "ws-refresh-<name>" Job appears distinct
// from the materialize Job → faking that Job's success advances
// status.lastRefreshedAt to a recent time.
func TestWorkspaceSource_ScheduledRefresh(t *testing.T) {
	env := testenv.Shared(t)
	startWSManager(t, env)
	ctx := context.Background()

	ws := &spiceboxv1alpha1.WorkspaceSource{
		ObjectMeta: metav1.ObjectMeta{Name: "refresh-source", Namespace: "default"},
		Spec: spiceboxv1alpha1.WorkspaceSourceSpec{
			Source: spiceboxv1alpha1.WorkspaceSourceRef{Kind: "git", Locator: "https://example.com/o/r.git", Ref: "main"},
			Base:   spiceboxv1alpha1.WorkspaceBase{Refresh: "1m"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ws))

	// base materializes first, same as the non-refresh path
	var matJob batchv1.Job
	eventually(t, 10*time.Second, func() bool {
		return env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-mat-refresh-source"}, &matJob) == nil
	})
	matJob.Status.Succeeded = 1
	require.NoError(t, env.Client.Status().Update(ctx, &matJob))

	// Ready flips True and the refresh clock is baselined to "now"
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.WorkspaceSource
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(ws), &got); err != nil {
			return false
		}
		c := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionReady)
		return c != nil && c.Status == metav1.ConditionTrue && got.Status.LastRefreshedAt != nil
	})

	// pre-age the base so refresh is immediately due, rather than waiting a
	// full 1m interval in real time
	var toAge spiceboxv1alpha1.WorkspaceSource
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(ws), &toAge))
	aged := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	toAge.Status.LastRefreshedAt = &aged
	require.NoError(t, env.Client.Status().Update(ctx, &toAge))

	// a distinct refresh Job appears
	var refreshJob batchv1.Job
	eventually(t, 10*time.Second, func() bool {
		return env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-refresh-refresh-source"}, &refreshJob) == nil
	})
	assert.NotEqual(t, matJob.Name, refreshJob.Name, "refresh Job must not collide with the materialize Job")

	// simulate the refresh Job succeeding (no kubelet in envtest)
	refreshJob.Status.Succeeded = 1
	require.NoError(t, env.Client.Status().Update(ctx, &refreshJob))

	// status.lastRefreshedAt advances past the pre-aged value
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.WorkspaceSource
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(ws), &got); err != nil {
			return false
		}
		return got.Status.LastRefreshedAt != nil && got.Status.LastRefreshedAt.Time.After(aged.Time)
	})
}

// TestWorkspaceSource_RefreshFailureDoesNotAdvanceOrThrash is the regression
// guard for the refresh-phase JobFailed fix: a failed refresh Job must
// neither be treated as a successful refresh (status.lastRefreshedAt stays
// put) nor be deleted-and-recreated on every reconcile (which would hammer
// the origin faster than the configured base.refresh cadence). It drives a
// WorkspaceSource through materialize + a pre-aged refresh due, fakes the
// resulting refresh Job as JobFailed with LastTransitionTime=now, and
// asserts status.lastRefreshedAt is unchanged and the failed Job is still
// present (not immediately deleted, since less than the 1m interval has
// elapsed since the failure).
func TestWorkspaceSource_RefreshFailureDoesNotAdvanceOrThrash(t *testing.T) {
	env := testenv.Shared(t)
	startWSManager(t, env)
	ctx := context.Background()

	ws := &spiceboxv1alpha1.WorkspaceSource{
		ObjectMeta: metav1.ObjectMeta{Name: "refresh-fail-source", Namespace: "default"},
		Spec: spiceboxv1alpha1.WorkspaceSourceSpec{
			Source: spiceboxv1alpha1.WorkspaceSourceRef{Kind: "git", Locator: "https://example.com/o/r.git", Ref: "main"},
			Base:   spiceboxv1alpha1.WorkspaceBase{Refresh: "1m"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ws))

	// base materializes first, same as the non-refresh path
	var matJob batchv1.Job
	eventually(t, 10*time.Second, func() bool {
		return env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-mat-refresh-fail-source"}, &matJob) == nil
	})
	matJob.Status.Succeeded = 1
	require.NoError(t, env.Client.Status().Update(ctx, &matJob))

	// Ready flips True and the refresh clock is baselined to "now"
	eventually(t, 10*time.Second, func() bool {
		var got spiceboxv1alpha1.WorkspaceSource
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(ws), &got); err != nil {
			return false
		}
		c := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionReady)
		return c != nil && c.Status == metav1.ConditionTrue && got.Status.LastRefreshedAt != nil
	})

	// pre-age the base so refresh is immediately due
	var toAge spiceboxv1alpha1.WorkspaceSource
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(ws), &toAge))
	aged := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	toAge.Status.LastRefreshedAt = &aged
	require.NoError(t, env.Client.Status().Update(ctx, &toAge))

	// a distinct refresh Job appears
	var refreshJob batchv1.Job
	eventually(t, 10*time.Second, func() bool {
		return env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-refresh-refresh-fail-source"}, &refreshJob) == nil
	})

	// fake the refresh Job as failed, just now
	refreshJob.Status.Conditions = []batchv1.JobCondition{
		{
			Type:               batchv1.JobFailed,
			Status:             corev1.ConditionTrue,
			Reason:             "BackoffLimitExceeded",
			LastTransitionTime: metav1.NewTime(time.Now()),
		},
	}
	require.NoError(t, env.Client.Status().Update(ctx, &refreshJob))

	// give the controller a few reconciles to process the failure
	time.Sleep(2 * time.Second)

	// (a) status.lastRefreshedAt is NOT advanced — still the pre-aged value
	var got spiceboxv1alpha1.WorkspaceSource
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(ws), &got))
	require.NotNil(t, got.Status.LastRefreshedAt)
	assert.WithinDuration(t, aged.Time, got.Status.LastRefreshedAt.Time, time.Second,
		"a failed refresh must not be treated as a successful refresh")

	// (b) the failed Job is NOT immediately deleted — backoff, since less
	// than the 1m interval has elapsed since the failure
	var stillThere batchv1.Job
	assert.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ws-refresh-refresh-fail-source"}, &stillThere),
		"a freshly-failed refresh Job must not be deleted before a full refresh interval has elapsed")
}

func eventually(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", within)
}
