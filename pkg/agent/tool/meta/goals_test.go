package meta

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

func goalSchema(t *testing.T, op string) map[string]any {
	t.Helper()
	var schema map[string]any
	require.NoError(t, json.Unmarshal((&goalTool{op: op}).InputSchema(), &schema))
	return schema["properties"].(map[string]any)
}

func TestEveryGoalOpTakesAScope(t *testing.T) {
	for _, gt := range NewGoalTools(nil) {
		op := gt.(*goalTool).op
		t.Run(op, func(t *testing.T) {
			scope, ok := goalSchema(t, op)["scope"].(map[string]any)
			require.True(t, ok, "%s has a scope argument", gt.Name())
			assert.ElementsMatch(t, []any{"private", "group"}, scope["enum"])
		})
	}
}

func TestUpdateGoalManagesManagers(t *testing.T) {
	props := goalSchema(t, "update")
	actions := props["action"].(map[string]any)["enum"].([]any)
	assert.Contains(t, actions, "add_manager")
	assert.Contains(t, actions, "remove_manager")
	assert.Contains(t, props, "manager")

	var got goals.Request
	update := &goalTool{op: "update", call: func(_ context.Context, r goals.Request) (goals.Response, error) {
		got = r
		return goals.Response{}, nil
	}}
	_, err := update.Execute(context.Background(), json.RawMessage(`{"scope":"group","resource":"agent_goal_domain:d","requestID":"r","id":"g","revision":2,"action":"add_manager","manager":"bob"}`), &tool.SessionContext{})
	require.NoError(t, err)
	assert.Equal(t, "group", got.Scope)
	assert.Equal(t, "add_manager", got.Change.Action)
	require.NotNil(t, got.Change.Manager)
	assert.Equal(t, "bob", *got.Change.Manager)
	assert.Empty(t, got.TurnRef, "the turn is stamped by the runner, never taken from the model")
}

func TestScopeReachesTheRequestForEveryOp(t *testing.T) {
	for _, gt := range NewGoalTools(nil) {
		op := gt.(*goalTool).op
		t.Run(op, func(t *testing.T) {
			var got goals.Request
			g := &goalTool{op: op, call: func(_ context.Context, r goals.Request) (goals.Response, error) {
				got = r
				return goals.Response{}, nil
			}}
			_, err := g.Execute(context.Background(), json.RawMessage(`{"scope":"group"}`), &tool.SessionContext{})
			require.NoError(t, err)
			assert.Equal(t, "group", got.Scope)
		})
	}
}

func TestGoalDescriptionExplainsGroupGoals(t *testing.T) {
	d := (&goalTool{op: "create"}).Description()
	assert.Contains(t, d, "scope=group")
	assert.Contains(t, d, "act as the agent")
	assert.Contains(t, d, "manager")
}

// Group-scope writes are authorized by the goal server against the turn's
// author; the dispatch check would ask about the wrong person (the latest
// speaker) on a domain the creator does not manage yet.
func TestGroupScopeWritesAreAuthorizedByTheGoalServer(t *testing.T) {
	for _, gt := range NewGoalTools(nil) {
		if !IsGoalWrite(gt.Name()) {
			continue
		}
		t.Run(gt.Name(), func(t *testing.T) {
			vs := gt.PermissionVariants()
			got, matched, err := authz.ResolveVariant(vs, map[string]any{"scope": "group", "resource": "agent_goal_domain:d"})
			require.NoError(t, err)
			require.True(t, matched)
			assert.Equal(t, authz.Readwrite, got.StateImpact, "a group write is still a write to every impact consumer")
			require.NotNil(t, got.Check)
			assert.Equal(t, GoalResourceType, got.Check.ResourceType)
			assert.Equal(t, "pause", got.Check.Permission, "dispatch admits any member; the goal server decides the rest")
			assert.Equal(t, gt.Permission().Check.ResourceIDExpr, got.Check.ResourceIDExpr)

			for _, args := range []map[string]any{{"resource": "agent_goal_domain:d"}, {"scope": "private", "resource": "agent_goal_domain:d"}} {
				_, matched, err = authz.ResolveVariant(vs, args)
				require.NoError(t, err)
				assert.False(t, matched, "private scope keeps the static manage check: %v", args)
			}
			assert.Equal(t, "manage", gt.Permission().Check.Permission)
		})
	}
}
