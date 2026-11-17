package meta

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBoundedReportRechecksAuthorityBeforePublishing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refused bool
	}{{"live consent", false}, {"revoked consent", true}} {
		t.Run(tc.name, func(t *testing.T) {
			published := 0
			checks := 0
			report := newRespondTool(RespondConfig{Capabilities: []string{"text"}, ChannelKind: "fake", NATSSubjectPrefix: "ap.session.team.goal", NATSPublish: func(context.Context, string, []byte) error { published++; return nil }})
			candidates := append(NewGoalTools(nil), report)
			bounded := BoundedGoalTools(candidates, "digest", func(context.Context) error {
				checks++
				if tc.refused {
					return errors.New("revoked")
				}
				return nil
			})
			require.Len(t, bounded, 1)
			assert.True(t, bounded[0].(tool.PipelineRouted).PipelineRouted())
			assert.Equal(t, authz.Readwrite, bounded[0].Permission().StateImpact)
			assert.Equal(t, "agent_goal_execution", bounded[0].Permission().Check.ResourceType)
			result, err := bounded[0].Execute(memory.WithSystemApproval(context.Background(), "test"), json.RawMessage(`{"text":"Stretch now"}`), &tool.SessionContext{Namespace: "team", Name: "goal"})
			require.NoError(t, err)
			assert.Equal(t, tc.refused, result.IsError)
			assert.Equal(t, 1, checks)
			if tc.refused {
				assert.Zero(t, published)
			} else {
				assert.Positive(t, published)
			}
		})
	}
}

func TestBoundedResultReportingRechecksConsent(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "allowed", true: "revoked"}[denied], func(t *testing.T) {
			calls := 0
			bounded := BoundedGoalTools(nil, "consent-digest", func(context.Context) error {
				if denied {
					return errors.New("revoked")
				}
				return nil
			}, func(_ context.Context, req goals.Request) (goals.Response, error) {
				calls++
				require.Equal(t, "report_execution_result", req.Operation)
				require.Equal(t, "reported_success", req.Proposal.Status)
				return goals.Response{}, nil
			})
			require.Len(t, bounded, 1)
			require.Equal(t, "report_goal_result", bounded[0].Name())
			require.Equal(t, "agent_goal_execution", bounded[0].Permission().Check.ResourceType)
			result, err := bounded[0].Execute(context.Background(), json.RawMessage(`{"requestID":"result","status":"reported_success","summary":"Reminder published","evidence":["tool-call:reminder"]}`), &tool.SessionContext{})
			require.NoError(t, err)
			require.Equal(t, denied, result.IsError)
			if denied {
				require.Zero(t, calls)
			} else {
				require.Equal(t, 1, calls)
				require.True(t, result.Trusted)
				require.Contains(t, result.Content, "not verified delivery")
			}
		})
	}
}

func TestObservationOnlyGoalHasNoDeliveryTool(t *testing.T) {
	report := newRespondTool(RespondConfig{Capabilities: []string{"text"}, ChannelKind: "fake"})
	bounded := BoundedGoalToolsForTerms([]tool.Tool{report}, "digest", goals.ExecutionTerms{AllowedOperations: []string{"report_goal_event"}}, func(context.Context) error { return nil }, func(context.Context, goals.Request) (goals.Response, error) { return goals.Response{}, nil })
	require.Len(t, bounded, 1)
	require.Equal(t, "report_goal_result", bounded[0].Name())
}

// namedTool is a catalog tool of the class, outside the goal tool set.
type namedTool struct {
	tool.Tool
	name string
}

func (n namedTool) Name() string { return n.name }

// A group run keeps the class's catalog: only respond_to_user is
// wrapped with the execute check, and report_goal_result is added. A private
// run is narrowed to the reply and plan tools, as before.
func TestBoundedGoalToolsKeepTheCatalogForGroupRuns(t *testing.T) {
	respond := newRespondTool(RespondConfig{Capabilities: []string{"text"}, ChannelKind: "fake"})
	catalog := append([]tool.Tool{respond, namedTool{Tool: respond, name: "gh"}, namedTool{Tool: respond, name: "update_plan"}, namedTool{Tool: respond, name: "query_memory"}}, NewGoalTools(nil)...)
	caller := func(context.Context, goals.Request) (goals.Response, error) { return goals.Response{}, nil }
	ops := []string{"respond_to_user", "report_goal_result"}
	cases := []struct {
		name  string
		terms goals.ExecutionTerms
		want  []string
	}{
		{name: "private: narrowed to reply, plan controls and result", terms: goals.ExecutionTerms{AllowedOperations: ops}, want: []string{"respond_to_user", "update_plan", "report_goal_result"}},
		{name: "group: the class's catalog and goal reads plus result, never goal management", terms: goals.ExecutionTerms{AllowedOperations: ops, Group: &goals.GroupDestination{Channel: "team-chat", ChannelUID: "uid", MemberGroup: "group:g1#member"}}, want: []string{"respond_to_user", "gh", "update_plan", "query_memory", "list_goals", "get_goal", "list_goal_runs", "report_goal_result"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bounded := BoundedGoalToolsForTerms(catalog, "digest", tc.terms, func(context.Context) error { return nil }, caller)
			var names []string
			for _, b := range bounded {
				names = append(names, b.Name())
				if b.Name() == "respond_to_user" || b.Name() == "report_goal_result" {
					require.NotNil(t, b.Permission().Check, "%s checks execute", b.Name())
					assert.Equal(t, "agent_goal_execution", b.Permission().Check.ResourceType)
				}
			}
			assert.Equal(t, tc.want, names)
		})
	}
}
