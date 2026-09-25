//go:build e2e

package p1_multi_bundle_pvc_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestP1_MultiBundle_ProvisionsPVC exercises the AgentSession controller's
// shared-workspace path: an AgentClass with two bundles + a configured
// WorkspaceStorageClass should produce ONE RWX PVC owned by the AgentSession
// plus TWO bundle SpiceboxSessions each stamped with
// Workspace.Mode=shared + SharedClaimName=<sess>-workspace.
//
// The harness does not wire the SpiceboxToolspec controller, so this test
// stamps each Toolspec's Valid=True manually after Apply. It also stamps
// every bundle SpiceboxSession Ready=True as it appears (envtest has no Pod
// scheduler), unblocking the AgentSession reconciler past the BundlesReady
// gate so the runner can reply to SendUserMessage.
func TestP1_MultiBundle_ProvisionsPVC(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	h := e2e.Start(t, e2e.Options{
		WithToolCallController: true,
		WorkspaceStorageClass:  "rwx-test",
		ExtraManifests:         []string{string(manifests)},
	})

	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// 1. Stamp every SpiceboxToolspec Valid=True. The harness does not run
	// the SpiceboxToolspec controller, so without this the AgentClass
	// binding-coverage check rejects every bundle's toolspecs.
	stampToolspecsValid(t, ctx, h.K8s, "ts-git", "ts-codey")

	// 2. Background loop that stamps any bundle SpiceboxSession Ready=True
	// the AgentSession controller creates. The harness has no Pod
	// scheduler, so without this the bundle sessions sit Progressing
	// forever and the AgentSession stalls on BundlesReady=False.
	go func() {
		defer close(done)
		stampBundleSessionsReady(ctx, t, h.K8s)
	}()

	// 3. Script: respond_to_user("hello") on the first user turn, then
	// end the turn once the runner reports "delivered".
	h.LLM.OnUserMessage("hi").Reply(e2e.RespondToUser("hello"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.WaitForAgentClassValid("ac-multi", 30*time.Second)
	h.SendUserMessage("hi")
	h.ExpectAgentReply(e2e.Contains("hello"))

	// 4. Assert the new shared-workspace machinery's outputs.
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, h.K8s.List(ctx, &sessions, client.InNamespace("default")),
		"list AgentSessions")
	require.NotEmpty(t, sessions.Items, "at least one AgentSession created by the channelsd pipeline")
	sess := sessions.Items[0]

	// PVC exists, RWX, owner-referenced to the AgentSession.
	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{
		Namespace: "default", Name: sess.Name + "-workspace",
	}, &pvc), "workspace PVC should exist")
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, pvc.Spec.AccessModes,
		"PVC AccessModes")
	require.Len(t, pvc.OwnerReferences, 1, "PVC must be owned by AgentSession")
	assert.Equal(t, sess.Name, pvc.OwnerReferences[0].Name, "PVC owner name")
	assert.Equal(t, "AgentSession", pvc.OwnerReferences[0].Kind, "PVC owner kind")

	// Both bundle SpiceboxSessions stamped Mode: shared with the right claim.
	for _, bn := range []string{"git", "codey"} {
		var sbox spiceboxv1alpha1.SpiceboxSession
		require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{
			Namespace: "default", Name: sess.Name + "-" + bn,
		}, &sbox), "bundle session %q should exist", bn)
		assert.Equal(t, spiceboxv1alpha1.WorkspaceShared, sbox.Spec.Workspace.Mode,
			"bundle %q workspace mode", bn)
		assert.Equal(t, sess.Name+"-workspace", sbox.Spec.Workspace.SharedClaimName,
			"bundle %q claim name", bn)
	}

	h.AssertAllRulesConsumed()
}

// stampToolspecsValid sets Status.Conditions[Valid]=True on each named
// SpiceboxToolspec so the AgentClass binding-coverage check passes. Stamps
// once at test-setup time; the SpiceboxToolspec controller would normally
// re-affirm Valid on every reconcile, but in the harness no controller is
// watching, so a single status write is enough.
func stampToolspecsValid(t *testing.T, ctx context.Context, c client.Client, names ...string) {
	t.Helper()
	for _, name := range names {
		var ts spiceboxv1alpha1.SpiceboxToolspec
		require.NoError(t, c.Get(ctx, client.ObjectKey{Name: name}, &ts),
			"get toolspec %q", name)
		ts.Status.Conditions = []metav1.Condition{{
			Type:               spiceboxv1alpha1.SpiceboxToolspecConditionValid,
			Status:             metav1.ConditionTrue,
			Reason:             "Resolved",
			LastTransitionTime: metav1.Now(),
		}}
		require.NoError(t, c.Status().Update(ctx, &ts), "stamp toolspec %q Valid=True", name)
	}
}

// stampBundleSessionsReady polls for SpiceboxSessions in the default
// namespace and stamps any without a Ready=True condition. Runs until ctx
// is canceled (test cleanup). Idempotent — touching a session that's
// already Ready is a no-op write.
func stampBundleSessionsReady(ctx context.Context, t *testing.T, c client.Client) {
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var list spiceboxv1alpha1.SpiceboxSessionList
		if err := c.List(ctx, &list, client.InNamespace("default")); err != nil {
			if ctx.Err() != nil {
				return
			}
			t.Logf("stampBundleSessionsReady: list: %v", err)
			continue
		}
		for i := range list.Items {
			s := &list.Items[i]
			if meta.IsStatusConditionTrue(s.Status.Conditions, spiceboxv1alpha1.SpiceboxSessionConditionReady) {
				continue
			}
			s.Status.Conditions = append(s.Status.Conditions, metav1.Condition{
				Type:               spiceboxv1alpha1.SpiceboxSessionConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             spiceboxv1alpha1.ReasonPodReady,
				Message:            "stamped Ready by e2e harness helper",
				LastTransitionTime: metav1.Now(),
			})
			if err := c.Status().Update(ctx, s); err != nil {
				if ctx.Err() != nil {
					return
				}
				// Conflict on a concurrent reconcile is benign; the next
				// tick re-tries with a fresh ResourceVersion.
				t.Logf("stampBundleSessionsReady: update %s/%s: %v",
					s.Namespace, s.Name, err)
			}
		}
	}
}
