// pkg/controllers/guardian/spicedbbootstrap_controller_test.go
//
// End-to-end controller tests for the SpiceDBBootstrap reconcile flow.
// Uses the in-process fake client (no envtest binaries needed) plus a
// recording fakeWriter for the SpiceDB relationship surface and the
// existing fakeSchemaIO for the schema surface. Real-SpiceDB integration
// is covered separately by Task 11.
//
// Shared helpers (fakeSchemaIO, findCondition) live next door in
// agentsessiongrants_controller_test.go — don't redeclare.
package guardian_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/guardian"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// fakeWriter records every WriteRelationships / DeleteRelationships
// call and returns no error by default. Satisfies spicedb.BootstrapWriter.
// Tests can set deleteErr to simulate a transient DELETE failure (used
// by TestBootstrap_DeleteFailure_Retried to verify the reconciler keeps
// the tuple in lastDesired for a follow-up retry).
type fakeWriter struct {
	mu        sync.Mutex
	writes    []*v1.WriteRelationshipsRequest
	deletes   []*v1.DeleteRelationshipsRequest
	deleteErr error
}

func (f *fakeWriter) WriteRelationships(_ context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, req)
	return &v1.WriteRelationshipsResponse{}, nil
}

func (f *fakeWriter) DeleteRelationships(_ context.Context, req *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, req)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &v1.DeleteRelationshipsResponse{}, nil
}

// setDeleteErr installs a transient-failure injector on the next (and
// any subsequent) DeleteRelationships call. Pass nil to clear.
func (f *fakeWriter) setDeleteErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteErr = err
}

func (f *fakeWriter) WriteCount() int  { f.mu.Lock(); defer f.mu.Unlock(); return len(f.writes) }
func (f *fakeWriter) DeleteCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.deletes) }

// writesNameSubjectSince reports whether any WriteRelationships call
// recorded from index `from` onward (see WriteCount, captured before the
// reconcile under test) TOUCHes a relationship whose subject id is
// subjectID. Every currently-desired tuple is re-TOUCHed on every
// non-debounced reconcile pass (ComputeDiff's ToTouch is unconditionally
// ALL of newDesired, not just newly-added tuples), so a raw WriteCount
// delta can't distinguish "the same old tuple was harmlessly re-touched"
// from "a NEW tuple was written it should not have been" — this can.
func (f *fakeWriter) writesNameSubjectSince(from int, subjectID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.writes[from:] {
		for _, u := range req.Updates {
			if u.Relationship.GetSubject().GetObject().GetObjectId() == subjectID {
				return true
			}
		}
	}
	return false
}

// deletesNameSubjectSince is writesNameSubjectSince's DeleteRelationships
// counterpart: reports whether any delete call recorded from index `from`
// onward targets a filter naming subjectID.
func (f *fakeWriter) deletesNameSubjectSince(from int, subjectID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.deletes[from:] {
		if req.RelationshipFilter.GetOptionalSubjectFilter().GetOptionalSubjectId() == subjectID {
			return true
		}
	}
	return false
}

// conditionStatus returns the Status of the named condition, or empty
// string if the condition is absent. Convenience for testify assertions
// that read better as `assert.Equal(metav1.ConditionTrue, conditionStatus(...))`
// than nested NotNil + status lookups.
func conditionStatus(conds []metav1.Condition, t string) metav1.ConditionStatus {
	if c := findCondition(conds, t); c != nil {
		return c.Status
	}
	return ""
}

// newBootReconciler builds a Reconciler wired against a fake client
// pre-loaded with objs, the package-local fakeSchemaIO recorder, and a
// fakeWriter recorder for the SpiceDB relationship surface. Debounce is
// disabled so consecutive Reconcile calls in the same test all reach
// the I/O path.
func newBootReconciler(t *testing.T, objs ...client.Object) (*guardian.Reconciler, client.Client, *fakeSchemaIO, *fakeWriter) {
	t.Helper()
	scheme := testfixtures.NewScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&spiceboxv1alpha1.SpiceDBBootstrap{},
			&spiceboxv1alpha1.AgentSessionGrants{},
		).
		WithObjects(objs...).
		Build()
	io := &fakeSchemaIO{}
	w := &fakeWriter{}
	r := guardian.NewReconciler(c, io, w)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares
	// Tests drive Reconcile multiple times back-to-back; the 5s default
	// debounce would short-circuit every call after the first and the
	// refcount + ordering scenarios depend on subsequent passes
	// actually reaching applyDeletes / RunAll.
	guardian.SetDebounceForTest(r, 0)
	return r, c, io, w
}

// reconcileBootstrapTick fires one Reconcile against the synthetic
// bootstrap-tick key. The reconciler ignores the request name (it lists
// everything cluster-wide), so any non-empty key works; using the
// sentinel keeps intent obvious in test traces.
func reconcileBootstrapTick(t *testing.T, r *guardian.Reconciler) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "__bootstrap_tick__"},
	})
	require.NoError(t, err)
}

func mkBoot(name string, rels ...spiceboxv1alpha1.SpiceDBBootstrapRelationship) *spiceboxv1alpha1.SpiceDBBootstrap {
	return &spiceboxv1alpha1.SpiceDBBootstrap{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", Generation: 1,
		},
		Spec: spiceboxv1alpha1.SpiceDBBootstrapSpec{
			Relationships: rels,
			ReclaimPolicy: spiceboxv1alpha1.SpiceDBBootstrapReclaimDelete,
		},
	}
}

func relGroupMember(canonical string) spiceboxv1alpha1.SpiceDBBootstrapRelationship {
	return spiceboxv1alpha1.SpiceDBBootstrapRelationship{
		Resource: spiceboxv1alpha1.SpiceDBObjectRef{Type: "group", ID: "engineering"},
		Relation: "member",
		Subject:  spiceboxv1alpha1.SpiceDBSubjectRef{Type: "user", ID: canonical},
	}
}

// TestBootstrap_AppliesRelationships covers the happy path: three
// distinct tuples land on the first reconcile, Valid + RelationshipsApplied
// both flip True, and ObservedRelationships matches the spec count.
func TestBootstrap_AppliesRelationships(t *testing.T) {
	boot := mkBoot("seeds",
		relGroupMember("alice-canon"),
		relGroupMember("bob-canon"),
		relGroupMember("carol-canon"),
	)
	r, c, _, w := newBootReconciler(t, boot)
	reconcileBootstrapTick(t, r)

	var got spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "seeds", Namespace: "default"}, &got))
	assert.Equal(t, metav1.ConditionTrue,
		conditionStatus(got.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionValid))
	assert.Equal(t, metav1.ConditionTrue,
		conditionStatus(got.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionRelationshipsApplied))
	assert.Equal(t, int32(3), got.Status.ObservedRelationships)
	assert.Equal(t, 3, w.WriteCount())
}

// TestBootstrap_Refcount_DropOneCR — two CRs share a tuple; deleting one
// keeps the tuple. The second reconcile observes cr1 as
// DeletionTimestamp != nil, drops it from newDesired, but cr2 still
// claims the tuple so ComputeDiff produces no delete.
func TestBootstrap_Refcount_DropOneCR(t *testing.T) {
	cr1 := mkBoot("cr1", relGroupMember("alice-canon"))
	cr2 := mkBoot("cr2", relGroupMember("alice-canon"))
	r, c, _, w := newBootReconciler(t, cr1, cr2)
	reconcileBootstrapTick(t, r) // initial pass: both touched + finalizer added

	// Delete cr1 via the client's Delete method. The first reconcile
	// installed the SpiceDBBootstrap finalizer on cr1, so the fake
	// client (mirroring real K8s) keeps the object around with a
	// non-nil DeletionTimestamp until the finalizer is removed —
	// exactly the state buildNewDesired keys off of.
	var got spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "cr1", Namespace: "default"}, &got))
	require.NoError(t, c.Delete(context.Background(), &got))

	deleteCountBefore := w.DeleteCount()
	reconcileBootstrapTick(t, r)
	assert.Equal(t, deleteCountBefore, w.DeleteCount(),
		"tuple still claimed by cr2; no new delete")
}

// TestBootstrap_Refcount_DropLastCR — sole Delete owner gone → DELETE
// fires. Mirror of DropOneCR with a single CR; ComputeDiff sees the
// tuple drop out of newDesired with no surviving owner and queues it
// for delete.
func TestBootstrap_Refcount_DropLastCR(t *testing.T) {
	cr := mkBoot("cr-sole", relGroupMember("alice-canon"))
	r, c, _, w := newBootReconciler(t, cr)
	reconcileBootstrapTick(t, r) // initial pass: tuple touched + finalizer added

	// Delete via the client; finalizer keeps the object around with
	// DeletionTimestamp != nil so buildNewDesired drops cr-sole's
	// claim and ComputeDiff queues its tuple for delete.
	var got spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "cr-sole", Namespace: "default"}, &got))
	require.NoError(t, c.Delete(context.Background(), &got))

	deleteCountBefore := w.DeleteCount()
	reconcileBootstrapTick(t, r)
	assert.Equal(t, deleteCountBefore+1, w.DeleteCount(),
		"sole Delete owner gone → DELETE")
}

// TestBootstrap_SchemaFragmentLands — schema-only CR; SchemaIncluded
// flips True and the composed schema written through SchemaIO contains
// the new resource definition contributed by the CR.
func TestBootstrap_SchemaFragmentLands(t *testing.T) {
	boot := &spiceboxv1alpha1.SpiceDBBootstrap{
		ObjectMeta: metav1.ObjectMeta{Name: "schema-only", Namespace: "default", Generation: 1},
		Spec: spiceboxv1alpha1.SpiceDBBootstrapSpec{
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{{
					Standing:  spiceboxv1alpha1.StandingSessionOnly,
					Name:      "team",
					Relations: []spiceboxv1alpha1.SpiceDBRelation{{Name: "lead", SubjectType: "user"}},
				}},
			},
		},
	}
	r, c, io, _ := newBootReconciler(t, boot)
	reconcileBootstrapTick(t, r)

	var got spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "schema-only", Namespace: "default"}, &got))
	assert.Equal(t, metav1.ConditionTrue,
		conditionStatus(got.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionSchemaIncluded))
	require.Greater(t, len(io.writes), 0, "expected at least one WriteSchema call")
	assert.Contains(t, io.writes[len(io.writes)-1], "definition team")
}

// TestBootstrap_SchemaConflict_MarksInvalid — MCPServer and SpiceDBBootstrap
// both contribute a `team` resource with conflicting relation shapes.
//
// INVERTED again for Task 5 (order the partition by trust tier). This test
// previously pinned MCP1 (tenant-authored, key "default/mcp1") winning the
// conflict purely because "default/mcp1" sorts lexicographically before
// "spicedbbootstrap:default/conflict" — an accident of naming that let a
// tenant-authored fragment silently displace an operator-authored one.
// guardianschema.PartitionCompatibleFragments now sorts candidates by trust
// TIER first (guardianschema.FragmentTier: TierOperator > TierTenant) and
// only falls back to key within a tier, so the operator-authored bootstrap
// now wins this conflict regardless of how its key sorts against mcp1's.
//
// So "conflict" (the bootstrap) now:
//   - stays Valid=True/AllChecksPassed — its OWN spec has nothing wrong with
//     it, unchanged from before.
//   - stays SchemaIncluded=True/FragmentLanded — it is now the WINNING side
//     of the conflict, not the losing one.
//
// mcp1 is now the LOSING side and carries the guardian-owned
// SpiceDBSchemaValid=False/FragmentConflict condition. The bootstrap's
// fragment (not mcp1's) is what lands in the written schema.
func TestBootstrap_SchemaConflict_MarksInvalid(t *testing.T) {
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp1", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "mcp1", Version: "v1",
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://example/mcp", Transport: "http"},
			Tools:  []spiceboxv1alpha1.MCPServerTool{},
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{{
					Standing:  spiceboxv1alpha1.StandingSessionOnly,
					Name:      "team",
					Relations: []spiceboxv1alpha1.SpiceDBRelation{{Name: "lead", SubjectType: "user"}},
				}},
			},
		},
	}
	boot := &spiceboxv1alpha1.SpiceDBBootstrap{
		ObjectMeta: metav1.ObjectMeta{Name: "conflict", Namespace: "default", Generation: 1},
		Spec: spiceboxv1alpha1.SpiceDBBootstrapSpec{
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{{
					Standing:  spiceboxv1alpha1.StandingSessionOnly,
					Name:      "team",
					Relations: []spiceboxv1alpha1.SpiceDBRelation{{Name: "member", SubjectType: "user"}},
				}},
			},
		},
	}
	r, c, io, _ := newBootReconciler(t, mcp, boot)
	reconcileBootstrapTick(t, r)

	var got spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "conflict", Namespace: "default"}, &got))

	// Valid reflects the CR's own spec, which is fine on its own — it is
	// NOT the conflict signal.
	assert.Equal(t, metav1.ConditionTrue,
		conditionStatus(got.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionValid))

	// SchemaIncluded stays True: the operator-authored bootstrap now WINS
	// the tier-ordered conflict against the tenant-authored MCPServer, even
	// though "default/mcp1" sorts before "spicedbbootstrap:default/conflict"
	// lexicographically.
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionSchemaIncluded)
	require.NotNil(t, cond, "conflict should carry SchemaIncluded; got=%+v", got.Status.Conditions)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "FragmentLanded", cond.Reason)

	// The winning SpiceDBBootstrap's body is what's actually written, NOT
	// mcp1's. The NotContains substring is scoped to the closing brace (not
	// just "relation lead: user"), because mcp1's own resource name ("team")
	// would otherwise partially match the accepted body's own header line.
	require.Greater(t, len(io.writes), 0, "expected at least one WriteSchema call")
	written := io.writes[len(io.writes)-1]
	assert.Contains(t, written, "definition team {\n\trelation member: user\n}", "boot's body must win the team conflict")
	assert.NotContains(t, written, "relation lead: user\n}", "mcp1's conflicting body must NOT be written")

	// mcp1 (the losing side) carrying the guardian-owned
	// SpiceDBSchemaValid=False/FragmentConflict condition is asserted at
	// Reconcile level by
	// TestReconciler_OperatorBootstrapBeatsAnEarlierSortingTenantFragment in
	// agentsessiongrants_controller_test.go, which runs against a real
	// envtest apiserver. This file's harness (newBootReconciler) registers
	// only SpiceDBBootstrap and AgentSessionGrants via
	// WithStatusSubresource — an MCPServer status Patch through the fake
	// client here returns NotFound, a fake-client harness gap unrelated to
	// the tier-ordering fix under test.
}

// TestBootstrap_Ordering_DeleteBeforeSchemaWrite asserts the §4.1
// invariant: a spec edit that drops a relation AND the dependent tuple
// in the same pass must DELETE the tuple BEFORE rewriting the schema.
// Otherwise the schema write would fail because SpiceDB rejects a
// schema change that strands existing tuples.
func TestBootstrap_Ordering_DeleteBeforeSchemaWrite(t *testing.T) {
	boot := &spiceboxv1alpha1.SpiceDBBootstrap{
		ObjectMeta: metav1.ObjectMeta{Name: "team-with-lead", Namespace: "default", Generation: 1},
		Spec: spiceboxv1alpha1.SpiceDBBootstrapSpec{
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{{
					Standing:  spiceboxv1alpha1.StandingSessionOnly,
					Name:      "team",
					Relations: []spiceboxv1alpha1.SpiceDBRelation{{Name: "lead", SubjectType: "user"}},
				}},
			},
			Relationships: []spiceboxv1alpha1.SpiceDBBootstrapRelationship{{
				Resource: spiceboxv1alpha1.SpiceDBObjectRef{Type: "team", ID: "alpha"},
				Relation: "lead",
				Subject:  spiceboxv1alpha1.SpiceDBSubjectRef{Type: "user", ID: "bob-canon"},
			}},
			ReclaimPolicy: spiceboxv1alpha1.SpiceDBBootstrapReclaimDelete,
		},
	}
	r, c, io, w := newBootReconciler(t, boot)
	reconcileBootstrapTick(t, r)
	require.Equal(t, 1, w.WriteCount(), "initial: tuple touched")

	// Edit: drop the lead relation AND the dependent tuple.
	var got spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "team-with-lead", Namespace: "default"}, &got))
	got.Spec.SpiceDBSchema.Resources[0].Relations = nil
	got.Spec.Relationships = nil
	got.Generation = 2
	require.NoError(t, c.Update(context.Background(), &got))

	deleteCountBefore := w.DeleteCount()
	writesBefore := len(io.writes)
	reconcileBootstrapTick(t, r)

	require.Equal(t, deleteCountBefore+1, w.DeleteCount(), "tuple deleted")
	require.Greater(t, len(io.writes), writesBefore, "schema rewritten")
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "team-with-lead", Namespace: "default"}, &got))
	assert.Equal(t, metav1.ConditionTrue,
		conditionStatus(got.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionSchemaIncluded))
}

// TestBootstrap_DeleteFailure_Retried covers the spec §4.4 retry
// contract: a transient DELETE failure must NOT silently drop the
// tuple from the controller's lastDesired cache — the next reconcile
// has to re-issue the DELETE. Without the fix, a single transient
// gRPC error orphans the tuple forever.
// TestBootstrap_RestartMidDelete_StillReapsTuples — a SpiceDBBootstrap deleted
// while the operator is down must still have its reclaimPolicy=Delete tuples
// reaped by the process that comes up next.
//
// The claims a CR made live only in the in-process lastDesired snapshot;
// status carries just observedRelationships, a count. So after a restart
// buildNewDesired skips the deleting CR (correct — that drop-out is what queues
// the reap), ComputeDiff walks an empty prior map and finds nothing to delete,
// removeFinalizersOnDeleted sees zero delete failures and strips the finalizer,
// and the CR is garbage-collected while its tuples stay in SpiceDB with nothing
// left referencing them. Nothing ever reaps them.
func TestBootstrap_RestartMidDelete_StillReapsTuples(t *testing.T) {
	ctx := context.Background()
	key := types.NamespacedName{Name: "cr-restart", Namespace: "default"}

	cr := mkBoot("cr-restart", relGroupMember("alice-canon"))
	r, c, io, w := newBootReconciler(t, cr)
	reconcileBootstrapTick(t, r) // tuple touched + finalizer installed
	require.Equal(t, 1, w.WriteCount(), "initial touch landed")

	// Delete the CR. The finalizer keeps the object (and therefore its spec)
	// around with a non-nil DeletionTimestamp.
	var got spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, c.Get(ctx, key, &got))
	require.NoError(t, c.Delete(ctx, &got))

	// Operator restart: a brand-new Reconciler over the same cluster state,
	// with an empty in-process lastDesired. Same fake client, fresh recorders.
	restarted := &fakeWriter{}
	r2 := guardian.NewReconciler(c, io, restarted)
	guardian.SetDebounceForTest(r2, 0)
	reconcileBootstrapTick(t, r2)

	assert.Equal(t, 1, restarted.DeleteCount(),
		"the deleting CR's spec is still readable and its tuples are a pure function of it; the reap must not depend on an in-process snapshot the restart erased")
}

func TestBootstrap_DeleteFailure_Retried(t *testing.T) {
	cr := mkBoot("flake", relGroupMember("alice-canon"))
	r, c, _, w := newBootReconciler(t, cr)
	reconcileBootstrapTick(t, r)
	require.Equal(t, 1, w.WriteCount(), "initial touch landed")

	// Drop the tuple from spec → next reconcile will try to DELETE.
	var got spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "flake", Namespace: "default"}, &got))
	got.Spec.Relationships = nil
	got.Generation = 2
	require.NoError(t, c.Update(context.Background(), &got))

	// First reconcile: DELETE fails.
	w.setDeleteErr(errors.New("transient gRPC error"))
	reconcileBootstrapTick(t, r)
	require.Equal(t, 1, w.DeleteCount(), "first DELETE attempted")

	// Second reconcile: the writer recovers; controller MUST retry.
	w.setDeleteErr(nil)
	reconcileBootstrapTick(t, r)
	assert.Equal(t, 2, w.DeleteCount(), "DELETE retried after transient failure")
}

// TestBootstrap_BecomesInvalid_KeepsTuples covers the spec §4.4
// contract: "L2 validation fails → Valid=False, no SpiceDB I/O." When
// an operator edits a previously-valid CR into an invalid state
// (e.g. canonicalize=true on a non-email subject), the CR's already-
// applied tuples MUST stay in SpiceDB until the operator either fixes
// the spec or deletes the CR. Without the fix, the typo silently
// reaps live SpiceDB state.
func TestBootstrap_BecomesInvalid_KeepsTuples(t *testing.T) {
	cr := mkBoot("flippy", relGroupMember("alice-canon"))
	r, c, _, w := newBootReconciler(t, cr)
	reconcileBootstrapTick(t, r)
	require.Equal(t, 1, w.WriteCount(), "initial touch landed")

	// Edit to make the CR invalid: canonicalize=true with an id that
	// isn't an email triggers ReasonCanonicalizeRequiresUserEmail.
	var got spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "flippy", Namespace: "default"}, &got))
	got.Spec.Relationships = []spiceboxv1alpha1.SpiceDBBootstrapRelationship{{
		Resource: spiceboxv1alpha1.SpiceDBObjectRef{Type: "group", ID: "engineering"},
		Relation: "member",
		Subject: spiceboxv1alpha1.SpiceDBSubjectRef{
			Type: "user", ID: "alice-canon", Canonicalize: true, // id lacks @
		},
	}}
	got.Generation = 2
	require.NoError(t, c.Update(context.Background(), &got))

	deletesBefore := w.DeleteCount()
	reconcileBootstrapTick(t, r)

	// Valid must flip to False.
	var post spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "flippy", Namespace: "default"}, &post))
	assert.Equal(t, metav1.ConditionFalse,
		conditionStatus(post.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionValid))

	// But no DELETE: the previously-applied tuple must remain in SpiceDB.
	assert.Equal(t, deletesBefore, w.DeleteCount(),
		"becoming-invalid must NOT delete previously-applied tuples")
}

// TestBootstrap_FragmentRejected_RelationshipsNotWrittenPriorTuplesKept
// covers Task 4 review round 1, Finding 1 (Important) / Ruling 13: a
// bootstrap whose FRAGMENT was rejected by the isolation pass carries
// Valid=True (its own spec is fine — see validateBootstraps) with only
// SchemaIncluded=False, so validateBootstraps' invalidBoots set alone did
// NOT cover it. Before excludedBootstrapKeys was unioned into the skip set
// buildNewDesired reads, a fragment-rejected CR's spec.relationships were
// resolved and TOUCHed exactly like a perfectly valid CR's — either
// touching a relation only the rejected fragment declared (which a live
// SpiceDB would reject forever), or worse, landing tuples inside the
// WINNING fragment's same-named definition. Rejecting a fragment must also
// stop the writes that depend on it — the same "L2-invalid CRs get no new
// I/O, prior tuples carried forward" contract TestBootstrap_BecomesInvalid_
// KeepsTuples already proves for a spec-shape failure, now proven for a
// fragment-isolation rejection too.
//
// Phase 1: boot-b alone, no conflict — its relationship (alice-canon) lands
// normally. Phase 2: boot-a appears and wins a same-name conflict against
// boot-b's fragment (boot-a sorts first, "spicedbbootstrap:default/boot-a"
// < ".../boot-b"), and boot-b SIMULTANEOUSLY gains a second relationship
// (bob-canon) — the one a regression would write despite the rejection.
// writesNameSubjectSince (not a raw WriteCount delta) is what actually
// discriminates: ComputeDiff's ToTouch is unconditionally every tuple in
// newDesired, so alice-canon's tuple is harmlessly re-TOUCHed on EVERY
// pass regardless of this fix — only bob-canon's presence or absence in
// the phase-2 writes tells the two behaviors apart.
func TestBootstrap_FragmentRejected_RelationshipsNotWrittenPriorTuplesKept(t *testing.T) {
	bootB := &spiceboxv1alpha1.SpiceDBBootstrap{
		ObjectMeta: metav1.ObjectMeta{Name: "boot-b", Namespace: "default", Generation: 1},
		Spec: spiceboxv1alpha1.SpiceDBBootstrapSpec{
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{{
					Standing:  spiceboxv1alpha1.StandingSessionOnly,
					Name:      "shared_rel_res",
					Relations: []spiceboxv1alpha1.SpiceDBRelation{{Name: "member", SubjectType: "user"}},
				}},
			},
			Relationships: []spiceboxv1alpha1.SpiceDBBootstrapRelationship{relGroupMember("alice-canon")},
			ReclaimPolicy: spiceboxv1alpha1.SpiceDBBootstrapReclaimDelete,
		},
	}
	r, c, _, w := newBootReconciler(t, bootB)
	reconcileBootstrapTick(t, r) // no conflict yet: boot-b's fragment composes alone
	require.Equal(t, 1, w.WriteCount(), "boot-b's relationship lands while its fragment is still uncontested")

	// boot-a appears, contributing a CONFLICTING body for the same
	// resource name, and sorts first — so boot-a wins and boot-b's
	// fragment is rejected.
	bootA := &spiceboxv1alpha1.SpiceDBBootstrap{
		ObjectMeta: metav1.ObjectMeta{Name: "boot-a", Namespace: "default", Generation: 1},
		Spec: spiceboxv1alpha1.SpiceDBBootstrapSpec{
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{{
					Standing:  spiceboxv1alpha1.StandingSessionOnly,
					Name:      "shared_rel_res", // conflicts with boot-b's body
					Relations: []spiceboxv1alpha1.SpiceDBRelation{{Name: "lead", SubjectType: "user"}},
				}},
			},
		},
	}
	require.NoError(t, c.Create(context.Background(), bootA), "create boot-a (wins the conflict)")

	// boot-b simultaneously gains a SECOND relationship — the one that
	// must NOT be written now that its fragment has lost the conflict.
	var editing spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "boot-b", Namespace: "default"}, &editing))
	editing.Spec.Relationships = append(editing.Spec.Relationships, relGroupMember("bob-canon"))
	editing.Generation = 2
	require.NoError(t, c.Update(context.Background(), &editing))

	writesBefore := w.WriteCount()
	deletesBefore := w.DeleteCount()
	reconcileBootstrapTick(t, r)

	// boot-b's fragment lost the conflict: Valid stays True (its own spec
	// is fine — this is NOT a spec-shape failure), SchemaIncluded flips
	// False.
	var gotB spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "boot-b", Namespace: "default"}, &gotB))
	assert.Equal(t, metav1.ConditionTrue,
		conditionStatus(gotB.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionValid),
		"boot-b's own spec is fine — Valid stays True")
	condSI := findCondition(gotB.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionSchemaIncluded)
	require.NotNil(t, condSI, "boot-b must carry SchemaIncluded; got %+v", gotB.Status.Conditions)
	assert.Equal(t, metav1.ConditionFalse, condSI.Status, "boot-b's fragment lost the conflict")
	assert.Equal(t, spiceboxv1alpha1.ReasonSpicedbSchemaConflict, condSI.Reason)

	// The regression this fix closes: despite Valid=True, boot-b's NEW
	// relationship (bob-canon) must NOT be written — its fragment being
	// excluded from the schema means the resource/relation it names may
	// not even exist, or may exist only under the WINNING fragment's
	// different body.
	assert.False(t, w.writesNameSubjectSince(writesBefore, "bob-canon"),
		"a fragment-rejected bootstrap must not get a NEW relationship TOUCHed")

	// Its PRE-EXISTING tuple (alice-canon, applied in phase 1) must not be
	// DELETEd — a fragment rejection is not a relationship removal.
	assert.Equal(t, deletesBefore, w.DeleteCount(),
		"a fragment-rejected bootstrap's previously-applied tuples must be carried forward, not deleted")
}

// TestBootstrap_RefusedRelationship_DeletionCompletes is the regression pin
// for the Major fix: a tuple whose TOUCH was refused (pt_tag#session,
// claimed by pttagmint — same shape as ownershipRel/guardedFakeWriter in
// bootstrap_validation_test.go) was never actually written, and must never
// enter r.lastDesired as though it had been.
//
// Before the fix it did: buildNewDesired leaves a refused relationship in
// newDesired exactly like any other one (see validateShape's doc comment on
// why ownership must not invalidate the whole CR), and the unconditional
// `r.lastDesired = newDesired` on schema success recorded it as landed
// regardless of the touch outcome. Deleting "wedge" then made
// buildNewDesired drop its claim, ComputeDiff see the tuple vanish from
// newDesired while still present in the (wrongly recorded) lastDesired, and
// queue a DELETE that CheckDeleteFilter refuses forever (pttagmint owns
// it) — deleteFailures never reaches 0, so removeFinalizersOnDeleted never
// runs. Because deleteFailures is a GLOBAL count for the whole reconcile,
// "clean-sibling" — deleted in the very same pass, with no ownership
// conflict at all — was wedged right along with it. Both CRs completing
// deletion here is the proof the fix reaches both halves of that claim.
func TestBootstrap_RefusedRelationship_DeletionCompletes(t *testing.T) {
	refusedRel := ownershipRel("pt_tag", "session")
	refusedRel.Subject.ID = "wedge-subject"
	wedge := &spiceboxv1alpha1.SpiceDBBootstrap{
		ObjectMeta: metav1.ObjectMeta{Name: "wedge", Namespace: "default", Generation: 1},
		Spec: spiceboxv1alpha1.SpiceDBBootstrapSpec{
			Relationships: []spiceboxv1alpha1.SpiceDBBootstrapRelationship{refusedRel},
			ReclaimPolicy: spiceboxv1alpha1.SpiceDBBootstrapReclaimDelete,
		},
	}
	clean := mkBoot("clean-sibling", relGroupMember("alice-canon"))

	r, c, _, w := newBootReconciler(t, wedge, clean)
	r.Writer = &guardedFakeWriter{fakeWriter: w, src: spicedb.BootstrapSource}

	reconcileBootstrapTick(t, r) // pass 1: wedge's tuple refused, clean's lands

	require.False(t, w.writesNameSubjectSince(0, "wedge-subject"),
		"precondition: the claimed relationship must never reach the writer")
	require.True(t, w.writesNameSubjectSince(0, "alice-canon"),
		"precondition: the sibling's clean relationship must land normally")

	// Delete BOTH CRs in the same pass.
	var got spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "wedge"}, &got))
	require.NoError(t, c.Delete(context.Background(), &got))
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "clean-sibling"}, &got))
	require.NoError(t, c.Delete(context.Background(), &got))

	deleteCountBefore := w.DeleteCount()
	reconcileBootstrapTick(t, r) // pass 2: must complete both deletions

	// clean-sibling's own tuple (never refused) legitimately vanishes from
	// newDesired and IS deleted here — that delete succeeding is part of the
	// fix, not a violation of it. What must NEVER happen is a delete attempt
	// naming the refused tuple's subject: it was never written, so ComputeDiff
	// must never see it as something to retract.
	assert.False(t, w.deletesNameSubjectSince(0, "wedge-subject"),
		"the refused tuple was never written, so ComputeDiff must never queue a DELETE for it")
	assert.Equal(t, deleteCountBefore+1, w.DeleteCount(),
		"exactly one delete (clean-sibling's own tuple) — the refused tuple must never reach a delete attempt")

	// The fake client mirrors a real apiserver here: once the last finalizer
	// is removed via a merge patch on an object already carrying
	// DeletionTimestamp, the object is gone — Get returns NotFound. That is
	// "the CR completes": the finalizer is the only thing standing between
	// DeletionTimestamp and actual removal.
	var afterWedge spiceboxv1alpha1.SpiceDBBootstrap
	errWedge := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "wedge"}, &afterWedge)
	assert.True(t, apierrors.IsNotFound(errWedge),
		"wedge's finalizer must be removed and the CR must complete deletion — a refusal on a tuple that was "+
			"never actually written must not wedge the CR that carried it; got err=%v", errWedge)

	var afterClean spiceboxv1alpha1.SpiceDBBootstrap
	errClean := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "clean-sibling"}, &afterClean)
	assert.True(t, apierrors.IsNotFound(errClean),
		"a co-deleted sibling with no ownership conflict at all must not be dragged down by wedge's "+
			"deleteFailures, which is a global count for the whole reconcile; got err=%v", errClean)
}
