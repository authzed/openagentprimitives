// Pure-Go unit test (no build tag, no real SpiceDB) pinning the SHAPE and the
// CONSISTENCY of the agentidentity#update_credential check.
//
// The consistency half is the load-bearing one. The #platform link that makes
// this permission satisfiable at all is written by the SAME reconcile that
// surfaces the dead credential, so a MinimizeLatency read can legitimately
// land on a snapshot predating it. SpiceDB reports that as NO_PERMISSION,
// which is indistinguishable from "this person is not an admin" -- a
// legitimate admin refused the one action the tuple exists to permit, silently.
// Nothing at the schema, card, or button layer would show it. This is the only
// place that regression can be caught, so it is asserted here.
package spicedb

import (
	"context"
	"errors"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	authzed "github.com/authzed/authzed-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// fakeCheckClient captures the CheckPermission request and returns a
// caller-chosen permissionship/error.
type fakeCheckClient struct {
	v1.PermissionsServiceClient
	captured *v1.CheckPermissionRequest
	resp     v1.CheckPermissionResponse_Permissionship
	err      error
}

func (f *fakeCheckClient) CheckPermission(_ context.Context, in *v1.CheckPermissionRequest, _ ...grpc.CallOption) (*v1.CheckPermissionResponse, error) {
	f.captured = in
	if f.err != nil {
		return nil, f.err
	}
	return &v1.CheckPermissionResponse{Permissionship: f.resp}, nil
}

func TestCheckAgentIdentityUpdateCredential_ShapeAndConsistency(t *testing.T) {
	fake := &fakeCheckClient{resp: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION}
	c := &Client{cl: &authzed.Client{PermissionsServiceClient: fake}}

	ok, err := c.CheckAgentIdentityUpdateCredential(context.Background(),
		"integration-test", "support-bot", identity.CanonicalFromTrusted("admin@example.invalid", "test fixture"))
	require.NoError(t, err)
	assert.True(t, ok, "HAS_PERMISSION must answer true")

	req := fake.captured
	require.NotNil(t, req, "CheckPermission must have been called")
	assert.Equal(t, "agentidentity", req.GetResource().GetObjectType())
	assert.Equal(t, "integration-test/support-bot", req.GetResource().GetObjectId(),
		"the agentidentity object id is <namespace>/<name>, matching the tuple EnsureAgentIdentityPlatform writes")
	assert.Equal(t, "update_credential", req.GetPermission())
	assert.Equal(t, "user", req.GetSubject().GetObject().GetObjectType())
	assert.Equal(t, "admin@example.invalid", req.GetSubject().GetObject().GetObjectId())

	// The whole point. Anything other than FullyConsistent here silently
	// refuses legitimate admins inside SpiceDB's quantization window.
	require.NotNil(t, req.GetConsistency(), "a check with no explicit consistency defaults to MinimizeLatency server-side")
	assert.True(t, req.GetConsistency().GetFullyConsistent(),
		"agentidentity#update_credential MUST be FullyConsistent -- the #platform link is written by the very reconcile that surfaces the dead credential")
	assert.False(t, req.GetConsistency().GetMinimizeLatency(),
		"MinimizeLatency here is a silent refusal of a legitimate admin, not a latency optimization")
}

func TestCheckAgentIdentityUpdateCredential_NoPermissionAndErrorsAreDistinct(t *testing.T) {
	t.Run("NO_PERMISSION: false, no error", func(t *testing.T) {
		fake := &fakeCheckClient{resp: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION}
		c := &Client{cl: &authzed.Client{PermissionsServiceClient: fake}}

		ok, err := c.CheckAgentIdentityUpdateCredential(context.Background(), "ns", "bot", identity.CanonicalFromTrusted("nobody", "test fixture"))
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("transport failure: false AND an error, never a bare false", func(t *testing.T) {
		fake := &fakeCheckClient{err: errors.New("spicedb unavailable")}
		c := &Client{cl: &authzed.Client{PermissionsServiceClient: fake}}

		ok, err := c.CheckAgentIdentityUpdateCredential(context.Background(), "ns", "bot", identity.CanonicalFromTrusted("admin", "test fixture"))
		require.Error(t, err, "swallowing this would make an outage look like a permission denial")
		assert.Contains(t, err.Error(), "agentidentity:ns/bot#update_credential",
			"the error must name what was being checked")
		assert.False(t, ok)
	})
}
