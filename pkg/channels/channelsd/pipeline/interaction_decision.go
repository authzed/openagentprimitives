package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	memapproval "github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/parkedprompt"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// HandleInteractionDecision is the category-generic decision pipe: validate,
// reject an already-resolved click, check standing per the category's
// DeciderPolicy (fail-closed), invoke the bound handler, publish Applied to
// runner (.in) and surface (.out), then clear the pending prompt AND the
// durable PendingInteractions entry (publish-first ordering contract — the same
// commit-then-clear rule identity_choice.go and tool_approval.go follow).
//
// Idempotency has two legs, both consulted before the handler: the in-process
// resolvedCache (warm, and the only one that can name who decided) and the
// durable parked-prompt tombstone (survives a restart). Both matter — a
// handler is not replayable: tool_approval's writes a SpiceDB grant tuple.
//
// Surfaces own synchronous click feedback (HTTP 403, Slack ephemeral); this
// pipe is the authoritative validator and logs every rejection with context.
//
// WHICH SESSION is decided comes from env.Session, and that is sound only
// because the bus wrapper (internal/cmd/channelsd/main.go's envelopeHandler) has already
// cross-checked it against the NATS subject — the sole session identity a
// publisher's per-session JWT authorizes — and dropped any mismatch. WHO is
// deciding is a separate question, taken from pl.Decider below and re-checked
// against SpiceDB standing per the category's DeciderPolicy; never take either
// from the payload without its own check.
func (p *Pipeline) HandleInteractionDecision(ctx context.Context, env channelevents.Envelope) error {
	if env.Kind != channelevents.KindInteractionDecision {
		return fmt.Errorf("interaction decision: unexpected kind %q", env.Kind)
	}
	var pl channelevents.InteractionDecisionPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return fmt.Errorf("interaction decision: decode: %w", err)
	}
	if err := pl.Validate(); err != nil {
		return fmt.Errorf("interaction decision: %w", err)
	}
	ns, name := env.Session.Namespace, env.Session.Name
	ref := ns + "/" + name
	logger := log.FromContext(ctx).WithValues(
		"session", ref, "category", pl.Category, "requestRef", pl.RequestRef, "actionId", pl.ActionID)

	if !channelevents.ComponentDecisionIngress(ctx) && p.K8s != nil {
		var session spiceboxv1alpha1.AgentSession
		if err := p.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &session); err != nil {
			return fmt.Errorf("decision session trust lookup: %w", err)
		}
		if session.Spec.GoalExecution != nil {
			return fmt.Errorf("bounded goal decisions require component-only ingress")
		}
	}
	cat, ok := channelinteractions.Get(pl.Category)
	if !ok {
		return fmt.Errorf("interaction decision: unknown category %q (session %s)", pl.Category, ref)
	}

	// Idempotency, warm leg: first writer wins within this process; later clicks
	// are spectators. This leg is the one that can name WHO decided and what
	// they applied, so it is consulted before the durable leg below.
	if r, resolved := p.resolvedCache.get(pl.Category, pl.RequestRef); resolved {
		logger.Info("interaction decision: already resolved; spectator")
		return p.publishDecisionRejected(ctx, env.Session, pl, "already_resolved", "already resolved", &r.Approver, r.Decision)
	}

	// One durable read serves two jobs: the cross-restart already-resolved
	// verdict, and the original request handed to the standing check + handler.
	// Nil-tolerant on both: a ResurfaceRegenerate category stores nothing, and
	// the callers below already handle a nil req.
	rec, perr := p.parkedPromptFor(ctx, ns, name, pl.RequestRef)
	if perr != nil {
		logger.Info("interaction decision: reading the parked prompt failed; continuing without it",
			"session", ref, "err", perr.Error())
	}
	// Idempotency, durable leg: the tombstone survives the restart that empties
	// resolvedCache. Without it a second approver's click re-ran the bound
	// handler in full — for tool_approval a FRESH grant tuple (TTL=0 for a
	// non-external tool is a session-wide backstop that silently auto-approves
	// the next identical call) plus a second, possibly contradicting, Applied.
	// The same guard covers the gate-side timeout path, which resolves the
	// prompt without ever touching resolvedCache.
	//
	// The clicker is told, but not by whom: the tombstone records that the
	// interaction was resolved, not who resolved it, and the surfaces render a
	// nil OriginalDecider as "someone else" rather than inventing an approver.
	if rec != nil && rec.Resolved {
		logger.Info("interaction decision: durably already resolved (parked-prompt tombstone); spectator")
		return p.publishDecisionRejected(ctx, env.Session, pl, "already_resolved", "already resolved", nil, "")
	}
	var req *channelevents.InteractionRequestPayload
	if rec != nil {
		var envelope channelevents.Envelope
		if err := json.Unmarshal(rec.Envelope, &envelope); err != nil {
			logger.Info("interaction decision: undecodable parked interaction request; skipping", "err", err.Error())
		} else {
			var rp channelevents.InteractionRequestPayload
			if err := json.Unmarshal(envelope.Payload, &rp); err == nil {
				req = &rp
			} else {
				logger.Info("interaction decision: undecodable parked interaction payload; skipping", "err", err.Error())
			}
		}
	}

	// Integrity: a decision must not resolve a pending request of a different
	// category. The category is what selects the standing check below, and the
	// CLICK supplies it — so without this tie a decision naming the weakest
	// policy in the registry resolves any RequestRef it likes under that
	// policy, and the strict gate the request was raised behind never runs.
	//
	// requestCategoryWitness reads the category off whichever durable record
	// the request was required to leave. Both are written before the prompt can
	// be seen, and neither is the click.
	if raised, ok := p.requestCategoryWitness(ctx, ns, name, pl.RequestRef, rec, req); ok && raised != pl.Category {
		logger.Info("interaction decision: category mismatches the request as raised; rejecting", "raisedCategory", raised)
		return p.publishDecisionRejected(ctx, env.Session, pl, "category_mismatch", "this action no longer matches the pending request", nil, "")
	}

	deciderCanon, err := identity.FromExternal(identity.Kind(pl.Decider.Kind), identity.TeamScope(pl.Decider.TeamScope), identity.RawExternalID(pl.Decider.ExternalID), identity.Email(pl.Decider.Email)).Canonical()
	if err != nil {
		logger.Info("interaction decision: decider has no canonical identity; rejecting", "err", err.Error())
		return p.publishDecisionRejected(ctx, env.Session, pl, "not_authorized", "your identity could not be verified", nil, "")
	}

	switch cat.Deciders {
	case channelinteractions.DecideOwner, channelinteractions.DecideApprovers:
		// Both policies resolve through the unified approver seam today: nil
		// resources = session approve-set (owner-derived).
		authorized, err := p.Engine.CheckApproverAuthorized(ctx, authz.SessionRef{Namespace: ns, Name: name}, nil, deciderCanon)
		if err != nil {
			return fmt.Errorf("interaction decision: standing check errored (session %s, category %s): %w", ref, pl.Category, err)
		}
		if !authorized {
			logger.Info("interaction decision: decider lacks standing; rejecting")
			return p.publishDecisionRejected(ctx, env.Session, pl, "not_authorized", "you are not authorized to decide this request", nil, "")
		}
	case channelinteractions.DecideRequester:
		// Canonicalization here does not opt into AllowSynthetic, so an
		// email-less requester (a guest or foreign-workspace identity) can never
		// canonicalize and can therefore never decide their own prompt — a
		// fail-closed default. A future guest-addressed category that needs this
		// must opt into synthetic identity explicitly, here.
		if req == nil || req.Audience.Requester == nil {
			logger.Info("interaction decision: no cached request/requester for requester-policy check; fail-closed reject")
			return p.publishDecisionRejected(ctx, env.Session, pl, "not_authorized", "the original request is no longer available; your standing could not be verified", nil, "")
		}
		reqCanon, err := identity.FromExternal(identity.Kind(req.Audience.Requester.Kind), identity.TeamScope(req.Audience.Requester.TeamScope),
			identity.RawExternalID(req.Audience.Requester.ExternalID), identity.Email(req.Audience.Requester.Email)).Canonical()
		if err != nil || reqCanon != deciderCanon {
			logger.Info("interaction decision: decider is not the addressee; rejecting")
			return p.publishDecisionRejected(ctx, env.Session, pl, "not_authorized", "this request was addressed to someone else", nil, "")
		}
	case channelinteractions.DecideParticipant:
		allowed, err := p.Engine.CheckSessionInteract(ctx,
			authz.SessionRef{Namespace: ns, Name: name}, deciderCanon, true)
		if err != nil {
			return fmt.Errorf("interaction decision: standing check errored (session %s, category %s): %w", ref, pl.Category, err)
		}
		if !allowed {
			logger.Info("interaction decision: decider lacks interact standing; rejecting")
			return p.publishDecisionRejected(ctx, env.Session, pl, "not_authorized", "you are not a participant of this session", nil, "")
		}
	case channelinteractions.DecidePlatformAdmin:
		// Asked of the platform singleton's start_session area (can_admin
		// arm) with full consistency — an admin may have been granted
		// moments before their click. Deliberately NOT the class's own
		// agentclass#start_session: its `starter` relation is the per-guest
		// start override, and a guest allowed to start must not thereby
		// admit other guests.
		authorized, err := p.Authz.CheckOnResource(ctx, "platform", "platform", "start_session", deciderCanon, true)
		if err != nil {
			return fmt.Errorf("interaction decision: platform-admin standing check errored (session %s, category %s): %w", ref, pl.Category, err)
		}
		if !authorized {
			logger.Info("interaction decision: decider is not a platform admin; rejecting")
			return p.publishDecisionRejected(ctx, env.Session, pl, "not_authorized", "only a platform admin can decide this request", nil, "")
		}
	case channelinteractions.DecideResourceOwners:
		// The decider must be an #owner of at least one resource named in the
		// request. Resolve that resource set from the in-process request cache
		// when warm, else from the durable memapproval record (cross-restart).
		resources, ok := p.resolveDecisionResources(ctx, ns, name, pl.RequestRef, req)
		if !ok {
			// The resource set is genuinely unrecoverable — neither the cached
			// request nor the durable record has it. Fail closed (deny +
			// non-silent reject); NEVER fall through with an empty set here,
			// which CheckApproverAuthorized would fold into the session-approve
			// gate and let a session owner self-approve access to a resource
			// they don't own (the 2026-07-02 wrong-fallback regression). This is
			// distinct from a warm cache whose Resources are legitimately empty
			// (resolveDecisionResources returns ok=true with an empty slice) —
			// that case folds to the session gate on purpose. The prompt stays
			// parked in PendingInteractions, so a later warm-cache click can
			// still resolve it.
			logger.Info("interaction decision: resource set unrecoverable (no cached request, no durable record); fail-closed reject")
			return p.publishDecisionRejected(ctx, env.Session, pl, "not_authorized", "standing could not be verified", nil, "")
		}
		authorized, err := p.Engine.CheckApproverAuthorized(ctx, authz.SessionRef{Namespace: ns, Name: name}, resources, deciderCanon)
		if err != nil {
			return fmt.Errorf("interaction decision: resource-owner standing check errored (session %s, category %s): %w", ref, pl.Category, err)
		}
		if !authorized {
			logger.Info("interaction decision: decider is not a resource owner; rejecting")
			return p.publishDecisionRejected(ctx, env.Session, pl, "not_authorized", "you are not an owner of the affected resource", nil, "")
		}
	default:
		return fmt.Errorf("interaction decision: category %q has unknown decider policy %q", pl.Category, cat.Deciders)
	}

	handler, bound := channelinteractions.HandlerFor(pl.Category)
	if !bound {
		return fmt.Errorf("interaction decision: category %q has no bound decision handler (session %s)", pl.Category, ref)
	}
	out, err := handler(ctx, channelinteractions.Decision{Session: env.Session, Payload: pl, Request: req})
	if err != nil {
		// A bound-handler runtime failure (e.g. a downstream grant-write error) is
		// surfaced to the clicker AND propagated: publish a per-clicker rejection
		// first so the click isn't silent, then return the wrapped error so the
		// relay's log + watchdog paths fire. The pending prompt is left intact — a
		// failed handler never resolves the card, so a retry can still act on it.
		//
		// A pin refusal (ErrSlotPinned) is NOT a transient render error: the slot
		// is committed elsewhere and, on the tool_approval path, the class has no
		// plan route to move it (see toolApprovalHandler). Classify it separately
		// so the clicker is told the real route — a new session — through the
		// handler's own instance-naming message, rather than a generic
		// "handler_error" the retry button cannot fix.
		// NB: the class string deliberately avoids a "slot_pin"/"slot_grant_"
		// prefix — the slot-write guard test (pkg/authz/slot_grant_guard_test.go)
		// flags any such literal as a hand-built tuple, and this is a reject code,
		// not a relation name.
		class, reason := "handler_error", err.Error()
		if errors.Is(err, authz.ErrSlotPinned) {
			class = "pin_refused"
		}
		if pubErr := p.publishDecisionRejected(ctx, env.Session, pl, class, reason, nil, ""); pubErr != nil {
			logger.Info("interaction decision: publish handler-error rejection failed", "err", pubErr.Error())
		}
		return fmt.Errorf("interaction decision: handler for %q errored (session %s): %w", pl.Category, ref, err)
	}
	if out.Suppressed {
		// Async: skip the synchronous applied publish; the bridge publishes it
		// later from the runner's interrupt_applied. Still dedupe the decision
		// (resolvedCache) so a second click doesn't fire a second interrupt, and
		// clear the pending prompt (the prompt has been acted on). Do NOT call
		// out.Validate() — a suppressed outcome carries no Result.
		p.resolvedCache.put(pl.Category, pl.RequestRef, resolvedDecision{Approver: pl.Decider, Decision: out.Result, ResolvedAt: p.Now()})
		p.clearPendingPrompt(ctx, ns, name, pl.RequestRef)
		return nil
	}
	if err := out.Validate(); err != nil {
		return fmt.Errorf("interaction decision: handler for %q returned invalid outcome (session %s): %w", pl.Category, ref, err)
	}

	applied := channelevents.InteractionAppliedPayload{
		// Stamp the authoritative env.Session-derived ref, not the click-supplied
		// pl.AgentSessionRef — the envelope's session is what routed this call.
		AgentSessionRef: channelevents.SessionRef{Namespace: ns, Name: name},
		Category:        pl.Category,
		RequestRef:      pl.RequestRef,
		Outcome:         out.Result,
		OutcomeText:     out.OutcomeText,
		Reason:          out.Reason,
		MintedURL:       out.MintedURL,
		DecidedBy:       &pl.Decider,
		ResponseRef:     pl.ResponseRef,
	}
	// Publish first (runner resume + surface edit), THEN clear the pending
	// prompt — the ordering contract shared with identity_choice.go: a clear
	// before a failed publish would strand the session with nothing left to
	// re-surface.
	if err := channelevents.PublishIn(p.NATS.Publish, ns, name, channelevents.KindInteractionApplied, applied); err != nil {
		return fmt.Errorf("interaction decision: publish applied (in) failed (session %s): %w", ref, err)
	}
	if err := channelevents.PublishOut(p.NATS.Publish, ns, name, channelevents.KindInteractionApplied, applied); err != nil {
		return fmt.Errorf("interaction decision: publish applied (out) failed (session %s): %w", ref, err)
	}
	p.resolvedCache.put(pl.Category, pl.RequestRef, resolvedDecision{
		Approver:   pl.Decider,
		Decision:   out.Result,
		ResolvedAt: p.Now(),
	})
	p.clearPendingPrompt(ctx, ns, name, pl.RequestRef)
	// Durable guard: drop the parked PendingInteractions entry (generic — a
	// harmless no-op when the category never parked one, e.g.
	// identity_choice/permission_request). Applied has already published
	// above — the runner has resumed and the surface has updated — so a
	// clear failure here must not read back as "the decision failed"; log
	// with full context per AGENTS.md's no-silent-errors rule instead of
	// returning an error at this point.
	if err := p.clearPendingInteraction(ctx, ns, name, pl.RequestRef); err != nil {
		logger.Info("interaction decision: clear pending-interaction entry failed", "err", err.Error())
	}
	return nil
}

// requestCategoryWitness returns the category a request was RAISED under, and
// whether any durable record witnesses it. It is the tie between a RequestRef
// and the standing check that guards it; the click's own claim is never an
// input.
//
// Two witnesses, because neither covers every category on its own and each is
// written before the prompt can be seen:
//
//   - The parked prompt, for every ResurfaceCached category
//     (outbound/relay.go's notePendingPrompt, called BEFORE dispatch). Category
//     is a top-level field on the stored record, so it survives an envelope
//     body that will not parse — reading it from the DECODED payload instead is
//     what let an unreadable record skip this check entirely and hand the
//     decision to the click's own policy.
//   - The AgentSession's PendingInteractions entry, for every PARKING category
//     (interaction_request.go, likewise before delivery). K8s-witnessed, so it
//     is also the leg that still binds when memory is absent or its read failed.
//
// A category that parks, caches, or keeps a pending-requester entry therefore
// cannot be substituted for.
//
// WHAT IS STILL UNWITNESSED, named exactly rather than approximately: a
// category with Park "", ResurfaceNone and no record of its own. Today that is
// queued_messages, provider_error_retry (both DecideParticipant) and
// portal_access (DecideRequester). An earlier version of this comment claimed
// the first two were the whole list; permission_request and session_release are
// also witness-less by those two fields and are both DecideOwner, which is why
// the PendingRequesters leg below exists at all.
//
// For the three that remain, a substitution buys nothing: the two participant
// categories fire mid-turn against a live session and confer no standing the
// clicker lacks, and portal_access is addressed to the requester who is already
// the only party its handler acts for. session_release is NOT in that set --
// see the comment on its own leg. Refusing an unwitnessed category outright
// would break the interrupt path, so the residual is accepted and bounded here
// rather than hidden.
func (p *Pipeline) requestCategoryWitness(ctx context.Context, ns, name, requestRef string,
	rec *parkedprompt.Content, req *channelevents.InteractionRequestPayload) (string, bool) {
	if requestRef == "" {
		return "", false
	}
	if rec != nil && rec.Category != "" {
		return rec.Category, true
	}
	// A record written before Category was stored on it: the decoded payload is
	// the same claim from the same writer, so it is no weaker a witness.
	if req != nil && req.Category != "" {
		return req.Category, true
	}
	if p.K8s == nil {
		return "", false
	}
	var sess spiceboxv1alpha1.AgentSession
	if err := p.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		// Fail SILENT here, not open: the caller only skips the mismatch reject,
		// and every decider policy below still runs its own SpiceDB check. Log
		// it so a session that lost this leg is greppable.
		log.FromContext(ctx).Info("interaction decision: reading the session for the category witness failed",
			"session", ns+"/"+name, "requestRef", requestRef, "err", err.Error())
		return "", false
	}
	for _, pi := range sess.Status.PendingInteractions {
		if pi.RequestID == requestRef && pi.Category != "" {
			return pi.Category, true
		}
	}
	// Third witness: a pending requester entry. permission_request is
	// DecideOwner and neither parks nor caches, so the two witnesses above miss
	// it entirely — a participant could resolve an owner's card by claiming a
	// participant-policy category with its RequestRef, poisoning resolvedCache
	// so the owner's own click comes back "already_resolved". It keeps its own
	// durable record on the session, and decidePermission already matches on
	// exactly this field, so consulting it here costs one loop. The entry
	// carries the category it was raised under (start_approval rides the same
	// list); entries written before the field existed are join requests.
	for _, pr := range sess.Status.PendingRequesters {
		if pr.RequestRef == requestRef {
			if pr.Category != "" {
				return pr.Category, true
			}
			return categories.PermissionRequest, true
		}
	}
	return "", false
}

// parkedPromptFor returns the durable parked-prompt record for requestRef,
// INCLUDING a resolved tombstone, or nil when no record was ever written.
//
// parkedprompt.Outstanding deliberately hides tombstones: it serves
// re-surfacing, which must never replay a decided prompt. The decision pipe
// needs the opposite — the tombstone IS the durable witness that this
// interaction was already resolved, and the only one that outlives the process
// (clearPendingPrompt writes it on both the decision and the timeout path, and
// the outbound relay notes the record BEFORE the prompt is dispatched to a
// sender, so a clickable card always has one). Reading the kind directly is
// what gives the pipe the tri-state it needs: absent = never noted (not a
// verdict), Resolved=false = live, Resolved=true = decided.
//
// A read error yields (nil, err); callers log and proceed, so a memory blip
// degrades to the pre-existing in-process-only idempotency rather than
// refusing a legitimate decision.
func (p *Pipeline) parkedPromptFor(ctx context.Context, ns, name, requestRef string) (*parkedprompt.Content, error) {
	if p.Mem == nil || requestRef == "" {
		return nil, nil
	}
	res, err := p.Mem.Query(ctx, memory.Query{
		Scope: promptScope(ns, name),
		Kinds: []string{parkedprompt.KindName},
	})
	if err != nil {
		return nil, fmt.Errorf("query parked prompts (session %s/%s): %w", ns, name, err)
	}
	for _, e := range res.Entries {
		var c parkedprompt.Content
		if err := json.Unmarshal(e.Content, &c); err != nil {
			// One unreadable record must not cost the caller the one it wants —
			// the same tolerance parkedprompt.Outstanding applies.
			log.FromContext(ctx).Info("interaction decision: undecodable parked-prompt record; skipping",
				"session", ns+"/"+name, "entry", e.ID, "err", err.Error())
			continue
		}
		if c.RequestRef == requestRef {
			return &c, nil
		}
	}
	return nil, nil
}

// resolveDecisionResources yields the resource set for a resource-owner
// decision. From the cached request when present (even an empty Resources
// slice — a legitimate no-resource gate that folds to the session
// approve-set). On a cache miss it recovers Resources from the durable
// memapproval record (cross-restart). ok=false ⇒ NEITHER source has it, so
// the pipe fails closed rather than handing an empty set to the approver gate.
func (p *Pipeline) resolveDecisionResources(ctx context.Context, ns, name, requestRef string, req *channelevents.InteractionRequestPayload) ([]authz.ApproverResourceRef, bool) {
	if req != nil {
		out := make([]authz.ApproverResourceRef, 0, len(req.Resources))
		for _, r := range req.Resources {
			out = append(out, authz.ApproverResourceRef{Type: r.Type, ID: r.ID, Permission: r.Permission})
		}
		return out, true
	}
	if p.Mem == nil {
		return nil, false
	}
	rec, err := memapproval.RequestByID(ctx, p.Mem, memory.Scope{Kind: "session", ID: ns + "/" + name}, requestRef)
	if err != nil || rec == nil {
		if err != nil {
			log.FromContext(ctx).Info("interaction decision: durable resource recovery errored",
				"requestRef", requestRef, "err", err.Error())
		}
		return nil, false
	}
	out := make([]authz.ApproverResourceRef, 0, len(rec.Resources))
	for _, r := range rec.Resources {
		// The durable record stores the same wire type, so the declared
		// approverPermission survives a restart with the rest of the ref.
		out = append(out, authz.ApproverResourceRef{Type: r.Type, ID: r.ID, Permission: r.Permission})
	}
	return out, true
}

// publishDecisionRejected emits a per-clicker KindInteractionDecisionRejected on
// OUT so the surface can tell the clicker why their click did nothing. The
// pending prompt is UNCHANGED — a rejected click never resolves the card, so the
// real approver can still act. Introduced here so the fail-closed resource-owner
// path is non-silent from this boundary; the surface rendering + the remaining
// reject paths are wired separately.
func (p *Pipeline) publishDecisionRejected(ctx context.Context, sess channelevents.SessionRef, pl channelevents.InteractionDecisionPayload, class, reason string, orig *channelevents.ExternalIdentity, origOutcome string) error {
	rej := channelevents.InteractionDecisionRejectedPayload{
		AgentSessionRef: sess,
		Category:        pl.Category,
		RequestRef:      pl.RequestRef,
		Clicker:         pl.Decider,
		ResponseRef:     pl.ResponseRef,
		Class:           class,
		Reason:          reason,
		OriginalDecider: orig,
		OriginalOutcome: origOutcome,
	}
	if err := channelevents.PublishOut(p.NATS.Publish, sess.Namespace, sess.Name, channelevents.KindInteractionDecisionRejected, rej); err != nil {
		return fmt.Errorf("publish interaction_decision_rejected (session %s/%s): %w", sess.Namespace, sess.Name, err)
	}
	return nil
}
