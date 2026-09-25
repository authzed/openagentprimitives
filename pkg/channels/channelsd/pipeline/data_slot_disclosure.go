// Package-local: the decision handler for data_slot_disclosure.
//
// A parent agent wants to hand a specific datum to a child it is delegating
// to, and grading declined to auto-answer — the child's audience is not
// already among the tag's readers, or the datum carries untrusted content. A
// human who can speak for the datum decides.
//
// # Why the write-back lands on the SubagentRequest
//
// Unlike tool_approval, which writes a SpiceDB grant here, this handler writes
// only a RECORD OF CONSENT. The binding itself is the SubagentRequest
// controller's: it re-grades on its next pass and grants the slot only if the
// approval covers exactly the slot and tag it is about to bind.
//
// That split is deliberate. channelsd learns a human said yes; it does not
// learn — and must not decide — whether the parent still has standing to
// delegate that tag, whether the child is still the one that asked, or whether
// the tag has since been re-pointed. Those are re-checked operator-side
// against live state. A handler that wrote the grant directly would be
// approving on facts that were true when the card was published.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	memapproval "github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
)

// DataSlotDisclosureDetails is the side-band payload the operator publishes
// with the card and this handler reads back: which request, which slot, which
// datum.
//
// It names the SLOT AND THE TAG, not just the slot. A decision recorded
// against a slot alone would still apply after the parent re-pointed that slot
// at a different datum, which is consent transferred to something the human
// never saw.
type DataSlotDisclosureDetails struct {
	RequestNamespace string `json:"requestNamespace"`
	RequestName      string `json:"requestName"`
	Slot             string `json:"slot"`
	TagID            string `json:"tagID"`
}

// BindDataSlotDisclosureHandler binds the handler. Separate from the category
// Register for the usual reason: rows are declarative, handlers close over
// process dependencies at Bind time.
func BindDataSlotDisclosureHandler(p *Pipeline) {
	channelinteractions.Bind(categories.DataSlotDisclosure, dataSlotDisclosureHandler(p))
}

func dataSlotDisclosureHandler(p *Pipeline) channelinteractions.DecisionHandler {
	return func(ctx context.Context, d channelinteractions.Decision) (channelinteractions.Outcome, error) {
		det, err := resolveDataSlotDetails(ctx, d, p.Mem)
		if err != nil {
			return channelinteractions.Outcome{},
				fmt.Errorf("data slot disclosure: resolve details (requestRef %q): %w", d.Payload.RequestRef, err)
		}
		if p.K8s == nil {
			// Programming error. Fail loud rather than report an approval the
			// controller will never see — a card that says "approved" over a
			// delegation that then expires unexplained is worse than an error
			// the clicker can act on.
			return channelinteractions.Outcome{},
				fmt.Errorf("data slot disclosure: K8s not configured (programming error) (requestRef %q)", d.Payload.RequestRef)
		}

		approved := d.Payload.ActionID == "approve"
		if err := recordDataSlotDecision(ctx, p.K8s, det, approved); err != nil {
			return channelinteractions.Outcome{},
				fmt.Errorf("data slot disclosure: record decision on %s/%s: %w",
					det.RequestNamespace, det.RequestName, err)
		}
		if approved {
			return channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil
		}
		return channelinteractions.Outcome{Result: channelevents.OutcomeDenied}, nil
	}
}

// recordDataSlotDecision appends the decision to the request's status.
//
// A DENIAL is recorded as explicitly as an approval, and that is the point of
// writing it at all: without it the controller cannot distinguish "the human
// said no" from "nobody has answered yet", so a refused disclosure would sit
// until the wait window elapsed and then report a timeout. The parent would be
// told nobody decided when in fact someone did.
func recordDataSlotDecision(ctx context.Context, c client.Client, det DataSlotDisclosureDetails, approved bool) error {
	var sr spiceboxv1alpha1.SubagentRequest
	key := types.NamespacedName{Namespace: det.RequestNamespace, Name: det.RequestName}
	if err := c.Get(ctx, key, &sr); err != nil {
		return fmt.Errorf("get subagentrequest: %w", err)
	}
	entry := spiceboxv1alpha1.DataSlotRequest{Slot: det.Slot, TagID: det.TagID}

	// Idempotent: a decision delivered twice (a retry, a duplicate click)
	// must not append twice, or the controller reads a list whose length has
	// stopped meaning anything.
	list := &sr.Status.ApprovedDataSlots
	if !approved {
		list = &sr.Status.DeniedDataSlots
	}
	for _, e := range *list {
		if e.Slot == entry.Slot && e.TagID == entry.TagID {
			return nil
		}
	}
	*list = append(*list, entry)
	return c.Status().Update(ctx, &sr)
}

// resolveDataSlotDetails recovers the side-band payload, preferring the cached
// request and falling back to the durable memapproval record.
//
// The fallback is what makes a decision survive a channelsd restart between
// publishing the card and the human clicking it — mirrors
// resolveToolApprovalDetails, for the same reason.
func resolveDataSlotDetails(ctx context.Context, d channelinteractions.Decision, mem memory.Memory) (DataSlotDisclosureDetails, error) {
	var det DataSlotDisclosureDetails
	if d.Request != nil && len(d.Request.Details) > 0 {
		if err := json.Unmarshal(d.Request.Details, &det); err != nil {
			return det, fmt.Errorf("decode cached details: %w", err)
		}
		return det, nil
	}
	if mem == nil {
		return det, fmt.Errorf("no cached request and no durable memory reader")
	}
	rec, err := memapproval.RequestByID(ctx, mem,
		memory.Scope{Kind: "session", ID: d.Session.Namespace + "/" + d.Session.Name}, d.Payload.RequestRef)
	if err != nil {
		return det, fmt.Errorf("durable memapproval lookup: %w", err)
	}
	if rec == nil || len(rec.Details) == 0 {
		return det, fmt.Errorf("durable memapproval record missing details for requestRef %q", d.Payload.RequestRef)
	}
	if err := json.Unmarshal(rec.Details, &det); err != nil {
		return det, fmt.Errorf("decode durable details: %w", err)
	}
	return det, nil
}
