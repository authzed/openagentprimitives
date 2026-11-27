// pkg/channels/channelkinds/slack/app_home_goals_test.go
//
// Tests for the App Home goals section: the pure view builder (rows grouped by
// group, the row cap, which buttons a viewer sees) and the interaction handlers
// (View plan, Pause, and the Resume / Renew / Remove confirmation modals whose
// submit is the decision). The security property pinned here is the same one
// the preference clicks hold: the viewer handed to GoalsClient is always the
// canonical resolved from the Slack-verified clicker, never anything the view
// carries.
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// --- fakes and fixtures ---------------------------------------------------

type goalsListCall struct {
	viewer    identity.CanonicalUserID
	ns, class string
	limit     int
}

type goalsPlanCall struct {
	viewer               identity.CanonicalUserID
	ns, resource, action string
}

type goalsActCall struct {
	viewer                       identity.CanonicalUserID
	ns, resource, action, digest string
}

// fakeGoalsClient is a recording channelkinds.GoalsClient.
type fakeGoalsClient struct {
	mu      sync.Mutex
	rows    []channelkinds.GoalSummary
	total   int
	listErr error
	plan    plangate.Plan
	title   string
	digest  string
	planErr error
	actErr  error

	lists []goalsListCall
	plans []goalsPlanCall
	acts  []goalsActCall
}

func (f *fakeGoalsClient) ListForViewer(_ context.Context, viewer identity.CanonicalUserID, ns, class string, limit int) ([]channelkinds.GoalSummary, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists = append(f.lists, goalsListCall{viewer: viewer, ns: ns, class: class, limit: limit})
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	rows := f.rows
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, f.total, nil
}

func (f *fakeGoalsClient) StandingPlan(_ context.Context, viewer identity.CanonicalUserID, ns, resource, action string) (channelkinds.GoalPlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plans = append(f.plans, goalsPlanCall{viewer: viewer, ns: ns, resource: resource, action: action})
	return channelkinds.GoalPlan{Title: f.title, Plan: f.plan, Digest: f.digest}, f.planErr
}

func (f *fakeGoalsClient) Act(_ context.Context, viewer identity.CanonicalUserID, ns, resource, action, digest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acts = append(f.acts, goalsActCall{viewer: viewer, ns: ns, resource: resource, action: action, digest: digest})
	return f.actErr
}

// recordingGoalsAPI records both views.publish (the Home tab) and views.open
// (the plan and confirmation modals).
type recordingGoalsAPI struct {
	*recordingHomeClient
	opened  []slackapi.ModalViewRequest
	openErr error
}

func (c *recordingGoalsAPI) OpenViewContext(_ context.Context, _ string, view slackapi.ModalViewRequest) (*slackapi.ViewResponse, error) {
	c.opened = append(c.opened, view)
	if c.openErr != nil {
		return nil, c.openErr
	}
	return &slackapi.ViewResponse{}, nil
}

const (
	goalsFixtureNS    = "demo"
	goalsFixtureClass = "demo-agent"
	goalsFixtureUser  = "U_VIEWER"
	goalA             = "agent_goal:g-a"
)

func standingPlanFixture(t *testing.T) plangate.Plan {
	t.Helper()
	h, err := permsurface.NewPermHandle("read", "github_repo")
	require.NoError(t, err, "fixture handle")
	return plangate.Plan{Phases: []plangate.Phase{{
		Why:         "triage new issues",
		Permissions: []permsurface.Handle{h},
		Max:         plangate.MaxSpec{Count: 1},
		Slots:       []plangate.Slot{{Type: "github_repo", ID: "acme/widgets"}},
	}}}
}

// newGoalsListener wires a listener bound to demo/demo-agent, with the clicker
// in the identity cache (so canonical resolution takes the trusted path), a
// fake cluster holding the Channel and AgentClass (so the republished tab is a
// real page, not the empty state), and the recording goals client. goalsClient
// may be nil to model a channelsd with no goals client.
func newGoalsListener(t *testing.T, gc channelkinds.GoalsClient) (*slackListener, *recordingGoalsAPI, identity.CanonicalUserID) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: goalsFixtureClass, Namespace: goalsFixtureNS},
		Spec:       spiceboxv1alpha1.AgentClassSpec{DisplayName: "Demo Agent"},
	}
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-slack", Namespace: goalsFixtureNS},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "slack", AgentClass: goalsFixtureClass},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ac, ch).Build()

	api := &recordingGoalsAPI{recordingHomeClient: &recordingHomeClient{Client: fakeslack.New()}}
	deps := channelkinds.Deps{K8sClient: cli, Channel: ch}
	if gc != nil {
		deps.Goals = gc
	}
	l := &slackListener{
		api:             api,
		idents:          NewIdentityCache(8),
		installedTeamID: "T_HOME",
		deps:            deps,
		goals:           deps.Goals,
	}
	l.idents.Put(userInfo{UserID: goalsFixtureUser, Email: "viewer@example.com", TeamID: "T_HOME"})
	canonical, err := l.resolveCanonicalForSlackUser(context.Background(), goalsFixtureUser)
	require.NoError(t, err, "fixture canonical")
	cid, err := identity.Subject(canonical).CanonicalUserID()
	require.NoError(t, err, "fixture bare canonical")
	return l, api, cid
}

func goalButtonClick(action, resource string) slackapi.InteractionCallback {
	return slackapi.InteractionCallback{
		Type:      slackapi.InteractionTypeBlockActions,
		User:      slackapi.User{ID: goalsFixtureUser},
		TriggerID: "trig-1",
		ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{{
			ActionID: homeGoalActionID(action, resource),
			Value:    resource,
		}}},
	}
}

func goalSubmit(t *testing.T, modal slackapi.ModalViewRequest) slackapi.InteractionCallback {
	t.Helper()
	return slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeViewSubmission,
		User: slackapi.User{ID: goalsFixtureUser},
		View: slackapi.View{CallbackID: modal.CallbackID, PrivateMetadata: modal.PrivateMetadata},
	}
}

func modalText(v slackapi.ModalViewRequest) string {
	return appHomeText(slackapi.HomeTabViewRequest{Blocks: v.Blocks})
}

// actionIDs lists every button action_id in a block set.
func actionIDs(blocks []slackapi.Block) []string {
	var out []string
	for _, b := range blocks {
		ab, ok := b.(*slackapi.ActionBlock)
		if !ok {
			continue
		}
		for _, e := range ab.Elements.ElementSet {
			if btn, ok := e.(*slackapi.ButtonBlockElement); ok {
				out = append(out, btn.ActionID)
			}
		}
	}
	return out
}

func captureLogs(t *testing.T) (context.Context, func() string) {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	lg := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, prefix+" "+args)
	}, funcr.Options{})
	return log.IntoContext(context.Background(), lg), func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(lines, "\n")
	}
}

func goalsPage(rows []channelkinds.GoalSummary, total int) []slackapi.Block {
	return buildAppHomeView(appHomeViewInput{Agent: &appHomeAgent{
		Name: goalsFixtureClass, DisplayName: "Demo Agent",
		Goals: &appHomeGoals{Rows: rows, Total: total},
	}}).Blocks.BlockSet
}

// --- view builder ---------------------------------------------------------

func TestAppHomeGoals_RowsGroupedByGroupAndCapped(t *testing.T) {
	var rows []channelkinds.GoalSummary
	for i := range appHomeGoalLimit {
		group := "slack_channel:CAAA#member"
		if i%2 == 1 {
			group = "slack_channel:CBBB#member"
		}
		rows = append(rows, channelkinds.GoalSummary{
			Resource: fmt.Sprintf("agent_goal:g%d", i), Title: fmt.Sprintf("Goal %d", i),
			Group: group, State: string(goals.Active),
		})
	}
	text := appHomeText(slackapi.HomeTabViewRequest{Blocks: slackapi.Blocks{BlockSet: goalsPage(rows, 13)}})

	assert.Equal(t, 1, strings.Count(text, "<#CAAA>"), "each group is a heading once, not repeated per row")
	assert.Equal(t, 1, strings.Count(text, "<#CBBB>"), "each group is a heading once, not repeated per row")
	// Every CAAA goal comes before the CBBB heading: rows are grouped, not interleaved.
	bHeading := strings.Index(text, "<#CBBB>")
	for i := 0; i < appHomeGoalLimit; i += 2 {
		assert.Less(t, strings.Index(text, fmt.Sprintf("Goal %d*", i)), bHeading, "goal %d is under its own group", i)
	}
	assert.Contains(t, text, "and 3 more", "the rows beyond the cap are counted")
}

func TestAppHomeGoals_NoMoreLineWhenAllShown(t *testing.T) {
	rows := []channelkinds.GoalSummary{{Resource: goalA, Title: "Weekly triage", Group: "slack_channel:CAAA#member", State: string(goals.Active)}}
	text := appHomeText(slackapi.HomeTabViewRequest{Blocks: slackapi.Blocks{BlockSet: goalsPage(rows, 1)}})
	assert.Contains(t, text, "Weekly triage")
	assert.NotContains(t, text, "more", "no overflow line when every goal is shown")
}

func TestAppHomeGoals_ButtonsFollowPermissions(t *testing.T) {
	cases := []struct {
		name string
		row  channelkinds.GoalSummary
		want []string
	}{
		{
			name: "member of an active goal: View plan and Pause only",
			row:  channelkinds.GoalSummary{State: string(goals.Active), CanPause: true},
			want: []string{homeGoalViewPlan, goals.ActPause},
		},
		{
			name: "member of a paused goal: View plan only (no Pause on a paused goal, no Resume without manage)",
			row:  channelkinds.GoalSummary{State: string(goals.Paused), CanPause: true},
			want: []string{homeGoalViewPlan},
		},
		{
			name: "manager of an active goal: View plan, Pause, Remove",
			row:  channelkinds.GoalSummary{State: string(goals.Active), CanPause: true, CanManage: true},
			want: []string{homeGoalViewPlan, goals.ActPause, goals.ActRemove},
		},
		{
			name: "manager of a paused goal that can be resumed: View plan, Resume, Remove",
			row:  channelkinds.GoalSummary{State: string(goals.Paused), CanPause: true, CanManage: true, CanResume: true},
			want: []string{homeGoalViewPlan, goals.ActResume, goals.ActRemove},
		},
		{
			name: "manager of a paused goal with nothing left to resume: View plan, Remove (no Resume)",
			row:  channelkinds.GoalSummary{State: string(goals.Paused), CanPause: true, CanManage: true},
			want: []string{homeGoalViewPlan, goals.ActRemove},
		},
		{
			name: "manager who can renew: Renew shown",
			row:  channelkinds.GoalSummary{State: string(goals.Active), CanPause: true, CanManage: true, CanRenew: true},
			want: []string{homeGoalViewPlan, goals.ActPause, goals.ActRenew, goals.ActRemove},
		},
		{
			name: "viewer with no permissions: View plan only",
			row:  channelkinds.GoalSummary{State: string(goals.Active)},
			want: []string{homeGoalViewPlan},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.row.Resource, tc.row.Title, tc.row.Group = goalA, "Weekly triage", "slack_channel:CAAA#member"
			var got []string
			for _, id := range actionIDs(goalsPage([]channelkinds.GoalSummary{tc.row}, 1)) {
				if rest, ok := strings.CutPrefix(id, homeGoalActionPrefix); ok {
					action, resource, _ := strings.Cut(rest, ":")
					assert.Equal(t, goalA, resource, "every button names its goal")
					got = append(got, action)
				}
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestAppHomeGoals_FlagsAndDetailsRendered(t *testing.T) {
	next := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	expires := time.Date(2027, 1, 20, 9, 0, 0, 0, time.UTC)
	rows := []channelkinds.GoalSummary{
		{
			Resource: goalA, Title: "Weekly triage", Group: "slack_channel:CAAA#member", State: string(goals.Active),
			Schedule: "every Monday", NextRun: &next, ApprovalExpires: &expires,
			Managers: []string{"m1", "m2"}, NoManagerInGroup: true, RenewalPending: true,
		},
		{
			Resource: "agent_goal:g-b", Title: "Stalled", Group: "slack_channel:CAAA#member", State: string(goals.Active),
			BlockedReason: goals.BlockedApprovalExpired,
		},
	}
	text := appHomeText(slackapi.HomeTabViewRequest{Blocks: slackapi.Blocks{BlockSet: goalsPage(rows, 2)}})
	for _, want := range []string{
		"every Monday",
		fmt.Sprint(next.Unix()),
		fmt.Sprint(expires.Unix()),
		"No manager is still in this channel",
		"renewal",
		"2 managers",
		"Can't run: " + goals.BlockedApprovalExpired,
	} {
		assert.Contains(t, text, want)
	}
}

func TestAppHomeGoals_GoalTextIsInert(t *testing.T) {
	lure := "<https://attacker.example/connect|Connect your account>"
	rows := []channelkinds.GoalSummary{{
		Resource: goalA, Title: lure, Group: "slack_channel:<https://x.example|C>#member",
		State: string(goals.Active), Schedule: lure, BlockedReason: lure,
	}}
	text := appHomeText(slackapi.HomeTabViewRequest{Blocks: slackapi.Blocks{BlockSet: goalsPage(rows, 1)}})
	assert.NotContains(t, text, "<https://", "no goal-supplied text may render as a live link")
}

func TestAppHomeGoals_LoadFailedAndNotice(t *testing.T) {
	blocks := buildAppHomeView(appHomeViewInput{
		Notice: "Only a manager of this goal can remove it.",
		Agent:  &appHomeAgent{Name: goalsFixtureClass, Goals: &appHomeGoals{LoadFailed: true}},
	}).Blocks.BlockSet
	text := appHomeText(slackapi.HomeTabViewRequest{Blocks: slackapi.Blocks{BlockSet: blocks}})
	assert.Contains(t, text, "couldn't load your group goals")
	assert.Contains(t, text, "Only a manager of this goal can remove it.")
}

func TestAppHomeGoals_NoSectionWithoutGoals(t *testing.T) {
	for name, g := range map[string]*appHomeGoals{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			blocks := buildAppHomeView(appHomeViewInput{Agent: &appHomeAgent{Name: goalsFixtureClass, Goals: g}}).Blocks.BlockSet
			text := appHomeText(slackapi.HomeTabViewRequest{Blocks: slackapi.Blocks{BlockSet: blocks}})
			assert.NotContains(t, text, "Group goals")
			assert.Empty(t, actionIDs(blocks))
		})
	}
}

// --- loading the section --------------------------------------------------

func TestBuildHomeInput_LoadsGoalsForClicker(t *testing.T) {
	gc := &fakeGoalsClient{rows: []channelkinds.GoalSummary{{Resource: goalA, Title: "Weekly triage", Group: "slack_channel:CAAA#member", State: string(goals.Active)}}, total: 1}
	l, _, viewer := newGoalsListener(t, gc)

	in, err := l.buildHomeInputForUser(context.Background(), goalsFixtureUser)
	require.NoError(t, err)
	require.NotNil(t, in.Agent)
	require.NotNil(t, in.Agent.Goals, "a configured goals client loads the section")
	assert.Len(t, in.Agent.Goals.Rows, 1)
	assert.Equal(t, 1, in.Agent.Goals.Total)
	require.Len(t, gc.lists, 1)
	assert.Equal(t, goalsListCall{viewer: viewer, ns: goalsFixtureNS, class: goalsFixtureClass, limit: appHomeGoalLimit}, gc.lists[0])
}

func TestBuildHomeInput_ListErrorRendersLoadFailedAndLogs(t *testing.T) {
	gc := &fakeGoalsClient{listErr: errors.New("operator unreachable")}
	l, _, _ := newGoalsListener(t, gc)
	ctx, logs := captureLogs(t)

	in, err := l.buildHomeInputForUser(ctx, goalsFixtureUser)
	require.NoError(t, err)
	require.NotNil(t, in.Agent)
	require.NotNil(t, in.Agent.Goals)
	assert.True(t, in.Agent.Goals.LoadFailed)
	assert.Contains(t, logs(), "operator unreachable")
	assert.Contains(t, logs(), goalsFixtureUser)
}

func TestHandleAppHomeOpened_NilGoalsClientOmitsSection(t *testing.T) {
	l, api, _ := newGoalsListener(t, nil)
	require.NotPanics(t, func() {
		l.handleAppHomeOpened(context.Background(), &slackevents.AppHomeOpenedEvent{User: goalsFixtureUser, Tab: "home"})
	})
	require.Len(t, api.published, 1)
	text := appHomeText(api.published[0])
	assert.Contains(t, text, "Demo Agent")
	assert.NotContains(t, text, "Group goals")
}

// --- clicks ---------------------------------------------------------------

func TestHomeGoalClick_ViewPlanOpensPlanModal(t *testing.T) {
	gc := &fakeGoalsClient{plan: standingPlanFixture(t)}
	l, api, viewer := newGoalsListener(t, gc)

	handled, err := l.handleHomeGoalClick(context.Background(), goalButtonClick(homeGoalViewPlan, goalA))
	require.NoError(t, err)
	assert.True(t, handled)

	require.Len(t, gc.plans, 1)
	assert.Equal(t, goalsPlanCall{viewer: viewer, ns: goalsFixtureNS, resource: goalA}, gc.plans[0], "View plan names no action")
	assert.Empty(t, gc.acts, "viewing the plan decides nothing")
	require.Len(t, api.opened, 1)
	text := modalText(api.opened[0])
	assert.Contains(t, text, "acme/widgets", "the plan card names its resources")
	assert.Contains(t, text, "triage new issues", "the agent's reasons are shown")
	assert.Nil(t, api.opened[0].Submit, "the plan view has nothing to submit")
}

func TestHomeGoalClick_PauseActsAndRepublishes(t *testing.T) {
	gc := &fakeGoalsClient{}
	l, api, viewer := newGoalsListener(t, gc)

	handled, err := l.handleHomeGoalClick(context.Background(), goalButtonClick(goals.ActPause, goalA))
	require.NoError(t, err)
	assert.True(t, handled)

	require.Len(t, gc.acts, 1)
	assert.Equal(t, goalsActCall{viewer: viewer, ns: goalsFixtureNS, resource: goalA, action: goals.ActPause}, gc.acts[0])
	assert.Empty(t, api.opened, "pause acts immediately, without a modal")
	assert.Len(t, api.published, 1, "the tab is republished")
}

func TestHomeGoalClick_ConfirmedActionsOpenModalAndDecideOnlyOnSubmit(t *testing.T) {
	cases := []struct {
		action   string
		showPlan bool
		wantText string
		wantMD   map[string]any
		digest   string
	}{
		{action: goals.ActResume, showPlan: true, wantText: "approv", digest: "approval-binding:d1",
			wantMD: map[string]any{"goal": goalA, "digest": "approval-binding:d1"}},
		{action: goals.ActRenew, showPlan: true, wantText: "approv", digest: "approval-binding:d1",
			wantMD: map[string]any{"goal": goalA, "digest": "approval-binding:d1"}},
		{action: goals.ActRemove, showPlan: false, wantText: "cannot be undone", wantMD: map[string]any{"goal": goalA}},
	}
	for _, tc := range cases {
		t.Run(tc.action+": modal on click, Act on submit", func(t *testing.T) {
			gc := &fakeGoalsClient{plan: standingPlanFixture(t), title: "Weekly *triage* <!channel>", digest: tc.digest}
			l, api, viewer := newGoalsListener(t, gc)

			handled, err := l.handleHomeGoalClick(context.Background(), goalButtonClick(tc.action, goalA))
			require.NoError(t, err)
			assert.True(t, handled)
			assert.Empty(t, gc.acts, "the button click is not the decision")
			require.Len(t, api.opened, 1, "a confirmation modal opens")
			modal := api.opened[0]
			require.NotNil(t, modal.Submit, "the modal can be submitted")
			text := modalText(modal)
			assert.Contains(t, strings.ToLower(text), tc.wantText)
			require.Len(t, gc.plans, 1, "the server supplies what the modal shows")
			assert.Equal(t, goalsPlanCall{viewer: viewer, ns: goalsFixtureNS, resource: goalA, action: tc.action}, gc.plans[0],
				"read for the action, so it is what that action approves")
			assert.Contains(t, text, "Weekly", "the server's title is shown")
			assert.NotContains(t, text, "<!channel>", "and swept inert")
			if tc.showPlan {
				assert.Contains(t, text, "acme/widgets", "an approval shows the standing plan it approves")
			}
			var md map[string]any
			require.NoError(t, json.Unmarshal([]byte(modal.PrivateMetadata), &md))
			assert.Equal(t, tc.wantMD, md, "private_metadata carries the goal and, for an approval, the plan it binds to")

			handled, err = l.handleHomeGoalSubmit(context.Background(), goalSubmit(t, modal))
			require.NoError(t, err)
			assert.True(t, handled)
			require.Len(t, gc.acts, 1)
			assert.Equal(t, goalsActCall{viewer: viewer, ns: goalsFixtureNS, resource: goalA, action: tc.action, digest: tc.digest}, gc.acts[0],
				"the act names the plan the modal showed")
			assert.Len(t, api.published, 1, "the tab is republished after the decision")
		})
	}
}

func TestHomeGoalSubmit_ForgedMetadataViewerIgnored(t *testing.T) {
	gc := &fakeGoalsClient{}
	l, _, viewer := newGoalsListener(t, gc)
	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeViewSubmission,
		User: slackapi.User{ID: goalsFixtureUser},
		View: slackapi.View{
			CallbackID:      homeGoalConfirmCallbackPrefix + goals.ActRemove,
			PrivateMetadata: `{"goal":"agent_goal:g-a","viewer":"attacker","ns":"elsewhere","user":"U_ADMIN"}`,
		},
	}
	handled, err := l.handleHomeGoalSubmit(context.Background(), cb)
	require.NoError(t, err)
	assert.True(t, handled)
	require.Len(t, gc.acts, 1)
	assert.Equal(t, viewer, gc.acts[0].viewer, "the viewer is the Slack-verified clicker")
	assert.Equal(t, goalsFixtureNS, gc.acts[0].ns, "the namespace is the listener's own, not the metadata's")
	assert.NotContains(t, gc.acts[0].viewer.String(), "attacker")
}

func TestHomeGoalSubmit_RefusesUnconfirmableAction(t *testing.T) {
	gc := &fakeGoalsClient{}
	l, _, _ := newGoalsListener(t, gc)
	for _, action := range []string{goals.ActPause, "delete-everything", ""} {
		cb := slackapi.InteractionCallback{
			Type: slackapi.InteractionTypeViewSubmission,
			User: slackapi.User{ID: goalsFixtureUser},
			View: slackapi.View{CallbackID: homeGoalConfirmCallbackPrefix + action, PrivateMetadata: `{"goal":"agent_goal:g-a"}`},
		}
		handled, err := l.handleHomeGoalSubmit(context.Background(), cb)
		assert.True(t, handled, "%q: our callback is ours to refuse", action)
		assert.Error(t, err, "%q: not a confirmable action", action)
	}
	assert.Empty(t, gc.acts)
}

func TestHomeGoal_ActErrorSurfacesNoticeLogsAndRepublishes(t *testing.T) {
	cases := []struct {
		name       string
		action     string
		actErr     error
		wantNotice string
	}{
		{name: "denied remove: manager-only notice", action: goals.ActRemove, actErr: fmt.Errorf("%w: not a manager", goals.ErrDenied), wantNotice: "Only a manager of this goal can remove it."},
		{name: "denied resume: manager-only notice", action: goals.ActResume, actErr: fmt.Errorf("%w: not a manager", goals.ErrDenied), wantNotice: "Only a manager of this goal can resume it."},
		{name: "other failure: generic notice", action: goals.ActRenew, actErr: errors.New("boom"), wantNotice: "Couldn't renew this goal"},
		{name: "plan changed since shown: reopen notice", action: goals.ActRenew, actErr: fmt.Errorf("renew: %w: reopen it", goals.ErrPlanChanged), wantNotice: "plan changed since you opened"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gc := &fakeGoalsClient{actErr: tc.actErr, plan: standingPlanFixture(t)}
			l, api, _ := newGoalsListener(t, gc)
			ctx, logs := captureLogs(t)
			cb := slackapi.InteractionCallback{
				Type: slackapi.InteractionTypeViewSubmission,
				User: slackapi.User{ID: goalsFixtureUser},
				View: slackapi.View{CallbackID: homeGoalConfirmCallbackPrefix + tc.action, PrivateMetadata: `{"goal":"agent_goal:g-a"}`},
			}
			handled, err := l.handleHomeGoalSubmit(ctx, cb)
			assert.True(t, handled)
			assert.NoError(t, err, "a refusal is surfaced and logged, not returned for a second log")
			require.Len(t, api.published, 1, "the tab is republished even on failure")
			assert.Contains(t, appHomeText(api.published[0]), tc.wantNotice)
			got := logs()
			assert.Contains(t, got, goalA)
			assert.Contains(t, got, tc.action)
			assert.Contains(t, got, goalsFixtureUser)
		})
	}
}

func TestHomeGoalClick_PauseErrorSurfacesNotice(t *testing.T) {
	gc := &fakeGoalsClient{actErr: fmt.Errorf("%w: left the channel", goals.ErrDenied)}
	l, api, _ := newGoalsListener(t, gc)
	handled, err := l.handleHomeGoalClick(context.Background(), goalButtonClick(goals.ActPause, goalA))
	assert.True(t, handled)
	assert.NoError(t, err)
	require.Len(t, api.published, 1)
	assert.Contains(t, appHomeText(api.published[0]), "Only a member of this goal's channel can pause it.")
}

func TestHomeGoalClick_PlanErrorSurfacesNoticeWithoutModal(t *testing.T) {
	gc := &fakeGoalsClient{planErr: errors.New("plan store down")}
	l, api, _ := newGoalsListener(t, gc)
	ctx, logs := captureLogs(t)
	handled, err := l.handleHomeGoalClick(ctx, goalButtonClick(goals.ActResume, goalA))
	assert.True(t, handled)
	assert.NoError(t, err)
	assert.Empty(t, api.opened, "no approval modal without the plan it approves")
	assert.Empty(t, gc.acts)
	require.Len(t, api.published, 1)
	assert.Contains(t, appHomeText(api.published[0]), "Couldn't load")
	assert.Contains(t, logs(), "plan store down")
}

func TestHomeGoalClick_NilGoalsClientSurfacesNotice(t *testing.T) {
	l, api, _ := newGoalsListener(t, nil)
	require.NotPanics(t, func() {
		handled, err := l.handleHomeGoalClick(context.Background(), goalButtonClick(goals.ActPause, goalA))
		assert.True(t, handled)
		assert.Error(t, err)
	})
	require.Len(t, api.published, 1)
	assert.Contains(t, appHomeText(api.published[0]), "aren't available")
}

func TestHomeGoalClick_IgnoresOtherActions(t *testing.T) {
	l, _, _ := newGoalsListener(t, &fakeGoalsClient{})
	cb := slackapi.InteractionCallback{
		Type:           slackapi.InteractionTypeBlockActions,
		User:           slackapi.User{ID: goalsFixtureUser},
		ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{{ActionID: "home:pref_save:demo/demo-agent"}}},
	}
	handled, err := l.handleHomeGoalClick(context.Background(), cb)
	assert.NoError(t, err)
	assert.False(t, handled)
	handled, err = l.handleHomeGoalSubmit(context.Background(), slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeViewSubmission, View: slackapi.View{CallbackID: RestartModalCallbackID},
	})
	assert.NoError(t, err)
	assert.False(t, handled)
}

func TestOnInteraction_RoutesGoalClickAndSubmit(t *testing.T) {
	gc := &fakeGoalsClient{}
	l, _, _ := newGoalsListener(t, gc)
	require.NoError(t, l.onInteraction(context.Background(), goalButtonClick(goals.ActPause, goalA)))
	require.NoError(t, l.onInteraction(context.Background(), slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeViewSubmission,
		User: slackapi.User{ID: goalsFixtureUser},
		View: slackapi.View{CallbackID: homeGoalConfirmCallbackPrefix + goals.ActRemove, PrivateMetadata: `{"goal":"agent_goal:g-a"}`},
	}))
	require.Len(t, gc.acts, 2)
	assert.Equal(t, goals.ActPause, gc.acts[0].action)
	assert.Equal(t, goals.ActRemove, gc.acts[1].action)
}

// worstCaseGoals is a full goals section: the row cap, every goal in its own
// channel (a heading each), every button, and more goals than shown.
func worstCaseGoals() *appHomeGoals {
	var rows []channelkinds.GoalSummary
	for i := range appHomeGoalLimit {
		rows = append(rows, channelkinds.GoalSummary{
			Resource: fmt.Sprintf("agent_goal:g%d", i), Title: "t", State: string(goals.Paused),
			Group: fmt.Sprintf("slack_channel:C%03d#member", i), CanPause: true, CanManage: true, CanRenew: true,
		})
	}
	return &appHomeGoals{Rows: rows, Total: 50}
}

// worstCaseAgent is every other section at its largest beside goals: a notice,
// a passthrough agent with connections and a portal link, and nPrefs
// free-text preferences (with their Save button).
func worstCaseAgent(nPrefs int, g *appHomeGoals) appHomeViewInput {
	prefs := make([]preferences.Resolved, 0, nPrefs)
	for i := range nPrefs {
		prefs = append(prefs, preferences.Resolved{Name: fmt.Sprintf("k%d", i), Type: "string", Description: "d"})
	}
	return appHomeViewInput{Notice: "n", Agent: &appHomeAgent{
		Name: goalsFixtureClass, DisplayName: "Demo", Description: "desc",
		IdentityMode:             spiceboxv1alpha1.IdentityModeUserPassthrough,
		AllRequiredServiceLabels: []string{"GitHub"}, PortalLinkURL: "https://portal.example/my",
		ClassRef: goalsFixtureNS + "/" + goalsFixtureClass, Preferences: prefs,
		Goals: g,
	}}
}

func goalRowCount(blocks []slackapi.Block) int {
	n := 0
	for _, b := range blocks {
		if s, ok := b.(*slackapi.SectionBlock); ok && strings.HasPrefix(s.BlockID, "goal:") {
			n++
		}
	}
	return n
}

func TestAppHomeGoals_WorstCaseStaysUnderViewBlockLimit(t *testing.T) {
	cases := []struct {
		name     string
		prefs    int
		wantRows int
	}{
		{name: "a few preferences: every goal row fits", prefs: 5, wantRows: appHomeGoalLimit},
		{name: "many preferences: goal rows are cut to fit, and counted as more", prefs: 70, wantRows: -1},
		{name: "preferences nearly fill the page: no goal row, the section says how many there are", prefs: 84, wantRows: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks := buildAppHomeView(worstCaseAgent(tc.prefs, worstCaseGoals())).Blocks.BlockSet
			assert.LessOrEqual(t, len(blocks), appHomeMaxBlocks, "the tab never exceeds Slack's 100-block view limit")
			rows := goalRowCount(blocks)
			switch tc.wantRows {
			case -1:
				assert.Greater(t, rows, 0)
				assert.Less(t, rows, appHomeGoalLimit)
			default:
				assert.Equal(t, tc.wantRows, rows)
			}
			text := appHomeText(slackapi.HomeTabViewRequest{Blocks: slackapi.Blocks{BlockSet: blocks}})
			if rows == 0 {
				assert.Contains(t, text, "50 group goals", "with no row shown, the section still counts them")
			} else {
				assert.Contains(t, text, fmt.Sprintf("…and %d more", 50-rows), "every goal not shown is counted")
			}
		})
	}
	t.Run("no room even for the section's heading: no section, still under the limit", func(t *testing.T) {
		blocks := buildAppHomeView(worstCaseAgent(86, worstCaseGoals())).Blocks.BlockSet
		assert.LessOrEqual(t, len(blocks), appHomeMaxBlocks)
		assert.NotContains(t, appHomeText(slackapi.HomeTabViewRequest{Blocks: slackapi.Blocks{BlockSet: blocks}}), "Group goals")
	})
	t.Run("without goals, the rest of the page is unchanged", func(t *testing.T) {
		with := buildAppHomeView(worstCaseAgent(5, worstCaseGoals())).Blocks.BlockSet
		without := buildAppHomeView(worstCaseAgent(5, nil)).Blocks.BlockSet
		assert.Equal(t, len(without)+len(goalsSectionBlocks(worstCaseGoals())), len(with))
	})
}
