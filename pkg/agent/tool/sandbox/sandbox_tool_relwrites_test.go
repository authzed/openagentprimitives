package sandbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
)

// fakeRelWriter captures the tuples relwrites.Run hands it, optionally
// returning a canned error to exercise the writer-failure case. Mirrors
// pkg/agent/tool/mcp/dispatch_test.go's fakeRelWriter.
type fakeRelWriter struct {
	mu     sync.Mutex
	tuples []relwrites.ResolvedTuple
	err    error
}

func (f *fakeRelWriter) WriteRelationships(_ context.Context, ts []relwrites.ResolvedTuple) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tuples = append(f.tuples, ts...)
	return f.err
}

// relwritesSpec returns a toolspec declaring one WritesRelationships block
// pinning "cluster:<argv[1]>" to the calling session as debug_target — the
// fixture the Task 5 brief specifies.
func relwritesSpec() *spec.Spec {
	return &spec.Spec{
		Name:             "kubeconfig-gen",
		Version:          "1",
		Toolkit:          spec.ToolkitRef{Name: "kubectl", Revision: "2026-01-01"},
		AllowSubcommands: []string{"get-kubeconfig"},
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

// TestSandboxToolRelWrites covers the three post-Succeeded
// relwrites cases from the Task 5 brief: a successful ToolCall writes the
// declared pin tuple, a failed ToolCall writes nothing, and a writer error
// leaves the tool.Result unchanged (not IsError) but is logged — sandbox
// relwrites failures are NOT load-bearing the way the MCP path's are (see
// evaluateWritesRelationships's doc comment).
func TestSandboxToolRelWrites(t *testing.T) {
	cases := []struct {
		name       string
		writerErr  error
		mutate     func(tc *spiceboxv1alpha1.ToolCall)
		wantTuples []relwrites.ResolvedTuple
	}{
		{
			name: "ToolCall reaches Succeeded: pin tuple written",
			mutate: func(tc *spiceboxv1alpha1.ToolCall) {
				exit := int32(0)
				tc.Status.ExitCode = &exit
				tc.Status.Conditions = []metav1.Condition{{
					Type:               spiceboxv1alpha1.ToolCallConditionSucceeded,
					Status:             metav1.ConditionTrue,
					Reason:             spiceboxv1alpha1.ReasonProcessExited,
					LastTransitionTime: metav1.Now(),
				}}
			},
			wantTuples: []relwrites.ResolvedTuple{{
				Resource: "cluster:prod-example-1",
				Relation: "debug_target",
				Subject:  "agentsession:default/sess",
			}},
		},
		{
			name: "ToolCall reaches Failed: no tuples written",
			mutate: func(tc *spiceboxv1alpha1.ToolCall) {
				tc.Status.Conditions = []metav1.Condition{{
					Type:               spiceboxv1alpha1.ToolCallConditionFailed,
					Status:             metav1.ConditionTrue,
					Reason:             spiceboxv1alpha1.ReasonProcessExited,
					Message:            "exit 1",
					LastTransitionTime: metav1.Now(),
				}}
			},
			wantTuples: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeClient(t)
			reg := operations.New(nil, nil)
			op := reg.Begin("get kubeconfig for cluster")

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
				Spec:         relwritesSpec(),
			})
			stool.SetRelWriter(fw)

			patchToolCallStatus(t, c,
				client.ObjectKey{Namespace: "default", Name: "sess-1-tu-rw"},
				tc.mutate)

			ctx := t.Context()
			body, err := json.Marshal(map[string]any{
				"operation_id": op.ID,
				"_reason":      "debug prod-example-1",
				"args":         []string{"get-kubeconfig", "prod-example-1"},
			})
			require.NoError(t, err, "marshal body")

			res, err := stool.ExecuteWithIDs(ctx, json.RawMessage(body), st, sandbox.IDs{TurnIndex: 1, ToolUseID: "tu_rw"})
			require.NoError(t, err, "ExecuteWithIDs must succeed")

			if tc.wantTuples == nil {
				assert.Empty(t, fw.tuples, "expected no tuples written; got %+v", fw.tuples)
				return
			}
			require.False(t, res.IsError, "unexpected IsError; content=%s", res.Content)
			assert.Equal(t, tc.wantTuples, fw.tuples)
		})
	}
}

// TestSandboxToolRelWritesWriterError verifies the safety
// contract that diverges from the MCP path: a relwrites writer error
// leaves the already-composed tool.Result unchanged (IsError stays
// false — the sandbox ToolCall itself succeeded) but IS logged with
// structured context (tool, session, err), per the no-silent-errors rule.
func TestSandboxToolRelWritesWriterError(t *testing.T) {
	c := newFakeClient(t)
	reg := operations.New(nil, nil)
	op := reg.Begin("get kubeconfig for cluster")

	st := &tool.SessionContext{
		Namespace:      "default",
		Name:           "sess",
		K8sClient:      c,
		BundleSessions: map[string]string{"code": "sess-code"},
		Operations:     reg,
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	wantErr := errors.New("rpc error: code = InvalidArgument desc = object_id regex")
	fw := &fakeRelWriter{err: wantErr}
	stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName:   "code",
		Suffix:       "kubectl",
		ToolspecName: "kubeconfig-gen",
		Timeout:      30 * time.Second,
		PollInterval: 5 * time.Millisecond,
		Spec:         relwritesSpec(),
	})
	stool.SetRelWriter(fw)
	stool.SetLogger(logger)

	patchToolCallStatus(t, c,
		client.ObjectKey{Namespace: "default", Name: "sess-1-tu-rw"},
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

	res, err := stool.ExecuteWithIDs(ctx, json.RawMessage(body), st, sandbox.IDs{TurnIndex: 1, ToolUseID: "tu_rw"})
	require.NoError(t, err, "ExecuteWithIDs must succeed")
	assert.False(t, res.IsError, "writer error must not flip Result to IsError; content=%s", res.Content)
	assert.NotEmpty(t, res.Content, "Result content must be unchanged/non-empty")

	logged := buf.String()
	assert.Contains(t, logged, "object_id regex", "writer error must be logged")
	assert.Contains(t, logged, `"tool":"code_kubectl"`, "log must include the tool name")
	assert.Contains(t, logged, `"session":"sess"`, "log must include the session name")
}

// TestSandboxToolRelWritesWriteOnceConflict verifies the atomic-pin backstop:
// when the SpiceDB Exclusive precondition rejects a second pin, relwrites.Run
// surfaces relwrites.ErrWriteOnceConflict. The sandbox path must log this at
// INFO as an EXPECTED write-once rejection (not the generic failure line), and
// must NOT flip the tool.Result to IsError — the ToolCall itself succeeded.
func TestSandboxToolRelWritesWriteOnceConflict(t *testing.T) {
	c := newFakeClient(t)
	reg := operations.New(nil, nil)
	op := reg.Begin("get kubeconfig for cluster")

	st := &tool.SessionContext{
		Namespace:      "default",
		Name:           "sess",
		K8sClient:      c,
		BundleSessions: map[string]string{"code": "sess-code"},
		Operations:     reg,
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// The writer returns the write-once sentinel, as the real SpiceDBWriter does
	// when the MUST_NOT_MATCH precondition matches (second pin for the session).
	fw := &fakeRelWriter{err: fmt.Errorf("%w: cluster:c2#debug_target@agentsession:default/sess", relwrites.ErrWriteOnceConflict)}
	stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName:   "code",
		Suffix:       "kubectl",
		ToolspecName: "kubeconfig-gen",
		Timeout:      30 * time.Second,
		PollInterval: 5 * time.Millisecond,
		Spec:         relwritesSpec(),
	})
	stool.SetRelWriter(fw)
	stool.SetLogger(logger)

	patchToolCallStatus(t, c,
		client.ObjectKey{Namespace: "default", Name: "sess-1-tu-rw"},
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

	res, err := stool.ExecuteWithIDs(ctx, json.RawMessage(body), st, sandbox.IDs{TurnIndex: 1, ToolUseID: "tu_rw"})
	require.NoError(t, err, "ExecuteWithIDs must succeed")
	assert.False(t, res.IsError, "a write-once rejection must NOT flip the Result to IsError; content=%s", res.Content)

	logged := buf.String()
	assert.Contains(t, logged, "write-once", "write-once rejection must be logged as such")
	assert.NotContains(t, logged, "sandbox relwrites failed", "must NOT log the generic failure line for an expected write-once rejection")
	assert.Contains(t, logged, `"session":"sess"`, "log must include the session name")
}

// relwritesSecretOutputSpec returns a toolspec declaring BOTH a
// stdout-sourced secretOutput and a WritesRelationships block that reaches
// for result.stdoutJSON — the combination
// sandbox_tool_relwrites_stdout_test.go's four tests cannot prove safe on
// their own: those call evaluateWritesRelationships directly and hand it a
// non-empty stdout for a SecretOutput-declaring spec, a shape the real
// caller can never produce, since ExecuteWithIDs clears stdout to "" for
// exactly this case BEFORE calling either post-effect
// (sandbox_tool.go:552-560, see evaluateWritesRelationships's own doc
// comment). This fixture is for the test below, which drives the real path
// instead.
func relwritesSecretOutputSpec() *spec.Spec {
	return &spec.Spec{
		Name:             "kubeconfig-gen-secret",
		Version:          "1",
		Toolkit:          spec.ToolkitRef{Name: "kubectl", Revision: "2026-01-01"},
		AllowSubcommands: []string{"get-kubeconfig"},
		SecretOutput:     &spec.SecretOutputSpec{Name: "token", Source: "stdout"},
		WritesRelationships: []spec.RelationshipWriteSpec{{
			When: "has(result.stdoutJSON)",
			Tuple: spec.RelationshipTupleSpec{
				Resource: `"widget:leaked"`,
				Relation: `"observed_by"`,
				Subject:  `"agentsession:" + session`,
			},
		}},
	}
}

// TestSandboxToolRelWrites_SecretOutputSeesOnlySuccess_EndToEnd closes the
// gap flagged in review: the direct-call tests in
// sandbox_tool_relwrites_stdout_test.go pin the in-function
// SecretOutput==nil guard, but nothing drove the real ExecuteWithIDs path
// with a spec declaring both secretOutput and writesRelationships — so
// nothing proved the CALLER's stdout-clear (the actual protection; see
// evaluateWritesRelationships's doc comment) actually fires for this
// combination. This test drives ExecuteWithIDs for real and asserts the
// writer records nothing.
//
// Mirrors sandbox_tool_observe_test.go's
// TestSandboxToolObserves_SecretOutputSeesOnlySuccess and its
// newObserveSandboxTool/runToolCall shape — that file's own comment records
// the exact lesson this test exists to not re-learn: an earlier fixture that
// omitted the seeded AgentSession made its secretOutput test pass for the
// wrong reason (a "not found" Get error, never reaching the code under test
// at all). Seeding the AgentSession here for the same reason: a
// SecretOutput-declaring spec makes ExecuteWithIDs Get it for the write-once
// fast-fail (sandbox_tool.go ~line 348) BEFORE the ToolCall is even created.
func TestSandboxToolRelWrites_SecretOutputSeesOnlySuccess_EndToEnd(t *testing.T) {
	c := newFakeClient(t, &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sess"},
	})
	reg := operations.New(nil, nil)
	op := reg.Begin("get kubeconfig for cluster")

	st := &tool.SessionContext{
		Namespace:      "default",
		Name:           "sess",
		K8sClient:      c,
		ArtifactClient: &fakeArtifact{contents: map[string]string{"mem://stdout": `{"id":"secret-value"}`}},
		BundleSessions: map[string]string{"code": "sess-code"},
		Operations:     reg,
	}

	fw := &fakeRelWriter{}
	stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName:   "code",
		Suffix:       "kubectl",
		ToolspecName: "kubeconfig-gen-secret",
		Timeout:      30 * time.Second,
		PollInterval: 5 * time.Millisecond,
		Spec:         relwritesSecretOutputSpec(),
	})
	stool.SetRelWriter(fw)

	patchToolCallStatus(t, c,
		client.ObjectKey{Namespace: "default", Name: "sess-1-tu-secret"},
		func(tc *spiceboxv1alpha1.ToolCall) {
			exit := int32(0)
			tc.Status.ExitCode = &exit
			tc.Status.StdoutArtifactRef = "mem://stdout"
			tc.Status.Conditions = []metav1.Condition{{
				Type:               spiceboxv1alpha1.ToolCallConditionSucceeded,
				Status:             metav1.ConditionTrue,
				Reason:             spiceboxv1alpha1.ReasonProcessExited,
				LastTransitionTime: metav1.Now(),
			}}
		})

	ctx, cancel := context.WithTimeout(context.Background(), testCtxTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]any{
		"operation_id": op.ID,
		"_reason":      "get kubeconfig for cluster",
		"args":         []string{"get-kubeconfig", "prod-example-1"},
	})
	require.NoError(t, err, "marshal body")

	res, err := stool.ExecuteWithIDs(ctx, json.RawMessage(body), st, sandbox.IDs{TurnIndex: 1, ToolUseID: "tu_secret"})
	require.NoError(t, err, "ExecuteWithIDs must succeed")
	assert.False(t, res.IsError, "the call itself succeeded; content=%s", res.Content)

	assert.Empty(t, fw.tuples,
		"a secretOutput toolspec must never expose stdout to a relationship write, driven through the real ExecuteWithIDs path")
}
