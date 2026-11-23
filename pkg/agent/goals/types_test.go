package goals

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLifecycleDoesNotAuthorizeExecution(t *testing.T) {
	now := time.Now().UTC()
	g := Goal{ID: "goal-1", Domain: Domain{Namespace: "ns", Owner: "alice", Class: "assistant", ClassUID: "uid"}, Title: "Agenda", Outcome: "Prepare meeting", State: Draft, Revision: 1}
	for _, action := range []string{"activate", "pause", "resume", "revise", "complete"} {
		change := Change{Revision: g.Revision, Action: action}
		if action == "revise" {
			title := "Updated agenda"
			change.Title = &title
		}
		if action == "complete" {
			change.Result = &Result{Summary: "Prepared", Evidence: []string{"artifact:agenda"}}
		}
		next, err := Revise(g, change, now)
		require.NoError(t, err)
		assert.Equal(t, g.Revision+1, next.Revision)
		g = next
	}
	assert.Equal(t, Completed, g.State)
	_, err := Revise(g, Change{Revision: g.Revision, Action: "resume"}, now)
	require.ErrorIs(t, err, ErrInvalid)
}
func TestInvalidLifecycleDoesNotMutateGoal(t *testing.T) {
	g := Goal{Domain: Domain{Namespace: "ns", Owner: "alice", Class: "assistant", ClassUID: "uid"}, Title: "Agenda", Outcome: "Prepare", State: Draft, Revision: 1}
	for _, c := range []Change{{Revision: 1, Action: "pause"}, {Revision: 1, Action: "resume"}, {Revision: 1, Action: "complete"}, {Revision: 1, Action: "revise", ClearDue: true, DueAt: new(time.Time)}, {Revision: 1, Action: "activate", Outcome: new(string)}} {
		_, err := Revise(g, c, time.Now())
		require.ErrorIs(t, err, ErrInvalid)
		assert.Equal(t, Draft, g.State)
		assert.Equal(t, int64(1), g.Revision)
	}
}
