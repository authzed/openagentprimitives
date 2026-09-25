package pipeline

import (
	"context"
	"time"

	"github.com/google/cel-go/cel"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// TriggerSlotRequest is one slot's trigger-binding inputs.
type TriggerSlotRequest struct {
	ResourceType string
	Permission   string
	// Expr is the slot's compiled triggerInstance, nil when the slot
	// declared none and relies on the kind's TriggerSlotProvider.
	Expr cel.Program
}

// maxTriggerBoundBindings caps how many instances one verified delivery may
// bind without any human involvement. The delivery is a forge's own event,
// not a person's message — mirroring maxThreadSeededBindings' reasoning at
// event scale rather than at conversation scale. Anything past the cap is
// not dropped silently; it is logged, and the ordinary approval flow still
// works for it.
const maxTriggerBoundBindings = 8

// TriggerSlotRequestsFor mirrors threadSeedRequestsFor (backfill.go): slots
// eligible for the trigger fill source, plus — when a slot declares one —
// its own compiled TriggerInstance expression.
//
// Unlike every other fill source, eligibility here is authz.ExplicitlyAllowsFill,
// not authz.AllowsFill: an unset fillFrom NEVER makes a slot trigger-eligible,
// however it reads for query/ask/channel_thread/default on the same slot. See
// ExplicitlyAllowsFill's doc comment for why trigger alone needs this. The one
// rule applies uniformly to both ways a slot can bind from a delivery — a bare
// fillFrom:[trigger] relying on the kind's TriggerSlotProvider, and a
// TriggerInstance expression — so a slot with an expression but no `trigger`
// named in fillFrom is excluded exactly like one with neither.
//
// A resourceType whose ResolvedSlots publishes a non-empty ValueTransforms
// chain is also excluded (with a log naming why): the trigger path binds a
// kind- or expression-derived id VERBATIM (see BindTriggerSlots), while every
// other fill source runs the type's declared chain. Letting both live would
// mean either two different ids answering for what should be the same
// instance (an inert second grant that authorizes nothing else already
// authorized), or, when they DO collide, one bad id inside a single
// authz.SlotBinding batch failing the whole GrantSlots call and taking every
// sibling binding in that call down with it.
//
// A TriggerInstance that fails to compile here is skipped, with an INFO log,
// rather than binding nothing silently or panicking: the AgentClass
// slot-declaration validator already refuses a malformed expression at
// admission, so reaching a compile failure here means a class admitted
// before that check existed. Never a panic.
func TriggerSlotRequestsFor(ctx context.Context, class *spiceboxv1alpha1.AgentClass) []TriggerSlotRequest {
	logger := log.FromContext(ctx)
	if class == nil {
		logger.Info("trigger slot request: nil AgentClass; no requests")
		return nil
	}
	slots := class.Spec.GetSlots()
	if len(slots) == 0 {
		return nil
	}

	var anyTriggerEligible bool
	for _, s := range slots {
		if authz.ExplicitlyAllowsFill(s.FillFrom, authz.FillTrigger) {
			anyTriggerEligible = true
			break
		}
	}
	if anyTriggerEligible && len(class.Status.ResolvedSlots) == 0 {
		// The reconciler publishes ResolvedSlots once per declared slot
		// (resolveSlotValueKeying) — an empty list here, on a class that
		// declares a trigger-eligible slot, cannot mean "no slot publishes a
		// transform chain"; it means the reconciler has not run yet. The two
		// are indistinguishable from this list alone, and getting it wrong
		// binds a kind- or expression-derived id verbatim for a resourceType
		// whose chain simply has not been published. Fail closed: skip this
		// delivery, and the next one after reconcile catches up binds
		// correctly.
		logger.Info("trigger slot request: class declares a trigger-eligible slot but Status.ResolvedSlots is not yet published; skipping this delivery",
			"class", class.Namespace+"/"+class.Name)
		return nil
	}

	transforms := make(map[string][]string, len(class.Status.ResolvedSlots))
	for _, rs := range class.Status.ResolvedSlots {
		transforms[rs.ResourceType] = rs.ValueTransforms
	}
	var out []TriggerSlotRequest
	for _, s := range slots {
		if !authz.ExplicitlyAllowsFill(s.FillFrom, authz.FillTrigger) {
			continue
		}
		if len(transforms[s.ResourceType]) > 0 {
			logger.Info("trigger slot request: resourceType publishes a value-transform chain that the verbatim trigger path cannot honour; slot excluded",
				"class", class.Namespace+"/"+class.Name, "resourceType", s.ResourceType)
			continue
		}
		req := TriggerSlotRequest{ResourceType: s.ResourceType, Permission: s.Permission}
		if s.TriggerInstance != "" {
			prg, err := authz.CompileTriggerInstanceExpr(s.TriggerInstance)
			if err != nil {
				logger.Info("trigger slot request: triggerInstance failed to compile; slot excluded from this delivery",
					"class", class.Namespace+"/"+class.Name, "resourceType", s.ResourceType, "err", err.Error())
				continue
			}
			req.Expr = prg
		}
		out = append(out, req)
	}
	return out
}

// BindTriggerSlots grants each request the instance(s) a verified webhook
// delivery names, into sess's trigger-eligible slots.
//
// Reads ONLY p.Authz off the Pipeline, plus the chregistry package-level
// lookup — no other Pipeline field. Task 5's replay proof constructs a
// minimal Pipeline{Authz: ...} and drives this directly; anything else read
// off p here would silently stop working there, with no compile error to
// catch it.
//
// Per request: if Expr is set, EvalTriggerInstance is authoritative for that
// slot and the kind's own instances are not consulted for it. Otherwise the
// candidates are whichever of the kind's TriggerSlotProvider instances (
// derived once, regardless of how many requests need them) match the
// request's ResourceType exactly — a kind offering several resource types in
// one delivery must not let one slot's request absorb another's instances.
func (p *Pipeline) BindTriggerSlots(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	ev channelkinds.InboundEvent,
	reqs []TriggerSlotRequest,
	expiresAt time.Time,
) {
	if len(reqs) == 0 {
		return
	}
	logger := log.FromContext(ctx)
	if p.Authz == nil {
		logger.Info("trigger slot binding: nil Authz on the Pipeline; nothing can be granted",
			"session", sess.Namespace+"/"+sess.Name)
		return
	}
	if ev.Channel == nil {
		logger.Info("trigger slot binding: inbound event carries no Channel; cannot resolve a kind",
			"session", sess.Namespace+"/"+sess.Name)
		return
	}

	// The kind's own instances, derived ONCE — exactly the envelope-facts
	// shape (pipeline.go's TriggerFactProvider stanza): type-assert the value
	// the kind's WebhookReceiver returns, never the registered Kind itself,
	// which would silently and permanently derive nothing for every kind.
	// Derivation error, or no provider at all: kind instances are none, and
	// expression-backed requests may still bind.
	var kindInstances []channelkinds.TriggerSlotInstance
	if k, ok := chregistry.Get(ev.Channel.Spec.Kind); ok {
		if sp, ok := k.WebhookReceiver(channelkinds.Deps{}).(channelkinds.TriggerSlotProvider); ok {
			instances, ierr := sp.TriggerSlotInstances(ev.Channel, ev.DeliveryEvent, ev.RawDelivery)
			if ierr != nil {
				logger.Info("trigger slot binding: kind instance derivation failed; only expression-backed slots may bind",
					"session", sess.Namespace+"/"+sess.Name, "kind", ev.Channel.Spec.Kind, "err", ierr.Error())
			} else {
				kindInstances = instances
			}
		}
	}

	seen := map[string]struct{}{}
	var bindings []authz.SlotBinding
	var dropped int
	for _, req := range reqs {
		var rawIDs []string
		if req.Expr != nil {
			id, err := authz.EvalTriggerInstance(req.Expr, ev.DeliveryEvent, ev.RawDelivery)
			if err != nil {
				logger.Info("trigger slot binding: expression did not yield an id; slot not bound",
					"session", sess.Namespace+"/"+sess.Name, "kind", ev.Channel.Spec.Kind, "resourceType", req.ResourceType, "err", err.Error())
				continue
			}
			rawIDs = []string{id}
		} else {
			for _, ki := range kindInstances {
				if ki.ResourceType == req.ResourceType {
					rawIDs = append(rawIDs, ki.ResourceID)
				}
			}
			// The kind reported instances for THIS delivery but none of them
			// named this slot's resourceType. Logged, unlike a kind that
			// reported nothing at all (the ordinary case for a delivery event
			// none of the class's slots cares about) — a delivery that plainly
			// carried instances leaving a slot empty is the shape an author
			// needs to see, not per-delivery noise.
			if len(kindInstances) > 0 && len(rawIDs) == 0 {
				logger.Info("trigger slot binding: kind reported instances but none match this slot's resourceType; slot not bound",
					"session", sess.Namespace+"/"+sess.Name, "kind", ev.Channel.Spec.Kind,
					"resourceType", req.ResourceType, "offeredTypes", offeredResourceTypes(kindInstances))
			}
		}
		for _, raw := range rawIDs {
			// nil transforms. For a kind-derived id this is a structural,
			// immutable key the forge itself stamped (a GraphQL node id, not
			// free-form text a human wrote) — there is nothing to
			// canonicalise. For an expression-derived id this instead rests
			// on operator authorship: the CEL expression is written by the
			// class author and already passed CompileTriggerInstanceExpr at
			// class admission, so the string it yields is trusted verbatim
			// the same way any other admission-checked configuration value
			// is, not because the value happens to be structurally
			// immutable. Either way, TriggerSlotRequestsFor has already
			// excluded this resourceType if it publishes a ValueTransforms
			// chain, so nil here never bypasses a chain another fill source
			// would apply to the same type.
			//
			// This trust argument is not the only thing standing between a
			// hostile value and a written grant, even with nil transforms:
			// NewObjectID below refuses only an empty string, and GrantSlots'
			// write still passes through SpiceDB's own object-id grammar,
			// which refuses anything outside it at write time. That refusal
			// — fail-closed, not merely logged — is the actual backstop for a
			// forge- or expression-derived value that turns out not to be the
			// well-formed id this comment assumes it is.
			id, err := authz.NewObjectID(raw, nil)
			if err != nil {
				logger.Info("trigger slot binding: instance id refused; slot not bound",
					"session", sess.Namespace+"/"+sess.Name, "kind", ev.Channel.Spec.Kind, "resourceType", req.ResourceType, "err", err.Error())
				continue
			}
			b := authz.SlotBinding{ResourceType: req.ResourceType, ResourceID: id, Permission: req.Permission}
			dedupKey := b.ResourceType + "\x00" + id.String() + "\x00" + b.Permission
			if _, dup := seen[dedupKey]; dup {
				continue
			}
			// Marked BEFORE the cap check: once an instance is SEEN, a later
			// repeat of the exact same (type, id, permission) must always
			// hit the dedup branch above, whether or not the first sighting
			// made it under the cap. Marking after the cap check would let a
			// duplicate that arrives once the cap is already full inflate
			// `dropped` a second time for the same instance, rather than
			// being recognised as the repeat it is.
			seen[dedupKey] = struct{}{}
			if len(bindings) >= maxTriggerBoundBindings {
				dropped++
				continue
			}
			bindings = append(bindings, b)
		}
	}
	if dropped > 0 {
		logger.Info("trigger slot binding: capped; the rest bind nothing for this delivery",
			"session", sess.Namespace+"/"+sess.Name, "bound", len(bindings), "dropped", dropped, "cap", maxTriggerBoundBindings)
	}
	if len(bindings) == 0 {
		return
	}
	if err := p.Authz.GrantSlots(ctx, sess.Namespace, sess.Name, bindings, expiresAt); err != nil {
		logger.Info("trigger slot binding: grant write failed; every slot-bound write in this batch (see bindings) will be refused; there is nobody to approve on a triggered session",
			"session", sess.Namespace+"/"+sess.Name, "bindings", len(bindings), "err", err.Error())
		return
	}
	logger.Info("trigger slot binding: bound the instances a verified delivery named",
		"session", sess.Namespace+"/"+sess.Name, "bindings", len(bindings))
}

// offeredResourceTypes renders the distinct resourceTypes a kind's
// TriggerSlotInstances call reported, for a log line explaining why a
// request's own resourceType found nothing among them.
func offeredResourceTypes(instances []channelkinds.TriggerSlotInstance) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, ki := range instances {
		if _, dup := seen[ki.ResourceType]; dup {
			continue
		}
		seen[ki.ResourceType] = struct{}{}
		out = append(out, ki.ResourceType)
	}
	return out
}
