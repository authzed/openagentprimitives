package goals

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// postAs posts r to the goal server as team/session and returns the status
// and body.
func postAs(t *testing.T, s *Server, r core.Request) (int, string) {
	t.Helper()
	b, err := json.Marshal(r)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/goals/team/session", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// markGoalRun turns team/session into a goal-execution session.
func markGoalRun(t *testing.T, s *Server) {
	t.Helper()
	c, ok := s.Reader.(client.Client)
	require.True(t, ok)
	var sess v1.AgentSession
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "team", Name: "session"}, &sess))
	sess.Spec.GoalExecution = &v1.GoalExecutionReference{GoalID: "goal", OccurrenceID: "occ"}
	require.NoError(t, c.Update(context.Background(), &sess))
}

// TestGoalRunCannotManageGoals: any goal-execution session, private or group,
// is refused every goal-management operation with a reason; reads are not
// refused for being a run.
func TestGoalRunCannotManageGoals(t *testing.T) {
	const reason = "a goal run cannot"
	servers := map[string]func(t *testing.T) *Server{
		"private": func(t *testing.T) *Server { return fixture(t).s },
		"group":   func(t *testing.T) *Server { return newGroupFixture(t).s },
	}
	for kind, build := range servers {
		for _, op := range []string{"create", "update", "request_execution", "request_discovery", "stop_discovery", "get_discovery_policy", "get_discovery_proposal"} {
			t.Run(kind+" run, "+op+": 403 with reason", func(t *testing.T) {
				s := build(t)
				markGoalRun(t, s)
				code, body := postAs(t, s, core.Request{Operation: op})
				assert.Equal(t, http.StatusForbidden, code)
				assert.Contains(t, body, reason)
			})
		}
		t.Run(kind+" run, list: not refused for being a run", func(t *testing.T) {
			s := build(t)
			markGoalRun(t, s)
			_, body := postAs(t, s, core.Request{Operation: "list"})
			assert.NotContains(t, body, reason)
		})
		t.Run(kind+" conversation, create: not refused for being a run", func(t *testing.T) {
			_, body := postAs(t, build(t), core.Request{Operation: "create"})
			assert.NotContains(t, body, reason)
		})
	}
}
