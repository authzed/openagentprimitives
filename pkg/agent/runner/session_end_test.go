package runner

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
)

// seToolUse builds a single-tool_use assistant response (local to package
// runner; the runner_test package's toolUseResp is not importable here).
func seToolUse(id, name, input string) llm.Response {
	return llm.Response{StopReason: "tool_use", Content: []llm.ContentBlock{{
		Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: id, Name: name, Input: json.RawMessage(input)},
	}}}
}

// TestRun_FiresSessionEnd_WritesSessionEndAudit drives a tiny Run() to a clean
// completion (kubectl-driven: agent_work_complete → WriteSucceeded) and asserts
// the SessionEnd point fired, writing a session_end audit record carrying the
// terminal reason. This exercises the full firing path: terminal choke point →
// fireSessionEnd → executor → SessionCleanup hook → runnerHost.Audit.
func TestRun_FiresSessionEnd_WritesSessionEndAudit(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	key := memory.NamespacedName{Namespace: "default", Name: "se1"}
	mem := memory.NewLocal(inmem.NewBackend())

	var captured []infoleakageaudit.AuditRecord
	provider := llmfake.New([]llmfake.Step{
		{Resp: seToolUse("tu-1", "agent_work_complete", `{"summary":"done"}`)},
	})

	l := &Loop{
		Provider:   provider,
		Memory:     LocalMemoryAdapter(mem, key),
		Mem:        mem,
		Status:     LocalStatusPatcher(),
		Tools:      meta.Load(),
		System:     "test agent",
		UserPrompt: "do a small thing",
		Budget: NewBudget(spiceboxv1alpha1.BudgetConfig{
			MaxTurns: 10, MaxTokens: 10000, MaxDuration: metav1.Duration{Duration: time.Hour},
		}, nil, time.Now()),
		Model:      "claude-test",
		MaxTokens:  1024,
		SessionKey: key,
		AuditMemoryAppend: func(_ context.Context, rec infoleakageaudit.AuditRecord) error {
			captured = append(captured, rec)
			return nil
		},
	}

	require.NoError(t, l.Run(ctx), "Run should complete via agent_work_complete")

	var end *infoleakageaudit.AuditRecord
	for i := range captured {
		if captured[i].Kind == "session_end" {
			end = &captured[i]
			break
		}
	}
	require.NotNil(t, end, "a session_end audit record must be written on the terminal path")
	assert.Equal(t, "default/se1", end.Session, "session_end audit carries the session ref")
	assert.Equal(t, "completed", end.Details["reason"], "session_end audit carries the completed reason")
}
