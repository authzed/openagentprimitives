package subagentrequest

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// bindDataSlots binds the tags the parent may delegate onto the child, and
// reports the ones it may not.
//
// Attenuation FIRST, always: a parent must not bind what it has no standing to
// delegate, which for a tag is `pt_tag:<T>#access@agentsession:<parent>`. The
// parent's judgment SELECTS within an envelope it cannot widen, so a
// compromised or injected parent can choose badly among the tags it holds, and
// no worse. It grants nothing; it can only fail to deny.
//
// Refused slots are RETURNED and land on status rather than being dropped. A
// child waiting on data that will never arrive — with nothing anywhere saying
// why — is a far worse failure than a refusal the parent can read back and act
// on.
//
// The expiry is the one authz slots use, and it is a BACKSTOP rather than the
// mechanism: the AgentSession finalizer's DeleteDataSlotGrants is what normally
// removes these. The expiry bounds the damage when teardown never runs at all.
func (r *Reconciler) bindDataSlots(
	ctx context.Context, sr *v1.SubagentRequest, child, parent *v1.AgentSession,
) (bound []v1.DataSlotRequest, refused []v1.RefusedDataSlot, pending []v1.DataSlotRequest, err error) {
	// UNDELIVERED, not every spec entry. A slot the pre-create gate already
	// settled — refused as not-delegable, or refused because its grade could
	// not be computed — must not be bound here: this no longer grades, so a
	// slot that reached the grant would be treated as a yes precisely because
	// nobody could say yes to it.
	todo := undeliveredSlots(sr)
	if len(todo) == 0 {
		return nil, nil, nil, nil
	}
	requested := make([]authz.DataSlotBinding, 0, len(todo))
	for _, s := range todo {
		requested = append(requested, authz.DataSlotBinding{Slot: s.Slot, TagID: s.TagID})
	}

	parentRef := authz.SessionRef{Namespace: parent.Namespace, Name: parent.Name}
	delegable, notDelegable, err := authz.FilterDelegableDataSlots(ctx, r.Authz, parentRef, requested)
	if err != nil {
		// An unanswerable attenuation check is not a yes. Returning the error
		// rolls the child back rather than delegating with an unknown envelope.
		return nil, nil, nil, err
	}

	// NO GRADING HERE. settleDataSlotsBeforeChild graded every slot before this
	// child existed, which is what stopped the create path building a child and
	// deleting it again — a window a polling parent could observe as
	// ChildVanished, reporting "your delegation failed" for one that was merely
	// waiting on a person.
	//
	// Attenuation still runs, deliberately: it decides whether a grant may be
	// WRITTEN, so it belongs immediately before the write and against live
	// state. Grading decides whether to ASK, which has to be settled before
	// there is anything to ask about.
	if len(delegable) > 0 {
		expiry := authz.SlotGrantExpiry(time.Now(), 0)
		if err := r.Authz.GrantDataSlots(ctx, child.Namespace, child.Name, delegable, expiry); err != nil {
			return nil, nil, nil, err
		}
	}
	for _, b := range delegable {
		bound = append(bound, v1.DataSlotRequest{Slot: b.Slot, TagID: b.TagID})
	}
	// Attenuation can still refuse here even though the pre-create gate ran:
	// it is a LIVE check, and a parent's standing on a tag can be revoked
	// between the two.
	for _, b := range notDelegable {
		refused = append(refused, v1.RefusedDataSlot{
			Slot: b.Slot, TagID: b.TagID, Reason: v1.DataSlotNotDelegable,
		})
	}
	if len(refused) > 0 {
		// Logged as well as recorded on status: the parent reads status, but an
		// operator watching a delegation that "worked" and produced nothing
		// needs the reason somewhere greppable too.
		log.FromContext(ctx).Info("data slots refused at bind time",
			"request", sr.Namespace+"/"+sr.Name,
			"parent", parent.Namespace+"/"+parent.Name,
			"notDelegable", len(notDelegable), "bound", len(bound))
	}
	return bound, refused, nil, nil
}

// deniedAmong returns the first still-pending slot a human refused, or "".
//
// Matched on slot AND tag, the same pairing slotApproved uses: a decision names
// one datum, and a re-pointed slot is a different question that has not been
// answered either way.
func deniedAmong(sr *v1.SubagentRequest, pending []v1.DataSlotRequest) string {
	for _, p := range pending {
		for _, d := range sr.Status.DeniedDataSlots {
			if d.Slot == p.Slot && d.TagID == p.TagID {
				return p.Slot
			}
		}
	}
	return ""
}

// slotApproved reports whether a human cleared this exact slot for this
// request.
//
// Matched on BOTH slot and tag. Matching the slot alone would let a later
// re-point of that slot to a different tag ride an approval given for the
// first — the same repoint-reasks rule the plan gate's slots already follow.
func slotApproved(sr *v1.SubagentRequest, b authz.DataSlotBinding) bool {
	for _, a := range sr.Status.ApprovedDataSlots {
		if a.Slot == b.Slot && a.TagID == b.TagID {
			return true
		}
	}
	return false
}
