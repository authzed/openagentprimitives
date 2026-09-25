package toolcall_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/toolcall"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

// The snapshot audit entry is append-only, so the operator's memory facade
// signs it — and signing first seeds the publisher's hash chain by READING the
// scope. The reconcile context carries no memory approval of its own, so the
// facade's capability door denied that read and the whole tool call wedged:
// the reconcile errored every ~40s forever, the ToolCall never dispatched, and
// the session sat Running with healthy pods.
//
// The door is right to fail closed; what was missing is the mint. This is the
// same up-front mint every other operator controller that reaches the
// in-process facade already makes.
func TestWaitPreDispatchSnapshot_handsTheRecorderAContextTheMemoryDoorAccepts(t *testing.T) {
	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tc-1", Namespace: "ns",
			Labels: map[string]string{spiceboxv1alpha1.LabelToolUseID: "tool_use_1"},
		},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session:             "bundle",
			Tool:                "git",
			PreDispatchSnapshot: &spiceboxv1alpha1.PreDispatchSnapshot{SessionUID: "uid-1", TurnIndex: 6},
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "snap-uid-1-000006-000"},
		Status:     batchv1.JobStatus{Succeeded: 1},
	}
	c := fake.NewClientBuilder().WithScheme(snapshotTestScheme(t)).WithObjects(tc, job).
		WithStatusSubresource(&spiceboxv1alpha1.ToolCall{}).Build()

	const scopeID = "ns/parent"
	var readErr, writeErr error
	recorder := func(ctx context.Context, _, _ string, _ workspace.SnapshotHandle, _, _ string) error {
		// Exactly what the signing facade does on the caller's context: read the
		// scope to seed the chain, then write the entry.
		readErr = memory.EnsureApproval(ctx, memory.ReadMemory, scopeID)
		writeErr = memory.EnsureApproval(ctx, memory.WriteMemory, scopeID)
		return nil
	}
	r := &toolcall.Reconciler{Client: c, RecordSnapshotFn: recorder, Now: func() time.Time { return time.Unix(1, 0) }}

	done, err := toolcall.WaitPreDispatchSnapshot(context.Background(), r, tc,
		workspace.PVCRef{Namespace: "ns", Name: "ap-workspace-parent"}, "parent")
	require.NoError(t, err)
	require.True(t, done)

	assert.NoError(t, readErr,
		"chain seeding reads the scope; without an approval the capability door denies it and the ToolCall never dispatches")
	assert.NoError(t, writeErr, "and the audit entry itself has to land")
}
