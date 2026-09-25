package subagentrequest

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/handoff"
)

// bindOfferedDataSlots handles slots the parent offered AFTER the handoff —
// send_input's half — against a child that is already running.
//
// Returns done=false while a slot is still waiting on a person, which the
// caller turns into a requeue. It never blocks the child: the child is parked
// on its own request, and what it is waiting for is the DATA, not this loop.
//
// Simpler than the create path, not harder. There the child had to exist to be
// graded and then be rolled back if a slot needed a decision; here it already
// exists and stays, because a slot arriving later is the normal case rather
// than a half-built delegation.
//
// The SAME two checks in the same order, though, and that is the point: a
// parent offering data mid-flight gets no easier a path than one offering it
// up front. Attenuation asks whether this parent may delegate the tag at all;
// grading asks whether giving it to THIS child discloses, or whether the datum
// is untrusted.
func (r *Reconciler) bindOfferedDataSlots(
	ctx context.Context, sr *v1.SubagentRequest, child *v1.AgentSession,
) (bool, error) {
	pending := undeliveredSlots(sr)
	if len(pending) == 0 {
		return true, nil
	}

	parentRef := authz.SessionRef{Namespace: sr.Spec.Parent.Namespace, Name: sr.Spec.Parent.Name}
	requested := make([]authz.DataSlotBinding, 0, len(pending))
	for _, s := range pending {
		requested = append(requested, authz.DataSlotBinding{Slot: s.Slot, TagID: s.TagID})
	}
	delegable, notDelegable, err := authz.FilterDelegableDataSlots(ctx, r.Authz, parentRef, requested)
	if err != nil {
		// Unanswerable is not a yes. Returning the error retries rather than
		// binding on an envelope nobody could establish.
		return false, err
	}
	for _, b := range notDelegable {
		sr.Status.RefusedDataSlots = append(sr.Status.RefusedDataSlots, v1.RefusedDataSlot{
			Slot: b.Slot, TagID: b.TagID, Reason: v1.DataSlotNotDelegable,
		})
	}

	var grant []authz.DataSlotBinding
	var awaiting []v1.DataSlotRequest
	for _, b := range delegable {
		if slotApproved(sr, b) {
			grant = append(grant, b)
			continue
		}
		if r.Grader == nil {
			grant = append(grant, b)
			continue
		}
		grade, why, gerr := r.Grader(ctx, authz.SessionRef{Namespace: child.Namespace, Name: child.Name}, b.TagID)
		switch {
		case gerr != nil:
			log.FromContext(ctx).Info("offered data slot not granted: grading could not be completed",
				"request", sr.Namespace+"/"+sr.Name, "slot", b.Slot, "err", gerr.Error())
			sr.Status.RefusedDataSlots = append(sr.Status.RefusedDataSlots, v1.RefusedDataSlot{
				Slot: b.Slot, TagID: b.TagID, Reason: v1.DataSlotGradingUnavailable,
			})
		case grade != handoff.GradeAutoGrant:
			log.FromContext(ctx).Info("offered data slot needs a decision before it can be sent",
				"request", sr.Namespace+"/"+sr.Name, "slot", b.Slot, "reason", why)
			awaiting = append(awaiting, v1.DataSlotRequest{Slot: b.Slot, TagID: b.TagID})
		default:
			grant = append(grant, b)
		}
	}

	if len(grant) > 0 {
		if err := r.Authz.GrantDataSlots(ctx, child.Namespace, child.Name,
			grant, authz.SlotGrantExpiry(time.Now(), 0)); err != nil {
			return false, err
		}
		for _, b := range grant {
			sr.Status.BoundDataSlots = append(sr.Status.BoundDataSlots,
				v1.DataSlotRequest{Slot: b.Slot, TagID: b.TagID})
		}
		log.FromContext(ctx).Info("offered data slots bound onto a running child",
			"request", sr.Namespace+"/"+sr.Name, "count", len(grant))
	}

	sr.Status.PendingDataSlots = awaiting
	if len(awaiting) > 0 {
		if denied := deniedAmong(sr, awaiting); denied != "" {
			// A refused disclosure ends the OFFER, not the delegation: the
			// child is parked on its own request and can be told the data is
			// not coming, which is a better outcome than killing a delegation
			// that may be able to finish without it.
			log.FromContext(ctx).Info("offered data slot refused by a decider; it will not be sent",
				"request", sr.Namespace+"/"+sr.Name, "slot", denied)
			sr.Status.RefusedDataSlots = append(sr.Status.RefusedDataSlots, v1.RefusedDataSlot{
				Slot: denied, TagID: tagForSlot(awaiting, denied), Reason: v1.DataSlotWouldDisclose,
			})
			sr.Status.PendingDataSlots = nil
			return true, r.Status().Update(ctx, sr)
		}
		if sr.Status.DisclosureRequestedAt == nil {
			now := metav1.Now()
			sr.Status.DisclosureRequestedAt = &now
			var parent v1.AgentSession
			if err := r.Get(ctx, parentKey(sr), &parent); err == nil {
				r.publishDisclosureCards(ctx, sr, &parent, awaiting)
			} else {
				log.FromContext(ctx).Info("data slot disclosure: could not read the parent to publish a card",
					"request", sr.Namespace+"/"+sr.Name, "err", err.Error())
			}
		}
		return false, r.Status().Update(ctx, sr)
	}
	// Every offer settled. Clearing the clock lets a LATER offer start its own
	// wait rather than inheriting this one's, which would expire the next card
	// the moment it was published.
	sr.Status.DisclosureRequestedAt = nil
	return true, r.Status().Update(ctx, sr)
}

// undeliveredSlots returns the spec's offers that have no outcome yet.
//
// Keyed on slot AND tag throughout: a slot re-pointed at a different datum is
// a different offer and must be judged afresh, never carried by the first
// one's verdict.
func undeliveredSlots(sr *v1.SubagentRequest) []v1.DataSlotRequest {
	settled := func(slot, tag string) bool {
		for _, b := range sr.Status.BoundDataSlots {
			if b.Slot == slot && b.TagID == tag {
				return true
			}
		}
		for _, ref := range sr.Status.RefusedDataSlots {
			if ref.Slot == slot && ref.TagID == tag {
				return true
			}
		}
		return false
	}
	var out []v1.DataSlotRequest
	for _, s := range sr.Spec.DataSlots {
		if !settled(s.Slot, s.TagID) {
			out = append(out, s)
		}
	}
	return out
}

func parentKey(sr *v1.SubagentRequest) types.NamespacedName {
	return types.NamespacedName{Namespace: sr.Spec.Parent.Namespace, Name: sr.Spec.Parent.Name}
}

func tagForSlot(slots []v1.DataSlotRequest, slot string) string {
	for _, s := range slots {
		if s.Slot == slot {
			return s.TagID
		}
	}
	return ""
}
