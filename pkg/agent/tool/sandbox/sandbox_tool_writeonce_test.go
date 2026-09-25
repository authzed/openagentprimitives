package sandbox_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
)

// writeOnceSpec mirrors relwritesSpec (sandbox_tool_relwrites_test.go) but
// also declares a secretOutput — the write-once fast-fail only engages when
// the toolspec declares one.
func writeOnceSpec() *spec.Spec {
	return &spec.Spec{
		Name:             "kubeconfig-gen",
		Version:          "1",
		Toolkit:          spec.ToolkitRef{Name: "kubectl", Revision: "2026-01-01"},
		AllowSubcommands: []string{"get-kubeconfig"},
		SecretOutput: &spec.SecretOutputSpec{
			Name:        "kubeconfig",
			Source:      "stdout",
			Description: "admin kubeconfig; expires 1h",
		},
		WritesRelationships: []spec.RelationshipWriteSpec{{
			When: "result.success",
			Tuple: spec.RelationshipTupleSpec{
				Resource: `"cluster:" + args.argv[1]`,
				Relation: `"debug_target"`,
				Subject:  `"agentsession:" + session`,
			},
		}},
	}
}

// TestSandboxTool_SecretOutput_WriteOnce_FastFail verifies the
// ordering-critical runner-side gate (Task 6 layer 1): when the AgentSession's
// status already carries a SatisfiedSecretOutputs entry for the toolspec's
// secretOutput name, ExecuteWithIDs must refuse to execute — returning an
// IsError Result — WITHOUT ever creating the ToolCall CR. Combined with Task
// 5's fakeRelWriter, this also proves the toolspec's WritesRelationships pin
// block never fires for the rejected (second) fetch: the bogus pin for a
// second cluster can only happen if the ToolCall is allowed to run to
// Succeeded, and this test asserts it never is.
func TestSandboxTool_SecretOutput_WriteOnce_FastFail(t *testing.T) {
	c := newFakeClient(t, &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sess"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			SatisfiedSecretOutputs: []spiceboxv1alpha1.SecretOutputCompletion{
				{Name: "kubeconfig", Handle: "so-existing", SecretName: "sess-secret-outputs"},
			},
		},
	})

	reg := operations.New(nil, nil)
	op := reg.Begin("generate kubeconfig for a second cluster")

	st := &tool.SessionContext{
		Namespace:      "default",
		Name:           "sess",
		K8sClient:      c,
		BundleSessions: map[string]string{"code": "sess-code"},
		Operations:     reg,
	}

	fw := &fakeRelWriter{}
	stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName:   "code",
		Suffix:       "kubectl",
		ToolspecName: "kubeconfig-gen",
		Timeout:      30 * time.Second,
		PollInterval: 5 * time.Millisecond,
		Spec:         writeOnceSpec(),
	})
	stool.SetRelWriter(fw)

	ctx := t.Context()
	body, err := json.Marshal(map[string]any{
		"operation_id": op.ID,
		"_reason":      "debug a different cluster",
		"args":         []string{"get-kubeconfig", "other-cluster"},
	})
	require.NoError(t, err, "marshal body")

	res, err := stool.ExecuteWithIDs(ctx, json.RawMessage(body), st, sandbox.IDs{TurnIndex: 1, ToolUseID: "tu_wo"})
	require.NoError(t, err, "ExecuteWithIDs returns a Go error only on wiring failures")

	assert.True(t, res.IsError, "second fetch for an already-satisfied secret-output name must be rejected")
	assert.Contains(t, res.Content, `"kubeconfig"`, "error names the secret-output")
	assert.Contains(t, res.Content, "write-once", "error explains the write-once contract")
	assert.Contains(t, res.Content, "start a new session", "error tells the user how to proceed")

	// No ToolCall CR was created for the rejected call.
	var tc spiceboxv1alpha1.ToolCall
	getErr := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sess-1-tu-wo"}, &tc)
	assert.True(t, apierrors.IsNotFound(getErr), "no ToolCall CR should have been created; got err=%v", getErr)

	// No relwrites tuples were written — the pin block never ran because
	// composeResult/evaluateWritesRelationships never ran.
	assert.Empty(t, fw.tuples, "no relwrites tuples should be written for a fast-failed call")
}

// TestSandboxTool_SecretOutput_WriteOnce_DifferentNameStillExecutes verifies
// the fast-fail is scoped to the toolspec's OWN secretOutput name: a session
// that already satisfied a different secret-output name must still execute
// normally.
func TestSandboxTool_SecretOutput_WriteOnce_DifferentNameStillExecutes(t *testing.T) {
	c := newFakeClient(t, &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sess"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			SatisfiedSecretOutputs: []spiceboxv1alpha1.SecretOutputCompletion{
				{Name: "token", Handle: "so-existing", SecretName: "sess-secret-outputs"},
			},
		},
	})

	reg := operations.New(nil, nil)
	op := reg.Begin("generate kubeconfig")

	st := &tool.SessionContext{
		Namespace:      "default",
		Name:           "sess",
		K8sClient:      c,
		BundleSessions: map[string]string{"code": "sess-code"},
		Operations:     reg,
	}

	stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName:   "code",
		Suffix:       "kubectl",
		ToolspecName: "kubeconfig-gen",
		Timeout:      30 * time.Second,
		PollInterval: 5 * time.Millisecond,
		Spec:         writeOnceSpec(),
	})

	patchToolCallStatus(t, c,
		client.ObjectKey{Namespace: "default", Name: "sess-1-tu-wo2"},
		func(tc *spiceboxv1alpha1.ToolCall) {
			exit := int32(0)
			tc.Status.ExitCode = &exit
			tc.Status.Conditions = []metav1.Condition{{
				Type:               spiceboxv1alpha1.ToolCallConditionSucceeded,
				Status:             metav1.ConditionTrue,
				Reason:             spiceboxv1alpha1.ReasonProcessExited,
				LastTransitionTime: metav1.Now(),
			}}
		})

	ctx := t.Context()
	body, err := json.Marshal(map[string]any{
		"operation_id": op.ID,
		"_reason":      "debug prod-example-1",
		"args":         []string{"get-kubeconfig", "prod-example-1"},
	})
	require.NoError(t, err, "marshal body")

	res, err := stool.ExecuteWithIDs(ctx, json.RawMessage(body), st, sandbox.IDs{TurnIndex: 1, ToolUseID: "tu_wo2"})
	require.NoError(t, err, "ExecuteWithIDs must succeed")
	assert.False(t, res.IsError, "a different already-satisfied name must not block this toolspec's own name; content=%s", res.Content)

	var tc spiceboxv1alpha1.ToolCall
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sess-1-tu-wo2"}, &tc),
		"ToolCall CR should have been created")
}
