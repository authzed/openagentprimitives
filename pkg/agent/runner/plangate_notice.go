package runner

// plangate_notice.go tells a human when their plan approval did not take
// effect.
//
// The gate fails closed by design: if it cannot establish which resources the
// approver is entitled to share, it records nothing rather than granting
// something nobody authorized. That half is right. What was missing is the
// other half — saying so. An approval that vanishes silently leaves the person
// who clicked Approve looking at the identical card again, with no signal that
// anything went wrong, so they approve again, and again. The gate looks like it
// is working; the human is the one being made to repeat themselves.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// approverIDOf renders a principal for a LOG line — never for a chat surface,
// and never for an authorization call. Best effort: a principal that cannot
// canonicalize still needs to appear in the log that says so.
func approverIDOf(p identity.Principal) string {
	if e := p.Email(); e != "" {
		return string(e)
	}
	if id := p.ExternalID(); id != "" {
		return string(id)
	}
	if c, err := p.Canonical(); err == nil {
		return c.String()
	}
	return "(unidentified)"
}

// publishPlanGateApprovalFailed tells the session that an approval was received
// and could not be applied.
//
// Best-effort by the same rule as every other gate write — a notice that fails
// to publish must not take down the turn — but it must never be silent, so a
// failure to deliver the failure is itself logged.
//
// The text names no internal machinery. "The permission service rejected the
// lookup" is not something the person clicking Approve can act on, and this
// surface is a chat window, not an operator console; the operator detail is in
// the log line beside this call.
//
// pinnedRefusal branches the copy, because the reasons an approval fails to
// apply fall into two opposite buckets and the old single wording lied about
// both halves for one of them:
//
//   - a RECOVERABLE FAULT (pinnedRefusal=false): a standing lookup blipped, or
//     an approved MOVE could not execute because the pin drifted since the card
//     was shown. Nothing bound, and re-approving can take once the transient
//     condition clears — "a fault on our side, try once more" is true.
//   - a PLAIN PINNED REFUSAL (pinnedRefusal=true): a binding named a different
//     instance of a slot already committed to another, with no move approved.
//     This is NOT a fault, re-approving the same card will not change it, and
//     because GrantSlots is per-type partitioned OTHER instances the plan named
//     may well have bound — so "nothing was granted" would be a lie.
func (h *runnerHost) publishPlanGateApprovalFailed(ctx context.Context, cause error, pinnedRefusal bool) {
	if h.l == nil || h.l.InteractionRequestPublish == nil {
		return
	}
	sessNS, sessName := h.l.SessionKey.Namespace, h.l.SessionKey.Name
	if sessNS == "" && sessName == "" {
		sessNS, sessName = h.sess.Namespace, h.sess.Name
	}

	// Recoverable-fault copy (the default): nothing bound, and re-approving may
	// take. Bounded rather than "retry"/"don't retry" — one more attempt
	// distinguishes a transient blip from a persistent one; a third never does.
	lead := "Your approval could not be applied, so nothing was granted and the plan " +
		"has not started. This is a fault on our side, not a refusal."
	nextStep := "Try approving once more. If it fails again, report it — nothing " +
		"has been granted either way."
	if pinnedRefusal {
		// A plain pinned refusal: the slot is committed elsewhere and this is an
		// answer, not a fault. Honest that other instances may have bound and that
		// re-approving the SAME card changes nothing.
		lead = "This session is already committed to a different target for one of its " +
			"slots, so the instance your approval named was not granted. Other instances " +
			"the plan named may have been granted; this one was not."
		nextStep = "To target the committed instance, the plan must name it so an approved " +
			"amendment can move the commitment; otherwise start a new session for it. " +
			"Re-approving this card will not move it."
	}

	pl := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sessNS, Name: sessName},
		Category:        categories.InternalError,
		RequestRef:      newRequestID(),
		Lead:            lead,
		// Required of every degraded-tone notice, and the reason the rule exists:
		// without it this says something went wrong and leaves the reader holding
		// an unanswered card.
		NextStep: nextStep,
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceApprovers},
	}
	env, err := channelevents.BuildEnvelope(sessNS, sessName, channelevents.KindInteractionRequest, pl)
	if err != nil {
		h.logApprovalNoticeFailure(sessNS, sessName, fmt.Errorf("build envelope: %w", err), cause)
		return
	}
	if err := h.l.InteractionRequestPublish(ctx, sessNS, sessName, env); err != nil {
		h.logApprovalNoticeFailure(sessNS, sessName, err, cause)
	}
}

func (h *runnerHost) logApprovalNoticeFailure(ns, name string, err, cause error) {
	slog.Default().Info("plan_gate: could not tell the approver that their approval was discarded; "+
		"they will see the same card again with no explanation",
		"session", ns+"/"+name, "publishErr", err.Error(), "cause", cause.Error())
}

// tierTone maps a CardLine's Impact — "readonly", "readwrite" or "external",
// the plan-gate's own vocabulary (permsurface.Descriptor.StateImpact) — onto
// the wire Tone that carries the same three tiers. An empty or unrecognized
// Impact (the resource lines under a phase never set it) yields the zero
// Tone rather than guessing: no Impact means the translation has nothing to
// carry, not that the line is readonly by default.
func tierTone(impact string) channelevents.Tone {
	switch impact {
	case "readonly":
		return channelevents.ToneReadonly
	case "readwrite":
		return channelevents.ToneReadwrite
	case "external":
		return channelevents.ToneExternal
	default:
		return ""
	}
}

// planGateItems projects the card's phases onto the transport's generic
// structured-field shape.
//
// A translation and nothing more: no fact appears here that the card did not
// compute, and none is dropped. The card owns what an approval says; this owns
// only how it crosses the wire.
func planGateItems(c plangate.Card) []channelevents.InteractionItem {
	if len(c.Phases) == 0 {
		return nil
	}
	out := make([]channelevents.InteractionItem, 0, len(c.Phases)+1)
	for _, ph := range c.Phases {
		item := channelevents.InteractionItem{Text: ph.Title}
		for _, p := range ph.Permissions {
			tone := tierTone(p.Impact)
			if tone == "" && p.External {
				// Impact is the authoritative source (card.go derives both from
				// the same StateImpact lookup, so in practice they never
				// disagree), but a caller that only set External still gets the
				// tier its own flag names rather than no tone at all.
				tone = channelevents.ToneExternal
			}
			line := channelevents.InteractionItem{
				Text: p.Text,
				Tone: tone,
				// The raw handle rides as a hint a surface may reveal on demand
				// (a hover tooltip) — never as part of Text or Detail, which are
				// what a reader is shown by default.
				Hint: p.Handle,
			}
			if p.External {
				// The reason the card is worth reading. Carried as tone rather
				// than as a suffix so a surface can make it unskimmable, and
				// restated here as detail so the meaning survives wherever tone
				// alone can't (a copy-paste, a plain-text log line).
				line.Detail = "leaves this session"
			}
			item.Items = append(item.Items, line)
		}
		for _, r := range ph.Resources {
			// Icon and Href ride straight over the wire: the card already
			// decided them (resolveResourceLine, gated on eligibleHref), and
			// this translation adds nothing — in particular it never
			// recomputes Href from Detail, which is what keeps text == href a
			// property of construction all the way to the wire rather than
			// something this step could accidentally break.
			item.Items = append(item.Items, channelevents.InteractionItem{
				Text: "reaches " + r.Text, Detail: r.Detail, Icon: r.Icon, Href: r.Href,
			})
		}
		out = append(out, item)
	}
	if c.Coverage != "" {
		// What one click buys, kept as its own line rather than folded into the
		// last phase — it is a statement about the whole card.
		out = append(out, channelevents.InteractionItem{
			Text: c.Coverage, Tone: channelevents.ToneMuted,
		})
	}
	return out
}
