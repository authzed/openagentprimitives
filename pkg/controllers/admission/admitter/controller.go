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
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	"github.com/authzed/openagentprimitives/pkg/platform/schedfit"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsessions,verbs=get;list;watch;patch
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
	// MinWarm is how long an Idle session keeps its warm pods before a pass may
	// ask for them back early for a session of its line that does not fit
	// (--admission-min-warm).
	MinWarm time.Duration
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

// isWaiting matches a session the AgentSession reconciler's gate has put in
// line: booting ("" or Pending) and marked Admitted=False/Queued. The marker
// is required, not just the phase: a session's settings are persisted, and
// its phase set to Pending, before the start-approval, identity and
// credential gates park it and before a fatal-settings or image-pin block
// stops it — a session in any of those states never reached the line and must
// never be admitted, or it would reserve capacity it cannot use.
func isWaiting(sess *spiceboxv1alpha1.AgentSession) bool {
	return (sess.Status.Phase == "" || sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhasePending) &&
		admission.IsQueued(sess)
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
	var waiting, holding, preAdmission []*spiceboxv1alpha1.AgentSession
	for i := range sessions {
		sess := &sessions[i]
		switch {
		case isTerminal(sess):
			// Never a holder, even while still Admitted=True with pods: any
			// pods it has placed already reduce node headroom in the snapshot.
		case admission.IsAdmitted(sess):
			holding = append(holding, sess)
		case admission.PreAdmission(sess):
			preAdmission = append(preAdmission, sess)
		case isWaiting(sess):
			waiting = append(waiting, sess)
		}
	}
	if len(waiting) == 0 && (len(preAdmission) == 0 || !r.Enabled) {
		return requeue, nil
	}
	now := r.now()

	if !r.Enabled {
		var errs []error
		for _, sess := range waiting {
			if err := r.admit(ctx, sess, now, spiceboxv1alpha1.ReasonAdmitted, "Session admission is disabled; admitted at once."); err != nil {
				errs = append(errs, err)
			}
		}
		return requeue, errors.Join(errs...)
	}

	var errs []error
	pods, podsKnown := r.listPods(ctx)
	adopted, adoptErrs := r.adoptPreAdmission(ctx, preAdmission, pods, podsKnown, now)
	errs = append(errs, adoptErrs...)
	holding = append(holding, adopted...)
	if len(waiting) == 0 {
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
	in.Caps, in.Waiting = r.capLines(ctx, in.Waiting, byKey)

	pool := reclaimPool{
		sessions: map[types.NamespacedName]*spiceboxv1alpha1.AgentSession{},
		inFlight: map[string][]admission.PlacedPod{},
	}
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
		if podsKnown && isWarmIdle(sess) {
			pool.addWarm(sess, boundPods(sess.Namespace, d, pods))
		}
	}
	if podsKnown {
		r.addDrainingReclaims(ctx, &pool, sessions, pods)
	}

	in.Model = r.snapshot(ctx)
	if !in.Model.Known() {
		logger.Info("admission: cluster capacity unknown; admitting in line order", "waiting", len(in.Waiting))
	}
	in.TotalCPUMilli, in.TotalMemBytes = in.Model.Totals()

	dec := admission.Decide(in)

	for _, key := range dec.Admit {
		if err := r.admit(ctx, byKey[key], now, spiceboxv1alpha1.ReasonAdmitted, "The session's whole pod set fits the cluster."); err != nil {
			errs = append(errs, err)
		}
	}
	for key, pos := range dec.Positions {
		if err := r.writePosition(ctx, byKey[key], pos); err != nil {
			errs = append(errs, err)
		}
	}
	if in.Model.Known() {
		errs = append(errs, r.requestReclaims(ctx, in.Waiting, dec, &pool, in.Model, now)...)
	}
	return requeue, errors.Join(errs...)
}

// capLines reads spec.maxConcurrentSessions from the AgentClass of every line
// with a waiting candidate, keyed by line as Decide expects. A class that does
// not exist has no cap (pricing has already refused its sessions when it
// prices from the class). A class that cannot be read fails closed: that
// line's non-child candidates sit this pass out, since admitting them could
// exceed a cap nobody could check; its children stay, as a cap never blocks
// them.
func (r *Reconciler) capLines(ctx context.Context, waiting []admission.Candidate,
	sessions map[types.NamespacedName]*spiceboxv1alpha1.AgentSession,
) (map[string]int32, []admission.Candidate) {
	caps := map[string]int32{}
	unreadable := map[string]bool{}
	checked := map[string]bool{}
	for _, c := range waiting {
		if checked[c.Line] {
			continue
		}
		checked[c.Line] = true
		sess := sessions[c.Key]
		var ac spiceboxv1alpha1.AgentClass
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: sess.Spec.Class}, &ac); err != nil {
			if !apierrors.IsNotFound(err) {
				log.FromContext(ctx).Info("admission: cannot read the class's session cap; its non-delegated sessions keep waiting this pass",
					"class", c.Line, "err", err.Error())
				unreadable[c.Line] = true
			}
			continue
		}
		if limit := ac.Spec.MaxConcurrentSessions; limit != nil {
			caps[c.Line] = *limit
		}
	}
	if len(unreadable) == 0 {
		return caps, waiting
	}
	kept := waiting[:0:0]
	for _, c := range waiting {
		if c.Child || !unreadable[c.Line] {
			kept = append(kept, c)
		}
	}
	return caps, kept
}

// isWarmIdle matches a holder whose pods could be given back early: Idle,
// channel-attached (the AgentSession reconciler's sleep path, which honors the
// reclaim request, runs only for those), not yet slept, with a known idle
// start for PlanReclaim's minWarm check.
func isWarmIdle(sess *spiceboxv1alpha1.AgentSession) bool {
	return sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseIdle &&
		sess.Spec.InputChannel != nil &&
		sess.Status.SleptAt == nil &&
		sess.Status.LastIdleAt != nil
}

// boundPods is the part of a session's pod set the capacity snapshot counts
// against a node: its demand's pods (matched by name, as unplaced does) that
// are bound and not finished, priced from the live pod spec. A pod already
// marked for deletion is included — the snapshot still counts it until it is
// gone — so it is exactly what a reclaim gives back.
func boundPods(namespace string, d admission.Demand, pods podIndex) []admission.PlacedPod {
	var placed []admission.PlacedPod
	for _, p := range d.Pods {
		pod, ok := pods[types.NamespacedName{Namespace: namespace, Name: p.Name}]
		if !ok || pod.Spec.NodeName == "" ||
			pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		cpu, mem, eph := schedfit.EffectiveRequests(&pod.Spec)
		placed = append(placed, admission.PlacedPod{
			Node:      pod.Spec.NodeName,
			PodDemand: admission.PodDemand{Name: pod.Name, CPUMilli: cpu, MemBytes: mem, EphemeralBytes: eph},
		})
	}
	return placed
}

// reclaimPool is what a pass may plan reclaims from, per line.
//
// inFlight is the capacity reclaims requested on an earlier pass are already
// giving back: the bound pods of a session carrying the reclaim request but
// not yet slept, and of a slept session released for a reclaim whose pods are
// still being torn down. The snapshot still counts those pods, so without
// crediting them every pass between the request and the last pod's deletion —
// and the admitter is woken by each of those status writes and pod events —
// would see the head still not fit and reclaim the next-oldest warm session
// too, cascading through the line for one head.
type reclaimPool struct {
	warm     []admission.WarmIdle
	sessions map[types.NamespacedName]*spiceboxv1alpha1.AgentSession
	inFlight map[string][]admission.PlacedPod
}

// addWarm files a warm Idle holder's bound pods: as in flight when a reclaim
// is already requested (so it is not chosen again), else as a candidate. A
// session with no bound pods has nothing to give back and is skipped.
func (p *reclaimPool) addWarm(sess *spiceboxv1alpha1.AgentSession, placed []admission.PlacedPod) {
	if len(placed) == 0 {
		return
	}
	line := lineOf(sess)
	if _, requested := sess.Annotations[spiceboxv1alpha1.AnnotationReclaimRequested]; requested {
		p.inFlight[line] = append(p.inFlight[line], placed...)
		return
	}
	key := client.ObjectKeyFromObject(sess)
	p.warm = append(p.warm, admission.WarmIdle{Key: key, Line: line, IdleSince: sess.Status.LastIdleAt.Time, Pods: placed})
	p.sessions[key] = sess
}

// isDrainingReclaim matches a session the sleep path has already released for
// a reclaim (slept, Admitted=False/Reclaimed). It is neither a holder nor
// waiting, but its pods may still be terminating.
func isDrainingReclaim(sess *spiceboxv1alpha1.AgentSession) bool {
	if sess.Status.SleptAt == nil {
		return false
	}
	cond := meta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionAdmitted)
	return cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == spiceboxv1alpha1.ReasonAdmissionReclaimed
}

// sessionsWithPods is every session (by namespace/name) that has at least one
// listed pod: its runner, detector or sidecar pods by the session label, its
// sandbox pods by their bundle SpiceboxSession's label — which a pod adopted
// from a SandboxWarmPool also carries, under a pool-minted name.
type sessionsWithPods map[types.NamespacedName]bool

func indexSessionsWithPods(pods podIndex) sessionsWithPods {
	labelled := sessionsWithPods{}
	for _, pod := range pods {
		for _, l := range []string{sessionPodLabel, sandboxPodLabel} {
			if v := pod.Labels[l]; v != "" {
				labelled[types.NamespacedName{Namespace: pod.Namespace, Name: v}] = true
			}
		}
	}
	return labelled
}

// has reports whether sess has a listed pod.
func (l sessionsWithPods) has(sess *spiceboxv1alpha1.AgentSession) bool {
	if l[types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}] {
		return true
	}
	for _, b := range sess.Status.BundleSessions {
		if l[types.NamespacedName{Namespace: sess.Namespace, Name: b.SpiceboxSessionName}] {
			return true
		}
	}
	return false
}

// adoptPreAdmission writes Admitted=True/Adopted on every pre-admission
// session (admission.PreAdmission) that still has a listed pod, and returns
// them so this pass counts them as holders. One with no listed pod holds
// nothing and is left alone; so is every one when the pod list failed, since
// adopting a session with no pods would reserve capacity for nothing.
func (r *Reconciler) adoptPreAdmission(ctx context.Context, sessions []*spiceboxv1alpha1.AgentSession,
	pods podIndex, podsKnown bool, now time.Time,
) ([]*spiceboxv1alpha1.AgentSession, []error) {
	if len(sessions) == 0 || !podsKnown {
		return nil, nil
	}
	withPods := indexSessionsWithPods(pods)
	var adopted []*spiceboxv1alpha1.AgentSession
	var errs []error
	for _, sess := range sessions {
		if !withPods.has(sess) {
			continue
		}
		log.FromContext(ctx).Info("admission: adopting a session whose pods predate admission",
			"session", sess.Namespace+"/"+sess.Name, "class", lineOf(sess), "phase", sess.Status.Phase)
		if err := r.admit(ctx, sess, now, spiceboxv1alpha1.ReasonAdmissionAdopted,
			"The session's pods were created before admission was enabled; adopted as admitted."); err != nil {
			errs = append(errs, err)
		}
		// Counted as a holder either way: its pods are up whether or not the
		// write landed, and the next pass retries the write.
		adopted = append(adopted, sess)
	}
	return adopted, errs
}

// addDrainingReclaims credits the still-listed bound pods of every draining
// reclaim to its line. A slept session stays Admitted=False/Reclaimed for as
// long as it sleeps, so pricing is skipped unless one of its pods (runner pod
// by session label, sandbox pod by its bundle SpiceboxSession's label) is
// still listed — the common case is a long-gone pod set.
func (r *Reconciler) addDrainingReclaims(ctx context.Context, pool *reclaimPool, sessions []spiceboxv1alpha1.AgentSession, pods podIndex) {
	withPods := indexSessionsWithPods(pods)
	for i := range sessions {
		sess := &sessions[i]
		if !isDrainingReclaim(sess) || !withPods.has(sess) {
			continue
		}
		d, err := r.demand(ctx, sess)
		if err != nil {
			log.FromContext(ctx).Info("admission: cannot price a reclaimed session's terminating pods; not crediting them to its line",
				"session", sess.Namespace+"/"+sess.Name, "class", lineOf(sess), "err", err.Error())
			continue
		}
		line := lineOf(sess)
		pool.inFlight[line] = append(pool.inFlight[line], boundPods(sess.Namespace, d, pods)...)
	}
}

// requestReclaims asks for the warm Idle sessions PlanReclaim picks for each
// line's head (its still-waiting position 1) to be slept early. model is the
// one Decide reserved into, so this pass's admissions are respected. Before a
// line plans, its in-flight reclaims are freed on model, so only the shortfall
// they do not already cover is reclaimed. Lines go in name order; each line's
// frees and its head's demand are applied to model before the next line plans,
// so two lines never count the same free space.
func (r *Reconciler) requestReclaims(ctx context.Context, waiting []admission.Candidate, dec admission.Decision,
	pool *reclaimPool, model admission.CapacityModel, now time.Time,
) []error {
	var heads []admission.Candidate
	for _, c := range waiting {
		if pos, ok := dec.Positions[c.Key]; ok && pos.Index == 1 {
			heads = append(heads, c)
		}
	}
	slices.SortFunc(heads, func(a, b admission.Candidate) int { return strings.Compare(a.Line, b.Line) })

	pods := make(map[types.NamespacedName][]admission.PlacedPod, len(pool.warm))
	for _, w := range pool.warm {
		pods[w.Key] = w.Pods
	}
	var errs []error
	for _, head := range heads {
		inFlight := pool.inFlight[head.Line]
		model.Free(inFlight)
		plan := admission.PlanReclaim(head, pool.warm, model, now, r.MinWarm)
		for _, key := range plan {
			model.Free(pods[key])
			if err := r.requestReclaim(ctx, pool.sessions[key], head.Key, now); err != nil {
				errs = append(errs, err)
			}
		}
		if len(plan) == 0 && len(inFlight) == 0 {
			continue
		}
		// Hold the freed space for this head so a later line cannot plan on it.
		// With a plan, PlanReclaim proved the head fits; a failure means that
		// invariant broke. With only in-flight frees it may simply not fit yet.
		if !model.Reserve(head.Demand) && len(plan) > 0 {
			log.FromContext(ctx).Info("admission: head does not fit after its planned reclaims were freed; the plan's fit check and the model disagree",
				"class", head.Line, "head", head.Key.String(), "reclaims", len(plan))
		}
	}
	return errs
}

// requestReclaim stamps AnnotationReclaimRequested on sess with a
// metadata-only merge patch, unless a request is already there: a later pass
// re-planning the same reclaim must leave the first request's time alone.
func (r *Reconciler) requestReclaim(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, head types.NamespacedName, now time.Time) error {
	if _, ok := sess.Annotations[spiceboxv1alpha1.AnnotationReclaimRequested]; ok {
		return nil
	}
	logger := log.FromContext(ctx)
	logger.Info("admission: requesting early reclaim of a warm idle session for its line's waiting head",
		"session", sess.Namespace+"/"+sess.Name, "class", lineOf(sess), "head", head.String())
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`,
		spiceboxv1alpha1.AnnotationReclaimRequested, now.UTC().Format(time.RFC3339))
	target := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: sess.Name, Namespace: sess.Namespace}}
	if err := r.Client.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch)); err != nil {
		logger.Info("admission: writing the reclaim request failed; the next pass retries",
			"session", sess.Namespace+"/"+sess.Name, "class", lineOf(sess), "err", err.Error())
		return fmt.Errorf("request reclaim of %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	return nil
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

// admit writes Admitted=True with reason at now and clears the session's line
// position.
func (r *Reconciler) admit(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, now time.Time, reason, message string) error {
	original := sess.DeepCopy()
	meta.SetStatusCondition(&sess.Status.Conditions, metav1.Condition{
		Type:               spiceboxv1alpha1.AgentSessionConditionAdmitted,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
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
