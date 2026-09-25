package pipeline

import (
	"context"
	"encoding/json"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/parkedprompt"
	"github.com/authzed/openagentprimitives/pkg/x/besteffort"
)

// promptScope is the memory scope a session's parked prompts live under — the
// same "session" scope the memapproval records use.
func promptScope(ns, name string) memory.Scope {
	return memory.Scope{Kind: "session", ID: ns + "/" + name}
}

// nowOrWallClock reads the injected clock, falling back to the wall clock when
// none is wired. NewPipeline always sets Now; a Pipeline assembled field-by-field
// (every test that does not go through it) does not, and an expiry check is no
// reason to panic a re-surface — the one path whose whole job is to stop a user
// being stranded.
func (p *Pipeline) nowOrWallClock() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

// resurfacePending re-posts whatever prompt the session is currently blocked
// on, so it reaches a user who just re-interacted or a surface that just
// attached (Slack ephemerals only reach the device active at send time; a
// browser tab that connects after the one-shot publish saw nothing at all).
//
// Two legs, split on the category's declared Resurface policy, and NEITHER
// depends on process-local state — which is what makes a parked prompt survive
// a channelsd restart:
//
//   - regenerate: asks the registry which categories park at this phase and
//     rebuild themselves, then invokes each bound regenerator. Reads no storage
//     at all; the session's own phase is the entire input.
//   - cached: reads the durable parked_prompt records and republishes them, for
//     the categories that have nothing to rebuild from.
//
// There is no per-category branch here: a new category is a row in the
// registry. Best-effort throughout — failures are logged, never fatal. It acts
// only in parked phases (never Running), so Deliver's enqueue-ack path is
// untouched.
func (p *Pipeline) resurfacePending(ctx context.Context, active *spiceboxv1alpha1.AgentSession) {
	p.resurfaceRegenerate(ctx, active)
	p.resurfaceCached(ctx, active)
}

// resurfaceRegenerate rebuilds every prompt whose category both parks at the
// session's current phase and declares ResurfaceRegenerate — credential_link
// today.
//
// It is deliberately storage-free. The regenerator reconstructs the prompt from
// live durable sources (ForcePublish re-reads the SessionUserIdentity and mints
// a FRESH signed link), so the phase alone drives it. That is also the security
// property: the credential link is never persisted in replayable form, so it
// cannot be replayed out of storage.
func (p *Pipeline) resurfaceRegenerate(ctx context.Context, active *spiceboxv1alpha1.AgentSession) {
	logger := log.FromContext(ctx)
	sessRef := active.Namespace + "/" + active.Name
	for _, cat := range channelinteractions.RegenerateAtPark(active.Status.Phase) {
		r, ok := channelinteractions.RegeneratorFor(cat.Name)
		if !ok {
			logger.Info("resurface: category parks here and regenerates, but no regenerator is bound; skipping",
				"session", sessRef, "category", cat.Name, "phase", active.Status.Phase)
			continue
		}
		// The regenerator owns the whole payload, so it is handed only the
		// category being rebuilt — there is no prior request to pass along,
		// which is precisely why this works from a cold start.
		req := &channelevents.InteractionRequestPayload{
			AgentSessionRef: channelevents.SessionRef{Namespace: active.Namespace, Name: active.Name},
			Category:        cat.Name,
		}
		if err := r(ctx, active, req); err != nil {
			logger.Info("resurface: regenerator errored", "session", sessRef, "category", cat.Name, "err", err.Error())
		}
	}
}

// resurfaceCached republishes the durable parked_prompt records whose category
// parks at the session's current phase.
//
// A nil Mem is a LOGGED no-op, not a silent one: the cached categories
// (identity_choice, tool_approval, info_leakage, content_inspection) then have
// no way to re-surface, and an operator reading logs should see why.
func (p *Pipeline) resurfaceCached(ctx context.Context, active *spiceboxv1alpha1.AgentSession) {
	logger := log.FromContext(ctx)
	sessRef := active.Namespace + "/" + active.Name
	if p.Mem == nil {
		logger.Info("resurface: no memory facade wired; cached prompts cannot be re-surfaced", "session", sessRef)
		return
	}
	scope := promptScope(active.Namespace, active.Name)
	prompts, err := parkedprompt.Outstanding(ctx, p.Mem, scope)
	if err != nil {
		logger.Info("resurface: reading parked prompts failed", "session", sessRef, "err", err.Error())
		return
	}
	pendingIDs := pendingInteractionRequestIDs(active.Status)
	now := p.nowOrWallClock()
	for _, c := range prompts {
		cat, ok := channelinteractions.Get(c.Category)
		if !ok || cat.Park == "" || !parkedHere(cat, active.Status, pendingIDs, c.RequestRef) {
			continue
		}
		var env channelevents.Envelope
		if err := json.Unmarshal(c.Envelope, &env); err != nil {
			logger.Info("resurface: undecodable parked prompt; skipping",
				"session", sessRef, "requestRef", c.RequestRef, "err", err.Error())
			continue
		}
		if promptExpired(env, now) {
			// The prompt's own window has lapsed — its link is dead or its
			// approval already timed out — so re-posting it would hand the user
			// a card that cannot work. Nothing else prunes these.
			logger.Info("resurface: parked prompt has expired; resolving instead of re-posting",
				"session", sessRef, "requestRef", c.RequestRef)
			besteffort.Log(logger.Info, "resolveExpiredPrompt",
				parkedprompt.Resolve(ctx, p.Mem, scope, c.RequestRef), "session", sessRef)
			continue
		}
		// Durable stale-guard for the runner-approval park phase: a prompt whose
		// decision already landed (possibly on another replica) is no longer in
		// PendingInteractions — do not resurrect it. Categories parking
		// elsewhere (identity_choice at AwaitingIdentityChoice) write no
		// PendingInteractions entry, so the cross-check is scoped to the
		// AwaitingDecision park only.
		if cat.Park == spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision && !pendingIDs[c.RequestRef] {
			logger.Info("resurface: interaction decision already landed; not republishing",
				"session", sessRef, "requestRef", c.RequestRef)
			continue
		}
		p.republishPrompt(ctx, active, env, c)
	}
}

// parkedHere reports whether a cached prompt of category cat is the one the
// session is currently waiting on, and so should be re-posted to a surface
// that just attached.
//
// The ordinary answer is the phase: a prompt parks the session at cat.Park,
// and the session sitting at that phase means the prompt is still the thing
// blocking it. The exception is the runner going to sleep. Idle-sleep reaps
// the runner pod between turns and the phase follows it to Idle, but a
// decision the runner was waiting on does not go away with the pod: the ask
// stays listed in Status.PendingInteractions until it is answered. Observed
// live: a plan-phase approval asked, the runner asleep, the person reloading
// the page and seeing nothing, because Idle != AwaitingDecision skipped the
// only prompt that could have woken it. So for AwaitingDecision-park
// categories the durable PendingInteractions entry is the authority, whatever
// the sleeping runner left the phase at; resurfaceCached's already-landed
// guard still drops the prompt once that entry is gone.
func parkedHere(cat channelinteractions.Category, st spiceboxv1alpha1.AgentSessionStatus, pendingIDs map[string]bool, requestRef string) bool {
	if cat.Park == st.Phase {
		return true
	}
	return cat.Park == spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision && pendingIDs[requestRef]
}

// promptExpired reports whether the prompt's own ExpiresAt has passed. An
// envelope whose payload will not decode, or which carries no expiry, is
// treated as live: dropping a prompt on a decode failure would be the silent
// hang this path exists to prevent.
func promptExpired(env channelevents.Envelope, now time.Time) bool {
	var pl channelevents.InteractionRequestPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil || pl.ExpiresAt == nil {
		return false
	}
	return pl.ExpiresAt.Before(now)
}

// republishPrompt re-publishes a stored prompt envelope on its OUT subject so
// the outbound relay re-renders it. Interruptible prompts get a fresh interrupt
// RequestID stamped so the renderer appends an "Interrupt & Send Now" button;
// non-interruptible prompts (identity choice) go out verbatim.
func (p *Pipeline) republishPrompt(ctx context.Context, active *spiceboxv1alpha1.AgentSession, env channelevents.Envelope, c parkedprompt.Content) {
	if c.Interruptible {
		env.ResurfaceInterruptRequestID = mintRequestID()
	}
	sessRef := active.Namespace + "/" + active.Name
	data, err := json.Marshal(env)
	if err != nil {
		log.FromContext(ctx).Info("resurfacePending: marshal envelope failed",
			"session", sessRef, "requestRef", c.RequestRef, "err", err.Error())
		return
	}
	subj := channelevents.SubjectOut(channelevents.SubjectPrefix(active.Namespace, active.Name), env.Kind)
	besteffort.Log(log.FromContext(ctx).Info, "resurfaceRepublish", p.NATS.Publish(subj, data),
		"session", sessRef, "requestRef", c.RequestRef, "kind", string(env.Kind))
}

// pendingInteractionRequestIDs returns the set of still-pending generic
// interaction RequestIDs (the durable authority for the cached leg's
// stale-guard — a click that resolved the interaction between the note and this
// read must not resurrect it).
func pendingInteractionRequestIDs(st spiceboxv1alpha1.AgentSessionStatus) map[string]bool {
	ids := map[string]bool{}
	for _, e := range st.PendingInteractions {
		ids[e.RequestID] = true
	}
	return ids
}
