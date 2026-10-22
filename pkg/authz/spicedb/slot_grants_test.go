package spicedb

// Coverage for the slot-grant/slot-pin client surface added alongside the
// verbatim lifecycle copy (slot_grants.go): ListSlotPins's client-side filter,
// CopySlotTuples's plain batched TOUCH, and DeleteSlotGrants's inclusion of
// pins in teardown. Uses the same in-process gRPC PermissionsService double as
// writer_test.go and slot_pin_test.go (newRecordingWriterClient) rather than
// mocking the delegate.

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// grantTuple builds a slot_grant_<permission> relationship as
// ReadRelationships would stream it back, for scripting
// recordingWriteServer.readRels. Mirrors slot_pin_test.go's pinTuple.
func grantTuple(resourceType, resourceID, permission string, scope authz.SessionRef) *v1.Relationship {
	return &v1.Relationship{
		Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID},
		Relation: authz.SlotGrantRelationName(permission),
		Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "agentsession", ObjectId: scope.String()}},
	}
}

// TestListSlotPins_FiltersOutGrantsAndOtherSubjectRelations: the subject-only
// filter (slotGrantsOfSession) returns every relation naming this session as
// subject, so ListSlotPins must keep only the slot_pin tuple and drop the
// slot_grant_* ones client-side — the mirror image of ListSlotGrants dropping
// the pin.
func TestListSlotPins_FiltersOutGrantsAndOtherSubjectRelations(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	c := &Client{cl: cl}
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}
	srv.readRels = []*v1.Relationship{
		grantTuple("crm_company", "acme", "contact_access", scope),
		pinTuple("git_repo", "acme/app", scope),
	}

	pins, err := c.ListSlotPins(context.Background(), "ns", "s")
	require.NoError(t, err)
	require.Len(t, pins, 1, "the grant must be filtered out")
	assert.Equal(t, "git_repo", pins[0].ResourceType)
	assert.Equal(t, "acme/app", pins[0].ResourceID)
	assert.Equal(t, authz.SlotPinRelationName, pins[0].Relation)
	assert.Equal(t, "agentsession", pins[0].SubjectType)
	assert.Equal(t, "ns/s", pins[0].SubjectID)

	readReq := srv.lastRead()
	require.NotNil(t, readReq)
	_, fullyConsistent := readReq.GetConsistency().GetRequirement().(*v1.Consistency_FullyConsistent)
	assert.True(t, fullyConsistent, "a stale read here would hide a pin a write just moved")
}

// TestListSlotPins_NoneHeldReturnsEmpty is the ordinary case: a session with
// no single-occupancy slot has nothing to filter down to.
func TestListSlotPins_NoneHeldReturnsEmpty(t *testing.T) {
	cl, _ := newRecordingWriterClient(t)
	c := &Client{cl: cl}

	pins, err := c.ListSlotPins(context.Background(), "ns", "s")
	require.NoError(t, err)
	assert.Empty(t, pins)
}

// TestCopySlotTuples_WritesEveryRelationAsOneBatchedTouch: CopySlotTuples
// itself applies no gate and stamps no expiry — it trusts the caller
// (authz.CopySlotGrants) to have already re-targeted the subject and, for a
// grant, stamped the child's expiry — so it must simply TOUCH every relation
// handed to it, verbatim, in one RPC.
func TestCopySlotTuples_WritesEveryRelationAsOneBatchedTouch(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	c := &Client{cl: cl}
	exp := time.Unix(1700000000, 0).UTC()
	rels := []authz.Relation{
		{ResourceType: "git_repo", ResourceID: "acme/app", Relation: authz.SlotPinRelationName,
			SubjectType: "agentsession", SubjectID: "ns/child"},
		{ResourceType: "git_repo", ResourceID: "acme/app", Relation: "slot_grant_push",
			SubjectType: "agentsession", SubjectID: "ns/child", ExpiresAt: exp},
	}

	require.NoError(t, c.CopySlotTuples(context.Background(), authz.SessionRef{Namespace: "ns", Name: "child"}, rels, nil))

	writes := srv.recordedWrites()
	require.Len(t, writes, 1, "every relation copies in ONE batched RPC")
	require.Len(t, writes[0].Updates, 2)
	for _, u := range writes[0].Updates {
		assert.Equal(t, v1.RelationshipUpdate_OPERATION_TOUCH, u.Operation)
	}

	var sawPin, sawGrant bool
	for _, u := range writes[0].Updates {
		switch u.Relationship.GetRelation() {
		case authz.SlotPinRelationName:
			sawPin = true
			assert.Nil(t, u.Relationship.OptionalExpiresAt, "a pin carries no expiry, even when copied")
		case "slot_grant_push":
			sawGrant = true
			require.NotNil(t, u.Relationship.OptionalExpiresAt, "a grant keeps the caller-stamped expiry")
			assert.Equal(t, exp.Unix(), u.Relationship.OptionalExpiresAt.AsTime().Unix())
		}
	}
	assert.True(t, sawPin)
	assert.True(t, sawGrant)
}

// TestDeleteSlotGrants_RemovesPinsAlongsideGrants is brief step 1(c): the
// teardown sweep must remove the pin, not just the grant. A dropped pin
// delete is a PERMANENT leak — pins carry no expiration — so this asserts the
// delete traffic actually names the pin relation, not merely that the call
// returns success.
func TestDeleteSlotGrants_RemovesPinsAlongsideGrants(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	c := &Client{cl: cl}
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}
	srv.readRels = []*v1.Relationship{
		grantTuple("crm_company", "acme", "contact_access", scope),
		pinTuple("git_repo", "acme/app", scope),
	}

	require.NoError(t, c.DeleteSlotGrants(context.Background(), "ns", "s"))

	var sawGrantDelete, sawPinDelete bool
	for _, w := range srv.recordedWrites() {
		for _, u := range w.Updates {
			assert.Equal(t, v1.RelationshipUpdate_OPERATION_DELETE, u.Operation,
				"every update issued by DeleteSlotGrants must be a delete")
			switch u.Relationship.GetRelation() {
			case "slot_grant_contact_access":
				sawGrantDelete = true
				assert.Equal(t, "acme", u.Relationship.GetResource().GetObjectId())
			case authz.SlotPinRelationName:
				sawPinDelete = true
				assert.Equal(t, "acme/app", u.Relationship.GetResource().GetObjectId())
			}
		}
	}
	assert.True(t, sawGrantDelete, "the grant must still be deleted")
	assert.True(t, sawPinDelete,
		"the pin relation must be in the delete traffic — a dropped pin delete never expires and is a permanent leak")
}

// TestDeleteSlotGrants_NoGrantsOrPinsIsANoOp: a session that never bound an
// instance must not issue any write traffic on teardown.
func TestDeleteSlotGrants_NoGrantsOrPinsIsANoOp(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	c := &Client{cl: cl}

	require.NoError(t, c.DeleteSlotGrants(context.Background(), "ns", "s"))
	assert.Empty(t, srv.recordedWrites(), "nothing held and no pin means nothing to delete")
}

// TestDeleteSlotGrants_PinOnlyStillDeletes covers a grant that already
// expired (and so no longer lists) while its pin — which never expires —
// remains. Teardown must still clean up the orphaned pin.
func TestDeleteSlotGrants_PinOnlyStillDeletes(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	c := &Client{cl: cl}
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}
	srv.readRels = []*v1.Relationship{pinTuple("git_repo", "acme/app", scope)}

	require.NoError(t, c.DeleteSlotGrants(context.Background(), "ns", "s"))

	var sawPinDelete bool
	for _, w := range srv.recordedWrites() {
		for _, u := range w.Updates {
			if u.Relationship.GetRelation() == authz.SlotPinRelationName {
				sawPinDelete = true
				assert.Equal(t, v1.RelationshipUpdate_OPERATION_DELETE, u.Operation)
			}
		}
	}
	assert.True(t, sawPinDelete, "an orphaned pin with no live grant must still be deleted")
}

// TestCopySlotTuples_GuardsEveryCopiedTypeOnTheChildBeingUnpinned: the fork
// copy re-runs on every ReconcileRestart pass while the child's runner may be
// live, so each copied type must ride a MUST_NOT_MATCH on the CHILD holding any
// pin of that type — the same any-instance filter EnsurePin first-fills under —
// in the same RPC as the TOUCH.
func TestCopySlotTuples_GuardsEveryCopiedTypeOnTheChildBeingUnpinned(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	c := &Client{cl: cl}
	child := authz.SessionRef{Namespace: "ns", Name: "child"}
	rels := []authz.Relation{authz.SlotPinRelation("git_repo", "acme/app", child)}

	require.NoError(t, c.CopySlotTuples(context.Background(), child, rels, []string{"git_repo", "crm_company"}))

	writes := srv.recordedWrites()
	require.Len(t, writes, 1)
	pre := writes[0].GetOptionalPreconditions()
	require.Len(t, pre, 2, "one precondition per copied type")
	var guarded []string
	for _, p := range pre {
		assert.Equal(t, v1.Precondition_OPERATION_MUST_NOT_MATCH, p.GetOperation())
		f := p.GetFilter()
		assert.Empty(t, f.GetOptionalResourceId(), "any instance of the type, not just the copied one")
		assert.Equal(t, authz.SlotPinRelationName, f.GetOptionalRelation())
		assert.Equal(t, "agentsession", f.GetOptionalSubjectFilter().GetSubjectType())
		assert.Equal(t, child.String(), f.GetOptionalSubjectFilter().GetOptionalSubjectId(),
			"the guard is on the CHILD's pin, not the parent's")
		guarded = append(guarded, f.GetResourceType())
	}
	assert.ElementsMatch(t, []string{"git_repo", "crm_company"}, guarded)
}

// TestCopySlotTuples_ChildPinnedConcurrentlyWrapsErrSlotPinned: a precondition
// failure means the child committed a pin of a copied type after the caller
// read the child's pins; it must surface as ErrSlotPinned so the next pass
// re-reads and skips that type, and anything else stays a plain error.
func TestCopySlotTuples_ChildPinnedConcurrentlyWrapsErrSlotPinned(t *testing.T) {
	child := authz.SessionRef{Namespace: "ns", Name: "child"}
	rels := []authz.Relation{authz.SlotPinRelation("git_repo", "acme/app", child)}
	cases := []struct {
		name       string
		writeErr   error
		wantPinned bool
	}{
		{name: "precondition failure: wraps ErrSlotPinned", writeErr: status.Error(codes.FailedPrecondition, "pinned"), wantPinned: true},
		{name: "backend failure: plain error, not ErrSlotPinned", writeErr: status.Error(codes.Unavailable, "down"), wantPinned: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cl, srv := newRecordingWriterClient(t)
			c := &Client{cl: cl}
			srv.writeErr = tc.writeErr

			err := c.CopySlotTuples(context.Background(), child, rels, []string{"git_repo"})
			require.Error(t, err)
			assert.Equal(t, tc.wantPinned, errors.Is(err, authz.ErrSlotPinned))
		})
	}
}

// TestDeleteSlotGrants_DeletesPinsBeforeListingAndRevokingGrants: once the pin
// is gone, every racing WriteGrantsPinned fails its MUST_MATCH, so the grant
// list read AFTER the pin delete is final. Revoking grants first left a window
// for a pinned write to land unlisted and outlive the teardown.
func TestDeleteSlotGrants_DeletesPinsBeforeListingAndRevokingGrants(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	c := &Client{cl: cl}
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}
	srv.readRels = []*v1.Relationship{
		grantTuple("crm_company", "acme", "contact_access", scope),
		pinTuple("git_repo", "acme/app", scope),
	}

	require.NoError(t, c.DeleteSlotGrants(context.Background(), "ns", "s"))

	writes := srv.recordedWrites()
	require.Len(t, writes, 2, "one pin delete, then one grant revoke")
	require.Len(t, writes[0].Updates, 1)
	assert.Equal(t, authz.SlotPinRelationName, writes[0].Updates[0].Relationship.GetRelation(),
		"the pin delete must come first")
	require.Len(t, writes[1].Updates, 1)
	assert.Equal(t, "slot_grant_contact_access", writes[1].Updates[0].Relationship.GetRelation())
}
