//go:build integration

package goals

import (
	"context"
	"testing"
	"time"

	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/testspicedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGoalAuthorityWithRealSpiceDB(t *testing.T) {
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	cl, err := spicedb.NewClient(endpoint, token, true)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cl.Close()) })
	f := fixture(t)
	f.proof(t, "alice")
	f.s.Auth = cl
	ctx := context.Background()
	alice := identity.CanonicalFromTrusted("alice", "test")
	require.NoError(t, cl.TouchStartedBy(ctx, "team", "session", alice))
	status, r := f.call(t, "token", core.Request{Operation: "list"})
	require.Equal(t, 200, status)
	create := core.Request{Operation: "create", Resource: r.Resource, Create: core.CreateRequest{RequestID: "one", Title: "Agenda", Outcome: "Prepare meeting"}}
	status, r = f.call(t, "token", create)
	require.Equal(t, 200, status)
	assert.Equal(t, core.Draft, r.Goal.State)
	// Being the goal owner does not confer standing execution consent.
	ok, err := cl.CheckOnResource(ctx, ResourceType, r.Goal.Domain.ID(), "execute", alice, true)
	require.NoError(t, err)
	assert.False(t, ok)
	// The framework-owned pin cannot be rebound to another employee's domain.
	id, err := authz.NewObjectID("other-domain", nil)
	require.NoError(t, err)
	err = cl.GrantSlots(ctx, "team", "session", []authz.SlotBinding{{ResourceType: ResourceType, ResourceID: id, Permission: "write_memory", Occupancy: authz.SlotOccupancySingle, Rebind: authz.SlotRebindNever}}, authz.SlotGrantExpiry(time.Now(), 0))
	require.Error(t, err)
	require.NoError(t, cl.Relations().WriteRelationships(ctx, []authz.Relation{{ResourceType: ResourceType, ResourceID: r.Goal.Domain.ID(), Relation: "denied", SubjectType: "user", SubjectID: "alice"}}))
	status, _ = f.call(t, "token", create)
	assert.Equal(t, 404, status, "revocation wins even on replay")
}
