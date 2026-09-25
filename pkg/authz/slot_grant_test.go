package authz_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

type recordingRelWriter struct {
	wrote   []authz.Relation
	deleted []authz.Relation
	err     error
}

func (r *recordingRelWriter) WriteRelationships(_ context.Context, rels []authz.Relation) error {
	if r.err != nil {
		return r.err
	}
	r.wrote = append(r.wrote, rels...)
	return nil
}

func (r *recordingRelWriter) DeleteRelationships(_ context.Context, rels []authz.Relation) error {
	if r.err != nil {
		return r.err
	}
	r.deleted = append(r.deleted, rels...)
	return nil
}

func testScope() authz.SessionRef { return authz.SessionRef{Namespace: "default", Name: "sess-1"} }

// testExpiry is any non-zero expiry; GrantSlots refuses a zero one because the
// schema declares slot_grant `with expiration`.
func testExpiry() time.Time { return time.Unix(1700000000, 0).UTC() }

// The tuple points RESOURCE → SESSION. Asserted explicitly because the mirror
// image is a real shape in this codebase (the session-grant mechanism slots
// replace), and getting the direction backwards would still compile, still
// write a tuple, and silently reintroduce the need for a wildcard leaf to make
// any Check pass.
func TestSlotGrantRelation_pointsFromResourceToSession(t *testing.T) {
	rel := authz.SlotGrantRelation("crm_company", "acme", "contact_access", testScope())

	assert.Equal(t, "crm_company", rel.ResourceType, "the RESOURCE holds the tuple")
	assert.Equal(t, "acme", rel.ResourceID)
	assert.Equal(t, authz.SlotGrantRelationName("contact_access"), rel.Relation,
		"per-permission: a read grant must not be the same tuple as a write grant")
	assert.Equal(t, "agentsession", rel.SubjectType, "and the SESSION is the subject")
	assert.Equal(t, "default/sess-1", rel.SubjectID)
	assert.Empty(t, rel.SubjectRel,
		"the subject is the session object itself; the schema arrow (->interact) resolves its members")
}

func TestGrantSlots_writesOnePerBinding(t *testing.T) {
	w := &recordingRelWriter{}
	require.NoError(t, authz.GrantSlots(context.Background(), w, testScope(), []authz.SlotBinding{
		{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"},
		{ResourceType: "tracker_issue", ResourceID: authz.TrustedObjectID("L-140"), Permission: "write"},
	}, testExpiry()))

	require.Len(t, w.wrote, 2)
	assert.Equal(t, "acme", w.wrote[0].ResourceID)
	assert.Equal(t, "L-140", w.wrote[1].ResourceID)
	for _, r := range w.wrote {
		assert.Equal(t, "default/sess-1", r.SubjectID, "every grant binds to the same session")
	}
}

// An empty id would write a tuple naming no instance: it grants nothing and
// cannot be revoked by id afterwards. Refused before any write, so a bad entry
// cannot leave a partial set behind.
func TestGrantSlots_refusesAnEmptyIdentifierWithoutWritingAnything(t *testing.T) {
	for _, tc := range []struct {
		name    string
		binding authz.SlotBinding
	}{
		{"empty resourceID", authz.SlotBinding{ResourceType: "crm_company", Permission: "contact_access"}},
		{"empty resourceType", authz.SlotBinding{ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"}},
	} {
		t.Run(tc.name+": refused", func(t *testing.T) {
			w := &recordingRelWriter{}
			err := authz.GrantSlots(context.Background(), w, testScope(), []authz.SlotBinding{
				{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("good"), Permission: "contact_access"},
				tc.binding,
			}, testExpiry())
			require.Error(t, err)
			assert.Empty(t, w.wrote,
				"nothing may be written when any binding in the set is malformed")
		})
	}
}

// A write failure must surface. A silently-dropped grant leaves the agent
// believing it holds reach it does not, which shows up later as an inexplicable
// mid-task denial rather than a failure at bind time.
func TestGrantSlots_propagatesTheWriteError(t *testing.T) {
	w := &recordingRelWriter{err: errors.New("spicedb unavailable")}
	err := authz.GrantSlots(context.Background(), w, testScope(),
		[]authz.SlotBinding{{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"}}, testExpiry())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spicedb unavailable")
}

// Every grant that can be written must be revocable, or a binding is a one-way
// door for the life of the session.
func TestRevokeSlots_deletesTheSameTupleItWould(t *testing.T) {
	w := &recordingRelWriter{}
	binding := []authz.SlotBinding{{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"}}

	require.NoError(t, authz.GrantSlots(context.Background(), w, testScope(), binding, testExpiry()))
	require.NoError(t, authz.RevokeSlots(context.Background(), w, testScope(), binding))

	require.Len(t, w.wrote, 1)
	require.Len(t, w.deleted, 1)

	// Compared on IDENTITY, not the whole struct: a tuple is identified by
	// (resource, relation, subject), and the expiry is not part of that. A
	// revoker must be able to remove a grant without knowing when it was due
	// to lapse — requiring the expiry to match would make a grant unrevocable
	// by anyone who did not write it.
	got, want := w.deleted[0], w.wrote[0]
	want.ExpiresAt = got.ExpiresAt
	assert.Equal(t, want, got,
		"revoke must target exactly the tuple grant wrote, or it silently removes nothing")
	assert.True(t, got.ExpiresAt.IsZero(), "revoke names no expiry; it is not part of tuple identity")
}

// Nil writer / empty set are no-ops rather than errors: a class declaring no
// slots is the common case and must not need a guard at every call site.
func TestGrantSlots_nilWriterAndEmptySetAreNoOps(t *testing.T) {
	require.NoError(t, authz.GrantSlots(context.Background(), nil, testScope(),
		[]authz.SlotBinding{{ResourceType: "x", ResourceID: authz.TrustedObjectID("y"), Permission: "p"}}, testExpiry()))

	w := &recordingRelWriter{}
	require.NoError(t, authz.GrantSlots(context.Background(), w, testScope(), nil, testExpiry()))
	assert.Empty(t, w.wrote)
}

// TestGrantSlots_RefusesAZeroExpiry: the composed schema declares slot_grant
// `with expiration`, so SpiceDB would reject the write anyway — the point of
// catching it here is that the caller learns WHY instead of getting a caveat
// error from three layers down. An indefinite slot grant is the leak the
// expiry exists to backstop, so it must not be expressible.
func TestGrantSlots_RefusesAZeroExpiry(t *testing.T) {
	w := &recordingRelWriter{}
	err := authz.GrantSlots(context.Background(), w, testScope(),
		[]authz.SlotBinding{{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"}}, time.Time{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expiry")
	assert.Empty(t, w.wrote, "nothing may be written without an expiry")
}

func TestGrantSlots_StampsTheExpiryOnEveryTuple(t *testing.T) {
	w := &recordingRelWriter{}
	exp := testExpiry()
	require.NoError(t, authz.GrantSlots(context.Background(), w, testScope(), []authz.SlotBinding{
		{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"},
		{ResourceType: "tracker_issue", ResourceID: authz.TrustedObjectID("L-140"), Permission: "write"},
	}, exp))

	require.Len(t, w.wrote, 2)
	for _, r := range w.wrote {
		assert.Equal(t, exp, r.ExpiresAt, "every grant carries the expiry, not just the first")
	}
}

// GrantSlots must refuse a binding whose id was never derived, because a zero
// ObjectID names no instance and the resulting tuple could not be revoked.
func TestGrantSlots_RefusesAZeroObjectID(t *testing.T) {
	w := &recordingRelWriter{}
	err := authz.GrantSlots(context.Background(), w,
		authz.SessionRef{Namespace: "default", Name: "demo-session"},
		[]authz.SlotBinding{{ResourceType: "git_repo", Permission: "push"}}, // ResourceID left zero
		time.Now().Add(time.Hour))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resourceID")
	assert.Empty(t, w.wrote, "nothing may be written when any binding is invalid")
}

func TestGrantSlots_WritesTheDerivedID(t *testing.T) {
	id, err := authz.NewObjectID("https://github.com/acme/app", []string{"normalize_url", "spicedb_escape"})
	require.NoError(t, err)

	w := &recordingRelWriter{}
	require.NoError(t, authz.GrantSlots(context.Background(), w,
		authz.SessionRef{Namespace: "default", Name: "demo-session"},
		[]authz.SlotBinding{{ResourceType: "git_repo", ResourceID: id, Permission: "push"}},
		time.Now().Add(time.Hour)))

	require.Len(t, w.wrote, 1)
	assert.Equal(t, "https=3A//github=2Ecom/acme/app", w.wrote[0].ResourceID,
		"the escaped form reaches SpiceDB, never the raw URL")
	assert.Equal(t, "slot_grant_push", w.wrote[0].Relation)
}

// SlotGrantExpiry anchors to the session's WALL-CLOCK cap, and a session that
// declares none must still get a bounded grant — zero is not "forever".
func TestSlotGrantExpiry(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	assert.Equal(t, now.Add(2*time.Hour), authz.SlotGrantExpiry(now, 2*time.Hour),
		"a declared cap is used as-is")
	assert.Equal(t, now.Add(authz.DefaultSlotGrantTTL), authz.SlotGrantExpiry(now, 0),
		"no declared cap must NOT mean no expiry")
	assert.Equal(t, now.Add(authz.DefaultSlotGrantTTL), authz.SlotGrantExpiry(now, -time.Hour),
		"a negative cap is nonsense and must not produce a past expiry")
}

// fakeSlotCopier records what a fork carried.
type fakeSlotCopier struct {
	held      []authz.SlotBinding
	listErr   error
	grantErr  error
	grantedTo string
	granted   []authz.SlotBinding
	expiry    time.Time
}

func (f *fakeSlotCopier) ListSlotGrants(_ context.Context, ns, name string) ([]authz.SlotBinding, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.held, nil
}

func (f *fakeSlotCopier) GrantSlots(_ context.Context, ns, name string, b []authz.SlotBinding, exp time.Time) error {
	if f.grantErr != nil {
		return f.grantErr
	}
	f.grantedTo = ns + "/" + name
	f.granted = append(f.granted, b...)
	f.expiry = exp
	return nil
}

func TestCopySlotGrants_RePointsTheParentsGrantsAtTheChild(t *testing.T) {
	c := &fakeSlotCopier{held: []authz.SlotBinding{
		{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"},
		{ResourceType: "http_target", ResourceID: authz.TrustedObjectID("hash-1"), Permission: "reachable"},
	}}
	parent := authz.SessionRef{Namespace: "ns", Name: "parent"}
	childRef := authz.SessionRef{Namespace: "ns", Name: "child"}
	exp := testExpiry()

	n, err := authz.CopySlotGrants(context.Background(), c, parent, childRef, exp)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, "ns/child", c.grantedTo,
		"a grant names the SESSION as subject, so a continuation inherits nothing without re-pointing")
	assert.ElementsMatch(t, c.held, c.granted)
	assert.Equal(t, exp, c.expiry, "the child's grants get the child's expiry, not the parent's remaining time")
}

func TestCopySlotGrants_NoGrantsAndNilCopierAreNoOps(t *testing.T) {
	parent := authz.SessionRef{Namespace: "ns", Name: "parent"}
	childRef := authz.SessionRef{Namespace: "ns", Name: "child"}

	n, err := authz.CopySlotGrants(context.Background(), nil, parent, childRef, testExpiry())
	require.NoError(t, err)
	assert.Zero(t, n)

	empty := &fakeSlotCopier{}
	n, err = authz.CopySlotGrants(context.Background(), empty, parent, childRef, testExpiry())
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, empty.granted, "nothing held means nothing written")
}

func TestCopySlotGrants_SurfacesBothFailures(t *testing.T) {
	parent := authz.SessionRef{Namespace: "ns", Name: "parent"}
	childRef := authz.SessionRef{Namespace: "ns", Name: "child"}

	listFail := &fakeSlotCopier{listErr: errors.New("list boom")}
	_, err := authz.CopySlotGrants(context.Background(), listFail, parent, childRef, testExpiry())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list boom")

	grantFail := &fakeSlotCopier{
		held:     []authz.SlotBinding{{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"}},
		grantErr: errors.New("grant boom"),
	}
	_, err = authz.CopySlotGrants(context.Background(), grantFail, parent, childRef, testExpiry())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "grant boom")
}

// fakeRevokeChecker drives the two standing legs independently.
type fakeRevokeChecker struct {
	approve, admin       bool
	approveErr, adminErr error
	approveFC, adminFC   bool
	adminPerm            string
}

func (f *fakeRevokeChecker) CheckApprove(_ context.Context, _, _ string, _ identity.CanonicalUserID, fc bool) (bool, error) {
	f.approveFC = fc
	return f.approve, f.approveErr
}

func (f *fakeRevokeChecker) CheckPlatformPermission(_ context.Context, perm string, _ identity.CanonicalUserID, fc bool) (bool, error) {
	f.adminPerm, f.adminFC = perm, fc
	return f.admin, f.adminErr
}

// Standing to revoke is symmetric with standing to approve, plus platform
// admin. A rule where approving is easier than un-approving leaves people
// unable to undo their own decisions — and then they stop approving carefully.
func TestCheckMayRevokeSlot(t *testing.T) {
	who := identity.CanonicalFromTrusted("alice", "test fixture")
	cases := []struct {
		name string
		c    *fakeRevokeChecker
		want bool
		err  bool
	}{
		{name: "approver may revoke", c: &fakeRevokeChecker{approve: true}, want: true},
		{name: "platform admin may revoke without approve standing", c: &fakeRevokeChecker{admin: true}, want: true},
		{name: "neither: refused", c: &fakeRevokeChecker{}, want: false},
		{
			name: "approve check errors: fail closed, and say why",
			c:    &fakeRevokeChecker{approveErr: errors.New("spicedb down")},
			want: false, err: true,
		},
		{
			// Reached only when approve already said no, so a broken admin
			// check must not quietly grant.
			name: "platform check errors: fail closed",
			c:    &fakeRevokeChecker{adminErr: errors.New("spicedb down")},
			want: false, err: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := authz.CheckMayRevokeSlot(context.Background(), tc.c, testScope(), who)
			if tc.err {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCheckMayRevokeSlot_NoCheckerRefuses(t *testing.T) {
	got, err := authz.CheckMayRevokeSlot(context.Background(), nil, testScope(), identity.CanonicalFromTrusted("alice", "test fixture"))
	require.Error(t, err, "an unwired gate must refuse loudly, not silently allow")
	assert.False(t, got)
}

// Both legs read FULLY CONSISTENT: a caller who was just granted approve
// standing must not be told they lack it, and standing withdrawn a moment ago
// must not still authorize.
func TestCheckMayRevokeSlot_ReadsFullyConsistent(t *testing.T) {
	c := &fakeRevokeChecker{}
	_, err := authz.CheckMayRevokeSlot(context.Background(), c, testScope(), identity.CanonicalFromTrusted("alice", "test fixture"))
	require.NoError(t, err)
	assert.True(t, c.approveFC, "approve leg must be fully consistent")
	assert.True(t, c.adminFC, "platform leg must be fully consistent")
	assert.Equal(t, authz.PlatformKillSession, c.adminPerm,
		"must go through the per-area permission, never can_admin directly")
}
