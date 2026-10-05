package meta

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/stretchr/testify/require"
)

func TestGoalLimitsDiscoveryAndOmittedBounds(t *testing.T) {
	policy := &goals.ExecutionPolicy{DefaultBounds: goals.ExecutionBounds{180, 10, 10000, 90}, MaxBounds: goals.ExecutionBounds{600, 50, 40000, 600}, ServerTime: time.Now().UTC()}
	list := &goalTool{op: "list", call: func(context.Context, goals.Request) (goals.Response, error) {
		return goals.Response{ExecutionPolicy: policy}, nil
	}}
	result, err := list.Execute(context.Background(), json.RawMessage(`{}`), &tool.SessionContext{})
	require.NoError(t, err)
	var response goals.Response
	require.NoError(t, json.Unmarshal([]byte(result.Content), &response))
	require.Equal(t, policy.DefaultBounds, response.ExecutionPolicy.DefaultBounds)
	request := &goalTool{op: "request_execution", call: func(_ context.Context, r goals.Request) (goals.Response, error) {
		require.Zero(t, r.Execution.Terms.Bounds)
		return goals.Response{}, nil
	}}
	var schema struct {
		Properties map[string]struct{ Required []string }
	}
	require.NoError(t, json.Unmarshal(request.InputSchema(), &schema))
	require.NotContains(t, schema.Properties["terms"].Required, "bounds")
	result, err = request.Execute(context.Background(), json.RawMessage(`{"terms":{"dueAt":"2026-10-06T00:00:00Z","expiresAt":"2026-10-07T00:00:00Z","allowedOperations":["respond_to_user"],"evidence":["receipt"]}}`), &tool.SessionContext{})
	require.NoError(t, err)
	require.False(t, result.IsError)
}
