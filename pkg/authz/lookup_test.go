package authz_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

type fakeLookup struct {
	approvers            []string
	subjects             map[string][]string // per-subject-ref results; preferred when set
	interactParticipants []string
	subjectIncludes      bool
	subjectIncludesErr   error
}

func (f *fakeLookup) LookupSubjects(_ context.Context, ref string) ([]string, error) {
	if f.subjects != nil {
		return f.subjects[ref], nil
	}
	return f.approvers, nil
}
func (f *fakeLookup) LookupInteractSubjects(_ context.Context, _, _ string) ([]string, error) {
	return f.interactParticipants, nil
}
func (f *fakeLookup) LookupSubjectIncludes(_ context.Context, _ string, _ identity.CanonicalUserID) (bool, error) {
	return f.subjectIncludes, f.subjectIncludesErr
}

func TestLookupApprovers(t *testing.T) {
	f := &fakeLookup{approvers: []string{"alice", "bob"}}
	got, err := authz.LookupApprovers(context.Background(), f, "group:eng#member")
	require.NoError(t, err)
	assert.Equal(t, []string{"alice", "bob"}, got)
}

func TestLookupInteractParticipants(t *testing.T) {
	f := &fakeLookup{interactParticipants: []string{"alice"}}
	got, err := authz.LookupInteractParticipants(context.Background(), f,
		authz.SessionRef{Namespace: "ns", Name: "n"})
	require.NoError(t, err)
	assert.Equal(t, []string{"alice"}, got)
}

func TestLookup_NilLookuperReturnsNil(t *testing.T) {
	gotA, errA := authz.LookupApprovers(context.Background(), nil, "x")
	require.NoError(t, errA)
	assert.Nil(t, gotA)
	gotI, errI := authz.LookupInteractParticipants(context.Background(), nil, authz.SessionRef{})
	require.NoError(t, errI)
	assert.Nil(t, gotI)
}

func TestLookupSubjectIncludes_DelegatesToLookuper(t *testing.T) {
	f := &fakeLookup{subjectIncludes: true}
	got, err := authz.LookupSubjectIncludes(context.Background(), f, "group:eng#member", identity.CanonicalFromTrusted("alice", "test fixture"))
	require.NoError(t, err)
	assert.True(t, got)
}

// The eligibility rule under test (see ResolveApprovers): when source-resource
// owner-sets are present they REPLACE the session approve-set — the resource
// owner vouches for access to their resource regardless of session standing —
// and the pool is the UNION across the sets (any single owner can approve).
// The session approve-set is only the pool for owner-only gates (no resources).
func TestResolveApprovers(t *testing.T) {
	f := &fakeLookup{subjects: map[string][]string{
		"agentsession:ns/s#approve": {"user:joey"},
		"crm_company:acme#owner":    {"user:louis"},
		"crm_company:globex#owner":  {"user:louis", "user:marie"},
		"crm_company:unowned#owner": {},
	}}
	ctx := context.Background()

	cases := []struct {
		name          string
		ownerSets     []string
		wantApprovers []string
		wantEmpty     bool
	}{
		{
			name:          "no resource owner-sets: session approve-set is the pool",
			ownerSets:     nil,
			wantApprovers: []string{"user:joey"},
		},
		{
			name:          "resource owner-set REPLACES session set: louis eligible without session standing",
			ownerSets:     []string{"crm_company:acme#owner"},
			wantApprovers: []string{"user:louis"},
		},
		{
			name:      "unowned resource: fail-closed empty",
			ownerSets: []string{"crm_company:unowned#owner"},
			wantEmpty: true,
		},
		{
			name:          "multiple resources: UNION across owner-sets, deduped (any owner approves)",
			ownerSets:     []string{"crm_company:acme#owner", "crm_company:globex#owner"},
			wantApprovers: []string{"user:louis", "user:marie"},
		},
		{
			name:          "an unowned resource does not empty the pool when a sibling has owners",
			ownerSets:     []string{"crm_company:acme#owner", "crm_company:unowned#owner"},
			wantApprovers: []string{"user:louis"},
		},
		{
			name:      "all resources unowned: fail-closed empty",
			ownerSets: []string{"crm_company:unowned#owner", "crm_company:unowned#owner"},
			wantEmpty: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, empty, err := authz.ResolveApprovers(ctx, f, "agentsession:ns/s#approve", tc.ownerSets)
			require.NoError(t, err)
			assert.Equal(t, tc.wantEmpty, empty)
			if !tc.wantEmpty {
				assert.ElementsMatch(t, tc.wantApprovers, got)
			}
		})
	}
}

func TestLookupSubjectIncludes_NilLookuperReturnsFalse(t *testing.T) {
	got, err := authz.LookupSubjectIncludes(context.Background(), nil, "group:eng#member", identity.CanonicalFromTrusted("alice", "test fixture"))
	require.NoError(t, err)
	assert.False(t, got)
}
