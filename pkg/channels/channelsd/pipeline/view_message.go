package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/viewurn"
)

// HandleViewMessage services ap.session.<ns>.<name>.in.view_message — a user
// message injected by a browser/TUI *view* rather than by the Channel's own
// listener.
//
// It reconstructs the InboundEvent from the session's own input binding, so
// Deliver's correlation lookup (LabelChannelName + channelkey.LabelValue),
// the fully-consistent Interact hook, the signed memory append, and wake all
// run unchanged — exactly the same inbound path a real channel listener
// drives.
//
// Author on the payload is a CLAIM. NATS is a trusted-publisher bus, so any
// publisher able to reach .in.view_message could name any author. The canonical
// subject is therefore RE-DERIVED here, with allowSynthetic gated on the
// SESSION's own K8s-witnessed InputChannel.Kind via
// channelkinds.Kind.AllowsSyntheticIdentity() — never on the payload's claimed
// Author.Kind. Kinds whose users are legitimately email-less (the `local` TUI on
// a no-IdP cluster) opt in; kinds whose users always carry a verified email do
// not, so an email-less claim there fails closed on an unresolvable subject.
// Deliver's own Interact re-check is the second, independent gate: the
// re-derivation and that check — never the claim — are the authorization.
//
// The SESSION this delivers into comes from env.Session, which is sound only
// because the bus wrapper (internal/cmd/channelsd/main.go's respondingHandler) has
// already cross-checked it against the NATS subject and refused any mismatch —
// the same rule the outbound relay applies. The claim re-derivation above is
// about WHO is speaking; the subject check is about WHICH session hears it.
//
// Both return values are meaningful: the error goes to channelsd's log, and
// the payload's Error field goes back to the requester so it surfaces a real
// reason instead of waiting out its timeout (AGENTS.md: no silent errors).
func (p *Pipeline) HandleViewMessage(ctx context.Context, env channelevents.Envelope) (channelevents.ViewMessageResultPayload, error) {
	var pl channelevents.ViewMessagePayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return viewMessageFail(fmt.Errorf("view_message: decode payload: %w", err))
	}

	// Attestation always reauthorizes; ordinary messages deduplicate by request ID.
	if pl.AttestOnly || pl.RequestID == "" || p.viewDedup == nil {
		return p.deliverViewMessage(ctx, env, pl)
	}

	// Idempotency, keyed by the client's RequestID (reused across the chat's
	// Retry), in TWO layers so NEITHER path double-posts to the agent:
	//   1. Completion cache — a Retry that lands AFTER the first reply already
	//      returned replays the cached decision WITHOUT re-delivering.
	//   2. Single-flight — a Retry that RACES the first delivery while it is still
	//      in flight (before it has cached) JOINS the same call and shares its
	//      real result, closing the in-flight window the cache alone leaves open.
	if cached, ok := p.viewDedup.get(pl.RequestID); ok {
		return cached, nil
	}
	v, err, _ := p.viewFlight.Do(pl.RequestID, func() (any, error) {
		// A concurrent flight may have finished + cached between our get() and here.
		if cached, ok := p.viewDedup.get(pl.RequestID); ok {
			return cached, nil
		}
		return p.deliverViewMessage(ctx, env, pl)
	})
	res, _ := v.(channelevents.ViewMessageResultPayload)
	return res, err
}

// deliverViewMessage performs the actual (non-idempotent) delivery: resolve the
// session + channel, re-derive + re-authorize the subject, Deliver to the agent,
// mirror the echo, and cache the successful result for RequestID replay. All
// idempotency gating lives in HandleViewMessage; this ALWAYS delivers.
func (p *Pipeline) deliverViewMessage(ctx context.Context, env channelevents.Envelope, pl channelevents.ViewMessagePayload) (channelevents.ViewMessageResultPayload, error) {
	fail := viewMessageFail
	ns, name := env.Session.Namespace, env.Session.Name

	var sess spiceboxv1alpha1.AgentSession
	if err := p.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		return fail(fmt.Errorf("view_message: get session %s/%s: %w", ns, name, err))
	}
	if sess.Spec.InputChannel == nil {
		return fail(fmt.Errorf("view_message: session %s/%s has no input channel binding", ns, name))
	}

	var ch spiceboxv1alpha1.Channel
	if err := p.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: sess.Spec.InputChannel.Name}, &ch); err != nil {
		return fail(fmt.Errorf("view_message: get channel %s/%s: %w", ns, sess.Spec.InputChannel.Name, err))
	}

	// Re-derive the canonical subject. AllowSynthetic is opted into ONLY when
	// the SESSION's own witnessed kind (ch.Spec.Kind, never the payload's
	// pl.Author.Kind) declares itself synthetic-tolerant. A registry miss fails
	// closed: minting a synthetic subject for an identity that should have
	// carried a verified email writes and checks a subject that at best
	// resolves to no grant, and at worst collides with one an attacker shaped.
	principal := identity.FromExternal(identity.Kind(pl.Author.Kind), identity.TeamScope(pl.Author.TeamScope), identity.RawExternalID(pl.Author.ExternalID), identity.Email(pl.Author.Email))
	if k, ok := chregistry.Get(ch.Spec.Kind); ok && k.AllowsSyntheticIdentity() {
		principal = principal.AllowSynthetic()
	}
	if _, err := principal.Canonical(); err != nil {
		return fail(fmt.Errorf("view_message: unresolvable author identity: %w", err))
	}

	// Via is server-minted by webd/ap, but re-validate fail-closed here: a
	// malformed Via on the wire is a crafted publish, and Via renders into
	// Slack + the agent's context (injection surfaces). Empty is tolerated.
	if pl.Via != "" {
		if _, err := viewurn.Parse(pl.Via); err != nil {
			return fail(fmt.Errorf("view_message: invalid via %q: %w", pl.Via, err))
		}
	}

	// SECURITY: authorSubject derives Turn.Author from ExternalID alone and
	// does NOT fall back to Email, so a payload carrying Email but no
	// ExternalID would pass Canonical() and the Interact re-check (both keyed
	// off the email-derived subject) and then append a Turn with an EMPTY
	// Author. advanceRequester treats an empty author as a no-op and keeps the
	// session's PRIOR requester — so under authSubjectMode=currentRequester
	// this lower-privileged sender's later tool calls would authorize as
	// whoever sent the previous turn. Defaulting ExternalID to Email keeps the
	// identifier passed to Deliver in agreement with the subject already
	// authorized above. A no-op for honest callers, which already set
	// ExternalID = Email whenever Email is used.
	externalID := pl.Author.ExternalID
	if externalID == "" {
		externalID = identity.RawExternalID(pl.Author.Email)
	}

	if pl.AttestOnly {
		canonical, err := principal.Canonical()
		if err != nil {
			return fail(err)
		}
		if p.Authz == nil {
			return fail(fmt.Errorf("view_message: attestation requires authorization"))
		}
		allowed, err := p.Authz.CheckInteract(ctx, ns, name, canonical, true)
		if err != nil {
			return fail(fmt.Errorf("view_message: attestation interact check: %w", err))
		}
		if !allowed {
			return channelkinds.DecisionToWire(channelkinds.InboundDecision{Outcome: channelkinds.OutcomeDeniedByPermission}), nil
		}
		if err := p.recordGoalActor(ctx, &sess, channelkinds.InboundEvent{ExternalIDs: channelkinds.ExternalIdentity{
			Kind: pl.Author.Kind, ExternalID: externalID, Email: pl.Author.Email, TeamScope: pl.Author.TeamScope,
		}}); err != nil {
			return fail(err)
		}
		return channelkinds.DecisionToWire(channelkinds.InboundDecision{Outcome: channelkinds.OutcomeRouted, RequesterCanonicalID: canonical.String()}), nil
	}

	dec, err := p.Deliver(ctx, channelkinds.InboundEvent{
		Channel:     &ch,
		ChannelKey:  sess.Spec.InputChannel.Key,
		MessageText: pl.Text,
		Via:         pl.Via,
		ExternalIDs: channelkinds.ExternalIdentity{
			Kind:       pl.Author.Kind,
			ExternalID: externalID,
			Email:      pl.Author.Email,
			TeamScope:  pl.Author.TeamScope,
		},
	})
	if err != nil {
		// No res.Outcome == "" fallback: every Deliver error return sets an
		// explicit Outcome, and Outcome.String() never returns "" (its
		// zero-value fallback is "unknown"), so such a guard would be dead code.
		res := channelkinds.DecisionToWire(dec)
		res.Error = err.Error()
		return res, fmt.Errorf("view_message: deliver: %w", err)
	}

	// Mirror a view-originated message back to the origin channel so its
	// participants see it. Only for a routed message carrying a Via, and not
	// when the publisher set DeferEcho — a structured payload whose
	// human-readable mirror the runner authors instead. A failed echo publish is
	// logged, never fatal: the turn is already durably routed.
	if pl.Via != "" && dec.Outcome == channelkinds.OutcomeRouted && !pl.DeferEcho {
		echo := channelevents.UserEchoPayload{Text: pl.Text, Author: pl.Author, Via: pl.Via, RequestID: pl.RequestID}
		if err := channelevents.PublishOut(p.NATS.Publish, ns, name, channelevents.KindUserEcho, echo); err != nil {
			log.FromContext(ctx).Info("view_message: publish user_echo failed; message routed but not mirrored",
				"session", ns+"/"+name, "err", err.Error())
		}
	}
	res := channelkinds.DecisionToWire(dec)
	// Cache this successful delivery so a retry with the same id replays it
	// instead of re-delivering (Deliver already succeeded above; err paths
	// returned earlier and are intentionally NOT cached, so they can retry).
	if pl.RequestID != "" && p.viewDedup != nil {
		p.viewDedup.put(pl.RequestID, res)
	}
	return res, nil
}

// viewMessageFail builds the internal-error result payload the requester surfaces
// (its Error field) so a failed view_message returns a real reason instead of
// waiting out its timeout (AGENTS.md: no silent errors).
func viewMessageFail(err error) (channelevents.ViewMessageResultPayload, error) {
	return channelevents.ViewMessageResultPayload{
		Outcome: channelkinds.OutcomeInternalError.String(),
		Error:   err.Error(),
	}, err
}
