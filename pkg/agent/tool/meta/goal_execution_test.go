package meta

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
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
