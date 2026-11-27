package httpclient

import (
	"context"
	"encoding/json"
	"errors"
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

// TestGoalConflictRebuildsTheNarrowestSentinel: the goal server answers every
// conflict with 409 and the refusal's message. Only a second reply after a
// hand-off may be reported as "already handed off"; a run that is no longer
// current, or anything that is not a 409, must not be.
func TestGoalConflictRebuildsTheNarrowestSentinel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		conflict  bool
		handedOff bool
	}{
		{name: "409 naming the hand-off: ErrReplyHandedOff", status: http.StatusConflict, body: "prepare reply: " + goals.ErrReplyHandedOff.Error(), conflict: true, handedOff: true},
		{name: "409 for any other conflict: ErrConflict only", status: http.StatusConflict, body: goals.ErrConflict.Error(), conflict: true},
		{name: "404: neither", status: http.StatusNotFound, body: "goal or requested source unavailable"},
		{name: "500 quoting the hand-off text: neither (only a 409 is a conflict)", status: http.StatusInternalServerError, body: goals.ErrReplyHandedOff.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, tc.body, tc.status) }))
			t.Cleanup(srv.Close)
			_, err := New(srv.URL, "tok").Goals(context.Background(), "ns", "run", goals.Request{Operation: "prepare_reply"})
			require.Error(t, err)
			rebuilt := GoalConflict(err)
			assert.Equal(t, tc.conflict, errors.Is(rebuilt, goals.ErrConflict))
			assert.Equal(t, tc.handedOff, errors.Is(rebuilt, goals.ErrReplyHandedOff))
			code, ok := StatusCode(err)
			require.True(t, ok)
			assert.Equal(t, tc.status, code)
		})
	}
	assert.NoError(t, GoalConflict(nil))
}
