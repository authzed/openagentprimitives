package sandbox_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestSandboxTool_StampsToolUseIDLabel locks the producer half of the
// snapshot audit contract: the toolcall controller records the
// tool_dispatch_snapshot entry under an ID that embeds
// tc.Labels[spiceboxv1alpha1.LabelToolUseID]. That label is the ONLY field
// that makes the entry ID unique per dispatch — turnIndex resets to 0 on
// every session resume and the memory scope (namespace/name) is reused
// across recreated sessions that share a deterministic name. If the runner
// does not stamp the label, every snapshot is recorded with an empty
// tool_use ID (`tds-<turn>-<seq>-`), and two runs that reach the same
// (turn, seq) collide append-only. This asserts the ToolCall the runner
// builds carries the RAW LLM tool_use ID under that label.
func TestSandboxTool_StampsToolUseIDLabel(t *testing.T) {
	c := newFakeClient(t)
	reg := operations.New(nil, nil)
	op := reg.Begin("git status")

	st := &tool.SessionContext{
		Namespace:       "default",
		Name:            "sess",
		AgentSessionUID: "uid-test-123",
		K8sClient:       c,
		BundleSessions:  map[string]string{"code": "sess-code"},
		Operations:      reg,
	}

	stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName:   "code",
		Suffix:       "git",
		ToolspecName: "git-readonly",
		Timeout:      30 * time.Second,
		PollInterval: 5 * time.Millisecond,
	})

	// The ToolCall CR name normalizes the tool_use ID (lowercases, strips
	// underscores) but the LABEL must carry the RAW id, because the audit
	// entry ID is derived from the label value and must stay globally unique.
	const rawToolUseID = "toolu_01ABCdefGhIjKlMnOpQrStUv"
	patchToolCallStatus(t, c,
		client.ObjectKey{Namespace: "default", Name: "sess-13-toolu-01abcdefghijklmnopqrstuv"},
		func(tc *spiceboxv1alpha1.ToolCall) {
			exit := int32(0)
			tc.Status.ExitCode = &exit
			tc.Status.Conditions = []metav1.Condition{{
				Type: spiceboxv1alpha1.ToolCallConditionSucceeded, Status: metav1.ConditionTrue,
				Reason: spiceboxv1alpha1.ReasonProcessExited, LastTransitionTime: metav1.Now(),
			}}
		})

	ctx, cancel := context.WithTimeout(context.Background(), testCtxTimeout)
	t.Cleanup(cancel)
	body, err := json.Marshal(map[string]any{
		"operation_id": op.ID,
		"_reason":      "check the working tree",
		"args":         []string{"status"},
	})
	require.NoError(t, err, "marshal body")

	_, err = stool.ExecuteWithIDs(ctx, json.RawMessage(body), st, sandbox.IDs{TurnIndex: 13, ToolUseID: rawToolUseID})
	require.NoError(t, err, "ExecuteWithIDs must succeed")

	var list spiceboxv1alpha1.ToolCallList
	require.NoError(t, c.List(ctx, &list), "list ToolCalls")
	require.Len(t, list.Items, 1, "exactly one ToolCall should have been created")

	got := list.Items[0].Labels[spiceboxv1alpha1.LabelToolUseID]
	assert.Equal(t, rawToolUseID, got,
		"ToolCall must carry the raw tool_use ID under LabelToolUseID so the snapshot audit entry ID stays unique per dispatch")
}
