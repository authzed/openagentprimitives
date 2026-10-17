package local

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// interactionSender is the host-side "interaction" sub-channel sender:
// semantic interaction requests (credential_link, identity_choice, etc.). It
// emits a MsgInteractionRequest carrying both the structured Payload and a
// RenderText markdown floor. The TUI drives decision INPUT off the Payload's
// ActionKindDecision actions (a blocking modal — see chat_tui.go); the Text
// floor is used for the read-only (link / notice) path where no modal opens.
type interactionSender struct {
	sink *inertSink
	// minter and principal mint the screenshot reference link RenderText
	// appends for a browser-originated interaction (see screenshotURL below).
	// Both are the same ArtifactViewMinter/Principal pair host.go already wires
	// into liveViewOfferSender for the sibling "live_view_offer" sub-channel —
	// nil minter (no webd base URL configured) degrades to no line, never an
	// error: unlike a live-view offer, the screenshot line is a side note on a
	// card whose Lead/Actions must still be delivered.
	minter    channelkinds.ArtifactViewMinter
	principal identity.Principal
}

func (s *interactionSender) emitErr(sess channelkinds.SessionInfo, k channelevents.Kind, err error) (channelkinds.SubChannelSendResult, error) {
	s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: k, Err: err.Error(), At: timeNowUTC()})
	return channelkinds.SubChannelSendResult{}, fmt.Errorf("local interaction sender (%s): %w", k, err)
}

func (s *interactionSender) Send(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	switch env.Kind {
	case channelevents.KindInteractionRequest:
		var pl channelevents.InteractionRequestPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitErr(sess, env.Kind, err)
		}
		// The ONE sweep any sender in this package still does itself, and it is
		// about ORDERING, not coverage — the sink sweeps these fields anyway.
		// RenderText below composes markdown AROUND these slots, so it must run
		// on already-swept text. See NOTES.md.
		pl = inertInteractionPayload(pl)

		// nil decisionURL: link actions use their own URL, decision actions
		// render as plain labels plus a note to respond from a connected app.
		// The rendered Text is the read-only timeline floor; Payload carries the
		// structured actions the TUI's decision modal binds keys to.
		text := channelinteractions.RenderText(pl, nil, s.screenshotURL(ctx, sess))
		s.sink.Emit(MsgInteractionRequest{Session: refOf(sess), Payload: pl, Text: text})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindInteractionApplied:
		var pl channelevents.InteractionAppliedPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitErr(sess, env.Kind, err)
		}
		// OutcomeText lands in the same timeline as the prompt above and is
		// runner-controlled, so it goes through the same door as everything
		// else this kind emits: one live rendering beside one inert one, for
		// the same interaction, is worse than either alone.
		s.sink.Emit(MsgInteractionApplied{
			Session: refOf(sess), Category: pl.Category,
			OutcomeText: pl.OutcomeText, Outcome: pl.Outcome,
		})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindInteractionDecisionRejected:
		var pl channelevents.InteractionDecisionRejectedPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitErr(sess, env.Kind, err)
		}
		s.sink.Emit(MsgInteractionRejected{
			Session: refOf(sess), Class: pl.Class,
			Reason:          pl.Reason,
			OriginalOutcome: pl.OriginalOutcome,
		})
		return channelkinds.SubChannelSendResult{}, nil

	default:
		return s.emitErr(sess, env.Kind, fmt.Errorf("unsupported envelope kind for the interaction sender"))
	}
}

// screenshotURL returns the per-call closure RenderText consults to resolve a
// browser-originated interaction's redacted screenshot to a human-facing
// link. Best-effort, matching the AGENTS.md "never silently drop errors"
// rule with a log line rather than a failure: the TUI's decision modal and
// the rest of the card must still render if minting fails or is unconfigured,
// so a mint error here is a skipped line, never a dropped card.
func (s *interactionSender) screenshotURL(ctx context.Context, sess channelkinds.SessionInfo) func(*channelevents.AttachmentRef) string {
	return func(ref *channelevents.AttachmentRef) string {
		if s.minter == nil || ref == nil || ref.ArtifactID == "" {
			return ""
		}
		sessionRef := sess.Namespace + "/" + sess.Name
		url, err := s.minter.MintArtifactViewLink(ref.ArtifactID, sessionRef, s.principal, "")
		if err != nil {
			log.FromContext(ctx).Info("local interaction sender: mint screenshot view link failed (best-effort, card still delivered)",
				"session", sessionRef, "artifactID", ref.ArtifactID, "err", err.Error())
			return ""
		}
		return url
	}
}
