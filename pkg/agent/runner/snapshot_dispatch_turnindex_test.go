package runner

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// snapshotCapturingTool records the sandbox.IDs (and thus the PreDispatch
// snapshot request) its Execute sees, so a test can assert which turn index
// the runner stamped onto a stateful dispatch's pre-dispatch snapshot.
type snapshotCapturingTool struct {
	mu  sync.Mutex
	ids sandbox.IDs
	ran bool
}

func (f *snapshotCapturingTool) Name() string        { return "writer" }
func (f *snapshotCapturingTool) Kind() tool.Kind     { return tool.KindMCP }
func (f *snapshotCapturingTool) Description() string { return "fake stateful tool" }
func (f *snapshotCapturingTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (f *snapshotCapturingTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Readwrite}
}
func (f *snapshotCapturingTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (f *snapshotCapturingTool) Execute(ctx context.Context, _ json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	ids, _ := sandbox.IDsFromCtx(ctx)
	f.mu.Lock()
	f.ids, f.ran = ids, true
	f.mu.Unlock()
	return tool.Result{Content: "ok"}, nil
}

// TestDispatch_StatefulSnapshotUsesMonotonicMemTurnIndex locks the restart
// contract: the pre-dispatch snapshot's TurnIndex must be the durable,
// monotonic memory turn index (memTurnIndex), NOT the per-Run turnCount that
// resets to 0 on every session resume. The AgentSession restart reconciler
// compares this TurnIndex against the user-chosen CutTurnIndex — a transcript
// index — in AnalyzePostCut (pkg/controllers/agentsession/restart_decide.go);
// if the snapshot carried turnCount, a fork after any resume would select the
// wrong bundle snapshots to restore (a post-cut dispatch whose turnCount fell
// below the cut looks pre-cut, and vice versa).
func TestDispatch_StatefulSnapshotUsesMonotonicMemTurnIndex(t *testing.T) {
	ft := &snapshotCapturingTool{}
	l := &Loop{
		Tools:      []tool.Tool{ft},
		SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"},
	}
	loopWithInjectedExecutor(t, l, pipeline.NewRegistry())

	sess := &tool.SessionContext{Namespace: "default", Name: "disp", AgentSessionUID: "uid-x"}
	uses := []llm.ToolUseBlock{{ID: "tu-1", Name: "writer", Input: json.RawMessage(`{"args":{}}`)}}

	const turnCount = int32(3) // per-Run counter — resets to 0 on resume
	const memTurnIndex = 17    // durable transcript index — monotonic across resume
	_, _ = l.dispatchToolUses(memory.WithSystemApproval(context.Background(), "test"),
		uses, sess, turnCount, memTurnIndex, nil, nil)

	ft.mu.Lock()
	defer ft.mu.Unlock()
	require.True(t, ft.ran, "the stateful tool must have executed so its dispatch context was captured")
	require.NotNil(t, ft.ids.PreDispatch, "a readwrite dispatch must carry a pre-dispatch snapshot request")
	assert.Equal(t, int32(memTurnIndex), ft.ids.PreDispatch.TurnIndex,
		"snapshot TurnIndex must be the monotonic memTurnIndex (survives resume), not the per-Run turnCount")
}
