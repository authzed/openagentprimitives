// Package sessionhold hosts the SessionHold reconciler: the operator-side
// component that publishes the session_release approval card once a hold has
// been observed Active, and applies the human's decision on it.
//
// Shaped after pkg/controllers/credentialupdaterequest — a per-session-CR
// reconciler that Gets its own CR, gates on its own status.phase, and writes
// ONLY its own status via a merge patch against a captured prior. It departs
// from that shape in the one way the task's design calls for: where
// CredentialUpdateRequest determines only (channelsd's CredentialUpdateWatcher
// publishes the card and reacts to the Secret that fulfills it), a SessionHold
// has no live runner and no AwaitingXYZ park for a channelsd watcher to poll —
// the session was already parked at PhaseHeld and its runner reaped by
// pkg/controllers/agentsession's own reconcileHold before this controller ever
// sees the CR. So THIS reconciler both publishes (mirroring
// pkg/channels/channelsd/pipeline/credential_request.go's
// CredentialRequestWatcher: an injected channelevents.PublishFunc onto the
// operator's own NATS connection — internal/cmd/operator/main.go already
// builds one, "monitoringPublish", reused here the same way it is reused for
// sandbox-degradation events) and decides (Decide, shaped as a
// channelinteractions.DecisionHandler — see that method's own doc for what is
// and is not wired to call it today).
//
// Phase stamping (Active, TrippedAt) and containment (snapshot, pod reap)
// belong to pkg/controllers/agentsession's reconcileHold, not to this
// package: this reconciler only ever observes an ALREADY-Active hold.
//
// This controller NEVER writes AgentSession.status. It flips only the hold's
// OWN status.phase to Released; the AgentSession reconciler observes that on
// its next pass (activeHoldFor treats a released hold as inactive) and emits
// the Released lifecycle event itself — co-ownership of one CR's phase by two
// operator reconcilers is the hazard
// pkg/controllers/agentsession/credentialupdate.go documents, and the reason
// this package touches only SessionHold.status.
package sessionhold

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories/sessionrelease"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// Reconciler implements the SessionHold controller.
//
// Memory and NATSPublish are declared as interface/func-typed fields and must
// only ever be assigned a real, non-nil value — see AGENTS.md's typed-nil
// rule. A nil Memory or NATSPublish is tolerated at call time (checked and
// reported as a wiring-bug error, per AGENTS.md's no-silent-errors rule)
// rather than panicking, so a misconfigured test fixture fails loudly instead
// of crashing the process.
type Reconciler struct {
	Client client.Client

	// Memory is the operator's signing memory facade (opSigned in
	// internal/cmd/operator/main.go). Used to read the session's plan-gate log
	// for its current plan digest and to write EventApprovalsCleared before a
	// release — the same facade and the same append-only Kind
	// derivePlanGateRoot uses (pkg/controllers/agentsession/restart.go), so
	// this reconciler's writes are signed and verified the identical way.
	Memory memory.Memory

	// NATSPublish publishes the session_release interaction_request onto the
	// held session's .out subject. There is no live runner and no channelsd
	// watcher polling this CR, so the operator's own connection is the only
	// publisher available — see the package doc.
	NATSPublish channelevents.PublishFunc

	// Now is the time source for the plan-gate clear record's timestamp; nil
	// defaults to time.Now().UTC(). Tests inject a fixed value.
	Now func() time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=sessionholds,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=sessionholds/status,verbs=get;update;patch

// Reconcile cascades an Active hold across its session's delegation subtree,
// then publishes the session_release card the first time it observes a
// SessionHold already stamped Active (Phase and TrippedAt are stamped by
// pkg/controllers/agentsession's reconcileHold, not here) — UNLESS hold is
// itself a cascaded hold (carries LabelCascadeOf), in which case no card is
// ever published for it: the originating hold's card is the one release path
// for the whole subtree (see the guard just above publishCard, below). Every
// later pass over a non-cascaded hold — including one where the card is
// still unanswered — is a card-publishing no-op: nothing in this method ever
// advances status.phase to Released except an explicit approved Decide call,
// which is the property sessionrelease.FailsClosedOnTimeout documents.
//
// cascadeHold runs on EVERY reconcile of an Active hold, not just the first —
// placed before the InteractionRef short-circuit below rather than after it,
// deliberately, so a card already having been published never stops later
// reconciles from re-attempting the cascade (idempotently: cascadeName is
// deterministic and AlreadyExists is success). Without that placement, a
// delegation started after the card was already published would never be
// caught by this method at all, only by the AgentSession watch in
// SetupWithManager re-enqueueing the hold.
//
// A cascade FAILURE must never block the card: publishCard is what sets
// Status.InteractionRef, and Decide refuses any click whose RequestRef does
// not match it (see Decide) — so if cascadeHold's error blocked publishCard,
// a permanently-failing cascade (an over-length label value, say) would leave
// a frozen session, its pods already reaped, with NO click able to release it
// at all. So a cascade failure is recorded on Status.Determination and
// logged, the card is published regardless, and the error is returned
// AFTERWARD so the standard requeue-with-backoff keeps retrying the cascade —
// mirrors reconcileHold's own "containment still wins: park and reap anyway,
// but say so loudly and on status" for its snapshot-failure branch
// (pkg/controllers/agentsession/hold.go).
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// This reconciler reaches *memory.Local directly (via the operator's
	// signing facade), not over HTTP — see agentsession.Reconciler.Reconcile's
	// identical wrap for why: the facade's capability door denies any caller
	// that arrives without a system approval on ctx.
	ctx = memory.WithSystemApproval(ctx, "operator:sessionhold-controller")
	logger := log.FromContext(ctx)

	var hold spiceboxv1alpha1.SessionHold
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &hold); !cont {
		return ctrl.Result{}, err
	}

	if hold.IsReleased() {
		// Terminal from this controller's perspective: the AgentSession
		// reconciler is the one that reacts to a released hold.
		return ctrl.Result{}, nil
	}
	if hold.Status.Phase != spiceboxv1alpha1.SessionHoldPhaseActive {
		// Not yet stamped Active by reconcileHold. Nothing for this controller
		// to do until it has been.
		return ctrl.Result{}, nil
	}

	cascadeErr := r.cascadeHold(ctx, &hold, logger)
	if cascadeErr != nil {
		logger.Info("sessionhold: cascade failed; some descendants may still be running unheld; publishing the release card anyway so a human is not locked out",
			"hold", hold.Namespace+"/"+hold.Name, "err", cascadeErr.Error())
		r.recordCascadeFailure(ctx, &hold, cascadeErr, logger)
	}

	// A cascaded hold (carrying LabelCascadeOf) never gets a card of its own:
	// the originating hold's card IS the release path for the whole subtree —
	// approving it releases every cascaded hold in one call (releaseCascade).
	// Without this guard, agentsession's OWN reconcileHold stamps Phase=Active
	// on the first unreleased hold naming a session, cascaded holds included,
	// and this method — having already skipped CASCADING on the label guard
	// inside cascadeHold — falls straight through to publishCard anyway.
	// Holding a root with N descendants would then post N+1 "Release this held
	// session?" cards into the same thread, all addressed to the same owner,
	// and open a silent partial-release path: a human clearing one child's
	// card while the parent (and the rest of the subtree) stays under review.
	//
	// This does not depend on a cascaded child being headless. A conversational
	// (task/chat) child has a real spec.inputChannel of its own, but it names
	// an `agent`-kind Channel whose far side is its parent session — and
	// releaseBinding's walk (ResolveHumanDirectedBinding, gated on
	// registry.DeliversToHuman) refuses to stop on a binding no human reads,
	// for either child mode. So every cascaded hold's card, guard or no guard,
	// resolves to the SAME ancestor binding as the originating hold's: the N+1
	// consequence above is structural, for every child mode, not contingent on
	// what a given descendant happens to be bound to.
	_, isCascaded := hold.Labels[spiceboxv1alpha1.LabelCascadeOf]
	if hold.Status.InteractionRef == "" && !isCascaded {
		if res, err := r.publishCard(ctx, &hold, logger); err != nil {
			return res, err
		}
	}

	if cascadeErr != nil {
		// Containment is incomplete until the cascade succeeds. Retried via the
		// standard requeue-on-error path (controller-runtime's own backoff),
		// same as every other retryable failure in this file — the failure is
		// already surfaced on Status.Determination above (recordCascadeFailure),
		// so an operator inspecting the SessionHold is not left guessing why
		// descendants are still running. The card itself does not show it.
		return ctrl.Result{}, fmt.Errorf("sessionhold: reconcile: cascade hold %s/%s: %w", hold.Namespace, hold.Name, cascadeErr)
	}
	return ctrl.Result{}, nil
}

// releaseBinding finds the nearest human-readable channel binding in sess's
// lineage — sess's own binding when a human reads it, otherwise the nearest
// ancestor's — so ownerIdentity can stamp a real Kind onto a delegated
// child's approver identity, rather than leaving it empty.
//
// No delegated child answers that question from its own binding. A single_turn
// child is headless by construction: it has no spec.inputChannel, and a held
// session cannot gain one (see the package doc). A task/chat child DOES have
// one, but it is a Channel of kind `agent` whose far side is its parent
// session, which DeliversToHuman rejects — so both shapes fall through to the
// ancestor walk, for the same reason and by the same predicate. Without this
// walk, publishCard's old
// sess.Spec.InputChannel == nil guard would treat every held child exactly
// like a genuinely channel-less kubectl root: no card, no Decide path, and
// no exit but deletion — which destroys the evidence the hold exists to
// preserve.
//
// The card is still addressed at sess itself (the child): the payload names
// the child, which headless_test.go asserts. Delivery is resolved the same
// way, independently, on the far side of the bus: the outbound relay
// (pkg/channels/channelsd/outbound/relay.go) re-runs this same
// ResolveHumanDirectedBinding walk for the KindInteractionRequest envelope
// this card rides on, so the card built here lands in the resolved ancestor's
// channel instead of being published and never seen. Both sides pass
// registry.DeliversToHuman, so both skip an `agent`-kind binding — a released
// hold is a person's decision, and a conversational child's own Channel leads
// to its parent agent, not to a person.
//
// Three outcomes, kept distinct rather than folded into a single nil: a
// resolved binding (found on sess or an ancestor), no human-readable binding
// anywhere (nil, nil — a kubectl-driven session has nowhere to route to, and
// that is not an error), and a cycle, over-deep chain, or unregistered channel
// kind (a non-nil error — never silently treated as "no binding", per
// AGENTS.md's no-silent-errors rule).
func (r *Reconciler) releaseBinding(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (*spiceboxv1alpha1.ChannelBinding, error) {
	target, err := spiceboxv1alpha1.ResolveHumanDirectedBinding(ctx, r.Client, sess, registry.DeliversToHuman)
	if err != nil {
		return nil, fmt.Errorf("sessionhold: releaseBinding: %w", err)
	}
	if target == nil {
		// No human-readable binding anywhere in the lineage. Preserved as a
		// nil binding rather than a zero-value one: the caller distinguishes
		// "nobody to ask" from "here is where to ask", and a non-nil binding
		// carrying an empty name would read as the latter.
		return nil, nil
	}
	// This caller routes only; the resolved owner matters to the outbound
	// relay, which has to tell a client-hosted host WHICH session it is being
	// asked about.
	return target.Binding, nil
}

func (r *Reconciler) publishCard(ctx context.Context, hold *spiceboxv1alpha1.SessionHold, logger logr.Logger) (ctrl.Result, error) {
	if r.NATSPublish == nil {
		// Never a silent no-op: without a publisher there is no way to deliver
		// the card at all, which would hang the hold exactly as a nil
		// sub-channel sender would hang CredentialRequestWatcher's park.
		return ctrl.Result{}, fmt.Errorf(
			"sessionhold: NATSPublish not configured (wiring bug); cannot deliver the release card for %s/%s",
			hold.Namespace, hold.Name)
	}

	sessKey := client.ObjectKey{Namespace: hold.Spec.SessionRef.Namespace, Name: hold.Spec.SessionRef.Name}
	var sess spiceboxv1alpha1.AgentSession
	if err := r.Client.Get(ctx, sessKey, &sess); err != nil {
		return ctrl.Result{}, fmt.Errorf("sessionhold: get AgentSession %s: %w", sessKey, err)
	}
	binding, err := r.releaseBinding(ctx, &sess)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("sessionhold: resolve release-card binding for %s/%s: %w",
			hold.Namespace, hold.Name, err)
	}
	if binding == nil {
		// No human-readable channel binding anywhere in this session's
		// lineage. Two shapes collapse to this nil: a kubectl-only root that
		// has no binding at all, and a conversational child whose entire
		// ancestry is `agent`-kind bindings that only lead to another agent
		// session (see ResolveHumanDirectedBinding's doc) — every session in
		// that chain HAS a binding, just none a person reads. Logged so an
		// operator can see why the hold never got a card; not an error, since
		// nothing here will ever change until a session in the lineage gains,
		// or already has, a binding a human actually reads — which a held
		// session cannot make happen on its own.
		logger.Info("sessionhold: no human-readable channel binding anywhere in hold's session lineage; no surface to publish the release card on",
			"hold", hold.Namespace+"/"+hold.Name, "session", sessKey.String())
		return ctrl.Result{}, nil
	}

	card := sessionrelease.BuildCard(sessionrelease.CardInput{
		SessionName:    hold.Spec.SessionRef.Name,
		Reason:         hold.Spec.Reason,
		Source:         hold.Spec.Source,
		SnapshotHandle: hold.Status.SnapshotHandle,
	})
	ref := mintRequestID()
	payload := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sessKey.Namespace, Name: sessKey.Name},
		Category:        sessionrelease.CategoryName,
		RequestRef:      ref,
		Lead:            card.Lead,
		Body:            card.Body,
		Fields:          cardFields(card),
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Release session", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "refuse", Label: "Keep held", Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{ownerIdentity(&sess, binding)},
		},
	}
	if err := payload.Validate(); err != nil {
		return ctrl.Result{}, fmt.Errorf("sessionhold: built an invalid interaction_request payload for %s/%s: %w",
			hold.Namespace, hold.Name, err)
	}
	if err := channelevents.PublishOut(r.NATSPublish, sessKey.Namespace, sessKey.Name, channelevents.KindInteractionRequest, payload); err != nil {
		return ctrl.Result{}, fmt.Errorf("sessionhold: publish release interaction_request for %s/%s: %w",
			hold.Namespace, hold.Name, err)
	}

	prior := hold.DeepCopy()
	hold.Status.InteractionRef = ref
	if err := r.Client.Status().Patch(ctx, hold, client.MergeFrom(prior)); err != nil {
		// Published but not recorded: a later reconcile would re-publish a
		// second card. Logged loudly and returned so the caller retries the
		// patch — per AGENTS.md's no-silent-errors rule.
		return ctrl.Result{}, fmt.Errorf("sessionhold: patch InteractionRef for %s/%s: %w", hold.Namespace, hold.Name, err)
	}
	logger.Info("sessionhold: published release card",
		"hold", hold.Namespace+"/"+hold.Name, "session", sessKey.String(), "requestRef", ref)
	return ctrl.Result{}, nil
}

// Decide is the bound decision handler for sessionrelease.CategoryName,
// shaped exactly like channelinteractions.DecisionHandler so it can be
// registered with channelinteractions.Bind wherever a process needs to apply
// this category's clicks — mirroring
// pkg/channels/channelsd/pipeline/permission_interaction.go's decidePermission,
// the bound handler for the other DecideOwner, no-park, no-resume category
// (permission_request). Like that handler, standing is NOT re-checked here:
// the generic decision pipe that invokes a bound handler already runs the
// category's DeciderPolicy check first (channelinteractions.DecideOwner for
// session_release) — this method's only job is to apply the decision.
//
// Delivering a human's click to THIS method requires a process to actually
// call it: internal/cmd/operator/main.go registers Reconcile with the
// controller-runtime manager, which publishes the card, and
// internal/cmd/channelsd/main.go calls pipeline.BindSessionReleaseHandler at
// process start, which binds this method (via Pipeline.decideSessionRelease,
// pkg/channels/channelsd/pipeline/session_release_interaction.go) into the
// registry HandleInteractionDecision
// (pkg/channels/channelsd/pipeline/interaction_decision.go) invokes for every
// resolved interaction_decision envelope.
//
// On approve: writes plangateaudit.EventApprovalsCleared for the session's
// CURRENT plan digest, THEN releases every cascaded hold in this hold's
// subtree (releaseCascade), THEN flips this hold itself to Released. That
// order is load-bearing throughout — see clearStandingApprovals's error path:
// a release that clears nothing hands the agent back the exact ceiling it
// was frozen with, which is the failure this whole feature exists to
// prevent, so a failed clear must leave the hold Active rather than release
// anyway. releaseCascade sits in the same "must succeed before we commit"
// position for the identical reason: a release that cascades to only part of
// a delegation subtree is not a completed release, so a failure there also
// leaves this hold Active rather than Released, and a retry resumes against
// whatever cascaded holds are still left.
//
// On refuse (or any non-approve action): the hold is left Active. Task 3's
// SessionHoldPhase is exactly Active/Released; there is no third state to
// invent for a refusal. Nothing cascades on a refusal — releaseCascade is
// reached only on approve, below.
//
// This method never touches AgentSession.status — see the package doc.
func (r *Reconciler) Decide(ctx context.Context, d channelinteractions.Decision) (channelinteractions.Outcome, error) {
	ctx = memory.WithSystemApproval(ctx, "operator:sessionhold-controller")
	logger := log.FromContext(ctx)
	ns, name := d.Session.Namespace, d.Session.Name

	hold, err := r.activeHoldFor(ctx, ns, name)
	if err != nil {
		return channelinteractions.Outcome{}, fmt.Errorf("sessionhold: decide: list SessionHolds in %s: %w", ns, err)
	}
	if hold == nil {
		// Already released, or never held: a stale/duplicate click.
		logger.Info("sessionhold: decide: no active hold for session; stale or duplicate click",
			"session", ns+"/"+name, "requestRef", d.Payload.RequestRef)
		return channelinteractions.Outcome{Result: channelevents.OutcomeExpired, OutcomeText: "already resolved"}, nil
	}
	if d.Payload.RequestRef != hold.Status.InteractionRef {
		// A click on a request this hold no longer recognizes as current — a
		// stale card, or a decision meant for a different hold entirely.
		logger.Info("sessionhold: decide: requestRef does not match the hold's current InteractionRef; stale or duplicate click",
			"hold", hold.Namespace+"/"+hold.Name, "requestRef", d.Payload.RequestRef, "current", hold.Status.InteractionRef)
		return channelinteractions.Outcome{Result: channelevents.OutcomeExpired, OutcomeText: "already resolved"}, nil
	}

	if d.Payload.ActionID != "approve" {
		prior := hold.DeepCopy()
		hold.Status.Determination = "refused: the session stays held"
		if err := r.Client.Status().Patch(ctx, hold, client.MergeFrom(prior)); err != nil {
			return channelinteractions.Outcome{}, fmt.Errorf("sessionhold: decide: patch refusal for %s/%s: %w",
				hold.Namespace, hold.Name, err)
		}
		return channelinteractions.Outcome{Result: channelevents.OutcomeDenied, OutcomeText: "Kept held"}, nil
	}

	approvedBy, err := approverSubject(d.Payload.Decider)
	if err != nil {
		return channelinteractions.Outcome{}, fmt.Errorf("sessionhold: decide: resolve approver identity for %s/%s: %w",
			hold.Namespace, hold.Name, err)
	}

	if err := r.clearStandingApprovals(ctx, ns, name, hold); err != nil {
		return channelinteractions.Outcome{}, fmt.Errorf("sessionhold: decide: clear standing plan approvals for %s/%s: %w",
			hold.Namespace, hold.Name, err)
	}

	// Anti-one-way-door step: release every hold cascaded from this one BEFORE
	// this hold itself flips to Released — see releaseCascade's doc for why
	// this ordering, and why a cascaded hold is marked Released and then left
	// in place as the audit record, GC'd with its own session by the owner
	// ref, rather than deleted.
	if err := r.releaseCascade(ctx, hold, approvedBy, logger); err != nil {
		return channelinteractions.Outcome{}, fmt.Errorf("sessionhold: decide: release cascaded holds for %s/%s: %w",
			hold.Namespace, hold.Name, err)
	}

	prior := hold.DeepCopy()
	hold.Status.Phase = spiceboxv1alpha1.SessionHoldPhaseReleased
	hold.Status.Determination = "released"
	hold.Status.ReleasedBy = approvedBy
	if err := r.Client.Status().Patch(ctx, hold, client.MergeFrom(prior)); err != nil {
		return channelinteractions.Outcome{}, fmt.Errorf("sessionhold: decide: patch release for %s/%s: %w",
			hold.Namespace, hold.Name, err)
	}
	logger.Info("sessionhold: released", "hold", hold.Namespace+"/"+hold.Name, "session", ns+"/"+name)
	return channelinteractions.Outcome{Result: channelevents.OutcomeApproved, OutcomeText: "Released"}, nil
}

// approverSubject canonicalizes the clicker's claimed identity (d.Payload.Decider)
// into the Subject SessionHoldStatus.ReleasedBy carries. The generic decision
// pipe (pkg/channels/channelsd/pipeline/interaction_decision.go) has already
// canonicalized and standing-checked this same Decider before ever invoking
// Decide -- re-deriving it here, rather than threading the pipe's own
// canonical value through channelinteractions.Decision, keeps this package's
// only identity dependency the one it already had.
func approverSubject(decider channelevents.ExternalIdentity) (identity.Subject, error) {
	canon, err := identity.FromExternal(
		identity.Kind(decider.Kind),
		identity.TeamScope(decider.TeamScope),
		identity.RawExternalID(decider.ExternalID),
		identity.Email(decider.Email),
	).Canonical()
	if err != nil {
		return "", fmt.Errorf("decider has no canonical identity: %w", err)
	}
	return canon.Subject(), nil
}

// clearStandingApprovals writes plangateaudit.EventApprovalsCleared for the
// session's CURRENT plan digest, recomputed from its own plan-gate log via
// plangate.PlanFromRecords — the same reconstruction
// pkg/controllers/agentsession/restart.go's derivePlanGateRoot uses to derive
// a fork root purely from records, with no in-memory plan to consult. A
// session that never approved a plan (the gate disabled, or never used)
// clears nothing: per the design, release is then plain resumption.
func (r *Reconciler) clearStandingApprovals(ctx context.Context, ns, name string, hold *spiceboxv1alpha1.SessionHold) error {
	if r.Memory == nil {
		return fmt.Errorf("sessionhold: Memory not configured (wiring bug)")
	}
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
	records, err := plangateaudit.List(ctx, r.Memory, scope)
	if err != nil {
		return fmt.Errorf("list plan-gate log: %w", err)
	}
	plan, ok := plangate.PlanFromRecords(records)
	if !ok {
		return nil
	}
	return plangateaudit.Record(ctx, r.Memory, scope, plangateaudit.Content{
		Event:      plangateaudit.EventApprovalsCleared,
		PlanDigest: plan.Digest(),
		Mode:       "released",
		Provenance: "sessionhold:" + hold.Name,
		At:         r.now(),
	})
}

// activeHoldFor returns the first unreleased SessionHold naming the session,
// or nil. Mirrors pkg/controllers/agentsession's unexported activeHoldFor of
// the same name and shape (a namespace-scoped List + filter — there is no
// index from session to its holds, and cardinality per namespace is small);
// that one is a method on a different Reconciler type in a different
// package, so it cannot be called directly from here.
func (r *Reconciler) activeHoldFor(ctx context.Context, ns, sessionName string) (*spiceboxv1alpha1.SessionHold, error) {
	var list spiceboxv1alpha1.SessionHoldList
	if err := r.Client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("list SessionHolds in %s: %w", ns, err)
	}
	for i := range list.Items {
		h := &list.Items[i]
		if h.Spec.SessionRef.Name != sessionName {
			continue
		}
		if h.IsReleased() {
			continue
		}
		return h, nil
	}
	return nil, nil
}

// ownerIdentity resolves the session owner's ExternalIdentity from its
// started-by annotations, mirroring
// pkg/channels/channelsd/pipeline/credential_request.go's starterIdentity
// construction: the natural raw+email form when an email is on record, else
// the precomputed canonical Subject passthrough (no TeamScope annotation is
// preserved to safely re-derive a synthetic canonical without one).
//
// The channel Kind prefers sess's own InputChannel — the ordinary case, and
// the only one this ever saw before headless children could reach here — and
// falls back to binding (the ancestor's resolved release binding, from
// releaseBinding) only when sess has none. A headless child's own
// InputChannel is always nil, so without the fallback this would dereference
// a nil *ChannelBinding for exactly the case Task 11 exists to unblock.
//
// A CONVERSATIONAL child has an InputChannel, of kind `agent`, that no person
// reads — so the preferred branch stamps a kind this card can never be
// delivered by. The outbound relay corrects that at delivery, re-stamping the
// audience onto the channel it actually routed the card to
// (pkg/channels/channelsd/outbound/recipient_kind.go). It is corrected there
// rather than here because the same stamp is published by the runner too, and
// only the relay knows which binding the card ended up on.
func ownerIdentity(sess *spiceboxv1alpha1.AgentSession, binding *spiceboxv1alpha1.ChannelBinding) channelevents.ExternalIdentity {
	kind := ""
	switch {
	case sess.Spec.InputChannel != nil:
		kind = sess.Spec.InputChannel.Kind
	case binding != nil:
		kind = binding.Kind
	}
	id := channelevents.ExternalIdentity{
		Kind:       identity.Kind(kind),
		ExternalID: spiceboxv1alpha1.StartedByExternalID(sess),
		Email:      spiceboxv1alpha1.StartedByEmail(sess),
	}
	if id.Email == "" {
		id.Subject = spiceboxv1alpha1.StartedBySubject(sess)
	}
	return id
}

// cardFields renders sessionrelease.Card's platform-authored facts as
// deterministically-ordered InteractionFields — card.Fields is a map, and map
// iteration order is not stable, which would make two publishes of the
// identical card render their rows in a different order.
func cardFields(card sessionrelease.Card) []channelevents.InteractionField {
	order := []struct{ key, label string }{
		{"session", "Session"},
		{"reason", "Reason"},
		{"source", "Source"},
		{"evidence", "Evidence"},
		{"snapshot", "Snapshot"},
	}
	out := make([]channelevents.InteractionField, 0, len(card.Fields))
	for _, o := range order {
		if v, ok := card.Fields[o.key]; ok && v != "" {
			out = append(out, channelevents.InteractionField{Label: o.label, Value: v})
		}
	}
	return out
}

// mintRequestID mints a random correlation id for a published interaction
// request, mirroring pkg/channels/channelsd/pipeline's mintRequestID (and the
// runner's own newRequestID) — a small enough helper that every process
// producing request ids keeps its own copy rather than sharing one.
func mintRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("sessionhold: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// agentSessionCreateOnly restricts the AgentSession watch below to CREATE
// events only, matching mapSessionToAncestorHolds' actual purpose: catching a
// session's ancestors at the moment it is delegated (spec.parent is set once,
// at creation, and never changes). predicate.Funcs defaults every unset func
// to allow(true), so all four must be given explicitly to make this
// create-only rather than accidentally passing every event through — leaving
// UpdateFunc unset would run the mapping's List-plus-walk on every status
// write to every AgentSession in the namespace, not just the one event this
// watch exists to catch.
var agentSessionCreateOnly = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return true },
	UpdateFunc:  func(event.UpdateEvent) bool { return false },
	DeleteFunc:  func(event.DeleteEvent) bool { return false },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// SetupWithManager registers this reconciler with mgr, watching SessionHold
// objects (its own For) plus a Watches on AgentSession CREATE events, mapped
// through mapSessionToAncestorHolds: a session that is created (a
// delegation's new child, a moment after an ancestor was already held)
// re-enqueues every active hold covering one of its ancestors, so
// cascadeHold's next Reconcile discovers it promptly instead of only on
// whatever later reconcile would have happened to touch that hold anyway.
//
// A watch was chosen over the alternative of refusing the delegation outright
// in the SubagentRequest controller when an ancestor hold is active: refusing
// would mean a hold in progress makes `delegate` fail for the whole subtree
// rather than just catching the new child up to the same containment its
// siblings already have, and it would spread this concern across a second
// controller's package for something this one already has every piece of
// (DescendantsOf, cascadeHold, the SessionHold list). The watch does cost a
// List of SessionHolds plus a bounded ancestor walk per AgentSession CREATE,
// cluster-wide — see mapSessionToAncestorHolds' own doc for why that is
// bounded rather than unbounded, and it is judged worth that cost against
// silently missing a live delegation under a hold, which is the containment
// gap this controller exists to close.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.SessionHold{}).
		Watches(&spiceboxv1alpha1.AgentSession{}, handler.EnqueueRequestsFromMapFunc(r.mapSessionToAncestorHolds),
			builder.WithPredicates(agentSessionCreateOnly)).
		Complete(r)
}
