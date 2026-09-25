// Pure-Go unit test (no build tag, no real SpiceDB) pinning the tuple SHAPE
// written by EnsureAgentIdentityPlatform:
//
//	agentidentity:<ns>/<name>#platform@platform:platform
//
// That single tuple is the ONLY thing that makes agentidentity#update_credential
// satisfiable: the permission is `editor + platform->can_admin`, `editor` ships
// deliberately unpopulated, and the platform arm resolves only through this
// link. A regression that changes the object id shape, the relation name, or
// the subject is invisible at the schema and UI layers — the card publishes,
// the button renders, and every click is refused — so it is pinned here at the
// single write funnel, exactly as the artifact#platform link is.
package spicedb

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	authzed "github.com/authzed/authzed-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureAgentIdentityPlatform_WritesPlatformLink(t *testing.T) {
	fake := &fakePermClient{}
	c := &Client{cl: &authzed.Client{PermissionsServiceClient: fake}}

	require.NoError(t, c.EnsureAgentIdentityPlatform(context.Background(), "integration-test", "support-bot"))
	require.NotNil(t, fake.captured, "WriteRelationships must have been called")

	updates := fake.captured.GetUpdates()
	require.Len(t, updates, 1, "EnsureAgentIdentityPlatform must issue exactly one update (#platform)")

	u := updates[0]
	rel := u.GetRelationship()
	res := rel.GetResource()
	sub := rel.GetSubject().GetObject()

	assert.Equal(t, "agentidentity", res.GetObjectType())
	assert.Equal(t, "integration-test/support-bot", res.GetObjectId(),
		"the agentidentity object id is <namespace>/<name>, matching the agentsession convention")
	assert.Equal(t, "platform", rel.GetRelation())
	assert.Equal(t, "platform", sub.GetObjectType())
	assert.Equal(t, platformObjectID, sub.GetObjectId(), "must point at the singleton platform object")
	assert.Equal(t, v1.RelationshipUpdate_OPERATION_TOUCH, u.GetOperation(),
		"must be a TOUCH: the reconciler re-writes this on every reconcile, so CREATE would error on the second pass")
}
