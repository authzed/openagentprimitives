package toolcall_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/toolcall"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

func snapshotTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, batchv1.AddToScheme(sch))
	require.NoError(t, corev1.AddToScheme(sch))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	return sch
}

func TestHandlePreDispatchSnapshot_LaunchesJob_AndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	sch := snapshotTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).Build()

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-1", Namespace: "ns"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "parent-bundle",
			Tool:    "bash_run",
			PreDispatchSnapshot: &spiceboxv1alpha1.PreDispatchSnapshot{
				SessionUID: "uid-1", TurnIndex: 3, Sequence: 0,
			},
		},
	}
	snap := workspace.NewCPByPod(c, workspace.CPByPodConfig{
		SnapshotStorePVC: "ap-snapstore-parent",
		Image:            "busybox:1.36",
		ServiceAccount:   "ap-snapshotter",
	})
	r := &toolcall.Reconciler{Client: c, Snapshotter: snap, Now: time.Now}

	src := workspace.PVCRef{Namespace: "ns", Name: "ap-workspace-parent"}
	require.NoError(t, toolcall.HandlePreDispatchSnapshot(ctx, r, tc, src, "parent"))

	var jobs batchv1.JobList
	require.NoError(t, c.List(ctx, &jobs))
	require.Len(t, jobs.Items, 1, "snapshot Job created")
	assert.Contains(t, jobs.Items[0].Name, "snap-uid-1-000003-000")

	// Idempotent: second call doesn't create another Job.
	require.NoError(t, toolcall.HandlePreDispatchSnapshot(ctx, r, tc, src, "parent"))
	require.NoError(t, c.List(ctx, &jobs))
	assert.Len(t, jobs.Items, 1)
}

func TestHandlePreDispatchSnapshot_NoSpec_NoOp(t *testing.T) {
	ctx := context.Background()
	sch := snapshotTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).Build()

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-1", Namespace: "ns"},
		Spec:       spiceboxv1alpha1.ToolCallSpec{Session: "parent-bundle", Tool: "bash_run"},
	}
	r := &toolcall.Reconciler{Client: c, Snapshotter: workspace.NewCPByPod(c, workspace.CPByPodConfig{})}
	require.NoError(t, toolcall.HandlePreDispatchSnapshot(ctx, r, tc, workspace.PVCRef{}, "parent"))

	var jobs batchv1.JobList
	require.NoError(t, c.List(ctx, &jobs))
	assert.Empty(t, jobs.Items, "no PreDispatchSnapshot → no Job")
}

func TestWaitPreDispatchSnapshot_JobSucceeded_RecordsAudit(t *testing.T) {
	ctx := context.Background()
	sch := snapshotTestScheme(t)

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tc-1", Namespace: "ns",
			Labels: map[string]string{spiceboxv1alpha1.LabelToolUseID: "tool_use_xyz"},
		},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session:             "parent-bundle",
			Tool:                "bash_run",
			PreDispatchSnapshot: &spiceboxv1alpha1.PreDispatchSnapshot{SessionUID: "uid-1", TurnIndex: 3, Sequence: 0},
		},
	}
	succeededJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "snap-uid-1-000003-000",
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(tc, succeededJob).
		WithStatusSubresource(&batchv1.Job{}, &spiceboxv1alpha1.ToolCall{}).
		Build()

	var recordCount int
	var recorded struct {
		toolUseID  string
		bundleName string
		sessName   string
		sessNS     string
		h          workspace.SnapshotHandle
	}
	recorder := func(ctx context.Context, sessName, sessNS string, h workspace.SnapshotHandle, toolUseID, bundleName string) error {
		recordCount++
		recorded.toolUseID = toolUseID
		recorded.bundleName = bundleName
		recorded.sessName = sessName
		recorded.sessNS = sessNS
		recorded.h = h
		return nil
	}
	r := &toolcall.Reconciler{Client: c, RecordSnapshotFn: recorder, Now: func() time.Time { return time.Unix(1, 0) }}

	done, err := toolcall.WaitPreDispatchSnapshot(ctx, r, tc, workspace.PVCRef{Namespace: "ns", Name: "ap-workspace-parent"}, "parent")
	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, 1, recordCount, "recorder called exactly once on first reconcile")
	assert.Equal(t, "tool_use_xyz", recorded.toolUseID)
	assert.Equal(t, "parent-bundle", recorded.bundleName)
	assert.Equal(t, "parent", recorded.sessName, "audit scoped to parent AgentSession, not bundle")
	assert.Equal(t, "ns", recorded.sessNS)
	assert.Equal(t, workspace.SnapshotHandle{SessionUID: "uid-1", TurnIndex: 3, Sequence: 0, SnapshotStorePVC: "ap-snapstore-parent"}, recorded.h)

	// Re-entry (streaming ToolCall reconcile): recorder must NOT fire again.
	done2, err2 := toolcall.WaitPreDispatchSnapshot(ctx, r, tc, workspace.PVCRef{Namespace: "ns", Name: "ap-workspace-parent"}, "parent")
	require.NoError(t, err2)
	assert.True(t, done2)
	assert.Equal(t, 1, recordCount, "recorder skipped on re-entry: SnapshotAuditRecorded condition is True")

	_ = types.NamespacedName{}
}

func TestWaitPreDispatchSnapshot_JobFailed_ReturnsError(t *testing.T) {
	ctx := context.Background()
	sch := snapshotTestScheme(t)

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-1", Namespace: "ns"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session:             "parent-bundle",
			PreDispatchSnapshot: &spiceboxv1alpha1.PreDispatchSnapshot{SessionUID: "uid-1", TurnIndex: 3, Sequence: 0},
		},
	}
	failedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "snap-uid-1-000003-000"},
		Status:     batchv1.JobStatus{Failed: 1},
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(tc, failedJob).Build()
	r := &toolcall.Reconciler{Client: c, Now: time.Now}

	done, err := toolcall.WaitPreDispatchSnapshot(ctx, r, tc, workspace.PVCRef{Namespace: "ns", Name: "ws"}, "parent")
	assert.False(t, done)
	assert.Error(t, err)
}

func TestWaitPreDispatchSnapshot_JobStillRunning_NotDone(t *testing.T) {
	ctx := context.Background()
	sch := snapshotTestScheme(t)
	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-1", Namespace: "ns"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session:             "parent-bundle",
			PreDispatchSnapshot: &spiceboxv1alpha1.PreDispatchSnapshot{SessionUID: "uid-1", TurnIndex: 3, Sequence: 0},
		},
	}
	runningJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "snap-uid-1-000003-000"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(tc, runningJob).Build()
	r := &toolcall.Reconciler{Client: c, Now: time.Now}

	done, err := toolcall.WaitPreDispatchSnapshot(ctx, r, tc, workspace.PVCRef{Namespace: "ns", Name: "ws"}, "parent")
	require.NoError(t, err)
	assert.False(t, done, "running Job → not done; caller should requeue")
}
