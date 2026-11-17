package runner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// A session woken with NOTHING NEW to process must not call the model.
//
// The failure this pins is not theoretical; it was observed on a live cluster.
// A parked session was woken to serve an agent-defined UI's data bindings, the
// runner replayed a transcript ending with its own last reply, drained an empty
// inbox, and ran a turn anyway. The request therefore ended with an assistant
// message and the provider refused it outright:
//
//	400 invalid_request_error — "This model does not support assistant message
//	prefill. The conversation must end with a user message."
//
// The runner exited into AwaitingRetry, so every retry reproduced it, and the
// dashboard that triggered the wake never got its data.
//
// The bug is older than the wake that exposed it. Channelsd only ever wakes a
// session WITH an inbound message, so this path had never been taken until a
// binding-driven wake asked for a runner with nothing to say.
//
// llmfake.New(nil) is an empty script: any Send at all is a test failure, which
// is exactly the assertion — not "the call succeeded" but "no call was made".
func newResumeLoop(t *testing.T, prior []memory.Turn) (*Loop, *llmfake.Provider) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	key := memory.NamespacedName{Namespace: "default", Name: "resume1"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// Seeded through the SAME adapter the loop reads back with, so the prior
	// transcript is written exactly as a real session's would be — a
	// hand-rolled write could differ in a way that made this pass for the
	// wrong reason.
	seed := LocalMemoryAdapter(mem, key)
	for _, turn := range prior {
		require.NoError(t, seed.Append(ctx, turn), "seed prior turn %d", turn.Index)
	}

	provider := llmfake.New(nil)
	l := &Loop{
		Memory:             LocalMemoryAdapter(mem, key),
		Mem:                mem,
		SessionKey:         key,
		StartedByCanonical: identity.CanonicalFromTrusted("user:alice", "test fixture"),
		Engine:             engine.New(engine.Deps{Memory: mem}),
		Provider:           provider,
		Status:             LocalStatusPatcher(),
		Budget:             NewBudget(spiceboxv1alpha1.BudgetConfig{}, nil, time.Now()),
		AgentClass: &spiceboxv1alpha1.AgentClass{
			Spec: spiceboxv1alpha1.AgentClassSpec{
				Authz: &spiceboxv1alpha1.AuthzBlock{
					ApprovalTimeout: &metav1.Duration{Duration: 50 * time.Millisecond},
				},
			},
		},
	}
	return l, provider
}

func TestResumeWithNothingNewDoesNotCallTheModel(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// A completed exchange: the viewer asked, the agent answered. This is what
	// every parked session's transcript looks like — it ends with the agent,
	// because the agent is what finished last.
	l, provider := newResumeLoop(t, []memory.Turn{
		{Index: 0, Role: "user", Content: []memory.ContentBlock{{Type: "text", Text: "show me the dashboard"}}},
		{Index: 1, Role: "assistant", Content: []memory.ContentBlock{{Type: "text", Text: "here it is"}}},
	})

	runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = l.Run(runCtx)

	assert.Zero(t, len(provider.Requests()),
		"a resume with an empty inbox has nothing to respond to: calling the model sends a "+
			"conversation ending in an assistant message, which the provider rejects outright")
}

// Parking is an IDLE EXIT and must be recorded as one.
//
// The first version of this returned from Run directly, on a comment asserting
// that doing so "is the same IdleExit the await tool produces on the ordinary
// path". It was not, and nothing checked: the ordinary path calls
// idleWithWakeRecheck, which writes the Idle phase and re-checks the inbox for
// a message that landed during the transition. Returning early skipped both,
// so a parked session sat at Pending forever — e2e caught it as
// TestRefusalRecovery_HappyPath timing out waiting for Idle, several layers
// away from the cause.
//
// A session stuck at Pending is worse than one that took a needless turn: the
// phase is what every other surface reads to decide whether the agent is
// working, so it reports busy indefinitely for a runner that has finished.
func TestParkingRecordsAnIdleExit(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")

	l, provider := newResumeLoop(t, []memory.Turn{
		{Index: 0, Role: "user", Content: []memory.ContentBlock{{Type: "text", Text: "show me the dashboard"}}},
		{Index: 1, Role: "assistant", Content: []memory.ContentBlock{{Type: "text", Text: "here it is"}}},
	})
	l.ChannelAttached = true

	runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = l.Run(runCtx)

	require.Zero(t, len(provider.Requests()), "precondition: this run parks rather than turning")
	assert.True(t, l.Status.LocalIdle(),
		"a park must write the Idle phase; without it the session reports busy forever for a runner that has finished")
}

// The negative control, and the line this must not cross. A resume that DOES
// find something new is an ordinary turn and must still run one — otherwise
// the fix would silently stop every woken session from answering the message
// that woke it, which is the whole point of a wake.
func TestResumeWithAnInboxMessageStillRunsATurn(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")

	l, provider := newResumeLoop(t, []memory.Turn{
		{Index: 0, Role: "user", Content: []memory.ContentBlock{{Type: "text", Text: "show me the dashboard"}}},
		{Index: 1, Role: "assistant", Content: []memory.ContentBlock{{Type: "text", Text: "here it is"}}},
		// An inbox turn is what channelsd writes while the session is parked.
		// drainInbox promotes it to a real user turn, so the conversation ends
		// with the viewer and there IS something to answer.
		{Index: 2, Role: "inbox", Content: []memory.ContentBlock{{Type: "text", Text: "what about last week?"}}},
	})

	runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = l.Run(runCtx)

	assert.Positive(t, len(provider.Requests()),
		"a resume carrying a new message must still answer it; suppressing the turn here "+
			"would make every woken session ignore what woke it")
}

// Transcript builders for the completed-round cases. Each returns fresh turns.
func userText(idx int, text string) memory.Turn {
	return memory.Turn{Index: idx, Role: "user", Content: []memory.ContentBlock{{Type: "text", Text: text}}}
}

func toolUse(id, name string) memory.ContentBlock {
	return memory.ContentBlock{Type: "tool_use", ToolUse: &memory.ToolUseBlock{ID: id, Name: name, Input: []byte(`{}`)}}
}

func toolResult(id, content string, isErr bool) memory.ContentBlock {
	return memory.ContentBlock{Type: "tool_result", ToolResult: &memory.ToolResultBlock{ToolUseID: id, Content: content, IsError: isErr}}
}

// completedRound is a transcript whose last round closed with
// agent_work_complete: the agent called the terminal tool and the tool's result
// is the last thing recorded. completeIsError makes it the gate's refusal
// instead, which does NOT close the round.
func completedRound(completeIsError bool) []memory.Turn {
	result := "session complete"
	if completeIsError {
		result = "completion refused: a requirement is not met"
	}
	return []memory.Turn{
		userText(0, "post the digest"),
		{Index: 1, Role: "assistant", Content: []memory.ContentBlock{toolUse("toolu_done", "agent_work_complete")}},
		{Index: 2, Role: "user", Content: []memory.ContentBlock{toolResult("toolu_done", result, completeIsError)}},
	}
}

// runResume runs a resume over prior and returns Run's error and the model
// calls it made.
func runResume(t *testing.T, l *Loop) error {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return l.Run(runCtx)
}

// A round closed by a RECORDED, successful agent_work_complete is at rest even
// though its transcript ends with the tool's result rather than an assistant
// message — for a channel-attached session, the one kind the live loop parks
// Idle after agent_work_complete. Every other shape near it is outstanding work
// and must take its turn.
//
// The parking case is the bug: the result block (role user) slipped past the
// assistant-last check, so a wake that brought nothing made the agent take an
// unprompted turn on "session complete" — and only on schedules where the wake
// landed after the runner reached Idle, which is how an e2e transcript
// came to consume a model call that existed only sometimes.
func TestResumeOntoACompletedRound(t *testing.T) {
	cases := []struct {
		name  string
		prior func() []memory.Turn
		park  bool
	}{
		{
			name:  "successful completion recorded: parks, no model call",
			prior: func() []memory.Turn { return completedRound(false) },
			park:  true,
		},
		{
			name: "successful completion beside an errored sibling: parks (the live loop parks on the completion whatever its siblings returned)",
			prior: func() []memory.Turn {
				return []memory.Turn{
					userText(0, "post the digest"),
					{Index: 1, Role: "assistant", Content: []memory.ContentBlock{
						toolUse("toolu_r", "respond_to_user"), toolUse("toolu_done", "agent_work_complete"),
					}},
					{Index: 2, Role: "user", Content: []memory.ContentBlock{
						toolResult("toolu_r", "delivery failed", true), toolResult("toolu_done", "session complete", false),
					}},
				}
			},
			park: true,
		},
		{
			name:  "refused completion: takes a turn (the round is still open)",
			prior: func() []memory.Turn { return completedRound(true) },
		},
		{
			// A runner that died after appending the assistant turn and before
			// recording any result: nothing in the batch ran. Replay synthesizes
			// "no result recorded" answers, which are not evidence of anything,
			// so parking here would drop the reply and the completion.
			name: "orphan batch [respond_to_user, agent_work_complete] with no recorded result: takes a turn",
			prior: func() []memory.Turn {
				return []memory.Turn{
					userText(0, "post the digest"),
					{Index: 1, Role: "assistant", Content: []memory.ContentBlock{
						toolUse("toolu_r", "respond_to_user"), toolUse("toolu_done", "agent_work_complete"),
					}},
				}
			},
		},
		{
			name: "only the completion's result recorded, its sibling's lost: takes a turn",
			prior: func() []memory.Turn {
				return []memory.Turn{
					userText(0, "post the digest"),
					{Index: 1, Role: "assistant", Content: []memory.ContentBlock{
						toolUse("toolu_r", "respond_to_user"), toolUse("toolu_done", "agent_work_complete"),
					}},
					{Index: 2, Role: "user", Content: []memory.ContentBlock{toolResult("toolu_done", "session complete", false)}},
				}
			},
		},
		{
			name: "authorless user text turn after the completed round: takes a turn",
			prior: func() []memory.Turn {
				return append(completedRound(false), userText(3, "operator notice: the test run ended"))
			},
		},
		{
			name: "inbox message waiting: takes a turn",
			prior: func() []memory.Turn {
				return append(completedRound(false),
					memory.Turn{Index: 3, Role: "inbox", Content: []memory.ContentBlock{{Type: "text", Text: "and last week's?"}}})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, provider := newResumeLoop(t, tc.prior())
			l.ChannelAttached = true

			// The empty fake script fails any Send as a provider error, which the
			// loop records as a terminal status; Run itself errs only when a
			// status write fails, so it is nil on both paths.
			require.NoError(t, runResume(t, l), "Run")
			if tc.park {
				assert.Zero(t, len(provider.Requests()), "nothing new to answer: the resume must not call the model")
				assert.True(t, l.Status.LocalIdle(), "the park must record the Idle phase")
				assert.Nil(t, l.Status.LocalFailure(), "a park is not a failure")
			} else {
				assert.Positive(t, len(provider.Requests()), "outstanding work: the resume must take its turn")
			}
		})
	}
}

// A kubectl session (not channel-attached) does not park after
// agent_work_complete — the live loop writes Succeeded — so a resume onto a
// completed round must not park either: parking there returns with NO terminal
// status at all. It takes its turn, and the loop reaches a terminal write.
func TestResumeOntoACompletedRoundWithoutAChannelStillReachesATerminalStatus(t *testing.T) {
	l, provider := newResumeLoop(t, completedRound(false))

	require.NoError(t, runResume(t, l), "Run")
	assert.Positive(t, len(provider.Requests()), "a kubectl resume onto a completed round takes its turn")
	assert.True(t, l.Status.LocalFailure() != nil || l.Status.LocalResult() != nil,
		"Run must always leave a terminal status; a park with no write leaves the session unsettled")
	assert.False(t, l.Status.LocalIdle(), "a kubectl session has no Idle to park in")
}

// The park is a real wait: an inbound that arrives WHILE parked on a completed
// round is drained and answered.
func TestResumeParkedOnACompletedRoundWakesOnInbound(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	l, provider := newResumeLoop(t, completedRound(false))
	l.ChannelAttached = true

	inbound := make(chan struct{}, 1)
	parked := make(chan struct{})
	l.Tools = []tool.Tool{meta.NewAwait(meta.AwaitConfig{
		IdleTTL:   time.Hour,
		InboundCh: inbound,
		OnYield:   func(context.Context) { close(parked) },
	})}

	done := make(chan error, 1)
	go func() { done <- runResume(t, l) }()

	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("the resume never parked")
	}
	require.Zero(t, len(provider.Requests()), "precondition: parked without a turn")
	require.NoError(t, l.Memory.Append(ctx, memory.Turn{
		Index: 3, Role: "inbox", Content: []memory.ContentBlock{{Type: "text", Text: "and last week's?"}},
	}), "write the inbound while parked")
	inbound <- struct{}{}

	select {
	case err := <-done:
		require.NoError(t, err, "Run")
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the inbound")
	}
	require.Positive(t, len(provider.Requests()), "the inbound that woke the park must be answered")
	assert.Contains(t, describeLast(provider.Requests()), "and last week's?",
		"the drained inbound is what the model is asked about")
}

// describeLast flattens the text of the last request's final message.
func describeLast(reqs []llm.Request) string {
	if len(reqs) == 0 {
		return ""
	}
	msgs := reqs[len(reqs)-1].Messages
	if len(msgs) == 0 {
		return ""
	}
	var out string
	for _, b := range msgs[len(msgs)-1].Content {
		out += b.Text
	}
	return out
}
