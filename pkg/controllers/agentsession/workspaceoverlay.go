package agentsession

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/workspacesource"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

// ensureWorkspaceOverlay seeds the session workspace PVC (dstClaim) from the
// AgentClass's bound WorkspaceSource, as a per-session copy-on-write overlay.
// Returns (_, true, nil) once the reflink cut has completed (caller persists
// at the end of reconcile); (_, false, nil) with a populated ctrl.Result while
// waiting/requeuing (this function owns its own persistence via r.applyStatus
// in that case). It snapshots status.ResolvedWorkspaceSource and, on a hard
// failure (missing WorkspaceSource, no storage, failed Job), marks the
// session boot-failed via r.markBootFailed (which persists itself — this
// function must not double-persist after calling it).
func (r *Reconciler) ensureWorkspaceOverlay(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ac *spiceboxv1alpha1.AgentClass, dstClaim string) (ctrl.Result, bool, error) {
	ref := ac.Spec.WorkspaceSource
	if ref == nil {
		return ctrl.Result{}, true, nil // no binding → nothing to seed
	}
	// Once the overlay is cut it is FROZEN for the session's lifetime: the
	// /workspace PVC is the agent's live working copy. Never re-run the reflink
	// over it — not if the finished Job is pruned out-of-band, and not if the
	// binding is re-pointed mid-session (a changed AgentClass workspaceSource
	// ref, or a changed source revision). Re-seeding would clobber the agent's
	// edits (a base-over-work overwrite). Latch on OverlayCut alone, NOT on the
	// ref still matching: a mid-session rebind keeps the workspace the session
	// started with; a new source needs a new session (same frozen-at-start
	// semantics as ResolvedSidecarToolboxes).
	if sess.Status.ResolvedWorkspaceSource != nil &&
		sess.Status.ResolvedWorkspaceSource.OverlayCut {
		return ctrl.Result{}, true, nil
	}
	if dstClaim == "" {
		// A WorkspaceSource is bound but no shared-workspace StorageClass is
		// configured, so there is nowhere to seed. Fail loudly rather than
		// silently give the agent an empty workspace.
		res, err := r.markBootFailed(ctx, sess, spiceboxv1alpha1.ReasonAgentSessionWorkspaceStorageRequired,
			"workspaceSource is bound but no shared-workspace StorageClass is configured")
		return res, false, err
	}

	var ws spiceboxv1alpha1.WorkspaceSource
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: ref.Ref}, &ws); err != nil {
		if apierrors.IsNotFound(err) {
			res, ferr := r.markBootFailed(ctx, sess, spiceboxv1alpha1.ReasonAgentClassWorkspaceSourceMissing,
				fmt.Sprintf("WorkspaceSource %q missing in namespace %q", ref.Ref, sess.Namespace))
			return res, false, ferr
		}
		return ctrl.Result{}, false, fmt.Errorf("get WorkspaceSource %q: %w", ref.Ref, err)
	}
	ready := meta.FindStatusCondition(ws.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ws.Status.BaseClaimName == "" {
		if ready != nil && ready.Status == metav1.ConditionFalse && ready.Reason == spiceboxv1alpha1.ReasonWorkspaceSourceMaterializeFailed {
			res, ferr := r.markBootFailed(ctx, sess, spiceboxv1alpha1.ReasonAgentSessionWorkspaceSourceNotReady,
				fmt.Sprintf("WorkspaceSource %q base materialization failed", ref.Ref))
			return res, false, ferr
		}
		// Base not materialized yet — wait (the AgentClass validator already
		// gates on Valid; Ready lags materialization). Requeue.
		sess.Status.ResolvedWorkspaceSource = &spiceboxv1alpha1.ResolvedWorkspaceSource{Ref: ref.Ref}
		if err := r.applyStatus(ctx, sess); err != nil {
			return ctrl.Result{}, false, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, false, nil
	}

	sess.Status.ResolvedWorkspaceSource = &spiceboxv1alpha1.ResolvedWorkspaceSource{
		Ref:                     ref.Ref,
		BaseClaimName:           ws.Status.BaseClaimName,
		Kind:                    ws.Spec.Source.Kind,
		Locator:                 ws.Spec.Source.Locator,
		Revision:                ws.Spec.Source.Ref,
		ReconcileImage:          r.WorkspaceReconcileImage,
		ReconcileServiceAccount: r.SnapshotServiceAccount,
	}

	dst := workspace.PVCRef{Namespace: sess.Namespace, Name: dstClaim}
	base := workspace.PVCRef{Namespace: sess.Namespace, Name: ws.Status.BaseClaimName}

	var job batchv1.Job
	switch err := r.Client.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: workspace.OverlayCutJobName(dst)}, &job); {
	case apierrors.IsNotFound(err):
		j := workspace.BuildOverlayCutJob(
			workspace.CPByPodConfig{Image: r.SnapshotImage, ServiceAccount: r.SnapshotServiceAccount},
			base, dst, workspacesource.BaseCheckoutSubdir)
		j.OwnerReferences = sessionOwnerRef(sess) // GC with the session
		if cerr := r.Client.Create(ctx, j); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return ctrl.Result{}, false, fmt.Errorf("create overlay-cut Job: %w", cerr)
		}
		if err := r.applyStatus(ctx, sess); err != nil {
			return ctrl.Result{}, false, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, false, nil // wait for it
	case err != nil:
		return ctrl.Result{}, false, fmt.Errorf("get overlay-cut Job: %w", err)
	}

	if job.Status.Succeeded > 0 {
		sess.Status.ResolvedWorkspaceSource.OverlayCut = true
		return ctrl.Result{}, true, nil // caller persists at end of reconcile
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			res, ferr := r.markBootFailed(ctx, sess, spiceboxv1alpha1.ReasonAgentSessionWorkspaceOverlayFailed,
				"workspace overlay cut Job failed")
			return res, false, ferr
		}
	}
	if err := r.applyStatus(ctx, sess); err != nil {
		return ctrl.Result{}, false, err
	}
	return ctrl.Result{RequeueAfter: 2 * time.Second}, false, nil // still running
}
