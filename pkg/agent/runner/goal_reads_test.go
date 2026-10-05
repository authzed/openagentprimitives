package runner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGoalReadsRetainTransitiveSources(t *testing.T) {
	source := goals.Source{ResourceType: "document", ResourceID: "restricted", Permission: "view_memory"}
	goal := goals.Goal{Sources: []goals.Source{source}}
	run := goals.Occurrence{Proposal: &goals.RunProposal{Sources: []goals.Source{source}}}
	for _, tc := range []struct {
		name     string
		response goals.Response
	}{
		{"goal", goals.Response{Goal: &goal}},
		{"list", goals.Response{Page: &goals.Page{Goals: []goals.Goal{goal}}}},
		{"run", goals.Response{Run: &run}},
		{"runs", goals.Response{Runs: &goals.RunPage{Runs: []goals.Occurrence{run}}}},
		{"policy", goals.Response{DiscoveryPolicy: &goals.DiscoveryPolicy{Template: goal}}},
		{"proposal", goals.Response{DiscoveryProposal: &goals.DiscoveryProposal{Observation: sessionevents.Observation{Dependencies: []sessionevents.Dependency{{ResourceType: source.ResourceType, ResourceID: source.ResourceID, Permission: source.Permission}}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.response.Resource = "agent_goal_domain:private"
			raw, err := json.Marshal(tc.response)
			require.NoError(t, err)
			decl := (&Loop{}).memoryPoolReadsDecl("get_goal")
			resources, err := decl.ResultResources(string(raw))
			require.NoError(t, err)
			assert.Contains(t, resources, hooks.ToolReadResource{Type: source.ResourceType, ID: source.ResourceID, Permission: source.Permission, Content: string(raw)}, "a new session must inherit the original resource gate before copying this result")
			assert.Contains(t, resources, hooks.ToolReadResource{Type: "agent_goal_domain", ID: "private", Permission: "view_memory", Content: string(raw)})
		})
	}
}

func TestGoalReadMalformedSourceFailsClosed(t *testing.T) {
	raw, err := json.Marshal(goals.Response{Resource: "agent_goal_domain:private", Goal: &goals.Goal{Sources: []goals.Source{{ResourceType: "document", ResourceID: "restricted"}}}})
	require.NoError(t, err)
	_, err = (&Loop{}).memoryPoolReadsDecl("get_goal").ResultResources(string(raw))
	require.Error(t, err)
}

func TestGoalReadDispatchPersistsOriginalResourceTaint(t *testing.T) {
	source := goals.Source{ResourceType: "document", ResourceID: "restricted", Permission: "view_memory"}
	tools := meta.NewGoalTools(func(context.Context, goals.Request) (goals.Response, error) {
		return goals.Response{Resource: "agent_goal_domain:private", Goal: &goals.Goal{Sources: []goals.Source{source}}}, nil
	})
	l, rec := readGateLoop(t, "enforcing", tools...)
	results, _ := l.dispatchToolUses(memory.WithSystemApproval(context.Background(), "test"), []llm.ToolUseBlock{{ID: "read-goal", Name: "get_goal", Input: json.RawMessage(`{"id":"goal-a"}`)}}, &tool.SessionContext{Namespace: "ns", Name: "s1"}, 0, 0, nil, nil)
	require.Len(t, results, 1)
	require.False(t, results[0].IsError)
	require.Len(t, rec.taints(), 2)
	assert.Equal(t, []string{"agent_goal_domain:private#view_memory", "document:restricted#view_memory"}, rec.audienceLookups())
	original := rec.taints()[1]
	assert.Equal(t, source.ResourceType, original.ResourceType)
	assert.Equal(t, source.ResourceID, original.ResourceID)
	assert.Equal(t, source.Permission, original.Permission)
}
