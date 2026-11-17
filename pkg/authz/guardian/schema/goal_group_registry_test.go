package schema_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// registeredKindFragments mirrors the guardian controller's baseline: every
// registered kind's static schema contribution.
func registeredKindFragments() []*spiceboxv1alpha1.SpiceDBSchemaFragment {
	var out []*spiceboxv1alpha1.SpiceDBSchemaFragment
	for _, k := range registry.All() {
		if sc, ok := k.(channelkinds.SchemaContributor); ok {
			if f := sc.SpiceDBSchemaFragment(); f != nil {
				out = append(out, f)
			}
		}
	}
	return out
}

// composeRegisteredGoalSchema composes exactly what the guardian controller
// writes for the registered kinds: their fragments, their session links, and
// their group member links.
func composeRegisteredGoalSchema(t *testing.T) string {
	t.Helper()
	io := &fakeSchemaIO{}
	_, err := schema.RunAll(context.Background(), io, nil, registeredKindFragments(), nil, nil,
		registry.GroupMemberLinks(), registry.SessionRelationLinks()...)
	require.NoError(t, err, "compose the registered kinds' schema")
	return io.current
}

// member_group admits MEMBER refs only. An audience ref (slack_channel#view)
// composed in would make a goal's owners as wide as everyone who can see the
// conversation — a public channel's whole workspace — so it must not be a
// type the schema accepts there at all.
func TestRegisteredGoalGroupLinks_MemberGroupAdmitsNoAudienceRef(t *testing.T) {
	got := relationTypesIn(t, composeRegisteredGoalSchema(t), "agent_goal_domain", "member_group")
	assert.Contains(t, got, "slack_channel#member", "the slack member ref is composed in")
	assert.Contains(t, got, "group#member")
	assert.NotContains(t, got, "slack_channel#view", "the slack AUDIENCE ref is never an owner type: %v", got)
}
