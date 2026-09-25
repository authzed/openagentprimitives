package spicedb

// Table-driven coverage for LookupStartableClasses, the agentclass sibling of
// LookupInteractableSessions (lookup_resources_test.go). It backs the browser's
// agent picker and — when read as a gate — decides whether a user may start a
// session of a class at all, so the properties that matter are the same three:
// an undetermined answer must never read as a grant, a short list must
// self-report as short, and a stream failure must never render as a complete
// (empty) answer.

import (
	"context"
	"errors"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookupStartableClasses(t *testing.T) {
	streamErr := errors.New("boom")

	cases := []struct {
		name            string
		scripted        []*v1.LookupResourcesResponse
		recvErr         error
		limit           uint32
		fullyConsistent bool
		check           func(t *testing.T, capture *capturingPermsServer, got StartableClasses, err error)
	}{
		{
			name: "happy: HAS_PERMISSION ids split into Refs in stream order",
			scripted: []*v1.LookupResourcesResponse{
				hasPermission("demo-ns/alpha"),
				hasPermission("demo-ns/beta"),
				hasPermission("other-ns/gamma"),
			},
			limit: 50,
			check: func(t *testing.T, _ *capturingPermsServer, got StartableClasses, err error) {
				require.NoError(t, err, "LookupStartableClasses")
				assert.Equal(t, []ClassRef{{"demo-ns", "alpha"}, {"demo-ns", "beta"}, {"other-ns", "gamma"}}, got.Refs)
				assert.False(t, got.Truncated)
				assert.Empty(t, got.Unrepresentable)
				assert.Empty(t, got.Conditional)
			},
		},
		{
			// agentclass#start_session can be reached by more than one arm, and
			// LookupResources emits per reachable PATH, not per distinct
			// resource — so one class can stream twice and would otherwise draw
			// twice in the picker.
			name: "the same id reached by two permission arms yields ONE ref",
			scripted: []*v1.LookupResourcesResponse{
				hasPermission("demo-ns/alpha"),
				hasPermission("demo-ns/alpha"),
				hasPermission("demo-ns/beta"),
			},
			limit: 50,
			check: func(t *testing.T, _ *capturingPermsServer, got StartableClasses, err error) {
				require.NoError(t, err, "LookupStartableClasses")
				assert.Equal(t, []ClassRef{{"demo-ns", "alpha"}, {"demo-ns", "beta"}}, got.Refs)
			},
		},
		{
			name: "duplicates do not inflate the truncation test",
			scripted: []*v1.LookupResourcesResponse{
				hasPermission("demo-ns/alpha"),
				hasPermission("demo-ns/alpha"),
				hasPermission("demo-ns/beta"),
			},
			limit: 3,
			check: func(t *testing.T, _ *capturingPermsServer, got StartableClasses, err error) {
				require.NoError(t, err, "LookupStartableClasses")
				assert.Len(t, got.Refs, 2)
				assert.False(t, got.Truncated, "two distinct results under a limit of three is not truncated")
			},
		},
		{
			name: "conditional permissionship: reported separately, absent from Refs",
			scripted: []*v1.LookupResourcesResponse{
				hasPermission("demo-ns/alpha"),
				conditionalPermission("demo-ns/beta"),
			},
			limit: 50,
			check: func(t *testing.T, _ *capturingPermsServer, got StartableClasses, err error) {
				require.NoError(t, err, "LookupStartableClasses")
				assert.Equal(t, []string{"demo-ns/beta"}, got.Conditional)
				assert.Equal(t, []ClassRef{{"demo-ns", "alpha"}}, got.Refs,
					"an undetermined answer must never read as a grant")
			},
		},
		{
			name: "unspecified permissionship: reported separately, absent from Refs",
			scripted: []*v1.LookupResourcesResponse{
				hasPermission("demo-ns/alpha"),
				unspecifiedPermission("demo-ns/beta"),
			},
			limit: 50,
			check: func(t *testing.T, _ *capturingPermsServer, got StartableClasses, err error) {
				require.NoError(t, err, "LookupStartableClasses")
				assert.Equal(t, []string{"demo-ns/beta"}, got.Conditional)
				assert.Equal(t, []ClassRef{{"demo-ns", "alpha"}}, got.Refs,
					"an unspecified answer must never read as a grant")
			},
		},
		{
			name: "an id that is both definite and conditional counts only as definite",
			scripted: []*v1.LookupResourcesResponse{
				conditionalPermission("demo-ns/alpha"),
				hasPermission("demo-ns/alpha"),
			},
			limit: 50,
			check: func(t *testing.T, _ *capturingPermsServer, got StartableClasses, err error) {
				require.NoError(t, err, "LookupStartableClasses")
				assert.Equal(t, []ClassRef{{"demo-ns", "alpha"}}, got.Refs)
				assert.Empty(t, got.Conditional, "a definite grant settles the id")
			},
		},
		{
			name:     "id with no slash: Unrepresentable, not a ref with an empty namespace",
			scripted: []*v1.LookupResourcesResponse{hasPermission("bare-id")},
			limit:    50,
			check: func(t *testing.T, _ *capturingPermsServer, got StartableClasses, err error) {
				require.NoError(t, err, "LookupStartableClasses")
				assert.Equal(t, []string{"bare-id"}, got.Unrepresentable)
				assert.Empty(t, got.Refs)
			},
		},
		{
			name:     `id like "demo-ns/" (empty name half): Unrepresentable`,
			scripted: []*v1.LookupResourcesResponse{hasPermission("demo-ns/")},
			limit:    50,
			check: func(t *testing.T, _ *capturingPermsServer, got StartableClasses, err error) {
				require.NoError(t, err, "LookupStartableClasses")
				assert.Equal(t, []string{"demo-ns/"}, got.Unrepresentable)
				assert.Empty(t, got.Refs)
			},
		},
		{
			name:     `id like "/class-name" (empty namespace half): Unrepresentable`,
			scripted: []*v1.LookupResourcesResponse{hasPermission("/class-name")},
			limit:    50,
			check: func(t *testing.T, _ *capturingPermsServer, got StartableClasses, err error) {
				require.NoError(t, err, "LookupStartableClasses")
				assert.Equal(t, []string{"/class-name"}, got.Unrepresentable)
				assert.Empty(t, got.Refs, "must not decode to a ClassRef with an empty Namespace")
			},
		},
		{
			name:            "fullyConsistent=false requests MinimizeLatency (list path)",
			scripted:        []*v1.LookupResourcesResponse{hasPermission("demo-ns/alpha")},
			limit:           50,
			fullyConsistent: false,
			check: func(t *testing.T, capture *capturingPermsServer, _ StartableClasses, err error) {
				require.NoError(t, err, "LookupStartableClasses")
				reqs := capture.capturedLookupResources()
				require.Len(t, reqs, 1, "exactly one LookupResources RPC")
				assert.True(t, reqs[0].GetConsistency().GetMinimizeLatency(),
					"a picker refresh may read a slightly stale snapshot")
			},
		},
		{
			name:            "fullyConsistent=true requests FullyConsistent (gate path)",
			scripted:        []*v1.LookupResourcesResponse{hasPermission("demo-ns/alpha")},
			limit:           50,
			fullyConsistent: true,
			check: func(t *testing.T, capture *capturingPermsServer, _ StartableClasses, err error) {
				require.NoError(t, err, "LookupStartableClasses")
				reqs := capture.capturedLookupResources()
				require.Len(t, reqs, 1, "exactly one LookupResources RPC")
				assert.True(t, reqs[0].GetConsistency().GetFullyConsistent(),
					"a gate read must see the platform link written by the reconcile that created the class")
			},
		},
		{
			name: "the limit is passed through and reaching it reports Truncated",
			scripted: []*v1.LookupResourcesResponse{
				hasPermission("demo-ns/alpha"),
				hasPermission("demo-ns/beta"),
			},
			limit: 2,
			check: func(t *testing.T, capture *capturingPermsServer, got StartableClasses, err error) {
				require.NoError(t, err, "LookupStartableClasses")
				reqs := capture.capturedLookupResources()
				require.Len(t, reqs, 1)
				assert.Equal(t, uint32(2), reqs[0].GetOptionalLimit())
				assert.True(t, got.Truncated, "a list at the limit must self-report as a possible prefix")
			},
		},
		{
			name:     "subject is user:<canonical>, permission start_session on agentclass",
			scripted: []*v1.LookupResourcesResponse{hasPermission("demo-ns/alpha")},
			limit:    50,
			check: func(t *testing.T, capture *capturingPermsServer, _ StartableClasses, err error) {
				require.NoError(t, err, "LookupStartableClasses")
				reqs := capture.capturedLookupResources()
				require.Len(t, reqs, 1)
				req := reqs[0]
				assert.Equal(t, "agentclass", req.GetResourceObjectType())
				assert.Equal(t, "start_session", req.GetPermission())
				assert.Equal(t, "user", req.GetSubject().GetObject().GetObjectType())
				assert.Equal(t, demoCanonical, req.GetSubject().GetObject().GetObjectId())
			},
		},
		{
			name:     "a mid-stream Recv error is returned, not partially swallowed",
			scripted: []*v1.LookupResourcesResponse{hasPermission("demo-ns/alpha")},
			recvErr:  streamErr,
			limit:    50,
			check: func(t *testing.T, _ *capturingPermsServer, got StartableClasses, err error) {
				require.Error(t, err, "LookupStartableClasses must surface the Recv failure")
				assert.Contains(t, err.Error(), "startable classes recv",
					"error text must name the failing call, not just be non-nil")
				assert.Equal(t, StartableClasses{}, got,
					"a caller that ignores err must not be able to render a partial list as complete")
			},
		},
		{
			name:     "no results: empty, non-nil Refs, nil error",
			scripted: nil,
			limit:    50,
			check: func(t *testing.T, _ *capturingPermsServer, got StartableClasses, err error) {
				require.NoError(t, err, "holding no platform grant is a legitimate answer, not an error")
				assert.NotNil(t, got.Refs)
				assert.Empty(t, got.Refs)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, capture := newCapturingClient(t)
			capture.setLookupResourcesScript(tc.scripted, tc.recvErr)

			got, err := c.LookupStartableClasses(context.Background(), identity.CanonicalFromTrusted(demoCanonical, "test fixture"), tc.limit, tc.fullyConsistent)
			tc.check(t, capture, got, err)
		})
	}
}
