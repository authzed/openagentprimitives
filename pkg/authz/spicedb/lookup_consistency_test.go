package spicedb

// Unit tests asserting the approval-path LookupSubjects calls request
// FullyConsistent reads. A freshly written approver/participant tuple
// must be visible to the approval-gate decision; MinimizeLatency (the
// default when Consistency is unset) can serve a pre-write snapshot.
// Uses a local in-process gRPC PermissionsService that records requests.

import (
	"context"
	"net"
	"sync"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type capturingPermsServer struct {
	v1.UnimplementedPermissionsServiceServer

	mu         sync.Mutex
	lookupReqs []*v1.LookupSubjectsRequest

	lookupResourceReqs []*v1.LookupResourcesRequest
	// lookupResourcesScript/lookupResourcesRecvErr are armed by
	// setLookupResourcesScript before each LookupResources call this harness
	// serves: the responses to stream back, and — for the mid-stream-failure
	// case — the error to end the stream with instead of a clean close.
	lookupResourcesScript  []*v1.LookupResourcesResponse
	lookupResourcesRecvErr error
}

func (s *capturingPermsServer) LookupSubjects(req *v1.LookupSubjectsRequest, stream v1.PermissionsService_LookupSubjectsServer) error {
	s.mu.Lock()
	s.lookupReqs = append(s.lookupReqs, req)
	s.mu.Unlock()
	return stream.Send(&v1.LookupSubjectsResponse{
		Subject: &v1.ResolvedSubject{SubjectObjectId: "alice"},
	})
}

func (s *capturingPermsServer) captured() []*v1.LookupSubjectsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*v1.LookupSubjectsRequest, len(s.lookupReqs))
	copy(out, s.lookupReqs)
	return out
}

// LookupResources records the request and replays whatever
// setLookupResourcesScript last armed: the scripted responses in order, then
// the scripted error (nil for a clean EOF close) instead of a second script.
func (s *capturingPermsServer) LookupResources(req *v1.LookupResourcesRequest, stream v1.PermissionsService_LookupResourcesServer) error {
	s.mu.Lock()
	s.lookupResourceReqs = append(s.lookupResourceReqs, req)
	resp := s.lookupResourcesScript
	recvErr := s.lookupResourcesRecvErr
	s.mu.Unlock()

	for _, r := range resp {
		if err := stream.Send(r); err != nil {
			return err
		}
	}
	return recvErr
}

// setLookupResourcesScript arms the next LookupResources call(s) this server
// serves to stream resp in order and then end with recvErr (nil for a normal
// EOF close).
func (s *capturingPermsServer) setLookupResourcesScript(resp []*v1.LookupResourcesResponse, recvErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookupResourcesScript = resp
	s.lookupResourcesRecvErr = recvErr
}

func (s *capturingPermsServer) capturedLookupResources() []*v1.LookupResourcesRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*v1.LookupResourcesRequest, len(s.lookupResourceReqs))
	copy(out, s.lookupResourceReqs)
	return out
}

// newCapturingClient starts a local gRPC PermissionsService and returns a
// Client dialed against it plus the capturing server.
func newCapturingClient(t *testing.T) (*Client, *capturingPermsServer) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen")

	srv := grpc.NewServer()
	capture := &capturingPermsServer{}
	v1.RegisterPermissionsServiceServer(srv, capture)
	go func() {
		if err := srv.Serve(ln); err != nil && err != grpc.ErrServerStopped {
			t.Logf("perms server exited: %v", err)
		}
	}()
	t.Cleanup(srv.Stop)

	c, err := NewClient(ln.Addr().String(), "test-token", true)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = c.Close() })
	return c, capture
}

func TestLookupSubjectIncludes_RequestsFullyConsistent(t *testing.T) {
	c, capture := newCapturingClient(t)

	ok, err := c.LookupSubjectIncludes(context.Background(), "group:eng#member", identity.CanonicalFromTrusted("alice", "test fixture"))
	require.NoError(t, err, "LookupSubjectIncludes")
	assert.True(t, ok, "alice is in the resolved subject set")

	reqs := capture.captured()
	require.Len(t, reqs, 1, "exactly one LookupSubjects RPC")
	assert.True(t, reqs[0].GetConsistency().GetFullyConsistent(),
		"approval-gate read must be FullyConsistent: a just-written approver tuple must be visible")
}

func TestLookupSubjects_RequestsFullyConsistent(t *testing.T) {
	c, capture := newCapturingClient(t)

	got, err := c.LookupSubjects(context.Background(), "group:eng#member")
	require.NoError(t, err, "LookupSubjects")
	assert.Equal(t, []string{"alice"}, got)

	reqs := capture.captured()
	require.Len(t, reqs, 1, "exactly one LookupSubjects RPC")
	assert.True(t, reqs[0].GetConsistency().GetFullyConsistent(),
		"approver fan-out read must be FullyConsistent to match the decision path")
}
