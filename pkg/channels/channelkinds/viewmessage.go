package channelkinds

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
)

// ViewMessageTimeout bounds the wait for channelsd's reply. The reply comes
// only after the whole inbound path runs — correlation lookups, the
// fully-consistent SpiceDB Interact check, the signed memory append — so the
// budget must cover that path under load. On a single-node cluster those
// backends stay slow for a minute or two after a restart while SpiceDB, the
// memory store, and the apiserver caches warm up, and a budget short enough to
// expire during warm-up reports "failed to send" for a message that was in
// fact delivered. 30s clears warm-up while still being short enough that a
// browser user sees a real failure rather than an indefinite spinner. A
// timeout surfaces as OutcomeInternalError, never as silence.
const ViewMessageTimeout = 30 * time.Second

// DecisionToWire projects an InboundDecision onto the transport payload; only
// caller-visible fields cross the wire.
//
// Notice is flattened to NoticeWire here because *notice.Notice resolves
// severity from a registry the far side may not have (a browser has none), so
// the boundary denormalises severity and glyph. A nil or suppressed notice
// yields a nil wire — nothing to draw.
func DecisionToWire(d InboundDecision) channelevents.ViewMessageResultPayload {
	return channelevents.ViewMessageResultPayload{
		Outcome:              d.Outcome.String(),
		Notice:               d.Notice.ToWire(),
		NewSession:           d.NewSession,
		RequesterCanonicalID: d.RequesterCanonicalID,
	}
}

// DecisionFromWire is the inverse. The bool reports whether the wire's Outcome
// label was recognized; an unrecognized label yields OutcomeUnknown and false,
// never a guess. SessionInfo is intentionally NOT carried: the caller already
// knows (ns, name) — it addressed the request with them.
//
// The rebuilt Notice is renderable but not re-publishable (see notice.FromWire):
// a view surface displays what channelsd decided, it does not re-emit it.
func DecisionFromWire(p channelevents.ViewMessageResultPayload) (InboundDecision, bool) {
	outcome, ok := ParseOutcome(p.Outcome)
	return InboundDecision{
		Outcome:              outcome,
		Notice:               notice.FromWire(p.Notice),
		NewSession:           p.NewSession,
		RequesterCanonicalID: p.RequesterCanonicalID,
	}, ok
}

// RequestViewMessage submits a view-originated user message to channelsd and
// returns the resulting InboundDecision.
//
// This is the ONE helper both view surfaces call — the browser chat and the
// `oap` TUI. Neither may write the session's memory itself: that would require
// the per-session Ed25519 audit-signing seed, and a user-facing process must
// not hold a credential that can forge the audit chain.
func RequestViewMessage(deps Deps, ns, name, text, via string, ext ExternalIdentity, deferEcho bool, requestID string) (InboundDecision, error) {
	if deps.NATSRequest == nil {
		return InboundDecision{Outcome: OutcomeInternalError},
			errors.New("channelkinds: RequestViewMessage: no NATS request transport wired")
	}
	pl := channelevents.ViewMessagePayload{
		Text:      text,
		Via:       via,
		DeferEcho: deferEcho,
		RequestID: requestID,
		Author: channelevents.ExternalIdentity{
			Kind:       ext.Kind,
			ExternalID: ext.ExternalID,
			Email:      ext.Email,
			TeamScope:  ext.TeamScope,
		},
	}
	raw, err := channelevents.RequestIn(deps.NATSRequest, ns, name,
		channelevents.KindViewMessage, pl, ViewMessageTimeout)
	if err != nil {
		return InboundDecision{Outcome: OutcomeInternalError},
			fmt.Errorf("channelkinds: view_message request: %w", err)
	}
	var res channelevents.ViewMessageResultPayload
	if err := json.Unmarshal(raw, &res); err != nil {
		return InboundDecision{Outcome: OutcomeInternalError},
			fmt.Errorf("channelkinds: view_message decode reply: %w", err)
	}
	dec, ok := DecisionFromWire(res)
	// A handler-side failure travels in res.Error, not in the transport. Check
	// it BEFORE the label check: channelsd may legitimately send an error with
	// an outcome we understand, and its reason is the more useful message.
	if res.Error != "" {
		return dec, fmt.Errorf("channelkinds: view_message rejected by channelsd: %s", res.Error)
	}
	if !ok {
		// Version skew or corruption. Never let an unrecognized label read as
		// success — dec.Outcome is OutcomeUnknown, and the caller learns why.
		return dec, fmt.Errorf("channelkinds: view_message reply has unrecognized outcome %q", res.Outcome)
	}
	return dec, nil
}
