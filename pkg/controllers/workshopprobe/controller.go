// Package workshopprobe — controller.go: the reconciler that turns a
// WorkshopProbe CR into a run probe. podbuild.go builds the hardened
// pod/NetworkPolicy; prober.go applies them and collects a result; this file
// is the third leg — deciding WHETHER a probe may run at all (the
// workshop:<workshopID>#build SpiceDB tuple, checked against the session
// carried on the workshop namespace's own labels, never the CR's own
// self-asserted fields) and HOW MANY may run concurrently
// (Workshop.spec.limits.maxConcurrentProbes), before ever calling Prober.
package workshopprobe

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// WorkshopBuildChecker answers whether the session bound to a workshop may
// run a probe pod inside it — the SpiceDB workshop:<workshopID>#build check
// (schema: "permission build = session", pkg/authz/spicedb/schema),
// satisfied by *spicedb.Client's CheckWorkshopBuild (pkg/authz/spicedb/
// workshop.go). Declared as a local interface — rather than a
// *spicedb.Client field — for two reasons: this package's tests can inject
// a fake with no live SpiceDB, and the Reconciler never carries a typed
// pointer that could be assigned nil into an interface field (CLAUDE.md's
// typed-nil rule): a genuinely-unwired dependency is a true nil interface,
// caught by the r.Build == nil check below, not a panic hidden by
// controller-runtime's silent panic recovery.
type WorkshopBuildChecker interface {
	CheckWorkshopBuild(ctx context.Context, workshopID, sessNS, sessName string) (bool, error)
}

// requeueCapDelay is how soon a WorkshopProbe blocked by
// Workshop.spec.limits.maxConcurrentProbes is retried. Short enough that a
// probe waiting for a slot doesn't sit idle for long once one frees; long
// enough that a namespace pinned at its cap doesn't hot-loop the reconciler
// while every slot stays busy.
const requeueCapDelay = 15 * time.Second

// Reconciler implements the WorkshopProbe controller: gate on the SpiceDB
// tuple, gate on the concurrency cap, run the probe, record the result.
//
// Build is declared as the WorkshopBuildChecker interface (not
// *spicedb.Client) and is nil-checked at its use site rather than trusted —
// see CLAUDE.md's typed-nil rule and the WorkshopBuildChecker doc above.
// This mirrors pkg/controllers/workshop.Reconciler's own nil-checked Tuples
// field, which has the same shape for the same tuple-writing SpiceDB client.
type Reconciler struct {
	Client client.Client
	// Prober runs one WorkshopProbe's discriminated target and reports what
	// it found. Production wiring injects a *PodProber (prober.go); tests
	// inject a fake so no live pod ever runs there.
	Prober Prober
	// Build is the tuple check gating every probe: no workshop:<id>#build
	// tuple, no probe pod, ever — see WorkshopBuildChecker.
	Build WorkshopBuildChecker
}

// The operator holds create on workshopprobes NOT because this controller
// creates them (the ap-workshop sidecar does, inside a workshop namespace) but
// because the Workshop reconciler provisions a Role granting the workshop SA
// workshopprobes:create, and Kubernetes RBAC escalation prevention refuses to
// let the operator grant a verb it does not itself hold. Guarded by
// TestOperatorCanGrantTheWorkshopRole.
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=workshopprobes,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=workshopprobes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=workshops,verbs=get;list;watch

// Reconcile gates and runs one WorkshopProbe: refuse a probe whose session
// does not hold the workshop:<id>#build tuple, refuse (by requeue, not by
// failing) a probe that would exceed Workshop.spec.limits.maxConcurrentProbes,
// then run it and record what it found. Every error path either returns an
// error (so the reconcile is retried) or persists a status recording it —
// never both silent and unrecorded.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var wp spiceboxv1alpha1.WorkshopProbe
	if err := r.Client.Get(ctx, req.NamespacedName, &wp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get WorkshopProbe %s: %w", req.NamespacedName, err)
	}

	// Idempotent: a terminal WorkshopProbe never re-probes. A probe pod is a
	// one-shot discovery run; re-running it on every reconcile (a status
	// write, a requeue, an informer resync) would burn a pod per event
	// forever instead of reporting the result once.
	if wp.Status.Phase == spiceboxv1alpha1.WorkshopProbePhaseSucceeded || wp.Status.Phase == spiceboxv1alpha1.WorkshopProbePhaseFailed {
		return ctrl.Result{}, nil
	}

	// The session this probe's workshop belongs to is read off the WORKSHOP
	// NAMESPACE's own labels (BuildWorkshopNamespace, pkg/controllers/
	// workshop/rbac.go) — stamped once by the operator's own Workshop
	// controller at provisioning — never off the WorkshopProbe CR's own
	// fields. The CR is created by the (untrusted) sidecar SA; a label it
	// wrote on its own object would be exactly the kind of self-asserted
	// claim a permission check must not trust.
	var ns corev1.Namespace
	if err := r.Client.Get(ctx, types.NamespacedName{Name: wp.Namespace}, &ns); err != nil {
		return ctrl.Result{}, fmt.Errorf("get workshop namespace %s: %w", wp.Namespace, err)
	}
	sessNS := ns.Labels[spiceboxv1alpha1.LabelWorkshopSessionNamespace]
	sessName := ns.Labels[spiceboxv1alpha1.LabelWorkshopSessionName]
	if sessNS == "" || sessName == "" {
		return r.failProbe(ctx, &wp, spiceboxv1alpha1.ReasonWorkshopProbeFailed,
			fmt.Sprintf("workshop namespace %s carries no %s/%s labels — cannot attribute this probe to a session",
				wp.Namespace, spiceboxv1alpha1.LabelWorkshopSessionNamespace, spiceboxv1alpha1.LabelWorkshopSessionName))
	}

	// SECURITY BOUNDARY: wp.Namespace IS the workshop ID
	// (EnsureWorkshopSubjects/CheckWorkshopBuild both key on it, see
	// pkg/authz/spicedb/workshop.go) — a real, cluster-created workshop
	// namespace, never a value the CR's own spec could forge. Both an
	// explicit refusal and a check error deny: an unreachable SpiceDB must
	// never be read as "no tuple means allow", and a nil Build (unwired
	// dependency, see the typed-nil rule) denies the same way.
	if r.Build == nil {
		return r.failProbe(ctx, &wp, spiceboxv1alpha1.ReasonWorkshopProbeDenied,
			"no WorkshopBuildChecker is wired — refusing to run a probe pod with no authorization backend to check it against")
	}
	allowed, err := r.Build.CheckWorkshopBuild(ctx, wp.Namespace, sessNS, sessName)
	if err != nil {
		return r.failProbe(ctx, &wp, spiceboxv1alpha1.ReasonWorkshopProbeDenied,
			fmt.Sprintf("checking workshop:%s#build for session %s/%s: %v", wp.Namespace, sessNS, sessName, err))
	}
	if !allowed {
		return r.failProbe(ctx, &wp, spiceboxv1alpha1.ReasonWorkshopProbeDenied,
			fmt.Sprintf("session %s/%s does not hold workshop:%s#build — refusing to run this probe", sessNS, sessName, wp.Namespace))
	}

	// The concurrency cap: Workshop.spec.limits.maxConcurrentProbes, read
	// from the Workshop CR alongside the AgentSession (sessNS,
	// WorkshopName(sessName)) — not from this probe's own namespace, which
	// holds WorkshopProbes, not the Workshop CR itself.
	var ws spiceboxv1alpha1.Workshop
	wsKey := types.NamespacedName{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}
	if err := r.Client.Get(ctx, wsKey, &ws); err != nil {
		return ctrl.Result{}, fmt.Errorf("get Workshop %s for concurrency limit: %w", wsKey, err)
	}

	var probes spiceboxv1alpha1.WorkshopProbeList
	if err := r.Client.List(ctx, &probes, client.InNamespace(wp.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("list WorkshopProbes in %s: %w", wp.Namespace, err)
	}
	// Count only probes ACTUALLY IN FLIGHT (Phase == Running), never every
	// non-terminal one. A freshly-created probe starts Phase == "" (also
	// non-terminal) — counting that would make every probe in a batch count
	// every OTHER not-yet-admitted probe against the same cap, so N probes
	// created together with cap M<N would all see "N-1 others pending",
	// all requeue, and none would ever transition to Running: a permanent
	// livelock, on exactly the scenario (pending > cap) the cap exists to
	// handle.
	running := 0
	for i := range probes.Items {
		p := &probes.Items[i]
		if p.Name == wp.Name {
			continue // never count this probe against its own slot
		}
		if p.Status.Phase == spiceboxv1alpha1.WorkshopProbePhaseRunning {
			running++
		}
	}
	if running >= int(ws.Spec.Limits.MaxConcurrentProbes) {
		// At or over the cap: requeue without probing and WITHOUT marking
		// Failed — this probe will run once a slot frees, it has not been
		// refused.
		return ctrl.Result{RequeueAfter: requeueCapDelay}, nil
	}

	// A slot is available: mark Running before the probe pod exists, so a
	// crash mid-probe leaves a visibly in-flight (not silently stuck-Pending)
	// WorkshopProbe for the next reconcile to retry.
	wp.Status.Phase = spiceboxv1alpha1.WorkshopProbePhaseRunning
	wp.Status.ObservedGeneration = wp.Generation
	if err := r.Client.Status().Update(ctx, &wp); err != nil {
		return ctrl.Result{}, fmt.Errorf("persist WorkshopProbe %s/%s Running phase: %w", wp.Namespace, wp.Name, err)
	}

	if r.Prober == nil {
		return ctrl.Result{}, fmt.Errorf("WorkshopProbe %s/%s: Reconciler.Prober is not wired", wp.Namespace, wp.Name)
	}
	result, err := r.Prober.Probe(ctx, &wp)
	if err != nil {
		// An infrastructure fault (prober.go's own contract: everything else
		// is a RESULT, not an error) — return it so the reconcile retries.
		// The object stays Running; the retry re-enters this same function
		// and re-probes.
		return ctrl.Result{}, fmt.Errorf("probe WorkshopProbe %s/%s: %w", wp.Namespace, wp.Name, err)
	}

	wp.Status.Tools = result.Tools
	wp.Status.HelpText = result.HelpText
	wp.Status.ResolvedDigest = result.ResolvedDigest
	wp.Status.PodFailure = result.PodFailure
	wp.Status.ObservedGeneration = wp.Generation
	if result.PodFailure != "" {
		wp.Status.Phase = spiceboxv1alpha1.WorkshopProbePhaseFailed
		conditions.SetFalse(&wp, &wp.Status.Conditions, spiceboxv1alpha1.WorkshopProbeConditionProbed,
			spiceboxv1alpha1.ReasonWorkshopProbeFailed, result.PodFailure)
	} else {
		wp.Status.Phase = spiceboxv1alpha1.WorkshopProbePhaseSucceeded
		conditions.SetTrue(&wp, &wp.Status.Conditions, spiceboxv1alpha1.WorkshopProbeConditionProbed,
			spiceboxv1alpha1.ReasonWorkshopProbeSucceeded)
	}
	if err := r.Client.Status().Update(ctx, &wp); err != nil {
		return ctrl.Result{}, fmt.Errorf("persist WorkshopProbe %s/%s result: %w", wp.Namespace, wp.Name, err)
	}
	return ctrl.Result{}, nil
}

// failProbe stamps WorkshopProbeConditionProbed False with reason/message,
// sets phase Failed and podFailure, and persists — ALWAYS returning nil so a
// refused probe (missing labels, a denied tuple check) is a recorded
// terminal RESULT, not a reconcile error that requeues forever retrying a
// decision that will not change. A persist failure itself is logged, never
// swallowed: the next reconcile (this WorkshopProbe is not yet terminal on
// the server) retries both the gate and the persist.
func (r *Reconciler) failProbe(ctx context.Context, wp *spiceboxv1alpha1.WorkshopProbe, reason, message string) (ctrl.Result, error) {
	wp.Status.Phase = spiceboxv1alpha1.WorkshopProbePhaseFailed
	wp.Status.PodFailure = message
	wp.Status.ObservedGeneration = wp.Generation
	conditions.SetFalse(wp, &wp.Status.Conditions, spiceboxv1alpha1.WorkshopProbeConditionProbed, reason, message)
	if err := r.Client.Status().Update(ctx, wp); err != nil {
		log.FromContext(ctx).Info("workshopprobe: persisting a refused/failed probe's status errored; the next reconcile retries",
			"workshopprobe", wp.Namespace+"/"+wp.Name, "reason", reason, "persistErr", err.Error())
	}
	return ctrl.Result{}, nil
}

// SetupWithManager wires the controller. No Owns: the pod, NetworkPolicy and
// (Script-mode) ConfigMap prober.go creates are owner-refed to the
// WorkshopProbe and reaped by GC, not reconciled back onto it — the pod's
// lifecycle is fully driven within one Probe() call, not across reconciles.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.WorkshopProbe{}).
		Complete(r)
}
