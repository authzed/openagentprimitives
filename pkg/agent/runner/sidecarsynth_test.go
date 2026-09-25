package runner_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	mcpprobe "github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// TestProbeSynthSidecar_ProbeFailure_RecordsUnreachable is the deterministic
// guarantee behind the e2e's mid-session-unreachable ordering case: when the
// live probe fails, ProbeSynthSidecar (a) returns the error to the caller and
// (b) RECORDS Reachable=false + the actual reason on status.sidecarReachability
// rather than failing silently. The e2e owns the observable behavior (tool never
// synthesized, session survives); this owns the status write.
func TestProbeSynthSidecar_ProbeFailure_RecordsUnreachable(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	sp := runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess))

	rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name: "kube", Ref: "kube-tb", Port: 8080, SidecarPodIP: "10.0.0.9",
		RunMode: "separate-pod",
	}
	failing := func(_ context.Context, _ string) ([]mcpprobe.Tool, error) {
		return nil, fmt.Errorf("connection refused")
	}

	tools, err := runner.ProbeSynthSidecar(context.Background(), "http://10.0.0.9:8080", rt, failing, nil, nil, sp, nil)
	require.Error(t, err, "a probe failure must surface as an error to the caller")
	assert.Nil(t, tools, "no tools are synthesized on probe failure")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(sess), &got), "Get after ProbeSynthSidecar")
	require.Len(t, got.Status.SidecarReachability, 1, "the unreachable sidecar must be recorded, not silent")
	sr := got.Status.SidecarReachability[0]
	assert.Equal(t, "kube", sr.Name)
	assert.False(t, sr.Reachable, "a failed probe records Reachable=false")
	assert.NotEmpty(t, sr.Unreachable, "the failure reason must be recorded")
	assert.Contains(t, sr.Unreachable, "connection refused", "the actual probe error is surfaced")
}

// TestProbeSynthSidecar_NilStatusPatcher_NoPanic guards the best-effort
// recording contract: a nil StatusPatcher (recording disabled) must not panic —
// the probe error is still returned to the caller.
func TestProbeSynthSidecar_NilStatusPatcher_NoPanic(t *testing.T) {
	rt := spiceboxv1alpha1.ResolvedSidecarToolbox{Name: "kube", Ref: "kube-tb"}
	failing := func(_ context.Context, _ string) ([]mcpprobe.Tool, error) {
		return nil, fmt.Errorf("boom")
	}
	_, err := runner.ProbeSynthSidecar(context.Background(), "http://x:1", rt, failing, nil, nil, nil, nil)
	require.Error(t, err)
}
