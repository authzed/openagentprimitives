package subagentrequest

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// publishDisclosureCards asks a human to rule on each slot that would disclose.
//
// ONE CARD PER SLOT, not one per request. Each names a different datum going
// to the same child, and the people entitled to rule on them are different
// sets — a single card would either ask one group about data they cannot see
// or force an all-or-nothing answer over unrelated disclosures.
//
// Published on the PARENT's IN subject, where channelsd parks the session and
// re-emits for the renderer. Publishing on OUT directly would bypass the park
// and the durable record, and the durable record is what lets a decision
// survive a channelsd restart between the card being shown and the click.
//
// Errors are logged and NOT returned. A card that failed to publish leaves the
// request parked, and the bounded wait ends it with a stated reason — whereas
// returning would retry the whole reconcile and risk publishing the card twice
// for the one that did succeed. What must never happen is proceeding to bind
// something nobody was asked about, and that cannot happen here: binding reads
// status.approvedDataSlots, which only a real decision writes.
func (r *Reconciler) publishDisclosureCards(
	ctx context.Context, sr *v1.SubagentRequest, parent *v1.AgentSession, pending []v1.DataSlotRequest,
) {
	if r.PublishInteraction == nil {
		log.FromContext(ctx).Info("data slot disclosure: no interaction publisher wired; nobody will be asked and the request will expire",
			"request", sr.Namespace+"/"+sr.Name, "pending", len(pending))
		return
	}
	logger := log.FromContext(ctx)
	expires := time.Now().Add(disclosureWaitWindow)

	for _, slot := range pending {
		det, merr := json.Marshal(pipeline.DataSlotDisclosureDetails{
			RequestNamespace: sr.Namespace,
			RequestName:      sr.Name,
			Slot:             slot.Slot,
			TagID:            slot.TagID,
		})
		if merr != nil {
			logger.Info("data slot disclosure: could not encode card details; this slot will not be asked about",
				"request", sr.Namespace+"/"+sr.Name, "slot", slot.Slot, "err", merr.Error())
			continue
		}

		resources := r.disclosureResources(ctx, parent, slot.TagID)
		approvers := r.disclosureApprovers(ctx, resources)
		if len(approvers) == 0 {
			// The envelope contract refuses an approvers-scope prompt with no
			// approvers, so publishing would fail anyway. Saying it here names
			// the CAUSE — nobody holds owner on the datum's sources — instead
			// of leaving a validation error to be read as a bug.
			logger.Info("data slot disclosure: nobody can rule on this datum, so no card is published; the request will expire",
				"request", sr.Namespace+"/"+sr.Name, "slot", slot.Slot, "tag", slot.TagID)
			continue
		}

		pl := channelevents.InteractionRequestPayload{
			AgentSessionRef: channelevents.SessionRef{Namespace: parent.Namespace, Name: parent.Name},
			Category:        categories.DataSlotDisclosure,
			RequestRef:      fmt.Sprintf("dsd-%s-%s", sr.Name, slot.Slot),
			Lead:            fmt.Sprintf("%s wants to share data with %s", parent.Name, sr.Spec.Class),
			Body: fmt.Sprintf(
				"It would fill %s's %q input slot with data you can see and %s's readers may not, "+
					"or that came from a source whose content cannot be trusted.",
				sr.Spec.Class, slot.Slot, sr.Spec.Class),
			NextStep:  "Approve only if that agent should be able to read this data.",
			Fields:    disclosureFields(sr, slot),
			Actions:   disclosureActions(),
			Details:   det,
			Resources: resources,
			Audience:  channelevents.InteractionAudience{Scope: channelevents.AudienceApprovers, Approvers: approvers},
			ExpiresAt: &expires,
		}
		env, eerr := channelevents.BuildEnvelope(
			parent.Namespace, parent.Name, channelevents.KindInteractionRequest, pl)
		if eerr != nil {
			logger.Info("data slot disclosure: could not build the card envelope",
				"request", sr.Namespace+"/"+sr.Name, "slot", slot.Slot, "err", eerr.Error())
			continue
		}
		if perr := r.PublishInteraction(ctx, parent.Namespace, parent.Name, env); perr != nil {
			logger.Info("data slot disclosure: card publish failed; nobody was asked about this slot",
				"request", sr.Namespace+"/"+sr.Name, "slot", slot.Slot, "err", perr.Error())
			continue
		}
		logger.Info("data slot disclosure: asked for a decision",
			"request", sr.Namespace+"/"+sr.Name, "slot", slot.Slot, "tag", slot.TagID)
	}
}

// disclosureFields are the supporting rows. Deliberately structural — the slot
// and the receiving agent — and never the datum itself: a card that showed the
// content would disclose it to whoever can see the card, which is the very
// thing being asked about.
func disclosureFields(sr *v1.SubagentRequest, slot v1.DataSlotRequest) []channelevents.InteractionField {
	return []channelevents.InteractionField{
		{Label: "Receiving agent", Value: sr.Spec.Class},
		{Label: "Input slot", Value: slot.Slot},
		{Label: "Task", Value: sr.Spec.Task},
	}
}

func disclosureActions() []channelevents.InteractionAction {
	return []channelevents.InteractionAction{
		{ID: "approve", Label: "Share it", Kind: channelevents.ActionKindDecision, Style: channelevents.ActionStylePrimary},
		{ID: "deny", Label: "Don't share", Kind: channelevents.ActionKindDecision},
	}
}

// disclosureResources names the objects the datum came from, so the category's
// DecideResourceOwners policy can route to their owners.
//
// A lookup failure yields NO resources rather than an error, which falls the
// card back to the session approve-set. That is the right degradation: an
// empty resource list means "the owner set could not be computed", and asking
// the session's approvers is strictly better than asking nobody — the
// alternative to a decider is an expiry.
func (r *Reconciler) disclosureResources(
	ctx context.Context, parent *v1.AgentSession, tagID string,
) []channelevents.InteractionResourceRef {
	if r.TagSources == nil {
		return nil
	}
	srcs, err := r.TagSources(ctx, authz.SessionRef{Namespace: parent.Namespace, Name: parent.Name}, tagID)
	if err != nil {
		log.FromContext(ctx).Info("data slot disclosure: could not resolve the datum's sources; the card falls back to the session approvers",
			"tag", tagID, "err", err.Error())
		return nil
	}
	refs := make([]channelevents.InteractionResourceRef, 0, len(srcs))
	for _, s := range srcs {
		t, id, ok := splitTypeID(s)
		if !ok {
			continue
		}
		refs = append(refs, channelevents.InteractionResourceRef{Type: t, ID: id})
	}
	return refs
}

// disclosureApprovers turns the datum's source objects into the people who may
// rule on sharing it, by expanding each source's `#owner`.
//
// OWNER, not viewer. Being able to read a thing is not authority to authorize
// sharing it — the same distinction the info-leakage approver model draws, and
// the reason a fixture that declares only `viewer` produces a card nobody can
// decide.
//
// Deduplicated: one person owning two of the sources must appear once, or the
// delivery layer addresses them twice for a single question.
func (r *Reconciler) disclosureApprovers(
	ctx context.Context, resources []channelevents.InteractionResourceRef,
) []channelevents.ExternalIdentity {
	if r.ResourceOwners == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []channelevents.ExternalIdentity
	for _, res := range resources {
		owners, err := r.ResourceOwners(ctx, res.Type, res.ID)
		if err != nil {
			// One unresolvable source must not blank the whole decider set:
			// another source may still yield an owner, and a card delivered to
			// some of the right people beats no card at all.
			log.FromContext(ctx).Info("data slot disclosure: could not resolve owners of a source",
				"resource", res.Type+":"+res.ID, "err", err.Error())
			continue
		}
		for _, o := range owners {
			if o == "" || seen[o] {
				continue
			}
			seen[o] = true
			out = append(out, channelevents.ExternalIdentity{
				Subject: identity.CanonicalFromTrusted(o,
					"owner canonical resolved by the controller").Subject(),
			})
		}
	}
	return out
}

// splitTypeID splits a "type:id" source. Refuses anything else rather than
// guessing a type — a malformed ref silently dropped would narrow the decider
// set without saying so, which looks exactly like a correct restrictive answer.
func splitTypeID(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' && i > 0 && i+1 < len(s) {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}
