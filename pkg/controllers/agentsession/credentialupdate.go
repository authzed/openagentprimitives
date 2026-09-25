// pkg/controllers/agentsession/credentialupdate.go
//
// The credential-update park/unpark half of the credentialupdaterequest
// slice (see pkg/controllers/credentialupdaterequest's package doc for the
// determination side). By design that reconciler NEVER writes
// AgentSession.status.phase -- phase is owned by THIS controller, which
// already has the analogous AwaitingCredentials park for the passthrough
// identity gate (parkAwaitingCredentials, passthrough.go). Co-ownership of
// one status field by two operator reconcilers is the exact hazard
// AGENTS.md's StatusFieldOwner note warns about, so this file is the ONLY
// place that reads a CredentialUpdateRequest to decide this session's phase.
package agentsession

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// mapCredentialUpdateRequestToSession re-enqueues the AgentSession a
// CredentialUpdateRequest's spec.sessionRef names, on every change to that
// request. A pure name-projection: unlike sessionsForAgentIdentityChange and
// sessionsForUserIdentityChange it needs no List, because spec.sessionRef
// already names the exact session 1:1.
func mapCredentialUpdateRequestToSession(_ context.Context, o client.Object) []reconcile.Request {
	cur, ok := o.(*spiceboxv1alpha1.CredentialUpdateRequest)
	if !ok {
		return nil
	}
	if cur.Spec.SessionRef.Namespace == "" || cur.Spec.SessionRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{
		Namespace: cur.Spec.SessionRef.Namespace,
		Name:      cur.Spec.SessionRef.Name,
	}}}
}

// reconcileCredentialUpdatePark is the credential_update category's park/
// unpark step, called early in the main Reconcile (before foldLifecycle/
// derivePhase, exactly like the identity-gate's AwaitingCredentials park) so
// its phase override is never immediately clobbered by the lifecycle-derived
// steady-state write (see phase.go's reconcilePhase: any non-Pending
// projected phase wins over whatever is on the CR, so a direct override here
// MUST return before that computation runs).
//
// proceed=false means "stop, the caller should return res/err as-is" (a
// request still waiting on a human, forcing the park); proceed=true means
// "keep going" (nothing to do, or an unpark just cleared -- either way the
// rest of Reconcile, including derivePhase, still needs to run to compute the
// real phase).
//
// Never touches the runner pod: unlike parkAwaitingCredentials (identity
// gate, which parks BEFORE a runner exists and stops any leftover one), this
// park happens WHILE the runner is alive and blocked in-process inside the
// request_credential_update meta tool call -- tearing the pod down here
// would destroy that blocked call and make "credential fixed -> retry"
// impossible.
func (r *Reconciler) reconcileCredentialUpdatePark(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (ctrl.Result, bool, error) {
	if isTerminalPhase(sess.Status.Phase) {
		return ctrl.Result{}, true, nil
	}

	awaiting, err := r.awaitingCredentialUpdateRequestFor(ctx, sess)
	if err != nil {
		return ctrl.Result{}, false, fmt.Errorf("list CredentialUpdateRequests for session %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	pending := conditions.IsTrue(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending)

	if awaiting != nil {
		// Park (or stay parked): force AwaitingCredentials every reconcile while
		// a request is still waiting on a human, and return early so derivePhase
		// never runs this pass -- reconcilePhase would otherwise overwrite this
		// back to whatever the (unrelated, still-Running) lifecycle log projects.
		conditions.SetTrue(sess, &sess.Status.Conditions,
			spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending, spiceboxv1alpha1.ReasonCredentialUpdateRequested)
		sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials
		return ctrl.Result{}, false, r.applyStatus(ctx, sess)
	}

	if pending {
		// Unpark: the request that caused the park is no longer waiting on
		// anybody (Fulfilled, Expired, Refused, or deleted). Clear the marker and
		// fall through -- deliberately WITHOUT touching Phase here. The single
		// applyStatus call at the tail of Reconcile persists this condition-clear
		// together with whatever phase derivePhase computes a few lines later, in
		// one write.
		conditions.SetFalse(sess, &sess.Status.Conditions,
			spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending, spiceboxv1alpha1.ReasonCredentialUpdateResolved,
			"the CredentialUpdateRequest that parked this session is no longer waiting on a human")
	}
	return ctrl.Result{}, true, nil
}

// awaitingCredentialUpdateRequestFor returns the first CredentialUpdateRequest
// whose spec.sessionRef names sess AND which is still waiting on a human, or
// nil if none. Namespace-scoped List + filter, mirroring
// credentialupdaterequest.budgetExceeded's own shape -- there is no index from
// session to its requests, and cardinality per namespace is small (bounded by
// how many sessions + requests share it).
//
// "Waiting on a human" is IsCredentialUpdateRequestAwaitingHuman, NOT
// phase == Open. A COLLAPSED request -- one whose credential already has a card
// up for another session -- parks its session exactly as hard as an Open one
// does: the agent behind it is blocked in the same meta-tool call, on the same
// credential, and the meta tool waits on non-terminal rather than on Open. A
// filter that only saw Open left that session reading Running while its runner
// was stuck, and made "the follower's session unparks when its request settles"
// a statement about a park that never happened.
func (r *Reconciler) awaitingCredentialUpdateRequestFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (*spiceboxv1alpha1.CredentialUpdateRequest, error) {
	var list spiceboxv1alpha1.CredentialUpdateRequestList
	if err := r.Client.List(ctx, &list, client.InNamespace(sess.Namespace)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		item := &list.Items[i]
		if !spiceboxv1alpha1.IsCredentialUpdateRequestAwaitingHuman(item.Status.Phase) {
			continue
		}
		if item.Spec.SessionRef.Namespace != sess.Namespace || item.Spec.SessionRef.Name != sess.Name {
			continue
		}
		return item, nil
	}
	return nil, nil
}
