// pkg/channels/channelkinds/slack/app_home_goals_interaction.go
//
// The App Home goals section's clicks, following handleHomePrefClick:
//
//   - "home:goal:view_plan:<goal>" opens the standing plan in a modal.
//   - "home:goal:pause:<goal>" pauses at once (reversible), then republishes.
//   - "home:goal:resume|renew|remove:<goal>" opens a confirmation modal. Resume
//     and renew are approvals, so the modal shows the standing plan they
//     approve; remove is destructive. Nothing is decided on the click.
//   - the modal's view_submission (callback "home:goal:confirm:<action>") is
//     the decision: GoalsClient.Act, then republish. An approval's modal is
//     read for its action (GoalsClient.StandingPlan), so it shows what that
//     act approves, and carries the read's digest to Act, which refuses the
//     approval if the plan changed since.
//
// THE SECURITY PROPERTY: the viewer handed to GoalsClient is always
// clickerCanonical(cb.User.ID) — the user Slack itself names on the signed
// payload, resolved the way the preference clicks resolve it. The action_id
// carries only the action and the goal; private_metadata carries only the
// goal and the plan digest (goalConfirmMetadata), and any other field there
// is ignored; the
// namespace is the listener's own bound Channel's. The view's buttons follow
// the viewer's permissions as listed, but authority is the server's: every
// action re-checks the clicker's live permission in the goal server's management API.
//
// Failures are logged with user, goal and action, and surfaced where the user
// is waiting: on the republished Home tab, as a notice at the top. App Home
// block_actions and view_submissions carry neither a response_url nor a
// channel, so a notice on the tab is the ephemeral this surface has. Nothing is
// ever posted into the goal's channel.
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

const (
	// homeGoalActionPrefix starts every goal button's action_id:
	// "home:goal:<action>:<goal resource>".
	homeGoalActionPrefix = "home:goal:"
	// homeGoalConfirmCallbackPrefix starts a confirmation modal's callback_id:
	// "home:goal:confirm:<action>".
	homeGoalConfirmCallbackPrefix = "home:goal:confirm:"
	// homeGoalViewPlan is the View plan button's action. It is not a
	// GoalsClient action: it only reads.
	homeGoalViewPlan = "view_plan"

	goalResourcePrefix = "agent_goal:"
)

// homeGoalActionID is a goal button's action_id. A resource names one goal,
// and each goal is one row, so the id is unique within the view.
func homeGoalActionID(action, resource string) string {
	return homeGoalActionPrefix + action + ":" + resource
}

// validGoalResource accepts "agent_goal:<id>" with a non-empty id.
func validGoalResource(resource string) bool {
	id, ok := strings.CutPrefix(resource, goalResourcePrefix)
	return ok && id != "" && !strings.ContainsAny(id, ":#")
}

// handleHomeGoalClick handles a goal button on the Home tab, returning
// (false, nil) for anything else so onInteraction falls through.
func (l *slackListener) handleHomeGoalClick(ctx context.Context, cb slackapi.InteractionCallback) (bool, error) {
	if cb.Type != slackapi.InteractionTypeBlockActions || len(cb.ActionCallback.BlockActions) == 0 {
		return false, nil
	}
	rest, ok := strings.CutPrefix(cb.ActionCallback.BlockActions[0].ActionID, homeGoalActionPrefix)
	if !ok {
		return false, nil
	}
	action, resource, _ := strings.Cut(rest, ":")
	if !validGoalResource(resource) {
		return true, fmt.Errorf("home goal click: malformed goal in action_id %q", cb.ActionCallback.BlockActions[0].ActionID)
	}
	logger := log.FromContext(ctx).WithValues("user", cb.User.ID, "goal", resource, "action", action)

	viewer, ns, err := l.goalClickContext(ctx, cb.User.ID)
	if err != nil {
		return true, fmt.Errorf("home goal click %s %s by %s: %w", action, resource, cb.User.ID, err)
	}

	switch action {
	case homeGoalViewPlan:
		plan, err := l.goals.StandingPlan(ctx, viewer, ns, resource, "")
		if err != nil {
			logger.Info("app_home: goal StandingPlan failed", "namespace", ns, "err", err.Error())
			l.republishHomeTabWithNotice(ctx, cb.User.ID, goalPlanNotice(err))
			return true, nil
		}
		l.openGoalModal(ctx, cb, logger, buildGoalPlanModal(plan))
		return true, nil

	case goals.ActPause:
		l.actOnGoal(ctx, cb.User.ID, viewer, ns, resource, action, "")
		return true, nil

	case goals.ActResume, goals.ActRenew, goals.ActRemove:
		// Read for the action: for an approval, the plan that act approves
		// (and the digest binding the act to it); for every action, the
		// title, from the server rather than the tab the click came from.
		plan, err := l.goals.StandingPlan(ctx, viewer, ns, resource, action)
		if err != nil {
			logger.Info("app_home: goal StandingPlan for confirmation failed", "namespace", ns, "err", err.Error())
			l.republishHomeTabWithNotice(ctx, cb.User.ID, goalPlanNotice(err))
			return true, nil
		}
		approval := action != goals.ActRemove
		if approval && len(plan.Plan.Phases) == 0 {
			logger.Info("app_home: goal has no standing plan to approve", "namespace", ns)
			l.republishHomeTabWithNotice(ctx, cb.User.ID,
				":warning: This goal has no standing plan to approve. Ask the agent in its channel to request one.")
			return true, nil
		}
		modal, err := buildGoalConfirmModal(action, resource, plan, approval)
		if err != nil {
			return true, fmt.Errorf("home goal click: %w", err)
		}
		l.openGoalModal(ctx, cb, logger, modal)
		return true, nil

	default:
		return true, fmt.Errorf("home goal click: unknown action %q on %s", action, resource)
	}
}

// handleHomeGoalSubmit handles a confirmation modal's submit — the decision
// for resume, renew and remove — returning (false, nil) for any other
// view_submission.
func (l *slackListener) handleHomeGoalSubmit(ctx context.Context, cb slackapi.InteractionCallback) (bool, error) {
	if cb.Type != slackapi.InteractionTypeViewSubmission {
		return false, nil
	}
	action, ok := strings.CutPrefix(cb.View.CallbackID, homeGoalConfirmCallbackPrefix)
	if !ok {
		return false, nil
	}
	if _, confirmable := goalConfirmCopy[action]; !confirmable {
		return true, fmt.Errorf("home goal submit: %q is not a confirmed goal action", action)
	}
	// Only the goal and the plan digest are read from the metadata. A
	// viewer, namespace or any other field a client put there is ignored.
	// The digest is no authority — a forged one only gets the approval
	// refused — it binds the decision to the plan this modal showed.
	var md goalConfirmMetadata
	if err := json.Unmarshal([]byte(cb.View.PrivateMetadata), &md); err != nil {
		return true, fmt.Errorf("home goal submit %s by %s: private_metadata: %w", action, cb.User.ID, err)
	}
	if !validGoalResource(md.Goal) {
		return true, fmt.Errorf("home goal submit %s by %s: malformed goal %q", action, cb.User.ID, md.Goal)
	}
	viewer, ns, err := l.goalClickContext(ctx, cb.User.ID)
	if err != nil {
		return true, fmt.Errorf("home goal submit %s %s by %s: %w", action, md.Goal, cb.User.ID, err)
	}
	l.actOnGoal(ctx, cb.User.ID, viewer, ns, md.Goal, action, md.Digest)
	return true, nil
}

// goalClickContext resolves what every goal action needs from the server side
// alone: the clicker's canonical and the listener's namespace. On failure the
// user is told on the tab; the caller returns the error for onInteraction to
// log.
func (l *slackListener) goalClickContext(ctx context.Context, slackUserID string) (identity.CanonicalUserID, string, error) {
	if l.goals == nil {
		l.republishHomeTabWithNotice(ctx, slackUserID, ":warning: Group goals aren't available right now.")
		return identity.CanonicalUserID{}, "", errors.New("no goals client configured")
	}
	if l.deps.Channel == nil || l.deps.Channel.Namespace == "" {
		l.republishHomeTabWithNotice(ctx, slackUserID, ":warning: Group goals aren't available right now.")
		return identity.CanonicalUserID{}, "", errors.New("listener has no bound Channel")
	}
	viewer, err := l.clickerCanonical(ctx, slackUserID)
	if err != nil {
		l.republishHomeTabWithNotice(ctx, slackUserID, ":warning: Couldn't verify your identity — please try again.")
		return identity.CanonicalUserID{}, "", err
	}
	return viewer, l.deps.Channel.Namespace, nil
}

// actOnGoal takes action as viewer, logs the outcome with its context, and
// republishes the tab with a notice saying what happened.
func (l *slackListener) actOnGoal(ctx context.Context, slackUserID string, viewer identity.CanonicalUserID, ns, resource, action, planDigest string) {
	logger := log.FromContext(ctx).WithValues("user", slackUserID, "goal", resource, "action", action, "namespace", ns)
	if err := l.goals.Act(ctx, viewer, ns, resource, action, planDigest); err != nil {
		logger.Info("app_home: goal Act failed", "err", err.Error())
		l.republishHomeTabWithNotice(ctx, slackUserID, goalActFailureNotice(action, err))
		return
	}
	logger.Info("app_home: goal action taken")
	l.republishHomeTabWithNotice(ctx, slackUserID, goalActSuccessNotice(action))
}

// openGoalModal opens a goal modal from a button click. A failure (an expired
// trigger, a rejected view) is logged and told on the tab.
func (l *slackListener) openGoalModal(ctx context.Context, cb slackapi.InteractionCallback, logger logr.Logger, modal slackapi.ModalViewRequest) {
	if _, err := l.api.OpenViewContext(ctx, cb.TriggerID, modal); err != nil {
		logger.Info("app_home: goal views.open failed", "err", err.Error())
		l.republishHomeTabWithNotice(ctx, cb.User.ID, ":warning: Couldn't open that — please try again.")
	}
}

// goalVerb is how a notice names an action.
var goalVerb = map[string]string{
	goals.ActPause:  "pause",
	goals.ActResume: "resume",
	goals.ActRenew:  "renew",
	goals.ActRemove: "remove",
}

func goalActFailureNotice(action string, err error) string {
	verb := goalVerb[action]
	switch {
	case errors.Is(err, goals.ErrPlanChanged):
		return fmt.Sprintf(":warning: This goal's plan changed since you opened it, so nothing was approved. Click %s again to see the plan you'd approve.", strings.ToUpper(verb[:1])+verb[1:])
	case errors.Is(err, goals.ErrDenied) && action == goals.ActPause:
		return ":warning: Only a member of this goal's channel can pause it."
	case errors.Is(err, goals.ErrDenied):
		return fmt.Sprintf(":warning: Only a manager of this goal can %s it.", verb)
	case errors.Is(err, goals.ErrNotFound):
		return ":warning: That goal no longer exists, or you can no longer see it."
	default:
		return fmt.Sprintf(":warning: Couldn't %s this goal — please try again. If it keeps failing, check with your workspace admin.", verb)
	}
}

func goalActSuccessNotice(action string) string {
	switch action {
	case goals.ActResume:
		return ":white_check_mark: Goal resumed. You approved its standing plan."
	case goals.ActRenew:
		return ":white_check_mark: Approval renewed. You approved its standing plan."
	case goals.ActRemove:
		return ":white_check_mark: Goal removed."
	default:
		return ""
	}
}

func goalPlanNotice(err error) string {
	switch {
	case errors.Is(err, goals.ErrDenied):
		return ":warning: You can't see this goal's plan."
	case errors.Is(err, goals.ErrNotFound):
		return ":warning: That goal no longer exists, or you can no longer see it."
	default:
		return ":warning: Couldn't load this goal's standing plan — please try again."
	}
}
