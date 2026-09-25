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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/toolcall"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

// A snapshot Job that can never create a pod reports Succeeded=0 AND Failed=0 —
// byte-identical to one still working — so the wait returned "not done" and the
// caller requeued every 2s forever. The ToolCall never resolved, the agent went
// silent mid-turn, and nothing reached the user.
//
// Observed live: `ap-snapshotter` exists only in the system namespace, so a Job
// in a session namespace logged FailedCreate x15 over 9m39s while the operator
// logged "toolspec allowed" on a 2s loop. The user saw an agent that stopped.
func TestWaitPreDispatchSnapshot_failsAStalledJobRatherThanWaitingForever(t *testing.T) {
	tc, job := stalledSnapshotFixture(t, 20*time.Minute)
	r := &toolcall.Reconciler{Client: fake.NewClientBuilder().
		WithScheme(snapshotTestScheme(t)).WithObjects(tc, job).Build()}

	done, err := toolcall.WaitPreDispatchSnapshot(context.Background(), r, tc,
		workspace.PVCRef{Namespace: "default", Name: "ws"}, "sess")

	assert.False(t, done)
	require.Error(t, err, "a Job stalled past the deadline must FAIL the call, not requeue")
	assert.Contains(t, err.Error(), "could not start",
		"the message must say the Job never ran, not merely that it is incomplete")
	assert.Contains(t, err.Error(), job.Name,
		"naming the Job is what lets an operator go read its events")
}

// When the Job DID create a pod (Active>0) but it never ran, the cause is not a
// missing ServiceAccount (that leaves Active=0 with FailedCreate events) — the
// pod is stuck Pending, typically unschedulable on an unbound PVC (e.g. an
// undersized snapshot-store PVC that a StorageClass floor rejects). The message
// must point at the pod's scheduling, not send the reader chasing the SA.
// Observed live: acaa7788's snapshot Job sat Active=1 with the SA present, its
// pod Pending on the 8Gi snapshot-store PVC that Filestore's 10Gi floor refused.
func TestWaitPreDispatchSnapshot_stalledPodPointsAtSchedulingNotServiceAccount(t *testing.T) {
	tc, job := stalledSnapshotFixture(t, 20*time.Minute)
	job.Status.Active = 1 // a pod WAS created — rules out the missing-SA case
	r := &toolcall.Reconciler{Client: fake.NewClientBuilder().
		WithScheme(snapshotTestScheme(t)).WithObjects(tc, job).Build()}

	done, err := toolcall.WaitPreDispatchSnapshot(context.Background(), r, tc,
		workspace.PVCRef{Namespace: "default", Name: "ws"}, "sess")

	assert.False(t, done)
	require.Error(t, err, "a Job stalled past the deadline must FAIL the call")
	assert.Contains(t, err.Error(), "could not start")
	assert.Contains(t, err.Error(), job.Name)
	assert.NotContains(t, err.Error(), "ServiceAccount",
		"Active>0 means the pod was created; the missing-SA hint misdiagnoses it")
	assert.Contains(t, err.Error(), "PersistentVolumeClaim",
		"the actionable cause for a created-but-never-ran pod is an unschedulable pod, e.g. an unbound PVC")
}

// Inside the deadline it still requeues: a slow snapshot of a large workspace
// is normal and must not be turned into a failure.
func TestWaitPreDispatchSnapshot_stillWaitsInsideTheDeadline(t *testing.T) {
	tc, job := stalledSnapshotFixture(t, 5*time.Second)
	r := &toolcall.Reconciler{Client: fake.NewClientBuilder().
		WithScheme(snapshotTestScheme(t)).WithObjects(tc, job).Build()}

	done, err := toolcall.WaitPreDispatchSnapshot(context.Background(), r, tc,
		workspace.PVCRef{Namespace: "default", Name: "ws"}, "sess")

	require.NoError(t, err)
	assert.False(t, done, "still in progress — the caller requeues")
}

func stalledSnapshotFixture(t *testing.T, age time.Duration) (*spiceboxv1alpha1.ToolCall, *batchv1.Job) {
	t.Helper()
	h := workspace.SnapshotHandle{SessionUID: "u", TurnIndex: 2, Sequence: 0, SnapshotStorePVC: "store"}
	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc", Namespace: "default"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			PreDispatchSnapshot: &spiceboxv1alpha1.PreDispatchSnapshot{
				SessionUID: "u", TurnIndex: 2, Sequence: 0,
			},
		},
	}
	// Succeeded=0, Failed=0, Active=0 — the shape of a Job whose pods were
	// never created at all.
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              workspace.SnapshotJobName(h),
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
		},
	}
	return tc, job
}

// The root cause of the stall. Snapshot Jobs run in the SESSION's namespace and
// reference ap-snapshotter, which the install bundle creates only in
// agentprimitives-system. Every session elsewhere therefore produced a Job that
// could not create a pod:
//
//	FailedCreate: pods ... forbidden: error looking up service account
//	default/ap-snapshotter: serviceaccount "ap-snapshotter" not found
//
// The account needs no permissions, so ensuring it per-namespace is a bare
// create. Namespaces are dynamic, so a static manifest cannot cover them —
// whoever launches the Job has to.
func TestHandlePreDispatchSnapshot_ensuresTheServiceAccountInTheJobsNamespace(t *testing.T) {
	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc", Namespace: "team-a"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			PreDispatchSnapshot: &spiceboxv1alpha1.PreDispatchSnapshot{SessionUID: "u", TurnIndex: 1},
		},
	}
	c := fake.NewClientBuilder().WithScheme(snapshotTestScheme(t)).WithObjects(tc).Build()
	r := &toolcall.Reconciler{Client: c, Snapshotter: workspace.NewCPByPod(c, workspace.CPByPodConfig{})}

	require.NoError(t, toolcall.HandlePreDispatchSnapshot(context.Background(), r, tc,
		workspace.PVCRef{Namespace: "team-a", Name: "ws"}, "sess"))

	var sa corev1.ServiceAccount
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "team-a", Name: workspace.SnapshotServiceAccount}, &sa),
		"the Job's namespace must have the account before the Job is created")
}

// Idempotent: a second call must not error on an account that already exists.
func TestHandlePreDispatchSnapshot_serviceAccountEnsureIsIdempotent(t *testing.T) {
	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc", Namespace: "team-a"},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			PreDispatchSnapshot: &spiceboxv1alpha1.PreDispatchSnapshot{SessionUID: "u", TurnIndex: 1},
		},
	}
	existing := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: workspace.SnapshotServiceAccount}}
	c := fake.NewClientBuilder().WithScheme(snapshotTestScheme(t)).WithObjects(tc, existing).Build()
	r := &toolcall.Reconciler{Client: c, Snapshotter: workspace.NewCPByPod(c, workspace.CPByPodConfig{})}

	assert.NoError(t, toolcall.HandlePreDispatchSnapshot(context.Background(), r, tc,
		workspace.PVCRef{Namespace: "team-a", Name: "ws"}, "sess"))
}
