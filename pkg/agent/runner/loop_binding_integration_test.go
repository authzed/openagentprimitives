// pkg/agent/runner/loop_binding_integration_test.go
//
// Verifies the runner's session-cold-start binding write goes through Engine:
// internal/cmd/runner/main.go calls eng.BindClassDefaults, which writes the allowed
// defaults to SessionScope.resources.
//
// These tests call that Engine method directly (the same call internal/cmd/runner
// makes) and assert on the resulting memory state. This avoids a dependency
// on package main while preserving the same test coverage goal.
//
// The per-session authz config snapshot is deliberately NOT covered here: it
// is written by the operator's AgentSession reconciler, not the runner, and is
// tested there and on its own Kind.
//
// A recording Engine fake captures method calls; assertions check call counts
// and the resulting memory state via the session_scope Kind accessor.
package runner_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

// recordingEngine wraps a real Engine (backed by inmem memory) so tests can
// assert on which methods were called and how many times.
type recordingEngine struct {
	engine.Engine
	bindClassDefaultsCalls int
	lastDefaultsEntities   []authz.BoundEntitySpec
	lastDefaultsSubject    string
}

func (r *recordingEngine) BindClassDefaults(ctx context.Context, scope memory.Scope, sess authz.SessionRef, entities []authz.BoundEntitySpec, subject string) error {
	r.bindClassDefaultsCalls++
	r.lastDefaultsEntities = entities
	r.lastDefaultsSubject = subject
	return r.Engine.BindClassDefaults(ctx, scope, sess, entities, subject)
}

// slotGrantStore is an in-memory stand-in for SpiceDB's slot-grant tuples: it
// satisfies both the write side (engine.RelWriter) and the read side
// (engine.SlotListerImpl), so a test can assert that what BindClassDefaults
// grants is exactly what FillToolArgs later reads back.
type slotGrantStore struct {
	grants map[string][]authz.SlotBinding // keyed by "<ns>/<name>"
}

func newSlotGrantStore() *slotGrantStore {
	return &slotGrantStore{grants: map[string][]authz.SlotBinding{}}
}

func (s *slotGrantStore) WriteRelationships(_ context.Context, rels []authz.Relation) error {
	for _, rel := range rels {
		if !strings.HasPrefix(rel.Relation, authz.SlotGrantRelationPrefix) {
			continue
		}
		key := rel.SubjectID
		b := authz.SlotBinding{
			ResourceType: rel.ResourceType,
			// TrustedObjectID: rel.ResourceID is a tuple already written by
			// GrantSlots, so this is the read-back stand-in for the real
			// ListSlotGrants (pkg/spicedb/slot_grants.go), which does the same.
			ResourceID: authz.TrustedObjectID(rel.ResourceID),
			Permission: strings.TrimPrefix(rel.Relation, authz.SlotGrantRelationPrefix),
		}
		// TOUCH semantics, keyed on the grant-identifying triple: SlotBinding
		// carries a []precondition.Rule now and so is no longer comparable, but
		// this read-back stand-in only ever populates the triple anyway.
		dup := slices.ContainsFunc(s.grants[key], func(g authz.SlotBinding) bool {
			return g.ResourceType == b.ResourceType &&
				g.ResourceID.String() == b.ResourceID.String() &&
				g.Permission == b.Permission
		})
		if !dup {
			s.grants[key] = append(s.grants[key], b)
		}
	}
	return nil
}

func (s *slotGrantStore) DeleteRelationships(_ context.Context, rels []authz.Relation) error {
	for _, rel := range rels {
		key := rel.SubjectID
		s.grants[key] = slices.DeleteFunc(s.grants[key], func(b authz.SlotBinding) bool {
			return b.ResourceType == rel.ResourceType && b.ResourceID.String() == rel.ResourceID
		})
	}
	return nil
}

func (s *slotGrantStore) ListSlotGrants(_ context.Context, ns, name string) ([]authz.SlotBinding, error) {
	return s.grants[ns+"/"+name], nil
}

func bindTestSession() authz.SessionRef {
	return authz.SessionRef{Namespace: "default", Name: "bind-test"}
}

// buildRecordingEngine constructs a recordingEngine backed by an in-process
// inmem memory and a slot-grant store. The ToolChecker always allows so
// BindClassDefaults writes all defaults through.
func buildRecordingEngine(t *testing.T) (*recordingEngine, memory.Memory, memory.Scope, *slotGrantStore) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "default/bind-test"}
	slots := newSlotGrantStore()
	inner := engine.New(engine.Deps{
		ToolChecker: &allowAllToolChecker{},
		RelWriter:   slots,
		SlotLister:  slots,
		Memory:      mem,
	})
	rec := &recordingEngine{Engine: inner}
	return rec, mem, scope, slots
}

// boundEntitySpecsForTest mirrors the adapter internal/cmd/runner uses
// (boundEntitySpecsFromClass). Kept local to avoid depending on package main.
func boundEntitySpecsForTest(entities []spiceboxv1alpha1.BoundEntityType) []authz.BoundEntitySpec {
	out := make([]authz.BoundEntitySpec, 0, len(entities))
	for _, et := range entities {
		spec := authz.BoundEntitySpec{
			ResourceType: et.ResourceType,
			Permission:   et.Permission,
			Defaults:     et.Defaults,
		}
		for _, af := range et.AutoFillArgs {
			spec.AutoFillArgs = append(spec.AutoFillArgs, authz.AutoFillArgSpec{
				ArgName:         af.ArgName,
				ToolNamePattern: af.ToolNamePattern,
			})
		}
		out = append(out, spec)
	}
	return out
}

// TestLoop_BindClassDefaults_WritesAllowedDefaultsToMemory verifies that
// Engine.BindClassDefaults with allowed defaults populates the SessionScope
// with ScopeResource entries tagged source=default, and grants a slot per
// allowed default.
func TestLoop_BindClassDefaults_WritesAllowedDefaultsToMemory(t *testing.T) {
	rec, mem, scope, slots := buildRecordingEngine(t)

	classEntities := []spiceboxv1alpha1.BoundEntityType{
		{
			ResourceType: "github_repo",
			Permission:   "read",
			Defaults:     []string{"demo-org/demo-repo", "demo-org/internal"},
		},
	}
	entities := boundEntitySpecsForTest(classEntities)

	err := rec.BindClassDefaults(memory.WithSystemApproval(context.Background(), "test"), scope, bindTestSession(), entities, "alice@example.com")
	require.NoError(t, err)

	// Recording: exactly one call with the right arguments.
	assert.Equal(t, 1, rec.bindClassDefaultsCalls, "BindClassDefaults should be called once")
	assert.Equal(t, "alice@example.com", rec.lastDefaultsSubject)
	require.Len(t, rec.lastDefaultsEntities, 1)
	assert.Equal(t, "github_repo", rec.lastDefaultsEntities[0].ResourceType)

	// SpiceDB state: a slot grant per allowed default. This is the half that
	// authorizes — the scope document below only narrows.
	granted, err := slots.ListSlotGrants(context.Background(), "default", "bind-test")
	require.NoError(t, err)
	assert.ElementsMatch(t, []authz.SlotBinding{
		{ResourceType: "github_repo", ResourceID: authz.TrustedObjectID("demo-org/demo-repo"), Permission: "read"},
		{ResourceType: "github_repo", ResourceID: authz.TrustedObjectID("demo-org/internal"), Permission: "read"},
	}, granted, "each allowed default must bind a slot grant carrying the slot's permission")

	// Memory state: defaults landed as SessionScope ScopeResource entries.
	sc, ok, err := sessionscope.Get(memory.WithSystemApproval(context.Background(), "test"), mem, scope)
	require.NoError(t, err)
	require.True(t, ok, "session_scope entry must exist after BindClassDefaults")
	require.Len(t, sc.Resources, 1, "one ScopeResource entry for github_repo")
	r := sc.Resources[0]
	assert.Equal(t, "github_repo", r.ResourceType)
	assert.Equal(t, "default", string(r.Source))
	assert.ElementsMatch(t, []string{"demo-org/demo-repo", "demo-org/internal"}, r.IDs)
}

// TestLoop_Autofill_ReadsWhatBindClassDefaultsGranted closes the loop between
// the two halves of this seam. Autofill used to read the Layer-2 scope
// document while the Check read SpiceDB, so the value the agent was handed and
// the value the gate would admit came from different stores. Both now come
// from the grants, and this asserts it end-to-end through the Engine rather
// than through either function alone.
func TestLoop_Autofill_ReadsWhatBindClassDefaultsGranted(t *testing.T) {
	rec, _, scope, _ := buildRecordingEngine(t)

	entities := boundEntitySpecsForTest([]spiceboxv1alpha1.BoundEntityType{{
		ResourceType: "github_repo",
		Permission:   "read",
		Defaults:     []string{"demo-org/demo-repo"},
		AutoFillArgs: []spiceboxv1alpha1.AuthzSlotAutoFillArg{{ArgName: "repo", ToolNamePattern: "gh_*"}},
	}})

	require.NoError(t, rec.BindClassDefaults(memory.WithSystemApproval(context.Background(), "test"),
		scope, bindTestSession(), entities, "alice@example.com"))

	filled, err := rec.FillToolArgs(context.Background(), bindTestSession(), entities, "gh_repo_view", json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"repo":"demo-org/demo-repo"}`, string(filled),
		"autofill must read the instance the bind step granted")

	// A session that was granted nothing fills nothing — the lookup is keyed on
	// the session, so a grant cannot leak across sessions.
	other, err := rec.FillToolArgs(context.Background(), authz.SessionRef{Namespace: "default", Name: "other-session"},
		entities, "gh_repo_view", json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(other), "another session's grants must not fill this session's args")
}

// TestLoop_BindClassDefaults_DroppedOnDenied verifies that defaults whose
// Check returns Denied are not written to memory.
func TestLoop_BindClassDefaults_DroppedOnDenied(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "default/bind-deny-test"}

	// Wire an engine with a ToolChecker that always denies.
	denyChecker := &denyAllToolChecker{}
	slots := newSlotGrantStore()
	inner := engine.New(engine.Deps{
		ToolChecker: denyChecker,
		RelWriter:   slots,
		SlotLister:  slots,
		Memory:      mem,
	})
	rec := &recordingEngine{Engine: inner}

	entities := boundEntitySpecsForTest([]spiceboxv1alpha1.BoundEntityType{
		{
			ResourceType: "github_repo",
			Permission:   "read",
			Defaults:     []string{"secret/repo"},
		},
	})

	err := rec.BindClassDefaults(memory.WithSystemApproval(context.Background(), "test"), scope, bindTestSession(), entities, "alice@example.com")
	require.NoError(t, err)

	assert.Equal(t, 1, rec.bindClassDefaultsCalls, "BindClassDefaults should be called once")

	// Memory state: no session_scope written because Check denied all defaults.
	_, ok, err := sessionscope.Get(memory.WithSystemApproval(context.Background(), "test"), mem, scope)
	require.NoError(t, err)
	assert.False(t, ok, "denied defaults must not create a session_scope entry")

	granted, err := slots.ListSlotGrants(context.Background(), "default", "bind-test")
	require.NoError(t, err)
	assert.Empty(t, granted, "a default the Check refuses must not become a grant either")
}

// denyAllToolChecker is a test-local engine.ToolChecker that always denies.
type denyAllToolChecker struct{}

func (d *denyAllToolChecker) CheckToolCall(_ context.Context, _ authz.Permission, _ authz.Inputs) authz.Result {
	return authz.Result{Outcome: authz.OutcomeDenied, Message: "test: always deny"}
}
