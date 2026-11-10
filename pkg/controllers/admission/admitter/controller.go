// Package admitter is the one controller that decides which booting
// AgentSessions may create their pods. Each pass rebuilds every admission line
// from the AgentSession objects themselves, prices each session's whole pod
// set, and hands the result to admission.Decide; the AgentSession reconciler
// then creates pods only for a session it marked Admitted=True.
//
// The pass keeps no queue state in memory: the line is derived from
// AgentSession status on every pass, so an operator restart or leader change
// resumes exactly where the last leader stopped. It runs on the leader only
// (controller-runtime's default for a controller; nothing here overrides it),
// so two operators never admit against the same free space.
package admitter

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/admission"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentstatus"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsessions,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsessions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Key is the single request every event maps to: a pass always decides every
// line at once, so there is nothing per-object to reconcile.
var Key = types.NamespacedName{Namespace: "admission", Name: "cluster"}

// resyncInterval re-runs the pass with no event, so capacity freed by pods the
// admitter does not watch (other workloads finishing, nodes joining) is seen.
const resyncInterval = 15 * time.Second

// Session pods carry one of two labels: the runner, detector and sidecar pods
// the AgentSession reconciler builds carry the session's name under
// sessionPodLabel; sandbox pods are built for a bundle SpiceboxSession
// (podspec.Build) and carry that SpiceboxSession's name under
// sandboxPodLabel. A pod event with either can change a holder's unplaced
// demand.
const (
	sessionPodLabel = "agentprimitives.authzed.com/agentsession"
	sandboxPodLabel = "agentprimitives.authzed.com/session"
)

// Reconciler runs the admission pass.
type Reconciler struct {
	Client client.Client
	// APIReader lists AgentSessions uncached. The pass must see its own last
	// Admitted=True writes: a cache that lags them would show an admitted
	// session as still waiting, free its capacity, and let the pass admit a
	// second session into the same space. Nil falls back to Client.
	APIReader client.Reader
	// Namespaces is the operator's watch set (--watch-namespaces). When set,
	// sessions are listed one namespace at a time: the APIReader list is
	// uncached, so a cluster-wide one would 403 under the per-namespace RBAC a
	// watch set exists for, and would reach sessions in namespaces this
	// operator does not own. Empty means cluster-wide.
	Namespaces []string
	// Capacity snapshots what the cluster can still place. Nil, or a snapshot
	// error, means unknown capacity: every waiting session fits.
	Capacity admission.CapacitySource
	// Enabled false admits every waiting session at once, with no capacity
	// check (--session-admission=false).
	Enabled bool
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Demand prices a session's whole pod set. Nil means the default: get the
	// session's AgentClass and call agentsession.DemandFor. A test seam, for
	// a harness where no AgentSession reconciler has resolved settings.
	Demand func(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (admission.Demand, error)
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) sessionReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// lineOf is the admission line a session draws capacity from: its AgentClass,
// which lives in the session's namespace.
func lineOf(sess *spiceboxv1alpha1.AgentSession) string {
	return sess.Namespace + "/" + sess.Spec.Class
}

func isTerminal(sess *spiceboxv1alpha1.AgentSession) bool {
	return sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseSucceeded ||
		sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseFailed
}

// isWaiting matches the AgentSession reconciler's gate: a booting session
// ("" or Pending) that is not admitted. A session parked at a pre-start gate
// has another phase and is not in line yet.
func isWaiting(sess *spiceboxv1alpha1.AgentSession) bool {
	return (sess.Status.Phase == "" || sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhasePending) &&
		!admission.IsAdmitted(sess)
}

func (r *Reconciler) demand(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (admission.Demand, error) {
	if r.Demand != nil {
		return r.Demand(ctx, sess)
	}
	var ac spiceboxv1alpha1.AgentClass
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: sess.Spec.Class}, &ac); err != nil {
		return admission.Demand{}, fmt.Errorf("get AgentClass %s: %w", lineOf(sess), err)
	}
	return agentsession.DemandFor(ctx, r.Client, sess, &ac)
}

// Reconcile runs one admission pass over every line.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	requeue := ctrl.Result{RequeueAfter: resyncInterval}

	sessions, err := r.listSessions(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	var waiting, holding []*spiceboxv1alpha1.AgentSession
	for i := range sessions {
		sess := &sessions[i]
		switch {
		case isTerminal(sess):
			// Never a holder, even while still Admitted=True with pods: any
			// pods it has placed already reduce node headroom in the snapshot.
		case admission.IsAdmitted(sess):
			holding = append(holding, sess)
		case isWaiting(sess):
			waiting = append(waiting, sess)
		}
	}
	if len(waiting) == 0 {
		return requeue, nil
	}
	now := r.now()

	if !r.Enabled {
		var errs []error
		for _, sess := range waiting {
			if err := r.admit(ctx, sess, now, "Session admission is disabled; admitted at once."); err != nil {
				errs = append(errs, err)
			}
		}
		return requeue, errors.Join(errs...)
	}

	in := admission.Input{}
	byKey := make(map[types.NamespacedName]*spiceboxv1alpha1.AgentSession, len(waiting))
	for _, sess := range waiting {
		d, err := r.demand(ctx, sess)
		if err != nil {
			logger.Info("admission: cannot price session's pod set; it keeps waiting",
				"session", sess.Namespace+"/"+sess.Name, "class", lineOf(sess), "err", err.Error())
			continue
		}
		key := client.ObjectKeyFromObject(sess)
		byKey[key] = sess
		in.Waiting = append(in.Waiting, admission.Candidate{
			Key:      key,
			Line:     lineOf(sess),
			JoinedAt: admission.JoinedAt(sess),
			Child:    sess.Spec.Parent != nil,
			Demand:   d,
		})
	}

	pods, podsKnown := r.listPods(ctx)
	for _, sess := range holding {
		d, err := r.demand(ctx, sess)
		if err != nil {
			// Still a holder: it counts toward its line's share and cap. Its
			// placed pods are in the snapshot; only its unplaced ones go
			// unreserved, which is the accepted cost of an unpriceable holder.
			logger.Info("admission: cannot price an admitted session's pod set; counting it with no unplaced demand",
				"session", sess.Namespace+"/"+sess.Name, "class", lineOf(sess), "err", err.Error())
		}
		in.Holders = append(in.Holders, admission.Holder{
			Key:      client.ObjectKeyFromObject(sess),
			Line:     lineOf(sess),
			Child:    sess.Spec.Parent != nil,
			Demand:   d,
			Unplaced: unplaced(sess.Namespace, d, pods, podsKnown),
		})
	}

	in.Model = r.snapshot(ctx)
	if !in.Model.Known() {
		logger.Info("admission: cluster capacity unknown; admitting in line order", "waiting", len(in.Waiting))
	}
	in.TotalCPUMilli, in.TotalMemBytes = in.Model.Totals()

	dec := admission.Decide(in)

	var errs []error
	for _, key := range dec.Admit {
		if err := r.admit(ctx, byKey[key], now, "The session's whole pod set fits the cluster."); err != nil {
			errs = append(errs, err)
		}
	}
	for key, pos := range dec.Positions {
		if err := r.writePosition(ctx, byKey[key], pos); err != nil {
			errs = append(errs, err)
		}
	}
	return requeue, errors.Join(errs...)
}

// listSessions lists every AgentSession the operator owns: cluster-wide with
// no watch set, else one namespaced list per watched namespace. Any failed
// list fails the pass (logged by the caller's error return) rather than
// deciding lines from a partial view.
func (r *Reconciler) listSessions(ctx context.Context) ([]spiceboxv1alpha1.AgentSession, error) {
	if len(r.Namespaces) == 0 {
		var list spiceboxv1alpha1.AgentSessionList
		if err := r.sessionReader().List(ctx, &list); err != nil {
			return nil, fmt.Errorf("admission pass: list AgentSessions: %w", err)
		}
		return list.Items, nil
	}
	var out []spiceboxv1alpha1.AgentSession
	for _, ns := range r.Namespaces {
		var list spiceboxv1alpha1.AgentSessionList
		if err := r.sessionReader().List(ctx, &list, client.InNamespace(ns)); err != nil {
			return nil, fmt.Errorf("admission pass: list AgentSessions in namespace %q: %w", ns, err)
		}
		out = append(out, list.Items...)
	}
	return out, nil
}

// snapshot returns the capacity model, or an unknown one when there is no
// source or the snapshot fails (logged): missing data must not block sessions.
func (r *Reconciler) snapshot(ctx context.Context) admission.CapacityModel {
	if r.Capacity == nil {
		return admission.NewNodeModel(nil, nil)
	}
	m, err := r.Capacity.Snapshot(ctx)
	if err != nil {
		log.FromContext(ctx).Info("admission: capacity snapshot failed; treating capacity as unknown", "err", err.Error())
		return admission.NewNodeModel(nil, nil)
	}
	return m
}

// podIndex is the cluster's pods by namespace/name.
type podIndex map[types.NamespacedName]*corev1.Pod

// listPods indexes every pod for the unplaced-demand check. A list error is
// logged and reported as not known, so every holder pod counts as unplaced.
func (r *Reconciler) listPods(ctx context.Context) (podIndex, bool) {
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods); err != nil {
		log.FromContext(ctx).Info("admission: list pods failed; counting every admitted session's pods as unplaced", "err", err.Error())
		return nil, false
	}
	idx := make(podIndex, len(pods.Items))
	for i := range pods.Items {
		idx[client.ObjectKeyFromObject(&pods.Items[i])] = &pods.Items[i]
	}
	return idx, true
}

// unplaced is the part of a holder's demand the capacity snapshot does not
// already account for, matched to pods by name in the session's namespace
// (DemandFor names each PodDemand after the pod its builder creates):
//
//   - a pod bound to a node (spec.nodeName set) is in the snapshot: placed;
//   - a pod that exists but is not yet bound, or does not exist yet, is not:
//     unplaced, so Decide reserves it before admitting anyone else.
//
// When the pod list failed (known=false), every pod is unplaced: reserving too
// much under-admits for a pass, never over-admits.
func unplaced(namespace string, d admission.Demand, pods podIndex, known bool) admission.Demand {
	var out admission.Demand
	for _, p := range d.Pods {
		if known {
			if pod, ok := pods[types.NamespacedName{Namespace: namespace, Name: p.Name}]; ok && pod.Spec.NodeName != "" {
				continue
			}
		}
		out.Pods = append(out.Pods, p)
	}
	return out
}

// admit writes Admitted=True at now and clears the session's line position.
func (r *Reconciler) admit(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, now time.Time, message string) error {
	original := sess.DeepCopy()
	meta.SetStatusCondition(&sess.Status.Conditions, metav1.Condition{
		Type:               spiceboxv1alpha1.AgentSessionConditionAdmitted,
		Status:             metav1.ConditionTrue,
		Reason:             spiceboxv1alpha1.ReasonAdmitted,
		Message:            message,
		ObservedGeneration: sess.Generation,
		LastTransitionTime: metav1.NewTime(now),
	})
	sess.Status.Admission = nil
	if err := agentstatus.WriteOwned(ctx, r.Client, sess, original, spiceboxv1alpha1.OwnerOperator); err != nil {
		log.FromContext(ctx).Info("admission: writing Admitted=True failed; the next pass retries",
			"session", sess.Namespace+"/"+sess.Name, "class", lineOf(sess), "err", err.Error())
		return fmt.Errorf("admit %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	return nil
}

// writePosition records a waiting session's place in line, only when its
// position, line length or join time changed.
func (r *Reconciler) writePosition(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, pos admission.Position) error {
	want := &spiceboxv1alpha1.AdmissionStatus{
		Position:   pos.Index,
		LineLength: pos.LineLength,
		// The apiserver stores times to the second; compare at that precision
		// so an unchanged join time never reads as a change.
		JoinedAt: metav1.NewTime(admission.JoinedAt(sess)).Rfc3339Copy(),
	}
	if cur := sess.Status.Admission; cur != nil && cur.Position == want.Position &&
		cur.LineLength == want.LineLength && cur.JoinedAt.Time.Equal(want.JoinedAt.Time) {
		return nil
	}
	original := sess.DeepCopy()
	sess.Status.Admission = want
	if err := agentstatus.WriteOwned(ctx, r.Client, sess, original, spiceboxv1alpha1.OwnerOperator); err != nil {
		log.FromContext(ctx).Info("admission: writing line position failed; the next pass retries",
			"session", sess.Namespace+"/"+sess.Name, "class", lineOf(sess), "err", err.Error())
		return fmt.Errorf("write admission position for %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	return nil
}

// SetupWithManager maps every AgentSession event, and every event on a pod
// carrying a session label, to the single Key.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	toKey := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: Key}}
	})
	sessionPod := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		labels := obj.GetLabels()
		_, runner := labels[sessionPodLabel]
		_, sandbox := labels[sandboxPodLabel]
		return runner || sandbox
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("admission").
		Watches(&spiceboxv1alpha1.AgentSession{}, toKey).
		Watches(&corev1.Pod{}, toKey, builder.WithPredicates(sessionPod)).
		Complete(r)
}
