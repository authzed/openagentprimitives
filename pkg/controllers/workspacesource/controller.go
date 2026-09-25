package workspacesource

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/registry"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=workspacesources,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=workspacesources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=workspacesources/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

// Reconciler materializes a WorkspaceSource's shared base checkout.
type Reconciler struct {
	Client                    client.Client
	BaseStorageClass          string // empty → Ready=False (BaseStorageUnconfigured)
	BaseSize                  string // default 2Gi
	MaterializeImage          string // image with git; default from operator flag
	MaterializeServiceAccount string
	RevalidateInterval        time.Duration // 0 → 5m
}

func (r *Reconciler) interval() time.Duration {
	if r.RevalidateInterval > 0 {
		return r.RevalidateInterval
	}
	return 5 * time.Minute
}

func (r *Reconciler) baseSize() string {
	if r.BaseSize != "" {
		return r.BaseSize
	}
	return "2Gi"
}

// parseRefresh interprets spec.base.refresh: "" and "onDemand" both mean "no
// scheduled refresh" (ok=false); anything else must parse as a Go duration.
// An unparseable value returns ok=false rather than propagating an error —
// ValidateWorkspaceSourceSpec already rejects it at the validate phase, so by
// the time the refresh phase runs the value is either valid or the source is
// already Valid=False and materialize never got this far.
func parseRefresh(s string) (time.Duration, bool) {
	if s == "" || s == "onDemand" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, false
	}
	return d, true
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ws spiceboxv1alpha1.WorkspaceSource
	if err := r.Client.Get(ctx, req.NamespacedName, &ws); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ws.DeletionTimestamp.IsZero() {
		// No finalizer in Phase 2 — the owner-referenced base PVC + materialize
		// Job are GC'd by Kubernetes. Nothing to do on delete.
		return ctrl.Result{}, nil
	}

	validate := func(ctx context.Context) apreconcile.Outcome {
		if err := spiceboxv1alpha1.ValidateWorkspaceSourceSpec(ws.Name, ws.Spec); err != nil {
			conditions.SetFalse(&ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionValid,
				spiceboxv1alpha1.ReasonWorkspaceSourceSpecInvalid, err.Error())
			return apreconcile.StopAfter()
		}
		drv, ok := registry.Get(ws.Spec.Source.Kind)
		if !ok {
			conditions.SetFalse(&ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionValid,
				spiceboxv1alpha1.ReasonWorkspaceSourceUnknownKind,
				fmt.Sprintf("no registered workspace-source driver for kind %q", ws.Spec.Source.Kind))
			return apreconcile.StopAfter()
		}
		if err := drv.Validate(SpecToDriver(ws.Spec)); err != nil {
			conditions.SetFalse(&ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionValid,
				spiceboxv1alpha1.ReasonWorkspaceSourceSpecInvalid, err.Error())
			return apreconcile.StopAfter()
		}
		conditions.SetTrue(&ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionValid,
			spiceboxv1alpha1.ReasonWorkspaceSourceSpecOK)
		return apreconcile.Continue()
	}

	materialize := func(ctx context.Context) apreconcile.Outcome {
		if r.BaseStorageClass == "" {
			conditions.SetFalse(&ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionReady,
				spiceboxv1alpha1.ReasonWorkspaceSourceBaseUnconfigured,
				"no base StorageClass configured (operator --workspace-base-storage-class)")
			return apreconcile.StopAfter()
		}
		// ensure base PVC (honor per-resource spec.base.size, else operator default)
		size := ws.Spec.Base.Size
		if size == "" {
			size = r.baseSize()
		}
		baseName := BaseClaimName(&ws)
		var pvc corev1.PersistentVolumeClaim
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: baseName}, &pvc); apierrors.IsNotFound(err) {
			if cerr := r.Client.Create(ctx, BuildBasePVC(&ws, r.BaseStorageClass, size)); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
				return apreconcile.FailWith(fmt.Errorf("create base PVC: %w", cerr))
			}
		} else if err != nil {
			return apreconcile.FailWith(fmt.Errorf("get base PVC: %w", err))
		}
		ws.Status.BaseClaimName = baseName

		// ensure materialize Job
		drv, ok := registry.Get(ws.Spec.Source.Kind)
		if !ok {
			return apreconcile.FailWith(fmt.Errorf("workspace-source driver %q not registered", ws.Spec.Source.Kind))
		}
		cmds, err := drv.MaterializeCommands(SpecToDriver(ws.Spec), baseCheckoutDir)
		if err != nil {
			return apreconcile.FailWith(fmt.Errorf("build materialize commands: %w", err))
		}
		var job batchv1.Job
		switch err := r.Client.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: MaterializeJobName(&ws)}, &job); {
		case apierrors.IsNotFound(err):
			if cerr := r.Client.Create(ctx, BuildMaterializeJob(&ws, baseName, r.MaterializeServiceAccount, r.MaterializeImage, cmds)); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
				return apreconcile.FailWith(fmt.Errorf("create materialize Job: %w", cerr))
			}
			conditions.SetFalse(&ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionReady,
				spiceboxv1alpha1.ReasonWorkspaceSourceMaterializing, "materialize Job created")
			return apreconcile.StopAfter()
		case err != nil:
			return apreconcile.FailWith(fmt.Errorf("get materialize Job: %w", err))
		}
		// inspect Job status
		if job.Status.Succeeded > 0 {
			// The materialize Job is created once with a generation label. If the
			// spec has since changed, the base still holds the older checkout —
			// Phase 2 materializes once (re-materialize is a later-phase refresh
			// capability). Report that divergence honestly rather than latching
			// Ready=True for a generation we never materialized.
			if job.Labels[BaseGenerationLabel] != generationString(ws.Generation) {
				// Best-effort: clean up any in-flight refresh Job for the
				// generation being abandoned so it isn't orphaned. A NotFound is
				// expected when no refresh is in flight and is the only error
				// this path tolerates silently; anything else (RBAC, apiserver
				// 5xx) leaves the Job orphaned and must be visible, like the two
				// sibling deletes below.
				if derr := r.Client.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: ws.Namespace, Name: RefreshJobName(&ws)}},
					client.PropagationPolicy(metav1.DeletePropagationBackground)); derr != nil && !apierrors.IsNotFound(derr) {
					log.FromContext(ctx).Info("failed to delete the abandoned-generation refresh Job; it may be left orphaned",
						"workspacesource", ws.Name, "job", RefreshJobName(&ws), "err", derr.Error())
				}
				conditions.SetFalse(&ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionReady,
					spiceboxv1alpha1.ReasonWorkspaceSourceSpecChanged,
					fmt.Sprintf("base materialized at generation %s; spec now at generation %d; re-materialize is deferred to refresh",
						job.Labels[BaseGenerationLabel], ws.Generation))
				return apreconcile.StopAfter()
			}
			if ws.Status.LastMaterializedAt == nil {
				now := metav1.NewTime(time.Now())
				ws.Status.LastMaterializedAt = &now
			}
			conditions.SetTrue(&ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionReady,
				spiceboxv1alpha1.ReasonWorkspaceSourceMaterialized)
			return apreconcile.Continue()
		}
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				conditions.SetFalse(&ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionReady,
					spiceboxv1alpha1.ReasonWorkspaceSourceMaterializeFailed, "materialize Job failed")
				return apreconcile.StopAfter()
			}
		}
		conditions.SetFalse(&ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionReady,
			spiceboxv1alpha1.ReasonWorkspaceSourceMaterializing, "materialize Job in progress")
		return apreconcile.StopAfter()
	}

	// refresh keeps an already-materialized base warm: when spec.base.refresh
	// names an interval and that interval has elapsed since the last refresh
	// (or the base has never been refreshed), it runs the driver's
	// SyncCommands (a pull, not a re-clone) as a one-shot Job and stamps
	// status.lastRefreshedAt on success. The base stays Ready=True throughout
	// — a refresh in progress (or a failed one) leaves the prior checkout
	// usable; only the next scheduled attempt is affected.
	refresh := func(ctx context.Context) apreconcile.Outcome {
		ready := meta.FindStatusCondition(ws.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionReady)
		if ready == nil || ready.Status != metav1.ConditionTrue {
			return apreconcile.Continue() // base not materialized yet
		}
		interval, ok := parseRefresh(ws.Spec.Base.Refresh)
		if !ok {
			return apreconcile.Continue() // onDemand
		}
		if ws.Status.LastRefreshedAt == nil {
			// First time Ready with a refresh interval configured: baseline the
			// clock to now rather than treating "never refreshed" as instantly
			// due (the materialize Job just populated the base — it is current).
			now := metav1.NewTime(time.Now())
			ws.Status.LastRefreshedAt = &now
			return apreconcile.Continue()
		}
		if time.Since(ws.Status.LastRefreshedAt.Time) < interval {
			return apreconcile.Continue() // not due
		}
		drv, okd := registry.Get(ws.Spec.Source.Kind)
		if !okd {
			return apreconcile.Continue() // driver missing is caught by validate; don't fail refresh over it
		}
		cmds, err := drv.SyncCommands(SpecToDriver(ws.Spec), baseCheckoutDir)
		if err != nil {
			return apreconcile.Continue() // don't fail the source over a refresh build error
		}
		var job batchv1.Job
		key := client.ObjectKey{Namespace: ws.Namespace, Name: RefreshJobName(&ws)}
		switch e := r.Client.Get(ctx, key, &job); {
		case apierrors.IsNotFound(e):
			j := BuildRefreshJob(&ws, BaseClaimName(&ws), r.MaterializeServiceAccount, r.MaterializeImage, cmds)
			if cerr := r.Client.Create(ctx, j); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
				return apreconcile.FailWith(fmt.Errorf("create refresh Job: %w", cerr))
			}
			conditions.SetTrue(&ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionReady,
				spiceboxv1alpha1.ReasonWorkspaceSourceRefreshing)
			return apreconcile.Continue() // base stays Ready while refreshing
		case e != nil:
			return apreconcile.FailWith(fmt.Errorf("get refresh Job: %w", e))
		}
		if job.Status.Succeeded > 0 {
			now := metav1.NewTime(time.Now())
			ws.Status.LastRefreshedAt = &now
			conditions.SetTrue(&ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionReady,
				spiceboxv1alpha1.ReasonWorkspaceSourceMaterialized)
			if derr := r.Client.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); derr != nil && !apierrors.IsNotFound(derr) {
				log.FromContext(ctx).Info("delete succeeded refresh Job errored", "workspacesource", ws.Name, "err", derr.Error())
			}
			return apreconcile.Continue()
		}
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				log.FromContext(ctx).Info("workspace base refresh Job failed; base is stale but still usable, will retry next interval",
					"workspacesource", ws.Name, "job", job.Name, "reason", c.Reason)
				// Back off: keep the failed Job as the marker and only retry
				// (delete → recreate next reconcile) once a full base.refresh
				// interval has elapsed since it failed, so a persistent failure
				// (revoked creds, permanently diverged branch) does not retry
				// faster than the configured cadence.
				if time.Since(c.LastTransitionTime.Time) >= interval {
					if derr := r.Client.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); derr != nil && !apierrors.IsNotFound(derr) {
						log.FromContext(ctx).Info("failed to delete failed refresh Job", "workspacesource", ws.Name, "err", derr.Error())
					}
				}
				return apreconcile.Continue()
			}
		}
		conditions.SetTrue(&ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkspaceSourceConditionReady,
			spiceboxv1alpha1.ReasonWorkspaceSourceRefreshing)
		return apreconcile.Continue() // refresh Job still running
	}

	finalizeStatus := func(ctx context.Context) apreconcile.Outcome {
		ws.Status.ObservedGeneration = ws.Generation
		return apreconcile.Continue()
	}

	if err := apreconcile.RunPhases(ctx, r.Client, &ws, []apreconcile.Phase{validate, materialize, refresh, finalizeStatus}); err != nil {
		return ctrl.Result{}, err
	}
	requeue := r.interval()
	if iv, ok := parseRefresh(ws.Spec.Base.Refresh); ok && iv < requeue {
		requeue = iv
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.WorkspaceSource{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}
