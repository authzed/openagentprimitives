package runner_test

// A delegated child's outstanding question must survive its pod dying.
//
// status.parentExchange.pending is the ONLY thing that tells the SubagentRequest
// controller a child is waiting on its parent. While it is set the request sits
// at AwaitingParent, the parent's delegate/reply_to_subagent call surfaces the
// question, and AwaitingParentSince runs the per-exchange bound that reclaims a
// child whose parent never answers. Clearing it says "answered" — and the only
// thing that answers a question is a message.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// parkedChildLoop builds the loop a delegated child comes back as after its pod
// was killed mid-park: the question is already recorded and pending on the
// durable AgentSession status (written through the production AskParent path,
// not hand-stamped), and `prior` is the transcript it replays.
//
// Status is a REAL StatusPatcher over the fake client rather than
// LocalStatusPatcher, which no-ops every write — the whole assertion here is
// about what lands on the CR.
func parkedChildLoop(
	t *testing.T, c client.Client, key client.ObjectKey, prior []memory.Turn, script []llmfake.Step,
) (*runner.Loop, *llmfake.Provider) {
	t.Helper()

	sp := runner.NewStatusPatcher(c, key)
	n, err := sp.AskParent(context.Background(), "which of the two repos should I open the PR against?")
	require.NoError(t, err, "seed the pending question through the same writer ask_parent uses")
	require.Equal(t, int64(1), n, "precondition: the seeded question is exchange 1")

	mem := memory.NewLocal(inmem.NewBackend())
	mk := memory.NamespacedName{Namespace: key.Namespace, Name: key.Name}
	ctx := memory.WithSystemApproval(context.Background(), "test")
	seed := runner.LocalMemoryAdapter(mem, mk)
	for _, turn := range prior {
		require.NoError(t, seed.Append(ctx, turn), "seed prior turn %d", turn.Index)
	}

	provider := llmfake.New(script)
	return &runner.Loop{
		Memory:             runner.LocalMemoryAdapter(mem, mk),
		Mem:                mem,
		SessionKey:         mk,
		StartedByCanonical: identity.CanonicalFromTrusted("user:alice", "test fixture"),
		Engine:             engine.New(engine.Deps{Memory: mem}),
		Provider:           provider,
		Status:             sp,
		Budget:             runner.NewBudget(spiceboxv1alpha1.BudgetConfig{}, nil, time.Now()),
		AgentClass: &spiceboxv1alpha1.AgentClass{
			Spec: spiceboxv1alpha1.AgentClassSpec{
				Authz: &spiceboxv1alpha1.AuthzBlock{
					ApprovalTimeout: &metav1.Duration{Duration: 50 * time.Millisecond},
				},
			},
		},
	}, provider
}

// A pod killed while the child was parked in ask_parent comes back with the
// question STILL unanswered, and Run must leave it standing.
//
// Run used to clear pending unconditionally at start, on the premise that a Run
// that is working must have been woken by an answer. A restart is the
// counter-example, and it is an ordinary one — status.runnerRestarts exists
// because OOM kills, drains and evictions happen. Clearing it here told the
// controller the child had resumed, which moved the request AwaitingParent →
// Running and dropped AwaitingParentSince: the parent never saw the question,
// polled its full ceiling, and the bound that would have reclaimed the child
// was gone. For a `task` child the re-ask is then refused on budget, so the
// question is lost outright.
func TestRun_RestartWhileParked_LeavesTheChildsQuestionOutstanding(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	key := client.ObjectKeyFromObject(sess)

	// The transcript a killed child replays: it ends with the child's own last
	// assistant turn, and nothing new arrived while the pod was gone.
	l, provider := parkedChildLoop(t, c, key, []memory.Turn{
		{Index: 0, Role: "user", Content: []memory.ContentBlock{{Type: "text", Text: "open a PR against the docs repo"}}},
		{Index: 1, Role: "assistant", Content: []memory.ContentBlock{{Type: "text", Text: "asking the delegating agent"}}},
	}, nil)

	ctx, cancel := context.WithTimeout(memory.WithSystemApproval(context.Background(), "test"), 5*time.Second)
	defer cancel()
	require.NoError(t, l.Run(ctx), "the restarted runner parks rather than failing")

	require.Zero(t, len(provider.Requests()),
		"precondition: this is a resume with nothing new, so no turn runs and nothing could have answered")

	pe := readExchange(t, c, key)
	require.NotNil(t, pe, "the durable record of the question must survive the restart")
	assert.True(t, pe.Pending,
		"a restart is not an answer: clearing pending here reports the child as resumed, so the parent never sees the question and the per-exchange bound stops running")
	assert.Equal(t, int64(1), pe.Exchange, "the exchange number must stay put for the parent to match a reply against")
}

// The negative control, and the line the fix must not cross: when the parent's
// answer IS there, the resume drain places it as a user turn and the pending
// flag clears — otherwise the request would sit at AwaitingParent forever for a
// child that had already been answered and gone back to work.
func TestRun_ResumeDrainingTheParentsAnswer_ClearsTheOutstandingQuestion(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	key := client.ObjectKeyFromObject(sess)

	// An "inbox"-role turn is what channelsd writes when it delivers the
	// parent's reply_to_subagent message to a child whose pod is gone.
	l, provider := parkedChildLoop(t, c, key, []memory.Turn{
		{Index: 0, Role: "user", Content: []memory.ContentBlock{{Type: "text", Text: "open a PR against the docs repo"}}},
		{Index: 1, Role: "assistant", Content: []memory.ContentBlock{{Type: "text", Text: "asking the delegating agent"}}},
		{Index: 2, Role: "inbox", Content: []memory.ContentBlock{{Type: "text", Text: "the docs repo, not the site one"}}},
	}, []llmfake.Step{{Resp: llm.Response{
		Content:    []llm.ContentBlock{{Type: "text", Text: "on it"}},
		StopReason: "end_turn",
	}}})

	ctx, cancel := context.WithTimeout(memory.WithSystemApproval(context.Background(), "test"), 5*time.Second)
	defer cancel()
	_ = l.Run(ctx)

	require.Positive(t, len(provider.Requests()),
		"precondition: the drained answer is a real turn, so the model is called")

	pe := readExchange(t, c, key)
	require.NotNil(t, pe, "the exchange record is kept; only the flag moves")
	assert.False(t, pe.Pending,
		"the answer arrived and drained, so the delegation must stop reporting the child as awaiting its parent")
	assert.Equal(t, int64(1), pe.Exchange,
		"the number stays monotonic so the child's NEXT question is distinguishable from this one")
}
