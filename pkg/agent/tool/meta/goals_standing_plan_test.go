package meta

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// groupStandingRequest is a group request_goal_execution call as a model
// following the published schema writes it.
const groupStandingRequest = `{
	"scope": "group",
	"resource": "agent_goal_domain:d1",
	"requestID": "digest-standing",
	"id": "goal-1",
	"revision": 4,
	"terms": {
		"actionApproval": "standing_plan",
		"dueAt": "2030-01-01T12:00:00Z",
		"expiresAt": "2030-01-20T12:00:00Z",
		"schedule": {"kind": "interval", "timezone": "UTC", "intervalSeconds": 3600, "maxRuns": 3, "runWindowSeconds": 600},
		"allowedOperations": ["respond_to_user"],
		"evidence": ["digest token"],
		"standingPlan": [
			{"id": "read", "label": "Read the digest", "why": "the group's weekly digest",
			 "permissions": [{"handle": "perm:digest:grpdemo_repo", "why": "read it"}],
			 "slots": [{"type": "grpdemo_repo", "id": "acme-widgets", "why": "the group's repository"}]},
			{"id": "post", "label": "Post the digest", "why": "deliver it",
			 "permissions": [{"handle": "perm:execute:agent_goal_execution", "why": "reply and report"}],
			 "requires": [{"phase": "read", "why": "nothing to post before reading"}]}
		]
	}
}`

func TestRequestGoalExecutionSchemaAcceptsAGroupStandingPlan(t *testing.T) {
	var schema jsonschema.Schema
	require.NoError(t, json.Unmarshal((&goalTool{op: "request_execution"}).InputSchema(), &schema))
	resolved, err := schema.Resolve(nil)
	require.NoError(t, err)

	var instance map[string]any
	require.NoError(t, json.Unmarshal([]byte(groupStandingRequest), &instance))
	assert.NoError(t, resolved.Validate(instance), "the published schema accepts a group standing-plan request")

	// The frozen plangate.Plan shape is not what the schema offers.
	frozen := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(groupStandingRequest), &frozen))
	frozen["terms"].(map[string]any)["standingPlan"] = map[string]any{"Phases": []any{map[string]any{"Label": "x"}}}
	assert.Error(t, resolved.Validate(frozen), "the frozen shape is refused by the schema")
}

func TestRequestGoalExecutionCarriesTheAuthoredStandingPlanBesideTheTerms(t *testing.T) {
	var got goals.Request
	call := func(_ context.Context, r goals.Request) (goals.Response, error) {
		got = r
		return goals.Response{}, nil
	}
	tl := &goalTool{op: "request_execution", call: call}
	res, err := tl.Execute(context.Background(), json.RawMessage(groupStandingRequest), &tool.SessionContext{})
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content)

	assert.Equal(t, "group", got.Scope)
	assert.Equal(t, "standing_plan", got.Execution.Terms.ActionApproval)
	assert.Nil(t, got.Execution.Terms.StandingPlan, "terms never carry a caller's plan; the server freezes one into them")
	assert.Equal(t, []goals.StandingPhase{
		{ID: "read", Label: "Read the digest", Why: "the group's weekly digest",
			Permissions: []goals.StandingPermission{{Handle: "perm:digest:grpdemo_repo", Why: "read it"}},
			Slots:       []goals.StandingSlot{{Type: "grpdemo_repo", ID: "acme-widgets", Why: "the group's repository"}}},
		{ID: "post", Label: "Post the digest", Why: "deliver it",
			Permissions: []goals.StandingPermission{{Handle: "perm:execute:agent_goal_execution", Why: "reply and report"}},
			Requires:    []goals.StandingRequires{{Phase: "read", Why: "nothing to post before reading"}}},
	}, got.Execution.StandingPlan)

	// A frozen-shape plan does not decode as authored phases: refused.
	frozen := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(groupStandingRequest), &frozen))
	frozen["terms"].(map[string]any)["standingPlan"] = map[string]any{"Phases": []any{}}
	raw, err := json.Marshal(frozen)
	require.NoError(t, err)
	res, err = tl.Execute(context.Background(), raw, &tool.SessionContext{})
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "terms.standingPlan")
}
