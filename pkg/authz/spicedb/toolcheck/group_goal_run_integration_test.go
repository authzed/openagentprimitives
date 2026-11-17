//go:build integration

package toolcheck_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// A group goal run acts as its own session; its two delivery tools
// (respond_to_user and report_goal_result) check execute on the execution
// and pass through the run tuple the dispatcher wrote for exactly that
// session, until it expires. Against the production schema.
func TestGroupGoalRunDeliveryToolsAuthorizedByRunTuple(t *testing.T) {
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	cli := newAuthzClient(t, endpoint, token)
	ctx := context.Background()

	const digest = "exec-digest"
	caller := func(context.Context, goals.Request) (goals.Response, error) { return goals.Response{}, nil }
	tools := meta.BoundedGoalToolsForTerms([]tool.Tool{meta.New(meta.RespondConfig{})}, digest, goals.ExecutionTerms{AllowedOperations: []string{"respond_to_user", "report_goal_result"}}, func(context.Context) error { return nil }, caller)
	byName := map[string]authz.Permission{}
	for _, tl := range tools {
		byName[tl.Name()] = tl.Permission()
	}
	require.Contains(t, byName, "respond_to_user")
	require.Contains(t, byName, "report_goal_result")

	grant := func(execID, session string, expires time.Time) {
		t.Helper()
		require.NoError(t, cli.Relations().WriteRelationships(ctx, []authz.Relation{
			{ResourceType: "agent_goal_execution", ResourceID: execID, Relation: "run", SubjectType: "agentsession", SubjectID: session, ExpiresAt: expires},
		}))
	}
	check := func(p authz.Permission, session string) authz.Result {
		t.Helper()
		return toolcheck.Checker{Cli: cli}.CheckToolCall(ctx, p, authz.Inputs{Subject: "agentsession:" + session, RequireFreshest: true})
	}

	grant(digest, "goals/run-1", time.Now().Add(time.Hour))
	for _, name := range []string{"respond_to_user", "report_goal_result"} {
		p := byName[name]
		require.NotNil(t, p.Check, "%s checks execute", name)
		assert.Equal(t, authz.OutcomeAllowed, check(p, "goals/run-1").Outcome, "%s: the run session holds execute", name)
		assert.Equal(t, authz.OutcomeDenied, check(p, "goals/run-2").Outcome, "%s: a different session is refused", name)
	}

	stale := tools[0].Permission()
	stale.Check = &authz.PermissionCheck{ResourceType: "agent_goal_execution", Permission: "execute", ResourceIDExpr: "'stale-digest'"}
	grant("stale-digest", "goals/run-1", time.Now().Add(2*time.Second))
	require.Equal(t, authz.OutcomeAllowed, check(stale, "goals/run-1").Outcome, "precondition: allowed before expiry")
	require.Eventually(t, func() bool { return check(stale, "goals/run-1").Outcome == authz.OutcomeDenied }, 15*time.Second, 250*time.Millisecond, "expired run tuple: refused")
}
