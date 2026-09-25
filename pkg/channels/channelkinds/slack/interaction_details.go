// pkg/channels/channelkinds/slack/interaction_details.go
//
// Show-Details modal for the unified Interaction model
// (pkg/channels/channelevents/interaction.go). Whenever an
// InteractionRequestPayload carries Details (today: tool_approval's
// channelevents.ToolApprovalDetails), buildInteractionRequestBlocks
// (interaction.go) appends a "Show Details" button alongside the payload's
// decision actions; clicking it opens a modal with the full ground-truth
// fields.
//
// It is category-agnostic — the Details blob is a json.RawMessage — and is the
// sole Show-Details path, resolving cache → durable fallback → degraded.
package slack

import (
	"context"
	"encoding/json"
	"fmt"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	memapproval "github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
)

// interactionDetailsActionID is the block_action id for the Show-Details
// button appended to any InteractionRequestPayload whose Details field is
// populated. It is the sole Show-Details affordance — every decision category
// renders through the unified Interaction model.
const interactionDetailsActionID = "interaction_show_details"

// Per-slot rune caps for the modal's short fields.
//
// escapeToolApprovalDetails already reasons that overflowing Slack's
// 3000-character text-object limit "fails the whole modal with an opaque
// invalid_blocks" — but only the three PROSE slots were ever capped
// (summarizeForSlackSection at 2800). These five never were, and ResourceID is
// bound from the model's own tool-call arguments and is length-unbounded, so
// roughly 750 "<" characters — which the escape expands fourfold — was enough
// for the agent to decide, per request, whether the approver could open the
// ground-truth view at all before clicking Approve. An approver who cannot open
// Details is an approver deciding blind, which is the interesting half of the
// failure: it is a denial of the REVIEW STEP, not just a broken dialog.
//
// The budgets are sized so each block stays under the limit with every slot at
// its cap: the Resource section is 3×700 plus ~40 characters of label, and the
// footer is 4×300 plus ~60. Capping runs AFTER the escape (which is the first
// line of this renderer), never before — reversing that is exactly the mistake
// escapeToolApprovalDetails' own doc warns about.
//
// sessRef / requestRef are capped alongside StateImpact and ArgsHash even
// though they arrive from a button value this package composed: they are on the
// same block, and a cap that holds only while every OTHER contributor stays
// short is not a cap.
const (
	detailsIdentifierRunes = 700
	detailsFooterRunes     = 300
)

// renderInteractionDetailsModal builds the Block Kit body for the Show-Details
// modal from a decoded ToolApprovalDetails blob: Resource/Permission, the
// upstream MCP tool description, the agent's justification, the raw args, and a
// session/request footer — the exact set ToolApprovalDetails carries. The tool
// NAME is intentionally absent (there is no Tool field): the prompt's own
// trusted Lead line already names it.
//
// This is a WIRE BOUNDARY, like buildInteractionRequestBlocks and
// buildInteractionAppliedBlocks: d arrives verbatim from the delivery cache or
// the durable memapproval record, and every block below is mrkdwn, so it is
// made inert HERE, once, at the top — see escapeToolApprovalDetails for why the
// blob reaches no other sweep and why its slots take three different treatments
// for the three different sinks below. The modal composes no live markup of its
// own (no mentions, no links), so unlike publicNoteText there is no ordering
// constraint to respect after the sweep — only the truncation one, which is
// why the sweep runs before the summarize* calls rather than after.
//
// Every cap below is delimiter-aware, because the sweep now emits PAIRED
// backticks and the caps run after it:
//
//   - the two prose sections go through capInertRunes, which re-closes a code
//     span the cut opened (b3c01bf1's second App Home defect, arriving here
//     with the sweep);
//   - the five identifier slots are inertSpanValue'd, so they contain no
//     backtick at all and a plain truncateRunes cannot split anything;
//   - sessRef / requestRef are composed by this package but land inside the
//     same spans, and a delimiter contract that holds only while every OTHER
//     contributor behaves is not a contract — so they are made span-safe too.
func renderInteractionDetailsModal(d channelevents.ToolApprovalDetails, requestRef, sessRef string) []slackapi.Block {
	d = escapeToolApprovalDetails(d)
	requestRef, sessRef = inertSpanValue(requestRef), inertSpanValue(sessRef)

	var blocks []slackapi.Block

	if d.ResourceType != "" || d.ResourceID != "" || d.Permission != "" {
		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("*Resource*: `%s:%s` · *Permission*: `%s`",
					truncateRunes(d.ResourceType, detailsIdentifierRunes),
					truncateRunes(d.ResourceID, detailsIdentifierRunes),
					truncateRunes(d.Permission, detailsIdentifierRunes)),
				false, false),
			nil, nil,
		))
	}

	if desc := summarizeForSlackSection(d.ToolDescription, 2800); desc != "" {
		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject("mrkdwn", "*Tool description (from MCP)*\n"+desc, false, false),
			nil, nil,
		))
	}

	if just := summarizeForSlackSection(d.Justification, 2800); just != "" {
		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject("mrkdwn", "*Justification*\n"+just, false, false),
			nil, nil,
		))
	}

	// Args: deliberately NOT label-substituted, same rationale as
	// the legacy Show-Details modal's NOTE — the Show
	// Details modal is the always-available ground-truth view of what the
	// agent is asking to run; rewriting ids here would make the args text
	// disagree with the wire data the approver is gating on.
	if argsText := summarizeArgsForContext(d.ArgsJSON, 2800); argsText != "" {
		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject("mrkdwn",
				"*Args*\n```\n"+argsText+"\n```",
				false, false),
			nil, nil,
		))
	}

	footer := fmt.Sprintf("Session: `%s` · Request: `%s`",
		truncateRunes(sessRef, detailsFooterRunes), truncateRunes(requestRef, detailsFooterRunes))
	if d.StateImpact != "" {
		footer = fmt.Sprintf("State impact: `%s` · %s",
			truncateRunes(d.StateImpact, detailsFooterRunes), footer)
	}
	if d.ArgsHash != "" {
		footer += fmt.Sprintf(" · Args hash: `%s`", truncateRunes(d.ArgsHash, detailsFooterRunes))
	}
	blocks = append(blocks, slackapi.NewContextBlock("",
		slackapi.NewTextBlockObject("mrkdwn", footer, false, false),
	))

	return blocks
}

// degradedInteractionDetailsBlocks renders the "details not available" modal
// body shown when both the in-process delivery cache and the durable
// memapproval record miss. Mirrors the legacy Show-Details degraded path
// verbatim.
func degradedInteractionDetailsBlocks(requestRef, sessRef string) []slackapi.Block {
	return []slackapi.Block{
		slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject("mrkdwn",
				"_Details for this request are not available._\n"+
					"This can happen for requests made before details were persisted, or when the memory service is unreachable.",
				false, false),
			nil, nil,
		),
		slackapi.NewContextBlock("",
			slackapi.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("Request: `%s` · Session: `%s`",
					truncateRunes(requestRef, detailsFooterRunes),
					truncateRunes(sessRef, detailsFooterRunes)),
				false, false),
		),
	}
}

// handleInteractionDetailsAction recognizes a generic Show-Details
// block_action (discriminator: BlockActions[*].Value JSON's
// v:"interaction_details") and opens a Slack modal via views.open showing
// the request's Details. Returns (true, nil) on a recognised click — even
// when the details lookup fails on both legs (we still hand back true so
// the caller doesn't fall through to another handler); the lookup failure
// is logged with full context, never silently dropped.
//
// Resolution order (mirrors the legacy per-kind Show-Details handler):
//  1. l.interactionDelivery — the process-wide cache
//     interactionSender.sendRequest populates at prompt-delivery time
//     (interaction_delivery.go). Hit on the common path: the request is
//     still pending and this process rendered it.
//  2. memapproval.RequestByID — the durable approval record the runner
//     wrote at request time. Falls back here on a channelsd restart between
//     request and click, or when the request already resolved and its
//     delivery entry was dropped.
//  3. Degraded modal — both missed. Explains why, rather than silently
//     dropping the click.
func (l *slackListener) handleInteractionDetailsAction(ctx context.Context, cb slackapi.InteractionCallback) (bool, error) {
	if len(cb.ActionCallback.BlockActions) == 0 {
		return false, nil
	}
	logger := log.FromContext(ctx)
	for _, a := range cb.ActionCallback.BlockActions {
		if a.ActionID != interactionDetailsActionID {
			continue
		}
		v, ok := decodeApprovalButtonValue(a.Value)
		if !ok || v.V != discInteractionDetails {
			// Not an interaction_details click after all (malformed value, or
			// some other action_id stub) — fall through.
			continue
		}

		var modalBlocks []slackapi.Block
		source := "degraded" // "cache" | "memory" | "degraded" — for the modal-opened log
		if l.interactionDelivery != nil {
			if raw, ok := l.interactionDelivery.getDetails(v.R); ok {
				var d channelevents.ToolApprovalDetails
				if err := json.Unmarshal(raw, &d); err != nil {
					logger.Info("interaction_details: decode cached details failed",
						"requestRef", v.R, "err", err.Error())
				} else {
					modalBlocks = renderInteractionDetailsModal(d, v.R, v.S)
					source = "cache"
				}
			}
		}
		if modalBlocks == nil {
			// Cache miss (channelsd restarted, or the request resolved and its
			// delivery entry was dropped). Fall back to the durable approval
			// record the runner wrote at request time.
			if d, ok := l.interactionDetailsFromMemory(ctx, v.R, v.S); ok {
				modalBlocks = renderInteractionDetailsModal(d, v.R, v.S)
				source = "memory"
			}
		}
		if modalBlocks == nil {
			logger.Info("interaction_details: no cached or persisted details; opening degraded modal",
				"requestRef", v.R, "sessRef", v.S, "clicker", cb.User.ID)
			modalBlocks = degradedInteractionDetailsBlocks(v.R, v.S)
		}

		view := slackapi.ModalViewRequest{
			Type:   slackapi.VTModal,
			Title:  slackapi.NewTextBlockObject("plain_text", "Request Details", true, false),
			Close:  slackapi.NewTextBlockObject("plain_text", "Close", true, false),
			Blocks: slackapi.Blocks{BlockSet: modalBlocks},
		}
		if _, err := l.api.OpenViewContext(ctx, cb.TriggerID, view); err != nil {
			logger.Info("interaction_details: views.open failed",
				"requestRef", v.R, "triggerID", cb.TriggerID, "err", err.Error())
			return true, fmt.Errorf("interaction_details: views.open: %w", err)
		}
		logger.Info("interaction_details: modal opened",
			"requestRef", v.R, "clicker", cb.User.ID, "source", source)
		return true, nil
	}
	return false, nil
}

// interactionDetailsFromMemory retrieves and decodes the persisted Details
// blob for requestID (written by the runner at request time; see
// pkg/memory/kinds/approval's Request.Details). Returns (zero, false) when
// memory is not wired, the record is absent, it carries no Details (a category
// that publishes none), or the query/decode fails — every failure path is
// logged, never silently dropped.
func (l *slackListener) interactionDetailsFromMemory(ctx context.Context, requestID, sessRef string) (channelevents.ToolApprovalDetails, bool) {
	if l.deps.Memory == nil {
		return channelevents.ToolApprovalDetails{}, false
	}
	rec, err := memapproval.RequestByID(ctx, l.deps.Memory,
		memory.Scope{Kind: "session", ID: sessRef}, requestID)
	if err != nil {
		log.FromContext(ctx).Info("interaction_details: memory lookup failed",
			"requestID", requestID, "sessRef", sessRef, "err", err.Error())
		return channelevents.ToolApprovalDetails{}, false
	}
	if rec == nil || len(rec.Details) == 0 {
		return channelevents.ToolApprovalDetails{}, false
	}
	var d channelevents.ToolApprovalDetails
	if err := json.Unmarshal(rec.Details, &d); err != nil {
		log.FromContext(ctx).Info("interaction_details: decode persisted details failed",
			"requestID", requestID, "sessRef", sessRef, "err", err.Error())
		return channelevents.ToolApprovalDetails{}, false
	}
	return d, true
}
