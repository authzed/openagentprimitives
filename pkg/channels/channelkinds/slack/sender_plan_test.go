package slack

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// planEnvelope builds a KindPlanUpdate envelope with the given payload
// for the test's standard session ref.
func planEnvelope(t *testing.T, pl channelevents.PlanUpdatePayload) channelevents.Envelope {
	t.Helper()
	body, err := json.Marshal(pl)
	require.NoError(t, err)
	return channelevents.Envelope{
		Version:     1,
		Kind:        channelevents.KindPlanUpdate,
		Session:     channelevents.SessionRef{Namespace: "default", Name: "sess1"},
		PublishedAt: metav1.Now().Time,
		Payload:     body,
	}
}

func sessForPlanTest() channelkinds.SessionInfo {
	return channelkinds.SessionInfo{
		Namespace: "default", Name: "sess1",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			Name: "slack-eng", Kind: "slack",
			External: map[string]string{
				"channel_id": "C01ABCDEF",
				"thread_ts":  "1614191050.013300",
			},
		},
	}
}

// TestSlackPlanUpdate_RestoresAssistantStatusAfterFirstPost is the
// regression test for: "when the plan was updated, the status
// cleared". Slack auto-clears assistant.threads.setStatus whenever
// the agent posts a chat.postMessage in the assistant thread (per
// Slack's documented behavior). The first plan_update envelope hits
// the chat.postMessage path, which kills whatever update_status had
// just published. The sender must re-issue setStatus immediately
// after the post so the indicator the user was watching reappears.
func TestSlackPlanUpdate_RestoresAssistantStatusAfterFirstPost(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := sessForPlanTest()

	// 1. Agent calls update_status — sender publishes the indicator.
	pl, _ := json.Marshal(channelevents.NotificationPayload{
		Text:  "Reading demo-org/demo-repo commits…",
		Short: "Reading commits",
	})
	_, err := s.Send(context.Background(), sess, channelevents.Envelope{
		Version: 1, Kind: channelevents.KindNotification,
		Session: channelevents.SessionRef{Namespace: "default", Name: "sess1"},
		Payload: pl,
	})
	require.NoError(t, err)
	require.Len(t, c.setStatusCalls, 1, "update_status should produce one setStatus call")
	require.Equal(t, "Reading demo-org/demo-repo commits…", c.setStatusCalls[0].status)

	// 2. Agent calls update_plan for the first time — first post hits
	//    chat.postMessage, which Slack uses to auto-clear setStatus.
	planPl := channelevents.PlanUpdatePayload{
		PlanName: "main",
		Items:    []channelevents.PlanItemRef{{ID: "a", Label: "A", Status: "pending"}},
	}
	_, err = s.Send(context.Background(), sess, planEnvelope(t, planPl))
	require.NoError(t, err)
	require.Len(t, c.postMessageCalls, 1, "first plan envelope should chat.postMessage")

	// 3. Sender must restore the previous status by re-issuing
	//    setStatus with the same text. Without this, the user sees
	//    the indicator vanish until the next update_status arrives.
	require.Len(t, c.setStatusCalls, 2,
		"plan post should be followed by a restorative setStatus; got setStatusCalls=%v", c.setStatusCalls)
	require.Equal(t, "Reading demo-org/demo-repo commits…", c.setStatusCalls[1].status,
		"restored setStatus must echo the most-recent update_status text")
}

// TestSlackPlanUpdate_DoesNotRestoreStatusOnUpdate covers the
// non-first plan envelope path: chat.update does NOT clear
// setStatus, so the sender must NOT re-issue (avoids unnecessary
// API calls and an audible "blink" if the runner re-applies the
// same value).
func TestSlackPlanUpdate_DoesNotRestoreStatusOnUpdate(t *testing.T) {
	c := &fakeSlackClient{}
	c.postedTS = "1700000000.000100"
	s := newSender(c)
	sess := sessForPlanTest()

	pl, _ := json.Marshal(channelevents.NotificationPayload{
		Text: "Working…", Short: "Working",
	})
	_, err := s.Send(context.Background(), sess, channelevents.Envelope{
		Version: 1, Kind: channelevents.KindNotification,
		Session: channelevents.SessionRef{Namespace: "default", Name: "sess1"},
		Payload: pl,
	})
	require.NoError(t, err)

	// First plan post: chat.postMessage + restored setStatus.
	planPl := channelevents.PlanUpdatePayload{PlanName: "main",
		Items: []channelevents.PlanItemRef{{ID: "a", Label: "A", Status: "pending"}}}
	_, _ = s.Send(context.Background(), sess, planEnvelope(t, planPl))
	require.Len(t, c.setStatusCalls, 2, "first plan post must restore status")

	// Second plan post: chat.update path. Should NOT issue another setStatus.
	planPl.Items[0].Status = "in_progress"
	_, _ = s.Send(context.Background(), sess, planEnvelope(t, planPl))
	require.Len(t, c.setStatusCalls, 2,
		"chat.update plan path must NOT re-issue setStatus; got %d total calls", len(c.setStatusCalls))
}

func TestSlackPlanUpdate_FirstEnvelopePostsMessage(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)

	pl := channelevents.PlanUpdatePayload{
		PlanName: "main",
		Items: []channelevents.PlanItemRef{
			{ID: "a", Label: "Step A", Status: "pending"},
		},
		UpdatedAt: time.Now().UTC(),
	}
	_, err := s.Send(context.Background(), sessForPlanTest(), planEnvelope(t, pl))
	require.NoError(t, err)

	require.Len(t, c.postMessageCalls, 1)
	require.Equal(t, "C01ABCDEF", c.postMessageCalls[0].channelID)
	require.Empty(t, c.updateCalls)
}

func TestSlackPlanUpdate_SecondEnvelopeUpdatesMessage(t *testing.T) {
	// Make the fake return a deterministic ts on PostMessage so we can
	// assert chat.update is keyed off the same value.
	c := &fakeSlackClient{}
	c.postedTS = "1700000000.000100"
	s := newSender(c)

	pl1 := channelevents.PlanUpdatePayload{
		PlanName: "main",
		Items:    []channelevents.PlanItemRef{{ID: "a", Label: "A", Status: "pending"}},
	}
	_, err := s.Send(context.Background(), sessForPlanTest(), planEnvelope(t, pl1))
	require.NoError(t, err)

	pl2 := channelevents.PlanUpdatePayload{
		PlanName: "main",
		Items:    []channelevents.PlanItemRef{{ID: "a", Label: "A", Status: "in_progress"}},
	}
	_, err = s.Send(context.Background(), sessForPlanTest(), planEnvelope(t, pl2))
	require.NoError(t, err)

	require.Len(t, c.postMessageCalls, 1, "second envelope must edit, not post")
	require.Len(t, c.updateCalls, 1)
	require.Equal(t, "C01ABCDEF", c.updateCalls[0].channelID)
	require.Equal(t, "1700000000.000100", c.updateCalls[0].ts)
}

func TestSlackPlanUpdate_DeletionEditsToCancelledStub(t *testing.T) {
	c := &fakeSlackClient{}
	c.postedTS = "1700000000.000200"
	s := newSender(c)

	// Create.
	plCreate := channelevents.PlanUpdatePayload{
		PlanName: "main",
		Items:    []channelevents.PlanItemRef{{ID: "a", Label: "A", Status: "pending"}},
	}
	_, err := s.Send(context.Background(), sessForPlanTest(), planEnvelope(t, plCreate))
	require.NoError(t, err)

	// Delete (empty items).
	plDelete := channelevents.PlanUpdatePayload{PlanName: "main", Items: nil}
	_, err = s.Send(context.Background(), sessForPlanTest(), planEnvelope(t, plDelete))
	require.NoError(t, err)

	// One post + one update; chat.delete NOT called (no method on the
	// mock for it; we'd see it as something other than the recorded
	// post/update slices).
	require.Len(t, c.postMessageCalls, 1)
	require.Len(t, c.updateCalls, 1)
	require.Equal(t, "1700000000.000200", c.updateCalls[0].ts)
}

func TestSlackPlanUpdate_TsMappingIsolatedPerSessionPlan(t *testing.T) {
	c := &fakeSlackClient{}
	c.postedTS = "1700000000.000300"
	s := newSender(c)

	pl := channelevents.PlanUpdatePayload{
		PlanName: "main",
		Items:    []channelevents.PlanItemRef{{ID: "a", Label: "A", Status: "pending"}},
	}

	sessA := sessForPlanTest()
	sessB := sessForPlanTest()
	sessB.Name = "sess2" // different session

	_, err := s.Send(context.Background(), sessA, planEnvelope(t, pl))
	require.NoError(t, err)
	_, err = s.Send(context.Background(), sessB, planEnvelope(t, pl))
	require.NoError(t, err)

	// Each (session, plan) tuple is independent — both got post calls.
	require.Len(t, c.postMessageCalls, 2)
	require.Empty(t, c.updateCalls)

	// Same plan name, distinct sessions; subsequent sessA update edits sessA's ts.
	pl2 := pl
	pl2.Items[0].Status = "in_progress"
	_, err = s.Send(context.Background(), sessA, planEnvelope(t, pl2))
	require.NoError(t, err)
	require.Len(t, c.updateCalls, 1)
}

// TestSlackPlanUpdate_FirstEnvelopePostsFresh verifies that the first
// plan_update always posts a new chat.postMessage — the informational starter
// posted by the listener is never edited. The planMessages map (keyed by
// session+planName) then tracks the returned ts for subsequent updates.
func TestSlackPlanUpdate_FirstEnvelopePostsFresh(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)

	pl := channelevents.PlanUpdatePayload{
		PlanName: "main",
		Items:    []channelevents.PlanItemRef{{ID: "a", Label: "A", Status: "pending"}},
	}
	_, err := s.Send(context.Background(), sessForPlanTest(), planEnvelope(t, pl))
	require.NoError(t, err)

	require.Len(t, c.postMessageCalls, 1,
		"first plan envelope must always post fresh (starter is never edited)")
	require.Empty(t, c.updateCalls, "no chat.update expected on first plan envelope")
	require.Equal(t, "C01ABCDEF", c.postMessageCalls[0].channelID)

	// Subsequent plan_update for the same plan edits the returned ts.
	c.postedTS = "1700000000.000100"
	// Seed the planMessages map as if the first post returned that ts.
	s.planMessages.set(sessForPlanTest(), "main", "1700000000.000100")

	pl2 := pl
	pl2.Items[0].Status = "in_progress"
	_, err = s.Send(context.Background(), sessForPlanTest(), planEnvelope(t, pl2))
	require.NoError(t, err)
	require.Len(t, c.updateCalls, 1)
	require.Equal(t, "1700000000.000100", c.updateCalls[0].ts)
}

func TestToSlackTaskStatus(t *testing.T) {
	cases := []struct {
		in   string
		want slackapi.TaskCardStatus
	}{
		{"pending", slackapi.TaskCardStatusPending},
		{"in_progress", slackapi.TaskCardStatusInProgress},
		{"done", slackapi.TaskCardStatusComplete},
		{"error", slackapi.TaskCardStatusError},
		{"", slackapi.TaskCardStatusPending},
		{"bogus", slackapi.TaskCardStatusPending},
	}
	for _, c := range cases {
		if got := toSlackTaskStatus(c.in); got != c.want {
			t.Errorf("toSlackTaskStatus(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPlainRichText(t *testing.T) {
	rt := plainRichText("hello & <world>")
	require.NotNil(t, rt)
	require.Equal(t, slackapi.MBTRichText, rt.Type)
	require.Len(t, rt.Elements, 1)

	sec, ok := rt.Elements[0].(*slackapi.RichTextSection)
	require.True(t, ok, "first element should be *RichTextSection, got %T", rt.Elements[0])
	require.Len(t, sec.Elements, 1)

	txt, ok := sec.Elements[0].(*slackapi.RichTextSectionTextElement)
	require.True(t, ok, "section element should be text, got %T", sec.Elements[0])
	require.Equal(t, "hello & <world>", txt.Text,
		"plainRichText must pass through verbatim — no HTML escaping; rich_text isn't mrkdwn")
}

func TestPlainRichText_EmptyInputReturnsNil(t *testing.T) {
	require.Nil(t, plainRichText(""))
}

func TestRenderPlanBlocks_NativeShape(t *testing.T) {
	pl := channelevents.PlanUpdatePayload{
		PlanName: "deploy",
		Items: []channelevents.PlanItemRef{
			{ID: "a", Label: "Pending step", Status: "pending"},
			{ID: "b", Label: "Active step", Status: "in_progress", Details: "fetching commits"},
			{ID: "c", Label: "Done step", Status: "done", Details: "fetched", Output: "found 3 PRs"},
			{ID: "d", Label: "Failed step", Status: "error", Output: "exit code 1"},
		},
	}
	blocks := renderPlanBlocks(pl)
	require.Len(t, blocks, 1, "no parent ref → expect just the plan block")

	plan, ok := blocks[0].(*slackapi.PlanBlock)
	require.True(t, ok, "first block should be *PlanBlock, got %T", blocks[0])
	require.Equal(t, "deploy", plan.Title)
	require.Len(t, plan.Tasks, 4)

	require.Equal(t, "a", plan.Tasks[0].TaskID)
	require.Equal(t, "Pending step", plan.Tasks[0].Title)
	require.Equal(t, slackapi.TaskCardStatusPending, plan.Tasks[0].Status)
	require.Nil(t, plan.Tasks[0].Details)
	require.Nil(t, plan.Tasks[0].Output)

	require.Equal(t, slackapi.TaskCardStatusInProgress, plan.Tasks[1].Status)
	require.NotNil(t, plan.Tasks[1].Details, "Details should be rendered for in_progress when supplied")

	require.Equal(t, slackapi.TaskCardStatusComplete, plan.Tasks[2].Status)
	require.NotNil(t, plan.Tasks[2].Details)
	require.NotNil(t, plan.Tasks[2].Output)

	require.Equal(t, slackapi.TaskCardStatusError, plan.Tasks[3].Status)
	require.NotNil(t, plan.Tasks[3].Output, "Output should be rendered for error when supplied")
	require.Nil(t, plan.Tasks[3].Details)
}

func TestRenderPlanBlocks_MainPlanNameBecomesGenericTitle(t *testing.T) {
	pl := channelevents.PlanUpdatePayload{
		PlanName: "main",
		Items:    []channelevents.PlanItemRef{{ID: "a", Label: "A", Status: "pending"}},
	}
	blocks := renderPlanBlocks(pl)
	plan := blocks[0].(*slackapi.PlanBlock)
	require.Equal(t, "Plan", plan.Title, "plan name 'main' should render as 'Plan' (the literal name is uninformative)")
}

func TestRenderPlanBlocks_ParentRefRendersAsLeadingContextBlock(t *testing.T) {
	pl := channelevents.PlanUpdatePayload{
		PlanName:   "subplan",
		ParentPlan: "main",
		ParentItem: "step2",
		Items:      []channelevents.PlanItemRef{{ID: "a", Label: "A", Status: "pending"}},
	}
	blocks := renderPlanBlocks(pl)
	require.Len(t, blocks, 2, "parent ref present → one context + one plan block")
	_, ok := blocks[0].(*slackapi.ContextBlock)
	require.True(t, ok, "first block should be *ContextBlock carrying the sub-plan badge, got %T", blocks[0])
	_, ok = blocks[1].(*slackapi.PlanBlock)
	require.True(t, ok)
}

func TestRenderPlanBlocks_EmptyItemsRendersCancelledStub(t *testing.T) {
	pl := channelevents.PlanUpdatePayload{PlanName: "main", Items: nil}
	blocks := renderPlanBlocks(pl)
	require.Len(t, blocks, 1)
	sec, ok := blocks[0].(*slackapi.SectionBlock)
	require.True(t, ok, "empty plan should fall back to a single section block 'Plan cancelled', got %T", blocks[0])
	require.NotNil(t, sec.Text)
	require.Contains(t, sec.Text.Text, "Plan cancelled")
}

func TestSlackPlanUpdate_FallsBackOnInvalidBlocks(t *testing.T) {
	c := &fakeSlackClient{}
	// First PostMessageContext fails with invalid_blocks; second succeeds.
	c.postMessageErrs = []error{
		slackapi.SlackErrorResponse{Err: "invalid_blocks"},
		nil,
	}
	s := newSender(c)

	pl := channelevents.PlanUpdatePayload{
		PlanName: "main",
		Items:    []channelevents.PlanItemRef{{ID: "a", Label: "A", Status: "pending"}},
	}
	_, err := s.Send(context.Background(), sessForPlanTest(), planEnvelope(t, pl))
	require.NoError(t, err, "fallback should swallow the invalid_blocks error and re-attempt")

	require.Len(t, c.postMessageCalls, 2,
		"expect 2 postMessage calls: first with native blocks (rejected), second with legacy blocks")
}

func TestSlackPlanUpdate_DoesNotFallBackOnUnrelatedError(t *testing.T) {
	c := &fakeSlackClient{}
	c.postMessageErrs = []error{errors.New("rate_limited")} // not invalid_blocks
	s := newSender(c)

	pl := channelevents.PlanUpdatePayload{
		PlanName: "main",
		Items:    []channelevents.PlanItemRef{{ID: "a", Label: "A", Status: "pending"}},
	}
	_, err := s.Send(context.Background(), sessForPlanTest(), planEnvelope(t, pl))
	require.Error(t, err, "non-block errors must surface to the caller")
	require.Len(t, c.postMessageCalls, 1, "no retry on unrelated errors")
}

func TestIsUnsupportedBlocksErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"invalid_blocks", slackapi.SlackErrorResponse{Err: "invalid_blocks"}, true},
		{"invalid_blocks_format", slackapi.SlackErrorResponse{Err: "invalid_blocks_format"}, true},
		{"block_unknown_type", slackapi.SlackErrorResponse{Err: "block_unknown_type"}, true},
		{"rate_limited", slackapi.SlackErrorResponse{Err: "rate_limited"}, false},
		{"plain error", errors.New("invalid_blocks"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isUnsupportedBlocksErr(c.err); got != c.want {
				t.Errorf("isUnsupportedBlocksErr(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestRenderPlanBlocks_PausedPrependsBannerAndNeutralisesActiveItem(t *testing.T) {
	pl := channelevents.PlanUpdatePayload{
		PlanName: "deploy",
		Items: []channelevents.PlanItemRef{
			{ID: "a", Label: "Done step", Status: "done"},
			{ID: "b", Label: "Active step", Status: "in_progress"},
			{ID: "c", Label: "Pending step", Status: "pending"},
		},
		Paused:     true,
		PauseCause: channelevents.PauseCauseApproval,
	}
	blocks := renderPlanBlocks(pl)
	require.Len(t, blocks, 2, "paused → one banner context block + one plan block")

	banner, ok := blocks[0].(*slackapi.ContextBlock)
	require.True(t, ok, "first block should be the paused banner ContextBlock, got %T", blocks[0])
	require.Contains(t, banner.ContextElements.Elements[0].(*slackapi.TextBlockObject).Text,
		"waiting for your approval")

	plan := blocks[1].(*slackapi.PlanBlock)
	require.Equal(t, slackapi.TaskCardStatusComplete, plan.Tasks[0].Status, "done item untouched")
	require.Equal(t, slackapi.TaskCardStatusPending, plan.Tasks[1].Status,
		"in_progress item must be neutralised to pending while paused")
	require.Equal(t, slackapi.TaskCardStatusPending, plan.Tasks[2].Status, "pending item untouched")
}

func TestRenderPlanBlocks_NotPausedRendersHourglass(t *testing.T) {
	pl := channelevents.PlanUpdatePayload{
		PlanName: "deploy",
		Items:    []channelevents.PlanItemRef{{ID: "b", Label: "Active", Status: "in_progress"}},
	}
	blocks := renderPlanBlocks(pl)
	require.Len(t, blocks, 1, "not paused → no banner")
	plan := blocks[0].(*slackapi.PlanBlock)
	require.Equal(t, slackapi.TaskCardStatusInProgress, plan.Tasks[0].Status)
}

func TestRenderPlanBlocks_PausedBannerLeadsSubPlanBadge(t *testing.T) {
	pl := channelevents.PlanUpdatePayload{
		PlanName: "subplan", ParentPlan: "main", ParentItem: "step2",
		Items:  []channelevents.PlanItemRef{{ID: "a", Label: "A", Status: "in_progress"}},
		Paused: true, PauseCause: channelevents.PauseCauseReply,
	}
	blocks := renderPlanBlocks(pl)
	require.Len(t, blocks, 3, "paused banner + sub-plan badge + plan")
	require.IsType(t, &slackapi.ContextBlock{}, blocks[0])
	require.Contains(t, blocks[0].(*slackapi.ContextBlock).ContextElements.Elements[0].(*slackapi.TextBlockObject).Text,
		"Waiting for your reply")
	require.IsType(t, &slackapi.ContextBlock{}, blocks[1]) // sub-plan badge
	require.IsType(t, &slackapi.PlanBlock{}, blocks[2])
}

func TestPausedBannerText(t *testing.T) {
	cases := []struct{ cause, want string }{
		{channelevents.PauseCauseApproval, "waiting for your approval"},
		{channelevents.PauseCauseLeakageApproval, "share data"},
		{channelevents.PauseCauseReply, "Waiting for your reply"},
		{channelevents.PauseCauseRetry, "Retry"},
		{channelevents.PauseCauseFailed, "Stopped"},
		{"", "Paused"},
		{"bogus_future_cause", "Paused"},
	}
	for _, tc := range cases {
		t.Run(tc.cause, func(t *testing.T) {
			require.Contains(t, pausedBannerText(tc.cause), tc.want)
		})
	}
}

func TestRenderPlanBlocksLegacy_PausedBannerAndNeutralise(t *testing.T) {
	pl := channelevents.PlanUpdatePayload{
		PlanName: "main",
		Items:    []channelevents.PlanItemRef{{ID: "b", Label: "Active", Status: "in_progress"}},
		Paused:   true, PauseCause: channelevents.PauseCauseRetry,
	}
	blocks := renderPlanBlocksLegacy(pl)
	first := blocks[0].(*slackapi.SectionBlock)
	require.Contains(t, first.Text.Text, "Retry", "legacy paused banner leads")
	// The active item line must use the pending glyph, not the hourglass.
	var body string
	for _, b := range blocks {
		if sec, ok := b.(*slackapi.SectionBlock); ok {
			body += sec.Text.Text
		}
	}
	require.NotContains(t, body, ":hourglass_flowing_sand:", "in_progress neutralised in legacy too")
	require.Contains(t, body, ":white_circle:")
}
