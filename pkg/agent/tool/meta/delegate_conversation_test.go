package meta

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// scriptedPoll returns a Poll func that walks the given statuses, one per
// call, holding on the last. It records how many calls it served so a test can
// tell "returned as soon as it could" from "spun to a timeout".
type scriptedPoll struct {
	mu     sync.Mutex
	states []v1.SubagentRequestStatus
	calls  int
}

func (s *scriptedPoll) poll(_ context.Context, _ string) (*v1.SubagentRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if i >= len(s.states) {
		i = len(s.states) - 1
	}
	return &v1.SubagentRequest{
		Spec:   v1.SubagentRequestSpec{Parent: v1.NamespacedRef{Namespace: "ns", Name: "demo-parent"}},
		Status: s.states[i],
	}, nil
}

func (s *scriptedPoll) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func awaitingStatus(exchange int64, message string) v1.SubagentRequestStatus {
	return v1.SubagentRequestStatus{
		Phase:    v1.SubagentRequestPhaseAwaitingParent,
		ChildRef: &v1.NamespacedRef{Namespace: "ns", Name: "demo-child"},
		Exchange: exchange,
		Message:  message,
	}
}

// Brief test 1: a child that asks a question makes delegate RETURN, carrying
// the question, instead of blocking to its timeout.
func TestDelegate_AwaitingParent_ReturnsTheQuestionInsteadOfBlockingToTimeout(t *testing.T) {
	sp := &scriptedPoll{states: []v1.SubagentRequestStatus{
		{Phase: v1.SubagentRequestPhaseRunning},
		awaitingStatus(1, "which of the two repos?"),
	}}
	tl := NewDelegateTool(DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Create:       func(context.Context, *v1.SubagentRequest) error { return nil },
		Poll:         sp.poll,
		PollInterval: time.Millisecond,
		// Long enough that a timeout return would take far longer than this
		// test does: reaching the assertions at all proves the AwaitingParent
		// arm returned rather than the deadline expiring.
		Timeout: time.Hour,
	})

	start := time.Now()
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"agent":"demo-coder","task":"open the PR","mode":"task"}`), nil)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 10*time.Second, "returned on the question, not on the one-hour deadline")
	assert.False(t, res.IsError, "a question is not a failure")
	assert.Contains(t, res.Content, "which of the two repos?", "the child's own words reach the parent")
	assert.Contains(t, res.Content, "reply_to_subagent", "the parent is told how to answer")
}

// Brief test 6: the question reaches the parent UNTRUSTED, on the same footing
// as the final result — asserted on the flag, not on the content.
//
// Remove `Trusted: false` (i.e. set it true) on waitForNext's AwaitingParent
// arm and this fails while every content assertion in this file still passes;
// that is exactly the regression it exists to catch.
func TestDelegate_AwaitingParent_QuestionIsUntrustedLikeTheResult(t *testing.T) {
	cases := []struct {
		name   string
		status v1.SubagentRequestStatus
	}{
		{"an outstanding question is the child's words: untrusted", awaitingStatus(1, "which repo?")},
		{"a final result is the child's words: untrusted", v1.SubagentRequestStatus{
			Phase: v1.SubagentRequestPhaseSucceeded, Result: "opened PR #7",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := &scriptedPoll{states: []v1.SubagentRequestStatus{tc.status}}
			tl := NewDelegateTool(DelegateConfig{
				Namespace: "ns", SessionName: "demo-parent",
				Create:       func(context.Context, *v1.SubagentRequest) error { return nil },
				Poll:         sp.poll,
				PollInterval: time.Millisecond,
				Timeout:      time.Minute,
			})
			res, err := tl.Execute(context.Background(), json.RawMessage(`{"agent":"demo-coder","task":"t"}`), nil)
			require.NoError(t, err)
			assert.False(t, res.Trusted,
				"a child's own words must run through content-guard inspection, never bypass it as platform-authored text")
		})
	}
}

// Brief test 7 (Track 1a regression guard): a single_turn delegation never
// enters the awaiting state, and delegate still blocks to a terminal phase.
func TestDelegate_SingleTurn_StillBlocksThroughRunningToATerminalPhase(t *testing.T) {
	sp := &scriptedPoll{states: []v1.SubagentRequestStatus{
		{Phase: ""},
		{Phase: v1.SubagentRequestPhaseRunning},
		{Phase: v1.SubagentRequestPhaseRunning},
		{Phase: v1.SubagentRequestPhaseSucceeded, Result: "done, here is the summary"},
	}}
	tl := NewDelegateTool(DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Create:       func(context.Context, *v1.SubagentRequest) error { return nil },
		Poll:         sp.poll,
		PollInterval: time.Millisecond,
		Timeout:      time.Minute,
	})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"agent":"demo-coder","task":"do it"}`), nil)
	require.NoError(t, err)
	assert.Equal(t, "done, here is the summary", res.Content, "the terminal result, verbatim and alone")
	assert.False(t, res.IsError)
	assert.GreaterOrEqual(t, sp.count(), 4, "it kept polling through Running rather than returning early")
}

// Brief test 2 (parent half): the reply is delivered to the CHILD named on the
// request, and the call then waits for what the child says next.
func TestReplyToSubagent_DeliversToTheChildThenReturnsTheFinalResult(t *testing.T) {
	sp := &scriptedPoll{states: []v1.SubagentRequestStatus{
		awaitingStatus(1, "which repo?"),
		// Still awaiting exchange 1 right after the send: the child has not
		// woken yet. Returning here would hand the model back the question it
		// just answered.
		awaitingStatus(1, "which repo?"),
		{Phase: v1.SubagentRequestPhaseRunning, ChildRef: &v1.NamespacedRef{Namespace: "ns", Name: "demo-child"}, Exchange: 1},
		{Phase: v1.SubagentRequestPhaseSucceeded, Result: "opened PR #7", Exchange: 1},
	}}
	type sent struct{ ns, name, text string }
	var got []sent
	tl := NewSubagentReplyTool(DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Poll: sp.poll,
		Send: func(_ context.Context, ns, name, text string) error {
			got = append(got, sent{ns, name, text})
			return nil
		},
		PollInterval: time.Millisecond,
		Timeout:      time.Minute,
	})

	res, err := tl.Execute(context.Background(),
		json.RawMessage(`{"delegation":"subreq-demo-parent-x","message":"the second one"}`), nil)
	require.NoError(t, err)
	require.Len(t, got, 1, "exactly one delivery, to exactly one child")
	assert.Equal(t, sent{"ns", "demo-child", "the second one"}, got[0])
	assert.Equal(t, "opened PR #7", res.Content)
	assert.False(t, res.IsError)
	assert.False(t, res.Trusted, "the child's final answer stays untrusted through this path too")
}

// Brief test 3: two exchanges, then a terminal phase — driven entirely through
// the tools a parent actually calls.
func TestDelegateThenTwoReplies_CompletesTheConversation(t *testing.T) {
	sp := &scriptedPoll{states: []v1.SubagentRequestStatus{
		awaitingStatus(1, "question one"),
		awaitingStatus(1, "question one"), // reply #1 re-reads before the child wakes
		awaitingStatus(2, "question two"),
		awaitingStatus(2, "question two"), // reply #2 re-reads before the child wakes
		{Phase: v1.SubagentRequestPhaseSucceeded, Result: "all done", Exchange: 2},
	}}
	cfg := DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Create:       func(context.Context, *v1.SubagentRequest) error { return nil },
		Poll:         sp.poll,
		Send:         func(context.Context, string, string, string) error { return nil },
		PollInterval: time.Millisecond,
		Timeout:      time.Minute,
	}
	delegate, reply := NewDelegateTool(cfg), NewSubagentReplyTool(cfg)
	ctx := context.Background()

	first, err := delegate.Execute(ctx, json.RawMessage(`{"agent":"demo-coder","task":"do it","mode":"chat"}`), nil)
	require.NoError(t, err, "delegate must reach the first question")
	require.Contains(t, first.Content, "question one")

	second, err := reply.Execute(ctx, json.RawMessage(`{"delegation":"subreq-x","message":"answer one"}`), nil)
	require.NoError(t, err, "the first reply must reach the second question")
	assert.Contains(t, second.Content, "question two",
		"the second question, not a re-read of the first")
	assert.NotContains(t, second.Content, "question one")

	final, err := reply.Execute(ctx, json.RawMessage(`{"delegation":"subreq-x","message":"answer two"}`), nil)
	require.NoError(t, err)
	assert.Equal(t, "all done", final.Content)
	assert.False(t, final.IsError, "the conversation ended in a terminal SUCCESS")
}

func TestReplyToSubagent_RefusesWhatItCannotAnswer(t *testing.T) {
	cases := []struct {
		name       string
		status     v1.SubagentRequestStatus
		parentName string
		args       string
		wantErr    string
		wantSends  int
	}{
		{
			name:       "a delegation that is merely running: refused, nothing sent",
			status:     v1.SubagentRequestStatus{Phase: v1.SubagentRequestPhaseRunning},
			parentName: "demo-parent",
			args:       `{"delegation":"subreq-x","message":"hello"}`,
			wantErr:    "is not waiting on you",
		},
		{
			name:       "another session's delegation: refused before anything is published",
			status:     awaitingStatus(1, "which repo?"),
			parentName: "some-other-parent",
			args:       `{"delegation":"subreq-x","message":"hello"}`,
			wantErr:    "is not yours to answer",
		},
		{
			name:       "an awaiting delegation with no child named: refused, not dereferenced",
			status:     v1.SubagentRequestStatus{Phase: v1.SubagentRequestPhaseAwaitingParent, Exchange: 1, Message: "?"},
			parentName: "demo-parent",
			args:       `{"delegation":"subreq-x","message":"hello"}`,
			wantErr:    "names no agent to answer",
		},
		{
			name:       "an empty message: refused, nothing sent",
			status:     awaitingStatus(1, "which repo?"),
			parentName: "demo-parent",
			args:       `{"delegation":"subreq-x","message":"   "}`,
			wantErr:    "message must not be empty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sends := 0
			tl := NewSubagentReplyTool(DelegateConfig{
				Namespace: "ns", SessionName: tc.parentName,
				Poll: func(context.Context, string) (*v1.SubagentRequest, error) {
					return &v1.SubagentRequest{
						Spec:   v1.SubagentRequestSpec{Parent: v1.NamespacedRef{Namespace: "ns", Name: "demo-parent"}},
						Status: tc.status,
					}, nil
				},
				Send:         func(context.Context, string, string, string) error { sends++; return nil },
				PollInterval: time.Millisecond,
				Timeout:      50 * time.Millisecond,
			})
			res, err := tl.Execute(context.Background(), json.RawMessage(tc.args), nil)
			require.NoError(t, err)
			assert.True(t, res.IsError)
			assert.True(t, res.Trusted, "a refusal is platform-authored, so it is not fed to the content guard as agent words")
			assert.Contains(t, res.Content, tc.wantErr)
			assert.Equal(t, tc.wantSends, sends, "a refused reply must publish nothing")
		})
	}
}

func TestReplyToSubagent_SendFailureIsReportedNotSwallowed(t *testing.T) {
	tl := NewSubagentReplyTool(DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Poll: func(context.Context, string) (*v1.SubagentRequest, error) {
			return &v1.SubagentRequest{
				Spec:   v1.SubagentRequestSpec{Parent: v1.NamespacedRef{Namespace: "ns", Name: "demo-parent"}},
				Status: awaitingStatus(1, "which repo?"),
			}, nil
		},
		Send:         func(context.Context, string, string, string) error { return assert.AnError },
		PollInterval: time.Millisecond,
		Timeout:      50 * time.Millisecond,
	})
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"delegation":"subreq-x","message":"hi"}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "could not deliver your answer")
	assert.Contains(t, res.Content, "may be retried")
}

func TestReplyToSubagent_WithNoSendWired_RefusesRatherThanPretending(t *testing.T) {
	tl := NewSubagentReplyTool(DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Poll: func(context.Context, string) (*v1.SubagentRequest, error) {
			return &v1.SubagentRequest{Status: awaitingStatus(1, "?")}, nil
		},
		PollInterval: time.Millisecond,
		Timeout:      50 * time.Millisecond,
	})
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"delegation":"subreq-x","message":"hi"}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "no way to reach a delegated agent")
}
