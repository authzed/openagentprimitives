// pkg/channels/channelkinds/slack/interaction.go
//
// interactionSender renders the "interaction" sub-channel
// (channelevents.KindInteractionRequest / KindInteractionApplied — the unified
// Interaction model, pkg/channels/channelevents/interaction.go).
//
//   - KindInteractionRequest: renders Lead/Body/Fields as a section block,
//     every ActionKindLink action as a Block Kit URL button, and every
//     ActionKindDecision action as a Block Kit button whose value encodes
//     {v:"interaction", r:requestRef, d:actionId, c:category, s:sessRef}
//     (buttoncodec.go's shared codec, discInteraction discriminator).
//     Delivered per the category's channelinteractions.Surface hint
//     (SurfaceDMOnly always opens a DM; the default posts
//     ephemeral-with-DM-fallback) to every identity in Audience.Requester or
//     Audience.Approvers depending on Scope. The delivery lands in s.delivery,
//     keyed by RequestRef, whenever deliveryEditable(p) says a later
//     interaction_applied will need to edit it in place.
//   - KindInteractionApplied{Category: credential_link, Outcome: resolved}:
//     posts a "connected" notice at the resolved tone. credential_link resolves
//     out-of-band (an OAuth callback, not a click), so there is no response_url
//     to route from; the recipient comes from SessionInfo.SessionInitiator
//     instead (the category's DecideRequester policy — requester = session
//     starter).
//   - KindInteractionApplied for any other category (a decision-kind category's
//     outcome — approved/denied/expired): edits the clicker's own ephemeral/DM
//     in place via the response_url that round-tripped from the decision
//     (listener.go's handleInteractionDecisionClick sets
//     InteractionDecisionPayload.ResponseRef = the click's cb.ResponseURL; the
//     decision pipe copies it verbatim onto Applied.ResponseRef — see
//     pkg/channels/channelsd/pipeline/interaction_decision.go). When
//     ResponseRef is empty (the decision resolved without a click —
//     identity_choice's timeout path, publishIdentityChoiceTimeoutApplied), it
//     falls back to a best-effort SessionInitiator notice.
//
// buildInteractionRequestBlocks appends the Show-Details button whenever the
// request payload carries Details; the cache-write happens here in sendRequest
// (s.delivery.recordDetails), while the click handler
// (handleInteractionDetailsAction) and modal render
// (renderInteractionDetailsModal) live in interaction_details.go.
package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// interactionSender is the "interaction" sub-channel Sender.
type interactionSender struct {
	client slackClient
	// httpClient is used for response_url POSTs (the decision-applied
	// edit-in-place path); nil => http.DefaultClient. Tests inject an
	// httptest.Server-backed client.
	httpClient *http.Client
	// delivery is the in-process delivery-record cache (see
	// interaction_delivery.go): remembers where a request's prompt/public
	// note landed so a later interaction_applied can edit it in place.
	delivery *interactionDeliveryStore
	// deps carries operator-tunable config, notably ApproverFanoutLimit — see
	// capApproverFanout's use in sendRequest's AudienceApprovers branch
	// (interaction_fanout.go). A zero-value deps (test construction via
	// &interactionSender{client: ...}) resolves to
	// channelkinds.DefaultApproverFanoutLimit, matching production's
	// unset-limit default.
	deps channelkinds.Deps

	// tickMu guards tickCancel. The request path (sendRequest → launchExpiryTicker)
	// writes it; the applied path (sendApplied → cancelExpiryTicker) and each
	// ticker goroutine's self-cleanup read+delete it, on different goroutines —
	// so the map is mutex-guarded (see interaction_ticker.go).
	tickMu sync.Mutex
	// tickCancel maps a pending interaction's RequestRef to its running
	// public-post expiry ticker's cancel handle. Best-effort + in-process,
	// exactly like the delivery store: only interactions with ExpiresAt +
	// Audience.PublicNote get an entry, and it is dropped on interaction_applied
	// or when the ticker self-terminates (max duration / edit error).
	tickCancel map[string]*tickerHandle
}

func newInteractionSender(deps channelkinds.Deps) *interactionSender {
	return &interactionSender{client: newSlackAPIClient(deps.Secret), delivery: newInteractionDeliveryStore(), deps: deps}
}

func (s *interactionSender) httpCli() *http.Client {
	if s.httpClient != nil {
		return s.httpClient
	}
	return http.DefaultClient
}

// Send conforms to channelkinds.Sender.
func (s *interactionSender) Send(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	if s.client == nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("interaction sender: slack client unconfigured (Secret missing bot token)")
	}
	switch env.Kind {
	case channelevents.KindInteractionRequest:
		return channelkinds.SubChannelSendResult{}, s.sendRequest(ctx, sess, env)
	case channelevents.KindInteractionApplied:
		return channelkinds.SubChannelSendResult{}, s.sendApplied(ctx, sess, env)
	case channelevents.KindInteractionDecisionRejected:
		return channelkinds.SubChannelSendResult{}, s.sendRejected(ctx, sess, env)
	default:
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("interaction sender: unexpected kind %q", env.Kind)
	}
}

// sendRejected renders a per-clicker decision rejection: the decision pipe
// refused this click (lacked standing, was a spectator on an already-resolved
// prompt, a category mismatch, or a bound-handler failure), so the clicker is
// told why their click did nothing. It posts to the click's own response_url
// with replace_original:false — a NEW ephemeral scoped to that clicker, WITHOUT
// touching the shared prompt (a rejected click never resolves the card, so the
// real approver's prompt must survive). Mirrors the listener's surfaceClickFailure
// response_url shape (replace_original:false). Best-effort like the applied edit:
// an empty/expired response_url leaves the clicker without inline feedback (there
// is no other surface to fall back to on a reject, since the prompt is intact) —
// logged, not fatal.
func (s *interactionSender) sendRejected(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) error {
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name, "kind", "interaction_decision_rejected")

	var p channelevents.InteractionDecisionRejectedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return fmt.Errorf("interaction sender: parse rejected payload: %w", err)
	}

	text := interactionRejectionText(p)
	if p.ResponseRef == "" {
		logger.Info("interaction sender: decision rejected has no response_url; clicker left without inline feedback",
			"requestRef", p.RequestRef, "category", p.Category, "class", p.Class)
		return nil
	}
	if err := postToResponseURL(ctx, s.httpCli(), p.ResponseRef, map[string]any{
		"replace_original": false, "text": text,
	}); err != nil {
		return fmt.Errorf("interaction sender: post rejection response_url (requestRef=%s, class=%s): %w", p.RequestRef, p.Class, err)
	}
	return nil
}

// interactionRejectionText renders the one-line reason shown to a clicker whose
// decision was refused. The already_resolved spectator class names the original
// decider (a Slack <@mention> when the decider is a Slack identity, else their
// email) and the outcome they applied, so a late clicker sees who resolved it;
// every other class renders the pipe-supplied Reason at the degraded tone.
func interactionRejectionText(p channelevents.InteractionDecisionRejectedPayload) string {
	if p.Class == "already_resolved" {
		who := "someone else"
		if p.OriginalDecider != nil {
			switch {
			case p.OriginalDecider.Kind == "slack" && p.OriginalDecider.ExternalID != "":
				who = "<@" + string(p.OriginalDecider.ExternalID) + ">"
			case p.OriginalDecider.Email != "":
				who = string(p.OriginalDecider.Email)
			}
		}
		msg := ":" + toneChip(channelinteractions.ToneHousekeeping, true) + ": This request was already resolved by " + who
		if p.OriginalOutcome != "" {
			msg += " (" + p.OriginalOutcome + ")"
		}
		return msg + "."
	}
	reason := strings.TrimSpace(p.Reason)
	if reason == "" {
		reason = "your click could not be applied"
	}
	// Degraded: the click did not land, but the request itself is unaffected
	// and clicking again usually works.
	return ":" + toneChip(channelinteractions.ToneDegraded, false) + ": " + reason + "."
}

// sendRequest renders an InteractionRequestPayload and delivers it to every
// recipient in scope — Audience.Requester (one addressee) or
// Audience.Approvers (the owner-derived approver model's single approver) —
// per the category's channelinteractions.Surface hint (deliverPrompt).
// AudienceParticipants is a third, recipient-less scope handled separately by
// postBroadcast: a plain post to the session channel/thread rather than a
// per-recipient ephemeral/DM fan-out. The delivery is recorded (see
// deliveryEditable) so a later interaction_applied can edit the prompt in
// place.
func (s *interactionSender) sendRequest(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) error {
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name, "kind", "interaction_request")

	var p channelevents.InteractionRequestPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return fmt.Errorf("interaction sender: parse request payload: %w", err)
	}
	if p.Lead == "" {
		return fmt.Errorf("interaction sender: request payload missing Lead (requestRef=%s)", p.RequestRef)
	}

	// Resolve Field.Mentions to Slack "<@id>" mentions before rendering: the
	// Slack client + ctx needed to resolve a canonical identity to a user id
	// only exist here, not inside the pure buildInteractionRequestBlocks. A
	// Field with no Mentions passes through unchanged (its Value is already
	// the display copy). On a per-recipient resolution miss the recipient's
	// own display text stands in — never dropped (info_leakage's "the data
	// owner must see exactly who" safety invariant).
	p.Fields = resolveFieldMentions(ctx, s.client, p.Fields, logger, p.RequestRef)

	// Decode Slack's entity encoding in the Excerpt for the same reason and at
	// the same seam — the client + ctx a users.info lookup needs exist here and
	// not in the pure builder — but in the opposite DIRECTION: a Field's
	// Mentions are a structured identity this kind renders INTO "<@id>" markup,
	// while an Excerpt already arrives carrying "<@id>" as data, quoted from a
	// message somebody sent, and gets rendered OUT to a name. The card fences
	// and escapes the excerpt, so an id left encoded reaches the approver as
	// literal "<@U0BL99R25K3>" and names nobody. Upstream of
	// buildInteractionRequestBlocks on purpose: inertExcerpt must get the last
	// word on a resolved display name.
	p.Excerpt = resolveExcerptMentions(ctx, s.client, p.Excerpt, logger, p.RequestRef)

	surface := channelinteractions.SurfaceEphemeralDMFallback
	if cat, ok := channelinteractions.Get(p.Category); ok {
		surface = cat.Surface
	}
	sessRef := sess.Namespace + "/" + sess.Name
	blocks := buildInteractionRequestBlocks(p, sessRef)
	// env.ResurfaceInterruptRequestID is stamped only when channelsd's
	// republishPrompt re-surfaces this exact prompt to a user who just
	// re-interacted from another device (see interaction_resurface_interrupt.go);
	// on a first publish it is empty and this is a no-op. Appended after
	// buildInteractionRequestBlocks rather than inside it because the payload
	// alone does not carry the id — only the envelope does.
	blocks = appendInteractionResurfaceInterrupt(blocks, env, sessRef)

	var recipients []channelevents.ExternalIdentity
	// noteApprovers is who the public note will name. It tracks the DELIVERED
	// set, not the eligible one: naming approvers nobody notified would both
	// cost a Slack lookup each and tell the channel to wait on people who were
	// never asked. (Still not an authorization statement — an unnamed eligible
	// approver can approve.)
	var noteApprovers []channelevents.ExternalIdentity
	switch p.Audience.Scope {
	case channelevents.AudienceRequester:
		if p.Audience.Requester == nil || !p.Audience.Requester.HasIdentity() {
			return fmt.Errorf("interaction sender: request payload missing Audience.Requester (requestRef=%s)", p.RequestRef)
		}
		recipients = []channelevents.ExternalIdentity{*p.Audience.Requester}
	case channelevents.AudienceApprovers:
		if len(p.Audience.Approvers) == 0 {
			return fmt.Errorf("interaction sender: approvers scope has no approvers (requestRef=%s)", p.RequestRef)
		}
		// Delivery-only cap: bounds how many approvers this fan-out notifies.
		// Never an authorization bound — click-time DecideResourceOwners admits
		// any eligible owner whether they were delivered to or not.
		recipients = capApproverFanout(logger, p.Audience.Approvers, s.deps.ResolvedApproverFanoutLimit(), sessRef)
		noteApprovers = recipients
	case channelevents.AudienceParticipants:
		// Broadcast: no addressee. Post plain (non-ephemeral) to the session
		// channel/thread so every interact-participant sees the prompt; the
		// decision's DecideParticipant policy gates who may action it.
		broadcastRef, err := s.postBroadcast(ctx, sess, blocks, p.Lead)
		if err != nil {
			return fmt.Errorf("interaction sender: %w (broadcast, requestRef=%s)", err, p.RequestRef)
		}
		if s.delivery != nil && deliveryEditable(p) {
			s.delivery.record(p.RequestRef, broadcastRef, deliveryRef{})
		}
		// Cache Details regardless of deliveryEditable: the Show-Details button
		// renders off Details alone, so a Details-only broadcast still needs
		// them cached for the click.
		if s.delivery != nil && len(p.Details) > 0 {
			s.delivery.recordDetails(p.RequestRef, p.Details)
		}
		return nil
	default:
		return fmt.Errorf("interaction sender: unknown audience scope %q (requestRef=%s)", p.Audience.Scope, p.RequestRef)
	}

	var lastRef deliveryRef
	delivered := false
	// Every recipient this loop addresses has already been canonical→user_id
	// resolved, at the cost of a live users.lookupByEmail for an email-typed
	// canonical. Keep the answers so the public note below re-uses them instead
	// of paying for the same lookups a second time.
	resolvedIDs := make(map[identity.CanonicalUserID]string, len(recipients))
	for _, r := range recipients {
		// "idp" is NOT a channel transport kind — it is a channel-agnostic
		// IdP/email identity (identity.KindIdP). The runner addresses a
		// user_preference_confirm requester this way (host_approval.go
		// currentTurnAuthorIdentity) precisely BECAUSE it cannot know which
		// channel will deliver: the identity carries a verified email, and the
		// delivering channel resolves it however it can. Slack resolves it via
		// users.lookupByEmail below, exactly like a "slack" recipient whose
		// canonical is email-derived — so an idp recipient is deliverable here,
		// not a foreign-kind skip. Rejecting it stranded a user who DM'd a
		// Slack-output cron session (reviewbot) to change a preference: the card
		// was addressed to nobody and reportUndeliverable fired.
		if r.Kind != "slack" && r.Kind != identity.KindIdP {
			// Addressed to another channel kind's identity — nothing for this
			// kind to deliver. Not an error: other channel kinds render this same
			// envelope for their own audience.
			logger.Info("interaction sender: recipient has no Slack identity; skipped",
				"requestRef", p.RequestRef, "category", p.Category, "recipientKind", r.Kind)
			continue
		}
		canon, err := r.Principal().AllowSynthetic().Canonical()
		if err != nil {
			// Mirrors the non-slack-recipient skip above: a recipient this
			// kind can't derive a canonical for is nothing this kind can
			// deliver to either — skip it rather than failing every other
			// recipient in the fan-out.
			logger.Info("interaction sender: recipient canonical derivation failed; skipped",
				"requestRef", p.RequestRef, "category", p.Category, "err", err.Error())
			continue
		}
		slackUserID, err := resolveSlackUserIDFromCanonical(ctx, s.client, canon)
		if err != nil {
			// Third per-recipient skip, for the same reason as the two above: a
			// users.lookupByEmail miss (an approver whose verified email has no
			// account in THIS workspace) is one recipient's problem, not the
			// fan-out's. Returning here asked nobody at all — every remaining
			// approver went unaddressed and the !delivered branch below, the only
			// thing that tells the thread nothing is waiting on it, never ran.
			// The relay does not retry an interaction_request and its
			// surfaceDeliveryFailure is gated on KindUserMessage, so that left the
			// session parked until timeout behind a single log line.
			logger.Info("interaction sender: recipient slack-id resolution failed; skipped",
				"requestRef", p.RequestRef, "category", p.Category, "err", err.Error())
			continue
		}
		resolvedIDs[canon] = slackUserID
		// p.Lead is validated non-empty by InteractionRequestPayload.Validate, so
		// deliverPrompt always has a non-empty DM thread title — which is what
		// keeps mobile agent_view titling meaningful.
		ref, err := s.deliverPrompt(ctx, sess, slackUserID, surface, blocks, p.Lead)
		if err != nil {
			return fmt.Errorf("interaction sender: %w (requestRef=%s)", err, p.RequestRef)
		}
		lastRef = ref
		delivered = true
	}
	// Nothing reached anybody. This is a FAILURE, not a benign skip: the
	// session parks awaiting a decision no one was asked for, and without a
	// signal it sits there until its timeout. Surface it on both surfaces —
	// in-thread for whoever is waiting, and monitoring for whoever can fix the
	// configuration.
	if !delivered && len(recipients) > 0 {
		s.reportUndeliverable(ctx, sess, p, recipients)
	}
	publicNote := deliveryRef{}
	// Both the public-note post and the delivery record are gated on delivered.
	// delivered=false means every recipient carried an identity this kind cannot
	// address — reportUndeliverable has just announced that in-thread and on
	// monitoring. Nobody was asked, so there is nothing for a public note to
	// announce and nothing for a later interaction_applied to edit. Posting the
	// note anyway would strand a "⏳ Approval pending." message in the channel
	// forever, since the applied-edit path finds it only via a delivery record
	// this sender never writes when nothing delivered.
	var publicNoteBody string
	if delivered {
		if p.Audience.PublicNote {
			body := p.Audience.PublicNoteBody
			if body == "" {
				body = "Approval pending."
			}
			// publicNoteText makes the publisher's sentence inert and THEN
			// names who the channel is waiting on: without the mentions a
			// reader cannot tell whether the answer is coming from them, a
			// teammate, or someone who is not in the room, and without the
			// escape a summarizer-supplied "<url|label>" is a live forged
			// action in a PUBLIC post one line above those mentions.
			//
			// Read p.Audience.PublicNoteBody exactly once, here: the composed
			// value below is what BOTH sinks get — postPublicNote and, further
			// down, launchExpiryTicker's re-render.
			publicNoteBody = publicNoteText(body,
				approverMentions(ctx, s.client, noteApprovers, resolvedIDs, logger, p.RequestRef))
			nref, err := s.postPublicNote(ctx, sess, p.RequestRef, publicNoteBody)
			if err != nil {
				return fmt.Errorf("interaction sender: %w", err)
			}
			publicNote = nref
		}
		// Record the delivery ONLY when a later interaction_applied will need to
		// edit it — i.e. the prompt carries a decision action or a public note.
		// A link-only card (portal_access: static link, no decision, no note) is
		// never edited, so recording it would leak an entry nothing ever drops.
		// deliveryEditable bounds the store to genuinely-pending interactions.
		if s.delivery != nil && deliveryEditable(p) {
			s.delivery.record(p.RequestRef, lastRef, publicNote)
			// Cache the payload alongside the refs: the interaction_applied
			// that edits them carries a verdict but none of this prompt's
			// detail, so without it the resolved card can say "Approved" but
			// not what was approved (buildInteractionAppliedBlocks).
			s.delivery.recordRequest(p.RequestRef, p)
		}
		// Cache Details independently of deliveryEditable, so a Details-only
		// category with no decision and no public note stays correct. Read by
		// the Show-Details click handler (handleInteractionDetailsAction,
		// interaction_details.go); dropped with the rest of the entry once the
		// interaction resolves (sendDecisionApplied's s.delivery.drop).
		if s.delivery != nil && len(p.Details) > 0 {
			s.delivery.recordDetails(p.RequestRef, p.Details)
		}
		// The public-post expiry ticker is best-effort and in-process: it runs
		// only when the interaction can expire (ExpiresAt) AND posted a public
		// note to caption (only tool_approval sets PublicNote today). It
		// periodically edits the note's deliveryRef with publicNoteBody plus an
		// elapsed caption, and is cancelled when the interaction_applied edit
		// for this RequestRef arrives (sendApplied → cancelExpiryTicker) or
		// self-stops on max duration / an edit error. A skipped public note (no
		// channel context → empty deliveryRef) has nothing to caption.
		if p.ExpiresAt != nil && p.Audience.PublicNote && publicNote.TS != "" {
			s.launchExpiryTicker(publicNote, p.RequestRef, time.Now(), publicNoteBody, logger)
		}
	}
	return nil
}

// sendApplied renders a resolved interaction outcome. credential_link is a
// special case (out-of-band "✅ connected" notice, see the file-top comment
// for why the recipient comes from sess.SessionInitiator rather than the
// (audience-less) Applied payload); every other category is treated as a
// decision-kind outcome (approved/denied/expired) generically — see
// sendDecisionApplied.
func (s *interactionSender) sendApplied(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) error {
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name, "kind", "interaction_applied")

	var p channelevents.InteractionAppliedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return fmt.Errorf("interaction sender: parse applied payload: %w", err)
	}

	// The interaction is resolved: stop its public-post expiry ticker (if any)
	// BEFORE editing any surface, so a straggler tick can't overwrite the
	// resolved note. A no-op when no ticker was launched (only ExpiresAt +
	// PublicNote interactions have one) — covers every applied category, not just
	// the decision path below. A decision rejection (sendRejected) deliberately
	// does NOT cancel: a rejected click leaves the prompt pending, so the ticker
	// must keep running.
	s.cancelExpiryTicker(p.RequestRef)

	if p.Category != "credential_link" {
		return s.sendDecisionApplied(ctx, sess, p, logger)
	}

	// credential_link: outcome is checked in addition to category (not
	// outcome alone) — "resolved" is documented as credential_link's
	// out-of-band completion signal, but nothing in the wire contract stops
	// a future category from reusing the same Outcome constant for a
	// different meaning. Rendering credential_link's "connected" copy for a
	// different outcome would be silently wrong, not just unsupported — fail
	// loud instead.
	if p.Outcome != channelevents.OutcomeResolved {
		return fmt.Errorf("interaction sender: unsupported applied payload (category=%q outcome=%q); credential_link only supports the resolved outcome (requestRef=%s)",
			p.Category, p.Outcome, p.RequestRef)
	}

	if sess.SessionInitiator.IsZero() {
		// Best-effort UX notice, not a gate: the credential is already
		// linked and the session resumes regardless of whether this notice
		// lands. Log and skip rather than fail the whole envelope.
		logger.Info("interaction sender: applied has no session initiator to notify; notice skipped",
			"requestRef", p.RequestRef, "category", p.Category)
		return nil
	}

	slackUserID, err := resolveSlackUserIDFromCanonical(ctx, s.client, sess.SessionInitiator)
	if err != nil {
		return fmt.Errorf("interaction sender: resolve session initiator (requestRef=%s): %w", p.RequestRef, err)
	}

	blocks := buildInteractionResolvedBlocks(p)
	if err := s.deliverEphemeralOrDM(ctx, sess, slackUserID, blocks, "A credential was just linked to your accounts"); err != nil {
		return fmt.Errorf("interaction sender: %w (applied, requestRef=%s)", err, p.RequestRef)
	}
	return nil
}

// sendDecisionApplied renders a decision-kind category's Applied outcome
// (identity_choice, permission_request today; any future ActionKindDecision
// category generically, since the request-side rendering already treats
// decision actions category-agnostically — see buildInteractionRequestBlocks).
// It edits BOTH surfaces a request may have produced — the clicker's own
// prompt and any public note posted alongside it — then drops the delivery
// record, since the interaction is resolved and nothing will edit it again.
// That record/drop pairing is what keeps the in-process store bounded.
//
//  1. Edit the prompt: prefer the clicker's response_url when the decision
//     carried one (replace_original:true edits exactly what they saw) —
//     best-effort, since an expired/5xx response_url is logged, not fatal, and
//     falls through to the recorded prompt ref rather than aborting the
//     resolution bookkeeping below; else fall back to the recorded prompt ref
//     via UpdateMessageContext, which covers a decision resolved without a
//     click (a timeout) that still has a known prompt location.
//  2. Edit the recorded public note (best-effort): strips its buttons down
//     to the outcome line. A miss (nothing recorded, e.g. no public note was
//     posted) is a silent no-op; a failed edit is logged, not fatal — the
//     interaction is already resolved regardless of whether the note updates.
//  3. Drop the delivery record unconditionally (no-op-safe on a missing
//     entry) — the interaction is resolved either way.
//  4. If neither surface was edited (no response_url, no recorded prompt —
//     e.g. identity_choice's publishIdentityChoiceTimeoutApplied when the
//     store never had an entry), fall back to a best-effort notice to the
//     session initiator, the same recipient-resolution credential_link's
//     out-of-band path uses, generalized to any category. A missing
//     SessionInitiator is a graceful skip, not an error (mirrors
//     credential_link).
func (s *interactionSender) sendDecisionApplied(ctx context.Context, sess channelkinds.SessionInfo, p channelevents.InteractionAppliedPayload, logger logr.Logger) error {
	outcomeText := interactionOutcomeText(p)

	// Rebuild the resolved card ONCE, from the cached prompt, and use it on
	// every surface below. The Applied payload carries only a verdict, so the
	// cached request is the only thing that can say what was decided; a miss
	// (channelsd restarted between prompt and click) degrades the card's
	// detail, never its shape. outcomeText stays the plain-text notification
	// preview, which renders where no block does.
	var cachedReq *channelevents.InteractionRequestPayload
	if s.delivery != nil {
		if req, ok := s.delivery.getRequest(p.RequestRef); ok {
			cachedReq = &req
		}
	}
	appliedBlocks := buildInteractionAppliedBlocks(p, cachedReq)

	// 1. Edit the prompt: prefer the clicker's response_url (edits exactly what
	//    they saw); else the recorded prompt ref; else fall back to a notice.
	// The response_url POST is best-effort, like every other surface edit in
	// this function: the interaction is already resolved (the pipe published
	// this Applied), so a cosmetic failure here — an expired/5xx response_url —
	// must not abort the resolution bookkeeping below (recorded-prompt-ref
	// edit, public-note edit, delivery-record drop, initiator fallback). On
	// failure, log and leave editedPrompt false so those steps still run.
	editedPrompt := false
	if p.ResponseRef != "" {
		// blocks, not text alone: a response_url body with no blocks replaces
		// the clicker's whole card with one line of text, discarding the ask
		// it was answering. text rides along as the notification preview.
		// escapeSlackText, not the raw outcomeText: Slack parses a response_url
		// body's "text" as mrkdwn exactly like MsgOptionText's field, and this
		// string is the runner's on the queued_messages path (see
		// interactionOutcomeText). The two chat.update sinks below reach the
		// same door through notifyOption; this one has no MsgOption to carry
		// it, so it escapes here.
		if err := postToResponseURL(ctx, s.httpCli(), p.ResponseRef, map[string]any{
			"replace_original": true, "text": escapeSlackText(outcomeText), "blocks": appliedBlocks,
		}); err != nil {
			logger.Info("interaction sender: post response_url failed (best-effort)", "requestRef", p.RequestRef, "err", err.Error())
		} else {
			editedPrompt = true
		}
	}

	var promptRef, noteRef deliveryRef
	var haveRecord bool
	if s.delivery != nil {
		promptRef, noteRef, haveRecord = s.delivery.get(p.RequestRef)
	}
	if !editedPrompt && haveRecord && promptRef.TS != "" {
		if _, _, _, err := s.client.UpdateMessageContext(ctx, promptRef.ChannelID, promptRef.TS,
			notifyOption(outcomeText),
			slackapi.MsgOptionBlocks(appliedBlocks...)); err != nil {
			logger.Info("interaction sender: edit prompt failed (best-effort)", "requestRef", p.RequestRef, "err", err.Error())
		} else {
			editedPrompt = true
		}
	}

	// 2. Edit the public note (best-effort): strip buttons → outcome line.
	if haveRecord && noteRef.TS != "" {
		if _, _, _, err := s.client.UpdateMessageContext(ctx, noteRef.ChannelID, noteRef.TS,
			notifyOption(outcomeText),
			slackapi.MsgOptionBlocks(appliedBlocks...)); err != nil {
			logger.Info("interaction sender: edit public note failed (best-effort)", "requestRef", p.RequestRef, "err", err.Error())
		}
	}

	// The interaction is resolved — drop its delivery record so the in-process
	// store stays bounded. It is the drop half of sendRequest's record/drop
	// pairing, and a no-op when nothing was recorded.
	if s.delivery != nil {
		s.delivery.drop(p.RequestRef)
	}

	// 3. Neither edited (timeout, no record, no channel): notice to initiator.
	if !editedPrompt {
		if sess.SessionInitiator.IsZero() {
			logger.Info("interaction sender: decision applied has no editable surface and no session initiator; notice skipped",
				"requestRef", p.RequestRef, "category", p.Category, "outcome", p.Outcome)
			return nil
		}
		slackUserID, err := resolveSlackUserIDFromCanonical(ctx, s.client, sess.SessionInitiator)
		if err != nil {
			return fmt.Errorf("interaction sender: resolve session initiator (decision applied, requestRef=%s): %w", p.RequestRef, err)
		}
		if _, err := s.deliverPrompt(ctx, sess, slackUserID, channelinteractions.SurfaceEphemeralDMFallback,
			appliedBlocks, outcomeText); err != nil {
			return fmt.Errorf("interaction sender: %w (decision applied, requestRef=%s)", err, p.RequestRef)
		}
	}
	return nil
}

// deliverEphemeralOrDM posts blocks to slackUserID: ephemeral in sess's
// channel/thread when the session has channel context, else a DM. Used by
// credential_link's out-of-band "connected" notice (sendApplied) — the one
// applied-notice path that can never have a recorded delivery ref to edit,
// since credential_link resolves without ever rendering a decision prompt.
// sendDecisionApplied's fallback notice uses deliverPrompt instead, so its DM
// path also gets deliverPrompt's thread-titling.
func (s *interactionSender) deliverEphemeralOrDM(ctx context.Context, sess channelkinds.SessionInfo, slackUserID string, blocks []slackapi.Block, notifyText string) error {
	// Through notifyOption like every other notify-text sink in this sender:
	// MsgOptionText's escape=false means slack-go does not escape the field, and
	// Slack parses it as mrkdwn. Its one caller passes a literal today, so this
	// is about the door being single rather than about a live payload — a
	// notify-text sink that escapes nothing is how the next caller inherits a
	// hole nobody looks for.
	msgOpts := []slackapi.MsgOption{
		slackapi.MsgOptionBlocks(blocks...),
		notifyOption(notifyText),
	}
	channelID := ""
	threadTS := ""
	if sess.Channel != nil {
		channelID = sess.Channel.External["channel_id"]
		threadTS = effectiveOutboundThreadTS(sess)
	}
	if threadTS != "" {
		msgOpts = append(msgOpts, slackapi.MsgOptionTS(threadTS))
	}

	if channelID != "" {
		if _, err := s.client.PostEphemeralContext(ctx, channelID, slackUserID, msgOpts...); err != nil {
			return fmt.Errorf("PostEphemeral: %w", err)
		}
		return nil
	}

	// No channel context (DM-only session): open a DM and PostMessage.
	dmConv, _, _, err := s.client.OpenConversationContext(ctx, &slackapi.OpenConversationParameters{
		Users: []string{slackUserID},
	})
	if err != nil {
		return fmt.Errorf("open DM with user %q: %w", slackUserID, err)
	}
	if _, _, err := s.client.PostMessageContext(ctx, dmConv.ID, msgOpts...); err != nil {
		return fmt.Errorf("PostMessage (DM): %w", err)
	}
	return nil
}

// interactionOutcomeText is the chip-led one-line summary of a decision
// outcome, used as the plain-text notification preview — what Slack shows on a
// lock screen and in the channel list, where no block renders.
//
// The wording is categories.OutcomeLabel's, shared with the oap chat TUI and
// mirrored by the webchat InteractionCard in TypeScript, so every surface
// names a resolved prompt the same way. Only the chip is Slack's own.
//
// It returns the text RAW, and each sink escapes it — the same per-SINK rule
// the Lead follows (inert.go), for the same reason: the four sinks do not
// agree about markup, so the decision cannot be made once here.
//
//   - the two chat.update edits and the response_url copy
//     (sendDecisionApplied) parse mrkdwn, and escape;
//   - the initiator fallback notice reaches deliverPrompt, whose contract is
//     "notifyText is raw, I escape it" (notifyOption) — and whose other caller
//     hands it a raw Lead that depends on that.
//
// Escaping here instead would double-escape the fallback, rendering
// "&amp;lt;@U123&gt;": escapeSlackText is not idempotent. A NEW sink must pick
// a side explicitly, and the safe default is notifyOption.
//
// This is not merely publisher copy. On the queued_messages path OutcomeText
// is the RUNNER's — the interrupt bridge builds it as "Couldn't interrupt — "
// plus a reason lifted verbatim off .out.interrupt_applied — so an unescaped
// sink renders an agent-supplied `<url|label>` as a live action in a message
// styled as the platform's own resolution notice. categories.OutcomeLabel
// returns the wire value verbatim (a TrimSpace) and cannot escape for us: it
// is surface-agnostic, shared with the TUI and mirrored in the webchat.
func interactionOutcomeText(p channelevents.InteractionAppliedPayload) string {
	return outcomeChipPrefix(p.Outcome) + categories.OutcomeLabel(p.Category, p.OutcomeText, p.Outcome)
}

// outcomeTone maps a resolved interaction's outcome onto the shared tone
// vocabulary, so an outcome line reads in the same colours as everything else.
//
// Denied is degraded rather than critical: the request was refused, but the
// session is fine and the user can usually ask for something else. Expired is
// waiting — nobody decided, which is a different fact from a decision.
func outcomeTone(outcome string) channelinteractions.Tone {
	switch outcome {
	case channelevents.OutcomeDenied:
		return channelinteractions.ToneDegraded
	case channelevents.OutcomeExpired:
		return channelinteractions.ToneWaiting
	default:
		return channelinteractions.ToneResolved
	}
}

// outcomeChipPrefix is the tone chip for a one-line outcome string, which has
// no container to hang a title on.
func outcomeChipPrefix(outcome string) string {
	return ":" + toneChip(outcomeTone(outcome), false) + ": "
}

// resolveFieldMentions returns a copy of fields with every Field that carries
// non-empty Mentions rendered as Slack "<@id>" mentions (comma-joined) in
// place of the flattened display Value — the FULL structured identity
// (Field.Mentions) is resolved here, where the Slack client + ctx exist,
// rather than flattening to a display string at publish time (standing
// rule). Fields with no Mentions pass through unchanged.
//
// Resolution mirrors sendRequest's own recipient-resolution idiom
// (Principal().AllowSynthetic().Canonical() → resolveSlackUserIDFromCanonical)
// for consistency, but the failure handling is deliberately different: a
// recipient a channel can't deliver a PROMPT to is skipped (nothing to
// deliver); a recipient a channel can't render a MENTION for still has a
// name — falling back to that recipient's own display text (parsed
// positionally out of the field's comma-joined Value, or the whole Value if
// the counts don't line up) — because dropping a named party from a "who
// will see this data" field is the exact leak-recipient safety invariant
// info_leakage exists to protect. A Mention whose Kind isn't "slack" is
// nothing this renderer can resolve either; same fallback, no API call
// attempted.
func resolveFieldMentions(ctx context.Context, cli slackClient, fields []channelevents.InteractionField, logger logr.Logger, requestRef string) []channelevents.InteractionField {
	hasMentions := false
	for _, f := range fields {
		if len(f.Mentions) > 0 {
			hasMentions = true
			break
		}
	}
	if !hasMentions {
		return fields
	}
	out := make([]channelevents.InteractionField, len(fields))
	for i, f := range fields {
		if len(f.Mentions) == 0 {
			// Left verbatim: escapedFieldValue escapes every Value it did not
			// see resolved here. Escaping in both places would double-escape.
			out[i] = f
			continue
		}
		fallbacks := strings.Split(f.Value, ", ")
		positional := len(fallbacks) == len(f.Mentions)
		parts := make([]string, 0, len(f.Mentions))
		for j, m := range f.Mentions {
			// The fallback is publisher-supplied display text, and this
			// function is the last place it can be made inert: the Value we
			// return carries real `<@U…>` markup, so the renderer must NOT
			// sweep it again (escapePublisherPayload skips every Field that
			// carries Mentions, for exactly that reason). Sweep the publisher's
			// half here and the two halves end up correct -- their text inert,
			// our mentions live.
			//
			// inertProse, the same sweep escapePublisherPayload gives the
			// Values it does handle: a fallback is the only half of this Value
			// the publisher chose, and one escaped rendering beside one live one
			// for the same field is worse than either alone.
			fallback := inertProse(f.Value)
			if positional {
				fallback = inertProse(strings.TrimSpace(fallbacks[j]))
			}
			if m.Kind != "slack" {
				parts = append(parts, fallback)
				continue
			}
			canon, err := m.Principal().AllowSynthetic().Canonical()
			if err != nil {
				logger.Info("interaction sender: field mention canonical derivation failed; falling back to display text",
					"requestRef", requestRef, "label", f.Label, "err", err.Error())
				parts = append(parts, fallback)
				continue
			}
			slackUserID, err := resolveSlackUserIDFromCanonical(ctx, cli, canon)
			if err != nil {
				logger.Info("interaction sender: field mention resolution failed; falling back to display text",
					"requestRef", requestRef, "label", f.Label, "err", err.Error())
				parts = append(parts, fallback)
				continue
			}
			parts = append(parts, "<@"+slackUserID+">")
		}
		nf := f
		nf.Value = strings.Join(parts, ", ")
		out[i] = nf
	}
	return out
}

// buildInteractionRequestBlocks renders any interaction — prompt or notice —
// as the one house shape: a container whose rich_text_title leads with the
// category's tone chip, over the body, any inert Excerpt, the action buttons,
// and a provenance footer.
//
// Both families share the shape deliberately. A reader scanning a thread
// should not have to learn two visual languages to tell what the system is
// saying; what differs between a prompt and a notice is whether there are
// buttons, which is visible without a second idiom. Tone and finality come
// from the registry either way, so a publisher can neither pick its own
// colour nor dress a prompt as a notice.
//
// ActionKindLink actions render as URL buttons; ActionKindDecision actions
// render as buttons whose value encodes {v:"interaction", r:requestRef,
// d:actionId, c:category, s:sessRef} via the shared codec — sessRef is
// "ns/name", supplied by the caller since the payload does not carry it.
// ActionKindLinkMint is not yet rendered. The actions block is omitted when
// there are no renderable actions.
//
// This is also the wire boundary: everything the payload carries is the
// publisher's, so it is made inert here (escapePublisherPayload) before the
// shared renderer — which other callers feed markup this kind composed — ever
// sees it.
func buildInteractionRequestBlocks(p channelevents.InteractionRequestPayload, sessRef string) []slackapi.Block {
	p = escapePublisherPayload(p)
	// The registry is the authority on tone and finality. A category it does
	// not know still renders — as routine, non-terminal — because an
	// unregistered category is a wiring bug, and swallowing the message would
	// hide it behind a second failure the user pays for.
	cat, ok := channelinteractions.Get(p.Category)
	if !ok {
		cat = channelinteractions.Category{Tone: channelinteractions.ToneRoutine}
	}
	return buildInteractionBlocks(p, cat, sessRef)
}

// interactionTierMarker returns the terse mrkdwn prefix that carries a
// line's tier as TEXT — Slack's only channel for it, since it has neither
// colour nor a glyph. All three tiers get one: webchat now distinguishes
// readonly/readwrite/external with a glyph each, and marking only external
// here would leave a Slack reader unable to tell a read from a write, which
// is the central claim of this feature. external stays the most prominent —
// bold, where the other two are italic — because it is the one tier that
// cannot be undone.
func interactionTierMarker(tone channelevents.Tone) string {
	switch tone {
	case channelevents.ToneReadonly:
		return "_read:_ "
	case channelevents.ToneReadwrite:
		return "_write:_ "
	case channelevents.ToneExternal:
		return "*external:* "
	default:
		return ""
	}
}

// interactionFieldItemsMrkdwn renders a field's structured Items as an
// indented mrkdwn bullet tree — Slack's rendering of the same structure
// FieldItems.tsx lays out for webchat. Called instead of printing Field.Value
// whenever Items is present, per InteractionField's own fallback contract
// (Value is the always-present fallback; Items is what a surface with a
// design system renders instead, and Slack's Block Kit qualifies).
//
// Hint (the raw permission handle) never appears here, on purpose: Slack has
// no on-demand affordance (no hover, no tooltip) to reveal it, and per
// InteractionItem.Hint's own contract a surface that cannot show a hint must
// omit it — never concatenate it inline.
func interactionFieldItemsMrkdwn(items []channelevents.InteractionItem, depth int) string {
	var b strings.Builder
	indent := strings.Repeat("    ", depth)
	for _, it := range items {
		fmt.Fprintf(&b, "\n%s• %s", indent, interactionTierMarker(it.Tone))
		b.WriteString(it.Text)
		if it.Detail != "" {
			fmt.Fprintf(&b, " (%s)", it.Detail)
		}
		if len(it.Items) > 0 {
			b.WriteString(interactionFieldItemsMrkdwn(it.Items, depth+1))
		}
	}
	return b.String()
}

// interactionActionElements builds the button row for an interaction.
//
// ActionKindLink renders as a URL button; ActionKindDecision as a button whose
// value round-trips {requestRef, actionID, category, sessRef} through the
// shared codec. ActionKindLinkMint has no production category yet and is
// skipped rather than guessed at.
//
// The Show-Details button is appended whenever the payload carries Details.
// It uses the approval codec rather than the interaction one because the modal
// render needs only RequestRef + sessRef, not a category.
func interactionActionElements(p channelevents.InteractionRequestPayload, sessRef string) []slackapi.BlockElement {
	var elements []slackapi.BlockElement
	for _, a := range p.Actions {
		switch a.Kind {
		case channelevents.ActionKindLink:
			if a.URL == "" {
				continue
			}
			elements = append(elements, slackapi.NewButtonBlockElement(
				a.ID, "",
				slackapi.NewTextBlockObject(slackapi.PlainTextType, a.Label, false, false),
			).WithURL(a.URL).WithStyle(slackInteractionButtonStyle(a.Style)))
		case channelevents.ActionKindDecision:
			elements = append(elements, slackapi.NewButtonBlockElement(
				a.ID,
				encodeInteractionButtonValue(p.RequestRef, a.ID, p.Category, sessRef),
				slackapi.NewTextBlockObject(slackapi.PlainTextType, a.Label, false, false),
			).WithStyle(slackInteractionButtonStyle(a.Style)))
		default:
			continue
		}
	}
	if len(p.Details) > 0 {
		elements = append(elements, slackapi.NewButtonBlockElement(
			interactionDetailsActionID,
			encodeApprovalButtonValue(discInteractionDetails, p.RequestRef, "", sessRef),
			slackapi.NewTextBlockObject(slackapi.PlainTextType, "Show Details", true, false),
		))
	}
	return elements
}

// slackInteractionButtonStyle maps the channel-agnostic ActionStyle onto
// slack-go's button Style enum.
func slackInteractionButtonStyle(s channelevents.ActionStyle) slackapi.Style {
	switch s {
	case channelevents.ActionStylePrimary:
		return slackapi.StylePrimary
	case channelevents.ActionStyleDanger:
		return slackapi.StyleDanger
	default:
		return slackapi.StyleDefault
	}
}

// buildInteractionResolvedBlocks renders a resolved-outcome
// InteractionApplied as a "✅ connected" notice, naming the credential from
// OutcomeText.
func buildInteractionResolvedBlocks(p channelevents.InteractionAppliedPayload) []slackapi.Block {
	lead := "Connected"
	if ot := strings.TrimSpace(p.OutcomeText); ot != "" {
		lead = ot + " connected"
	}
	pl := channelevents.InteractionRequestPayload{
		AgentSessionRef: p.AgentSessionRef,
		Lead:            lead,
		Body:            "Your session will resume shortly.\n_If this wasn't you, ask your agent to revoke it._",
	}
	return buildInteractionBlocks(pl,
		channelinteractions.Category{Tone: channelinteractions.ToneResolved}, "")
}

// reportUndeliverable surfaces an interaction request that reached no
// recipient. Best-effort on both surfaces; neither failure is worth aborting
// the send path, but silence is not an option.
//
// In-thread so whoever is waiting learns the session is parked on a decision
// nobody was asked for, and monitoring so a platform admin sees a condition
// only they can fix (the recipients carry a kind this channel cannot deliver
// to — typically a cron session whose approver was addressed by its bento
// input kind rather than its slack output kind).
func (s *interactionSender) reportUndeliverable(
	ctx context.Context,
	sess channelkinds.SessionInfo,
	p channelevents.InteractionRequestPayload,
	recipients []channelevents.ExternalIdentity,
) {
	logger := log.FromContext(ctx)
	kinds := make([]string, 0, len(recipients))
	for _, r := range recipients {
		kinds = append(kinds, string(r.Kind))
	}
	kindList := strings.Join(kinds, ", ")

	logger.Error(fmt.Errorf("no reachable recipient"),
		"interaction sender: request undeliverable; session will park with nobody asked",
		"session", sess.Namespace+"/"+sess.Name,
		"category", p.Category, "requestRef", p.RequestRef,
		"recipientKinds", kindList)

	// Through publicNoteText like every other public note, even though this
	// copy is the kind's own: p.Category is interpolated into it straight off
	// the wire, and this lands in the same two markup-parsing sinks. No
	// mentions clause — there is nobody to wait on, which is what it says.
	if _, err := s.postPublicNote(ctx, sess, p.RequestRef, publicNoteText(fmt.Sprintf(
		"⚠️ This step needs approval, but I couldn't reach anyone who can approve it — "+
			"so nothing is waiting on you. A platform admin has been notified. (%s)", p.Category), ""),
	); err != nil {
		logger.Info("interaction sender: undeliverable in-thread notice failed",
			"requestRef", p.RequestRef, "err", err.Error())
	}

	if s.deps.NATSPublish == nil {
		return
	}
	if err := channelevents.PublishMonitoring(s.deps.NATSPublish, channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelError,
		Category:   "session",
		Transition: channelevents.MonitoringTransitionFailed,
		Source: channelevents.MonitoringSourceRef{
			Kind: "AgentSession", Namespace: sess.Namespace, Name: sess.Name,
		},
		Condition: "InteractionUndeliverable",
		Reason:    "NoReachableRecipient",
		Summary: fmt.Sprintf("%s request %s reached no recipient; the session is parked awaiting a decision nobody was asked for.",
			p.Category, p.RequestRef),
		Hint: fmt.Sprintf("recipients were addressed with kind(s) [%s], which this channel cannot deliver to; "+
			"for a cron session the approver must be addressed by the OUTPUT channel's kind, not the input kind", kindList),
		Timestamp: time.Now().UTC(),
	}); err != nil {
		logger.Info("interaction sender: undeliverable monitoring event failed",
			"requestRef", p.RequestRef, "err", err.Error())
	}
}
