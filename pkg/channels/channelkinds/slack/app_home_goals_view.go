// pkg/channels/channelkinds/slack/app_home_goals_view.go
//
// Block Kit for the App Home "Group goals" section and its modals: the rows
// GoalsClient.ListForViewer returned, grouped by the channel that owns them,
// with the buttons the viewer's live permissions allow; the standing-plan
// modal behind View plan; and the confirmation modals behind Resume, Renew and
// Remove, whose submit (not the button) is the decision.
//
// Every goal-supplied string (title, schedule, blocked reason, an unparseable
// group, the plan card) is swept through inertAppHomeText before it reaches a
// mrkdwn slot: a goal's title is agent-authored, and it renders one line above
// buttons that approve standing authority.
//
// The buttons follow the Can* flags as listed; they are a rendering of the
// viewer's permissions, not a gate. Each action re-checks the clicker's live
// permission on the server (the goal server's management API), and the clicker is always
// resolved from the Slack-verified payload user — see
// app_home_goals_interaction.go.
package slack

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// appHomeGoals is the goals section's input. nil (no goals client, or no
// canonical for the viewer) and an empty Rows both render no section: group
// goals are created in a conversation, so a viewer with none has nothing to
// manage here.
type appHomeGoals struct {
	// Rows are the first appHomeGoalLimit goals the viewer can see.
	Rows []channelkinds.GoalSummary
	// Total is how many goals the viewer can see in all; Total-len(Rows) are
	// counted in an "and N more" line.
	Total int
	// LoadFailed renders a "couldn't load" notice instead of rows, which would
	// otherwise be indistinguishable from "you have no group goals".
	LoadFailed bool
}

// appHomeGoalLimit caps the rows on the Home tab. At two blocks per goal plus
// a heading per channel, ten is at most 33 blocks. The rest of the page is
// usually small, but an agent may declare any number of preferences, so the
// section is also cut to the room the page has left (goalsSectionWithin).
const appHomeGoalLimit = 10

// appHomeGoalTitleRunes caps a goal title in its row.
const appHomeGoalTitleRunes = 150

// goalPlanTextRunes caps each plan-card slot in a modal section, under Slack's
// 3000-character section text limit.
const goalPlanTextRunes = 2800

// slackGroupChannelRe matches the channel id inside the Slack kind's group
// member set ("slack_channel:<id>#member"). Only an id of this shape is
// rendered as a live <#id> channel mention; anything else renders inert.
var slackGroupChannelRe = regexp.MustCompile(`^slack_channel:([CG][A-Z0-9]{2,})#member$`)

// goalGroupLabel renders a GoalSummary.Group for a heading: a channel mention
// for this kind's own member sets, the swept raw value otherwise.
func goalGroupLabel(group string) string {
	if m := slackGroupChannelRe.FindStringSubmatch(group); m != nil {
		return "<#" + m[1] + ">"
	}
	if group == "" {
		return "an unknown conversation"
	}
	return inertAppHomeText(group).slot(appHomeGoalTitleRunes)
}

// goalsSectionBlocks renders the section, or nothing when g has nothing to
// show.
func goalsSectionBlocks(g *appHomeGoals) []slackapi.Block {
	if g == nil {
		return nil
	}
	if g.LoadFailed {
		return []slackapi.Block{
			slackapi.NewDividerBlock(),
			slackapi.NewSectionBlock(
				slackapi.NewTextBlockObject(slackapi.MarkdownType,
					":warning: couldn't load your group goals — try reopening this tab.", false, false),
				nil, nil, slackapi.SectionBlockOptionBlockID("goals_load_failed")),
		}
	}
	if len(g.Rows) == 0 && g.Total <= 0 {
		return nil
	}

	blocks := []slackapi.Block{
		slackapi.NewDividerBlock(),
		slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType,
				"*Group goals*\nScheduled goals owned by channels you're in.", false, false),
			nil, nil, slackapi.SectionBlockOptionBlockID("goals_header")),
	}

	// Group by owning channel, in the order the server listed them.
	var order []string
	byGroup := map[string][]channelkinds.GoalSummary{}
	for _, row := range g.Rows {
		if _, seen := byGroup[row.Group]; !seen {
			order = append(order, row.Group)
		}
		byGroup[row.Group] = append(byGroup[row.Group], row)
	}
	for i, group := range order {
		blocks = append(blocks, slackapi.NewContextBlock(fmt.Sprintf("goals_group:%d", i),
			slackapi.NewTextBlockObject(slackapi.MarkdownType, "*In "+goalGroupLabel(group)+"*", false, false)))
		for _, row := range byGroup[group] {
			blocks = append(blocks, goalRowBlocks(row)...)
		}
	}

	if more := g.Total - len(g.Rows); more > 0 {
		line := fmt.Sprintf("…and %d more. Ask the agent in the channel to list them all.", more)
		if len(g.Rows) == 0 {
			line = fmt.Sprintf("You can see %d group goals, but there's no room to list them here. Ask the agent in the channel to list them.", more)
		}
		blocks = append(blocks, slackapi.NewContextBlock("goals_more",
			slackapi.NewTextBlockObject(slackapi.MarkdownType, line, false, false)))
	}
	return blocks
}

// goalsSectionWithin is the goals section in at most budget blocks: whole if
// it fits, else with rows dropped from the end (each counted in the "more"
// line) until it does, else nothing. The Home tab is rejected outright over
// Slack's block limit, so a section that cannot fit must shrink rather than
// take the whole tab down with it.
func goalsSectionWithin(g *appHomeGoals, budget int) []slackapi.Block {
	full := goalsSectionBlocks(g)
	if len(full) <= budget {
		return full
	}
	if g == nil || g.LoadFailed {
		return nil
	}
	for k := len(g.Rows) - 1; k >= 0; k-- {
		cut := *g
		cut.Rows = g.Rows[:k]
		if blocks := goalsSectionBlocks(&cut); len(blocks) <= budget {
			return blocks
		}
	}
	return nil
}

// goalRowBlocks is one goal: its details and the buttons the viewer may use.
func goalRowBlocks(row channelkinds.GoalSummary) []slackapi.Block {
	title := inertAppHomeText(row.Title).slot(appHomeGoalTitleRunes)
	if title == "" {
		title = "Untitled goal"
	}
	lines := []string{"*" + title + "* · " + goalStateLabel(row.State)}

	var when []string
	if row.Schedule != "" {
		when = append(when, "Schedule: "+inertAppHomeText(row.Schedule).slot(appHomeGoalTitleRunes))
	}
	if row.NextRun != nil {
		when = append(when, "Next run: "+slackDate(*row.NextRun))
	}
	if row.ApprovalExpires != nil {
		when = append(when, "Approval ends: "+slackDate(*row.ApprovalExpires))
	}
	if len(when) > 0 {
		lines = append(lines, strings.Join(when, " · "))
	}
	if row.BlockedReason != "" {
		lines = append(lines, ":warning: Can't run: "+inertAppHomeText(row.BlockedReason).slot(appHomeGoalTitleRunes))
	}
	if row.RenewalPending {
		lines = append(lines, ":hourglass_flowing_sand: A renewal of this goal's approval is waiting for a manager.")
	}
	switch n := len(row.Managers); n {
	case 0:
	case 1:
		lines = append(lines, "1 manager")
	default:
		lines = append(lines, fmt.Sprintf("%d managers", n))
	}
	if row.NoManagerInGroup {
		lines = append(lines, ":warning: No manager is still in this channel — only an admin can resume, renew or remove it.")
	}

	blocks := []slackapi.Block{slackapi.NewSectionBlock(
		slackapi.NewTextBlockObject(slackapi.MarkdownType, strings.Join(lines, "\n"), false, false),
		nil, nil, slackapi.SectionBlockOptionBlockID("goal:"+row.Resource))}

	paused := row.State == string(goals.Paused)
	buttons := []slackapi.BlockElement{goalButton(homeGoalViewPlan, row.Resource, "View plan", "")}
	if row.CanPause && !paused {
		buttons = append(buttons, goalButton(goals.ActPause, row.Resource, "Pause", ""))
	}
	if row.CanManage && paused {
		buttons = append(buttons, goalButton(goals.ActResume, row.Resource, "Resume", slackapi.StylePrimary))
	}
	if row.CanRenew {
		buttons = append(buttons, goalButton(goals.ActRenew, row.Resource, "Renew", ""))
	}
	if row.CanManage {
		buttons = append(buttons, goalButton(goals.ActRemove, row.Resource, "Remove", slackapi.StyleDanger))
	}
	return append(blocks, slackapi.NewActionBlock("goal_actions:"+row.Resource, buttons...))
}

func goalButton(action, resource, label string, style slackapi.Style) *slackapi.ButtonBlockElement {
	btn := slackapi.NewButtonBlockElement(homeGoalActionID(action, resource), resource,
		slackapi.NewTextBlockObject(slackapi.PlainTextType, label, false, false))
	if style != "" {
		btn = btn.WithStyle(style)
	}
	return btn
}

func goalStateLabel(state string) string {
	switch state {
	case string(goals.Active):
		return "Active"
	case string(goals.Paused):
		return "Paused"
	case "":
		return "Unknown"
	default:
		return inertAppHomeText(state).slot(40)
	}
}

// slackDate renders t in the viewer's own timezone, with a UTC fallback for
// clients that cannot.
func slackDate(t time.Time) string {
	return fmt.Sprintf("<!date^%d^{date_short_pretty} at {time}|%s>", t.Unix(), t.UTC().Format("2 Jan 2006 15:04 UTC"))
}

// goalPlanBlocks renders a standing plan the way the group consent card does
// (pkg/web/goals/consent.go): plangate's whole-plan card, its trusted What and
// the agent's own Why, labelled as such.
func goalPlanBlocks(plan plangate.Plan) []slackapi.Block {
	if len(plan.Phases) == 0 {
		return []slackapi.Block{slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType, "This goal has no standing plan.", false, false), nil, nil)}
	}
	card := plangate.BuildCard(plangate.CardInput{Plan: plan, WholePlan: true})
	blocks := []slackapi.Block{slackapi.NewSectionBlock(
		slackapi.NewTextBlockObject(slackapi.MarkdownType,
			"*Standing plan*\n"+inertAppHomeText(card.What).slot(goalPlanTextRunes), false, false),
		nil, nil, slackapi.SectionBlockOptionBlockID("goal_plan_what"))}
	if card.Why != "" {
		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType,
				"*The agent's reasons* (its own words)\n"+inertAppHomeText(card.Why).slot(goalPlanTextRunes), false, false),
			nil, nil, slackapi.SectionBlockOptionBlockID("goal_plan_why")))
	}
	return blocks
}

// buildGoalPlanModal is View plan: the goal's title and plan, nothing to
// submit.
func buildGoalPlanModal(plan channelkinds.GoalPlan) slackapi.ModalViewRequest {
	blocks := append([]slackapi.Block{goalTitleBlock(plan.Title)}, goalPlanBlocks(plan.Plan)...)
	return slackapi.ModalViewRequest{
		Type:   slackapi.VTModal,
		Title:  slackapi.NewTextBlockObject(slackapi.PlainTextType, "Standing plan", false, false),
		Close:  slackapi.NewTextBlockObject(slackapi.PlainTextType, "Close", false, false),
		Blocks: slackapi.Blocks{BlockSet: blocks},
	}
}

// goalTitleBlock names the goal a modal is about, from the server's read.
// The title is agent-authored, so it is swept inert.
func goalTitleBlock(title string) slackapi.Block {
	t := inertAppHomeText(title).slot(appHomeGoalTitleRunes)
	if t == "" {
		t = "Untitled goal"
	}
	return slackapi.NewSectionBlock(
		slackapi.NewTextBlockObject(slackapi.MarkdownType, "*Goal:* "+t, false, false),
		nil, nil, slackapi.SectionBlockOptionBlockID("goal_title"))
}

// goalConfirmMetadata is a confirmation modal's private_metadata: the goal
// and, for an approval, the digest of the plan the modal shows. Who decides
// is resolved from the submitting user, and the namespace is the listener's
// own. Digest is no authority: the server refuses an approval whose plan no
// longer has it, so a forged one only gets the approval refused.
type goalConfirmMetadata struct {
	Goal   string `json:"goal"`
	Digest string `json:"digest,omitempty"`
}

// goalConfirmCopy is a confirmation modal's words per action.
var goalConfirmCopy = map[string]struct{ title, submit, body string }{
	goals.ActResume: {
		title: "Resume goal", submit: "Approve and resume",
		body: "Resuming this goal *approves its standing plan* below. Runs whose plan stays within it act as the agent and post in the channel without asking again, until the approval ends; anything beyond it still asks a manager.\n\nYou are approving as a manager of this goal.",
	},
	goals.ActRenew: {
		title: "Renew approval", submit: "Approve renewal",
		body: "Renewing *approves this goal's standing plan* below for a new approval period, on the same terms. Runs whose plan stays within it keep running without asking again; anything beyond it still asks a manager.\n\nYou are approving as a manager of this goal.",
	},
	goals.ActRemove: {
		title: "Remove goal", submit: "Remove goal",
		body: "Removing this goal cancels it for everyone in its channel. It stops running, and this cannot be undone.",
	},
}

// buildGoalConfirmModal is the confirmation behind Resume, Renew and Remove,
// from the server's read for that action: the goal's title always, and for
// the two approvals (approval true) the plan the act approves and its digest.
func buildGoalConfirmModal(action, resource string, plan channelkinds.GoalPlan, approval bool) (slackapi.ModalViewRequest, error) {
	words, ok := goalConfirmCopy[action]
	if !ok {
		return slackapi.ModalViewRequest{}, fmt.Errorf("no confirmation for goal action %q", action)
	}
	meta := goalConfirmMetadata{Goal: resource}
	if approval {
		meta.Digest = plan.Digest
	}
	md, err := json.Marshal(meta)
	if err != nil {
		return slackapi.ModalViewRequest{}, fmt.Errorf("encode confirmation metadata: %w", err)
	}
	blocks := []slackapi.Block{goalTitleBlock(plan.Title), slackapi.NewSectionBlock(
		slackapi.NewTextBlockObject(slackapi.MarkdownType, words.body, false, false), nil, nil,
		slackapi.SectionBlockOptionBlockID("goal_confirm_body"))}
	if approval {
		blocks = append(blocks, slackapi.NewDividerBlock())
		blocks = append(blocks, goalPlanBlocks(plan.Plan)...)
	}
	return slackapi.ModalViewRequest{
		Type:            slackapi.VTModal,
		CallbackID:      homeGoalConfirmCallbackPrefix + action,
		PrivateMetadata: string(md),
		Title:           slackapi.NewTextBlockObject(slackapi.PlainTextType, words.title, false, false),
		Submit:          slackapi.NewTextBlockObject(slackapi.PlainTextType, words.submit, false, false),
		Close:           slackapi.NewTextBlockObject(slackapi.PlainTextType, "Cancel", false, false),
		Blocks:          slackapi.Blocks{BlockSet: blocks},
	}, nil
}

// homeNoticeBlock renders the one-shot notice a goal action leaves at the top
// of the republished tab. Notices are this package's own sentences.
func homeNoticeBlock(notice string) slackapi.Block {
	return slackapi.NewSectionBlock(
		slackapi.NewTextBlockObject(slackapi.MarkdownType, notice, false, false),
		nil, nil, slackapi.SectionBlockOptionBlockID("home_notice"))
}
