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
	"sync/atomic"
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
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsessions,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsessions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=subagentrequests,verbs=get;list;watch
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
	// capacityUnknown is whether the previous pass found capacity unknown, so
	// the reason is logged once on a known→unknown transition rather than on
	// every pass (the admitter is woken by every session and pod event).
	capacityUnknown atomic.Bool
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
	var errs []error
	for i := range sessions {
		sess := &sessions[i]
		if !isWaiting(sess) {
			if err := r.clearPosition(ctx, sess); err != nil {
				errs = append(errs, err)
			}
		}
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
		return requeue, errors.Join(errs...)
	}
	now := r.now()

	if !r.Enabled {
		for _, sess := range waiting {
			if err := r.admit(ctx, sess, now, spiceboxv1alpha1.ReasonAdmitted, "Session admission is disabled; admitted at once."); err != nil {
				errs = append(errs, err)
			}
		}
		return requeue, errors.Join(errs...)
	}

	delegated, err := r.listDelegations(ctx)
	if err != nil {
		// Defer the whole pass rather than decide on a view known to be wrong:
		// with no delegation index every genuine child would read as a
		// non-child, losing its cap exemption and, if warm idle, becoming a
		// cap-reclaim candidate during an API blip.
		return requeue, errors.Join(append(errs, err)...)
	}
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
			Child:    delegated.isChild(ctx, sess),
			Demand:   d,
		})
	}
	in.Caps, in.Waiting = r.capLines(ctx, in.Waiting, byKey)

	pool := reclaimPool{
		sessions:  map[types.NamespacedName]*spiceboxv1alpha1.AgentSession{},
		inFlight:  map[string][]admission.PlacedPod{},
		requested: map[string]int{},
	}
	for _, sess := range holding {
		d, err := r.demand(ctx, sess)
		if err != nil {
			// Still a holder: it counts toward its line's cap. With no
			// demand it adds nothing to the line's share, and its unplaced
			// pods go unreserved (its placed ones are in the snapshot), which
			// is the accepted cost of an unpriceable holder.
			logger.Info("admission: cannot price an admitted session's pod set; counting it with no unplaced demand",
				"session", sess.Namespace+"/"+sess.Name, "class", lineOf(sess), "err", err.Error())
		}
		child := delegated.isChild(ctx, sess)
		in.Holders = append(in.Holders, admission.Holder{
			Key:      client.ObjectKeyFromObject(sess),
			Line:     lineOf(sess),
			Child:    child,
			Demand:   d,
			Unplaced: unplaced(sess.Namespace, d, pods, podsKnown),
		})
		if podsKnown && isWarmIdle(sess) {
			pool.addWarm(sess, child, boundPods(sess.Namespace, d, pods))
		}
	}
	if podsKnown {
		r.addDrainingReclaims(ctx, &pool, sessions, pods)
	}

	in.Model = r.snapshot(ctx, len(in.Waiting))
	in.TotalCPUMilli, in.TotalMemBytes = in.Model.Totals()

	dec := admission.Decide(in)

	// Under an unknown model nothing proved the set fits, so the reconciler
	// must not read a later stall as the model being wrong: the reason tells
	// its misfit release to leave the session to the pod-level hold.
	reason, message := spiceboxv1alpha1.ReasonAdmitted, "The session's whole pod set fits the cluster."
	if !in.Model.Known() {
		reason, message = spiceboxv1alpha1.ReasonAdmittedUnknownCapacity,
			"Cluster capacity is unknown to the operator; admitted in line order without a fit check."
	}
	for _, key := range dec.Admit {
		if err := r.admit(ctx, byKey[key], now, reason, message); err != nil {
			errs = append(errs, err)
		}
	}
	for key, pos := range dec.Positions {
		if err := r.writePosition(ctx, byKey[key], pos); err != nil {
			errs = append(errs, err)
		}
	}
	// Capacity reclaims plan only on a known model (PlanReclaim returns nil
	// otherwise); cap reclaims need no capacity data, so they run either way.
	errs = append(errs, r.requestReclaims(ctx, in.Waiting, dec, &pool, in.Model, now)...)
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
// against a node: its demand's pods (matched by podIndex.lookup, as unplaced
// does) that are bound and not finished, priced from the live pod spec. A pod
// already marked for deletion is included — the snapshot still counts it
// until it is gone — so it is exactly what a reclaim gives back.
func boundPods(namespace string, d admission.Demand, pods podIndex) []admission.PlacedPod {
	var placed []admission.PlacedPod
	for _, p := range d.Pods {
		pod := pods.lookup(namespace, p)
		if pod == nil || !isBound(pod) {
			continue
		}
		placed = append(placed, admission.PlacedPod{
			Node:      pod.Spec.NodeName,
			PodDemand: admission.PodDemandFor(pod.Name, &pod.Spec),
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
//
// requested counts, per line, the non-child holders already carrying a
// reclaim request and not yet slept: each is already giving a cap slot back,
// so a cap-blocked head waits for it rather than asking for another.
type reclaimPool struct {
	warm      []admission.WarmIdle
	sessions  map[types.NamespacedName]*spiceboxv1alpha1.AgentSession
	inFlight  map[string][]admission.PlacedPod
	requested map[string]int
}

// addWarm files a warm Idle holder's bound pods: as in flight when a reclaim
// is already requested (so it is not chosen again), else as a candidate. A
// session with no bound pods has nothing to give back and is skipped. child is
// the holder's proven delegation (delegations.isChild).
func (p *reclaimPool) addWarm(sess *spiceboxv1alpha1.AgentSession, child bool, placed []admission.PlacedPod) {
	line := lineOf(sess)
	if _, requested := sess.Annotations[spiceboxv1alpha1.AnnotationReclaimRequested]; requested {
		p.inFlight[line] = append(p.inFlight[line], placed...)
		if !child {
			p.requested[line]++
		}
		return
	}
	if len(placed) == 0 {
		return
	}
	key := client.ObjectKeyFromObject(sess)
	p.warm = append(p.warm, admission.WarmIdle{Key: key, Line: line, IdleSince: sess.Status.LastIdleAt.Time, Pods: placed, Child: child})
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
	for _, pod := range pods.byName {
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

// requestReclaims asks for the warm Idle sessions picked for each line's head
// (its still-waiting position 1) to be slept early. model is the one Decide
// reserved into, so this pass's admissions are respected. Lines go in name
// order; each line's frees and its head's demand are applied to model before
// the next line plans, so two lines never count the same free space.
//
// A head whose line is at its cap (dec.CapBlocked) needs a slot, not space:
// PlanCapReclaim picks one warm non-child holder of the line, unless one
// already requested is still in flight (it is already giving a slot back).
// Any other head gets PlanReclaim's capacity plan; before it plans, the
// line's in-flight reclaims are freed on model, so only the shortfall they do
// not already cover is reclaimed.
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
		var plan []types.NamespacedName
		if dec.CapBlocked[head.Line] {
			if key, ok := admission.PlanCapReclaim(head, pool.warm, now, r.MinWarm); ok && pool.requested[head.Line] == 0 {
				plan = []types.NamespacedName{key}
			}
		} else {
			plan = admission.PlanReclaim(head, pool.warm, model, now, r.MinWarm)
		}
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
		// With a capacity plan, PlanReclaim proved the head fits; a failure
		// means that invariant broke. With only in-flight frees, or a cap
		// reclaim (which frees a slot, not necessarily enough space), it may
		// simply not fit yet.
		if !model.Reserve(head.Demand) && len(plan) > 0 && !dec.CapBlocked[head.Line] {
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

// delegation is what a SubagentRequest's status records about the child it
// created: the request (its UID, which the child's controller owner reference
// names) and the parent it delegated for.
type delegation struct {
	request types.UID
	parent  spiceboxv1alpha1.NamespacedRef
}

// delegations indexes, by child session, every delegation the SubagentRequest
// controller has recorded.
//
// A session's own spec.parent is not proof that it is a delegated child:
// anyone who may create an AgentSession can set it, and a child skips its
// class's cap and goes first in its line. A request's status.childRef is
// written only by the operator (no namespace principal is granted
// subagentrequests/status), and only once it has created that child itself,
// so the exemption is read from there.
type delegations map[types.NamespacedName]delegation

// listDelegations indexes every SubagentRequest that names its child. A list
// error is returned, and the caller defers the whole pass on it: nothing is
// decided, admitted or reclaimed until a later pass can read the delegation
// records, since without them no session could be proven a delegated child.
func (r *Reconciler) listDelegations(ctx context.Context) (delegations, error) {
	var list spiceboxv1alpha1.SubagentRequestList
	if err := r.Client.List(ctx, &list); err != nil {
		log.FromContext(ctx).Info("admission: list SubagentRequests failed; deferring this admission pass",
			"err", err.Error())
		return delegations{}, fmt.Errorf("admission pass: list SubagentRequests: %w", err)
	}
	out := make(delegations, len(list.Items))
	for i := range list.Items {
		sr := &list.Items[i]
		if ref := sr.Status.ChildRef; ref != nil {
			out[types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}] = delegation{request: sr.UID, parent: sr.Spec.Parent}
		}
	}
	return out, nil
}

// isChild reports whether sess is a delegated child: it names a parent, and a
// SubagentRequest that owns it records creating it for that same parent. A
// session claiming a parent with no such record is treated as non-delegated
// and logged. That includes a genuine child in the moment between its
// creation and its request's status write; it waits as an ordinary session
// until the write lands, which wakes the pass (SetupWithManager).
func (d delegations) isChild(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) bool {
	claimed := sess.Spec.Parent
	if claimed == nil {
		return false
	}
	if got, ok := d[client.ObjectKeyFromObject(sess)]; ok && got.parent == *claimed {
		if owner := metav1.GetControllerOf(sess); owner != nil && owner.UID == got.request {
			return true
		}
	}
	log.FromContext(ctx).Info("admission: session names a parent but no SubagentRequest records delegating it; treating it as a non-delegated session",
		"session", sess.Namespace+"/"+sess.Name, "class", lineOf(sess), "claimedParent", claimed.Namespace+"/"+claimed.Name)
	return false
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
// source, when the snapshot fails, or when the operator is namespace-scoped:
// missing data must not block sessions. The reason is logged once per
// known→unknown transition (see capacityUnknown).
//
// A namespace-scoped operator (a watch set) sees only its own namespaces'
// pods, so node headroom computed from them overstates what is free by every
// pod in every other namespace. An optimistic model admits sets the scheduler
// cannot place, and the misfit release then reaps and re-admits them in a
// loop; an unknown one admits in line order and leaves stalls to the
// pod-level hold.
func (r *Reconciler) snapshot(ctx context.Context, waiting int) admission.CapacityModel {
	unknown := func(why string, kv ...any) admission.CapacityModel {
		if !r.capacityUnknown.Swap(true) {
			log.FromContext(ctx).Info("admission: cluster capacity unknown; admitting in line order without a fit check",
				append([]any{"reason", why, "waiting", waiting}, kv...)...)
		}
		return admission.NewNodeModel(nil, nil)
	}
	if len(r.Namespaces) > 0 {
		return unknown("the operator is namespace-scoped (--watch-namespaces) and cannot see other namespaces' pods",
			"namespaces", r.Namespaces)
	}
	if r.Capacity == nil {
		return unknown("no capacity source is configured")
	}
	m, err := r.Capacity.Snapshot(ctx)
	if err != nil {
		return unknown("the capacity snapshot failed", "err", err.Error())
	}
	if !m.Known() {
		return unknown("no node reports allocatable resources")
	}
	if r.capacityUnknown.Swap(false) {
		log.FromContext(ctx).Info("admission: cluster capacity known again; admitting by fit")
	}
	return m
}

// podIndex is the cluster's pods, looked up for a session's demand entries by
// role: a sandbox pod by its bundle SpiceboxSession's label (sandboxPodLabel),
// every other pod by name.
//
// The split is because a pod adopted from a SandboxWarmPool keeps the
// pool-minted name it was created under; tryAdopt injects the
// SpiceboxSession label onto it, and that label is the only stable link. The
// runner, detector and separate-pod sidecar pods are always created by this
// operator under the deterministic name their PodDemand carries.
type podIndex struct {
	byName    map[types.NamespacedName]*corev1.Pod
	bySandbox map[types.NamespacedName][]*corev1.Pod
}

func newPodIndex(pods []corev1.Pod) podIndex {
	idx := podIndex{
		byName:    make(map[types.NamespacedName]*corev1.Pod, len(pods)),
		bySandbox: map[types.NamespacedName][]*corev1.Pod{},
	}
	for i := range pods {
		p := &pods[i]
		idx.byName[client.ObjectKeyFromObject(p)] = p
		if v := p.Labels[sandboxPodLabel]; v != "" {
			k := types.NamespacedName{Namespace: p.Namespace, Name: v}
			idx.bySandbox[k] = append(idx.bySandbox[k], p)
		}
	}
	return idx
}

// lookup finds the pod a demand entry in namespace describes, or nil. Of
// several pods labelled for one sandbox (a replacement beside a terminating
// predecessor), a bound, unfinished one is preferred: that is the pod the
// snapshot is counting against a node.
func (idx podIndex) lookup(namespace string, p admission.PodDemand) *corev1.Pod {
	if p.SandboxSession == "" {
		return idx.byName[types.NamespacedName{Namespace: namespace, Name: p.Name}]
	}
	cands := idx.bySandbox[types.NamespacedName{Namespace: namespace, Name: p.SandboxSession}]
	for _, pod := range cands {
		if isBound(pod) {
			return pod
		}
	}
	if len(cands) > 0 {
		return cands[0]
	}
	return nil
}

// isBound reports whether the snapshot counts pod against a node: bound and
// not finished.
func isBound(pod *corev1.Pod) bool {
	return pod.Spec.NodeName != "" && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// listPods indexes every pod for the unplaced-demand check. A list error is
// logged and reported as not known, so every holder pod counts as unplaced.
func (r *Reconciler) listPods(ctx context.Context) (podIndex, bool) {
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods); err != nil {
		log.FromContext(ctx).Info("admission: list pods failed; counting every admitted session's pods as unplaced", "err", err.Error())
		return podIndex{}, false
	}
	return newPodIndex(pods.Items), true
}

// unplaced is the part of a holder's demand the capacity snapshot does not
// already account for, each entry matched to its pod in the session's
// namespace by podIndex.lookup:
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
			if pod := pods.lookup(namespace, p); pod != nil && pod.Spec.NodeName != "" {
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

// clearPosition removes a line position left on a session that is no longer
// waiting — it went terminal, was parked, or was admitted by a write that did
// not clear it — so its thread never shows a place in line it does not have.
// A no-op, with no write, when there is nothing to clear.
func (r *Reconciler) clearPosition(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	if sess.Status.Admission == nil {
		return nil
	}
	original := sess.DeepCopy()
	sess.Status.Admission = nil
	if err := agentstatus.WriteOwned(ctx, r.Client, sess, original, spiceboxv1alpha1.OwnerOperator); err != nil {
		log.FromContext(ctx).Info("admission: clearing a stale line position failed; the next pass retries",
			"session", sess.Namespace+"/"+sess.Name, "class", lineOf(sess), "phase", sess.Status.Phase, "err", err.Error())
		return fmt.Errorf("clear admission position for %s/%s: %w", sess.Namespace, sess.Name, err)
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

// SetupWithManager maps every AgentSession event, every SubagentRequest event
// (its status.childRef write is what proves a waiting session a delegated
// child), and every event on a pod carrying a session label, to the single
// Key.
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
		Watches(&spiceboxv1alpha1.SubagentRequest{}, toKey).
		Watches(&corev1.Pod{}, toKey, builder.WithPredicates(sessionPod)).
		Complete(r)
}
