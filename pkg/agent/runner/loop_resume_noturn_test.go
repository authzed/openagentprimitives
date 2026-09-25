package runner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
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
