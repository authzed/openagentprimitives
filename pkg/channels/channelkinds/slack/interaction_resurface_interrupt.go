// pkg/channels/channelkinds/slack/interaction_resurface_interrupt.go
//
// appendInteractionResurfaceInterrupt welds an "Interrupt & Send Now" button
// onto a RE-SURFACED interaction prompt — one channelsd republished because the
// user has since interacted from another device. tool_call and info_leakage
// approvals are published Interruptible (pkg/agent/runner/host_approval.go's
// buildToolCallPending / buildLeakagePending), so their resurfaced prompts
// render through interactionSender.sendRequest and pick the button up here.
//
// It needs NO routing of its own. The button carries the same
// discInteraction codec (encodeInteractionButtonValue) and the same category
// (categories.QueuedMessages) as the enqueue-ack's own interrupt button, so a
// click round-trips the existing path unchanged: onInteraction →
// handleInteractionDecisionClick → interaction_decision → channelsd's decision
// pipe, which re-checks the category's DeciderPolicy (QueuedMessages =
// DecideParticipant, i.e. CheckInteract — fail-closed) → decideQueuedInterrupt
// (pkg/channels/channelsd/pipeline/queued_interrupt.go). That handler
// republishes a KindInterruptRequest at the runner and returns
// Outcome{Suppressed:true} so the pipe skips the synchronous
// interaction_applied; the runner's own KindInterruptApplied resolves the card
// via internal/cmd/channelsd/interrupt_applied_bridge.go.
package slack

import (
	slackapi "github.com/slack-go/slack"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// interactionResurfaceInterruptActionID is both the Slack block action_id and
// the button value's "d" (actionId) field for the resurface-interrupt
// button. Matches the "interrupt" id the enqueue-ack's own interrupt button
// (pipeline.go) already uses for the same category — decideQueuedInterrupt
// does not branch on it (QueuedMessages carries exactly one decision action
// today), but keeping it identical documents the wire shape and lets both
// buttons be recognized as "the same affordance" at a glance.
const interactionResurfaceInterruptActionID = "interrupt"

// appendInteractionResurfaceInterrupt appends an "Interrupt & Send Now"
// action block to an already-rendered generic interaction prompt when the
// envelope was published as a re-surface of an interruptible request
// (env.ResurfaceInterruptRequestID set — stamped by channelsd's
// republishPrompt, pkg/channels/channelsd/pipeline/resurface.go, whenever the cached
// prompt's Interruptible flag is true). Otherwise it returns blocks
// unchanged. sessRef is "namespace/name".
//
// The button goes in its OWN action block, not into the prompt's, because the
// caller appends this AFTER buildInteractionRequestBlocks has already emitted
// its "interaction_actions" block.
func appendInteractionResurfaceInterrupt(blocks []slackapi.Block, env channelevents.Envelope, sessRef string) []slackapi.Block {
	if env.ResurfaceInterruptRequestID == "" {
		return blocks
	}
	return append(blocks, slackapi.NewActionBlock("interaction_resurface_interrupt_actions",
		slackapi.NewButtonBlockElement(
			interactionResurfaceInterruptActionID,
			encodeInteractionButtonValue(env.ResurfaceInterruptRequestID, interactionResurfaceInterruptActionID, categories.QueuedMessages, sessRef),
			slackapi.NewTextBlockObject(slackapi.PlainTextType, "Interrupt & Send Now", true, false),
		),
	))
}
