package httpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
)

// TestManageGoalsRebuildsTheGoalRefusal: the management API answers a
// refusal by status with a reason; the client returns the goal sentinel that
// status names, carrying the reason, so a surface can tell "not allowed"
// from "not found" and show why.
func TestManageGoalsRebuildsTheGoalRefusal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		sentinel error
	}{
		{name: "403: ErrDenied", status: http.StatusForbidden, sentinel: goals.ErrDenied},
		{name: "404: ErrNotFound", status: http.StatusNotFound, sentinel: goals.ErrNotFound},
		{name: "400: ErrInvalid", status: http.StatusBadRequest, sentinel: goals.ErrInvalid},
		{name: "409: ErrConflict", status: http.StatusConflict, sentinel: goals.ErrConflict},
		{name: "412: ErrPlanChanged (the approval named a plan no longer to be approved)", status: http.StatusPreconditionFailed, sentinel: goals.ErrPlanChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/goals/manage", r.URL.Path)
				http.Error(w, "only a manager of this group goal can do that", tc.status)
			}))
			t.Cleanup(srv.Close)
			_, err := New(srv.URL, "tok").ManageGoals(context.Background(), goals.ManageRequest{Operation: goals.ManageAct})
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.sentinel)
			assert.Contains(t, err.Error(), "only a manager of this group goal can do that")
		})
	}
	t.Run("200: the response decodes", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req goals.ManageRequest
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
			assert.NoError(t, json.NewEncoder(w).Encode(goals.ManageResponse{Total: 3, Goals: []goals.GoalSummary{{Resource: "agent_goal:g1", CanPause: true}}}))
		}))
		t.Cleanup(srv.Close)
		out, err := New(srv.URL, "tok").ManageGoals(context.Background(), goals.ManageRequest{Operation: goals.ManageList, Viewer: "v", Namespace: "ns"})
		require.NoError(t, err)
		assert.Equal(t, 3, out.Total)
		assert.Equal(t, []goals.GoalSummary{{Resource: "agent_goal:g1", CanPause: true}}, out.Goals)
	})
}
