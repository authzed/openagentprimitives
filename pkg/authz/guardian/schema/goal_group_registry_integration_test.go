//go:build integration

package schema_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// Against real SpiceDB, with the schema the guardian composes for the
// registered kinds: a member_group tuple naming a slack channel's AUDIENCE is
// refused at write time, while the member ref is accepted. The unsafe write is
// unrepresentable, not merely unwritten.
func TestRegisteredGoalGroupLinks_SpiceDBRefusesAudienceOwner(t *testing.T) {
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchemaText(t, endpoint, token, composeRegisteredGoalSchema(t))
	c, err := spicedb.NewClient(endpoint, token, true)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()

	write := func(subjRel string) error {
		return c.Relations().WriteRelationships(ctx, []authz.Relation{{
			ResourceType: "agent_goal_domain", ResourceID: "gg-audience-probe", Relation: "member_group",
			SubjectType: "slack_channel", SubjectID: "C123", SubjectRel: subjRel,
		}})
	}
	assert.NoError(t, write("member"), "the member ref is a valid owner")
	assert.Error(t, write("view"), "the audience ref is refused by the schema")
}
