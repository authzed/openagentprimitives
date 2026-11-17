package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// A group goal run acts as its own session in every subject mode, whatever
// the class's userless bit and whichever member last spoke in its thread; a
// private goal run and an ordinary session keep the usual precedence.
func TestResolveAuthSubjects_GroupGoalRunActsAsItself(t *testing.T) {
	const self = "agentsession:default/demo-session"
	cases := []struct {
		name             string
		groupRun         bool
		goalExecution    bool
		mode             string
		currentRequester string
		want             string
	}{
		{name: "group run, no human: the run session", groupRun: true, goalExecution: true, mode: "currentRequester", want: self},
		{name: "group run, a member spoke: still the run session", groupRun: true, goalExecution: true, mode: "currentRequester", currentRequester: fixtureRequester, want: self},
		{name: "group run, both mode: the run session alone", groupRun: true, goalExecution: true, mode: "both", currentRequester: fixtureRequester, want: self},
		{name: "flag without a goal execution: ordinary precedence", groupRun: true, mode: "currentRequester", currentRequester: fixtureRequester, want: fixtureRequester},
		{name: "private goal run: ordinary precedence (no subject without a human)", goalExecution: true, mode: "currentRequester", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := sessionWithAnnotations(t, tc.currentRequester, "", "")
			if tc.goalExecution {
				sess.Spec.GoalExecution = &spiceboxv1alpha1.GoalExecutionReference{GoalID: "goal", OccurrenceID: "occ"}
			}
			l := &Loop{AuthzCli: noopAuthzClient{}, GroupGoalRun: tc.groupRun}
			l.ResolveAuthSubjects(sess, classForSubjectResolution(t, tc.mode, false))
			assert.Equal(t, tc.want, l.authSubject.String())
			if tc.want == self {
				assert.Empty(t, l.authSubjects, "no human leg joins a group run's subject set")
				l.advanceRequester(identity.Subject("user:" + fixtureStarter))
				assert.Equal(t, self, l.authSubject.String(), "a member speaking in the thread never becomes the run's principal")
				assert.Empty(t, l.authSubjects)
			}
		})
	}
}
