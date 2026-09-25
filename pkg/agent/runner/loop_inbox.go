package runner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/runner/approval/summarizer"
	"github.com/authzed/openagentprimitives/pkg/authz/untrusted"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/viewurn"

	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// drainInbox places any not-yet-consumed "inbox"-role turns (written by
// channelsd for inbound human messages) as real "user" transcript turns at
// runner-assigned indices, making the runner the sole writer of user/assistant
// turns. Each consumed "inbox" turn gets a paired "inbox_done" marker turn at
// the same Index so a later drain (or a resumed runner) skips it. Returns the
// placed user turns and the advanced nextIndex.
//
// "inbox" turns never collide with runner turns because channelsd writes them
// under a distinct role (see internal/cmd/channelsd/memory.go's Append). The real "user"
// turn is appended BEFORE its "inbox_done" marker so a crash between the two
// re-drains (a duplicate user turn) rather than silently losing the message.
//
// Each drained turn with a non-empty Author also calls advanceRequester so the
// tool-call auth subject follows the CURRENT requester rather than the
// session's frozen initiator — see advanceRequester's doc.
func (l *Loop) drainInbox(ctx context.Context, nextIndex int) (placed []memory.Turn, newNextIndex int, err error) {
	inbox, err := l.heldInbox(ctx)
	if err != nil {
		return nil, nextIndex, fmt.Errorf("drain inbox: %w", err)
	}

	// Inbox turns are always mid-session follow-ups (channelsd writes them for
	// inbound human messages while the runner is parked); they are NEVER a new
	// session's first message — that arrives as spec.Prompt and is placed as
	// turn 0 by Run (where the runner drives the cold-start flow). drainInbox
	// therefore replays every inbox turn verbatim.
	for _, it := range inbox {
		// The raw inbox turn is ALWAYS marked inbox_done below (preserved in
		// memory for audit; never re-drained), and only after the real user turn
		// is durable.
		real := memory.Turn{
			Index:     nextIndex,
			Role:      "user",
			Content:   it.Content,
			CreatedAt: time.Now().UTC(),
			Author:    it.Author,
			Via:       it.Via,
		}
		if aerr := l.Memory.Append(ctx, real); aerr != nil {
			return placed, nextIndex, fmt.Errorf("drain inbox: append user turn: %w", aerr)
		}
		placed = append(placed, real)
		nextIndex++
		// A newly-placed user turn starts a fresh round, so the delivery guard
		// re-arms: this message is owed its own answer regardless of what was
		// sent for the previous one. Every drain path (drainHeldAtYield,
		// drainAwaitResume, the resume drain in Run) funnels through here, so
		// placing the reset at the placement point keeps the guard armed
		// without each caller having to remember to do it.
		l.beginDeliveryRound()
		// Advance the tool-call auth subject to THIS turn's sender. Turns
		// drain in ascending Index order, so when several are queued at
		// once the last iteration's call wins — the most-recent sender
		// becomes the subject for the tool calls the LLM is about to make
		// in response to this batch (see advanceRequester's doc for the
		// multi-turn-batch limitation).
		l.advanceRequester(it.Author)
		// Same "last iteration wins" advance for the CURRENT USER TURN
		// index, unconditional (unlike advanceRequester's authz-mode gate):
		// every turn drainInbox places here is a real "user" turn regardless
		// of whether it carries an Author, so real.Index is always a genuine
		// current-turn position. preferencesReader's turnIndex() reads this.
		l.setCurrentUserTurnIndex(real.Index)
		if !it.Author.Empty() {
			// Unconditional, unlike advanceRequester: display attribution must
			// not inherit the authorization mode's gating.
			l.lastInboundAuthor = it.Author
		}

		marker := memory.Turn{
			Index:     it.Index,
			Role:      "inbox_done",
			Content:   []memory.ContentBlock{{Type: "text", Text: "inbox entry consumed"}},
			CreatedAt: time.Now().UTC(),
		}
		if aerr := l.Memory.Append(ctx, marker); aerr != nil {
			return placed, nextIndex, fmt.Errorf("drain inbox: append inbox_done marker: %w", aerr)
		}
		l.CurrentInboxIdx = it.Index // track most-recent inbox turn index for autofill wait
	}
	return placed, nextIndex, nil
}

// isAnnotationTurn reports whether the turn is an annotation batch, keyed off
// the turn's server-minted Via sub-URN (urn:ap:view:artifact:<id>/annotations),
// NOT the text. The Via is digest-signed on the turn and viewurn-validated, so
// this is verifiable and unspoofable — a plain message whose text happens to
// contain the <untrusted-annotations> marker carries a plain artifact Via (no
// sub) and is correctly NOT treated as an annotation batch.
func isAnnotationTurn(t memory.Turn) bool {
	return viewurn.IsAnnotations(t.Via)
}

// stripUntrustedBlocks removes every <untrusted-annotations nonce="…"> …
// </untrusted-annotations nonce="…"> block (inclusive) from the annotation
// envelope, leaving ONLY the trusted numbered comment/intent/severity/target
// lines. The summarizer thus never sees the untrusted DOM data at all.
//
// The close for a given opener is matched by NONCE, not by the first
// close-prefix after it: untrusted DOM content embedding a fake
// </untrusted-annotations nonce="other"> would otherwise end the block early
// and leak the real tail data into the trusted output. Not reachable today (the
// DOM payload goes through json.Marshal, which HTML-escapes '<'/'>'), but the
// strip must not depend on that.
//
// A malformed/half-open block (no close carrying the SAME nonce as the opener)
// is dropped from the opener to the end — fail closed toward LESS text reaching
// the summarizer, never more.
func stripUntrustedBlocks(content string) string {
	openPrefix := "<" + untrusted.AnnotationsTag + " nonce=\""
	closePrefix := "</" + untrusted.AnnotationsTag + " nonce=\""
	var b strings.Builder
	rest := content
	for {
		i := strings.Index(rest, openPrefix)
		if i < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:i])
		// Extract THIS opener's nonce value (the text between its quotes) so
		// the matching close is found by nonce, not by prefix alone.
		afterOpen := rest[i+len(openPrefix):]
		q := strings.IndexByte(afterOpen, '"')
		if q < 0 {
			break // malformed opener (unterminated nonce attr): drop opener..end
		}
		nonce := afterOpen[:q]
		closeMarker := closePrefix + nonce + "\">"
		// find the SAME-NONCE close AFTER the opener
		after := rest[i:]
		j := strings.Index(after, closeMarker)
		if j < 0 {
			break // half-open (no matching same-nonce close): drop opener..end
		}
		rest = after[j+len(closeMarker):]
	}
	return b.String()
}

// turnText flattens a turn's content blocks into a single string, skipping
// non-text blocks (tool_use/tool_result). A memory.Turn's body is
// []ContentBlock (kgingestion's hooks.go joins the same way for its own
// turn-to-text read), while stripUntrustedBlocks operates on a plain string, so
// an annotation batch's "text" blocks must be flattened before it runs.
func turnText(t memory.Turn) string {
	var b strings.Builder
	for _, cb := range t.Content {
		if cb.Type != "text" || cb.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(cb.Text)
	}
	return b.String()
}

// maybeEchoAnnotationTurn authors the human-readable mirror of an annotation
// batch. channelsd defers the raw echo (DeferEcho), so this is the only mirror;
// it is published as KindUserEcho to reach BOTH the browser chat and Slack.
// Summary via the isolated summarizer, with a deterministic fallback so the
// mirror never silently vanishes. Never returns an error: an echo failure must
// not abort the drain that placed t, so every failure path is logged instead.
func (l *Loop) maybeEchoAnnotationTurn(ctx context.Context, t memory.Turn) {
	if !isAnnotationTurn(t) {
		return
	}
	// Recognition is by the Via (above); the summarizer input is still the turn
	// text with the untrusted DOM blocks stripped out.
	req := summarizer.AnnotationRequest{TrustedText: stripUntrustedBlocks(turnText(t))}
	summary := summarizer.FallbackAnnotationSummary(req)
	if l.AnnotationSummarizer != nil {
		cctx, cancel := context.WithTimeout(ctx, summarizer.DefaultTimeout)
		defer cancel()
		if s, err := l.AnnotationSummarizer.SummarizeAnnotations(cctx, req); err != nil {
			ctrllog.FromContext(ctx).Info("annotation summarizer failed; using fallback",
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
				"provider", l.AnnotationSummarizer.Name(), "err", err.Error())
		} else if s != "" {
			summary = s
		}
	}

	email := identity.DecodeForDisplay(t.Author.String())
	echo := channelevents.UserEchoPayload{
		Text:   summary,
		Via:    t.Via,
		Author: channelevents.ExternalIdentity{Kind: "idp", Email: identity.Email(email), ExternalID: identity.RawExternalID(email)},
	}
	if l.EchoPublish == nil {
		ctrllog.FromContext(ctx).Info("annotation echo skipped: no publisher wired",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name)
		return
	}
	if err := l.EnvelopeSigner.PublishOut(l.EchoPublish, l.SessionKey.Namespace, l.SessionKey.Name, channelevents.KindUserEcho, echo); err != nil {
		ctrllog.FromContext(ctx).Info("annotation echo publish failed; agent has the bundle but the mirror is missing",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", err.Error())
	}
}
