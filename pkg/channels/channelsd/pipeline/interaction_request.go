package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/parkedprompt"
)

// HandleInteractionRequest is the category-generic inbound park handler. The
// runner's approval gates publish interaction_request(<category>) on the IN
// subject; this writes and dedups a self-contained PendingInteractions entry via
// WriteOwned and re-emits the request on OUT for the generic renderer.
//
// It writes no PHASE-deriving status condition — phase=AwaitingDecision derives
// from the runner's signed lifecycle projection (DecisionAsked), independent of
// this list. It DOES restore the parallel OBSERVABLE condition the removed typed
// approval-request handlers always wrote, registry-driven: when the parked
// category declares a Category.PendingCondition, that condition is set True here.
// This is not a phase authority — it is the channelsd-owned surface the in-process
// e2e factory (which never reaches the operator's phase projection) reads, and the
// observability signal an operator greps CR status for.
//
// Category-agnostic write-list + re-emit-OUT: there is no per-category branch —
// the registered Category + the payload fields drive everything.
//
// The AgentSession it parks against is env.Session, which is the authorized
// session only because internal/cmd/channelsd/main.go's envelopeHandler cross-checked it
// against the NATS subject and dropped any mismatch before dispatching here.
// Same for HandleInteractionApplied below, which clears an entry by the same key.
func (p *Pipeline) HandleInteractionRequest(ctx context.Context, env channelevents.Envelope) error {
	if env.Kind != channelevents.KindInteractionRequest {
		return fmt.Errorf("HandleInteractionRequest: unexpected kind %q", env.Kind)
	}
	var pl channelevents.InteractionRequestPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return fmt.Errorf("decode interaction request payload (session %s/%s): %w",
			env.Session.Namespace, env.Session.Name, err)
	}
	if err := pl.Validate(); err != nil {
		return fmt.Errorf("interaction request (session %s/%s, requestRef %q): %w",
			env.Session.Namespace, env.Session.Name, pl.RequestRef, err)
	}
	cat, ok := channelinteractions.Get(pl.Category)
	if !ok {
		return fmt.Errorf("interaction request: unknown category %q (session %s/%s, requestRef %q)",
			pl.Category, env.Session.Namespace, env.Session.Name, pl.RequestRef)
	}

	// Only parking categories get a durable entry; a non-parking category
	// (link/notice card) is delivered but never tracked. Still re-emit OUT so
	// the renderer runs.
	if cat.Park != "" {
		var sess spiceboxv1alpha1.AgentSession
		key := client.ObjectKey{Namespace: env.Session.Namespace, Name: env.Session.Name}
		if err := p.K8s.Get(ctx, key, &sess); err != nil {
			return fmt.Errorf("get session %s (requestRef %q): %w", key, pl.RequestRef, err)
		}
		// Dedup: the runner re-publishes outstanding requests on restart; the
		// resume path must not duplicate the entry. A byte-identical re-park is a
		// no-op through WriteOwned's only-changed diff — but we short-circuit here
		// so a matched entry is never appended a second time, then still re-emit
		// OUT (idempotent render) below.
		for _, existing := range sess.Status.PendingInteractions {
			if existing.RequestID == pl.RequestRef {
				return p.reEmitInteractionRequest(env, pl)
			}
		}
		cp := sess.DeepCopy()
		cp.Status.PendingInteractions = append(cp.Status.PendingInteractions, spiceboxv1alpha1.PendingInteraction{
			RequestID:        pl.RequestRef,
			Category:         pl.Category,
			ApproverSubject:  approverSubjectFor(cat, pl, env.Session.Namespace, env.Session.Name),
			RequestRef:       pl.RequestRef,
			RequestedAt:      metav1.NewTime(p.Now()),
			AgentDisplayName: agentDisplayNameFromFields(pl),
			Summary:          pl.Lead,
		})
		// Restore the category's observable pending condition (registry-driven, no
		// per-category branch). This is a parallel surface — NOT a phase input.
		if cat.PendingCondition != "" {
			_, n := pendingConditionActive(cp.Status.PendingInteractions, cat.PendingCondition)
			conditions.Set(cp, &cp.Status.Conditions, metav1.Condition{
				Type:    cat.PendingCondition,
				Status:  metav1.ConditionTrue,
				Reason:  "AwaitingApproval",
				Message: fmt.Sprintf("%d %s decision(s) pending", n, pl.Category),
			})
		}
		if err := applyApprovalStatus(ctx, p.K8s, cp, &sess); err != nil {
			return fmt.Errorf("apply agentsession pending-interaction status (requestRef %q): %w", pl.RequestRef, err)
		}
	}
	return p.reEmitInteractionRequest(env, pl)
}

// reEmitInteractionRequest re-publishes the request on OUT so the outbound relay
// renders it (and caches it for resurface). The payload is re-marshaled from the
// decoded value so the OUT envelope is a clean interaction_request.
func (p *Pipeline) reEmitInteractionRequest(env channelevents.Envelope, pl channelevents.InteractionRequestPayload) error {
	if err := channelevents.PublishOut(p.NATS.Publish, env.Session.Namespace, env.Session.Name,
		channelevents.KindInteractionRequest, pl); err != nil {
		return fmt.Errorf("re-emit out.interaction_request (session %s/%s, requestRef %q): %w",
			env.Session.Namespace, env.Session.Name, pl.RequestRef, err)
	}
	return nil
}

// approverSubjectFor derives the SpiceDB subject-set with standing to decide a
// category's interaction, keyed on its DeciderPolicy (NOT on the category name —
// generic). Session-set policies resolve to agentsession#approve; a
// DecideResourceOwners policy with a named resource resolves to that resource's
// #owner set; a policy without any fixed subject (requester / participant, or a
// resource-owner policy with no resource) returns "" (the server re-checks
// standing authoritatively regardless, and folds an empty resource set to the
// session approve-set). This populates PendingInteraction.ApproverSubject so
// `oap session approve`'s client-side pre-check has a subject to LookupSubjects.
func approverSubjectFor(cat channelinteractions.Category, pl channelevents.InteractionRequestPayload, ns, name string) string {
	switch cat.Deciders {
	case channelinteractions.DecideApprovers, channelinteractions.DecideOwner:
		return "agentsession:" + ns + "/" + name + "#approve"
	case channelinteractions.DecideResourceOwners:
		// Per-resource pool: holders of the resource's DECLARED approver
		// permission have standing to decide. Empty resources ⇒ the session
		// approve-set decides — left "" here so the server's authoritative fold
		// is not pre-empted.
		//
		// The permission travels on the ref rather than being rebuilt as "#owner"
		// here. This runs in channelsd, a different process from the runner that
		// resolved the pool when it raised the ask; hardcoding a relation name
		// meant the two could route to different subject-sets for the same
		// prompt, admitting a click the raiser never intended (or refusing one it
		// did). A ref with no permission named no per-resource pool.
		if len(pl.Resources) > 0 && pl.Resources[0].Permission != "" {
			return pl.Resources[0].Type + ":" + pl.Resources[0].ID + "#" + pl.Resources[0].Permission
		}
		return ""
	default:
		return ""
	}
}

// pendingConditionActive reports how many remaining PendingInteractions entries
// belong to a category whose PendingCondition equals cond, and whether any do.
// The generic clear paths use it to recompute a category's observable condition:
// True while any entry of that condition's category remains parked, False once the
// last resolves. Keyed on the condition (not the category name) so it stays correct
// even if two categories were ever to share one observable condition.
func pendingConditionActive(entries []spiceboxv1alpha1.PendingInteraction, cond string) (active bool, count int) {
	for _, e := range entries {
		if c, ok := channelinteractions.Get(e.Category); ok && c.PendingCondition == cond {
			count++
		}
	}
	return count > 0, count
}

// recomputePendingCondition sets or clears the observable condition owned by the
// resolved entry's category after it was removed from entries. No-op when the
// category is unregistered or declares no PendingCondition. Mirrors the deleted
// typed handlers' clear: False/AllResolved when the last entry of the category is
// gone, True/AwaitingApproval while others of the same category remain.
func recomputePendingCondition(cp *spiceboxv1alpha1.AgentSession, resolvedCategory string) {
	cat, ok := channelinteractions.Get(resolvedCategory)
	if !ok || cat.PendingCondition == "" {
		return
	}
	if active, n := pendingConditionActive(cp.Status.PendingInteractions, cat.PendingCondition); active {
		conditions.Set(cp, &cp.Status.Conditions, metav1.Condition{
			Type:    cat.PendingCondition,
			Status:  metav1.ConditionTrue,
			Reason:  "AwaitingApproval",
			Message: fmt.Sprintf("%d %s decision(s) pending", n, resolvedCategory),
		})
	} else {
		conditions.Set(cp, &cp.Status.Conditions, metav1.Condition{
			Type:   cat.PendingCondition,
			Status: metav1.ConditionFalse,
			Reason: "AllResolved",
		})
	}
}

// agentDisplayNameFromFields extracts the agent display name if the publisher
// stamped it as an interaction Field labeled "Agent"; empty otherwise. Kept
// tolerant — the field is display-only and never load-bearing.
func agentDisplayNameFromFields(pl channelevents.InteractionRequestPayload) string {
	for _, f := range pl.Fields {
		if f.Label == "Agent" {
			return f.Value
		}
	}
	return ""
}

// HandleInteractionApplied clears the matching PendingInteractions entry when a
// timeout Applied (Outcome=expired) arrives on IN from the runner's gate-side
// timeout watcher. Timeout-only: the normal decision path clears its own entry
// inside HandleInteractionDecision, so this finds no matching RequestID and
// no-ops there. Writes no condition, matching HandleInteractionRequest.
func (p *Pipeline) HandleInteractionApplied(ctx context.Context, env channelevents.Envelope) error {
	if env.Kind != channelevents.KindInteractionApplied {
		return fmt.Errorf("HandleInteractionApplied: unexpected kind %q", env.Kind)
	}
	var pl channelevents.InteractionAppliedPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return fmt.Errorf("decode interaction applied payload (session %s/%s): %w",
			env.Session.Namespace, env.Session.Name, err)
	}
	unlock := p.lockInteraction(env.Session.Namespace, env.Session.Name, pl.RequestRef)
	defer unlock()
	var sess spiceboxv1alpha1.AgentSession
	key := client.ObjectKey{Namespace: env.Session.Namespace, Name: env.Session.Name}
	if err := p.K8s.Get(ctx, key, &sess); err != nil {
		return fmt.Errorf("get session %s (requestRef %q): %w", key, pl.RequestRef, err)
	}

	// Timeout reports enter through IN only. Publish the retained winner after
	// serialization with human decisions; never send a competing live verdict.
	if pl.Outcome == channelevents.OutcomeExpired {
		cached := false
		if cat, ok := channelinteractions.Get(pl.Category); ok && cat.Resurface == channelinteractions.ResurfaceCached && p.Mem != nil {
			record, found, err := parkedprompt.Find(ctx, p.Mem, promptScope(key.Namespace, key.Name), pl.RequestRef)
			if err != nil {
				return err
			}
			cached = found
			if found {
				if !record.Resolved {
					frozen := pl
					frozen.AgentSessionRef, frozen.ResponseRef, frozen.MintedURL = env.Session, "", ""
					canonical, err := channelevents.BuildEnvelope(key.Namespace, key.Name, env.Kind, frozen)
					if err != nil {
						return err
					}
					raw, err := json.Marshal(canonical)
					if err != nil {
						return err
					}
					record, err = parkedprompt.ResolveWithOutcome(ctx, p.Mem, promptScope(key.Namespace, key.Name), pl.RequestRef, raw)
					if err != nil {
						return err
					}
				}
				if len(record.Resolution) > 0 {
					var canonical channelevents.Envelope
					if err := json.Unmarshal(record.Resolution, &canonical); err != nil {
						return err
					}
					var winner channelevents.InteractionAppliedPayload
					if err := json.Unmarshal(canonical.Payload, &winner); err != nil {
						return err
					}
					if winner.Outcome != channelevents.OutcomeExpired {
						return nil
					}
					if record.ResolutionPending {
						if err := p.deliverInteractionResolution(ctx, record); err != nil {
							return err
						}
					}
				}
			}
		}
		if !cached {
			if err := channelevents.PublishOut(p.NATS.Publish, key.Namespace, key.Name, env.Kind, pl); err != nil {
				return err
			}
		}
	}
	matchedIdx := -1
	for i, e := range sess.Status.PendingInteractions {
		if e.RequestID == pl.RequestRef {
			matchedIdx = i
			break
		}
	}
	if matchedIdx < 0 {
		// Already cleared by HandleInteractionDecision (normal decision path) —
		// nothing to do.
		return nil
	}
	resolvedCategory := sess.Status.PendingInteractions[matchedIdx].Category
	cp := sess.DeepCopy()
	cp.Status.PendingInteractions = append(
		cp.Status.PendingInteractions[:matchedIdx],
		cp.Status.PendingInteractions[matchedIdx+1:]...,
	)
	recomputePendingCondition(cp, resolvedCategory)
	if err := applyApprovalStatus(ctx, p.K8s, cp, &sess); err != nil {
		return fmt.Errorf("apply agentsession pending-interaction status (requestRef %q): %w", pl.RequestRef, err)
	}
	// Resolved (timeout path) — never re-surface this prompt.
	p.clearPendingPrompt(ctx, env.Session.Namespace, env.Session.Name, pl.RequestRef)
	return nil
}

// clearPendingInteraction removes the resolved entry from PendingInteractions
// via WriteOwned. Called from the normal decision path
// (HandleInteractionDecision, interaction_decision.go) after Applied has
// published so a resolved interaction never leaks a stranded durable entry.
// Idempotent: a matched-index-not-found is a no-op — the category never
// parked (a non-parking category like identity_choice/permission_request has
// no PendingInteractions entry at all), or the entry was already cleared
// (e.g. by HandleInteractionApplied's timeout path racing this one). Safe to
// call for every category unconditionally.
func (p *Pipeline) clearPendingInteraction(ctx context.Context, ns, name, requestID string) error {
	var sess spiceboxv1alpha1.AgentSession
	key := client.ObjectKey{Namespace: ns, Name: name}
	if err := p.K8s.Get(ctx, key, &sess); err != nil {
		return fmt.Errorf("get session %s: %w", key, err)
	}
	matchedIdx := -1
	for i, e := range sess.Status.PendingInteractions {
		if e.RequestID == requestID {
			matchedIdx = i
			break
		}
	}
	if matchedIdx < 0 {
		return nil
	}
	resolvedCategory := sess.Status.PendingInteractions[matchedIdx].Category
	cp := sess.DeepCopy()
	cp.Status.PendingInteractions = append(
		cp.Status.PendingInteractions[:matchedIdx],
		cp.Status.PendingInteractions[matchedIdx+1:]...,
	)
	recomputePendingCondition(cp, resolvedCategory)
	return applyApprovalStatus(ctx, p.K8s, cp, &sess)
}
