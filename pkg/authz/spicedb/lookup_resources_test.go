package spicedb

// Table-driven coverage for LookupInteractableSessions (and, through it,
// lookupResources): the forward-direction sibling of
// lookup_consistency_test.go's LookupSubjects coverage. Reuses the same
// in-process capturingPermsServer harness, extended there with a
// LookupResources stream handler.

import (
	"context"
	"errors"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// demoCanonical is the fixed subject every case below asks about; only the
// scripted server responses and call parameters vary per case.
const demoCanonical = "demo-user"

func hasPermission(id string) *v1.LookupResourcesResponse {
	return &v1.LookupResourcesResponse{ResourceObjectId: id, Permissionship: v1.LookupPermissionship_LOOKUP_PERMISSIONSHIP_HAS_PERMISSION}
}

func conditionalPermission(id string) *v1.LookupResourcesResponse {
	return &v1.LookupResourcesResponse{ResourceObjectId: id, Permissionship: v1.LookupPermissionship_LOOKUP_PERMISSIONSHIP_CONDITIONAL_PERMISSION}
}

func unspecifiedPermission(id string) *v1.LookupResourcesResponse {
	return &v1.LookupResourcesResponse{ResourceObjectId: id, Permissionship: v1.LookupPermissionship_LOOKUP_PERMISSIONSHIP_UNSPECIFIED}
}

func TestLookupInteractableSessions(t *testing.T) {
	streamErr := errors.New("boom")

	cases := []struct {
		name            string
		scripted        []*v1.LookupResourcesResponse
		recvErr         error
		limit           uint32
		fullyConsistent bool
		check           func(t *testing.T, capture *capturingPermsServer, got InteractableSessions, err error)
	}{
		{
			name: "happy: three HAS_PERMISSION ids, split into Refs in stream order",
			scripted: []*v1.LookupResourcesResponse{
				hasPermission("demo-ns/a"),
				hasPermission("demo-ns/b"),
				hasPermission("other-ns/c"),
			},
			limit: 50,
			check: func(t *testing.T, _ *capturingPermsServer, got InteractableSessions, err error) {
				require.NoError(t, err, "LookupInteractableSessions")
				assert.Equal(t, []SessionRef{{"demo-ns", "a"}, {"demo-ns", "b"}, {"other-ns", "c"}}, got.Refs)
				assert.False(t, got.Truncated)
				assert.Empty(t, got.Unrepresentable)
				assert.Empty(t, got.Conditional)
			},
		},
		{
			// The shipped bug. agentsession#interact is
			// `owner + started_by + participant - denied`, and LookupResources
			// emits per reachable PATH, not per distinct resource — so a
			// session the viewer started AND owns streams twice. That is the
			// ordinary case for any session you started yourself, not an
			// exotic one.
			//
			// It surfaced as the same session drawn twice in the browser's
			// sidebar, which reads as two sessions rather than one row
			// repeated.
			name: "the same id reached by two permission arms yields ONE ref",
			scripted: []*v1.LookupResourcesResponse{
				hasPermission("demo-ns/a"),
				hasPermission("demo-ns/a"),
				hasPermission("demo-ns/b"),
			},
			limit: 50,
			check: func(t *testing.T, _ *capturingPermsServer, got InteractableSessions, err error) {
				require.NoError(t, err, "LookupInteractableSessions")
				assert.Equal(t, []SessionRef{{"demo-ns", "a"}, {"demo-ns", "b"}}, got.Refs)
			},
		},
		{
			// Duplicates must not be counted toward the limit either. Three
			// streamed items for two distinct resources, at a limit of 2,
			// would have compared 3 >= 2 and reported a COMPLETE list as
			// truncated — telling the viewer sessions were hidden when none
			// were.
			name: "duplicates do not inflate the truncation test",
			scripted: []*v1.LookupResourcesResponse{
				hasPermission("demo-ns/a"),
				hasPermission("demo-ns/a"),
				hasPermission("demo-ns/b"),
			},
			limit: 3,
			check: func(t *testing.T, _ *capturingPermsServer, got InteractableSessions, err error) {
				require.NoError(t, err, "LookupInteractableSessions")
				assert.Len(t, got.Refs, 2)
				assert.False(t, got.Truncated, "two distinct results under a limit of three is not truncated")
			},
		},
		{
			// A resource granted outright by one arm and caveated by another
			// is AVAILABLE. Listing it in both would show it in the sidebar
			// and simultaneously count it in "N sessions could not be loaded".
			name: "an id that is both definite and conditional counts only as definite",
			scripted: []*v1.LookupResourcesResponse{
				conditionalPermission("demo-ns/a"),
				hasPermission("demo-ns/a"),
				conditionalPermission("demo-ns/b"),
			},
			limit: 50,
			check: func(t *testing.T, _ *capturingPermsServer, got InteractableSessions, err error) {
				require.NoError(t, err, "LookupInteractableSessions")
				assert.Equal(t, []SessionRef{{"demo-ns", "a"}}, got.Refs)
				assert.Equal(t, []string{"demo-ns/b"}, got.Conditional,
					"only the id with no definite grant stays undetermined")
			},
		},
		{
			name: "a repeated conditional id is counted once",
			scripted: []*v1.LookupResourcesResponse{
				conditionalPermission("demo-ns/b"),
				conditionalPermission("demo-ns/b"),
			},
			limit: 50,
			check: func(t *testing.T, _ *capturingPermsServer, got InteractableSessions, err error) {
				require.NoError(t, err, "LookupInteractableSessions")
				assert.Equal(t, []string{"demo-ns/b"}, got.Conditional)
			},
		},
		{
			name: "conditional permissionship: dropped, counted, absent from Refs",
			scripted: []*v1.LookupResourcesResponse{
				hasPermission("demo-ns/a"),
				conditionalPermission("demo-ns/b"),
			},
			limit: 50,
			check: func(t *testing.T, _ *capturingPermsServer, got InteractableSessions, err error) {
				require.NoError(t, err, "LookupInteractableSessions")
				assert.Equal(t, []string{"demo-ns/b"}, got.Conditional)
				assert.Equal(t, []SessionRef{{"demo-ns", "a"}}, got.Refs,
					"the conditional id must not appear anywhere in Refs")
			},
		},
		{
			name: "unspecified permissionship: dropped, counted, absent from Refs",
			scripted: []*v1.LookupResourcesResponse{
				hasPermission("demo-ns/a"),
				unspecifiedPermission("demo-ns/b"),
			},
			limit: 50,
			check: func(t *testing.T, _ *capturingPermsServer, got InteractableSessions, err error) {
				require.NoError(t, err, "LookupInteractableSessions")
				assert.Equal(t, []string{"demo-ns/b"}, got.Conditional)
				assert.Equal(t, []SessionRef{{"demo-ns", "a"}}, got.Refs,
					"the unspecified id must not appear anywhere in Refs")
			},
		},
		{
			name: "id with no slash: Unrepresentable, not a ref with an empty namespace",
			scripted: []*v1.LookupResourcesResponse{
				hasPermission("bare-id"),
			},
			limit: 50,
			check: func(t *testing.T, _ *capturingPermsServer, got InteractableSessions, err error) {
				require.NoError(t, err, "LookupInteractableSessions")
				assert.Equal(t, []string{"bare-id"}, got.Unrepresentable)
				assert.Empty(t, got.Refs)
			},
		},
		{
			name: `id like "demo-ns/" (empty name half): Unrepresentable`,
			scripted: []*v1.LookupResourcesResponse{
				hasPermission("demo-ns/"),
			},
			limit: 50,
			check: func(t *testing.T, _ *capturingPermsServer, got InteractableSessions, err error) {
				require.NoError(t, err, "LookupInteractableSessions")
				assert.Equal(t, []string{"demo-ns/"}, got.Unrepresentable)
				assert.Empty(t, got.Refs)
			},
		},
		{
			name: `id like "/session-name" (empty namespace half): Unrepresentable`,
			scripted: []*v1.LookupResourcesResponse{
				hasPermission("/session-name"),
			},
			limit: 50,
			check: func(t *testing.T, _ *capturingPermsServer, got InteractableSessions, err error) {
				require.NoError(t, err, "LookupInteractableSessions")
				assert.Equal(t, []string{"/session-name"}, got.Unrepresentable)
				assert.Empty(t, got.Refs, "must not decode to a SessionRef with an empty Namespace")
			},
		},
		{
			name:            "fullyConsistent=false requests MinimizeLatency",
			scripted:        []*v1.LookupResourcesResponse{hasPermission("demo-ns/a")},
			limit:           50,
			fullyConsistent: false,
			check: func(t *testing.T, capture *capturingPermsServer, got InteractableSessions, err error) {
				require.NoError(t, err, "LookupInteractableSessions")
				reqs := capture.capturedLookupResources()
				require.Len(t, reqs, 1, "exactly one LookupResources RPC")
				assert.True(t, reqs[0].GetConsistency().GetMinimizeLatency(),
					"a list read must request MinimizeLatency, not FullyConsistent")
			},
		},
		{
			name:            "fullyConsistent=true requests FullyConsistent",
			scripted:        []*v1.LookupResourcesResponse{hasPermission("demo-ns/a")},
			limit:           50,
			fullyConsistent: true,
			check: func(t *testing.T, capture *capturingPermsServer, got InteractableSessions, err error) {
				require.NoError(t, err, "LookupInteractableSessions")
				reqs := capture.capturedLookupResources()
				require.Len(t, reqs, 1, "exactly one LookupResources RPC")
				assert.True(t, reqs[0].GetConsistency().GetFullyConsistent(),
					"a gate read must request FullyConsistent")
			},
		},
		{
			name: "the limit is passed through and reaching it reports Truncated",
			scripted: []*v1.LookupResourcesResponse{
				hasPermission("demo-ns/a"),
				hasPermission("demo-ns/b"),
			},
			limit: 2,
			check: func(t *testing.T, capture *capturingPermsServer, got InteractableSessions, err error) {
				require.NoError(t, err, "LookupInteractableSessions")
				reqs := capture.capturedLookupResources()
				require.Len(t, reqs, 1, "exactly one LookupResources RPC")
				assert.Equal(t, uint32(2), reqs[0].GetOptionalLimit())
				assert.True(t, got.Truncated, "the returned count equals the limit, so the list must self-report as a prefix")
			},
		},
		{
			name:     "subject is user:<canonical>, permission interact on agentsession",
			scripted: []*v1.LookupResourcesResponse{hasPermission("demo-ns/a")},
			limit:    50,
			check: func(t *testing.T, capture *capturingPermsServer, got InteractableSessions, err error) {
				require.NoError(t, err, "LookupInteractableSessions")
				reqs := capture.capturedLookupResources()
				require.Len(t, reqs, 1, "exactly one LookupResources RPC")
				req := reqs[0]
				assert.Equal(t, "agentsession", req.GetResourceObjectType())
				assert.Equal(t, "interact", req.GetPermission())
				assert.Equal(t, "user", req.GetSubject().GetObject().GetObjectType())
				assert.Equal(t, demoCanonical, req.GetSubject().GetObject().GetObjectId())
			},
		},
		{
			name:     "a mid-stream Recv error is returned, not partially swallowed",
			scripted: []*v1.LookupResourcesResponse{hasPermission("demo-ns/a")},
			recvErr:  streamErr,
			limit:    50,
			check: func(t *testing.T, _ *capturingPermsServer, got InteractableSessions, err error) {
				require.Error(t, err, "LookupInteractableSessions must surface the Recv failure")
				assert.Contains(t, err.Error(), "interactable sessions recv",
					"error text must name the failing call, not just be non-nil")
				assert.Equal(t, InteractableSessions{}, got,
					"a caller that ignores err must not be able to render a truncated list as complete")
			},
		},
		{
			name:     "no results: empty, non-nil Refs, nil error",
			scripted: nil,
			limit:    50,
			check: func(t *testing.T, _ *capturingPermsServer, got InteractableSessions, err error) {
				require.NoError(t, err, "an empty answer is legitimate and must not be an error")
				assert.NotNil(t, got.Refs)
				assert.Empty(t, got.Refs)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, capture := newCapturingClient(t)
			capture.setLookupResourcesScript(tc.scripted, tc.recvErr)

			got, err := c.LookupInteractableSessions(context.Background(), identity.CanonicalFromTrusted(demoCanonical, "test fixture"), tc.limit, tc.fullyConsistent)
			tc.check(t, capture, got, err)
		})
	}
}
