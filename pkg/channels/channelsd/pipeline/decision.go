// pkg/channels/channelsd/pipeline/decision.go
//
// The TTL-expiry leg of the permission_request (session-join) flow.
//
// The decision leg lives elsewhere: an Approve/Deny click arrives as a
// category-generic KindInteractionDecision, which HandleInteractionDecision
// (interaction_decision.go) routes to decidePermission
// (permission_interaction.go), the bound handler for
// categories.PermissionRequest. grantInteract, denyInteract and
// requesterCanonical below are the SpiceDB-write and canonicalization helpers
// that handler shares, so the grant is written in exactly one place.
package pipeline

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/x/besteffort"
)

// grantInteract writes the per-user interact participant relation in SpiceDB
// for an approved session-join request. Shared with decidePermission so the
// grant call is written once rather than re-derived per caller.
func (p *Pipeline) grantInteract(ctx context.Context, ns, name string, canonical identity.CanonicalUserID) error {
	return p.Engine.TouchInteractParticipantUser(ctx, authz.SessionRef{Namespace: ns, Name: name}, canonical)
}

// denyInteract writes the per-user denied-blocklist relation in SpiceDB for a
// declined session-join request, shared with decidePermission.
//
// The schema's `interact = owner + started_by + participant - denied` subtracts
// denied LAST, so a denied tuple overrides every grant source — a class-wide
// grant, a prior approval, even owner/started_by standing. handlePermissionDeny's
// blocklist gate reads this same relation back on the requester's next message;
// without the write both that gate and the PendingRequesters dedup are empty,
// and the pipeline re-mints an interaction_request and re-DMs the owner on every
// subsequent message from someone they already denied.
func (p *Pipeline) denyInteract(ctx context.Context, ns, name string, canonical identity.CanonicalUserID) error {
	return p.Engine.TouchDeniedUser(ctx, authz.SessionRef{Namespace: ns, Name: name}, canonical)
}

// requesterCanonical derives the canonical SpiceDB subject for a durable
// PendingRequester entry. A PendingRequester carries the same (Kind, TeamScope,
// ExternalID, Email) tuple the original inbound message did, so this reuses the
// package's canonicalID helper — AllowSynthetic-opted-in, so a guest or
// foreign-workspace requester with no verified email resolves to the same
// synthetic subject the inbound pipeline minted for them.
func requesterCanonical(pr spiceboxv1alpha1.PendingRequester) identity.CanonicalUserID {
	return canonicalID(channelkinds.ExternalIdentity{
		Kind:       identity.Kind(pr.Kind),
		TeamScope:  identity.TeamScope(pr.TeamScope),
		ExternalID: identity.RawExternalID(pr.ExternalID),
		Email:      identity.Email(pr.Email),
	})
}

// TimeoutPermissionRequest expires a single ad-hoc join request that has sat
// undecided past its TTL. A join is pre-runner (the session is Idle), so no
// runner orchestrator is parked on it — channelsd owns its lifetime. Matched
// by RequestRef (unique per request) so a fresh re-request from the same user
// that arrived after the sweep snapshot is left untouched. On expiry it removes
// the PendingRequester, recomputes the PermissionRequestPending condition, and
// publishes a generic interaction_applied (Outcome=Expired) so the channel
// kind edits the requester's prompt to "expired". Deliberately does NOT write a
// SpiceDB denied relation: a lapsed request is not an owner's rejection, and the
// user may ask again. A missing match (already decided, or the fresh-request
// case above) is a no-op.
func (p *Pipeline) TimeoutPermissionRequest(ctx context.Context, sessNS, sessName, requestRef string) error {
	logger := log.FromContext(ctx).WithValues(
		"session", sessNS+"/"+sessName, "requestRef", requestRef, "reason", "timeout")

	var sess spiceboxv1alpha1.AgentSession
	key := client.ObjectKey{Namespace: sessNS, Name: sessName}
	if err := p.K8s.Get(ctx, key, &sess); err != nil {
		return fmt.Errorf("get agentsession %s: %w", key, err)
	}

	updated := sess.DeepCopy()
	matchedIdx := -1
	for i, pr := range updated.Status.PendingRequesters {
		if pr.RequestRef == requestRef {
			matchedIdx = i
			break
		}
	}
	if matchedIdx < 0 {
		// Already decided by an owner click, or the entry was replaced by a
		// newer request from the same user. Nothing to expire.
		return nil
	}
	matched := updated.Status.PendingRequesters[matchedIdx]
	updated.Status.PendingRequesters = append(
		updated.Status.PendingRequesters[:matchedIdx],
		updated.Status.PendingRequesters[matchedIdx+1:]...,
	)

	if len(updated.Status.PendingRequesters) == 0 {
		conditions.Set(updated, &updated.Status.Conditions, metav1.Condition{
			Type:   spiceboxv1alpha1.AgentSessionConditionPermissionRequestPending,
			Status: metav1.ConditionFalse,
			Reason: "NoneOutstanding",
		})
	} else {
		conditions.Set(updated, &updated.Status.Conditions, metav1.Condition{
			Type:    spiceboxv1alpha1.AgentSessionConditionPermissionRequestPending,
			Status:  metav1.ConditionTrue,
			Reason:  "RequesterPending",
			Message: fmt.Sprintf("%d pending request(s)", len(updated.Status.PendingRequesters)),
		})
	}
	if err := applyApprovalStatus(ctx, p.K8s, updated, &sess); err != nil {
		// The status patch failed, but still tell the kind the request lapsed
		// so the requester's prompt isn't left hanging; the next sweep retries
		// the clear.
		besteffort.Log(logger.Info, "publish permission timeout applied (status-patch-failed branch)",
			p.publishPermissionTimeoutApplied(matched, key))
		return fmt.Errorf("apply agentsession approval status: %w", err)
	}
	logger.Info("expired pending join request past TTL",
		"requester", matched.Kind+":"+matched.ExternalID,
		"remaining", len(updated.Status.PendingRequesters))
	return p.publishPermissionTimeoutApplied(matched, key)
}

// publishPermissionTimeoutApplied emits a generic interaction_applied
// envelope (category permission_request, Outcome=Expired) for an expired
// join request, so the generic Slack interaction sender's sendDecisionApplied
// edits the requester's prompt to "expired" rather than attributing a
// rejection to an owner who never acted. DecidedBy is left nil — no one
// decided — and ResponseRef is left empty since there was no click; the
// sender falls back to its recorded delivery ref instead. Published on the
// OUT subject only: a join is pre-runner, so there is no runner-side resume
// to unblock (contrast identity_choice's timeout, which the runner itself
// consumes on IN).
func (p *Pipeline) publishPermissionTimeoutApplied(pr spiceboxv1alpha1.PendingRequester, key client.ObjectKey) error {
	// The entry's own raised-under category (start_approval rides the same
	// list); entries written before the field existed are join requests.
	category := pr.Category
	if category == "" {
		category = categories.PermissionRequest
	}
	applied := channelevents.InteractionAppliedPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: key.Namespace, Name: key.Name},
		Category:        category,
		RequestRef:      pr.RequestRef,
		Outcome:         channelevents.OutcomeExpired,
		OutcomeText:     "expired",
	}
	return channelevents.PublishOut(p.NATS.Publish, key.Namespace, key.Name,
		channelevents.KindInteractionApplied, applied)
}
