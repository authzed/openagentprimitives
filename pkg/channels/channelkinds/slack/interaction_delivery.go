// pkg/channels/channelkinds/slack/interaction_delivery.go
//
// Delivery mechanics for the Interaction renderer: surface routing (per the
// category's channelinteractions.SurfaceType) and an in-process delivery-record
// cache so a later interaction_applied can edit the exact message(s) the
// request posted. The cache is best-effort — in-process and lost on restart, at
// which point buildInteractionAppliedBlocks degrades to a verdict-only card.
package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// deliveryRef is where a posted message landed, so a later edit can target it.
type deliveryRef struct {
	ChannelID string // C… (ephemeral/channel) or D… (DM)
	TS        string
}

// deliveryEntry is one interactionDeliveryStore row: where the prompt and
// (optional) public note landed, plus the request's Details blob cached at
// delivery time for the Show-Details modal.
type deliveryEntry struct {
	prompt, publicNote deliveryRef
	// details is the request payload's Details (channelevents.
	// InteractionRequestPayload.Details, e.g. channelevents.
	// ToolApprovalDetails json), cached so the listener's generic
	// Show-Details click handler (interaction_details.go) can render the
	// modal without a second round-trip to memory. nil when the category
	// carried no Details.
	details json.RawMessage
	// request is the prompt payload this entry was posted for, cached so the
	// later interaction_applied edit can rebuild the card WITH its detail.
	// The Applied payload carries only a verdict, so without this the edit
	// can say "Approved" but not what was approved — see
	// buildInteractionAppliedBlocks. hasRequest distinguishes "not recorded"
	// from a zero-valued payload.
	request    channelevents.InteractionRequestPayload
	hasRequest bool
}

// interactionDeliveryStore remembers, per interaction requestRef, where the
// prompt and (optional) public note were posted, plus the request's Details
// blob. In-process + best-effort: a miss (channelsd restart, other replica)
// is a graceful skip — handleInteractionDetailsAction falls back to the
// durable memapproval record on a miss.
//
// Shared between the "interaction" sub-channel sender (writes, at request
// delivery time — see sendRequest) and the slackListener (reads, for the
// generic Show-Details modal): both hold the SAME instance, handed out by
// Kind.sharedInteractionDelivery (kind.go), the same process-wide-singleton
// pattern sharedAuthzRefs uses for the legacy tool_approval Show Details
// cache. Guarded by mu because the sender's request-time write and the
// listener's click-time read run on different goroutines.
type interactionDeliveryStore struct {
	mu      sync.Mutex
	entries map[string]deliveryEntry
}

func newInteractionDeliveryStore() *interactionDeliveryStore {
	return &interactionDeliveryStore{entries: map[string]deliveryEntry{}}
}

func (s *interactionDeliveryStore) record(requestRef string, prompt, publicNote deliveryRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[requestRef]
	e.prompt, e.publicNote = prompt, publicNote
	s.entries[requestRef] = e
}

func (s *interactionDeliveryStore) get(requestRef string) (prompt, publicNote deliveryRef, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[requestRef]
	return e.prompt, e.publicNote, ok
}

// recordDetails caches a request's Details blob, keyed by requestRef —
// independent of record() above (a Details-only payload with no decision
// action and no public note would otherwise never get an entry). Upserts:
// safe to call whether or not record() has already written prompt/publicNote
// for this requestRef.
func (s *interactionDeliveryStore) recordDetails(requestRef string, details json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[requestRef]
	e.details = details
	s.entries[requestRef] = e
}

// getDetails returns the cached Details blob for requestRef. ok is false
// when there is no entry, or the entry carries no Details (e.g. a category
// whose prompt only got recorded for its decision action / public note).
func (s *interactionDeliveryStore) getDetails(requestRef string) (json.RawMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[requestRef]
	if !ok || len(e.details) == 0 {
		return nil, false
	}
	return e.details, true
}

// recordRequest caches the prompt payload for requestRef, so the resolving
// interaction_applied can rebuild the card with the detail the Applied
// envelope does not carry. Upserts alongside record/recordDetails, which are
// written for the same requestRef at the same moment.
func (s *interactionDeliveryStore) recordRequest(requestRef string, p channelevents.InteractionRequestPayload) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[requestRef]
	e.request, e.hasRequest = p, true
	s.entries[requestRef] = e
}

// getRequest returns the cached prompt payload for requestRef. ok is false
// when nothing was recorded — a channelsd restart between prompt and decision,
// or another replica — which callers must treat as a detail-less degrade, not
// an error.
func (s *interactionDeliveryStore) getRequest(requestRef string) (channelevents.InteractionRequestPayload, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[requestRef]
	if !ok || !e.hasRequest {
		return channelevents.InteractionRequestPayload{}, false
	}
	return e.request, true
}

// drop removes a delivery record once its interaction is resolved, so the
// store does not grow unbounded across a long-lived process. Drops the
// Details blob too — once resolved, the prompt's buttons (including
// Show-Details) are stripped by sendDecisionApplied, so nothing can click it
// again.
func (s *interactionDeliveryStore) drop(requestRef string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, requestRef)
}

// deliveryEditable reports whether a request's delivery must be remembered so
// a later interaction_applied can edit it in place: true when the prompt
// carries a decision action (its buttons get stripped on resolution) or a
// public note (its text gets updated). Link-only / notice cards are never
// edited, so they are not recorded — keeping the delivery store bounded to
// pending interactions.
func deliveryEditable(p channelevents.InteractionRequestPayload) bool {
	if p.Audience.PublicNote {
		return true
	}
	for _, a := range p.Actions {
		if a.Kind == channelevents.ActionKindDecision {
			return true
		}
	}
	return false
}

// notifyOption renders the plain-text `text` field every interaction delivery
// carries: the push-notification preview and the channel-list line, which Slack
// shows where no block renders.
//
// It ESCAPES, and that is the ONE thing making that field inert. Every caller
// here passes the request payload's Lead, the single publisher slot
// escapePublisherPayload deliberately leaves live: the Lead's CARD sink is a
// rich_text element Slack renders literally, where escaping would instead show
// a reader "&amp;" for an ordinary ampersand. The `text` field is the opposite
// kind of sink — Slack parses it as mrkdwn — so the same string unescaped opens
// a real `<!channel>` ping or a `<url|label>` span that reads as a
// platform-authored action, and on postBroadcast that lands in a PUBLIC channel
// post. Escaping is lossless here: Slack decodes the entities back to the
// literal characters when it renders.
//
// The DM thread title (deliverPrompt, below) is deliberately NOT run through
// this: a title is not a markup surface, so entities there would be visible
// rather than defused.
func notifyOption(notifyText string) slackapi.MsgOption {
	return slackapi.MsgOptionText(escapeSlackText(notifyText), false)
}

// postBroadcast posts blocks PUBLICLY (never ephemeral, never a DM) to the
// session's channel/thread — the AudienceParticipants delivery mode
// (sendRequest's broadcast case): the prompt is addressee-less (e.g.
// provider_error_retry's no-addressee Retry button), so there is no
// recipient to resolve or DM; every interact-participant sees the same
// message, and the decision's DecideParticipant server-side standing check
// (not this delivery path) gates who may action it. Mirrors deliverPrompt's
// thread resolution (channel_id + effectiveOutboundThreadTS) but always
// posts via PostMessageContext, skipping the ephemeral/DM-fallback branch
// entirely since there is no slackUserID to target.
func (s *interactionSender) postBroadcast(ctx context.Context, sess channelkinds.SessionInfo, blocks []slackapi.Block, notifyText string) (deliveryRef, error) {
	channelID := ""
	threadTS := ""
	if sess.Channel != nil {
		channelID = sess.Channel.External["channel_id"]
		threadTS = effectiveOutboundThreadTS(sess)
	}
	if channelID == "" {
		return deliveryRef{}, fmt.Errorf("broadcast: session has no channel to post to")
	}
	opts := []slackapi.MsgOption{slackapi.MsgOptionBlocks(blocks...), notifyOption(notifyText)}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}
	_, ts, err := s.client.PostMessageContext(ctx, channelID, opts...)
	if err != nil {
		return deliveryRef{}, fmt.Errorf("PostMessage (broadcast): %w", err)
	}
	return deliveryRef{ChannelID: channelID, TS: ts}, nil
}

// deliverPrompt posts blocks to slackUserID per the category surface:
// SurfaceDMOnly always opens a DM; the default (SurfaceEphemeralDMFallback)
// posts ephemeral in the session's channel/thread and falls back to a DM on
// user_not_in_channel. Returns where the message landed.
func (s *interactionSender) deliverPrompt(ctx context.Context, sess channelkinds.SessionInfo, slackUserID string, surface channelinteractions.SurfaceType, blocks []slackapi.Block, notifyText string) (deliveryRef, error) {
	channelID := ""
	threadTS := ""
	if sess.Channel != nil {
		channelID = sess.Channel.External["channel_id"]
		threadTS = effectiveOutboundThreadTS(sess)
	}

	if surface != channelinteractions.SurfaceDMOnly && channelID != "" {
		opts := []slackapi.MsgOption{slackapi.MsgOptionBlocks(blocks...), notifyOption(notifyText)}
		if threadTS != "" {
			opts = append(opts, slackapi.MsgOptionTS(threadTS))
		}
		ts, err := s.client.PostEphemeralContext(ctx, channelID, slackUserID, opts...)
		if err == nil {
			return deliveryRef{ChannelID: channelID, TS: ts}, nil
		}
		if !isUserNotInChannelErr(err) {
			return deliveryRef{}, fmt.Errorf("PostEphemeral: %w", err)
		}
		// fall through to DM
	}

	dm, _, _, err := s.client.OpenConversationContext(ctx, &slackapi.OpenConversationParameters{Users: []string{slackUserID}})
	if err != nil {
		return deliveryRef{}, fmt.Errorf("open DM with user %q: %w", slackUserID, err)
	}
	if dm == nil || dm.ID == "" {
		return deliveryRef{}, fmt.Errorf("open DM with user %q: no channel id returned", slackUserID)
	}
	_, ts, err := s.client.PostMessageContext(ctx, dm.ID,
		slackapi.MsgOptionBlocks(blocks...), notifyOption(notifyText))
	if err != nil {
		return deliveryRef{}, fmt.Errorf("PostMessage (DM): %w", err)
	}
	// Title the freshly-posted thread. Under Slack's agent_view experience a
	// proactive DM only surfaces in the recipient's Messages tab (the surface
	// visible on mobile) when its thread is titled; without this the prompt is
	// shunted to a secondary "history" surface mobile hides. Best-effort: the
	// DM already landed, so a title failure must not doom the delivery — log
	// and return the successful delivery.
	if err := s.client.SetAssistantThreadsTitleContext(ctx, slackapi.AssistantThreadsSetTitleParameters{
		ChannelID: dm.ID, ThreadTS: ts, Title: notifyText,
	}); err != nil {
		log.FromContext(ctx).Info("interaction deliverPrompt: DM setTitle failed (best-effort, continuing)",
			"user", slackUserID, "err", err.Error())
	}
	return deliveryRef{ChannelID: dm.ID, TS: ts}, nil
}
