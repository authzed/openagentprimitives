// pkg/channels/channelsd/pipeline/permission_interaction.go
//
// The bound decision handler for the permission_request interaction category
// (the session-join grant), registered in channelinteractions. Approve grants
// the requester `interact` via grantInteract; deny blocks them via denyInteract,
// writing the SpiceDB denied relation the blocklist gate reads back. Both clear
// the durable PendingRequesters entry handlePermissionDeny stamped. Idempotent
// on that entry, so a duplicate or stale click is a no-op spectator that still
// renders the resolved outcome.
//
// Standing is NOT re-checked here: HandleInteractionDecision already ran the
// category's DeciderPolicy check (permission_request = DecideOwner → the
// session's approve-set) before invoking this handler, whose only job is to
// apply the decision.
//
// Approve also RESUBMITS: decidePermission's resubmit step replays the
// requester's stashed message through the inbound pipeline after granting
// interact, so the user does not have to retype once the Approve click lands.
// The end-to-end contract is grant → replay → agent responds.
package pipeline

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// BindPermissionHandler wires decidePermission into the interaction
// registry as the bound decision handler for categories.PermissionRequest.
// Called once at channelsd process start (internal/cmd/channelsd/main.go), after
// the pipeline pl is constructed — mirrors the identity_choice Bind site.
func BindPermissionHandler(p *Pipeline) {
	channelinteractions.Bind(categories.PermissionRequest, p.decidePermission)
}

// decidePermission applies a resolved permission_request decision: on
// approve it grants the requester `interact` via grantInteract, on deny
// it blocks the requester via denyInteract; either way it clears the
// matching PendingRequesters entry and recomputes the
// PermissionRequestPending condition. A requestRef with no matching
// PendingRequesters entry (already resolved by an earlier click, or
// expired via TimeoutPermissionRequest) is a stale/duplicate click: it
// renders the resolved outcome without touching SpiceDB or status again.
func (p *Pipeline) decidePermission(ctx context.Context, d channelinteractions.Decision) (channelinteractions.Outcome, error) {
	ns, name := d.Session.Namespace, d.Session.Name
	var sess spiceboxv1alpha1.AgentSession
	if err := p.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		return channelinteractions.Outcome{}, fmt.Errorf("permission decision: get session %s/%s: %w", ns, name, err)
	}

	approve := d.Payload.ActionID == "approve"
	result, text := channelevents.OutcomeDenied, "Denied"
	if approve {
		result, text = channelevents.OutcomeApproved, "Approved"
	}

	idx := -1
	for i, pr := range sess.Status.PendingRequesters {
		if pr.RequestRef == d.Payload.RequestRef {
			idx = i
			break
		}
	}
	if idx < 0 {
		// Stale/duplicate: the entry was already resolved (an earlier
		// click, or a TTL expiry) or never existed for this requestRef.
		// Render the resolved outcome so the clicker's surface still
		// reflects a sane state, but do NOT re-grant or touch status —
		// the earlier resolution is authoritative.
		return channelinteractions.Outcome{Result: result, OutcomeText: text}, nil
	}

	pr := sess.Status.PendingRequesters[idx]
	if approve {
		if err := p.grantInteract(ctx, ns, name, requesterCanonical(pr)); err != nil {
			return channelinteractions.Outcome{}, fmt.Errorf("permission decision: grant interact on %s/%s: %w", ns, name, err)
		}
	} else {
		// Write the SpiceDB denied relation so handlePermissionDeny's blocklist
		// gate silently drops their next message, instead of re-minting an
		// interaction_request and re-DMing the owner forever.
		if err := p.denyInteract(ctx, ns, name, requesterCanonical(pr)); err != nil {
			return channelinteractions.Outcome{}, fmt.Errorf("permission decision: write denied relation on %s/%s: %w", ns, name, err)
		}
	}

	cp := sess.DeepCopy()
	cp.Status.PendingRequesters = append(cp.Status.PendingRequesters[:idx], cp.Status.PendingRequesters[idx+1:]...)
	if len(cp.Status.PendingRequesters) == 0 {
		conditions.Set(cp, &cp.Status.Conditions, metav1.Condition{
			Type:   spiceboxv1alpha1.AgentSessionConditionPermissionRequestPending,
			Status: metav1.ConditionFalse,
			Reason: "AllResolved",
		})
	} else {
		conditions.Set(cp, &cp.Status.Conditions, metav1.Condition{
			Type:    spiceboxv1alpha1.AgentSessionConditionPermissionRequestPending,
			Status:  metav1.ConditionTrue,
			Reason:  "AwaitingApproval",
			Message: fmt.Sprintf("%d join request(s) pending", len(cp.Status.PendingRequesters)),
		})
	}
	if err := applyApprovalStatus(ctx, p.K8s, cp, &sess); err != nil {
		return channelinteractions.Outcome{}, fmt.Errorf("permission decision: clear pending requester on %s/%s: %w", ns, name, err)
	}

	// Auto-resubmit on approve: replay the requester's stashed message through
	// the inbound pipeline so they need not retype after the Approve click.
	// MessageText is empty when the original exceeded handlePermissionDeny's
	// maxStashedText guard, in which case there is nothing to replay and the
	// requester re-sends. Skips the permission check: the participant relation
	// was just written here, and MinimizeLatency may not yet reflect it.
	if approve && pr.MessageText != "" {
		ev := channelkinds.InboundEvent{
			ExternalIDs: channelkinds.ExternalIdentity{
				Kind:       identity.Kind(pr.Kind),
				TeamScope:  identity.TeamScope(pr.TeamScope),
				ExternalID: identity.RawExternalID(pr.ExternalID),
				Email:      identity.Email(pr.Email),
			},
			ChannelKey:  pr.ChannelKey,
			MessageText: pr.MessageText,
		}
		if err := p.ResubmitAuthorized(ctx, cp, ev); err != nil {
			// The approve already succeeded (interact granted, PendingRequester
			// cleared) — don't fail the decision over a resubmit hiccup, but log
			// loudly per AGENTS.md's no-silent-errors rule: the user's
			// experience degrades to "click approve, message lost — retry by
			// re-sending" and an operator should be able to find this in logs.
			log.FromContext(ctx).Error(err, "permission decision: approve resubmit failed; requester must retype",
				"session", ns+"/"+name, "requester", pr.Kind+":"+pr.ExternalID, "messageLen", len(pr.MessageText))
		}
	}

	return channelinteractions.Outcome{Result: result, OutcomeText: text}, nil
}
