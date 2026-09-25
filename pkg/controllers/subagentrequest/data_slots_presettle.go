package subagentrequest

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/handoff"
)

// settleDataSlotsBeforeChild decides every requested slot while there is still
// no child, so a delegation that needs a person's decision never creates and
// deletes one.
//
// done=false means the caller must return res: the request is parked awaiting a
// decision, or has failed. No child has been built, so there is nothing to roll
// back and nothing for a polling parent to see vanish.
//
// See the call site for why grading a not-yet-existing child is sound: its
// audience is exactly its parent's.
func (r *Reconciler) settleDataSlotsBeforeChild(
	ctx context.Context, sr *v1.SubagentRequest, parent *v1.AgentSession,
) (done bool, res ctrl.Result, err error) {
	if len(sr.Spec.DataSlots) == 0 {
		return true, ctrl.Result{}, nil
	}
	pendingBefore := undeliveredSlots(sr)
	if len(pendingBefore) == 0 {
		return true, ctrl.Result{}, nil
	}

	parentRef := authz.SessionRef{Namespace: parent.Namespace, Name: parent.Name}
	requested := make([]authz.DataSlotBinding, 0, len(pendingBefore))
	for _, s := range pendingBefore {
		requested = append(requested, authz.DataSlotBinding{Slot: s.Slot, TagID: s.TagID})
	}
	delegable, notDelegable, ferr := authz.FilterDelegableDataSlots(ctx, r.Authz, parentRef, requested)
	if ferr != nil {
		// Unanswerable is not a yes: retry rather than proceed on an envelope
		// nobody could establish.
		return false, ctrl.Result{}, ferr
	}
	for _, b := range notDelegable {
		sr.Status.RefusedDataSlots = append(sr.Status.RefusedDataSlots, v1.RefusedDataSlot{
			Slot: b.Slot, TagID: b.TagID, Reason: v1.DataSlotNotDelegable,
		})
	}

	var awaiting []v1.DataSlotRequest
	for _, b := range delegable {
		if slotApproved(sr, b) || r.Grader == nil {
			continue // settled: the create path binds it once the child exists
		}
		// parentRef, not a child ref — the child does not exist yet, and its
		// audience is its parent's.
		grade, why, gerr := r.Grader(ctx, parentRef, b.TagID)
		switch {
		case gerr != nil:
			log.FromContext(ctx).Info("data slot not granted: grading could not be completed",
				"request", sr.Namespace+"/"+sr.Name, "slot", b.Slot, "err", gerr.Error())
			sr.Status.RefusedDataSlots = append(sr.Status.RefusedDataSlots, v1.RefusedDataSlot{
				Slot: b.Slot, TagID: b.TagID, Reason: v1.DataSlotGradingUnavailable,
			})
		case grade != handoff.GradeAutoGrant:
			log.FromContext(ctx).Info("data slot needs a decision before the child is started",
				"request", sr.Namespace+"/"+sr.Name, "slot", b.Slot, "reason", why)
			awaiting = append(awaiting, v1.DataSlotRequest{Slot: b.Slot, TagID: b.TagID})
		}
	}
	if len(awaiting) == 0 {
		sr.Status.PendingDataSlots = nil
		sr.Status.DisclosureRequestedAt = nil
		return true, ctrl.Result{}, nil
	}

	sr.Status.PendingDataSlots = awaiting

	// A human's NO ends it before anything is built.
	if denied := deniedAmong(sr, awaiting); denied != "" {
		log.FromContext(ctx).Info("disclosure refused by a decider; the delegation was never made",
			"request", sr.Namespace+"/"+sr.Name, "slot", denied)
		sr.Status.Phase = v1.SubagentRequestPhaseFailed
		sr.Status.Determination = fmt.Sprintf(
			"handing the %q data slot to %s was refused by someone who can speak for that data. "+
				"The delegation was not made — no part of it ran.", denied, sr.Spec.Class)
		return false, ctrl.Result{}, r.Status().Update(ctx, sr)
	}

	started := sr.Status.DisclosureRequestedAt
	if started == nil {
		now := metav1.Now()
		sr.Status.DisclosureRequestedAt = &now
		log.FromContext(ctx).Info("data slots await a disclosure decision; no child is created",
			"request", sr.Namespace+"/"+sr.Name, "pending", len(awaiting))
		r.publishDisclosureCards(ctx, sr, parent, awaiting)
		started = sr.Status.DisclosureRequestedAt
	}
	if r.clock().Sub(started.Time) > disclosureWaitWindow {
		log.FromContext(ctx).Info("disclosure decision window elapsed; failing the delegation",
			"request", sr.Namespace+"/"+sr.Name, "pending", len(awaiting), "window", disclosureWaitWindow)
		sr.Status.Phase = v1.SubagentRequestPhaseFailed
		sr.Status.Determination = fmt.Sprintf(
			"handing %d data slot(s) to %s needed a decision from someone who can speak for that data, and none "+
				"arrived within %s. The delegation was not made — no part of it ran.",
			len(awaiting), sr.Spec.Class, disclosureWaitWindow)
		return false, ctrl.Result{}, r.Status().Update(ctx, sr)
	}

	sr.Status.Phase = v1.SubagentRequestPhaseAwaitingDisclosure
	sr.Status.Determination = fmt.Sprintf(
		"%d data slot(s) would disclose to %s; awaiting a decision from someone who can speak for the data",
		len(awaiting), sr.Spec.Class)
	return false, ctrl.Result{RequeueAfter: disclosurePollInterval}, r.Status().Update(ctx, sr)
}
