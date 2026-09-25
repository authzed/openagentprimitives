package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	exs "github.com/authzed/openagentprimitives/pkg/memory/kinds/extraction_state"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// fakeBackends implements ToolChecker + SessionInteractChecker for tests.
type fakeBackends struct {
	checkToolCallCalled bool
	checkInteractFunc   func(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fc bool) (bool, error)
	checkConverseFunc   func(ctx context.Context, ns, name, senderNS, senderName string, fc bool) (bool, error)
}

func (f *fakeBackends) CheckToolCall(_ context.Context, _ authz.Permission, _ authz.Inputs) authz.Result {
	f.checkToolCallCalled = true
	return authz.Result{Outcome: authz.OutcomeAllowed}
}

func (f *fakeBackends) CheckInteract(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fc bool) (bool, error) {
	if f.checkInteractFunc != nil {
		return f.checkInteractFunc(ctx, ns, name, canonicalID, fc)
	}
	return false, nil
}

func (f *fakeBackends) CheckConverse(ctx context.Context, ns, name, senderNS, senderName string, fc bool) (bool, error) {
	if f.checkConverseFunc != nil {
		return f.checkConverseFunc(ctx, ns, name, senderNS, senderName, fc)
	}
	return false, nil
}

func TestEngine_CheckToolCall_DelegatesToChecker(t *testing.T) {
	fb := &fakeBackends{}
	eng := engine.New(engine.Deps{ToolChecker: fb})
	res := eng.CheckToolCall(memory.WithSystemApproval(context.Background(), "test"), authz.Permission{}, authz.Inputs{})
	assert.True(t, fb.checkToolCallCalled)
	assert.Equal(t, authz.OutcomeAllowed, res.Outcome)
}

func TestEngine_CheckToolCall_NoToolCheckerDenied(t *testing.T) {
	eng := engine.New(engine.Deps{})
	res := eng.CheckToolCall(memory.WithSystemApproval(context.Background(), "test"), authz.Permission{}, authz.Inputs{})
	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
	assert.Contains(t, res.Message, "no ToolChecker wired")
}

func TestEngine_CheckSessionInteract_DelegatesToSpiceDB(t *testing.T) {
	fb := &fakeBackends{
		checkInteractFunc: func(_ context.Context, ns, name string, _ identity.CanonicalUserID, _ bool) (bool, error) {
			assert.Equal(t, "ns", ns)
			assert.Equal(t, "n", name)
			return true, nil
		},
	}
	eng := engine.New(engine.Deps{SessionInteractChecker: fb})
	got, err := eng.CheckSessionInteract(memory.WithSystemApproval(context.Background(), "test"),
		authz.SessionRef{Namespace: "ns", Name: "n"}, identity.CanonicalFromTrusted("alice", "test fixture"), false)
	require.NoError(t, err)
	assert.True(t, got)
}

func TestEngine_CheckSessionInteract_NilCheckerReturnsFalse(t *testing.T) {
	eng := engine.New(engine.Deps{})
	got, err := eng.CheckSessionInteract(memory.WithSystemApproval(context.Background(), "test"),
		authz.SessionRef{Namespace: "ns", Name: "n"}, identity.CanonicalFromTrusted("alice", "test fixture"), false)
	require.NoError(t, err)
	assert.False(t, got)
}

func TestEngine_FutureMoveStubs_Denied(t *testing.T) {
	eng := engine.New(engine.Deps{})

	res := eng.CheckEntityCanBind(memory.WithSystemApproval(context.Background(), "test"), authz.Permission{}, authz.Inputs{})
	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
	assert.Contains(t, res.Message, "CheckEntityCanBind")

	res = eng.CheckMCPTrust(memory.WithSystemApproval(context.Background(), "test"), authz.MCPToolRef{}, nil)
	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
	assert.Contains(t, res.Message, "CheckMCPTrust")

	res = eng.CheckInformationFlow(memory.WithSystemApproval(context.Background(), "test"), authz.ResourceRef{}, authz.ResourceRef{}, nil)
	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
	assert.Contains(t, res.Message, "CheckInformationFlow")
}

func TestEngine_ErrNotYetMigratedReExported(t *testing.T) {
	assert.True(t, errors.Is(engine.ErrNotYetMigrated, authz.ErrNotYetMigrated))
}

// fakeGranterImpl implements engine.GranterImpl for testing.
type fakeGranterImpl struct {
	startedByCalled         bool
	interactParticipantUser string
	touchDeniedCalled       bool
}

func (f *fakeGranterImpl) TouchStartedBy(_ context.Context, _, _ string, _ identity.CanonicalUserID) error {
	f.startedByCalled = true
	return nil
}
func (f *fakeGranterImpl) TouchOwner(_ context.Context, _, _, _ string) error {
	return nil
}
func (f *fakeGranterImpl) TouchInteractParticipant(_ context.Context, _, _, _ string) error {
	return nil
}
func (f *fakeGranterImpl) TouchInteractParticipantUser(_ context.Context, _, _ string, canonicalID identity.CanonicalUserID) error {
	f.interactParticipantUser = canonicalID.String()
	return nil
}
func (f *fakeGranterImpl) TouchDeniedUser(_ context.Context, _, _ string, _ identity.CanonicalUserID) error {
	f.touchDeniedCalled = true
	return nil
}

func TestEngine_TouchStartedBy_DelegatesToGranter(t *testing.T) {
	fg := &fakeGranterImpl{}
	eng := engine.New(engine.Deps{Granter: fg})
	err := eng.TouchStartedBy(memory.WithSystemApproval(context.Background(), "test"),
		authz.SessionRef{Namespace: "ns", Name: "n"}, identity.CanonicalFromTrusted("alice", "test fixture"))
	require.NoError(t, err)
	assert.True(t, fg.startedByCalled)
}

func TestEngine_TouchInteractParticipantUser_DelegatesToGranter(t *testing.T) {
	fg := &fakeGranterImpl{}
	eng := engine.New(engine.Deps{Granter: fg})
	err := eng.TouchInteractParticipantUser(memory.WithSystemApproval(context.Background(), "test"),
		authz.SessionRef{Namespace: "ns", Name: "n"}, identity.CanonicalFromTrusted("canonical-123", "test fixture"))
	require.NoError(t, err)
	assert.Equal(t, "canonical-123", fg.interactParticipantUser)
}

func TestEngine_TouchGranterNil_NoOp(t *testing.T) {
	eng := engine.New(engine.Deps{})
	assert.NoError(t, eng.TouchStartedBy(memory.WithSystemApproval(context.Background(), "test"), authz.SessionRef{}, identity.CanonicalFromTrusted("x", "test fixture")))
	assert.NoError(t, eng.TouchInteractParticipantUser(memory.WithSystemApproval(context.Background(), "test"), authz.SessionRef{}, identity.CanonicalFromTrusted("x", "test fixture")))
	assert.NoError(t, eng.TouchDeniedUser(memory.WithSystemApproval(context.Background(), "test"), authz.SessionRef{}, identity.CanonicalFromTrusted("x", "test fixture")))
}

// fakeLookuperImpl implements engine.LookuperImpl.
type fakeLookuperImpl struct {
	subjectIncludes bool
	includesErr     error
	subjects        []string
}

func (f *fakeLookuperImpl) LookupSubjects(_ context.Context, _ string) ([]string, error) {
	return f.subjects, nil
}
func (f *fakeLookuperImpl) LookupInteractSubjects(_ context.Context, _, _ string) ([]string, error) {
	return f.subjects, nil
}
func (f *fakeLookuperImpl) LookupSubjectIncludes(_ context.Context, _ string, _ identity.CanonicalUserID) (bool, error) {
	return f.subjectIncludes, f.includesErr
}

func TestEngine_LookupSubjectIncludes_DelegatesToLookuper(t *testing.T) {
	fl := &fakeLookuperImpl{subjectIncludes: true}
	eng := engine.New(engine.Deps{Lookuper: fl})
	got, err := eng.LookupSubjectIncludes(memory.WithSystemApproval(context.Background(), "test"), "group:eng#member", identity.CanonicalFromTrusted("alice", "test fixture"))
	require.NoError(t, err)
	assert.True(t, got)
}

func TestEngine_LookupSubjectIncludes_NilLookuperReturnsFalse(t *testing.T) {
	eng := engine.New(engine.Deps{})
	got, err := eng.LookupSubjectIncludes(memory.WithSystemApproval(context.Background(), "test"), "group:eng#member", identity.CanonicalFromTrusted("alice", "test fixture"))
	require.NoError(t, err)
	assert.False(t, got)
}

func TestEngine_WaitForExtraction_ReturnsOnComplete(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/n"}
	require.NoError(t, exs.Record(memory.WithSystemApproval(context.Background(), "test"), mem, scope, exs.Content{
		TurnIndex: 5, Status: exs.StatusComplete, StartedAt: time.Now(), CompletedAt: time.Now(),
	}))
	eng := engine.New(engine.Deps{Memory: mem})
	err := eng.WaitForExtraction(memory.WithSystemApproval(context.Background(), "test"), scope, 5, 500*time.Millisecond)
	require.NoError(t, err)
}

func TestEngine_WaitForExtraction_NoMemoryNoOp(t *testing.T) {
	eng := engine.New(engine.Deps{})
	assert.NoError(t, eng.WaitForExtraction(memory.WithSystemApproval(context.Background(), "test"),
		memory.Scope{Kind: "session", ID: "ns/n"}, 5, 100*time.Millisecond))
}
