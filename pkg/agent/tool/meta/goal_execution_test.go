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
