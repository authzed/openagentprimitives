package runner

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// leakageAskingTool raises a real info-leakage share approval through the
// runner host and waits on it, the way the respond-time leakage gate does
// inside a turn. Nobody answers, so it expires after wait.
type leakageAskingTool struct {
	host *runnerHost
	wait time.Duration
}

func (*leakageAskingTool) Name() string                 { return "share_record" }
func (*leakageAskingTool) Kind() tool.Kind              { return tool.KindMeta }
func (*leakageAskingTool) Description() string          { return "" }
func (*leakageAskingTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (*leakageAskingTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (*leakageAskingTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (t *leakageAskingTool) Execute(ctx context.Context, _ json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	reqID, err := t.host.PublishApproval(ctx, leakageAsk([]string{"user:evan@corp.example"}))
	if err != nil {
		return tool.Result{IsError: true, Content: err.Error()}, nil
	}
	approved, _, timedOut, err := t.host.AwaitDecision(ctx, reqID, t.wait)
	if err != nil || !approved {
		return tool.Result{IsError: true, Content: "share refused", ShareDenied: timedOut}, nil
	}
	return tool.Result{Content: "shared"}, nil
}

// groupRunLoop is a channel-attached run whose first turn raises a leakage
// share nobody answers within its approval deadline, and whose second turn
// (reached only if the run carries on) calls agent_work_complete.
func groupRunLoop(t *testing.T, group bool) (*Loop, *llmfake.Provider, *[]goals.RunProposal) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	key := memory.NamespacedName{Namespace: "ns", Name: "s"}
	provider := llmfake.New([]llmfake.Step{
		{Resp: llm.Response{
			Content:    []llm.ContentBlock{{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu_1", Name: "share_record", Input: json.RawMessage(`{}`)}}},
			StopReason: "tool_use", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5},
		}},
		{Resp: llm.Response{
			Content:    []llm.ContentBlock{{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu_2", Name: "agent_work_complete", Input: json.RawMessage(`{"summary":"carried on"}`)}}},
			StopReason: "tool_use", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5},
		}},
	})
	var (
		mu      sync.Mutex
		reports []goals.RunProposal
	)
	l := &Loop{
		Memory:             LocalMemoryAdapter(mem, key),
		Mem:                mem,
		SessionKey:         key,
		StartedByCanonical: identity.CanonicalFromTrusted("user:alice", "test fixture"),
		Engine:             engine.New(engine.Deps{Memory: mem}),
		Provider:           provider,
		Status:             LocalStatusPatcher(),
		Budget:             NewBudget(spiceboxv1alpha1.BudgetConfig{MaxTurns: 5, MaxTokens: 10000, MaxDuration: metav1.Duration{Duration: time.Hour}}, nil, time.Now()),
		System:             "you are a test agent",
		UserPrompt:         "do the thing",
		Model:              "test-model",
		MaxTokens:          1024,
		ChannelAttached:    true,
		ResourceStandings:  testResourceStandings(),
		Approval:           approval.New(),
		ChannelKind:        "slack",
		SpiceDBLookupSubjects: func(context.Context, string, string) ([]string, error) {
			return []string{"user:owner@corp.example"}, nil
		},
		InteractionRequestPublish: func(context.Context, string, string, channelevents.Envelope) error { return nil },
		TimeoutAppliedPublish:     func(context.Context, string, string, channelevents.Envelope) error { return nil },
		GroupGoalRun:              group,
	}
	if group {
		l.GroupRunReport = func(_ context.Context, p goals.RunProposal) error {
			mu.Lock()
			defer mu.Unlock()
			reports = append(reports, p)
			return nil
		}
	}
	// The approval deadline stands in for the terms' ApprovalSeconds, which
	// ConstrainGoalSettings makes the session's approval timeout.
	l.Tools = append(meta.Load(), &leakageAskingTool{host: newRunnerHost(l, hostSession{Namespace: key.Namespace, Name: key.Name}), wait: 20 * time.Millisecond})
	return l, provider, &reports
}

// A leakage card on a group goal run left unanswered past its approval
// deadline ends the run: the runner reports the result blocked, naming the
// unanswered approval, and parks Idle as agent_work_complete does, without
// another model turn. A private goal's run is unchanged: the deny reaches the
// model, which carries on.
func TestGroupRunApprovalExpiryReportsBlocked(t *testing.T) {
	cases := []struct {
		name       string
		group      bool
		modelCalls int
		reported   bool
	}{
		{name: "group run: blocked reported, concluded Idle, no further model turn", group: true, modelCalls: 1, reported: true},
		{name: "private run: nothing reported, the model carries on", group: false, modelCalls: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, provider, reports := groupRunLoop(t, tc.group)
			require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))
			assert.Equal(t, tc.modelCalls, len(provider.Requests()), "model calls")
			assert.True(t, l.Status.LocalIdle(), "the session parks Idle either way")
			if !tc.reported {
				assert.Empty(t, *reports)
				return
			}
			require.Len(t, *reports, 1, "exactly one result is reported")
			p := (*reports)[0]
			assert.Equal(t, "blocked", p.Status)
			assert.Contains(t, p.Summary, "information-sharing approval")
			assert.Contains(t, p.Summary, "no decision")
			require.Len(t, p.Evidence, 1)
			assert.Contains(t, p.Evidence[0], "approval request ")
			assert.NoError(t, p.Validate(), "the report is a proposal the goal server accepts")
		})
	}
}

func TestGroupRunBlockedProposalNamesTheApproval(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		kind, label string
	}{
		{kind: "plan_phase", label: "plan phase approval"},
		{kind: "tool_call", label: "tool permission approval"},
		{kind: "leakage_share", label: "information-sharing approval"},
		{kind: "something_new", label: "something_new approval"},
	}
	for _, tc := range cases {
		t.Run(tc.kind+": summary names "+tc.label, func(t *testing.T) {
			p := groupRunBlockedProposal(expiredApproval{RequestID: "req-1", Kind: tc.kind}, now)
			assert.Equal(t, "blocked", p.Status)
			assert.Contains(t, p.Summary, tc.label)
			assert.Contains(t, p.Summary, "req-1")
			assert.Equal(t, "approval-expired-req-1", p.RequestID, "a retried report is the identical proposal")
			assert.Equal(t, now, p.SubmittedAt)
			assert.NoError(t, p.Validate())
		})
	}
}

// Only the approval's own deadline ends a group run as blocked. A cancel,
// pause or shutdown of the run while it waits (pipeline.IsTimeout also matches
// context.Canceled), or the run's own context running out, is not a human
// leaving the ask unanswered, so nothing is noted and no blocked report follows.
func TestGroupRunApprovalExpiryOnlyOnTheApprovalDeadline(t *testing.T) {
	cases := []struct {
		name  string
		ctx   func() (context.Context, context.CancelFunc)
		noted bool
	}{
		{name: "approval deadline passes on a live run: noted", ctx: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		}, noted: true},
		{name: "run cancelled while awaiting: not noted", ctx: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(5*time.Millisecond, cancel)
			return ctx, cancel
		}},
		{name: "run's own context deadline passes first: not noted", ctx: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 5*time.Millisecond)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, _, reports := groupRunLoop(t, true)
			h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})
			ctx, cancel := tc.ctx()
			defer cancel()
			reqID, err := h.PublishApproval(context.Background(), leakageAsk([]string{"user:evan@corp.example"}))
			require.NoError(t, err)
			// The approval's own deadline is the longer one wherever the run's
			// context ends first.
			wait := 200 * time.Millisecond
			if tc.noted {
				wait = 5 * time.Millisecond
			}
			approved, _, _, _ := h.AwaitDecision(ctx, reqID, wait)
			assert.False(t, approved, "an undecided approval never approves")
			_, noted := l.takeGroupRunApprovalExpired()
			assert.Equal(t, tc.noted, noted)
			assert.Empty(t, *reports, "nothing is reported from the host itself")
		})
	}
}
