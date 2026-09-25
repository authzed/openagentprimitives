package runner_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/completion"
	_ "github.com/authzed/openagentprimitives/pkg/agent/completion/kinds/artifactdelivery" // registers artifact-delivered
	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/deliveries"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack" // the fixture's binding kind, resolved through the registry
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// The session fixture's identity. A UID is load-bearing: OwnedBySession matches
// renders by Kind+Name+UID, so a render with no matching UID is invisible to
// the artifact-delivered requirement and the gate under test would never fire.
const (
	gateNS   = "default"
	gateName = "s1"
	gateUID  = types.UID("session-uid-1")
)

// gateFixture is one wired session: the loop, the scripted provider, the
// delivery record the requirement reads, and the recorded bypasses.
type gateFixture struct {
	loop     *runner.Loop
	provider *llmfake.Provider
	store    *turnStore
	client   client.Client
	delivery *deliveries.Store
	bypasses []completion.Bypass
}

// turnStore is the narrow slice of the memory adapter these tests seed and
// read back; aliasing it keeps the fixture signature free of the adapter's
// concrete type spelling.
type turnStore = struct {
	Append func(ctx context.Context, t memory.Turn) error
}

// fixedStateRegistry is a tool.StateRegistry holding exactly the stores a test
// put in it.
type fixedStateRegistry map[string]tool.StateStore

func (r fixedStateRegistry) Get(name string) (tool.StateStore, bool) {
	s, ok := r[name]
	return s, ok
}

// readyRender builds a Ready ArtifactRender owned by the fixture session — the
// "the bytes exist and nothing carried them anywhere" state the
// artifact-delivered requirement exists to catch.
func readyRender(name string) *spiceboxv1alpha1.ArtifactRender {
	return &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: gateNS,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeGroupVersion.String(),
				Kind:       "AgentSession",
				Name:       gateName,
				UID:        gateUID,
			}},
		},
		Spec: spiceboxv1alpha1.ArtifactRenderSpec{Kind: "html", Filename: "report.html"},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase:          spiceboxv1alpha1.ArtifactRenderPhaseReady,
			OutputFilename: "report.html",
			OutputMIME:     "text/html",
		},
	}
}

// newGateFixture wires a channel-attached session whose AgentClass declares
// reqs, carrying renders, driven by script.
//
// The tool list mirrors production for the two tools that matter here: the
// per-session agent_work_complete carrying the same requirement set (so the
// bypass affordance is the real one), and await_user_message with a zero idle
// TTL so its park returns immediately rather than making the test wait.
func newGateFixture(t *testing.T, reqs []string, renders []*spiceboxv1alpha1.ArtifactRender, script []llmfake.Step) *gateFixture {
	t.Helper()

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: gateName, Namespace: gateNS, UID: gateUID, Generation: 1},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			// A HUMAN-facing binding, matching the channel-attached session
			// this fixture describes: artifact-delivered is answered by a
			// respond_to_user call, so it holds only a session offered one.
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "demo-channel", Kind: "slack"},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	objs := []client.Object{sess}
	for _, r := range renders {
		objs = append(objs, r)
	}
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()

	key := memory.NamespacedName{Namespace: gateNS, Name: gateName}
	store := runner.LocalMemoryAdapter(memory.NewLocal(inmem.NewBackend()), key)

	// The fixture owns its state registry rather than materializing the global
	// one, because it needs a direct handle on the deliveries Store: the gate's
	// assertions read what the artifact-delivered requirement saw, and a Store
	// reached only through the registry is not addressable from here.
	delivery := deliveries.NewStore(state.Deps{})
	sctx := &tool.SessionContext{
		Namespace:       gateNS,
		Name:            gateName,
		AgentSessionUID: gateUID,
		K8sClient:       c,
		State:           fixedStateRegistry{"deliveries": delivery},
	}
	_, ok := deliveries.TryFrom(sctx)
	require.True(t, ok, "the fixture's deliveries store must resolve; the requirement reads it")

	f := &gateFixture{provider: llmfake.New(script), client: c, delivery: delivery}

	tools := []tool.Tool{
		meta.NewAgentWorkComplete(meta.CompletionConfig{
			Requirements: reqs,
			RecordBypass: func(_ context.Context, b completion.Bypass) error {
				f.bypasses = append(f.bypasses, b)
				return nil
			},
		}),
		// Zero idle TTL: await returns its Terminal+IdleExit result at once, which
		// is the settle this test drives — not a wait to sit through.
		meta.NewAwait(meta.AwaitConfig{IdleTTL: 0}),
	}

	f.loop = &runner.Loop{
		Provider:        f.provider,
		Memory:          store,
		Status:          runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess)),
		Tools:           tools,
		System:          "you are a test agent",
		UserPrompt:      "write the report",
		ChannelAttached: true,
		SessionContext:  sctx,
		AgentClass: &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: "ac", Namespace: gateNS},
			Spec:       spiceboxv1alpha1.AgentClassSpec{CompletionRequirements: reqs},
		},
		AgentSession: sess,
		Budget: runner.NewBudget(spiceboxv1alpha1.BudgetConfig{
			MaxTurns:    50,
			MaxTokens:   100000,
			MaxDuration: metav1.Duration{Duration: time.Hour},
		}, nil, time.Now()),
		Model:      "claude-test",
		MaxTokens:  1024,
		SessionKey: key,
	}
	// Exposed only so a resume case can seed prior turns.
	f.store = &turnStore{Append: store.Append}
	return f
}

func (f *gateFixture) run(t *testing.T) {
	t.Helper()
	require.NoError(t, f.loop.Run(memory.WithSystemApproval(context.Background(), "test")),
		"Run must not fail the session; the gate refuses a settle, it does not error one")
}

func (f *gateFixture) phase(t *testing.T) string {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, f.client.Get(context.Background(),
		client.ObjectKey{Namespace: gateNS, Name: gateName}, &got), "Get session")
	return got.Status.Phase
}

// lastRequestText flattens every text and tool_result block of the final
// scripted request, so a test can assert on what the model was actually told.
func lastRequestText(t *testing.T, p *llmfake.Provider) string {
	t.Helper()
	reqs := p.Requests()
	require.NotEmpty(t, reqs, "the provider was never called")
	var b strings.Builder
	for _, m := range reqs[len(reqs)-1].Messages {
		for _, blk := range m.Content {
			b.WriteString(blk.Text)
			if blk.ToolResult != nil {
				b.WriteString(blk.ToolResult.Content)
			}
		}
	}
	return b.String()
}

// --- scripted turns ---------------------------------------------------------

func awaitTurn(id string) llm.Response {
	return llm.Response{
		Content: []llm.ContentBlock{{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
			ID: id, Name: "await_user_message", Input: json.RawMessage(`{}`),
		}}},
		StopReason: "tool_use",
	}
}

// proseOnlyTurn is the no-tool_use exit: the model answers in assistant prose
// and calls nothing at all, which no per-tool check can ever see.
func proseOnlyTurn() llm.Response {
	return llm.Response{
		Content:    []llm.ContentBlock{{Type: "text", Text: "Here is my report."}},
		StopReason: "end_turn",
	}
}

func completeTurn(id, extra string) llm.Response {
	return llm.Response{
		Content: []llm.ContentBlock{{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
			ID: id, Name: "agent_work_complete",
			Input: json.RawMessage(`{"summary":"wrote the report"` + extra + `}`),
		}}},
		StopReason: "tool_use",
	}
}

// --- tests ------------------------------------------------------------------

// TestCompletionGateRefusesEverySettleExit is the bug. A session that rendered
// an artifact and never delivered it must not reach a resting phase, no matter
// which door it leaves by — and only one of these doors ran a completion check
// before this gate existed.
//
// Each case scripts the settle attempt first and a bypassed agent_work_complete
// second. With the gate absent the run ends at the settle attempt and the
// provider is called exactly once; with it present the model gets the refusal
// and a second turn. The request count is therefore the load-bearing assertion.
func TestCompletionGateRefusesEverySettleExit(t *testing.T) {
	const bypass = `,"bypass_reason":"the render never came back; a degraded review is still a review"`

	cases := []struct {
		name string
		// seed places prior turns so the exit under test is reachable.
		seed   func(t *testing.T, f *gateFixture)
		script []llmfake.Step
		// wantCalls is how many provider turns a working gate produces.
		wantCalls int
	}{
		{
			name: "await_user_message idle-exit with an undelivered render: refused, not parked",
			script: []llmfake.Step{
				{Resp: awaitTurn("tu_await")},
				{Resp: completeTurn("tu_done", bypass)},
			},
			wantCalls: 2,
		},
		{
			name: "no tool_use at all with an undelivered render: refused, not auto-idled",
			script: []llmfake.Step{
				{Resp: proseOnlyTurn()},
				{Resp: completeTurn("tu_done", bypass)},
			},
			wantCalls: 2,
		},
		{
			// agent_work_complete's own gate refuses and returns a plain IsError
			// result — but await_user_message, dispatched in the same turn,
			// returns Terminal+IdleExit and the loop parks on it. The in-tool gate
			// is defeated by pairing the call it guards with one it does not.
			name: "agent_work_complete refused alongside await in one turn: the yield does not carry the settle",
			script: []llmfake.Step{
				{Resp: llm.Response{
					Content: append(completeTurn("tu_done", "").Content,
						awaitTurn("tu_await").Content...),
					StopReason: "tool_use",
				}},
				{Resp: completeTurn("tu_done_2", bypass)},
			},
			wantCalls: 2,
		},
		{
			name: "resume-park expiry with an undelivered render: refused, loop entered instead of exiting",
			seed: func(t *testing.T, f *gateFixture) {
				t.Helper()
				ctx := memory.WithSystemApproval(context.Background(), "test")
				require.NoError(t, f.store.Append(ctx, memory.Turn{
					Index: 0, Role: "user",
					Content: []memory.ContentBlock{{Type: "text", Text: "write the report"}},
				}), "seed turn 0")
				require.NoError(t, f.store.Append(ctx, memory.Turn{
					Index: 1, Role: "assistant",
					Content: []memory.ContentBlock{{Type: "text", Text: "on it"}},
				}), "seed the trailing assistant turn that makes this a resume with nothing to do")
			},
			// The park expires before any provider call, so a working gate
			// produces the FIRST turn rather than a second one.
			script:    []llmfake.Step{{Resp: completeTurn("tu_done", bypass)}},
			wantCalls: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateFixture(t, []string{"artifact-delivered"},
				[]*spiceboxv1alpha1.ArtifactRender{readyRender("ar-report-1")}, tc.script)
			if tc.seed != nil {
				tc.seed(t, f)
			}
			f.run(t)

			require.Len(t, f.provider.Requests(), tc.wantCalls,
				"the settle must be refused and the model given another turn; one call short means it settled unchecked")
			assert.Contains(t, lastRequestText(t, f.provider), "artifact-delivered",
				"the refusal must name the unmet requirement so the model can act on it")
			assert.Contains(t, lastRequestText(t, f.provider), "ar-report-1",
				"the refusal must name the undelivered render")
			assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, f.phase(t),
				"the bypassed completion still settles: the gate must not wedge a session that says why")
			require.Len(t, f.bypasses, 1, "the bypass must be recorded exactly once")
			assert.Contains(t, f.bypasses[0].Reason, "degraded review",
				"the agent's stated reason is the whole point of the bypass and must be recorded verbatim")
		})
	}
}

// TestCompletionGateLetsAnUnpreparedSessionPark is the constraint that makes
// this subtle. An agent that parks to ask a clarifying question before it has
// produced anything owes nothing yet, and blocking it would be a worse bug than
// the one the gate fixes.
func TestCompletionGateLetsAnUnpreparedSessionPark(t *testing.T) {
	cases := []struct {
		name   string
		script []llmfake.Step
	}{
		{name: "await_user_message", script: []llmfake.Step{{Resp: awaitTurn("tu_await")}}},
		{name: "no tool_use", script: []llmfake.Step{{Resp: proseOnlyTurn()}}},
	}
	for _, tc := range cases {
		t.Run(tc.name+" with nothing prepared: parks freely", func(t *testing.T) {
			// No renders at all: the requirement is declared but not yet due.
			f := newGateFixture(t, []string{"artifact-delivered"}, nil, tc.script)
			f.run(t)

			assert.Len(t, f.provider.Requests(), 1,
				"nothing is outstanding, so the park must not be challenged")
			assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, f.phase(t), "the session parks Idle")
			assert.Empty(t, f.bypasses, "nothing was unmet, so nothing was bypassed")
		})
	}
}

// TestCompletionGateSettlesOnceDelivered: the same session, with the delivery
// recorded, settles through the same doors unchallenged. Without this the
// refusal tests above would pass just as well against a gate that refuses
// everything.
func TestCompletionGateSettlesOnceDelivered(t *testing.T) {
	cases := []struct {
		name   string
		script []llmfake.Step
	}{
		{name: "await_user_message", script: []llmfake.Step{{Resp: awaitTurn("tu_await")}}},
		{name: "no tool_use", script: []llmfake.Step{{Resp: proseOnlyTurn()}}},
	}
	for _, tc := range cases {
		t.Run(tc.name+" after the render was delivered: settles unchallenged", func(t *testing.T) {
			f := newGateFixture(t, []string{"artifact-delivered"},
				[]*spiceboxv1alpha1.ArtifactRender{readyRender("ar-report-1")}, tc.script)
			require.NoError(t, f.delivery.Record(context.Background(),
				deliveries.Item{RenderName: "ar-report-1", ArtifactID: "artifact-1"}),
				"record the delivery respond_to_user would have recorded")

			f.run(t)

			assert.Len(t, f.provider.Requests(), 1, "a delivered round settles on its first attempt")
			assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, f.phase(t), "the session parks Idle")
			assert.Empty(t, f.bypasses, "nothing was unmet, so nothing was bypassed")
		})
	}
}

// TestCompletionGateInertWithoutDeclaredRequirements: registration makes a
// requirement available, never on. A class that declared nothing must settle
// exactly as it did before the gate existed, undelivered render or not.
func TestCompletionGateInertWithoutDeclaredRequirements(t *testing.T) {
	f := newGateFixture(t, nil,
		[]*spiceboxv1alpha1.ArtifactRender{readyRender("ar-report-1")},
		[]llmfake.Step{{Resp: awaitTurn("tu_await")}})
	f.run(t)

	assert.Len(t, f.provider.Requests(), 1, "an ungated class is never challenged")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, f.phase(t), "the session parks Idle")
}
