package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// These tests pin the Engine's fail-CLOSED contract: for every gate, an
// unwired dependency and a backend error must both land on "no access",
// never on an allow. A regression here is a security bug, not a flake, so
// each case asserts the outcome AND that the reason surfaces rather than
// being swallowed (AGENTS.md: never silently drop errors).

var errBackend = errors.New("spicedb unreachable")

// testCtx builds the system-approval context every memory-touching engine
// call needs. Kept as a helper so a signature change lands in one place.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	return memory.WithSystemApproval(context.Background(), "engine-test")
}

func testScope() authz.SessionRef {
	return authz.SessionRef{Namespace: "demo-ns", Name: "demo-session"}
}

// --- CheckSessionGrant ---

// stubGrantChecker records the arguments the engine forwarded and returns a
// canned verdict, so a test can prove both the delegation and the fail-closed
// mapping of an RPC error onto a Denied Result.
type stubGrantChecker struct {
	res  authz.Result
	err  error
	got  []string // ns, name, subject, permName, resType
	call int
}

func (s *stubGrantChecker) CheckSessionGrant(_ context.Context, ns, name, subject, permName, resType string) (authz.Result, error) {
	s.call++
	s.got = []string{ns, name, subject, permName, resType}
	return s.res, s.err
}

func TestEngine_CheckSessionGrant_FailsClosed(t *testing.T) {
	cases := []struct {
		name        string
		checker     *stubGrantChecker
		wantOutcome authz.Outcome
		wantMsg     string
	}{
		{
			name:        "backend errors: Denied, error text preserved as Message",
			checker:     &stubGrantChecker{err: errBackend},
			wantOutcome: authz.OutcomeDenied,
			wantMsg:     errBackend.Error(),
		},
		{
			name:        "backend denies: Denied, backend Message preserved",
			checker:     &stubGrantChecker{res: authz.Result{Outcome: authz.OutcomeDenied, Message: "no grant"}},
			wantOutcome: authz.OutcomeDenied,
			wantMsg:     "no grant",
		},
		{
			name:        "backend allows: Allowed",
			checker:     &stubGrantChecker{res: authz.Result{Outcome: authz.OutcomeAllowed}},
			wantOutcome: authz.OutcomeAllowed,
		},
		{
			name:        "backend errors AND returns Allowed: error wins, Denied",
			checker:     &stubGrantChecker{res: authz.Result{Outcome: authz.OutcomeAllowed}, err: errBackend},
			wantOutcome: authz.OutcomeDenied,
			wantMsg:     errBackend.Error(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := engine.New(engine.Deps{SessionGrantChecker: tc.checker})
			res := eng.CheckSessionGrant(testCtx(t), testScope(), "user:alice", "read", "repo")

			assert.Equal(t, tc.wantOutcome, res.Outcome)
			if tc.wantMsg != "" {
				assert.Contains(t, res.Message, tc.wantMsg, "the denial reason must surface, not be swallowed")
			}
			assert.Equal(t,
				[]string{"demo-ns", "demo-session", "user:alice", "read", "repo"},
				tc.checker.got, "engine must forward the scope and gate coordinates unchanged")
		})
	}
}

func TestEngine_CheckSessionGrant_NilCheckerDeniesWithReason(t *testing.T) {
	eng := engine.New(engine.Deps{})
	res := eng.CheckSessionGrant(testCtx(t), testScope(), "user:alice", "read", "repo")
	assert.Equal(t, authz.OutcomeDenied, res.Outcome, "an unwired grant checker must never admit a call")
	assert.Contains(t, res.Message, "no SessionGrantChecker wired", "the operator must be told why")
}

// --- CheckApproverAuthorized ---

// stubApproverChecker fakes the two-phase click-time approver gate. approveOK
// answers the session-approve branch; owners/ownerErrs answer the per-resource
// branch keyed by "<type>:<id>". consistentReads records the fullyConsistent
// flag the engine passed, which MUST be true — a stale read could admit an
// approver whose standing was just revoked.
type stubApproverChecker struct {
	approveOK        bool
	approveErr       error
	owners           map[string]bool
	ownerErrs        map[string]error
	consistentReads  []bool
	ownerCallOrder   []string
	approveCallCount int
}

func (s *stubApproverChecker) CheckApprove(_ context.Context, _, _ string, _ identity.CanonicalUserID, fc bool) (bool, error) {
	s.approveCallCount++
	s.consistentReads = append(s.consistentReads, fc)
	return s.approveOK, s.approveErr
}

func (s *stubApproverChecker) CheckOwnerOnResource(_ context.Context, resType, resID string, _ identity.CanonicalUserID, fc bool) (bool, error) {
	key := resType + ":" + resID
	s.ownerCallOrder = append(s.ownerCallOrder, key)
	s.consistentReads = append(s.consistentReads, fc)
	if err, ok := s.ownerErrs[key]; ok {
		return false, err
	}
	return s.owners[key], nil
}

// CheckOnResource satisfies the approver-gate interface; this stub models an
// owner-only resource type, so every permission delegates to the owner answer.
func (s *stubApproverChecker) CheckOnResource(ctx context.Context, resType, resID, _ string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	return s.CheckOwnerOnResource(ctx, resType, resID, canonicalID, fullyConsistent)
}

func TestEngine_CheckApproverAuthorized_NilCheckerRefuses(t *testing.T) {
	eng := engine.New(engine.Deps{})
	ok, err := eng.CheckApproverAuthorized(testCtx(t), testScope(), nil, identity.CanonicalFromTrusted("alice@example.com", "test fixture"))
	require.NoError(t, err)
	assert.False(t, ok, "an unwired approver checker must refuse, not panic and not admit")
}

// TestEngine_CheckApproverAuthorized_SessionOnlyGate covers the
// no-source-resource branch, where agentsession#approve alone decides.
func TestEngine_CheckApproverAuthorized_SessionOnlyGate(t *testing.T) {
	cases := []struct {
		name    string
		stub    *stubApproverChecker
		wantOK  bool
		wantErr error
	}{
		{
			name:   "session approve granted: authorized",
			stub:   &stubApproverChecker{approveOK: true},
			wantOK: true,
		},
		{
			name:   "session approve absent: not authorized",
			stub:   &stubApproverChecker{approveOK: false},
			wantOK: false,
		},
		{
			name:    "backend errors: not authorized, error propagated",
			stub:    &stubApproverChecker{approveErr: errBackend},
			wantOK:  false,
			wantErr: errBackend,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := engine.New(engine.Deps{ApproverChecker: tc.stub})
			ok, err := eng.CheckApproverAuthorized(testCtx(t), testScope(), nil, identity.CanonicalFromTrusted("alice@example.com", "test fixture"))

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr, "an infra failure must surface, never be swallowed into a verdict")
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, 1, tc.stub.approveCallCount, "session-only gate consults CheckApprove exactly once")
		})
	}
}

// TestEngine_CheckApproverAuthorized_ResourceGate covers the quorum-of-one
// resource branch: ownership of ANY listed resource suffices, and an outage
// on some resources can never manufacture an approval.
func TestEngine_CheckApproverAuthorized_ResourceGate(t *testing.T) {
	resources := []authz.ApproverResourceRef{
		{Type: "repo", ID: "r1"},
		{Type: "repo", ID: "r2"},
	}
	cases := []struct {
		name    string
		stub    *stubApproverChecker
		wantOK  bool
		wantErr error
	}{
		{
			name:   "owner of the first resource: authorized",
			stub:   &stubApproverChecker{owners: map[string]bool{"repo:r1": true}},
			wantOK: true,
		},
		{
			name:   "owner of only the second resource: authorized (quorum of one)",
			stub:   &stubApproverChecker{owners: map[string]bool{"repo:r2": true}},
			wantOK: true,
		},
		{
			name:   "owner of neither resource: not authorized, no error",
			stub:   &stubApproverChecker{owners: map[string]bool{}},
			wantOK: false,
		},
		{
			name: "every resource check errors: not authorized, first error propagated",
			stub: &stubApproverChecker{ownerErrs: map[string]error{
				"repo:r1": errBackend,
				"repo:r2": errBackend,
			}},
			wantOK:  false,
			wantErr: errBackend,
		},
		{
			name: "one resource errors, the other confirms ownership: authorized, no error",
			stub: &stubApproverChecker{
				ownerErrs: map[string]error{"repo:r1": errBackend},
				owners:    map[string]bool{"repo:r2": true},
			},
			wantOK: true,
		},
		{
			name: "one resource errors, the other denies: not authorized, error propagated",
			stub: &stubApproverChecker{
				ownerErrs: map[string]error{"repo:r1": errBackend},
				owners:    map[string]bool{"repo:r2": false},
			},
			wantOK:  false,
			wantErr: errBackend,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := engine.New(engine.Deps{ApproverChecker: tc.stub})
			ok, err := eng.CheckApproverAuthorized(testCtx(t), testScope(), resources, identity.CanonicalFromTrusted("alice@example.com", "test fixture"))

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr, "an infra failure must surface, never be swallowed into a verdict")
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantOK, ok)
			assert.Zero(t, tc.stub.approveCallCount,
				"agentsession#approve must NOT be consulted for a resource-scoped approval")
		})
	}
}

// TestEngine_CheckApproverAuthorized_AlwaysReadsFullyConsistent guards the
// staleness axis: click-time approver checks must read at head, or a just-
// revoked approver could still land an approval from a cached snapshot.
func TestEngine_CheckApproverAuthorized_AlwaysReadsFullyConsistent(t *testing.T) {
	t.Run("session-only gate reads at head", func(t *testing.T) {
		s := &stubApproverChecker{approveOK: true}
		eng := engine.New(engine.Deps{ApproverChecker: s})
		_, err := eng.CheckApproverAuthorized(testCtx(t), testScope(), nil, identity.CanonicalFromTrusted("alice@example.com", "test fixture"))
		require.NoError(t, err)
		require.NotEmpty(t, s.consistentReads)
		for _, fc := range s.consistentReads {
			assert.True(t, fc, "CheckApprove must be called with fullyConsistent=true")
		}
	})
	t.Run("resource gate reads at head for every resource", func(t *testing.T) {
		s := &stubApproverChecker{owners: map[string]bool{}}
		eng := engine.New(engine.Deps{ApproverChecker: s})
		_, err := eng.CheckApproverAuthorized(testCtx(t), testScope(),
			[]authz.ApproverResourceRef{{Type: "repo", ID: "r1"}, {Type: "repo", ID: "r2"}},
			identity.CanonicalFromTrusted("alice@example.com", "test fixture"))
		require.NoError(t, err)
		require.Len(t, s.consistentReads, 2)
		for _, fc := range s.consistentReads {
			assert.True(t, fc, "CheckOwnerOnResource must be called with fullyConsistent=true")
		}
	})
}

// --- CheckSessionInteract error propagation ---

// TestEngine_CheckSessionInteract_BackendErrorDeniesAndPropagates pins the
// inbound gate's failure shape. NOTE: the engine forwards the backend's
// (bool, error) tuple verbatim rather than forcing ok=false on error — the
// fail-closed guarantee rests on every real backend returning (false, err)
// (spicedb.Client.checkUser does) plus the documented caller rule "treat a
// non-nil error as a denial". This test therefore fixes the realistic shape.
func TestEngine_CheckSessionInteract_BackendErrorDeniesAndPropagates(t *testing.T) {
	fb := &fakeBackends{
		checkInteractFunc: func(_ context.Context, _, _ string, _ identity.CanonicalUserID, _ bool) (bool, error) {
			return false, errBackend
		},
	}
	eng := engine.New(engine.Deps{SessionInteractChecker: fb})
	ok, err := eng.CheckSessionInteract(testCtx(t), testScope(), identity.CanonicalFromTrusted("alice@example.com", "test fixture"), true)

	require.ErrorIs(t, err, errBackend, "the transport failure must reach the caller, not be swallowed")
	assert.False(t, ok, "an unreachable backend must never read as authorized")
}

func TestEngine_CheckSessionInteract_ForwardsFullyConsistentFlag(t *testing.T) {
	var got []bool
	fb := &fakeBackends{
		checkInteractFunc: func(_ context.Context, _, _ string, _ identity.CanonicalUserID, fc bool) (bool, error) {
			got = append(got, fc)
			return true, nil
		},
	}
	eng := engine.New(engine.Deps{SessionInteractChecker: fb})
	_, err := eng.CheckSessionInteract(testCtx(t), testScope(), identity.CanonicalFromTrusted("alice@example.com", "test fixture"), true)
	require.NoError(t, err)
	_, err = eng.CheckSessionInteract(testCtx(t), testScope(), identity.CanonicalFromTrusted("alice@example.com", "test fixture"), false)
	require.NoError(t, err)

	assert.Equal(t, []bool{true, false}, got,
		"the caller's consistency choice must reach SpiceDB verbatim, not be defaulted by the engine")
}

// --- Grant / Revoke ---

// recordingRelWriter captures relation writes and can fail on demand.
type recordingRelWriter struct {
	wrote     [][]authz.Relation
	deleted   [][]authz.Relation
	writeErr  error
	deleteErr error
}

func (w *recordingRelWriter) WriteRelationships(_ context.Context, rels []authz.Relation) error {
	w.wrote = append(w.wrote, rels)
	return w.writeErr
}

func (w *recordingRelWriter) DeleteRelationships(_ context.Context, rels []authz.Relation) error {
	w.deleted = append(w.deleted, rels)
	return w.deleteErr
}

func testRels() []authz.Relation {
	return []authz.Relation{{
		ResourceType: "agentsession", ResourceID: "demo-ns/demo-session",
		Relation:    "participant",
		SubjectType: "user", SubjectID: "alice@example.com",
	}}
}

func TestEngine_Grant(t *testing.T) {
	t.Run("wired writer: relations forwarded verbatim", func(t *testing.T) {
		w := &recordingRelWriter{}
		eng := engine.New(engine.Deps{RelWriter: w})
		require.NoError(t, eng.Grant(testCtx(t), testRels()))
		require.Len(t, w.wrote, 1)
		assert.Equal(t, testRels(), w.wrote[0])
	})
	t.Run("writer errors: error propagated so the caller cannot assume the grant landed", func(t *testing.T) {
		w := &recordingRelWriter{writeErr: errBackend}
		eng := engine.New(engine.Deps{RelWriter: w})
		require.ErrorIs(t, eng.Grant(testCtx(t), testRels()), errBackend)
	})
	t.Run("nil writer: no-op, no panic", func(t *testing.T) {
		eng := engine.New(engine.Deps{})
		require.NoError(t, eng.Grant(testCtx(t), testRels()))
	})
	t.Run("empty relation list: writer not called", func(t *testing.T) {
		w := &recordingRelWriter{}
		eng := engine.New(engine.Deps{RelWriter: w})
		require.NoError(t, eng.Grant(testCtx(t), nil))
		assert.Empty(t, w.wrote, "an empty batch must not reach SpiceDB")
	})
}

func TestEngine_Revoke(t *testing.T) {
	t.Run("wired writer: relations forwarded verbatim", func(t *testing.T) {
		w := &recordingRelWriter{}
		eng := engine.New(engine.Deps{RelWriter: w})
		require.NoError(t, eng.Revoke(testCtx(t), testRels()))
		require.Len(t, w.deleted, 1)
		assert.Equal(t, testRels(), w.deleted[0])
	})
	t.Run("writer errors: error propagated so access is not assumed revoked", func(t *testing.T) {
		w := &recordingRelWriter{deleteErr: errBackend}
		eng := engine.New(engine.Deps{RelWriter: w})
		require.ErrorIs(t, eng.Revoke(testCtx(t), testRels()), errBackend,
			"a failed delete means access may still be live; swallowing it would strand a revocation")
	})
	t.Run("empty relation list: writer not called", func(t *testing.T) {
		w := &recordingRelWriter{}
		eng := engine.New(engine.Deps{RelWriter: w})
		require.NoError(t, eng.Revoke(testCtx(t), nil))
		assert.Empty(t, w.deleted)
	})
}

// --- Touch* delegation and error propagation ---

// erroringGranter fails every write, proving the engine surfaces relation
// write failures rather than reporting a phantom success.
type erroringGranter struct{ err error }

func (g *erroringGranter) TouchStartedBy(context.Context, string, string, identity.CanonicalUserID) error {
	return g.err
}
func (g *erroringGranter) TouchOwner(context.Context, string, string, string) error { return g.err }
func (g *erroringGranter) TouchInteractParticipant(context.Context, string, string, string) error {
	return g.err
}
func (g *erroringGranter) TouchInteractParticipantUser(context.Context, string, string, identity.CanonicalUserID) error {
	return g.err
}
func (g *erroringGranter) TouchDeniedUser(context.Context, string, string, identity.CanonicalUserID) error {
	return g.err
}

func TestEngine_TouchMethods_PropagateWriteErrors(t *testing.T) {
	eng := engine.New(engine.Deps{Granter: &erroringGranter{err: errBackend}})
	ctx, scope := testCtx(t), testScope()

	cases := []struct {
		name string
		call func() error
	}{
		{name: "TouchStartedBy: write failure propagated", call: func() error {
			return eng.TouchStartedBy(ctx, scope, identity.CanonicalFromTrusted("alice@example.com", "test fixture"))
		}},
		{name: "TouchOwner: write failure propagated", call: func() error {
			return eng.TouchOwner(ctx, scope, "user:alice@example.com")
		}},
		{name: "TouchInteractParticipant: write failure propagated", call: func() error {
			return eng.TouchInteractParticipant(ctx, scope, "group:eng#member")
		}},
		{name: "TouchInteractParticipantUser: write failure propagated", call: func() error {
			return eng.TouchInteractParticipantUser(ctx, scope, identity.CanonicalFromTrusted("alice@example.com", "test fixture"))
		}},
		{name: "TouchDeniedUser: write failure propagated", call: func() error {
			return eng.TouchDeniedUser(ctx, scope, identity.CanonicalFromTrusted("mallory@example.com", "test fixture"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorIs(t, tc.call(), errBackend)
		})
	}
}

// recordingGranter captures the (ns, name, subject) each Touch* forwards, so
// the tests can prove the SessionRef is unpacked into the right coordinates.
type recordingGranter struct {
	ownerSubject       string
	participantSubject string
	ns, name           string
}

func (g *recordingGranter) TouchStartedBy(context.Context, string, string, identity.CanonicalUserID) error {
	return nil
}
func (g *recordingGranter) TouchOwner(_ context.Context, ns, name, subjectRef string) error {
	g.ns, g.name, g.ownerSubject = ns, name, subjectRef
	return nil
}
func (g *recordingGranter) TouchInteractParticipant(_ context.Context, ns, name, subject string) error {
	g.ns, g.name, g.participantSubject = ns, name, subject
	return nil
}
func (g *recordingGranter) TouchInteractParticipantUser(context.Context, string, string, identity.CanonicalUserID) error {
	return nil
}
func (g *recordingGranter) TouchDeniedUser(context.Context, string, string, identity.CanonicalUserID) error {
	return nil
}

func TestEngine_TouchOwnerAndParticipant_ForwardScopeAndSubject(t *testing.T) {
	t.Run("TouchOwner forwards the subject-set expression unchanged", func(t *testing.T) {
		g := &recordingGranter{}
		eng := engine.New(engine.Deps{Granter: g})
		require.NoError(t, eng.TouchOwner(testCtx(t), testScope(), "group:eng#member"))
		assert.Equal(t, "demo-ns", g.ns)
		assert.Equal(t, "demo-session", g.name)
		assert.Equal(t, "group:eng#member", g.ownerSubject)
	})
	t.Run("TouchInteractParticipant forwards the subject-set expression unchanged", func(t *testing.T) {
		g := &recordingGranter{}
		eng := engine.New(engine.Deps{Granter: g})
		require.NoError(t, eng.TouchInteractParticipant(testCtx(t), testScope(), "group:sre#member"))
		assert.Equal(t, "group:sre#member", g.participantSubject)
	})
}

func TestEngine_TouchOwnerAndParticipant_NilGranterNoOp(t *testing.T) {
	eng := engine.New(engine.Deps{})
	ctx, scope := testCtx(t), testScope()
	assert.NoError(t, eng.TouchOwner(ctx, scope, "user:alice@example.com"))
	assert.NoError(t, eng.TouchInteractParticipant(ctx, scope, "group:eng#member"))
}

// --- Lookup* ---

// stubLookuper answers the three lookup shapes with canned data or errors.
type stubLookuper struct {
	bySet       map[string][]string
	errBySet    map[string]error
	interact    []string
	interactErr error
}

func (s *stubLookuper) LookupSubjects(_ context.Context, subjectRef string) ([]string, error) {
	if err, ok := s.errBySet[subjectRef]; ok {
		return nil, err
	}
	return s.bySet[subjectRef], nil
}

func (s *stubLookuper) LookupInteractSubjects(_ context.Context, _, _ string) ([]string, error) {
	return s.interact, s.interactErr
}

func (s *stubLookuper) LookupSubjectIncludes(_ context.Context, _ string, _ identity.CanonicalUserID) (bool, error) {
	return false, nil
}

func TestEngine_LookupApprovers(t *testing.T) {
	t.Run("wired lookuper: members returned", func(t *testing.T) {
		l := &stubLookuper{bySet: map[string][]string{"repo:r1#owner": {"alice@example.com", "bob@example.com"}}}
		eng := engine.New(engine.Deps{Lookuper: l})
		got, err := eng.LookupApprovers(testCtx(t), "repo:r1#owner")
		require.NoError(t, err)
		assert.Equal(t, []string{"alice@example.com", "bob@example.com"}, got)
	})
	t.Run("lookup errors: error propagated, not reported as an empty approver pool", func(t *testing.T) {
		l := &stubLookuper{errBySet: map[string]error{"repo:r1#owner": errBackend}}
		eng := engine.New(engine.Deps{Lookuper: l})
		got, err := eng.LookupApprovers(testCtx(t), "repo:r1#owner")
		require.ErrorIs(t, err, errBackend,
			"an outage must not be indistinguishable from 'nobody can approve'")
		assert.Empty(t, got)
	})
	t.Run("nil lookuper: empty result, no error", func(t *testing.T) {
		eng := engine.New(engine.Deps{})
		got, err := eng.LookupApprovers(testCtx(t), "repo:r1#owner")
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestEngine_LookupInteractParticipants(t *testing.T) {
	t.Run("wired lookuper: participants returned", func(t *testing.T) {
		l := &stubLookuper{interact: []string{"alice@example.com"}}
		eng := engine.New(engine.Deps{Lookuper: l})
		got, err := eng.LookupInteractParticipants(testCtx(t), testScope())
		require.NoError(t, err)
		assert.Equal(t, []string{"alice@example.com"}, got)
	})
	t.Run("lookup errors: error propagated", func(t *testing.T) {
		l := &stubLookuper{interactErr: errBackend}
		eng := engine.New(engine.Deps{Lookuper: l})
		_, err := eng.LookupInteractParticipants(testCtx(t), testScope())
		require.ErrorIs(t, err, errBackend)
	})
	t.Run("nil lookuper: empty result, no error", func(t *testing.T) {
		eng := engine.New(engine.Deps{})
		got, err := eng.LookupInteractParticipants(testCtx(t), testScope())
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

// TestEngine_ResolveApprovers pins the raise-time eligibility rule that
// CheckApproverAuthorized mirrors at click time: resource owner-sets union
// (quorum of one), session approve-set only when no resource is named.
func TestEngine_ResolveApprovers(t *testing.T) {
	const sessionSet = "agentsession:demo-ns/demo-session#approve"

	cases := []struct {
		name          string
		lookuper      *stubLookuper
		resourceSets  []string
		wantApprovers []string
		wantEmpty     bool
		wantErr       error
	}{
		{
			name:          "no resource sets: session approve-set decides",
			lookuper:      &stubLookuper{bySet: map[string][]string{sessionSet: {"alice@example.com"}}},
			wantApprovers: []string{"alice@example.com"},
		},
		{
			name:      "no resource sets and an empty session set: empty=true",
			lookuper:  &stubLookuper{bySet: map[string][]string{}},
			wantEmpty: true,
		},
		{
			name: "resource sets present: union across owners, session set ignored",
			lookuper: &stubLookuper{bySet: map[string][]string{
				sessionSet:      {"carol@example.com"},
				"repo:r1#owner": {"alice@example.com"},
				"repo:r2#owner": {"bob@example.com"},
			}},
			resourceSets:  []string{"repo:r1#owner", "repo:r2#owner"},
			wantApprovers: []string{"alice@example.com", "bob@example.com"},
		},
		{
			name: "overlapping resource owners: deduped to one entry each",
			lookuper: &stubLookuper{bySet: map[string][]string{
				"repo:r1#owner": {"alice@example.com", "bob@example.com"},
				"repo:r2#owner": {"bob@example.com", "carol@example.com"},
			}},
			resourceSets:  []string{"repo:r1#owner", "repo:r2#owner"},
			wantApprovers: []string{"alice@example.com", "bob@example.com", "carol@example.com"},
		},
		{
			name:         "resource sets present but nobody owns them: empty=true",
			lookuper:     &stubLookuper{bySet: map[string][]string{}},
			resourceSets: []string{"repo:r1#owner"},
			wantEmpty:    true,
		},
		{
			name:     "session set lookup errors: error propagated, empty=false",
			lookuper: &stubLookuper{errBySet: map[string]error{sessionSet: errBackend}},
			wantErr:  errBackend,
		},
		{
			name: "one resource lookup errors: error propagated rather than a partial pool",
			lookuper: &stubLookuper{
				bySet:    map[string][]string{"repo:r1#owner": {"alice@example.com"}},
				errBySet: map[string]error{"repo:r2#owner": errBackend},
			},
			resourceSets: []string{"repo:r1#owner", "repo:r2#owner"},
			wantErr:      errBackend,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := engine.New(engine.Deps{Lookuper: tc.lookuper})
			got, empty, err := eng.ResolveApprovers(testCtx(t), sessionSet, tc.resourceSets)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.False(t, empty, "an error must not also claim the approver pool is empty")
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantEmpty, empty)
			assert.ElementsMatch(t, tc.wantApprovers, got)
		})
	}
}

func TestEngine_ResolveApprovers_NilLookuperReportsEmptyPool(t *testing.T) {
	eng := engine.New(engine.Deps{})
	got, empty, err := eng.ResolveApprovers(testCtx(t), "agentsession:demo-ns/demo-session#approve", nil)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.True(t, empty, "no lookuper means nobody can be shown to have standing")
}

func TestEngine_LookupSubjectIncludes_BackendErrorPropagates(t *testing.T) {
	fl := &fakeLookuperImpl{includesErr: errBackend}
	eng := engine.New(engine.Deps{Lookuper: fl})
	got, err := eng.LookupSubjectIncludes(testCtx(t), "group:eng#member", identity.CanonicalFromTrusted("alice@example.com", "test fixture"))
	require.ErrorIs(t, err, errBackend, "the lookup failure must reach the caller")
	assert.False(t, got, "a lookup failure must not read as membership")
}

// --- BindClassDefaults / FillToolArgs degrade paths ---

func TestEngine_BindClassDefaults_MissingDepsIsNoOp(t *testing.T) {
	entities := []authz.BoundEntitySpec{{ResourceType: "repo", Permission: "read", Defaults: []string{"r1"}}}
	cases := []struct {
		name string
		deps engine.Deps
	}{
		{name: "no Memory and no ToolChecker: no-op", deps: engine.Deps{}},
		{name: "ToolChecker without Memory: no-op", deps: engine.Deps{ToolChecker: &fakeBackends{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := engine.New(tc.deps)
			require.NoError(t, eng.BindClassDefaults(testCtx(t),
				memory.Scope{Kind: "session", ID: "demo-ns/demo-session"},
				authz.SessionRef{Namespace: "demo-ns", Name: "demo-session"}, entities, "user:alice@example.com"))
		})
	}
}

func TestEngine_FillToolArgs_NoMemoryReturnsArgsUnchanged(t *testing.T) {
	eng := engine.New(engine.Deps{})
	in := []byte(`{"path":"/tmp/x"}`)
	got, err := eng.FillToolArgs(testCtx(t),
		authz.SessionRef{Namespace: "demo-ns", Name: "demo-session"}, nil, "read_file", in)
	require.NoError(t, err)
	assert.JSONEq(t, string(in), string(got), "with no memory wired the args must pass through untouched")
}
