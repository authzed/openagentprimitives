package agentsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func pvcProvScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

func pendingPVC(name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
}

func provFailedEvent(pvc, msg string, ageSec int) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "default", Name: pvc + "-evt-" + msg[:1]},
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "default", Name: pvc},
		Reason:         "ProvisioningFailed",
		Message:        msg,
		Type:           corev1.EventTypeWarning,
		LastTimestamp:  metav1.NewTime(time.Now().Add(-time.Duration(ageSec) * time.Second)),
	}
}

// A session must fail fast with the provider's ACTUAL provisioning reason when
// its workspace PVC cannot bind — the incident's "less than minimum share size"
// / "Cloud Filestore API disabled" — instead of waiting out the 8-min bundle
// deadline behind a generic message.
func TestFirstPVCProvisioningFailure(t *testing.T) {
	t.Run("unbound PVC with a ProvisioningFailed event returns the provider message", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(pvcProvScheme(t)).WithObjects(
			pendingPVC("rb-workspace"),
			provFailedEvent("rb-workspace", "Request bytes 2147483648 is less than minimum share size bytes 10737418240", 5),
		).Build()
		r := &Reconciler{Client: c}

		msg, ok := r.firstPVCProvisioningFailure(context.Background(), "default", []string{"rb-workspace"})
		require.True(t, ok)
		assert.Contains(t, msg, "rb-workspace")
		assert.Contains(t, msg, "minimum share size")
	})

	t.Run("picks the most recent event when several exist", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(pvcProvScheme(t)).WithObjects(
			pendingPVC("rb-workspace"),
			provFailedEvent("rb-workspace", "older transient error", 300),
			provFailedEvent("rb-workspace", "Cloud Filestore API has not been used or is disabled", 5),
		).Build()
		r := &Reconciler{Client: c}

		msg, ok := r.firstPVCProvisioningFailure(context.Background(), "default", []string{"rb-workspace"})
		require.True(t, ok)
		assert.Contains(t, msg, "Filestore API")
	})

	t.Run("a transient cold-start error is NOT fast-failed", func(t *testing.T) {
		// Filestore multishare emits these while the backing instance is being
		// created — they resolve on retry, so a session must keep waiting, not
		// die. Fast-failing here would kill every first-of-a-cold-pool session.
		c := fake.NewClientBuilder().WithScheme(pvcProvScheme(t)).WithObjects(
			pendingPVC("rb-workspace"),
			provFailedEvent("rb-workspace", "rpc error: code = Aborted desc = All eligible filestore instances are busy. Instance fs-x busy with operation type instancecreate", 3),
		).Build()
		r := &Reconciler{Client: c}

		_, ok := r.firstPVCProvisioningFailure(context.Background(), "default", []string{"rb-workspace"})
		assert.False(t, ok, "an Aborted/instances-busy cold-start error is transient and must not fast-fail the session")
	})

	t.Run("a DeadlineExceeded provisioning error is transient, not fast-failed", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(pvcProvScheme(t)).WithObjects(
			pendingPVC("rb-workspace"),
			provFailedEvent("rb-workspace", "rpc error: code = DeadlineExceeded desc = context deadline exceeded", 3),
		).Build()
		r := &Reconciler{Client: c}

		_, ok := r.firstPVCProvisioningFailure(context.Background(), "default", []string{"rb-workspace"})
		assert.False(t, ok)
	})

	t.Run("a bound PVC is never reported", func(t *testing.T) {
		bound := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "rb-workspace"},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		}
		c := fake.NewClientBuilder().WithScheme(pvcProvScheme(t)).WithObjects(
			bound,
			provFailedEvent("rb-workspace", "stale error from before it bound", 600),
		).Build()
		r := &Reconciler{Client: c}

		_, ok := r.firstPVCProvisioningFailure(context.Background(), "default", []string{"rb-workspace"})
		assert.False(t, ok, "a PVC that has since bound must not be reported as a provisioning failure")
	})

	t.Run("unbound PVC with no ProvisioningFailed event returns false", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(pvcProvScheme(t)).WithObjects(pendingPVC("rb-workspace")).Build()
		r := &Reconciler{Client: c}

		_, ok := r.firstPVCProvisioningFailure(context.Background(), "default", []string{"rb-workspace", ""})
		assert.False(t, ok)
	})
}
