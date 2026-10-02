package spicedb

// Coverage for RelationWriter's authz.SlotPinner implementation
// (slot_pin.go): EnsurePin's atomic first-bind, the fully-consistent
// conflict read, WriteGrantsPinned's MUST_MATCH guard, and MovePin's single
// atomic RPC. Uses the same in-process gRPC PermissionsService double as
// writer_test.go and slot_grants_floor_test.go — newRecordingWriterClient,
// extended there with a scriptable write error and a server-streaming
// ReadRelationships — rather than mocking the delegate.

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// pinTuple builds a slot_pin relationship as ReadRelationships would stream
// it back, for scripting recordingWriteServer.readRels.
func pinTuple(resourceType, resourceID string, scope authz.SessionRef) *v1.Relationship {
	return &v1.Relationship{
		Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID},
		Relation: authz.SlotPinRelationName,
		Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "agentsession", ObjectId: scope.String()}},
	}
}

// TestEnsurePin_FirstBindAttachesMustNotMatchPrecondition pins the shape of
// the write a fresh bind issues: one TOUCH, guarded by a MUST_NOT_MATCH
// precondition that the session holds no slot_pin on ANY instance of the
// resource type yet (resource id unset — "any instance", not "this one").
func TestEnsurePin_FirstBindAttachesMustNotMatchPrecondition(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	w := &RelationWriter{c: &Client{cl: cl}}

	held, pinned, err := w.EnsurePin(context.Background(), "git_repo", "repoA",
		authz.SessionRef{Namespace: "ns", Name: "s"})
	require.NoError(t, err)
	assert.False(t, held, "first bind: nothing was already pinned")
	assert.Empty(t, pinned)

	req := srv.lastWrite()
	require.NotNil(t, req, "the delegate must have received the write")
	require.Len(t, req.OptionalPreconditions, 1)
	pc := req.OptionalPreconditions[0]
	assert.Equal(t, v1.Precondition_OPERATION_MUST_NOT_MATCH, pc.Operation)
	assert.Equal(t, "git_repo", pc.Filter.ResourceType)
	assert.Equal(t, "slot_pin", pc.Filter.OptionalRelation)
	assert.Empty(t, pc.Filter.OptionalResourceId, "must match ANY instance of the type")
	assert.Equal(t, "agentsession", pc.Filter.OptionalSubjectFilter.SubjectType)
	assert.Equal(t, "ns/s", pc.Filter.OptionalSubjectFilter.OptionalSubjectId)

	require.Len(t, req.Updates, 1)
	assert.Equal(t, v1.RelationshipUpdate_OPERATION_TOUCH, req.Updates[0].Operation)
	assert.Equal(t, "repoA", req.Updates[0].Relationship.GetResource().GetObjectId())
	assert.Equal(t, "slot_pin", req.Updates[0].Relationship.GetRelation())
}

// TestEnsurePin_ConflictReadsBackPinnedInstance_FullyConsistent scripts a
// FAILED_PRECONDITION on the write (the MUST_NOT_MATCH fired — something is
// already pinned) and one pin tuple on the read. EnsurePin must read back
// which instance, fully consistently, and report it rather than erroring.
func TestEnsurePin_ConflictReadsBackPinnedInstance_FullyConsistent(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}
	srv.writeErr = status.Error(codes.FailedPrecondition, "already pinned")
	srv.readRels = []*v1.Relationship{pinTuple("git_repo", "repoA", scope)}
	w := &RelationWriter{c: &Client{cl: cl}}

	held, pinned, err := w.EnsurePin(context.Background(), "git_repo", "repoB", scope)
	require.NoError(t, err)
	assert.True(t, held)
	assert.Equal(t, "repoA", pinned)

	readReq := srv.lastRead()
	require.NotNil(t, readReq, "the conflict must trigger a read")
	_, fullyConsistent := readReq.GetConsistency().GetRequirement().(*v1.Consistency_FullyConsistent)
	assert.True(t, fullyConsistent, "the conflict read must be fully consistent")
}

// TestEnsurePin_TwoPinsFailsClosed scripts the same write conflict but TWO
// pin tuples on the read-back — a state that should never occur, since the
// slot is single-occupancy. EnsurePin must refuse to guess which one is
// real and fail closed with an error.
func TestEnsurePin_TwoPinsFailsClosed(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}
	srv.writeErr = status.Error(codes.FailedPrecondition, "already pinned")
	srv.readRels = []*v1.Relationship{
		pinTuple("git_repo", "repoA", scope),
		pinTuple("git_repo", "repoB", scope),
	}
	w := &RelationWriter{c: &Client{cl: cl}}

	_, _, err := w.EnsurePin(context.Background(), "git_repo", "repoC", scope)
	assert.Error(t, err, "two pins observed for a single-occupancy slot must never be resolved by guessing")
}

// TestEnsurePin_ZeroPinsOnConflictFailsClosed covers the other "anything but
// exactly one" case: the write reported a conflict but the fully-consistent
// read-back found nothing. That combination should be impossible under a
// correct schema; EnsurePin must not silently report "unpinned" when the
// server just told it otherwise.
func TestEnsurePin_ZeroPinsOnConflictFailsClosed(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}
	srv.writeErr = status.Error(codes.FailedPrecondition, "already pinned")
	// srv.readRels left empty.
	w := &RelationWriter{c: &Client{cl: cl}}

	_, _, err := w.EnsurePin(context.Background(), "git_repo", "repoA", scope)
	assert.Error(t, err, "a reported conflict with no pin found on read-back must fail closed, not report unpinned")
}

// TestEnsurePin_NonPreconditionErrorIsReturnedVerbatim guards that an
// unrelated backend failure (not a precondition conflict) is surfaced as an
// error rather than being mistaken for "already pinned".
func TestEnsurePin_NonPreconditionErrorIsReturnedVerbatim(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	srv.writeErr = status.Error(codes.Unavailable, "backend down")
	w := &RelationWriter{c: &Client{cl: cl}}

	_, _, err := w.EnsurePin(context.Background(), "git_repo", "repoA", authz.SessionRef{Namespace: "ns", Name: "s"})
	require.Error(t, err)
	assert.False(t, status.Code(err) == codes.OK)
}

// TestWriteGrantsPinned_AttachesMustMatchOnThePinnedInstance pins the shape
// of the guarded grant write: the grant TOUCHes plus a MUST_MATCH
// precondition naming exactly <type>:<pinnedID>#slot_pin@agentsession:<scope>.
func TestWriteGrantsPinned_AttachesMustMatchOnThePinnedInstance(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	w := &RelationWriter{c: &Client{cl: cl}}
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}
	grantA := authz.Relation{
		ResourceType: "git_repo", ResourceID: "repoA", Relation: "slot_grant_read",
		SubjectType: "agentsession", SubjectID: scope.String(),
	}

	err := w.WriteGrantsPinned(context.Background(), []authz.Relation{grantA}, "git_repo", "repoA", scope)
	require.NoError(t, err)

	req := srv.lastWrite()
	require.NotNil(t, req)
	require.Len(t, req.OptionalPreconditions, 1)
	pc := req.OptionalPreconditions[0]
	assert.Equal(t, v1.Precondition_OPERATION_MUST_MATCH, pc.Operation)
	assert.Equal(t, "git_repo", pc.Filter.ResourceType)
	assert.Equal(t, "repoA", pc.Filter.OptionalResourceId)
	assert.Equal(t, "slot_pin", pc.Filter.OptionalRelation)
	assert.Equal(t, "agentsession", pc.Filter.OptionalSubjectFilter.SubjectType)
	assert.Equal(t, "ns/s", pc.Filter.OptionalSubjectFilter.OptionalSubjectId)

	require.Len(t, req.Updates, 1)
	assert.Equal(t, v1.RelationshipUpdate_OPERATION_TOUCH, req.Updates[0].Operation)
	assert.Equal(t, "slot_grant_read", req.Updates[0].Relationship.GetRelation())
}

// TestWriteGrantsPinned_FailedPreconditionWrapsErrSlotPinned is the "same
// instance bind races an approved move" case: the pin moved out from under
// this write between read and write, SpiceDB reports FAILED_PRECONDITION,
// and the caller must get back an errors.Is-able authz.ErrSlotPinned rather
// than a bare status error.
func TestWriteGrantsPinned_FailedPreconditionWrapsErrSlotPinned(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	srv.writeErr = status.Error(codes.FailedPrecondition, "pin moved")
	w := &RelationWriter{c: &Client{cl: cl}}
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}
	grantA := authz.Relation{
		ResourceType: "git_repo", ResourceID: "repoA", Relation: "slot_grant_read",
		SubjectType: "agentsession", SubjectID: scope.String(),
	}

	err := w.WriteGrantsPinned(context.Background(), []authz.Relation{grantA}, "git_repo", "repoA", scope)
	require.Error(t, err)
	assert.ErrorIs(t, err, authz.ErrSlotPinned)
}

// TestWriteGrantsPinned_OtherErrorIsNotMistakenForSlotPinned guards that a
// non-precondition failure is NOT wrapped as ErrSlotPinned — that sentinel
// means specifically "the pin moved", not "SpiceDB is unreachable".
func TestWriteGrantsPinned_OtherErrorIsNotMistakenForSlotPinned(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	srv.writeErr = status.Error(codes.Unavailable, "backend down")
	w := &RelationWriter{c: &Client{cl: cl}}
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}

	err := w.WriteGrantsPinned(context.Background(), []authz.Relation{{
		ResourceType: "git_repo", ResourceID: "repoA", Relation: "slot_grant_read",
		SubjectType: "agentsession", SubjectID: scope.String(),
	}}, "git_repo", "repoA", scope)
	require.Error(t, err)
	assert.NotErrorIs(t, err, authz.ErrSlotPinned)
}

// TestMovePin_OneRPCWithMustMatchDeleteTouchAndRevoke pins the shape of an
// approved move: ONE atomic RPC carrying a MUST_MATCH precondition that
// fromID is still pinned, a DELETE of the fromID pin, a TOUCH of the toID
// pin, and a DELETE of every revoked grant relation.
func TestMovePin_OneRPCWithMustMatchDeleteTouchAndRevoke(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	w := &RelationWriter{c: &Client{cl: cl}}
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}
	grantA := authz.Relation{
		ResourceType: "git_repo", ResourceID: "repoA", Relation: "slot_grant_read",
		SubjectType: "agentsession", SubjectID: scope.String(),
	}

	err := w.MovePin(context.Background(), "git_repo", "repoA", "repoB", []authz.Relation{grantA}, scope)
	require.NoError(t, err)

	require.Len(t, srv.recordedWrites(), 1, "one atomic RPC")
	req := srv.lastWrite()
	require.Len(t, req.OptionalPreconditions, 1)
	pc := req.OptionalPreconditions[0]
	assert.Equal(t, v1.Precondition_OPERATION_MUST_MATCH, pc.Operation)
	assert.Equal(t, "git_repo", pc.Filter.ResourceType)
	assert.Equal(t, "repoA", pc.Filter.OptionalResourceId)
	assert.Equal(t, "slot_pin", pc.Filter.OptionalRelation)
	assert.Equal(t, "agentsession", pc.Filter.OptionalSubjectFilter.SubjectType)
	assert.Equal(t, "ns/s", pc.Filter.OptionalSubjectFilter.OptionalSubjectId)

	require.Len(t, req.Updates, 3)

	del1 := req.Updates[0]
	assert.Equal(t, v1.RelationshipUpdate_OPERATION_DELETE, del1.Operation)
	assert.Equal(t, "slot_pin", del1.Relationship.GetRelation())
	assert.Equal(t, "repoA", del1.Relationship.GetResource().GetObjectId())

	touch := req.Updates[1]
	assert.Equal(t, v1.RelationshipUpdate_OPERATION_TOUCH, touch.Operation)
	assert.Equal(t, "slot_pin", touch.Relationship.GetRelation())
	assert.Equal(t, "repoB", touch.Relationship.GetResource().GetObjectId())

	del2 := req.Updates[2]
	assert.Equal(t, v1.RelationshipUpdate_OPERATION_DELETE, del2.Operation)
	assert.Equal(t, "slot_grant_read", del2.Relationship.GetRelation())
	assert.Equal(t, "repoA", del2.Relationship.GetResource().GetObjectId())
}

// TestMovePin_FailedPreconditionWrapsErrSlotPinned: the fromID pin moved (or
// was never there) between the caller's decision and this write landing;
// SpiceDB's FAILED_PRECONDITION must surface as an errors.Is-able
// authz.ErrSlotPinned.
func TestMovePin_FailedPreconditionWrapsErrSlotPinned(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	srv.writeErr = status.Error(codes.FailedPrecondition, "pin already moved")
	w := &RelationWriter{c: &Client{cl: cl}}
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}

	err := w.MovePin(context.Background(), "git_repo", "repoA", "repoB", nil, scope)
	require.Error(t, err)
	assert.ErrorIs(t, err, authz.ErrSlotPinned)
}

// TestListGrantsFor_ReturnsOnlyTheInstancesGrants_FullyConsistent: the read
// used to build a MovePin's revoke set must filter to this instance under this
// session, keep only the slot_grant_ relations (the pin and any other relation
// the session subjects are dropped), and read fully consistently.
func TestListGrantsFor_ReturnsOnlyTheInstancesGrants_FullyConsistent(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}
	srv.readRels = []*v1.Relationship{
		grantTuple("git_repo", "repoA", "push", scope),
		grantTuple("git_repo", "repoA", "read", scope),
		// A non-grant relation the session is also the subject of must be dropped.
		pinTuple("git_repo", "repoA", scope),
	}
	w := &RelationWriter{c: &Client{cl: cl}}

	got, err := w.ListGrantsFor(context.Background(), "git_repo", "repoA", scope)
	require.NoError(t, err)
	require.Len(t, got, 2, "only the slot_grant_ relations are revocable here")
	for _, r := range got {
		assert.Equal(t, "git_repo", r.ResourceType)
		assert.Equal(t, "repoA", r.ResourceID)
		assert.Equal(t, "ns/s", r.SubjectID)
		assert.True(t, r.Relation == "slot_grant_push" || r.Relation == "slot_grant_read", "got %q", r.Relation)
	}

	readReq := srv.lastRead()
	require.NotNil(t, readReq)
	assert.Equal(t, "git_repo", readReq.RelationshipFilter.ResourceType)
	assert.Equal(t, "repoA", readReq.RelationshipFilter.OptionalResourceId)
	assert.Equal(t, "agentsession", readReq.RelationshipFilter.OptionalSubjectFilter.SubjectType)
	assert.Equal(t, "ns/s", readReq.RelationshipFilter.OptionalSubjectFilter.OptionalSubjectId)
	_, fullyConsistent := readReq.GetConsistency().GetRequirement().(*v1.Consistency_FullyConsistent)
	assert.True(t, fullyConsistent, "the revoke-set read decides what authority to remove; it must be current")
}

// TestReadPin_ReturnsEmptyStringWhenNotPinned: zero pin tuples is the
// ordinary "nothing bound yet" case, not a fail-closed error — ReadPin is
// advisory, unlike EnsurePin's post-conflict read.
func TestReadPin_ReturnsEmptyStringWhenNotPinned(t *testing.T) {
	cl, _ := newRecordingWriterClient(t)
	w := &RelationWriter{c: &Client{cl: cl}}

	id, err := w.ReadPin(context.Background(), "git_repo", authz.SessionRef{Namespace: "ns", Name: "s"})
	require.NoError(t, err)
	assert.Empty(t, id)
}

// TestReadPin_ReturnsThePinnedInstance_FullyConsistent is ReadPin's happy
// path, and asserts the same fully-consistent requirement EnsurePin's
// conflict read depends on.
func TestReadPin_ReturnsThePinnedInstance_FullyConsistent(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}
	srv.readRels = []*v1.Relationship{pinTuple("git_repo", "repoA", scope)}
	w := &RelationWriter{c: &Client{cl: cl}}

	id, err := w.ReadPin(context.Background(), "git_repo", scope)
	require.NoError(t, err)
	assert.Equal(t, "repoA", id)

	readReq := srv.lastRead()
	require.NotNil(t, readReq)
	_, fullyConsistent := readReq.GetConsistency().GetRequirement().(*v1.Consistency_FullyConsistent)
	assert.True(t, fullyConsistent)
}

// TestReadPin_TwoPinsFailsClosed: an impossible state for a single-occupancy
// slot. ReadPin must refuse to guess rather than returning either ID.
func TestReadPin_TwoPinsFailsClosed(t *testing.T) {
	cl, srv := newRecordingWriterClient(t)
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}
	srv.readRels = []*v1.Relationship{
		pinTuple("git_repo", "repoA", scope),
		pinTuple("git_repo", "repoB", scope),
	}
	w := &RelationWriter{c: &Client{cl: cl}}

	_, err := w.ReadPin(context.Background(), "git_repo", scope)
	assert.Error(t, err)
}

// TestWriteGrantsPinned_AdvancesFreshnessFloor: a single-occupancy grant must
// advance the session's read-your-writes floor exactly as the plain
// WriteRelationships path does — skipping it reopens the approve-then-stale-
// denied race for the DEFAULT bind path. The floor hook gets the write's
// WrittenAt token, keyed by the granted rels' own resources.
func TestWriteGrantsPinned_AdvancesFreshnessFloor(t *testing.T) {
	cl, rec := newRecordingWriterClient(t)
	rec.writtenAt = "zt-after-pinned-grant"
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}

	var gotRels []authz.Relation
	var gotTok string
	w := &RelationWriter{c: &Client{cl: cl}, onWrite: func(rels []authz.Relation, token string) {
		gotRels, gotTok = rels, token
	}}

	grant := authz.Relation{
		ResourceType: "git_repo", ResourceID: "repoA", Relation: "slot_grant_push",
		SubjectType: "agentsession", SubjectID: scope.String(),
	}
	require.NoError(t, w.WriteGrantsPinned(context.Background(), []authz.Relation{grant}, "git_repo", "repoA", scope))

	assert.Equal(t, "zt-after-pinned-grant", gotTok, "the floor hook must get the pinned grant's WrittenAt token")
	require.Len(t, gotRels, 1, "the floor hook must get the granted rels")
	assert.Equal(t, "git_repo", gotRels[0].ResourceType)
	assert.Equal(t, "repoA", gotRels[0].ResourceID)
}

// TestMovePin_AdvancesFreshnessFloorForTouchedResources: a move must advance the
// floor for every resource it touched — the new and old pin instances and each
// revoked grant — so a Check on the displaced instance right after the move
// reads at-least-as-fresh as the revoke that stripped its grants.
func TestMovePin_AdvancesFreshnessFloorForTouchedResources(t *testing.T) {
	cl, rec := newRecordingWriterClient(t)
	rec.writtenAt = "zt-after-move"
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}

	var gotRels []authz.Relation
	var gotTok string
	w := &RelationWriter{c: &Client{cl: cl}, onWrite: func(rels []authz.Relation, token string) {
		gotRels, gotTok = rels, token
	}}

	revoke := []authz.Relation{{
		ResourceType: "git_repo", ResourceID: "repoA", Relation: "slot_grant_push",
		SubjectType: "agentsession", SubjectID: scope.String(),
	}}
	require.NoError(t, w.MovePin(context.Background(), "git_repo", "repoA", "repoB", revoke, scope))

	assert.Equal(t, "zt-after-move", gotTok, "the floor hook must get the move's WrittenAt token")
	ids := map[string]bool{}
	for _, r := range gotRels {
		ids[r.ResourceID] = true
	}
	assert.True(t, ids["repoA"], "the floor must advance for the displaced instance (old pin + revoked grant)")
	assert.True(t, ids["repoB"], "the floor must advance for the new pin instance")
}

// TestEnsurePin_DoesNotAdvanceFreshnessFloor pins the other half of the
// contract the file header states: EnsurePin writes only the slot_pin tuple,
// which nothing ever Checks, so it must NOT fire the floor hook (there is no
// floor a later check could need advanced).
func TestEnsurePin_DoesNotAdvanceFreshnessFloor(t *testing.T) {
	cl, rec := newRecordingWriterClient(t)
	rec.writtenAt = "zt-after-pin"
	scope := authz.SessionRef{Namespace: "ns", Name: "s"}

	called := false
	w := &RelationWriter{c: &Client{cl: cl}, onWrite: func([]authz.Relation, string) { called = true }}

	_, _, err := w.EnsurePin(context.Background(), "git_repo", "repoA", scope)
	require.NoError(t, err)
	assert.False(t, called, "nothing Checks the slot_pin relation, so EnsurePin must not advance the floor")
}
