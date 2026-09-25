package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestSandboxToolExecute_PollReadFailureSurfaces asserts that a ToolCall the
// runner can no longer read ends the call instead of spinning on it forever.
//
// The poll goroutine used to `continue` on every Get error with no log, no
// counter and no deadline — and nothing on the sandbox dispatch path sets a
// context deadline (the per-call timeout is written onto ToolCallSpec.Timeout,
// never onto the Go context), so the only exits were a successful terminal read
// or outer session cancellation. A revoked RBAC, a deleted CR or an
// unreachable apiserver therefore blocked the tool, and the agent turn behind
// it, in total silence.
func TestSandboxToolExecute_PollReadFailureSurfaces(t *testing.T) {
	base := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.ToolCall{}).
		Build()
	// Create succeeds; every subsequent read of the ToolCall fails, as it would
	// if the runner's access to the CR were withdrawn mid-call.
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*spiceboxv1alpha1.ToolCall); ok {
				return errors.New("forbidden: toolcalls is forbidden for this service account")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})

	reg := operations.New(nil, nil)
	op := reg.Begin("git-readonly poll failure")

	sess := &tool.SessionContext{
		Namespace:       "default",
		Name:            "sess",
		AgentSessionUID: "uid-poll-1",
		K8sClient:       c,
		BundleSessions:  map[string]string{"code": "sess-code"},
		Operations:      reg,
	}

	stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName:   "code",
		Suffix:       "git",
		ToolspecName: "git-readonly",
		Timeout:      30 * time.Second,
		PollInterval: time.Millisecond,
	})

	args, err := json.Marshal(map[string]any{
		"operation_id": op.ID, "_reason": "poll failure", "args": []string{"status"},
	})
	require.NoError(t, err, "marshal tool args")

	type outcome struct {
		res tool.Result
		err error
	}
	resCh := make(chan outcome, 1)
	go func() {
		r, e := stool.Execute(sandbox.WithIDs(context.Background(), sandbox.IDs{TurnIndex: 1, ToolUseID: "tu-1"}), args, sess)
		resCh <- outcome{r, e}
	}()

	select {
	case got := <-resCh:
		require.NoError(t, got.err, "Execute must report the failure as a result, not a transport error")
		assert.True(t, got.res.IsError, "an unreadable ToolCall must fail the call")
		assert.Contains(t, got.res.Content, "sandbox:", "the result should name the failing subsystem")
	case <-time.After(10 * time.Second):
		t.Fatal("Execute never returned: the poll loop spins on Get errors with no deadline and no give-up")
	}
}
