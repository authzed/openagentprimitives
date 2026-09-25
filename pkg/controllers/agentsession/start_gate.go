package agentsession

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentstatus"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// StartChecker answers agentclass:<ns>/<name>#<permission>@user:<canonicalID>,
// FullyConsistent. Satisfied by *spicedb.Client (CheckAgentClassStart).
type StartChecker interface {
	CheckAgentClassStart(ctx context.Context, ns, name, permission string, canonicalID identity.CanonicalUserID) (bool, error)
}

// startRefusedBody is what the person who tried to start the session reads.
// Plain words only: they can neither see nor act on a relation name.
const startRefusedBody = "This agent only runs for the people on its list, and you're not on it yet. Ask an administrator of this agent to add you."

// classMissingRestartBody is what the person who asked to continue a session
// whose agent has since been deleted reads. Same plain-words rule.
const classMissingRestartBody = "This agent no longer exists, so this conversation can't be continued."

// startersUnconfirmedDetail is the operator-facing reason a gate could not be
// evaluated: it names the missing precondition, not the person.
const startersUnconfirmedDetail = "the class's starter set has not been confirmed in the authorization store, so the start gate cannot be evaluated yet"

// EnforceStartGate is the one place every entry path — channel, browser,
// delegation, restart, fork — is checked against the class's allowlist. It
// runs after the class resolves and before anything that costs a pod.
//
// Decided ONCE per session: the verdict is a status condition, so a restarted
// operator, or any later reconcile, does not re-ask — and a running session
// whose class later changes its list is not killed mid-turn (the Interact
// gate governs who may keep talking to it). Fail-closed on every branch that
// cannot answer: no human starter, no checker wired. Indeterminate (the check
// RPC errored) is NOT a refusal: it is recorded and requeued.
//
// halted reports that the caller must return (res, err) now.
func (r *Reconciler) EnforceStartGate(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ac *spiceboxv1alpha1.AgentClass) (res ctrl.Result, halted bool, err error) {
	perm := ac.StartGatePermission()
	if perm == "" {
		return ctrl.Result{}, false, nil
	}
	if c := meta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionStarterAllowed); c != nil && c.Status == metav1.ConditionTrue {
		return ctrl.Result{}, false, nil
	}
	if isTerminalPhase(sess.Status.Phase) || podAlreadyStarted(sess) {
		return ctrl.Result{}, false, nil
	}
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name, "agentclass", ac.Namespace+"/"+ac.Name, "permission", perm)

	canonical := spiceboxv1alpha1.StartedByCanonical(sess)
	if canonical.IsZero() {
		logger.Info("start gate: refusing a session with no human starter on an allowlisted class")
		return r.refuseStart(ctx, sess, identity.CanonicalUserID{}, "the class allows only listed starters and this session has no human starter")
	}
	if r.StartChecker == nil {
		logger.Info("start gate: no StartChecker wired; refusing (fail closed)")
		return r.refuseStart(ctx, sess, canonical, "the start check is not configured on this cluster, so an allowlisted class cannot admit anyone")
	}
	// The set the check resolves against must be CONFIRMED before a "no" from it
	// means anything about the person. An absent or False StartersLinked says the
	// class's own reconciler has not made agentclass#starter equal
	// spec.allowedStarters — the write may be queued behind a transient SpiceDB
	// failure, or have been refused outright — so the check would be asking about
	// a list the store was never told, and every listed starter would be told
	// they are not on it. INDETERMINATE, exactly like an RPC error: recorded and
	// requeued, never a refusal.
	//
	// Below the nil-checker guard on purpose. No checker at all is a permanent
	// configuration verdict no requeue can change, and stays a fail-closed
	// refusal; an unconfirmed link is a race a requeue routinely wins.
	if !meta.IsStatusConditionTrue(ac.Status.Conditions, spiceboxv1alpha1.AgentClassConditionStartersLinked) {
		logger.Info("start gate: the class's starter set is not confirmed in the authorization store; requeuing")
		r.setFalseCondition(sess, spiceboxv1alpha1.AgentSessionConditionStarterAllowed,
			spiceboxv1alpha1.ReasonAgentSessionStartAuthzUnavailable, startersUnconfirmedDetail)
		if serr := r.applyStatus(ctx, sess); serr != nil {
			return ctrl.Result{}, true, serr
		}
		return ctrl.Result{}, true, fmt.Errorf("start gate for %s/%s: %s", sess.Namespace, sess.Name, startersUnconfirmedDetail)
	}
	ok, cerr := r.StartChecker.CheckAgentClassStart(ctx, ac.Namespace, ac.Name, perm, canonical)
	if cerr != nil {
		logger.Info("start gate: check could not be evaluated; requeuing", "err", cerr.Error())
		r.setFalseCondition(sess, spiceboxv1alpha1.AgentSessionConditionStarterAllowed,
			spiceboxv1alpha1.ReasonAgentSessionStartAuthzUnavailable, cerr.Error())
		if serr := r.applyStatus(ctx, sess); serr != nil {
			return ctrl.Result{}, true, serr
		}
		return ctrl.Result{}, true, fmt.Errorf("start gate for %s/%s: %w", sess.Namespace, sess.Name, cerr)
	}
	if !ok {
		logger.Info("start gate: refused", "subject", canonical.String())
		return r.refuseStart(ctx, sess, canonical, "started_by does not hold "+perm+" on the class")
	}
	r.setTrueCondition(sess, spiceboxv1alpha1.AgentSessionConditionStarterAllowed,
		spiceboxv1alpha1.ReasonAgentSessionStarterAllowed, "")
	// Persist the verdict now: "decided once" is a promise about durable
	// status, not the in-memory object this call happened to be handed. Without
	// this write the True condition lives only on the caller's local copy, and
	// whether it ever reaches the API server depends on some LATER applyStatus
	// in the same Reconcile happening to run — which the checker-error branch
	// above does not leave to chance either.
	if serr := r.applyStatus(ctx, sess); serr != nil {
		return ctrl.Result{}, true, serr
	}
	return ctrl.Result{}, false, nil
}

// refuseStart fails the session as a policy halt and tells the requester. The
// operator-facing detail goes on status; the person-facing body is fixed copy.
func (r *Reconciler) refuseStart(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, requester identity.CanonicalUserID, detail string) (ctrl.Result, bool, error) {
	sess.Status.StartFailure = &spiceboxv1alpha1.AgentSessionStartFailure{
		Reason:  spiceboxv1alpha1.ReasonAgentSessionNotAnAllowedStarter,
		Message: detail,
	}
	r.setFalseCondition(sess, spiceboxv1alpha1.AgentSessionConditionStarterAllowed,
		spiceboxv1alpha1.ReasonAgentSessionNotAnAllowedStarter, detail)
	if !requester.IsZero() {
		if r.StartRefusedNoticePublish == nil {
			log.FromContext(ctx).Info("start gate: no notice publisher wired; the person will not be told",
				"session", sess.Namespace+"/"+sess.Name)
		} else if err := r.StartRefusedNoticePublish(ctx, sess.Namespace, sess.Name, "user:"+requester.String(), startRefusedBody); err != nil {
			log.FromContext(ctx).Info("start gate: refusal notice publish failed; the refusal stands",
				"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		}
	}
	// The Failed condition's Message is NOT operator-facing: the browser chat
	// renders it verbatim as the reason a session ended (chat.failureMessageOf),
	// so `detail` — which names a relation and a status field — would be read by
	// the very person the fixed copy above exists to protect. The detail is still
	// recorded, on status.startFailure.Message, the StarterAllowed condition, and
	// the log, where only an operator reads it.
	res, err := r.markBootFailed(ctx, sess, spiceboxv1alpha1.ReasonAgentSessionNotAnAllowedStarter, startRefusedBody)
	return res, true, err
}

// restartStarterAllowed enforces sess's class start gate against triggeredBy
// before ReconcileRestart materializes ANY state — no memory copy, no child
// CR, no started_by tuple — for EVERY restart mode, takeover included.
//
// Takeover deliberately skips the SessionFork gate that runs after this one
// (Step 1.5), on the premise that channelsd's app-mention is "the same basis
// on which [the requester] could start their own session" — see that gate's
// comment. A class's allowedStarters is exactly what invalidates that
// premise for someone not on the list: without this check here, a
// non-listed takeover would land started_by (⇒ interact ⇒ read_transcript)
// on a child seeded with a copy of the parent's transcript, with no pod ever
// running and no other gate in the path to catch it.
//
// allowed=false with err==nil means the denial — RestartDenied=True,
// PendingRestart cleared — is already persisted; the caller must return
// (false, res, nil) now. A non-nil err is indeterminate (the checker RPC
// itself failed, the class read failed for any reason OTHER than NotFound, or
// its starter set is not confirmed linked) and must requeue, never deny —
// mirroring EnforceStartGate's indeterminate-is-not-a-refusal rule. A DELETED
// class is the one read failure that denies rather than requeues; see below.
func (r *Reconciler) restartStarterAllowed(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, triggeredBy identity.Subject) (allowed bool, res ctrl.Result, err error) {
	var ac spiceboxv1alpha1.AgentClass
	if gerr := r.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: sess.Spec.Class}, &ac); gerr != nil {
		if !apierrors.IsNotFound(gerr) {
			return false, ctrl.Result{}, fmt.Errorf("restartStarterAllowed: get AgentClass %q: %w", sess.Spec.Class, gerr)
		}
		// The class is GONE, and a deleted class never comes back under the
		// same name by accident. Erroring here would be a permanent error:
		// ReconcileRestart runs above chain-head anchoring and the terminal
		// reap, so a terminal session still carrying a restart marker would
		// requeue forever and never finish being cleaned up. DENY instead —
		// same shape as every other refusal below — so the marker burns and
		// the rest of the reconcile can proceed.
		log.FromContext(ctx).Info("restart start gate: the session's AgentClass no longer exists; denying the restart so cleanup can proceed",
			"session", sess.Namespace+"/"+sess.Name, "agentclass", sess.Spec.Class)
		return r.denyRestart(ctx, sess, spiceboxv1alpha1.ReasonAgentClassMissing, classMissingRestartBody)
	}
	perm := ac.StartGatePermission()
	if perm == "" {
		return true, ctrl.Result{}, nil
	}
	// triggeredBy can be a non-user subject (a service principal), which can
	// never sit on agentclass#starter. CanonicalUserID fails closed there, and
	// the zero canonical drops into the no-human-requester refusal below rather
	// than admitting an id parsed out of a subject that does not name a person.
	canonical, subjErr := triggeredBy.CanonicalUserID()
	if subjErr != nil {
		log.FromContext(ctx).Info("restart start gate: restart requester is not a user subject; refusing",
			"session", sess.Namespace+"/"+sess.Name, "subject", triggeredBy.String(), "err", subjErr.Error())
	}
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name,
		"agentclass", ac.Namespace+"/"+ac.Name, "permission", perm, "subject", canonical.String())

	admit := false
	switch {
	case canonical.IsZero():
		logger.Info("restart start gate: refusing a restart with no human requester on an allowlisted class")
	case r.StartChecker == nil:
		logger.Info("restart start gate: no StartChecker wired; refusing (fail closed)")
	default:
		// Same indeterminate rule EnforceStartGate applies: an unconfirmed
		// starter set makes the check a question about a list SpiceDB was never
		// told, so it must requeue, never deny. Denying would burn the restart
		// marker on a race a retry routinely wins.
		if !meta.IsStatusConditionTrue(ac.Status.Conditions, spiceboxv1alpha1.AgentClassConditionStartersLinked) {
			return false, ctrl.Result{}, fmt.Errorf("restartStarterAllowed for %s/%s: %s",
				sess.Namespace, sess.Name, startersUnconfirmedDetail)
		}
		ok, cerr := r.StartChecker.CheckAgentClassStart(ctx, ac.Namespace, ac.Name, perm, canonical)
		if cerr != nil {
			return false, ctrl.Result{}, fmt.Errorf("restartStarterAllowed: check: %w", cerr)
		}
		if !ok {
			logger.Info("restart start gate: refused")
		}
		admit = ok
	}
	if admit {
		return true, ctrl.Result{}, nil
	}
	return r.denyRestart(ctx, sess, spiceboxv1alpha1.ReasonAgentSessionNotAnAllowedStarter, startRefusedBody)
}

// denyRestart records a definitive restart refusal exactly the way the
// SessionFork gate denies (restart.go's Step 1.5): a False→True RestartDenied
// condition channelsd's session watcher relays to the thread, and
// PendingRestart cleared so the denial does not reconcile-loop. No separate
// notice publish — the relay is the delivery.
//
// body is what the person reads, so it is plain words; reason is what an
// operator and the relay switch on.
//
// Returns the (allowed=false, err=nil) shape restartStarterAllowed's contract
// documents: the denial is already persisted and the caller must stop.
func (r *Reconciler) denyRestart(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, reason, body string) (bool, ctrl.Result, error) {
	base := sess.DeepCopy()
	patched := sess.DeepCopy()
	conditions.Set(patched, &patched.Status.Conditions, metav1.Condition{
		Type:    spiceboxv1alpha1.AgentSessionConditionRestartDenied,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: body,
	})
	patched.Status.PendingRestart = nil
	if perr := agentstatus.WriteOwned(ctx, r.Client, patched, base, spiceboxv1alpha1.OwnerOperator); perr != nil {
		return false, ctrl.Result{}, fmt.Errorf("restartStarterAllowed: patch restart-denied: %w", perr)
	}
	return false, ctrl.Result{}, nil
}
