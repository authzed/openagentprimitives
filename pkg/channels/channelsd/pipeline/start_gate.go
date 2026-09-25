// pkg/channels/channelsd/pipeline/start_gate.go
//
// The session-START gate: on a channel whose kind attributes org membership
// (channelkinds.OrgMembershipAttributor), a brand-new session may be created
// freely only by an org MEMBER. Anyone else — a stamped guest, or an identity
// an attributing kind failed to stamp (fail-closed) — must hold
// agentclass#start_session; without it the session is created PARKED
// (AnnotationStartApprovalRequestRef, phase AwaitingStartApproval via the
// operator), no started_by tuple is written, and a start_approval
// interaction_request is DM'd to the platform admins
// (platform#start_session holders). The admin's decision is applied by
// decideStartApproval.
//
// The gate deliberately does NOT run for: org members (today's behavior,
// untouched), non-human/service subjects (governed by the existing
// authzSubject paths), and channels whose kind does not attribute membership
// (they have no org to be a guest of). An EXPLICIT guest stamp gates even on
// a non-attributing channel — nothing legitimate stamps guest and expects a
// free pass.
package pipeline

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// platformStartSessionSubject is the subject-set whose members may approve a
// guest's start request AND whose standing the decision pipe re-checks on the
// click: the platform-admin arm of the session-start area
// (platform#start_session = can_admin). Deliberately NOT the class's own
// agentclass#start_session — its `starter` relation is the per-guest override,
// and a guest allowed to start must not thereby admit other guests.
const platformStartSessionSubject = "platform:platform#start_session"

// startGateApplies reports whether the org-membership start gate governs this
// inbound. Pure; the SpiceDB check happens in evaluateStartGate.
func startGateApplies(ev channelkinds.InboundEvent, nonHumanSubject bool) bool {
	if nonHumanSubject {
		return false
	}
	switch ev.ExternalIDs.OrgMembership {
	case channelkinds.OrgMembershipMember:
		return false
	case channelkinds.OrgMembershipGuest:
		return true
	}
	// Empty stamp: gate exactly when the channel's kind claims to attribute
	// membership — for such a channel a missing stamp is a dropped signal,
	// and a dropped signal must gate, never widen (fail-closed).
	if ev.Channel == nil {
		return false
	}
	k, ok := chregistry.Get(ev.Channel.Spec.Kind)
	if !ok {
		return false
	}
	a, ok := k.(channelkinds.OrgMembershipAttributor)
	return ok && a.AttributesOrgMembership(ev.Channel)
}

// startGateVerdict is evaluateStartGate's answer for a gated inbound.
type startGateVerdict struct {
	// allowed: the subject holds agentclass#start_session — proceed exactly
	// as an ungated create.
	allowed bool
	// adminSubjects are the platform admins' canonical user ids (unprefixed),
	// resolved only when the create must park; the approval card fans out to
	// them.
	adminSubjects []string
}

// evaluateStartGate answers "may this non-member start a session of this
// class, and if not, who can approve it?" — fail-closed on every uncertain
// path: no authz client, an unrepresentable class id, a check error, and a
// LookupSubjects error all refuse rather than create.
func (p *Pipeline) evaluateStartGate(ctx context.Context, ev channelkinds.InboundEvent, canonical identity.CanonicalUserID) (startGateVerdict, error) {
	if p.Authz == nil {
		return startGateVerdict{}, fmt.Errorf("start gate on channel %s: cannot verify agentclass#start_session (no authz client); refusing fail-closed",
			channelRefForLog(ev.Channel))
	}
	classID, err := spicedb.AgentClassObjectID(ev.Channel.Namespace, ev.Channel.Spec.AgentClass)
	if err != nil {
		return startGateVerdict{}, fmt.Errorf("start gate: compose agentclass id: %w", err)
	}
	// FullyConsistent: an admin may have granted `starter` moments before the
	// guest's retry, and one consistent check per NEW session is cheap.
	allowed, err := p.Authz.CheckOnResource(ctx, "agentclass", classID, "start_session", canonical, true)
	if err != nil {
		return startGateVerdict{}, fmt.Errorf("start gate: check agentclass:%s#start_session: %w", classID, err)
	}
	if allowed {
		return startGateVerdict{allowed: true}, nil
	}
	admins, err := p.Authz.LookupSubjects(ctx, platformStartSessionSubject)
	if err != nil {
		return startGateVerdict{}, fmt.Errorf("start gate: resolve platform admins (%s): %w", platformStartSessionSubject, err)
	}
	return startGateVerdict{adminSubjects: admins}, nil
}

// startAdminApprovers renders the platform admins' canonical ids as
// Subject-passthrough identities for the approval card's audience. The
// surface (Slack's approver resolver, the fake kind's recorder) derives
// delivery addressing from the canonical subject.
func startAdminApprovers(adminSubjects []string) []channelevents.ExternalIdentity {
	out := make([]channelevents.ExternalIdentity, 0, len(adminSubjects))
	for _, id := range adminSubjects {
		out = append(out, channelevents.ExternalIdentity{Subject: identity.Subject("user:" + id)})
	}
	return out
}

// refuseUnstartableGuest handles the parked-create dead end: a guest needs an
// approval and no platform admin is resolvable to approve it. Refuse cleanly
// (no session, no orphan pending record), tell the guest, and tell the
// monitoring channel — this needs a human with cluster access, not a retry.
func (p *Pipeline) refuseUnstartableGuest(ctx context.Context, ev channelkinds.InboundEvent) channelkinds.InboundDecision {
	log.FromContext(ctx).Info("start gate: refusing guest session start — no addressable platform admin to approve it",
		"channel", channelRefForLog(ev.Channel),
		"requester", ev.ExternalIDs.ExternalID.String(),
		"hint", "grant a platform admin (oap platform grant-admin) or grant this user agentclass#starter")
	p.publishUnstartableGuestMonitoring(ctx, ev)
	return channelkinds.InboundDecision{
		Outcome: channelkinds.OutcomeDeniedByPermission,
		Notice: notice.New(categories.StartNoAdmin, notice.Args{
			Lead:     "Can't start a session for you",
			Body:     "Starting a session here needs an admin's approval, and no admin is reachable to ask.",
			NextStep: "A platform admin has been notified — ask them to grant you access.",
			Audience: participantsAudience(),
		}),
	}
}

// publishUnstartableGuestMonitoring mirrors publishUnapprovableJoinMonitoring
// for the start gate's dead end. Sourced from the CHANNEL — no session exists
// on this path, and none will until an admin exists to approve one.
func (p *Pipeline) publishUnstartableGuestMonitoring(ctx context.Context, ev channelkinds.InboundEvent) {
	if p.NATS == nil || ev.Channel == nil {
		return
	}
	mev := channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelWarning,
		Category:   "session",
		Transition: channelevents.MonitoringTransitionFailed,
		Source: channelevents.MonitoringSourceRef{
			Kind:      "Channel",
			Namespace: ev.Channel.Namespace,
			Name:      ev.Channel.Name,
		},
		Condition: spiceboxv1alpha1.AgentSessionConditionStartApprovalPending,
		Reason:    "NoAddressablePlatformAdmin",
		// The canonical subject, not the raw channel id, for the same reason
		// as the unapprovable-join event: an admin acting on this needs a
		// value they can paste into a grant.
		Summary: fmt.Sprintf("user:%s (an org non-member) tried to start a session of class %s/%s and no platform admin is resolvable to approve it.",
			canonicalID(ev.ExternalIDs).String(), ev.Channel.Namespace, ev.Channel.Spec.AgentClass),
		Hint:      "grant a platform admin (`oap platform grant-admin <user>`) so guest start requests have an approver, or grant this user agentclass#starter directly",
		Timestamp: p.Now(),
	}
	if err := channelevents.PublishMonitoring(p.NATS.Publish, mev); err != nil {
		log.FromContext(ctx).Info("start gate: publish unstartable-guest monitoring event failed",
			"channel", channelRefForLog(ev.Channel), "err", err.Error())
	}
}

// handleParkedStartApproval intercepts an inbound routed to a session parked
// awaiting start approval, BEFORE the interact check: nobody holds standing
// on such a session yet, so the join flow's approver seat (started_by) has no
// tuple and every path through it misfires. handled=false only when the
// session is not parked.
//
//   - The STARTER re-messaging their thread: suppressed when their pending
//     entry exists (the card's public note already said "awaiting approval");
//     when it does NOT — the crash window between the Create's annotation and
//     the status patch — the request is RE-RAISED under the annotation's own
//     requestRef, so the durable record and any already-rendered card agree.
//   - Anyone else: refused with a notice. No join card, no denied tuple — a
//     bystander asking about a parked thread is not a permission event.
func (p *Pipeline) handleParkedStartApproval(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	ev channelkinds.InboundEvent,
) (channelkinds.InboundDecision, bool, error) {
	if !spiceboxv1alpha1.StartApprovalPending(sess) {
		return channelkinds.InboundDecision{}, false, nil
	}
	requestRef := spiceboxv1alpha1.StartApprovalRequestRef(sess)
	starter := spiceboxv1alpha1.StartedBySubject(sess)
	requester := canonicalID(ev.ExternalIDs).Subject()
	if starter != "" && requester == starter {
		for _, pr := range sess.Status.PendingRequesters {
			if pr.RequestRef == requestRef {
				return channelkinds.InboundDecision{
					Outcome: channelkinds.OutcomeDeniedByPermission,
					Notice:  notice.Suppressed("the starter's start-approval request is already pending; re-posting the wait would be spam"),
				}, true, nil
			}
		}
		// Crash window: the annotation was stamped on the Create but the
		// status patch (or the publish) never landed. Re-resolve the admins
		// and re-raise under the SAME requestRef.
		if p.Authz == nil {
			return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError}, true,
				fmt.Errorf("start gate: cannot re-raise parked %s/%s (no authz client); refusing fail-closed", sess.Namespace, sess.Name)
		}
		admins, err := p.Authz.LookupSubjects(ctx, platformStartSessionSubject)
		if err != nil {
			return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError}, true,
				fmt.Errorf("start gate: re-resolve platform admins for parked %s/%s: %w", sess.Namespace, sess.Name, err)
		}
		if len(admins) == 0 {
			return p.refuseUnstartableGuest(ctx, ev), true, nil
		}
		log.FromContext(ctx).Info("start gate: re-raising a parked session's lost start-approval request",
			"session", sess.Namespace+"/"+sess.Name, "requestRef", requestRef)
		dec, err := p.finalizeStartApproval(ctx, sess, ev, admins, requestRef)
		return dec, true, err
	}
	return channelkinds.InboundDecision{
		Outcome: channelkinds.OutcomeDeniedByPermission,
		Notice: notice.New(categories.StartAwaitingApproval, notice.Args{
			Lead:     "This session hasn't started yet",
			Body:     "It's waiting for an admin to approve the person who opened it.",
			NextStep: "Check back once it's approved, or start your own thread.",
			Audience: participantsAudience(),
		}),
	}, true, nil
}

// finalizeStartApproval runs after a PARKED session's Create: stamp the
// durable PendingRequesters entry + condition (verified readback, same
// stale-CRD guard as the join flow), then publish the start_approval card to
// the platform admins. Ordering contract shared with handlePermissionDeny:
// the durable record an Approve click resolves against must exist before any
// approver can see the button.
func (p *Pipeline) finalizeStartApproval(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	ev channelkinds.InboundEvent,
	adminSubjects []string,
	requestRef string,
) (channelkinds.InboundDecision, error) {
	updated := sess.DeepCopy()
	updated.Status.PendingRequesters = append(updated.Status.PendingRequesters,
		spiceboxv1alpha1.PendingRequester{
			Kind:       ev.ExternalIDs.Kind.String(),
			TeamScope:  ev.ExternalIDs.TeamScope.String(),
			ExternalID: ev.ExternalIDs.ExternalID.String(),
			Email:      ev.ExternalIDs.Email.String(),
			RequestRef: requestRef,
			Category:   categories.StartApproval,
			// MessageText deliberately empty: the guest's message is already
			// the parked session's spec.Prompt (turn 0), so approval replays
			// nothing — unparking lets the runner cold-start on it.
			RequestedAt: metav1.NewTime(p.Now()),
			ChannelKey:  ev.ChannelKey,
		})
	conditions.SetTrue(updated, &updated.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionStartApprovalPending, "AwaitingPlatformAdmin")
	if err := applyApprovalStatus(ctx, p.K8s, updated, sess); err != nil {
		return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
			fmt.Errorf("apply start-approval status on %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	// Same stale-CRD guard as the join flow: the apiserver silently prunes
	// fields the installed CRD does not declare, and an Approve with nothing
	// behind it does nothing.
	var verify spiceboxv1alpha1.AgentSession
	if err := p.K8s.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &verify); err != nil {
		return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
			fmt.Errorf("verify start-approval PendingRequesters readback on %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	if !pendingRequestersContains(verify.Status.PendingRequesters, ev.ExternalIDs.Kind.String(), ev.ExternalIDs.ExternalID.String()) {
		return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
			fmt.Errorf("status.pendingRequesters did not persist after patch on %s/%s — likely a stale CRD: re-apply config/crds/agentprimitives.authzed.com_agentsessions.yaml",
				sess.Namespace, sess.Name)
	}

	req := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Category:        categories.StartApproval,
		RequestRef:      requestRef,
		Lead:            "Session start request",
		Body: fmt.Sprintf("Someone from outside your org asked to start a session of agent %q. "+
			"They can't start sessions on their own; approve to run this one session, or deny to block them.", sess.Spec.Class),
		Fields:  []channelevents.InteractionField{requesterField(ev.ExternalIDs)},
		Excerpt: requesterMessageExcerpt(ev.MessageText),
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:          channelevents.AudienceApprovers,
			Approvers:      startAdminApprovers(adminSubjects),
			PublicNote:     true,
			PublicNoteBody: "⏳ Approval needed — starting a session here needs an admin's approval. They've been asked.",
		},
	}
	if err := req.Validate(); err != nil {
		return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
			fmt.Errorf("built invalid start_approval interaction_request (session %s/%s): %w", sess.Namespace, sess.Name, err)
	}
	if err := channelevents.PublishOut(p.NATS.Publish, sess.Namespace, sess.Name,
		channelevents.KindInteractionRequest, req); err != nil {
		return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError},
			fmt.Errorf("publish start_approval interaction_request (session %s/%s): %w", sess.Namespace, sess.Name, err)
	}

	// Suppressed for the same reason as the join flow: the interaction
	// sender's public note is the one user-facing message, and a listener
	// notice here would double-report it.
	return channelkinds.InboundDecision{
		Outcome:              channelkinds.OutcomeDeniedByPermission,
		RequesterCanonicalID: canonicalID(ev.ExternalIDs).String(),
		Notice:               notice.Suppressed("the start_approval interaction's public note already told the requester"),
	}, nil
}
