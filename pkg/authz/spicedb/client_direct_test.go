// Pure-Go unit tests for the directUserSubject short-circuit in
// LookupSubjectIncludes and LookupSubjects. These tests do NOT
// require a real SpiceDB endpoint and carry no build tag so they run
// in every CI pass.
package spicedb

import (
	"context"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDirectUserSubject_Helper exercises the package-private helper
// directly to pin its parsing contract.
func TestDirectUserSubject_Helper(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantCanon string
		wantOK    bool
	}{
		{"plain user", "user:alice", "alice", true},
		{"empty trailing hash (equivalent to plain)", "user:alice#", "alice", true},
		{"subject-set relation → not direct", "user:alice#member", "", false},
		{"non-user type → not direct", "group:eng", "", false},
		{"non-user subject-set", "group:eng#member", "", false},
		{"empty string", "", "", false},
		{"user: with empty id", "user:", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			canon, ok := directUserSubject(tc.in)
			assert.Equal(t, tc.wantOK, ok, "ok")
			assert.Equal(t, tc.wantCanon, canon, "canonical")
		})
	}
}

// TestLookupSubjectIncludes_DirectUserSubject verifies the short-circuit:
// a direct "user:<id>" subjectRef must NOT call SpiceDB (c.cl is nil
// — any attempt to use it would panic), and must return the correct
// true/false based on whether the canonicalID matches.
func TestLookupSubjectIncludes_DirectUserSubject(t *testing.T) {
	c := &Client{} // cl field is nil; direct-subject path must not touch SpiceDB

	ok, err := c.LookupSubjectIncludes(context.Background(), "user:alice", identity.CanonicalFromTrusted("alice", "test fixture"))
	require.NoError(t, err, "direct subject: matching canonical must not error")
	assert.True(t, ok, "user:alice includes alice")

	ok, err = c.LookupSubjectIncludes(context.Background(), "user:alice", identity.CanonicalFromTrusted("bob", "test fixture"))
	require.NoError(t, err, "direct subject: non-matching canonical must not error")
	assert.False(t, ok, "user:alice does not include bob")

	// Empty trailing # is also a direct subject.
	ok, err = c.LookupSubjectIncludes(context.Background(), "user:alice#", identity.CanonicalFromTrusted("alice", "test fixture"))
	require.NoError(t, err, "trailing empty hash: must not error")
	assert.True(t, ok, "user:alice# includes alice")
}

// TestLookupSubjects_DirectUserSubject verifies the short-circuit:
// a direct "user:<id>" subjectRef returns a single-element slice
// without touching SpiceDB.
func TestLookupSubjects_DirectUserSubject(t *testing.T) {
	c := &Client{} // cl is nil; direct-subject path must not touch SpiceDB

	got, err := c.LookupSubjects(context.Background(), "user:alice")
	require.NoError(t, err, "direct subject: must not error")
	assert.Equal(t, []string{"alice"}, got, "single canonical returned")

	// Empty trailing # is also a direct subject.
	got, err = c.LookupSubjects(context.Background(), "user:bob#")
	require.NoError(t, err, "trailing empty hash: must not error")
	assert.Equal(t, []string{"bob"}, got, "canonical without trailing hash")
}
