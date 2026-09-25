package workspace_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

func TestCPByPod_BuildSnapshotJob_ShapeAndCommand(t *testing.T) {
	cfg := workspace.CPByPodConfig{
		Namespace:        "spicebox",
		SnapshotStorePVC: "sess-snapstore",
		Image:            "busybox:1.36",
		ServiceAccount:   "ap-snapshotter",
	}
	src := workspace.PVCRef{Namespace: "spicebox", Name: "sess-workspace"}
	h := workspace.SnapshotHandle{SessionUID: "uid-1", TurnIndex: 3, Sequence: 0}

	job := workspace.BuildSnapshotJob(cfg, src, h)
	require.NotNil(t, job)
	assert.Equal(t, "spicebox", job.Namespace)
	assert.Contains(t, job.Name, "snap-uid-1-")
	assert.Equal(t, cfg.ServiceAccount, job.Spec.Template.Spec.ServiceAccountName)
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	c := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, cfg.Image, c.Image)
	// Command must use cp -a --reflink=auto from /src to the snapstore path.
	require.Len(t, c.Command, 1)
	assert.Equal(t, "sh", c.Command[0])
	require.Len(t, c.Args, 2)
	assert.Contains(t, c.Args[1], "cp -a --reflink=auto /src/. /snap/uid-1/000003-000/")
	// Two volume mounts.
	mounts := map[string]bool{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = true
	}
	assert.True(t, mounts["/src"], "must mount source at /src")
	assert.True(t, mounts["/snap"], "must mount snapshot store at /snap")
	// Two volumes, one per PVC.
	vols := map[string]string{}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			vols[v.Name] = v.PersistentVolumeClaim.ClaimName
		}
	}
	assert.Equal(t, "sess-workspace", vols["src"])
	assert.Equal(t, "sess-snapstore", vols["snap"])
	// BackoffLimit = 1 so a transient pod-pull failure retries once;
	// permanent failure surfaces quickly.
	require.NotNil(t, job.Spec.BackoffLimit)
	assert.Equal(t, int32(1), *job.Spec.BackoffLimit)
}

func newFakeClient(t *testing.T) client.Client {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, batchv1.AddToScheme(sch))
	return fake.NewClientBuilder().WithScheme(sch).Build()
}

func TestCPByPod_BuildRestoreJob_Shape(t *testing.T) {
	cfg := workspace.CPByPodConfig{
		Namespace:        "spicebox",
		SnapshotStorePVC: "sess-snapstore",
		Image:            "busybox:1.36",
		ServiceAccount:   "ap-snapshotter",
	}
	h := workspace.SnapshotHandle{SessionUID: "uid-1", TurnIndex: 3, Sequence: 0}
	dst := workspace.PVCRef{Namespace: "spicebox", Name: "child-workspace"}

	job := workspace.BuildRestoreJob(cfg, h, dst)
	require.NotNil(t, job)
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	c := job.Spec.Template.Spec.Containers[0]
	// Reverse direction: cp from snap subdir into dst.
	assert.Contains(t, c.Args[1], "cp -a --reflink=auto /snap/uid-1/000003-000/. /dst/")
	// dst volume is RW (not read-only).
	var dstRO *bool
	for _, m := range c.VolumeMounts {
		if m.MountPath == "/dst" {
			mm := m
			dstRO = &mm.ReadOnly
		}
	}
	require.NotNil(t, dstRO)
	assert.False(t, *dstRO, "destination must be RW")
}

func TestCPByPod_Snapshot_LaunchesJob_AndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	s := workspace.NewCPByPod(c, workspace.CPByPodConfig{
		Namespace:        "ns",
		SnapshotStorePVC: "snapstore",
		Image:            "busybox:1.36",
		ServiceAccount:   "ap-snapshotter",
	})
	src := workspace.PVCRef{Namespace: "ns", Name: "src"}
	h := workspace.SnapshotHandle{SessionUID: "u", TurnIndex: 1, Sequence: 0}

	require.NoError(t, s.Snapshot(ctx, src, h))
	require.NoError(t, s.Snapshot(ctx, src, h), "idempotent re-snapshot must not error")

	var jobs batchv1.JobList
	require.NoError(t, c.List(ctx, &jobs))
	assert.Len(t, jobs.Items, 1, "two snapshot calls at the same handle must produce one Job")
}

func TestCPByPod_Restore_LaunchesJob_AndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	s := workspace.NewCPByPod(c, workspace.CPByPodConfig{
		Namespace:        "ns",
		SnapshotStorePVC: "snapstore",
		Image:            "busybox:1.36",
		ServiceAccount:   "ap-snapshotter",
	})
	h := workspace.SnapshotHandle{SessionUID: "u", TurnIndex: 1, Sequence: 0}
	dst := workspace.PVCRef{Namespace: "ns", Name: "dst"}

	require.NoError(t, s.Restore(ctx, h, dst))
	require.NoError(t, s.Restore(ctx, h, dst), "idempotent")

	var jobs batchv1.JobList
	require.NoError(t, c.List(ctx, &jobs))
	assert.Len(t, jobs.Items, 1)
}

func TestCPByPod_SatisfiesSnapshotterInterface(t *testing.T) {
	var _ workspace.Snapshotter = (*workspace.CPByPod)(nil)
}

// TestBuildSnapshotJob_HandleStorePVCOverridesCfg verifies that a non-empty
// h.SnapshotStorePVC wins over cfg.SnapshotStorePVC so a single operator-wide
// CPByPod instance can serve snapshots for many AgentSessions (each with its
// own store PVC).
func TestBuildSnapshotJob_HandleStorePVCOverridesCfg(t *testing.T) {
	cfg := workspace.CPByPodConfig{
		SnapshotStorePVC: "cfg-snapstore",
		Image:            "busybox:1.36",
		ServiceAccount:   "ap-snapshotter",
	}
	src := workspace.PVCRef{Namespace: "ns", Name: "sess-workspace"}
	h := workspace.SnapshotHandle{
		SessionUID:       "uid-2",
		TurnIndex:        1,
		Sequence:         0,
		SnapshotStorePVC: "handle-snapstore",
	}

	job := workspace.BuildSnapshotJob(cfg, src, h)
	require.NotNil(t, job)
	// Job namespace derived from src.Namespace, not cfg.Namespace (empty).
	assert.Equal(t, "ns", job.Namespace)
	// The snap volume must use the handle's store PVC, not cfg's.
	vols := map[string]string{}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			vols[v.Name] = v.PersistentVolumeClaim.ClaimName
		}
	}
	assert.Equal(t, "handle-snapstore", vols["snap"], "h.SnapshotStorePVC must override cfg.SnapshotStorePVC")
}

// TestBuildJobs_AdHocTurnIndexProducesValidMetadata is the regression test for
// the zap2 restart wedge: the ad-hoc clean-fork snapshot handle uses
// TurnIndex=-1, and rendering it verbatim into the turnIndex label produced
// "-1" — an invalid label value the apiserver rejects, so the snapshot Job
// could never be created and ReconcileRestart looped forever.
func TestBuildJobs_AdHocTurnIndexProducesValidMetadata(t *testing.T) {
	h := workspace.SnapshotHandle{
		SessionUID: "2c19c9e7-3ee8-46c3-af4b-20f79af8c92b",
		TurnIndex:  -1, // ad-hoc clean-fork marker
		Sequence:   0,
	}
	cfg := workspace.CPByPodConfig{
		Namespace:        "default",
		SnapshotStorePVC: "store",
		Image:            "busybox",
	}
	src := workspace.PVCRef{Namespace: "default", Name: "ws"}
	dst := workspace.PVCRef{Namespace: "default", Name: "ws-child"}

	snapJob := workspace.BuildSnapshotJob(cfg, src, h)
	restJob := workspace.BuildRestoreJob(cfg, h, dst)

	for _, job := range []*batchv1.Job{snapJob, restJob} {
		for k, v := range job.Labels {
			assert.Empty(t, validation.IsValidLabelValue(v),
				"label %s=%q on Job %s must be a valid label value", k, v, job.Name)
		}
		assert.Empty(t, validation.IsDNS1123Subdomain(job.Name),
			"Job name %q must be a valid DNS-1123 subdomain", job.Name)
	}

	assert.NotContains(t, h.PathSegment(), "-1",
		"ad-hoc path segment must not embed the raw -1 index")
	assert.False(t, strings.Contains(snapJob.Name, "--"),
		"snapshot Job name %q must not contain the '--' artifact of %%06d on -1", snapJob.Name)
}

// TestBuildJobs_QualifiedAdHocHandle_ValidMetadataAndDistinct extends the
// ad-hoc validity test to a Qualifier-carrying handle (the CLEAN-fork path
// stamps the child session name): the qualifier must surface in the path
// segment and both Job names — so two forks of the same parent get distinct
// snapshots — while all metadata stays apiserver-valid.
func TestBuildJobs_QualifiedAdHocHandle_ValidMetadataAndDistinct(t *testing.T) {
	base := workspace.SnapshotHandle{
		SessionUID: "2c19c9e7-3ee8-46c3-af4b-20f79af8c92b",
		TurnIndex:  -1, // ad-hoc clean-fork marker
		Sequence:   0,
	}
	fork1 := base
	fork1.Qualifier = "parent-fk1"
	fork2 := base
	fork2.Qualifier = "parent-fk2"

	cfg := workspace.CPByPodConfig{
		Namespace:        "default",
		SnapshotStorePVC: "store",
		Image:            "busybox",
	}
	src := workspace.PVCRef{Namespace: "default", Name: "ws"}
	dst := workspace.PVCRef{Namespace: "default", Name: "ws-child"}

	snapJob := workspace.BuildSnapshotJob(cfg, src, fork1)
	restJob := workspace.BuildRestoreJob(cfg, fork1, dst)

	for _, job := range []*batchv1.Job{snapJob, restJob} {
		for k, v := range job.Labels {
			assert.Empty(t, validation.IsValidLabelValue(v),
				"label %s=%q on Job %s must be a valid label value", k, v, job.Name)
		}
		assert.Empty(t, validation.IsDNS1123Subdomain(job.Name),
			"Job name %q must be a valid DNS-1123 subdomain", job.Name)
		assert.Contains(t, job.Name, "parent-fk1", "the qualifier must appear in the Job name")
	}
	assert.Contains(t, fork1.PathSegment(), "adhoc-parent-fk1-",
		"the qualifier must appear in the on-disk path segment")

	// Distinctness: a second fork of the same parent must NOT share the
	// first fork's snapshot identity.
	assert.NotEqual(t, fork1.PathSegment(), fork2.PathSegment())
	assert.NotEqual(t, workspace.SnapshotJobName(fork1), workspace.SnapshotJobName(fork2))
	assert.NotEqual(t,
		workspace.BuildRestoreJob(cfg, fork1, dst).Name,
		workspace.BuildRestoreJob(cfg, fork2, dst).Name)

	// A qualifier needing sanitization still yields valid metadata.
	messy := base
	messy.Qualifier = "Parent.Fork_1"
	messyJob := workspace.BuildSnapshotJob(cfg, src, messy)
	assert.Empty(t, validation.IsDNS1123Subdomain(messyJob.Name))
	assert.Contains(t, messyJob.Name, "adhoc-parentfork1-",
		"invalid runes are dropped, valid ones lowercased")
}

// TestSnapshotJobName_CanonicalFormat pins the exported helper as the single
// source of truth: it must equal BuildSnapshotJob's actual Job name, and for
// non-negative (JIT) indexes it must match the "snap-%s-%06d-%03d" format
// byte-for-byte, since existing Jobs are named that way.
func TestSnapshotJobName_CanonicalFormat(t *testing.T) {
	h := workspace.SnapshotHandle{SessionUID: "uid-1", TurnIndex: 3, Sequence: 7}
	cfg := workspace.CPByPodConfig{Namespace: "ns", SnapshotStorePVC: "store", Image: "busybox"}
	src := workspace.PVCRef{Namespace: "ns", Name: "ws"}

	name := workspace.SnapshotJobName(h)
	assert.Equal(t, "snap-uid-1-000003-007", name,
		"JIT snapshot Job name must stay byte-identical to the pre-dedupe format")
	assert.Equal(t, workspace.BuildSnapshotJob(cfg, src, h).Name, name,
		"SnapshotJobName must match the Job BuildSnapshotJob creates")

	adHoc := workspace.SnapshotHandle{SessionUID: "uid-1", TurnIndex: -1, Sequence: 0, Qualifier: "child-a"}
	assert.Equal(t, "snap-uid-1-adhoc-child-a-000", workspace.SnapshotJobName(adHoc))
	assert.Equal(t, workspace.BuildSnapshotJob(cfg, src, adHoc).Name, workspace.SnapshotJobName(adHoc))
}

// TestCPByPod_SnapshotDone_RestoreDone covers the Done probes against a fake
// client: missing Job → ErrSnapshotNotFound; no terminal condition → still
// running; JobComplete → done; JobFailed → ErrSnapshotJobFailed.
func TestCPByPod_SnapshotDone_RestoreDone(t *testing.T) {
	cfg := workspace.CPByPodConfig{
		Namespace:        "ns",
		SnapshotStorePVC: "snapstore",
		Image:            "busybox:1.36",
		ServiceAccount:   "ap-snapshotter",
	}
	src := workspace.PVCRef{Namespace: "ns", Name: "src"}
	dst := workspace.PVCRef{Namespace: "ns", Name: "dst"}
	h := workspace.SnapshotHandle{SessionUID: "u", TurnIndex: 1, Sequence: 0}

	withConditions := func(job *batchv1.Job, conds ...batchv1.JobCondition) *batchv1.Job {
		job.Status.Conditions = conds
		return job
	}

	cases := []struct {
		name      string
		jobs      []client.Object
		wantDone  bool
		wantErrIs error
	}{
		{
			name:      "no Job → ErrSnapshotNotFound",
			jobs:      nil,
			wantErrIs: workspace.ErrSnapshotNotFound,
		},
		{
			name:     "Job with no conditions → still running (false, nil)",
			jobs:     []client.Object{workspace.BuildSnapshotJob(cfg, src, h), workspace.BuildRestoreJob(cfg, h, dst)},
			wantDone: false,
		},
		{
			name: "JobComplete=True → done (true, nil)",
			jobs: []client.Object{
				withConditions(workspace.BuildSnapshotJob(cfg, src, h),
					batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}),
				withConditions(workspace.BuildRestoreJob(cfg, h, dst),
					batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}),
			},
			wantDone: true,
		},
		{
			name: "JobFailed=True → ErrSnapshotJobFailed",
			jobs: []client.Object{
				withConditions(workspace.BuildSnapshotJob(cfg, src, h),
					batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "BackoffLimitExceeded"}),
				withConditions(workspace.BuildRestoreJob(cfg, h, dst),
					batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "BackoffLimitExceeded"}),
			},
			wantErrIs: workspace.ErrSnapshotJobFailed,
		},
		{
			name: "JobFailed=False (transient probe) → still running",
			jobs: []client.Object{
				withConditions(workspace.BuildSnapshotJob(cfg, src, h),
					batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionFalse}),
				withConditions(workspace.BuildRestoreJob(cfg, h, dst),
					batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionFalse}),
			},
			wantDone: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sch := runtime.NewScheme()
			require.NoError(t, batchv1.AddToScheme(sch))
			c := fake.NewClientBuilder().WithScheme(sch).WithObjects(tc.jobs...).Build()
			s := workspace.NewCPByPod(c, cfg)

			snapDone, snapErr := s.SnapshotDone(ctx, src, h)
			restDone, restErr := s.RestoreDone(ctx, h, dst)

			if tc.wantErrIs != nil {
				require.Error(t, snapErr, "SnapshotDone must error")
				assert.ErrorIs(t, snapErr, tc.wantErrIs)
				assert.False(t, snapDone)
				require.Error(t, restErr, "RestoreDone must error")
				assert.ErrorIs(t, restErr, tc.wantErrIs)
				assert.False(t, restDone)
				return
			}
			require.NoError(t, snapErr)
			assert.Equal(t, tc.wantDone, snapDone, "SnapshotDone")
			require.NoError(t, restErr)
			assert.Equal(t, tc.wantDone, restDone, "RestoreDone")
		})
	}
}

// TestCPByPod_JobFailedMessageSurfaced ensures the Job's failure message
// lands in the returned error (no-silent-errors: the operator reading the
// condition must see WHY the Job failed without kubectl spelunking).
func TestCPByPod_JobFailedMessageSurfaced(t *testing.T) {
	ctx := context.Background()
	cfg := workspace.CPByPodConfig{Namespace: "ns", SnapshotStorePVC: "store", Image: "busybox"}
	src := workspace.PVCRef{Namespace: "ns", Name: "src"}
	h := workspace.SnapshotHandle{SessionUID: "u", TurnIndex: 1, Sequence: 0}

	job := workspace.BuildSnapshotJob(cfg, src, h)
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		Message: "Job has reached the specified backoff limit",
	}}
	sch := runtime.NewScheme()
	require.NoError(t, batchv1.AddToScheme(sch))
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(job).Build()

	_, err := workspace.NewCPByPod(c, cfg).SnapshotDone(ctx, src, h)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backoff limit", "the Job's failure message must surface in the error")
}
