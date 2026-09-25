package relwrites

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// fakeSlotGrantLister stands in for (*spicedb.Client): it records the
// (ns, name) it was asked about and returns a canned grant set.
type fakeSlotGrantLister struct {
	grants  []authz.SlotBinding
	err     error
	calls   int
	gotNS   string
	gotName string
}

func (f *fakeSlotGrantLister) ListSlotGrants(_ context.Context, ns, name string) ([]authz.SlotBinding, error) {
	f.calls++
	f.gotNS, f.gotName = ns, name
	return f.grants, f.err
}

// TestNewSlotBoundChecker_ApprovesAGrantedResource pins the positive case and
// the session the grants are read for: the checker asks about the session it
// was built for, and approves a resource that session holds a grant on.
func TestNewSlotBoundChecker_ApprovesAGrantedResource(t *testing.T) {
	lister := &fakeSlotGrantLister{grants: []authz.SlotBinding{
		{ResourceType: "pull_request", ResourceID: authz.TrustedObjectID("owner-repo-7"), Permission: "read"},
	}}
	checker := NewSlotBoundChecker(lister, "ns", "sess")
	require.NotNil(t, checker)

	ok, err := checker(context.Background(), "pull_request:owner-repo-7")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "ns", lister.gotNS)
	assert.Equal(t, "sess", lister.gotName)
}

// TestNewSlotBoundChecker_PermissionIsDeliberatelyIgnored pins the ruling: a
// grant is a human act naming the INSTANCE, so any slot_grant_* on the
// resource binds it. Which permission the grant carries is the pool
// machinery's concern, not this gate's — a checker that compared permissions
// would refuse a write on an instance a human had already named.
func TestNewSlotBoundChecker_PermissionIsDeliberatelyIgnored(t *testing.T) {
	lister := &fakeSlotGrantLister{grants: []authz.SlotBinding{
		{ResourceType: "pull_request", ResourceID: authz.TrustedObjectID("owner-repo-7"), Permission: "read"},
	}}
	checker := NewSlotBoundChecker(lister, "ns", "sess")

	ok, err := checker(context.Background(), "pull_request:owner-repo-7")
	require.NoError(t, err)
	assert.True(t, ok, "a read-permission grant still names the instance, so the instance is slot-bound")
}

// TestNewSlotBoundChecker_RefusesAnUngrantedResource proves a resource the
// session holds no grant on is refused — including one whose TYPE matches a
// held grant but whose id does not, and one whose ID matches but whose type
// does not. Comparing only half the reference is how a gate lets a
// neighbouring instance through.
func TestNewSlotBoundChecker_RefusesAnUngrantedResource(t *testing.T) {
	lister := &fakeSlotGrantLister{grants: []authz.SlotBinding{
		{ResourceType: "pull_request", ResourceID: authz.TrustedObjectID("owner-repo-7"), Permission: "read"},
	}}
	checker := NewSlotBoundChecker(lister, "ns", "sess")

	for _, resource := range []string{
		"pull_request:owner-repo-8", // same type, different instance
		"issue:owner-repo-7",        // same id, different type
		"pull_request:",             // degenerate
		"owner-repo-7",              // not a reference at all
	} {
		ok, err := checker(context.Background(), resource)
		require.NoError(t, err, resource)
		assert.False(t, ok, "%s must not be slot-bound", resource)
	}
}

// TestNewSlotBoundChecker_ReadErrorIsReturnedNotSwallowed proves an unreadable
// grant set surfaces as an error. Run treats that exactly like a false, so a
// swallowed error would fail OPEN.
func TestNewSlotBoundChecker_ReadErrorIsReturnedNotSwallowed(t *testing.T) {
	lister := &fakeSlotGrantLister{err: fmt.Errorf("spicedb unavailable")}
	checker := NewSlotBoundChecker(lister, "ns", "sess")

	ok, err := checker(context.Background(), "pull_request:owner-repo-7")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spicedb unavailable")
	assert.False(t, ok)
}

// TestNewSlotBoundChecker_NilListerYieldsANilChecker proves the constructor
// degrades into exactly the unwired case Run already refuses loudly, rather
// than into a checker that approves. A runner with no SpiceDB must not be a
// runner where a marked block writes unchecked.
func TestNewSlotBoundChecker_NilListerYieldsANilChecker(t *testing.T) {
	assert.Nil(t, NewSlotBoundChecker(nil, "ns", "sess"),
		"a nil lister must produce a nil checker so Run's unwired refusal fires")

	blocks := []Block{{RequireSlotBound: true, Tuple: Tuple{Resource: `"a:1"`, Relation: `"r"`, Subject: `"b:1"`}}}
	w := &fakeWriter{}
	_, err := Run(context.Background(), w, blocks, nil, NewSlotBoundChecker(nil, "ns", "sess"), func(string, ...any) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unwired")
	assert.Empty(t, w.got)
}

// TestNewSlotBoundChecker_IsNotCached proves the grant set is re-read per
// call. A grant revoked mid-session must stop authorizing the very next
// write, which a memoized first answer would not do.
func TestNewSlotBoundChecker_IsNotCached(t *testing.T) {
	lister := &fakeSlotGrantLister{grants: []authz.SlotBinding{
		{ResourceType: "pull_request", ResourceID: authz.TrustedObjectID("7")},
	}}
	checker := NewSlotBoundChecker(lister, "ns", "sess")

	ok, err := checker(context.Background(), "pull_request:7")
	require.NoError(t, err)
	require.True(t, ok)

	lister.grants = nil // revoked
	ok, err = checker(context.Background(), "pull_request:7")
	require.NoError(t, err)
	assert.False(t, ok, "a revoked grant must stop authorizing on the next call")
	assert.Equal(t, 2, lister.calls, "the grant set must be re-read per call, never cached")
}
