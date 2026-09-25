// pkg/channels/channelkinds/slack/metaagent_callback.go
//
// F5: Slack button-callback routing for metaagent scope-approval blocks.
// Action-IDs on these buttons are prefixed "metaagent_". The session ref
// (ns/name) and requestId are carried in the button value JSON.
package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/metaagentaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// metaagentApprovalValue is the JSON shape embedded in every metaagent
// scope-approval button's value field. Kept compact so it fits Slack's
// ~2000-byte button value limit.
type metaagentApprovalValue struct {
	// V is the "metaagent_approval" sentinel; anything else is a foreign click.
	V string `json:"v"`
	// R is the scope-approval request this click answers.
	R string `json:"r"`
	// S is the session the request belongs to, as "namespace/name".
	S string `json:"s"`
	// D is the verdict: approve|deny|show_details mid-session, or
	// approve_cleaned|approve_original|run_without_scope|deny|show_details on a
	// cold start.
	D string `json:"d"`
}

// handleMetaagentApprovalAction recognizes a "metaagent_*" block_action
// and routes it to authzd via ap.session.<ns>.<name>.in.metaagent_approval_applied.
//
// Discriminator: BlockActions[0].ActionID has prefix "metaagent_" AND the
// button value JSON has v:"metaagent_approval". Returns (true, nil) when the
// click was recognized and processed; (false, nil) when not a metaagent click.
func (l *slackListener) handleMetaagentApprovalAction(ctx context.Context, cb slackapi.InteractionCallback) (bool, error) {
	if len(cb.ActionCallback.BlockActions) == 0 {
		return false, nil
	}
	logger := log.FromContext(ctx)

	for _, a := range cb.ActionCallback.BlockActions {
		if !strings.HasPrefix(a.ActionID, "metaagent_") {
			continue
		}
		// Parse the value JSON — this is the authoritative discriminator.
		var v metaagentApprovalValue
		if err := json.Unmarshal([]byte(a.Value), &v); err != nil {
			// Malformed value — not a metaagent button we own.
			continue
		}
		if v.V != "metaagent_approval" {
			continue
		}
		// Recognized: this is a metaagent approval button.
		ns, name, ok := strings.Cut(v.S, "/")
		if !ok || ns == "" || name == "" {
			return true, fmt.Errorf("metaagent approval: malformed session ref %q in action %q", v.S, a.ActionID)
		}

		switch v.D {
		case MetaagentDecisionApprove,
			MetaagentDecisionDeny,
			MetaagentDecisionApproveCleaned,
			MetaagentDecisionApproveOriginal,
			MetaagentDecisionRunWithoutScope:
			// All "proceed" variants (mid-session approve + the three cold-start
			// approving actions) report approved:true; deny reports false. The
			// action string disambiguates among the cold-start variants for
			// authzd's cold-start decider — the mid-session "approve" carries it
			// too, harmlessly (the mid-session decider ignores Action).
			approved := v.D != MetaagentDecisionDeny
			// Resolve + canonicalize the clicker so authzd can gate them on
			// agentsession#manage_scope (= owner). authzd has no Slack API to
			// canonicalize a bare user_id, and the owner tuples are email-keyed
			// canonical (user:<base64(email)>); so the canonicalization MUST
			// happen here, where resolveIdentity can derive the trusted email.
			// Same shape the tool_approval click uses. approverId (the raw
			// user_id) is kept for display / back-compat.
			clickerExt := l.resolveIdentity(ctx, cb.User.ID)
			// The clicker is gated on agentsession#manage_scope (= owner), so
			// they must resolve to a real user:<email>. No verified email ⇒
			// surface an authz failure rather than mint a synthetic subject
			// the owner-check would reject.
			clickerCanon, ccerr := identity.FromExternal(
				identity.Kind(clickerExt.Kind), identity.TeamScope(clickerExt.TeamScope),
				identity.RawExternalID(clickerExt.ExternalID), identity.Email(clickerExt.Email),
			).Canonical()
			if ccerr != nil {
				return true, fmt.Errorf("metaagent approval: approver identity unresolved (no verified email): %w", ccerr)
			}
			approverCanonical := clickerCanon.Subject().String()
			// ns/name come from the button value, which only this app writes and
			// which reaches us inside a Slack-signature-verified interaction
			// payload — but they are still only a routing hint here. authzd
			// re-derives the session from the SUBJECT this publishes on and
			// re-checks the clicker against that session's owner tuple, so a
			// wrong pair can misdeliver a click, never authorize one.
			if err := channelevents.PublishMetaagentIn(l.deps.NATSPublish, ns, name,
				channelevents.KindMetaagentApprovalApplied,
				channelevents.MetaagentApprovalAppliedPayload{
					RequestID:         v.R,
					Approved:          approved,
					ApproverID:        cb.User.ID,
					ApproverCanonical: approverCanonical,
					Action:            v.D,
				},
			); err != nil {
				logger.Info("metaagent approval: publish metaagent_approval_applied failed",
					"session", ns+"/"+name, "err", err.Error())
				// The publish failed: authzd never receives the decision and
				// the scope-change request hangs unresolved. Tell the approver
				// their click didn't register so they can retry.
				l.surfaceClickFailure(ctx, cb.ResponseURL, cb.Container.ChannelID, cb.User.ID,
					"⚠️ Couldn't record your scope decision — the agent wasn't reachable. Please try clicking again.")
				return true, fmt.Errorf("metaagent approval: publish approval_applied: %w", err)
			}
			logger.Info("metaagent approval: published metaagent_approval_applied",
				"requestId", v.R, "decision", v.D, "session", v.S, "approver", cb.User.ID)
			// Cold-start blocks are ephemeral (only the starter saw them); post a
			// permanent resolution message into the thread so the decision leaves
			// a durable, channel-visible record.
			l.postMetaagentColdStartResolution(ctx, v.R, v.S, v.D, cb)
			return true, nil

		case MetaagentDecisionShowDetails:
			// Look up the cached payload from metaagentApprovalRefs.
			// Post an ephemeral with the technical summary instead of a modal
			// so we don't need a trigger_id round-trip for restarts.
			return true, l.handleMetaagentShowDetails(ctx, cb, v.R, v.S)

		default:
			logger.Info("metaagent approval: unknown decision in action_id",
				"actionID", a.ActionID, "decision", v.D)
			// The click was recognized as ours but carries a decision we don't
			// handle (e.g. a stale button shape after a schema bump). Tell the
			// clicker rather than leaving the button silently dead.
			l.surfaceClickFailure(ctx, cb.ResponseURL, cb.Container.ChannelID, cb.User.ID,
				"⚠️ This action isn't recognized — it may be from an outdated message. Please retry from a fresh prompt.")
			return true, nil
		}
	}
	return false, nil
}

// postMetaagentColdStartResolution posts a permanent (non-ephemeral) message
// into the session thread recording a cold-start scope decision. The cold-start
// approval block itself is ephemeral (only the starter sees it); this is the
// durable, channel-visible record of what was decided. No-op for mid-session
// approvals (their block is already a permanent in-thread message) and when the
// request cannot be resolved from either the cache or memory.
func (l *slackListener) postMetaagentColdStartResolution(ctx context.Context, requestID, sessRef, decision string, cb slackapi.InteractionCallback) {
	if l.api == nil {
		return
	}
	logger := log.FromContext(ctx)
	ref, found := l.resolveMetaagentApprovalRef(ctx, requestID, sessRef)
	if !found {
		// Nothing left to write the record from. Say so: this is the only
		// channel-visible trace the decision would ever have left, and its
		// absence is otherwise indistinguishable from a mid-session approval,
		// which deliberately posts nothing.
		logger.Info("metaagent resolution: request not found in cache or memory; no permanent record posted",
			"requestId", requestID, "sessRef", sessRef, "decision", decision, "clicker", cb.User.ID)
		return
	}
	if !ref.ColdStart {
		return // mid-session: the approval block is already a permanent message
	}
	channelID := ref.ChannelID
	if channelID == "" {
		channelID = cb.Container.ChannelID
	}
	threadTS := ref.ThreadTS
	if threadTS == "" {
		threadTS = cb.Container.ThreadTs
	}
	if channelID == "" {
		logger.Info("metaagent resolution: no channel to post into; skipping", "requestId", requestID)
		return
	}
	text := renderMetaagentColdStartResolution(ref, decision)
	opts := []slackapi.MsgOption{slackapi.MsgOptionText(text, false)}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}
	if _, _, err := l.api.PostMessageContext(ctx, channelID, opts...); err != nil {
		logger.Info("metaagent resolution: post in-thread message failed",
			"requestId", requestID, "err", err.Error())
	}
}

// renderMetaagentColdStartResolution formats the permanent thread record for a
// cold-start scope decision: a decision header, the verbatim request as a
// blockquote, and (on approve) the applied scope summary + the task that will run.
//
// A cold-start card is EPHEMERAL to the starter, so this in-thread post is the
// only channel-visible trace of what was decided — it outlives the card and
// quotes the request back — which is why it runs the sweep itself rather than
// relying on the card having done it (escapeMetaagentRef, inert.go).
func renderMetaagentColdStartResolution(ref MetaagentApprovalRef, decision string) string {
	ref = escapeMetaagentRef(ref)

	var header, tail string
	switch decision {
	case MetaagentDecisionApproveCleaned, MetaagentDecisionApprove:
		header = "✅ Scope approved"
		if strings.TrimSpace(ref.ApproverSummary) != "" {
			tail += "\nApplied: " + ref.ApproverSummary
		}
		if strings.TrimSpace(ref.CleanedTask) != "" {
			tail += "\nRunning: " + ref.CleanedTask
		}
	case MetaagentDecisionApproveOriginal:
		header = "✅ Scope approved"
		if strings.TrimSpace(ref.ApproverSummary) != "" {
			tail += "\nApplied: " + ref.ApproverSummary
		}
		tail += "\nRunning the original request."
	case MetaagentDecisionRunWithoutScope:
		header = "⏭️ Running without scope changes"
	case MetaagentDecisionDeny:
		header = "🚫 Scope denied — the agent will not run this request."
	default:
		header = "Scope decision recorded"
	}
	var sb strings.Builder
	sb.WriteString(header)
	if strings.TrimSpace(ref.Verbatim) != "" {
		sb.WriteString("\n> " + strings.ReplaceAll(ref.Verbatim, "\n", "\n> "))
	}
	sb.WriteString(tail)
	return sb.String()
}

// resolveMetaagentApprovalRef returns the approval details for requestID:
// first from the in-process cache the sender populated, then — on a miss —
// from the metaagent_audit request record authzd persisted at publish time.
//
// The cache has no durable twin and does not survive a channelsd restart, so
// every reader of it needs this fallback. Both button handlers share the one
// resolver rather than each deciding for itself how hard to look — a handler
// that skips the fallback loses the permanent record on exactly the restart
// this is here to survive.
func (l *slackListener) resolveMetaagentApprovalRef(ctx context.Context, requestID, sessRef string) (MetaagentApprovalRef, bool) {
	if l.metaagentApprovalRefs != nil {
		if ref, found := l.metaagentApprovalRefs.get(requestID); found {
			return ref, true
		}
	}
	if l.deps.Memory == nil || sessRef == "" {
		return MetaagentApprovalRef{}, false
	}
	c, err := metaagentaudit.RequestByID(ctx, l.deps.Memory,
		memory.Scope{Kind: "session", ID: sessRef}, requestID)
	if err != nil {
		log.FromContext(ctx).Info("metaagent: approval-request memory lookup failed",
			"requestId", requestID, "sessRef", sessRef, "err", err.Error())
		return MetaagentApprovalRef{}, false
	}
	if c == nil {
		return MetaagentApprovalRef{}, false
	}
	// ChannelID/ThreadTS are deliberately absent: the durable record carries no
	// Slack coordinates. Callers that need them fall back to the interaction
	// callback's own container, which points at the very block being clicked.
	ref := MetaagentApprovalRef{
		RequestID:       c.RequestID,
		Requester:       c.Requester,
		Verbatim:        c.RequestText,
		ApproverSummary: c.ComposerOutput.ApproverSummary,
		SkippedExplain:  c.ComposerOutput.SkippedExplain,
		CaveatExplain:   c.ComposerOutput.CaveatExplain,
		ColdStart:       c.ColdStart,
		CleanedTask:     c.CleanedTask,
	}
	// authzd persisted the request BEFORE any Slack rendering touched it, so
	// its text still carries Slack's "<@U…>" encoding — where the cached copy
	// holds the decoded names the sender already resolved. Decode here so the
	// same card reads the same way either side of a channelsd restart; without
	// it, surviving the restart would visibly downgrade every name to an id.
	// Costs one users.info per distinct id, on the miss path only.
	if l.api != nil {
		ref = refWithResolvedMentions(ctx, l.api, ref, log.FromContext(ctx))
	}
	return ref, true
}

// refWithResolvedMentions is resolveMetaagentMentions over the cache's own
// type. The two structs carry the same six slots for the same three renderers,
// so they must agree on which five get decoded and which one — Requester —
// stays a raw id for the renderers to compose a live mention from.
func refWithResolvedMentions(ctx context.Context, cli userNamer, ref MetaagentApprovalRef, logger logr.Logger) MetaagentApprovalRef {
	p := resolveMetaagentMentions(ctx, cli, scope.MetaagentApprovalPayload{
		RequestID:       ref.RequestID,
		Verbatim:        ref.Verbatim,
		ApproverSummary: ref.ApproverSummary,
		SkippedExplain:  ref.SkippedExplain,
		CaveatExplain:   ref.CaveatExplain,
		CleanedTask:     ref.CleanedTask,
	}, logger)
	ref.Verbatim = p.Verbatim
	ref.ApproverSummary = p.ApproverSummary
	ref.SkippedExplain = p.SkippedExplain
	ref.CaveatExplain = p.CaveatExplain
	ref.CleanedTask = p.CleanedTask
	return ref
}

// handleMetaagentShowDetails posts an ephemeral message with the request
// details to the button clicker, resolved via resolveMetaagentApprovalRef. A
// degraded message is posted only when BOTH the cache and memory miss.
func (l *slackListener) handleMetaagentShowDetails(ctx context.Context, cb slackapi.InteractionCallback, requestID, sessRef string) error {
	if l.api == nil {
		return nil
	}
	logger := log.FromContext(ctx)

	ns, name, ok := strings.Cut(sessRef, "/")
	if !ok {
		return fmt.Errorf("metaagent show_details: malformed session ref %q", sessRef)
	}

	var text string
	if ref, found := l.resolveMetaagentApprovalRef(ctx, requestID, sessRef); found {
		text = renderMetaagentShowDetailsText(ref)
	}
	if text == "" {
		logger.Info("metaagent show_details: no cached or persisted payload; showing degraded message",
			"requestId", requestID, "sessRef", sessRef, "clicker", cb.User.ID)
		text = fmt.Sprintf("_Details for this request are not available._\n"+
			"Request ID: `%s` · Session: `%s/%s`", requestID, ns, name)
	}

	// Determine the channel + thread to post into. Use the container info
	// from the interaction callback (the channel where the approval block lives).
	channelID := cb.Container.ChannelID
	threadTS := cb.Container.MessageTs
	if channelID == "" {
		return nil // no channel to post into
	}

	opts := []slackapi.MsgOption{slackapi.MsgOptionText(text, false)}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}
	if _, err := l.api.PostEphemeralContext(ctx, channelID, cb.User.ID, opts...); err != nil {
		logger.Info("metaagent show_details: post ephemeral failed",
			"requestId", requestID, "clicker", cb.User.ID, "err", err.Error())
	}
	return nil
}

// renderMetaagentShowDetailsText formats the cached approval payload as plain
// mrkdwn for the Show Details ephemeral.
//
// It re-renders the same six publisher slots the card does, through a second
// and entirely independent renderer, so it runs the sweep itself
// (escapeMetaagentRef, inert.go). A card made inert whose Show Details is not
// would be the shape this package keeps re-finding: one escaped rendering
// beside one live one, for the same payload.
func renderMetaagentShowDetailsText(ref MetaagentApprovalRef) string {
	ref = escapeMetaagentRef(ref)

	var sb strings.Builder
	sb.WriteString("*Scope-change request details*\n")
	sb.WriteString(fmt.Sprintf("Request ID: `%s`\n", ref.RequestID))
	if ref.Requester != "" {
		sb.WriteString(fmt.Sprintf("Requested by: <@%s>\n", ref.Requester))
	}
	if ref.Verbatim != "" {
		sb.WriteString(fmt.Sprintf("Original request:\n> %s\n", ref.Verbatim))
	}
	if ref.ApproverSummary != "" {
		sb.WriteString(fmt.Sprintf("Summary: %s\n", ref.ApproverSummary))
	}
	if ref.CleanedTask != "" {
		sb.WriteString(fmt.Sprintf("Task if approved:\n> %s\n", ref.CleanedTask))
	}
	return strings.TrimRight(sb.String(), "\n")
}
