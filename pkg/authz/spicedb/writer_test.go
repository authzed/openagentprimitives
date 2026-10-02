package spicedb

// Coverage for RelWriter / NewWriter (writer.go): the guarded surface that
// replaced the old raw-client handout. NewWriter's delegate is a concrete
// *authzed.Client, so — matching this package's established pattern in
// client_backend_error_test.go and lookup_consistency_test.go — these tests
// stand up a real, in-process gRPC PermissionsService rather than mocking
// the delegate.

import (
	"context"
	"net"
	"sync"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	authzed "github.com/authzed/authzed-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// This package cannot blank-import pkg/authz/spicedb/relsource/imports — the
// bundle imports this package (for TypedWritesSource's claims), so doing so
// from here would be an import cycle. Mark the table complete directly
// instead; CheckWrite/CheckDeleteFilter otherwise refuse every call in this
// file regardless of what a test's fixture Source claims.
func init() {
	relsource.MarkComplete()
}

// recordingWriteServer is a scripted PermissionsService that records every
// Write/Delete/CheckBulkPermissions request it receives and answers with a
// canned, empty-but-successful response.
type recordingWriteServer struct {
	v1.UnimplementedPermissionsServiceServer

	mu         sync.Mutex
	writeReqs  []*v1.WriteRelationshipsRequest
	deleteReqs []*v1.DeleteRelationshipsRequest
	checkReqs  []*v1.CheckBulkPermissionsRequest
	// writtenAt, when non-empty, is echoed as the WriteRelationshipsResponse's
	// WrittenAt ZedToken — real SpiceDB always returns one, and the freshness
	// floor advance under test depends on capturing it.
	writtenAt string
	// writeErr, when non-nil, is returned once from the next
	// WriteRelationships call instead of a success response, then cleared.
	// Scripts a precondition failure (or any other write error) for the
	// slot-pin tests without needing a schema that can actually produce one.
	// The request is still recorded before the error is returned — a real
	// SpiceDB receives and evaluates the request before it refuses it.
	writeErr error
	// readRels, when set, is served back verbatim by every ReadRelationships
	// call, regardless of the request's filter — the slot-pin tests script
	// exactly the relationships they want read back rather than simulating
	// SpiceDB's filter evaluation.
	readRels []*v1.Relationship
	readReqs []*v1.ReadRelationshipsRequest
}

func (s *recordingWriteServer) WriteRelationships(_ context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	s.mu.Lock()
	s.writeReqs = append(s.writeReqs, req)
	tok := s.writtenAt
	err := s.writeErr
	s.writeErr = nil
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	resp := &v1.WriteRelationshipsResponse{}
	if tok != "" {
		resp.WrittenAt = &v1.ZedToken{Token: tok}
	}
	return resp, nil
}

// ReadRelationships is a server-streaming RPC: it serves back s.readRels
// unconditionally and records the request so a test can assert on its
// Consistency (the slot-pin conflict read must be fully consistent).
func (s *recordingWriteServer) ReadRelationships(req *v1.ReadRelationshipsRequest, stream v1.PermissionsService_ReadRelationshipsServer) error {
	s.mu.Lock()
	s.readReqs = append(s.readReqs, req)
	rels := make([]*v1.Relationship, len(s.readRels))
	copy(rels, s.readRels)
	s.mu.Unlock()
	for _, rel := range rels {
		if err := stream.Send(&v1.ReadRelationshipsResponse{Relationship: rel}); err != nil {
			return err
		}
	}
	return nil
}

func (s *recordingWriteServer) DeleteRelationships(_ context.Context, req *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error) {
	s.mu.Lock()
	s.deleteReqs = append(s.deleteReqs, req)
	s.mu.Unlock()
	return &v1.DeleteRelationshipsResponse{}, nil
}

func (s *recordingWriteServer) CheckBulkPermissions(_ context.Context, req *v1.CheckBulkPermissionsRequest) (*v1.CheckBulkPermissionsResponse, error) {
	s.mu.Lock()
	s.checkReqs = append(s.checkReqs, req)
	s.mu.Unlock()
	return &v1.CheckBulkPermissionsResponse{}, nil
}

func (s *recordingWriteServer) recordedWrites() []*v1.WriteRelationshipsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*v1.WriteRelationshipsRequest, len(s.writeReqs))
	copy(out, s.writeReqs)
	return out
}

// lastWrite returns the most recently received WriteRelationshipsRequest, or
// nil if none has arrived yet.
func (s *recordingWriteServer) lastWrite() *v1.WriteRelationshipsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.writeReqs) == 0 {
		return nil
	}
	return s.writeReqs[len(s.writeReqs)-1]
}

// lastRead returns the most recently received ReadRelationshipsRequest, or
// nil if none has arrived yet.
func (s *recordingWriteServer) lastRead() *v1.ReadRelationshipsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.readReqs) == 0 {
		return nil
	}
	return s.readReqs[len(s.readReqs)-1]
}

func (s *recordingWriteServer) recordedChecks() []*v1.CheckBulkPermissionsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*v1.CheckBulkPermissionsRequest, len(s.checkReqs))
	copy(out, s.checkReqs)
	return out
}

// newRecordingWriterClient dials a real, in-process gRPC PermissionsService
// and returns the raw *authzed.Client NewWriter wraps, plus the server
// recording what it received.
func newRecordingWriterClient(t *testing.T) (*authzed.Client, *recordingWriteServer) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen")

	srv := grpc.NewServer()
	rec := &recordingWriteServer{}
	v1.RegisterPermissionsServiceServer(srv, rec)
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && serveErr != grpc.ErrServerStopped {
			t.Logf("permissions server exited: %v", serveErr)
		}
	}()
	t.Cleanup(srv.Stop)

	cl, err := authzed.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err, "authzed.NewClient")
	t.Cleanup(func() { _ = cl.Close() })
	return cl, rec
}

// touchUpdate builds a TOUCH RelationshipUpdate — the only operation these
// tests need. Mirrors relsource's own check_test.go helper of the same
// shape.
func touchUpdate(resourceType, resourceID, relation, subjectType, subjectID string) *v1.RelationshipUpdate {
	return &v1.RelationshipUpdate{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{
			Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID},
			Relation: relation,
			Subject: &v1.SubjectReference{
				Object: &v1.ObjectReference{ObjectType: subjectType, ObjectId: subjectID},
			},
		},
	}
}

// With no claims registered the wrapper is transparent: this task is a
// refactor, and a behaviour change here would be indistinguishable from the
// enforcement a later task adds deliberately.
func TestWriter_PassesThroughWhenNothingIsClaimed(t *testing.T) {
	cl, rec := newRecordingWriterClient(t)
	w := NewWriter(cl, relsource.Source{Name: "writer-test-passthrough"})

	_, err := w.WriteRelationships(context.Background(), &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{touchUpdate("gadget", "w1", "owner", "user", "u1")},
	})
	require.NoError(t, err, "an unclaimed relation must never be refused")
	assert.Len(t, rec.recordedWrites(), 1, "the delegate must have received the write")
}

// Reads are never guarded, whatever is claimed.
func TestWriter_DoesNotGuardReads(t *testing.T) {
	relsource.Register(relsource.Source{Name: "writer-test-read-claimant", Claims: []string{"widget#owner"}})

	cl, rec := newRecordingWriterClient(t)
	// A DIFFERENT source than the one claiming widget#owner above — if reads
	// were guarded the same way writes are, this call would be refused.
	w := NewWriter(cl, relsource.Source{Name: "writer-test-unrelated-reader"})

	_, err := w.CheckBulkPermissions(context.Background(), &v1.CheckBulkPermissionsRequest{
		Items: []*v1.CheckBulkPermissionsRequestItem{{
			Resource:   &v1.ObjectReference{ObjectType: "widget", ObjectId: "w1"},
			Permission: "owner",
			Subject:    &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: "u1"}},
		}},
	})
	require.NoError(t, err, "a read must never be refused by the write guard, even against a relation another source claims")
	assert.Len(t, rec.recordedChecks(), 1, "the delegate must have received the read")
}

// The delegate receives exactly what the caller passed.
func TestWriter_ForwardsUpdatesUnaltered(t *testing.T) {
	cl, rec := newRecordingWriterClient(t)
	w := NewWriter(cl, relsource.Source{Name: "writer-test-forward"})

	_, err := w.WriteRelationships(context.Background(), &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{
			touchUpdate("gadget", "w1", "owner", "user", "u1"),
			touchUpdate("gadget", "w1", "viewer", "user", "u2"),
		},
	})
	require.NoError(t, err)

	got := rec.recordedWrites()
	require.Len(t, got, 1, "exactly one WriteRelationships RPC")
	require.Len(t, got[0].GetUpdates(), 2, "both updates must reach the delegate")
	first, second := got[0].GetUpdates()[0].GetRelationship(), got[0].GetUpdates()[1].GetRelationship()
	assert.Equal(t, "owner", first.GetRelation())
	assert.Equal(t, "u1", first.GetSubject().GetObject().GetObjectId())
	assert.Equal(t, "viewer", second.GetRelation())
	assert.Equal(t, "u2", second.GetSubject().GetObject().GetObjectId())
}

// TestNewWriter_NilClientYieldsNilInterface guards the AGENTS.md typed-nil
// rule at NewWriter's documented call site: a nil *authzed.Client must
// produce a genuine nil RelWriter interface, not a non-nil interface
// wrapping a nil *relWriter. Mirrors
// TestNewGrantWriter_NilClientYieldsNilPointer in schemaio_test.go.
//
// assert.Nil is deliberately NOT used here: testify's Nil unwraps an
// interface via reflection and reports true for EITHER a genuine nil
// interface OR a non-nil interface boxing a nil pointer — exactly the two
// cases this test needs to tell apart. Only a direct `== nil` comparison on
// the interface value fails when NewWriter returns a typed-nil boxed in a
// RelWriter.
func TestNewWriter_NilClientYieldsNilInterface(t *testing.T) {
	w := NewWriter(nil, relsource.Source{Name: "writer-test-nil-client"})
	require.True(t, w == nil,
		"NewWriter(nil, ...) must yield a genuine nil RelWriter interface, not a non-nil interface wrapping a nil *relWriter")
}

// TestClientWriter_NilClientYieldsNilInterface is
// TestNewWriter_NilClientYieldsNilInterface's counterpart for
// (*Client).Writer — the same typed-nil hazard, one call further out: a nil
// *Client must also yield a genuine nil RelWriter, not a non-nil interface
// wrapping a nil pointer.
func TestClientWriter_NilClientYieldsNilInterface(t *testing.T) {
	var c *Client
	w := c.Writer(relsource.Source{Name: "writer-test-nil-client"})
	require.True(t, w == nil,
		"(*Client)(nil).Writer(...) must yield a genuine nil RelWriter interface, not a non-nil interface wrapping a nil pointer")
}
