//go:build integration

package spicedb

import (
	"context"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGoalExecutionGrantIsExactOwnedAndRevocable(t *testing.T) {
	c := newIntegrationClient(t)
	ctx := context.Background()
	owner := identity.CanonicalFromTrusted("goal-owner-test", "integration fixture")
	other := identity.CanonicalFromTrusted("goal-other-test", "integration fixture")
	const domainID = "goal-grant-domain"
	const digest = "goal-grant-reviewed-consent"
	ownerRelation := authz.Relation{ResourceType: "agent_goal_domain", ResourceID: domainID, Relation: "owner", SubjectType: "user", SubjectID: owner.String()}
	domainRelation := authz.Relation{ResourceType: "agent_goal_execution", ResourceID: digest, Relation: "domain", SubjectType: "agent_goal_domain", SubjectID: domainID}
	approval := authz.Relation{ResourceType: "agent_goal_execution", ResourceID: digest, Relation: "approved", SubjectType: "user", SubjectID: owner.String(), ExpiresAt: time.Now().Add(time.Minute)}
	require.NoError(t, c.Relations().WriteRelationships(ctx, []authz.Relation{ownerRelation, domainRelation}))
	check := func(id string, user identity.CanonicalUserID, want bool) {
		t.Helper()
		allowed, err := c.CheckOnResource(ctx, "agent_goal_execution", id, "execute", user, true)
		require.NoError(t, err)
		assert.Equal(t, want, allowed)
	}
	check(digest, owner, false)
	require.NoError(t, c.Relations().WriteRelationships(ctx, []authz.Relation{approval}))
	check(digest, owner, true)
	check("different-consent", owner, false)
	check(digest, other, false)
	require.NoError(t, c.Relations().DeleteRelationships(ctx, []authz.Relation{approval}))
	check(digest, owner, false)
	require.NoError(t, c.Relations().WriteRelationships(ctx, []authz.Relation{approval}))
	denied := authz.Relation{ResourceType: "agent_goal_domain", ResourceID: domainID, Relation: "denied", SubjectType: "user", SubjectID: owner.String()}
	require.NoError(t, c.Relations().WriteRelationships(ctx, []authz.Relation{denied}))
	check(digest, owner, false)
}
