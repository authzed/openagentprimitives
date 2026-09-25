package authz_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// recordingApproverChecker records which permission the gate asked about, so a
// test can assert on the QUESTION rather than only on the answer.
type recordingApproverChecker struct {
	askedPermissions []string
	allow            map[string]bool
}

func (c *recordingApproverChecker) CheckApprove(context.Context, string, string, identity.CanonicalUserID, bool) (bool, error) {
	c.askedPermissions = append(c.askedPermissions, "agentsession#approve")
	return false, nil
}

func (c *recordingApproverChecker) CheckOwnerOnResource(ctx context.Context, resType, resID string, id identity.CanonicalUserID, fc bool) (bool, error) {
	return c.CheckOnResource(ctx, resType, resID, "owner", id, fc)
}

func (c *recordingApproverChecker) CheckOnResource(_ context.Context, _, _, permission string, _ identity.CanonicalUserID, _ bool) (bool, error) {
	c.askedPermissions = append(c.askedPermissions, permission)
	return c.allow[permission], nil
}

// A resource type declares which permission confers standing to approve
// requests against it — approverPermission — and the runner expands exactly
// that when it raises the card. The wire type carries it precisely BECAUSE
// channelsd used to rebuild the subject-set as `#owner`, hardcoding an
// assumption the runner no longer makes.
//
// The click side dropped it again: it rebuilt the ref without the permission
// and checked a literal "owner". So raise-time and click-time disagreed about
// who may approve — and on a type whose approverPermission is maintainer or
// admin, the person the prompt was routed to is refused while an owner nobody
// asked is admitted.
//
// This does NOT touch the deliberate rule the resource branch encodes: any one
// resource owner still vouches, and session standing is still not consulted
// here. Only the permission NAME changes.
func TestCheckApproverAuthorized_ConsultsTheDeclaredPermission(t *testing.T) {
	c := &recordingApproverChecker{allow: map[string]bool{"maintainer": true}}

	ok, err := authz.CheckApproverAuthorized(context.Background(), c, "ns", "sess",
		[]authz.ApproverResourceRef{{Type: "repo", ID: "acme/app", Permission: "maintainer"}},
		identity.CanonicalFromTrusted("alice", "test fixture"))

	require.NoError(t, err)
	assert.True(t, ok, "the declared approverPermission must be the one consulted")
	assert.Contains(t, c.askedPermissions, "maintainer",
		"the gate must ask about the declared permission, not a hardcoded owner")
	assert.NotContains(t, c.askedPermissions, "owner",
		"asking owner as well would admit an owner the type did not nominate as an approver")
}

// An empty Permission keeps the historical meaning — owner — so every existing
// resource ref and every durable record written before this field existed
// behaves exactly as before.
func TestCheckApproverAuthorized_EmptyPermissionStillMeansOwner(t *testing.T) {
	c := &recordingApproverChecker{allow: map[string]bool{"owner": true}}

	ok, err := authz.CheckApproverAuthorized(context.Background(), c, "ns", "sess",
		[]authz.ApproverResourceRef{{Type: "repo", ID: "acme/app"}},
		identity.CanonicalFromTrusted("alice", "test fixture"))

	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, []string{"owner"}, c.askedPermissions)
}

// Quorum is still one: any single resource the clicker qualifies on vouches,
// and each is asked with its OWN declared permission.
func TestCheckApproverAuthorized_AnyOneResourceVouchesWithItsOwnPermission(t *testing.T) {
	c := &recordingApproverChecker{allow: map[string]bool{"admin": true}}

	ok, err := authz.CheckApproverAuthorized(context.Background(), c, "ns", "sess",
		[]authz.ApproverResourceRef{
			{Type: "repo", ID: "acme/one", Permission: "maintainer"},
			{Type: "repo", ID: "acme/two", Permission: "admin"},
		},
		identity.CanonicalFromTrusted("alice", "test fixture"))

	require.NoError(t, err)
	assert.True(t, ok, "one qualifying resource is enough; quorum is 1")
	assert.Equal(t, []string{"maintainer", "admin"}, c.askedPermissions)
}

// No resources still folds to the session approve-set, untouched.
func TestCheckApproverAuthorized_NoResourcesStillUsesSessionApprove(t *testing.T) {
	c := &recordingApproverChecker{}

	_, err := authz.CheckApproverAuthorized(context.Background(), c, "ns", "sess", nil,
		identity.CanonicalFromTrusted("alice", "test fixture"))

	require.NoError(t, err)
	assert.Equal(t, []string{"agentsession#approve"}, c.askedPermissions)
}
