// pkg/controllers/agentsession/export_test.go
//
// Test-only exports for white-box unit tests in the agentsession_test package.
package agentsession

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
)

// TerminalPodWaitReasonForTest exposes the unexported terminalPodWaitReason
// helper to agentsession_test. Test-only.
func TerminalPodWaitReasonForTest(pod *corev1.Pod) (reason, message string, ok bool) {
	return terminalPodWaitReason(pod)
}

// CollectCredentialSecretNamesForTest exposes the unexported
// collectCredentialSecretNames helper to agentsession_test so it can be
// tested directly. Test-only.
func CollectCredentialSecretNamesForTest(ctx context.Context, ai *spiceboxv1alpha1.AgentIdentity) []string {
	return collectCredentialSecretNames(ctx, ai)
}

// ApplyStatusForTest exposes the unexported applyStatus helper so integration
// tests in package agentsession_test can drive it directly without wiring a
// full controller-runtime manager. Test-only.
func (r *Reconciler) ApplyStatusForTest(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	return r.applyStatus(ctx, sess)
}

// WithReconcileOriginalForTest exposes the unexported withReconcileOriginal so
// agentsession_test can drive applyStatus the way Reconcile does — with the
// per-reconcile write state installed — instead of falling back to the
// read-the-server base. Test-only.
func WithReconcileOriginalForTest(ctx context.Context, original *spiceboxv1alpha1.AgentSession) context.Context {
	return withReconcileOriginal(ctx, original)
}

// ReconcileOriginalForTest exposes the unexported reconcileOriginal so
// agentsession_test can assert that the start-of-reconcile snapshot (the
// edge-detection baseline) survives intervening status writes. Test-only.
func ReconcileOriginalForTest(ctx context.Context) *spiceboxv1alpha1.AgentSession {
	return reconcileOriginal(ctx)
}

// MarkBootFailedForTest exposes the unexported markBootFailed so
// agentsession_test can drive the whole terminal-boot-failure transition —
// including the trigger-status report it makes on behalf of a session whose
// runner never came up — without standing up a full reconcile. Test-only.
func (r *Reconciler) MarkBootFailedForTest(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, reason, msg string) (ctrl.Result, error) {
	return r.markBootFailed(ctx, sess, reason, msg)
}

// SidecarPodFailureForTest exposes the unexported sidecarPodFailure classifier
// to agentsession_test. Test-only.
func SidecarPodFailureForTest(pod *corev1.Pod) *spiceboxv1alpha1.SidecarPodFailure {
	return sidecarPodFailure(pod)
}

// EnsureDetectorPodForTest exposes the unexported ensureDetectorPod so
// agentsession_test can assert the write ORDER (deny-all-egress policy before
// the detector pod) without an envtest apiserver. Test-only.
func (r *Reconciler) EnsureDetectorPodForTest(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, spec cosidecar.Spec, pod *corev1.Pod) error {
	return r.ensureDetectorPod(ctx, sess, spec, pod)
}
