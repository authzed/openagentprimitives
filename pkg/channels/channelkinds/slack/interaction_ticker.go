// pkg/channels/channelkinds/slack/interaction_ticker.go
//
// The public-post expiry ticker for the Interaction renderer: a background
// goroutine that periodically edits an interaction's public note with the
// publisher's original body plus an elapsed caption ("_Nm elapsed._"), so an
// observer watching the channel sees that the request is still open, what it is
// about, and how long it has been waiting. Cadence and formatting come from
// awaitingTickInterval / awaitingMaxDuration / formatElapsedSuffix
// (sender_helpers.go).
//
// Lifecycle: launched from sendRequest ONLY when p.ExpiresAt != nil &&
// p.Audience.PublicNote (only tool_approval sets PublicNote today), against the
// public note's deliveryRef; cancelled when the interaction_applied edit for
// the same RequestRef arrives (sendApplied → cancelExpiryTicker). Best-effort
// and in-process like the delivery store — a channelsd restart loses the
// ticker, and the note simply stops updating.
package slack

import (
	"context"
	"strings"
	"time"

	"github.com/go-logr/logr"
	slackapi "github.com/slack-go/slack"
)

// tickerHandle wraps a running ticker's cancel func so the launching goroutine
// can identity-compare (pointer equality — context.CancelFunc values are not
// comparable) when self-cleaning its own map entry, without clobbering a newer
// ticker that happens to share the same RequestRef.
type tickerHandle struct {
	cancel context.CancelFunc
}

// launchExpiryTicker starts the public-post expiry ticker for requestRef in a
// background goroutine (its own context, derived from context.Background() so it
// outlives the request-handling ctx — the legacy driver did the same). The
// cancel handle is stored under tickMu so cancelExpiryTicker (applied path) can
// stop it. Callers gate on ref.TS != "" && requestRef != ""; this method
// re-checks defensively so a mis-gated call is a safe no-op rather than a leaked
// goroutine. If a ticker somehow already exists for requestRef it is cancelled
// first, so the map never leaks a superseded goroutine. body is the publisher's
// PublicNoteBody (the same text already posted as the initial public note); the
// ticker re-renders it on every tick with the elapsed suffix appended, instead
// of a content-free generic caption — see runInteractionTicker.
func (s *interactionSender) launchExpiryTicker(ref deliveryRef, requestRef string, startedAt time.Time, body string, logger logr.Logger) {
	if ref.TS == "" || requestRef == "" {
		return
	}
	tickCtx, cancel := context.WithCancel(context.Background())
	h := &tickerHandle{cancel: cancel}

	s.tickMu.Lock()
	if s.tickCancel == nil {
		s.tickCancel = map[string]*tickerHandle{}
	}
	prev, hadPrev := s.tickCancel[requestRef]
	s.tickCancel[requestRef] = h
	s.tickMu.Unlock()
	if hadPrev {
		// Supersede any existing ticker for this ref — never leak it. Called
		// outside the lock: cancelExpiryTicker's policy comment applies here too
		// (don't hold tickMu across cancel(), which may block on the ticker
		// goroutine's own tickMu acquisition in its self-cleanup).
		prev.cancel()
	}

	go func() {
		s.runInteractionTicker(tickCtx, ref, startedAt, awaitingTickInterval, body, logger)
		// Self-cleanup: the ticker returned on its own (max duration / edit
		// error / ctx cancel). Drop our entry so the map stays bounded, but only
		// if it is still ours — a concurrent launchExpiryTicker for the same
		// RequestRef may have superseded us and installed a newer handle we must
		// not delete. cancel() is idempotent, so cancelExpiryTicker racing us is
		// safe either way.
		s.tickMu.Lock()
		if s.tickCancel[requestRef] == h {
			delete(s.tickCancel, requestRef)
		}
		s.tickMu.Unlock()
		cancel()
	}()
}

// cancelExpiryTicker stops and forgets the public-post expiry ticker for
// requestRef, if one is running. A missing key (no ticker was launched, or it
// already self-terminated / was cancelled) is a safe no-op, so a double-cancel
// or an applied for a non-ticked interaction costs nothing.
func (s *interactionSender) cancelExpiryTicker(requestRef string) {
	s.tickMu.Lock()
	h, ok := s.tickCancel[requestRef]
	if ok {
		delete(s.tickCancel, requestRef)
	}
	s.tickMu.Unlock()
	if ok {
		h.cancel() // outside the lock: don't hold tickMu across the cancel
	}
}

// runInteractionTicker periodically edits the public note at ref with its
// original body plus an elapsed caption. It reproduces the legacy
// runPendingTicker loop exactly:
// time.NewTicker(interval); on each tick, stop if elapsed exceeds
// awaitingMaxDuration, else chat.update the note; stop on ctx cancel (the
// applied path cancels via cancelExpiryTicker) or on an update error. Every
// termination path returns (defer ticker.Stop() releases the timer), so the
// goroutine never leaks and the update loop never runs away — the three exits
// are ctx-cancel, max-duration, and update-error, all logged, never swallowed.
//
// interval is a parameter (production passes awaitingTickInterval; tests pass a
// fast interval) so a test can drive many ticks without touching the
// package-level constant — the same test seam toolApprovalPending.Interval used.
// body is the publisher's PublicNoteBody — what is being asked — which the
// tick re-renders with the elapsed suffix appended rather than discarding. An
// empty body is defensive: sendRequest always threads a non-empty value when
// it posted a note, and the note still reads correctly on its fixed lead
// alone.
func (s *interactionSender) runInteractionTicker(ctx context.Context, ref deliveryRef, startedAt time.Time, interval time.Duration, body string, logger logr.Logger) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	refLabel := ref.ChannelID + ":" + ref.TS
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			elapsed := time.Since(startedAt)
			if elapsed > awaitingMaxDuration {
				logger.Info("interaction expiry ticker hit max duration; stopping",
					"ref", refLabel, "elapsed", elapsed.String())
				return
			}
			// Re-render in the same shape the note was posted in. A tick that
			// rewrote it as a plain section would silently downgrade a live
			// message every few seconds.
			ticked := strings.TrimSpace(body + " " + formatElapsedSuffix(elapsed))
			if _, _, _, err := s.client.UpdateMessageContext(ctx, ref.ChannelID, ref.TS,
				slackapi.MsgOptionText(publicNoteNotifyText(ticked), false),
				slackapi.MsgOptionBlocks(publicNoteBlocks(ticked)...),
			); err != nil {
				logger.Info("interaction expiry ticker: chat.update failed; stopping",
					"ref", refLabel, "err", err.Error())
				return
			}
		}
	}
}
