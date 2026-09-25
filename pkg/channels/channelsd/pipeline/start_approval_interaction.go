// pkg/channels/channelsd/pipeline/start_approval_interaction.go
//
// The bound decision handler for the start_approval interaction category —
// the platform admin's verdict on a session an org non-member started.
// Approve writes the standing the parked create deliberately withheld
// (started_by for the guest, the class's interact-policy participant) and
// removes the parking marker annotation, which is the operator's unpark
// signal. Deny writes the denied blocklist relation and sets the
// StartFailure signal the operator drives to Failed/StartDenied. Either way
// the durable PendingRequesters entry is cleared.
//
// Standing is NOT re-checked here: HandleInteractionDecision already ran the
// category's DeciderPolicy (start_approval = DecidePlatformAdmin →
// platform#start_session) before invoking this handler.
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
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// BindStartApprovalHandler wires decideStartApproval into the interaction
// registry as the bound decision handler for categories.StartApproval.
// Called once at channelsd process start (internal/cmd/channelsd/main.go) AND by the
// e2e harness's own wiring site — mirrors BindPermissionHandler.
func BindStartApprovalHandler(p *Pipeline) {
	channelinteractions.Bind(categories.StartApproval, p.decideStartApproval)
}

// decideStartApproval applies a resolved start_approval decision. A
// requestRef with no matching PendingRequesters entry (already decided, or
// expired) renders the resolved outcome without touching SpiceDB or status.
func (p *Pipeline) decideStartApproval(ctx context.Context, d channelinteractions.Decision) (channelinteractions.Outcome, error) {
	ns, name := d.Session.Namespace, d.Session.Name
	var sess spiceboxv1alpha1.AgentSession
	if err := p.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		return channelinteractions.Outcome{}, fmt.Errorf("start-approval decision: get session %s/%s: %w", ns, name, err)
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
		// Stale/duplicate click: an earlier decision (or the timeout sweep)
		// already resolved this entry. Render the outcome; write nothing.
		return channelinteractions.Outcome{Result: result, OutcomeText: text}, nil
	}
	pr := sess.Status.PendingRequesters[idx]
	guest := requesterCanonical(pr)
	ref := authz.SessionRef{Namespace: ns, Name: name}

	updated := sess.DeepCopy()
	if approve {
		// The two standing writes the parked create withheld, in the same
		// shapes the ordinary create path uses.
		if err := p.Engine.TouchStartedBy(ctx, ref, guest); err != nil {
			return channelinteractions.Outcome{}, fmt.Errorf("start-approval decision: write started_by on %s/%s: %w", ns, name, err)
		}
		if interactPerm := p.effectiveInteractPermission(ctx, &sess); interactPerm != "" {
			if err := p.Engine.TouchInteractParticipant(ctx, ref, interactPerm); err != nil {
				// Non-fatal, exactly as on the create path: the guest's own
				// standing (started_by) is written, so the session can run;
				// the policy write is recorded as failed on the condition.
				log.FromContext(ctx).Info("start-approval decision: interact-policy participant write failed; condition recorded",
					"session", ns+"/"+name, "subject", interactPerm, "err", err.Error())
				conditions.SetFalse(updated, &updated.Status.Conditions,
					spiceboxv1alpha1.AgentSessionConditionInteractPolicyApplied,
					spiceboxv1alpha1.ReasonInteractPolicySpiceDBWriteFailed, err.Error())
			} else {
				updated.Status.AppliedInteractPermission = interactPerm
				now := metav1.NewTime(p.Now())
				updated.Status.AppliedInteractPermissionAt = &now
				conditions.SetTrue(updated, &updated.Status.Conditions,
					spiceboxv1alpha1.AgentSessionConditionInteractPolicyApplied,
					spiceboxv1alpha1.ReasonInteractPolicyApplied)
			}
		}
	} else {
		// The denied relation makes the guest's next message in this thread
		// silently drop at the blocklist gate rather than re-minting cards.
		if err := p.denyInteract(ctx, ns, name, guest); err != nil {
			return channelinteractions.Outcome{}, fmt.Errorf("start-approval decision: write denied relation on %s/%s: %w", ns, name, err)
		}
		// The operator's StartFailure consumer drives Failed/StartDenied —
		// channelsd sets the signal, never the terminal phase itself.
		updated.Status.StartFailure = &spiceboxv1alpha1.AgentSessionStartFailure{
			Reason:  spiceboxv1alpha1.ReasonAgentSessionStartDenied,
			Message: "a platform admin denied the start-approval request",
		}
	}

	updated.Status.PendingRequesters = append(updated.Status.PendingRequesters[:idx], updated.Status.PendingRequesters[idx+1:]...)
	conditions.Set(updated, &updated.Status.Conditions, metav1.Condition{
		Type:   spiceboxv1alpha1.AgentSessionConditionStartApprovalPending,
		Status: metav1.ConditionFalse,
		Reason: "Decided",
	})
	if err := applyApprovalStatus(ctx, p.K8s, updated, &sess); err != nil {
		return channelinteractions.Outcome{}, fmt.Errorf("start-approval decision: clear pending requester on %s/%s: %w", ns, name, err)
	}

	// Remove the parking marker LAST, after every durable record of the
	// decision is in place: the moment it disappears the operator unparks
	// (approve) — an early removal could start a session whose decision
	// bookkeeping then failed to land. Removed on deny too: the marker means
	// "a request is pending", and none is.
	unmarked := updated.DeepCopy()
	delete(unmarked.Annotations, spiceboxv1alpha1.AnnotationStartApprovalRequestRef)
	if err := p.K8s.Patch(ctx, unmarked, client.MergeFrom(updated)); err != nil {
		return channelinteractions.Outcome{}, fmt.Errorf("start-approval decision: remove parking marker on %s/%s: %w", ns, name, err)
	}
	return channelinteractions.Outcome{Result: result, OutcomeText: text}, nil
}

// effectiveInteractPermission reads the session's class and answers its
// EffectiveSessionInteractPermission — the same value the ordinary create
// path snapshots. A lookup miss degrades to "" (no policy write) with a log,
// matching the create path's tolerance.
func (p *Pipeline) effectiveInteractPermission(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) string {
	if sess.Spec.Class == "" {
		return ""
	}
	var class spiceboxv1alpha1.AgentClass
	if err := p.K8s.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Spec.Class}, &class); err != nil {
		log.FromContext(ctx).Info("start-approval decision: AgentClass lookup miss; skipping interact-policy write",
			"session", sess.Namespace+"/"+sess.Name, "class", sess.Spec.Class, "err", err.Error())
		return ""
	}
	return class.EffectiveSessionInteractPermission()
}
