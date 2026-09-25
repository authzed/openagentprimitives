// Pure-Go unit tests (no build tag, no real SpiceDB) pinning the tuple SHAPE
// EnsureWorkshopSubjects writes, and the SHAPE and CONSISTENCY of the
// workshop#close check.
//
// The three tuples go out in ONE request on purpose, the same funnel
// TouchArtifactParent uses: `build` rests on #session while `close` rests on
// #starter + #platform, so a workshop provisioned with only the first would
// refuse its own starter's close with no schema error, no failed reconcile and
// no log line. A write split across call sites is how that gap gets
// reintroduced, so the single request is pinned here.
package spicedb

import (
	"context"
	"errors"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	authzed "github.com/authzed/authzed-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// writtenTuple is a captured relationship reduced to the three strings that
// identify it, so assertions do not pin update ORDER.
type writtenTuple struct{ res, rel, sub string }

// capturedTuples indexes a captured WriteRelationshipsRequest by tuple.
func capturedTuples(t *testing.T, req *v1.WriteRelationshipsRequest) map[writtenTuple]v1.RelationshipUpdate_Operation {
	t.Helper()
	require.NotNil(t, req, "WriteRelationships must have been called")
	got := map[writtenTuple]v1.RelationshipUpdate_Operation{}
	for _, u := range req.GetUpdates() {
		rel := u.GetRelationship()
		res := rel.GetResource()
		sub := rel.GetSubject().GetObject()
		got[writtenTuple{
			res: res.GetObjectType() + ":" + res.GetObjectId(),
			rel: rel.GetRelation(),
			sub: sub.GetObjectType() + ":" + sub.GetObjectId(),
		}] = u.GetOperation()
	}
	return got
}

func TestEnsureWorkshopSubjects_WritesSessionStarterAndPlatformInOneRequest(t *testing.T) {
	fake := &fakePermClient{}
	c := &Client{cl: &authzed.Client{PermissionsServiceClient: fake}}

	require.NoError(t, c.EnsureWorkshopSubjects(context.Background(), "ws-a1b2c3d4e5f6", "default", "builder-s1",
		identity.CanonicalFromTrusted("c4nonical", "test fixture")))

	got := capturedTuples(t, fake.captured)
	require.Len(t, got, 3, "session, starter and platform must go out together; got %+v", got)
	for _, want := range []writtenTuple{
		{"workshop:ws-a1b2c3d4e5f6", "session", "agentsession:default/builder-s1"},
		{"workshop:ws-a1b2c3d4e5f6", "starter", "user:c4nonical"},
		{"workshop:ws-a1b2c3d4e5f6", "platform", "platform:platform"},
	} {
		op, ok := got[want]
		require.True(t, ok, "must write %s#%s@%s; got %+v", want.res, want.rel, want.sub, got)
		assert.Equal(t, v1.RelationshipUpdate_OPERATION_TOUCH, op,
			"%s#%s must be a TOUCH — the controller re-ensures on every reconcile", want.res, want.rel)
	}
}

func TestEnsureWorkshopSubjects_NoStarterRecorded_WritesSessionAndPlatformOnly(t *testing.T) {
	fake := &fakePermClient{}
	c := &Client{cl: &authzed.Client{PermissionsServiceClient: fake}}

	// spec.starterCanonical is +optional. An empty subject id is InvalidArgument
	// to SpiceDB, and it would fail the WHOLE request — taking #session with it,
	// so a workshop nobody is attributed with would never reach Ready. Skipping
	// the one tuple leaves it closable by a platform admin alone: fail closed on
	// the permission, not on provisioning.
	require.NoError(t, c.EnsureWorkshopSubjects(context.Background(), "ws-a1b2c3d4e5f6", "default", "builder-s1",
		identity.CanonicalUserID{}))

	got := capturedTuples(t, fake.captured)
	require.Len(t, got, 2, "no starter recorded: only session and platform; got %+v", got)
	assert.Contains(t, got, writtenTuple{"workshop:ws-a1b2c3d4e5f6", "session", "agentsession:default/builder-s1"})
	assert.Contains(t, got, writtenTuple{"workshop:ws-a1b2c3d4e5f6", "platform", "platform:platform"})
	for k := range got {
		assert.NotEqual(t, "starter", k.rel, "an empty starter must never be written as a subject id")
	}
}

func TestCheckWorkshopClose_ShapeAndConsistency(t *testing.T) {
	fake := &fakeCheckClient{resp: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION}
	c := &Client{cl: &authzed.Client{PermissionsServiceClient: fake}}

	ok, err := c.CheckWorkshopClose(context.Background(), "ws-a1b2c3d4e5f6",
		identity.CanonicalFromTrusted("c4nonical", "test fixture"))
	require.NoError(t, err)
	assert.True(t, ok, "HAS_PERMISSION must answer true")

	req := fake.captured
	require.NotNil(t, req, "CheckPermission must have been called")
	assert.Equal(t, "workshop", req.GetResource().GetObjectType())
	assert.Equal(t, "ws-a1b2c3d4e5f6", req.GetResource().GetObjectId(),
		"the workshop object id is its NAMESPACE name, matching the tuple EnsureWorkshopSubjects writes")
	assert.Equal(t, "close", req.GetPermission())
	assert.Equal(t, "user", req.GetSubject().GetObject().GetObjectType(),
		"close is asked of the PERSON the requesting builder acts for, not of a session")
	assert.Equal(t, "c4nonical", req.GetSubject().GetObject().GetObjectId())

	// The tuples this reads were written by the very reconcile that provisioned
	// the target workshop, so a MinimizeLatency snapshot can predate them and
	// refuse the starter their own workshop.
	require.NotNil(t, req.GetConsistency(), "a check with no explicit consistency defaults to MinimizeLatency server-side")
	assert.True(t, req.GetConsistency().GetFullyConsistent(), "workshop#close MUST be FullyConsistent")
	assert.False(t, req.GetConsistency().GetMinimizeLatency())
}

func TestCheckWorkshopClose_NoPermissionAndErrorsAreDistinct(t *testing.T) {
	t.Run("NO_PERMISSION: false, no error", func(t *testing.T) {
		fake := &fakeCheckClient{resp: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION}
		c := &Client{cl: &authzed.Client{PermissionsServiceClient: fake}}

		ok, err := c.CheckWorkshopClose(context.Background(), "ws-a1b2c3d4e5f6",
			identity.CanonicalFromTrusted("stranger", "test fixture"))
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("transport failure: false AND an error, never a bare false", func(t *testing.T) {
		fake := &fakeCheckClient{err: errors.New("spicedb unavailable")}
		c := &Client{cl: &authzed.Client{PermissionsServiceClient: fake}}

		ok, err := c.CheckWorkshopClose(context.Background(), "ws-a1b2c3d4e5f6",
			identity.CanonicalFromTrusted("c4nonical", "test fixture"))
		require.Error(t, err, "an outage answered as a bare false would read as 'not yours to close'")
		assert.Contains(t, err.Error(), "workshop:ws-a1b2c3d4e5f6#close", "the error must name what was being checked")
		assert.False(t, ok)
	})
}
