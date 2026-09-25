// pkg/channels/channelkinds/slack/sender_plan_legacy.go
//
// Legacy emoji-section renderer for KindPlanUpdate. Retained as the
// fallback target when a workspace rejects the native PlanBlock /
// TaskCardBlock primitives produced by renderPlanBlocks. No production
// caller invokes it directly outside that fallback path.
package slack

import (
	"fmt"

	slackapi "github.com/slack-go/slack"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// renderPlanBlocksLegacy returns the pre-Block-Kit emoji-section
// rendering for the payload. Preserved verbatim from the original
// implementation prior to the PlanBlock/TaskCardBlock migration.
func renderPlanBlocksLegacy(pl channelevents.PlanUpdatePayload) []slackapi.Block {
	if len(pl.Items) == 0 {
		return []slackapi.Block{
			slackapi.NewSectionBlock(
				slackapi.NewTextBlockObject("mrkdwn", "_Plan cancelled_", false, false),
				nil, nil,
			),
		}
	}

	var blocks []slackapi.Block

	if pl.Paused {
		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject("mrkdwn", pausedBannerText(pl.PauseCause), false, false),
			nil, nil,
		))
	}

	if pl.PlanName != "main" && pl.PlanName != "" {
		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("*%s*", escapeSlackText(pl.PlanName)), false, false),
			nil, nil,
		))
	}
	if pl.ParentPlan != "" {
		blocks = append(blocks, slackapi.NewContextBlock("",
			slackapi.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("_↳ sub-plan of_ `%s` / `%s`",
					escapeSlackText(pl.ParentPlan),
					escapeSlackText(pl.ParentItem)),
				false, false),
		))
	}

	allDone := true
	for _, it := range pl.Items {
		if it.Status != "done" {
			allDone = false
		}
		eff := it
		if pl.Paused && eff.Status == "in_progress" {
			eff.Status = "pending"
		}
		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject("mrkdwn", planItemMrkdwnLegacy(eff), false, false),
			nil, nil,
		))
	}
	if allDone {
		blocks = append(blocks, slackapi.NewContextBlock("",
			slackapi.NewTextBlockObject("mrkdwn", "_Completed_", false, false),
		))
	}
	return blocks
}

// planItemMrkdwnLegacy emits the emoji-prefixed line for one item.
// "error" status renders as a red-cross emoji with strikethrough,
// matching the visual treatment of a done item but distinguishable
// at a glance.
func planItemMrkdwnLegacy(it channelevents.PlanItemRef) string {
	emoji := ":white_circle:"
	label := escapeSlackText(it.Label)
	switch it.Status {
	case "in_progress":
		emoji = ":hourglass_flowing_sand:"
	case "done":
		emoji = ":white_check_mark:"
		label = "~" + label + "~"
	case "error":
		emoji = ":x:"
		label = "~" + label + "~"
	}
	return emoji + " " + label
}
