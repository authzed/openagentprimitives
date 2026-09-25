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

type fakeSessionChecker struct {
	allowed bool
	err     error
	called  struct {
		ns, name, canonicalID string
		fullyConsistent       bool
	}
}

func (f *fakeSessionChecker) CheckInteract(_ context.Context, ns, name string, canonicalID identity.CanonicalUserID, fc bool) (bool, error) {
	f.called.ns = ns
	f.called.name = name
	f.called.canonicalID = canonicalID.String()
	f.called.fullyConsistent = fc
	return f.allowed, f.err
}

func TestCheckSessionInteract_Allowed(t *testing.T) {
	sdb := &fakeSessionChecker{allowed: true}
	got, err := authz.CheckSessionInteract(context.Background(), sdb,
		authz.SessionRef{Namespace: "ns", Name: "n"}, identity.CanonicalFromTrusted("alice", "test fixture"), false)
	require.NoError(t, err)
	assert.True(t, got)
	assert.Equal(t, "ns", sdb.called.ns)
	assert.Equal(t, "n", sdb.called.name)
	assert.Equal(t, "alice", sdb.called.canonicalID)
	assert.False(t, sdb.called.fullyConsistent)
}

func TestCheckSessionInteract_ErrorPropagates(t *testing.T) {
	sentinel := errors.New("spicedb down")
	sdb := &fakeSessionChecker{err: sentinel}
	_, err := authz.CheckSessionInteract(context.Background(), sdb,
		authz.SessionRef{Namespace: "ns", Name: "n"}, identity.CanonicalFromTrusted("alice", "test fixture"), true)
	assert.ErrorIs(t, err, sentinel)
}

func TestCheckSessionInteract_NilCheckerIsNoOp(t *testing.T) {
	got, err := authz.CheckSessionInteract(context.Background(), nil,
		authz.SessionRef{Namespace: "ns", Name: "n"}, identity.CanonicalFromTrusted("alice", "test fixture"), false)
	require.NoError(t, err)
	assert.False(t, got)
}

// fakeInboundChecker records which ARM of the inbound gate was taken and with
// what subject, so a dispatch test can assert the right question was asked
// rather than only that some answer came back.
type fakeInboundChecker struct {
	interactAllowed bool
	converseAllowed bool
	err             error

	interactCalls int
	converseCalls int
	gotCanonical  string
	gotSenderNS   string
	gotSenderName string
	gotFC         bool
}

func (f *fakeInboundChecker) CheckInteract(_ context.Context, _, _ string, canonicalID identity.CanonicalUserID, fc bool) (bool, error) {
	f.interactCalls++
	f.gotCanonical = canonicalID.String()
	f.gotFC = fc
	return f.interactAllowed, f.err
}

func (f *fakeInboundChecker) CheckConverse(_ context.Context, _, _, senderNS, senderName string, fc bool) (bool, error) {
	f.converseCalls++
	f.gotSenderNS, f.gotSenderName = senderNS, senderName
	f.gotFC = fc
	return f.converseAllowed, f.err
}

// TestCheckSessionInbound_DispatchesOnTheActorSubjectType is the unit-level
// guard on the defect that made agent-to-agent inbound permanently denied: one
// gate, two questions, and picking the wrong one is unrecoverable rather than
// merely wrong — a session handed to the user-typed check reaches SpiceDB as
// `user:agentsession:<ns>/<name>`, which no relation can match.
func TestCheckSessionInbound_DispatchesOnTheActorSubjectType(t *testing.T) {
	scope := authz.SessionRef{Namespace: "ns", Name: "child"}

	cases := []struct {
		name  string
		actor identity.Subject
		// Exactly one of these two is expected to have been consulted.
		wantInteractCalls, wantConverseCalls int
		check                                func(t *testing.T, f *fakeInboundChecker)
	}{
		{
			name:              "user subject: asks agentsession#interact with the BARE canonical",
			actor:             "user:YWxpY2U",
			wantInteractCalls: 1,
			check: func(t *testing.T, f *fakeInboundChecker) {
				assert.Equal(t, "YWxpY2U", f.gotCanonical,
					"the type prefix must be stripped; SpiceDB re-adds `user:` around the id")
			},
		},
		{
			name:              "agentsession subject: asks agentsession#converse with the sender split into ns/name",
			actor:             "agentsession:demo-ns/parent-1",
			wantConverseCalls: 1,
			check: func(t *testing.T, f *fakeInboundChecker) {
				assert.Equal(t, "demo-ns", f.gotSenderNS)
				assert.Equal(t, "parent-1", f.gotSenderName)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeInboundChecker{interactAllowed: true, converseAllowed: true}
			ok, err := authz.CheckSessionInbound(context.Background(), f, scope, tc.actor, true)
			require.NoError(t, err)
			assert.True(t, ok)
			assert.Equal(t, tc.wantInteractCalls, f.interactCalls, "interact arm")
			assert.Equal(t, tc.wantConverseCalls, f.converseCalls, "converse arm")
			assert.True(t, f.gotFC, "fullyConsistent must be forwarded, not dropped")
			tc.check(t, f)
		})
	}
}

// TestCheckSessionInbound_FailsClosedOnAnActorItCannotAskAbout covers every
// shape that must reach NEITHER arm. Each returns ErrActorTypeUnsupported so
// the caller can tell "no permission exists for this kind of subject" — which
// no tuple will ever fix — from a backend failure, which is retryable.
//
// service: is the live case, not a hypothetical: a cron/bento Channel asserts
// one, and it only escapes this today because such an inbound normally SPAWNS
// its session and the gate runs on existing sessions only.
func TestCheckSessionInbound_FailsClosedOnAnActorItCannotAskAbout(t *testing.T) {
	scope := authz.SessionRef{Namespace: "ns", Name: "child"}

	cases := []struct {
		name  string
		actor identity.Subject
	}{
		{name: "service subject: no inbound permission admits it", actor: "service:nightly-report"},
		{name: "group subject: a set, not an actor", actor: "group:engineering"},
		{name: "unregistered type: nothing known answers for it", actor: "robot:hal"},
		{name: "no type prefix at all", actor: "just-an-id"},
		{name: "empty actor: no principal to ask about", actor: ""},
		{name: "agentsession id with no namespace separator", actor: "agentsession:parent-1"},
		{name: "agentsession id with an empty namespace", actor: "agentsession:/parent-1"},
		{name: "agentsession id with an empty name", actor: "agentsession:demo-ns/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Both arms would answer TRUE if reached, so a false here is the
			// dispatch refusing rather than the backend denying.
			f := &fakeInboundChecker{interactAllowed: true, converseAllowed: true}
			ok, err := authz.CheckSessionInbound(context.Background(), f, scope, tc.actor, true)
			assert.False(t, ok)
			require.ErrorIs(t, err, authz.ErrActorTypeUnsupported)
			assert.Zero(t, f.interactCalls, "must not reach the human arm")
			assert.Zero(t, f.converseCalls, "must not reach the agent-to-agent arm")
		})
	}
}

func TestCheckSessionInbound_BackendErrorPropagatesAndIsNotTheUnsupportedSentinel(t *testing.T) {
	sentinel := errors.New("spicedb down")
	f := &fakeInboundChecker{err: sentinel}
	ok, err := authz.CheckSessionInbound(context.Background(), f,
		authz.SessionRef{Namespace: "ns", Name: "child"}, "agentsession:demo-ns/parent-1", true)
	assert.False(t, ok)
	require.ErrorIs(t, err, sentinel)
	assert.NotErrorIs(t, err, authz.ErrActorTypeUnsupported,
		"a backend outage is retryable and must not be reported as a permanent refusal")
}

func TestCheckSessionInbound_NilCheckerIsNoOp(t *testing.T) {
	ok, err := authz.CheckSessionInbound(context.Background(), nil,
		authz.SessionRef{Namespace: "ns", Name: "child"}, "user:YWxpY2U", true)
	require.NoError(t, err)
	assert.False(t, ok)
}

type fakeManageScopeChecker struct {
	allowed bool
	err     error
	called  struct {
		ns, name, canonicalID string
		fullyConsistent       bool
	}
}

func (f *fakeManageScopeChecker) CheckManageScope(_ context.Context, ns, name string, canonicalID identity.CanonicalUserID, fc bool) (bool, error) {
	f.called.ns = ns
	f.called.name = name
	f.called.canonicalID = canonicalID.String()
	f.called.fullyConsistent = fc
	return f.allowed, f.err
}

func TestCheckSessionManageScope_OwnerAllowed(t *testing.T) {
	sdb := &fakeManageScopeChecker{allowed: true}
	got, err := authz.CheckSessionManageScope(context.Background(), sdb,
		authz.SessionRef{Namespace: "ns", Name: "n"}, identity.CanonicalFromTrusted("user:alice", "test fixture"), true)
	require.NoError(t, err)
	assert.True(t, got)
	assert.Equal(t, "ns", sdb.called.ns)
	assert.Equal(t, "n", sdb.called.name)
	assert.Equal(t, "user:alice", sdb.called.canonicalID)
	assert.True(t, sdb.called.fullyConsistent, "manage_scope must be checked fully-consistent")
}

func TestCheckSessionManageScope_NonOwnerDenied(t *testing.T) {
	sdb := &fakeManageScopeChecker{allowed: false}
	got, err := authz.CheckSessionManageScope(context.Background(), sdb,
		authz.SessionRef{Namespace: "ns", Name: "n"}, identity.CanonicalFromTrusted("user:bob", "test fixture"), true)
	require.NoError(t, err)
	assert.False(t, got, "a non-owner must be denied manage_scope")
}

func TestCheckSessionManageScope_ErrorPropagates(t *testing.T) {
	sentinel := errors.New("spicedb down")
	sdb := &fakeManageScopeChecker{err: sentinel}
	_, err := authz.CheckSessionManageScope(context.Background(), sdb,
		authz.SessionRef{Namespace: "ns", Name: "n"}, identity.CanonicalFromTrusted("user:alice", "test fixture"), true)
	assert.ErrorIs(t, err, sentinel)
}

func TestCheckSessionManageScope_NilCheckerIsNoOp(t *testing.T) {
	got, err := authz.CheckSessionManageScope(context.Background(), nil,
		authz.SessionRef{Namespace: "ns", Name: "n"}, identity.CanonicalFromTrusted("user:alice", "test fixture"), true)
	require.NoError(t, err)
	assert.False(t, got, "nil checker → (false, nil) fail-closed")
}

type fakeForkChecker struct {
	allow bool
	err   error
}

func (f fakeForkChecker) CheckFork(_ context.Context, _, _ string, _ identity.CanonicalUserID, _ bool) (bool, error) {
	return f.allow, f.err
}

func TestCheckSessionFork(t *testing.T) {
	ref := authz.SessionRef{Namespace: "ns", Name: "s1"}
	t.Run("owner allowed: true", func(t *testing.T) {
		ok, err := authz.CheckSessionFork(context.Background(), fakeForkChecker{allow: true}, ref, identity.CanonicalFromTrusted("user:alice", "test fixture"), true)
		require.NoError(t, err)
		assert.True(t, ok)
	})
	t.Run("non-owner: false", func(t *testing.T) {
		ok, err := authz.CheckSessionFork(context.Background(), fakeForkChecker{allow: false}, ref, identity.CanonicalFromTrusted("user:bob", "test fixture"), true)
		require.NoError(t, err)
		assert.False(t, ok)
	})
	t.Run("nil checker: fail-closed false, no error", func(t *testing.T) {
		ok, err := authz.CheckSessionFork(context.Background(), nil, ref, identity.CanonicalFromTrusted("user:alice", "test fixture"), true)
		require.NoError(t, err)
		assert.False(t, ok)
	})
	t.Run("checker error: propagated", func(t *testing.T) {
		ok, err := authz.CheckSessionFork(context.Background(), fakeForkChecker{err: errors.New("boom")}, ref, identity.CanonicalFromTrusted("user:alice", "test fixture"), true)
		require.Error(t, err)
		assert.False(t, ok)
	})
}

type fakeSessionGrant struct {
	res authz.Result
	err error
}

func (f *fakeSessionGrant) CheckSessionGrant(_ context.Context, _, _, _, _, _ string) (authz.Result, error) {
	return f.res, f.err
}

func TestCheckSessionGrant_Allowed(t *testing.T) {
	sdb := &fakeSessionGrant{res: authz.Result{Outcome: authz.OutcomeAllowed}}
	res := authz.CheckSessionGrant(context.Background(), sdb,
		authz.SessionRef{Namespace: "ns", Name: "n"}, "alice", "read", "github_repo")
	assert.Equal(t, authz.OutcomeAllowed, res.Outcome)
}

func TestCheckSessionGrant_NilCheckerDenied(t *testing.T) {
	res := authz.CheckSessionGrant(context.Background(), nil,
		authz.SessionRef{Namespace: "ns", Name: "n"}, "alice", "read", "github_repo")
	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
	assert.Contains(t, res.Message, "no SessionGrantChecker")
}
