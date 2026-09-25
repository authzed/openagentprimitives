package authz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

type fakeGranter struct {
	started, interact, denied struct {
		ns, name, subject string
		called            bool
	}
	owner struct {
		ns, name, subjectRef string
		called               bool
	}
	participantUser struct {
		ns, name, canonicalID string
		called                bool
	}
	interactor struct {
		classNS, className, subjectRef string
		called                         bool
	}
}

func (f *fakeGranter) TouchStartedBy(_ context.Context, ns, name string, canonicalID identity.CanonicalUserID) error {
	f.started.ns, f.started.name, f.started.subject, f.started.called = ns, name, canonicalID.String(), true
	return nil
}
func (f *fakeGranter) TouchOwner(_ context.Context, ns, name, subjectRef string) error {
	f.owner.ns, f.owner.name, f.owner.subjectRef, f.owner.called = ns, name, subjectRef, true
	return nil
}
func (f *fakeGranter) TouchInteractParticipant(_ context.Context, ns, name, subject string) error {
	f.interact.ns, f.interact.name, f.interact.subject, f.interact.called = ns, name, subject, true
	return nil
}
func (f *fakeGranter) TouchInteractParticipantUser(_ context.Context, ns, name string, canonicalID identity.CanonicalUserID) error {
	f.participantUser.ns, f.participantUser.name, f.participantUser.canonicalID, f.participantUser.called = ns, name, canonicalID.String(), true
	return nil
}
func (f *fakeGranter) TouchDeniedUser(_ context.Context, ns, name string, canonicalID identity.CanonicalUserID) error {
	f.denied.ns, f.denied.name, f.denied.subject, f.denied.called = ns, name, canonicalID.String(), true
	return nil
}
func (f *fakeGranter) TouchInteractor(_ context.Context, classNS, className, subjectRef string) error {
	f.interactor.classNS, f.interactor.className, f.interactor.subjectRef, f.interactor.called = classNS, className, subjectRef, true
	return nil
}

func TestTouch_DelegatesToSpiceDB(t *testing.T) {
	g := &fakeGranter{}
	scope := authz.SessionRef{Namespace: "ns", Name: "n"}
	require.NoError(t, authz.TouchStartedBy(context.Background(), g, scope, identity.CanonicalFromTrusted("alice", "test fixture")))
	require.NoError(t, authz.TouchInteractParticipant(context.Background(), g, scope, "group:eng#member"))
	require.NoError(t, authz.TouchDeniedUser(context.Background(), g, scope, identity.CanonicalFromTrusted("mallory", "test fixture")))
	assert.Equal(t, "alice", g.started.subject)
	assert.Equal(t, "group:eng#member", g.interact.subject)
	assert.Equal(t, "mallory", g.denied.subject)
}

func TestTouchInteractor_DelegatesToSpiceDB(t *testing.T) {
	g := &fakeGranter{}
	require.NoError(t, authz.TouchInteractor(context.Background(), g, "ns", "demo-class", "user:alice"))
	assert.True(t, g.interactor.called)
	assert.Equal(t, "ns", g.interactor.classNS)
	assert.Equal(t, "demo-class", g.interactor.className)
	assert.Equal(t, "user:alice", g.interactor.subjectRef)
}

func TestTouchInteractor_NilGranterNoOp(t *testing.T) {
	require.NoError(t, authz.TouchInteractor(context.Background(), nil, "ns", "demo-class", "user:alice"))
}

func TestTouchInteractParticipantUser_DelegatesToSpiceDB(t *testing.T) {
	g := &fakeGranter{}
	scope := authz.SessionRef{Namespace: "ns", Name: "n"}
	require.NoError(t, authz.TouchInteractParticipantUser(context.Background(), g, scope, identity.CanonicalFromTrusted("canonical-abc123", "test fixture")))
	assert.True(t, g.participantUser.called)
	assert.Equal(t, "ns", g.participantUser.ns)
	assert.Equal(t, "n", g.participantUser.name)
	assert.Equal(t, "canonical-abc123", g.participantUser.canonicalID)
}

func TestTouchInteractParticipantUser_NilGranterNoOp(t *testing.T) {
	assert.NoError(t, authz.TouchInteractParticipantUser(context.Background(), nil,
		authz.SessionRef{Namespace: "ns", Name: "n"}, identity.CanonicalFromTrusted("canonical-abc", "test fixture")))
}

type fakeRelWriter struct {
	writes  []authz.Relation
	deletes []authz.Relation
}

func (f *fakeRelWriter) WriteRelationships(_ context.Context, rels []authz.Relation) error {
	f.writes = append(f.writes, rels...)
	return nil
}
func (f *fakeRelWriter) DeleteRelationships(_ context.Context, rels []authz.Relation) error {
	f.deletes = append(f.deletes, rels...)
	return nil
}

func TestGrantRevoke_DelegatesAndShortCircuits(t *testing.T) {
	w := &fakeRelWriter{}
	rels := []authz.Relation{{ResourceType: "github_repo", ResourceID: "a/b", Relation: "viewer", SubjectType: "user", SubjectID: "alice"}}
	require.NoError(t, authz.Grant(context.Background(), w, rels))
	assert.Equal(t, rels, w.writes)
	require.NoError(t, authz.Revoke(context.Background(), w, rels))
	assert.Equal(t, rels, w.deletes)

	// empty list no-op
	require.NoError(t, authz.Grant(context.Background(), w, nil))
	require.NoError(t, authz.Revoke(context.Background(), w, nil))
	assert.Equal(t, rels, w.writes, "no additional writes")
	assert.Equal(t, rels, w.deletes, "no additional deletes")
}

func TestGrantRevoke_NilWriterNoOp(t *testing.T) {
	rels := []authz.Relation{{ResourceType: "x", ResourceID: "y", Relation: "z", SubjectType: "user", SubjectID: "alice"}}
	assert.NoError(t, authz.Grant(context.Background(), nil, rels))
	assert.NoError(t, authz.Revoke(context.Background(), nil, rels))
}

// recordingGranter collects every TouchDeniedUser call so CopyDeniedUsers'
// per-user copy can be asserted (the shared fakeGranter keeps only the last).
type recordingGranter struct {
	fakeGranter
	deniedTouched []string // "ns/name|canonicalID"
}

func (r *recordingGranter) TouchDeniedUser(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) error {
	r.deniedTouched = append(r.deniedTouched, ns+"/"+name+"|"+canonicalID.String())
	return r.fakeGranter.TouchDeniedUser(ctx, ns, name, canonicalID)
}

type fakeDeniedLister struct {
	denied []string
	err    error
}

func (f fakeDeniedLister) ListDeniedUsers(_ context.Context, _, _ string) ([]string, error) {
	return f.denied, f.err
}

func TestCopyDeniedUsers_CopiesEachParentDeniedToChild(t *testing.T) {
	g := &recordingGranter{}
	l := fakeDeniedLister{denied: []string{"mallory", "trudy"}}
	src := authz.SessionRef{Namespace: "ns", Name: "parent"}
	dst := authz.SessionRef{Namespace: "ns", Name: "child"}

	require.NoError(t, authz.CopyDeniedUsers(context.Background(), l, g, src, dst))
	assert.ElementsMatch(t, []string{"ns/child|mallory", "ns/child|trudy"}, g.deniedTouched,
		"every parent-denied user must be denied on the child")
}

func TestCopyDeniedUsers_NilDepsFailClosed(t *testing.T) {
	l := fakeDeniedLister{denied: []string{"mallory"}}
	// Fail-closed: a missing dependency must NOT silently skip the copy (that
	// would fork a child with a weaker blocklist — a transcript-read leak).
	assert.Error(t, authz.CopyDeniedUsers(context.Background(), nil, &recordingGranter{},
		authz.SessionRef{Namespace: "ns", Name: "p"}, authz.SessionRef{Namespace: "ns", Name: "c"}))
	assert.Error(t, authz.CopyDeniedUsers(context.Background(), l, nil,
		authz.SessionRef{Namespace: "ns", Name: "p"}, authz.SessionRef{Namespace: "ns", Name: "c"}))
}

func TestCopyDeniedUsers_ListErrorPropagates(t *testing.T) {
	g := &recordingGranter{}
	l := fakeDeniedLister{err: errors.New("spicedb read down")}
	// A read failure must abort the fork, not proceed with an empty blocklist.
	assert.Error(t, authz.CopyDeniedUsers(context.Background(), l, g,
		authz.SessionRef{Namespace: "ns", Name: "p"}, authz.SessionRef{Namespace: "ns", Name: "c"}))
	assert.Empty(t, g.deniedTouched, "no child denied touched when the parent read failed")
}
