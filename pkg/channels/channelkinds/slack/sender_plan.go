// pkg/channels/channelkinds/slack/sender_plan.go
//
// Renders KindPlanUpdate envelopes to Slack Block Kit messages. The first
// envelope for a (session, planName) pair posts a new threaded message and
// stores its ts in planMessageMap. Subsequent envelopes call chat.update on
// that ts so the plan list edits in place rather than flooding the thread.
// A deletion (empty Items) edits the existing message to a "Plan cancelled"
// stub — chat.delete is intentionally avoided to keep an audit trail.
package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// pausedBannerText maps a PlanUpdatePayload.PauseCause to the Slack banner
// mrkdwn shown above a paused plan. Unknown/empty causes get a generic
// "Paused" — the renderer never panics on a cause the runner adds later.
func pausedBannerText(cause string) string {
	switch cause {
	case channelevents.PauseCauseApproval:
		return "⏸️ Paused — waiting for your approval"
	case channelevents.PauseCauseLeakageApproval:
		return "⏸️ Paused — waiting for approval to share data"
	// The idle exit and an explicit await_user_message read the same in a Slack
	// thread: the agent has stopped and the next message is the person's either
	// way, and the thread is the place to type it. The two causes are told apart
	// where it matters — a browser view that would otherwise drop a reply modal
	// over the agent's closing statement.
	case channelevents.PauseCauseReply, channelevents.PauseCauseIdle:
		return "⏸️ Waiting for your reply"
	case channelevents.PauseCauseRetry:
		return "⏸️ Paused — provider error; click *Retry* above"
	case channelevents.PauseCauseFailed:
		return "⏹️ Stopped — the agent hit an error"
	case channelevents.PauseCauseStopped:
		return "⏹️ Stopped — the session was interrupted"
	default:
		return "⏸️ Paused"
	}
}

// toSlackTaskStatus maps the generic plan-item status string to the
// Slack TaskCardStatus enum. Unknown statuses default to pending —
// the renderer never panics on a value the generic layer might add
// later. "done" maps to "complete" because the Slack primitive's
// vocabulary uses complete; both mean "finished successfully".
func toSlackTaskStatus(s string) slackapi.TaskCardStatus {
	switch s {
	case "in_progress":
		return slackapi.TaskCardStatusInProgress
	case "done":
		return slackapi.TaskCardStatusComplete
	case "error":
		return slackapi.TaskCardStatusError
	case "stopped":
		// Stopped items did not complete; error glyph signals interruption.
		return slackapi.TaskCardStatusError
	default:
		return slackapi.TaskCardStatusPending
	}
}

// plainRichText wraps a plain-text string as a *RichTextBlock with
// one rich_text_section containing one text element. Used for
// agent-supplied Details/Output, which we treat as plain text — we
// do NOT route them through mrkdwn parsing, since an asterisk in a
// file path should not become bold and rich_text isn't mrkdwn anyway.
// Returns nil for empty input so callers can skip WithDetails/
// WithOutput entirely (Slack rejects empty rich_text blocks).
func plainRichText(s string) *slackapi.RichTextBlock {
	if s == "" {
		return nil
	}
	return &slackapi.RichTextBlock{
		Type: slackapi.MBTRichText,
		Elements: []slackapi.RichTextElement{
			&slackapi.RichTextSection{
				Type: slackapi.RTESection,
				Elements: []slackapi.RichTextSectionElement{
					&slackapi.RichTextSectionTextElement{
						Type: slackapi.RTSEText,
						Text: s,
					},
				},
			},
		},
	}
}

// The plan renderer's block-rejection guard now lives in blocks_degrade.go:
// notices need the identical check for the container block, and two copies of
// a Slack-error allowlist is how they drift apart. See isUnsupportedBlocksErr
// there; the fallback to renderPlanBlocksLegacy below is unchanged.

// planMessageMap is the per-process (sessionKey, planName) → message ts map.
// Lives on each slackSender instance; the sender's Send method consults it
// before calling chat.postMessage vs chat.update.
type planMessageMap struct {
	mu sync.Mutex
	m  map[string]string // key = "<ns>/<sess>::<planName>"
}

func newPlanMessageMap() *planMessageMap { return &planMessageMap{m: map[string]string{}} }

func (p *planMessageMap) key(sess channelkinds.SessionInfo, planName string) string {
	return sess.Namespace + "/" + sess.Name + "::" + planName
}

func (p *planMessageMap) get(sess channelkinds.SessionInfo, planName string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ts, ok := p.m[p.key(sess, planName)]
	return ts, ok
}

// has reports whether a ts is recorded for (sess, planName). Used by
// sendPlanUpdate to detect first-time posts up-front (before any Slack
// call) so the same decision can drive both fallback and status-restore
// logic.
func (p *planMessageMap) has(sess channelkinds.SessionInfo, planName string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.m[p.key(sess, planName)]
	return ok
}

func (p *planMessageMap) set(sess channelkinds.SessionInfo, planName, ts string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[p.key(sess, planName)] = ts
}

// renderPlanBlocks emits the Block Kit primitive rendering of the
// payload: a *PlanBlock containing one *TaskCardBlock per item,
// optionally preceded by a paused-banner *ContextBlock (when pl.Paused)
// and a sub-plan badge *ContextBlock (when ParentPlan is set), in that
// order. While paused, the in_progress item is drawn as pending (no
// "actively working" hourglass). When pl.Items is empty (plan
// cancelled), falls back to the legacy section-block stub — PlanBlock
// with zero tasks is technically valid but visually noisy.
func renderPlanBlocks(pl channelevents.PlanUpdatePayload) []slackapi.Block {
	if len(pl.Items) == 0 {
		return []slackapi.Block{
			slackapi.NewSectionBlock(
				slackapi.NewTextBlockObject("mrkdwn", "_Plan cancelled_", false, false),
				nil, nil,
			),
		}
	}

	title := pl.PlanName
	if title == "" || title == "main" {
		title = "Plan"
	}

	cards := make([]*slackapi.TaskCardBlock, 0, len(pl.Items))
	for _, it := range pl.Items {
		// While paused, the single in_progress item is drawn as a neutral
		// circle — Slack's task_card has no "paused" status, and an hourglass
		// would falsely read as "actively working". Render-only: the agent's
		// declared status in plans.Store is unchanged.
		status := it.Status
		if pl.Paused && status == "in_progress" {
			status = "pending"
		}
		tc := slackapi.NewTaskCardBlock(it.ID, it.Label).
			WithStatus(toSlackTaskStatus(status))
		if rt := plainRichText(it.Details); rt != nil {
			tc = tc.WithDetails(rt)
		}
		if rt := plainRichText(it.Output); rt != nil {
			tc = tc.WithOutput(rt)
		}
		cards = append(cards, tc)
	}
	plan := slackapi.NewPlanBlock(title).WithTasks(cards...)

	var blocks []slackapi.Block
	if pl.Paused {
		blocks = append(blocks, slackapi.NewContextBlock("",
			slackapi.NewTextBlockObject("mrkdwn", pausedBannerText(pl.PauseCause), false, false)))
	}
	if pl.ParentPlan != "" {
		blocks = append(blocks, slackapi.NewContextBlock("",
			slackapi.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("_↳ sub-plan of_ `%s` / `%s`",
					escapeSlackText(pl.ParentPlan),
					escapeSlackText(pl.ParentItem)),
				false, false)))
	}
	blocks = append(blocks, plan)
	return blocks
}

// sendPlanUpdate publishes the rendered plan to Slack as a Block Kit message.
// The dispatcher in Send routes here for env.Kind == KindPlanUpdate.
// channelID and threadTS are pre-resolved by Send before calling.
func (s *slackSender) sendPlanUpdate(
	ctx context.Context,
	sess channelkinds.SessionInfo,
	channelID, threadTS string,
	env channelevents.Envelope,
) (channelkinds.SubChannelSendResult, error) {
	var pl channelevents.PlanUpdatePayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("slack sender: decode plan_update payload: %w", err)
	}
	if err := pl.Validate(); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("slack sender: invalid plan_update payload: %w", err)
	}

	// postOrUpdate picks the right Slack call (update existing ts /
	// chat.postMessage) for the current envelope and dispatches it with the
	// supplied blocks. Returns the message ts on a fresh post (so the caller
	// can record it in planMessages for subsequent updates), "" otherwise.
	// wasPost is true iff this dispatch invoked chat.postMessage — used
	// by the caller to drive restoreStatus (only chat.postMessage auto-
	// clears assistant.threads.setStatus in an assistant thread).
	postOrUpdate := func(blocks []slackapi.Block) (newTS string, wasPost bool, err error) {
		if ts, ok := s.planMessages.get(sess, pl.PlanName); ok {
			if _, _, _, err := s.client.UpdateMessageContext(ctx, channelID, ts,
				slackapi.MsgOptionBlocks(blocks...)); err != nil {
				return "", false, fmt.Errorf("slack sender: chat.update plan: %w", err)
			}
			return "", false, nil
		}
		opts := []slackapi.MsgOption{slackapi.MsgOptionBlocks(blocks...)}
		if threadTS != "" {
			opts = append(opts, slackapi.MsgOptionTS(threadTS))
		}
		_, ts, err := s.client.PostMessageContext(ctx, channelID, opts...)
		if err != nil {
			return "", true, fmt.Errorf("slack sender: chat.postMessage plan: %w", err)
		}
		return ts, true, nil
	}

	logger := log.FromContext(ctx)

	newTS, wasPost, err := postOrUpdate(renderPlanBlocks(pl))
	if err != nil && isUnsupportedBlocksErr(err) {
		logger.Info("slack: plan/task_card blocks rejected; falling back to legacy emoji rendering",
			"session", sess.Name, "err", err.Error())
		newTS, wasPost, err = postOrUpdate(renderPlanBlocksLegacy(pl))
	}
	if err != nil {
		return channelkinds.SubChannelSendResult{}, err
	}

	if newTS != "" && !s.planMessages.has(sess, pl.PlanName) {
		s.planMessages.set(sess, pl.PlanName, newTS)
	}
	if wasPost {
		s.restoreStatus(ctx, sess)
	}
	return channelkinds.SubChannelSendResult{}, nil
}
