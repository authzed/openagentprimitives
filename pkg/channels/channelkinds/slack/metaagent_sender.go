// pkg/channels/channelkinds/slack/metaagent_sender.go
//
// The two metaagent sub-channel senders: the scope-approval prompt (Block Kit
// with decision buttons) and the one-line private notice (chat.postEphemeral).
//
// Both lived in internal/cmd/channelsd until they were moved here. That file was the
// only one in the transport-agnostic binary importing slack-go, and it read
// binding.External["channel_id"] / ["thread_ts"] — keys only this package ever
// writes — so a session bound to any other kind had its scope approval dropped
// with an Info log while the runner blocked the whole approval window and then
// halted. Behind SubChannelSender, "this kind cannot render it" is a nil
// Sender the caller already handles, and adding a kind that CAN render it is a
// new sender rather than an edit to channelsd.
package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// metaagentTarget is where a metaagent surface is posted: the Slack channel
// and (optionally) the thread anchoring the session.
type metaagentTarget struct {
	channelID string
	threadTS  string
}

// metaagentTargetFor reads this kind's own routing keys off the binding the
// caller resolved. Only slack writes these keys, which is exactly why the
// read belongs here and not in the generic handler.
func metaagentTargetFor(sess channelkinds.SessionInfo) (metaagentTarget, error) {
	if sess.Channel == nil {
		return metaagentTarget{}, fmt.Errorf("session %s/%s has no channel binding", sess.Namespace, sess.Name)
	}
	t := metaagentTarget{
		channelID: sess.Channel.External["channel_id"],
		threadTS:  sess.Channel.External["thread_ts"],
	}
	if t.channelID == "" {
		return metaagentTarget{}, fmt.Errorf("session %s/%s binding has no channel_id", sess.Namespace, sess.Name)
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// metaagent_scope_approval
// ---------------------------------------------------------------------------

// metaagentScopeApprovalSender implements channelkinds.Sender for the
// "metaagent_scope_approval" sub-channel.
type metaagentScopeApprovalSender struct {
	client slackClient
	// refs is the per-process cache the listener's Show Details handler reads.
	// The sender records every prompt it posts so a click renders the real
	// request instead of the degraded fallback; writing it here, next to the
	// post, is what keeps the two from drifting.
	refs *metaagentApprovalRefCache
}

// Send renders the three-region approval prompt into the session's thread.
//
// Cold-start approvals are EPHEMERAL to the session starter — for a cold start
// the approver IS the starter — and the ephemeral target needs the raw Slack
// user_id, which the payload's canonical Requester is not. It comes from the
// session annotation the relay copies into SessionInfo.Annotations. When that
// is missing we post in-thread instead of dropping: a visible prompt to the
// wrong-sized audience beats an invisible one, because the runner is blocking
// on this decision.
func (s *metaagentScopeApprovalSender) Send(
	ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope,
) (channelkinds.SubChannelSendResult, error) {
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name)

	if s.client == nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("metaagent_scope_approval sender: slack client unconfigured (Secret missing bot token)")
	}
	var pl scope.MetaagentApprovalPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("metaagent_scope_approval sender: parse payload: %w", err)
	}
	target, err := metaagentTargetFor(sess)
	if err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("metaagent_scope_approval sender: %w", err)
	}

	sessRef := sess.Namespace + "/" + sess.Name

	// Decode Slack's mention encoding before anything renders or caches this
	// payload. Verbatim is the requester's raw message with only the metaagent
	// bot's own mention stripped, so every OTHER "<@U…>" in it — and any the
	// composer carried into its prose — reaches the approver as an opaque id
	// inside a blockquote that escaping has made literal.
	//
	// Here rather than in the three renderers: this is the one place with a
	// client, a ctx and a payload at once, and resolving here means the ref
	// cache below records the decoded text, so Show Details and the permanent
	// resolution record inherit it without a lookup per click. See
	// resolveMetaagentMentions for why Requester is left alone.
	pl = resolveMetaagentMentions(ctx, s.client, pl, logger)

	// The notification preview is the card's SECOND sink and a separate code
	// path from its blocks: MsgOptionText(_, false) means slack-go escapes
	// nothing, and this is the string a push notification and the channel list
	// show. It runs the same sweep the blocks do (escapeMetaagentApproval,
	// inert.go) and then composes the `<@…>` mention itself, in that order, so
	// the summary is inert and the mention stays clickable.
	preview := escapeMetaagentApproval(pl)
	opts := []slackapi.MsgOption{
		slackapi.MsgOptionBlocks(buildMetaagentScopeApprovalBlocks(pl, sessRef)...),
		slackapi.MsgOptionText(
			fmt.Sprintf("Scope-change request from <@%s>: %s", preview.Requester, preview.ApproverSummary), false),
	}
	if target.threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(target.threadTS))
	}

	starterSlackID := sess.Annotations[spiceboxv1alpha1.AnnotationStartedByExternalID]
	switch {
	case pl.ColdStart && starterSlackID != "":
		if _, err := s.client.PostEphemeralContext(ctx, target.channelID, starterSlackID, opts...); err != nil {
			return channelkinds.SubChannelSendResult{}, fmt.Errorf("metaagent_scope_approval sender: chat.postEphemeral: %w", err)
		}
		logger.Info("metaagent_scope_approval: posted ephemeral approval block to starter",
			"requestId", pl.RequestID, "starter", starterSlackID)
	default:
		if pl.ColdStart {
			logger.Info("metaagent_scope_approval: cold-start but no starter external id; posting non-ephemeral",
				"requestId", pl.RequestID)
		}
		if _, _, err := s.client.PostMessageContext(ctx, target.channelID, opts...); err != nil {
			return channelkinds.SubChannelSendResult{}, fmt.Errorf("metaagent_scope_approval sender: chat.postMessage: %w", err)
		}
		logger.Info("metaagent_scope_approval: posted approval block",
			"requestId", pl.RequestID, "requester", pl.Requester)
	}

	if s.refs != nil {
		s.refs.put(MetaagentApprovalRef{
			RequestID:       pl.RequestID,
			Requester:       pl.Requester,
			Verbatim:        pl.Verbatim,
			ApproverSummary: pl.ApproverSummary,
			SkippedExplain:  pl.SkippedExplain,
			CaveatExplain:   pl.CaveatExplain,
			ColdStart:       pl.ColdStart,
			CleanedTask:     pl.CleanedTask,
			ChannelID:       target.channelID,
			ThreadTS:        target.threadTS,
		})
	}
	return channelkinds.SubChannelSendResult{RequestRef: pl.RequestID}, nil
}

// ---------------------------------------------------------------------------
// metaagent_notice
// ---------------------------------------------------------------------------

// metaagentNoticeSender implements channelkinds.Sender for the
// "metaagent_notice" sub-channel: a single-line ephemeral to one user.
type metaagentNoticeSender struct {
	client slackClient
}

func (s *metaagentNoticeSender) Send(
	ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope,
) (channelkinds.SubChannelSendResult, error) {
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name)

	if s.client == nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("metaagent_notice sender: slack client unconfigured (Secret missing bot token)")
	}
	var pl channelkinds.MetaagentNoticePayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("metaagent_notice sender: parse payload: %w", err)
	}
	if strings.TrimSpace(pl.Body) == "" {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("metaagent_notice sender: payload has no body")
	}
	target, err := metaagentTargetFor(sess)
	if err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("metaagent_notice sender: %w", err)
	}

	// Turning a canonical subject into an addressable user is this kind's job:
	// the operator publishes only PendingRestart.TriggeredBy and has no Slack
	// id for the forker.
	recipient, err := channelkinds.ResolveNoticeRecipient(ctx, pl,
		func(c context.Context, canonical identity.CanonicalUserID) (string, error) {
			return resolveSlackUserIDFromCanonical(c, s.client, canonical)
		})
	if err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("metaagent_notice sender: cannot address recipient (requester=%q canonical=%q): %w",
			pl.Requester, pl.RequesterCanonical, err)
	}

	// mrkdwn sink (MsgOptionText escapes nothing), so the body is made inert
	// here. Unlike the scope-approval card's slots, nothing attacker-controlled
	// reaches this one today — every publisher composes Body from an in-process
	// literal — so this is the per-SINK rule inert.go states rather than a live
	// exposure: escaping a mrkdwn sink is lossless (Slack decodes the entities
	// back), and a body slot on a wire payload any publisher may fill should not
	// depend on all of them continuing to pass literals.
	opts := []slackapi.MsgOption{slackapi.MsgOptionText(escapeSlackText(pl.Body), false)}
	if target.threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(target.threadTS))
	}
	if _, err := s.client.PostEphemeralContext(ctx, target.channelID, recipient, opts...); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("metaagent_notice sender: chat.postEphemeral to %s: %w", recipient, err)
	}
	logger.Info("metaagent_notice: posted ephemeral", "recipient", recipient)
	return channelkinds.SubChannelSendResult{}, nil
}

// ---------------------------------------------------------------------------
// Block Kit
// ---------------------------------------------------------------------------

// buildMetaagentScopeApprovalBlocks renders the three-region Block Kit layout
// for a metaagent scope-approval request.
//
// Region 1: verbatim user text as a mrkdwn blockquote (> …). Mid-session
// requests prefix it with @{requester}: (the asker may differ from the
// approver); cold-start omits the prefix (the approver just started the session).
//
// Region 2: ApproverSummary as a section block.
// Region 3 (optional): ⚠ Heads up — SkippedExplain and/or CaveatExplain.
// Buttons: Approve, Deny, Show Details.
//
// This is a WIRE BOUNDARY: pl arrives over NATS and every region below is a
// MarkdownType section, so the publisher's slots are made inert HERE, on a
// local copy, before this function composes any markup of its own — see
// escapeMetaagentApproval (inert.go) for why the six slots are untrusted and
// why the order matters. The caller's pl is deliberately untouched: the sender
// records it verbatim into the ref cache, which is the record of what was
// published and has its own sink-side sweep.
func buildMetaagentScopeApprovalBlocks(pl scope.MetaagentApprovalPayload, sessRef string) []slackapi.Block {
	pl = escapeMetaagentApproval(pl)

	var blocks []slackapi.Block

	// Region 1: verbatim request text. For cold-start the approver IS the person
	// who just started the session, so the @requester prefix is noise — omit it.
	// Mid-session @metaagent requests keep the mention so the approver sees who
	// asked (it may be a different user in the thread).
	//
	// Every region below caps through capInertRunes rather than
	// stringsx.CapRunes — same clip, but it re-closes a code span the cut
	// opened. The sweep above wraps each URL in a PAIR of backticks, and these
	// caps run after it, so a cut landing between the two drops the closing one
	// and the truncated URL linkifies again (b3c01bf1's second App Home defect,
	// which arrives on every surface the moment that surface starts defusing).
	// The requester can choose one whitespace-free run straddling the cap.
	quoted := strings.ReplaceAll(pl.Verbatim, "\n", "\n> ")
	region1 := "> " + quoted
	if !pl.ColdStart {
		region1 = fmt.Sprintf("<@%s>:\n> %s", pl.Requester, quoted)
	}
	blocks = append(blocks, slackapi.NewSectionBlock(
		slackapi.NewTextBlockObject(slackapi.MarkdownType,
			capInertRunes(region1, 2800), false, false),
		nil, nil,
	))

	// Region 2: approver summary.
	if strings.TrimSpace(pl.ApproverSummary) != "" {
		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType,
				capInertRunes(pl.ApproverSummary, 2800), false, false),
			nil, nil,
		))
	}

	// Cold-start region: the task the agent will be asked to run if approved.
	// Rendered between the approver summary and the warnings.
	if pl.ColdStart {
		if strings.TrimSpace(pl.CleanedTask) != "" {
			region := fmt.Sprintf("…and the agent will be asked to:\n> %s",
				strings.ReplaceAll(pl.CleanedTask, "\n", "\n> "))
			blocks = append(blocks, slackapi.NewSectionBlock(
				slackapi.NewTextBlockObject(slackapi.MarkdownType,
					capInertRunes(region, 2800), false, false),
				nil, nil,
			))
		} else {
			blocks = append(blocks, slackapi.NewSectionBlock(
				slackapi.NewTextBlockObject(slackapi.MarkdownType,
					"_(no task — this message only sets permissions)_", false, false),
				nil, nil,
			))
		}
	}

	// Region 3: warnings (only when Skipped or Caveats lines are present).
	var warnLines []string
	if strings.TrimSpace(pl.SkippedExplain) != "" {
		warnLines = append(warnLines, pl.SkippedExplain)
	}
	if strings.TrimSpace(pl.CaveatExplain) != "" {
		warnLines = append(warnLines, pl.CaveatExplain)
	}
	if len(warnLines) > 0 {
		var sb strings.Builder
		sb.WriteString("⚠ *Heads up:*")
		for _, line := range warnLines {
			sb.WriteString("\n• ")
			sb.WriteString(capInertRunes(line, 300))
		}
		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType,
				sb.String(), false, false),
			nil, nil,
		))
	}

	// Buttons. The button value JSON carries the decision string, which the
	// Slack callback decodes (v.D) and routes to authzd.
	btn := func(decision, label string) *slackapi.ButtonBlockElement {
		return slackapi.NewButtonBlockElement(
			fmt.Sprintf("metaagent_%s_%s", decision, pl.RequestID),
			MetaagentApprovalButtonValue(pl.RequestID, sessRef, decision),
			slackapi.NewTextBlockObject(slackapi.PlainTextType, label, true, false),
		)
	}
	if pl.ColdStart {
		// Cold-start: five actions. approve_cleaned / approve_original /
		// run_without_scope are all "proceed" variants; deny aborts the turn.
		blocks = append(blocks, slackapi.NewActionBlock("metaagent_approval_actions",
			btn(MetaagentDecisionApproveCleaned, "Approve").WithStyle(slackapi.StylePrimary),
			btn(MetaagentDecisionApproveOriginal, "Approve w/ original"),
			btn(MetaagentDecisionRunWithoutScope, "Run w/o scope"),
			btn(MetaagentDecisionDeny, "Deny").WithStyle(slackapi.StyleDanger),
			btn(MetaagentDecisionShowDetails, "Show Details"),
		))
	} else {
		// Mid-session: three actions (unchanged).
		blocks = append(blocks, slackapi.NewActionBlock("metaagent_approval_actions",
			btn(MetaagentDecisionApprove, "Approve").WithStyle(slackapi.StylePrimary),
			btn(MetaagentDecisionDeny, "Deny").WithStyle(slackapi.StyleDanger),
			btn(MetaagentDecisionShowDetails, "Show Details"),
		))
	}

	// Footer context.
	blocks = append(blocks, slackapi.NewContextBlock("",
		slackapi.NewTextBlockObject(slackapi.MarkdownType,
			fmt.Sprintf("Session `%s` · Request `%s`", sessRef, pl.RequestID),
			false, false),
	))

	return blocks
}
