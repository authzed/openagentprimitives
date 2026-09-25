//go:build integration

// pkg/controllers/toolcall/snapshot_envtest_test.go
//
// Integration test for the JIT snapshot end-to-end path:
//  1. Create a ToolCall with PreDispatchSnapshot against a real envtest apiserver.
//  2. Call HandlePreDispatchSnapshot — asserts a snapshot Job is created.
//  3. Simulate Job success by patching status.
//  4. Call WaitPreDispatchSnapshot — asserts (done=true, nil error) and that
//     the audit record was written to the in-memory store.
package toolcall_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/controllers/toolcall"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/tool_dispatch_snapshot"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

// TestEnvtest_JITSnapshot_OnReadwriteToolCall drives a ToolCall with
// PreDispatchSnapshot through the toolcall snapshot helpers against a
// real apiserver, simulates Job success by patching status, and asserts
// the audit entry was written.
func TestEnvtest_JITSnapshot_OnReadwriteToolCall(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	env := testenv.Shared(t)

	ns := "default"

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "demo",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "hi"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, sess))

	sbx := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "parent-bundle",
			Namespace: ns,
			Labels:    map[string]string{"agentprimitives.authzed.com/agentsession": "parent"},
		},
		Spec: spiceboxv1alpha1.SpiceboxSessionSpec{Class: "demo"},
	}
	require.NoError(t, env.Client.Create(ctx, sbx))

	// Create workspace + snapstore PVCs (cp-by-pod will mount these).
	wsPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "ap-workspace-parent", Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: ptr.To("standard"),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, wsPVC))

	snapPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "ap-snapstore-parent", Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: ptr.To("standard"),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("4Gi"),
				},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, snapPVC))

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tc-1",
			Namespace: ns,
			Labels:    map[string]string{spiceboxv1alpha1.LabelToolUseID: "tool_use_xyz"},
		},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "parent-bundle",
			Tool:    "bash_run",
			PreDispatchSnapshot: &spiceboxv1alpha1.PreDispatchSnapshot{
				SessionUID: "uid-1", TurnIndex: 3, Sequence: 0,
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, tc))

	// Wire reconciler with in-memory mem + cp-by-pod against envtest client.
	mem := memory.NewLocal(inmem.NewBackend())
	snap := workspace.NewCPByPod(env.Client, workspace.CPByPodConfig{
		Image:          "busybox:1.36",
		ServiceAccount: "ap-snapshotter",
	})
	r := &toolcall.Reconciler{
		Client:      env.Client,
		Snapshotter: snap,
		RecordSnapshotFn: func(ctx context.Context, sessName, sessNS string, h workspace.SnapshotHandle, toolUseID, bundleName string) error {
			scope := memory.Scope{Kind: "AgentSession", ID: sessNS + "/" + sessName}
			return tool_dispatch_snapshot.Record(ctx, mem, scope, tool_dispatch_snapshot.Content{
				ToolUseID:       toolUseID,
				SpiceboxSession: bundleName,
				TurnIndex:       h.TurnIndex,
				Sequence:        h.Sequence,
				SessionUID:      h.SessionUID,
			})
		},
		Now: time.Now,
	}

	src := workspace.PVCRef{Namespace: ns, Name: "ap-workspace-parent"}
	require.NoError(t, toolcall.HandlePreDispatchSnapshot(ctx, r, tc, src, "parent"))

	// Verify Job was created in envtest.
	var jobs batchv1.JobList
	require.NoError(t, env.Client.List(ctx, &jobs))
	require.Len(t, jobs.Items, 1)
	job := jobs.Items[0]
	assert.Contains(t, job.Name, "snap-uid-1-000003-000")

	// Simulate Job success by patching status.
	job.Status.Succeeded = 1
	require.NoError(t, env.Client.Status().Update(ctx, &job))

	done, err := toolcall.WaitPreDispatchSnapshot(ctx, r, tc, src, "parent")
	require.NoError(t, err)
	assert.True(t, done)

	// Verify the audit entry was recorded. The read goes through the gated
	// memory door; this test exercises snapshot recording, not per-caller authz,
	// so clear the door with a system approval.
	scope := memory.Scope{Kind: "AgentSession", ID: ns + "/parent"}
	all, err := tool_dispatch_snapshot.ReadAll(memory.WithSystemApproval(ctx, "test"), mem, scope)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "tool_use_xyz", all[0].ToolUseID)
	assert.Equal(t, 3, all[0].TurnIndex)
	assert.Equal(t, "uid-1", all[0].SessionUID)
	assert.Equal(t, "parent-bundle", all[0].SpiceboxSession)
}
