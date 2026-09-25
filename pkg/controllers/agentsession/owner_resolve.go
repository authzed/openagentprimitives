package agentsession

import (
	"context"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// resolveOwnerSubject implements the per-channel owner precedence as a pure
// function. starter is the AnnotationStartedByCanonicalID value
// ("user:<canon>" or ""). outputGroupRef is the output channel kind's
// OwnerGroupRef ("" if none/not requested).
//
// Precedence (highest first):
//  1. identityMode=userPassthrough → starter (forced; error if absent)
//  2. policy.Explicit → explicit override
//  3. starter != "" → channel-provided starting user
//  4. policy.Ownerless.Permission → permission subject-set
//  5. outputGroupRef != "" → the output channel's membership
//  6. nothing resolvable → error (fail-closed)
//
// Delegates to the shared resolver in pkg/apis/v1alpha1 so the operator (which
// WRITES the tuple) and any surface that DESCRIBES the ownership to humans can
// never disagree about who owns a session.
func resolveOwnerSubject(identityMode string, policy *spiceboxv1alpha1.ChannelOwnerPolicy, starter, outputGroupRef string, ceiling *spiceboxv1alpha1.OwnerCeiling) (string, error) {
	subject, _, err := spiceboxv1alpha1.ResolveOwnerSubject(identityMode, policy, starter, outputGroupRef, ceiling)
	return subject, err
}

// ResolveAndWriteOwners resolves the session owner and writes agentsession#owner
// via the Granter. Idempotent (TOUCH). ch may be nil (kubectl/no-channel sessions).
func (r *Reconciler) ResolveAndWriteOwners(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ch *spiceboxv1alpha1.Channel, ac *spiceboxv1alpha1.AgentClass) error {
	starter := spiceboxv1alpha1.StartedBySubject(sess).String()
	var policy *spiceboxv1alpha1.ChannelOwnerPolicy
	if ch != nil {
		policy = ch.Spec.Owner
	}
	// Resolved in two passes so the output channel is read only when it is
	// actually needed. Everything that outranks the output channel's membership
	// — passthrough, an explicit owner, a starting user, a declared ownerless
	// permission — is decided on the first pass with no group ref at all; only
	// a session that resolved to nothing reaches for one. That keeps the
	// precedence in ResolveOwnerSubject alone (a caller that pre-decided when to
	// look would be a second, drifting copy of it) and spares every
	// starter-owned session an API read it would never consult.
	ceiling := ac.Spec.GetOwnerCeiling()
	subject, err := resolveOwnerSubject(ac.Spec.IdentityMode, policy, starter, "", ceiling)
	if err != nil {
		if ref := r.outputChannelGroupRef(ctx, sess, ch, ac); ref != "" {
			subject, err = resolveOwnerSubject(ac.Spec.IdentityMode, policy, starter, ref, ceiling)
		}
	}
	if err != nil {
		// Fail-closed, and WHOLLY closed: the trigger-owner annotation below is
		// deliberately not written either. It may be an unlinked, empty
		// subject-set, and writing it as the session's only owner would leave a
		// session that looks owned and has no resolvable approver.
		return err // channel validation prevents this for channel-driven sessions
	}
	ref := authz.SessionRef{Namespace: sess.Namespace, Name: sess.Name}
	if err := authz.TouchOwner(ctx, r.AuthzGranter, ref, subject); err != nil {
		return err
	}
	if err := r.touchTriggerOwner(ctx, sess, ref); err != nil {
		return err
	}
	r.writeInteractorTuples(ctx, sess, ac)
	return nil
}

// writeInteractorTuples records, on the AgentClass, that this session's human
// STARTER has interacted with it — the enumeration source
// LookupPersonalizableClasses (Task 3) walks to offer a user's App Home
// preferences pane.
//
// v1 SCOPE: only the started_by human is tracked. Per-turn multiplayer
// participants (a second human replying into an already-running session) are
// a documented follow-up — the App Home enumeration only needs "the user has
// interacted with this class at all", and the starter already covers the
// driving DM/reviewbot-trigger case. A service/agent starter (or no starter
// at all, e.g. a kubectl-created session) writes no tuple: a bot didn't
// interact with the class as a person, and #interactor is keyed on user:
// subjects only.
//
// Idempotent and best-effort like touchTriggerOwner: a TouchInteractor
// failure is logged, not propagated. The write is a TOUCH re-issued on every
// reconcile until it lands in status.interactorTuplesWritten, so a transient
// SpiceDB error heals on the next pass rather than failing owner resolution
// (which the caller already committed to by this point).
func (r *Reconciler) writeInteractorTuples(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ac *spiceboxv1alpha1.AgentClass) {
	starter := spiceboxv1alpha1.StartedBySubject(sess)
	if starter.Empty() || starter.ObjectType() != "user" {
		return
	}
	subjectRef := starter.String()
	if slices.Contains(sess.Status.InteractorTuplesWritten, subjectRef) {
		return
	}
	if err := authz.TouchInteractor(ctx, r.AuthzGranter, ac.Namespace, ac.Name, subjectRef); err != nil {
		// Logged, not propagated: mirrors touchTriggerOwner. The tuple is a
		// TOUCH, re-attempted every reconcile since it stays absent from
		// status.interactorTuplesWritten until it succeeds, so a transient
		// SpiceDB error (or a rolling upgrade where the schema hasn't yet
		// composed #interactor) heals on its own without blocking the owner
		// resolution this function runs after.
		log.FromContext(ctx).Info("interactor tuple write failed; retried next reconcile",
			"session", sess.Namespace+"/"+sess.Name, "class", ac.Namespace+"/"+ac.Name,
			"subject", subjectRef, "err", err.Error())
		return
	}
	before := slices.Clone(sess.Status.InteractorTuplesWritten)
	sess.Status.InteractorTuplesWritten = append(sess.Status.InteractorTuplesWritten, subjectRef)
	// Persist through applyStatus, NOT a raw Status().Patch: the reconcile
	// installs its lastWritten diff-base bookkeeping (statusapply.go) before
	// this runs, and a raw merge-patch here would leave that base stale — a
	// later applyStatus in the same pass would then diff against the pre-append
	// snapshot and blindly re-send interactorTuplesWritten over whatever a
	// concurrent writer put there, the exact lost-write shape statusapply.go
	// documents. applyStatus supports several calls per pass and advances the
	// base only on a successful write.
	if err := r.applyStatus(ctx, sess); err != nil {
		log.FromContext(ctx).Info("failed to record interactor tuple in status; SpiceDB write already landed, will re-attempt next reconcile",
			"session", sess.Namespace+"/"+sess.Name, "subject", subjectRef, "err", err.Error())
		// Roll back the in-memory append so the rest of THIS reconcile doesn't
		// believe the marker landed when the write that was to persist it failed.
		sess.Status.InteractorTuplesWritten = before
	}
}

// touchTriggerOwner writes the ADDITIONAL owner a triggered session's inbound
// named — the PR author as github_user:<id>#user, carried on
// AnnotationTriggerOwnerSubject by channelsd from a verified delivery. Written
// after, and only after, the policy owner above: additive, never a substitute
// for the approval anchor.
//
// The value is re-validated here, fail-closed, before it becomes an
// authorization write: it must be a well-formed <type>:<id>#<relation>
// subject-set with a concrete (non-wildcard) id, and its type#relation must be
// one a registered channel kind links into agentsession
// (chregistry.SessionRelationLinks — the same list the guardian composes into
// owner/participant/denied, so what is written is exactly what the schema
// admits). A refused value is logged with the reason and skipped; the session
// keeps its policy owner.
func (r *Reconciler) touchTriggerOwner(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ref authz.SessionRef) error {
	raw := sess.Annotations[spiceboxv1alpha1.AnnotationTriggerOwnerSubject]
	if raw == "" {
		return nil
	}
	if reason := triggerOwnerSubjectInvalid(raw, chregistry.SessionRelationLinks()); reason != "" {
		log.FromContext(ctx).Info("trigger-owner annotation refused; session keeps its policy owner only",
			"session", sess.Namespace+"/"+sess.Name, "subject", raw, "reason", reason)
		return nil
	}
	if err := authz.TouchOwner(ctx, r.AuthzGranter, ref, raw); err != nil {
		// Logged here, not propagated: the policy owner above already landed,
		// and the caller's error path says "session left ownerless", which
		// would be false for this half. The expected transient is a rolling
		// upgrade where the guardian has not yet composed github_user#user
		// into the live schema; the write is a TOUCH re-run on every
		// reconcile, so it heals as soon as the schema lands.
		log.FromContext(ctx).Info("trigger-owner grant failed; session keeps its policy owner, retried next reconcile",
			"session", sess.Namespace+"/"+sess.Name, "subject", raw, "err", err.Error())
	}
	return nil
}

// triggerOwnerSubjectInvalid reports why raw is not writable as a trigger
// owner, or "" when it is. Pure; the link list is passed in so the rule is
// testable without the registry.
func triggerOwnerSubjectInvalid(raw string, links []string) string {
	typ, rest, ok := strings.Cut(raw, ":")
	if !ok || typ == "" {
		return "not a <type>:<id>#<relation> subject reference"
	}
	id, rel, ok := strings.Cut(rest, "#")
	if !ok || rel == "" {
		return "not a subject-set (missing #relation)"
	}
	if id == "" {
		return "empty object id"
	}
	if strings.Contains(id, "*") {
		return "wildcard object id"
	}
	if !slices.Contains(links, typ+"#"+rel) {
		return "type#relation is not a channel-kind session link"
	}
	return ""
}

// outputChannelGroupRef returns the membership subject-set of the Channel this
// session's output lands in, or "" when there is none to be had.
//
// Asked unconditionally of the Channel rather than gated on
// spec.owner.ownerless.fromOutputChannel: a Channel that declared nothing at all
// is exactly the case the derivation exists for, and by the time the caller
// reaches here every declared owner source has already won. What IS gated is the
// class's ownerCeiling — the admin veto ResolveOwnerSubject never sees, and which
// a derived owner (never written into a Channel's spec) would otherwise slip
// past entirely.
func (r *Reconciler) outputChannelGroupRef(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, inputCh *spiceboxv1alpha1.Channel, ac *spiceboxv1alpha1.AgentClass) string {
	if !spiceboxv1alpha1.OwnerMayComeFromOutputChannel(ac.Spec.GetOwnerCeiling()) {
		return ""
	}
	outCh := inputCh // OutputChannel nil ⇒ same channel serves as both input and output
	if sess.Spec.OutputChannel != nil {
		var c spiceboxv1alpha1.Channel
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Spec.OutputChannel.Name}, &c); err == nil {
			outCh = &c
		} else {
			log.FromContext(ctx).Info("outputChannelGroupRef: failed to get output channel; falling back to input channel",
				"session", sess.Namespace+"/"+sess.Name,
				"outputChannel", sess.Spec.OutputChannel.Name,
				"err", err.Error())
		}
	}
	return chregistry.OwnerGroupRefForChannel(outCh)
}
