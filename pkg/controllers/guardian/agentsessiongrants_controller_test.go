//go:build integration

package guardian_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	guardianschema "github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/controllers/guardian"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// githubRepoFragment is the slice-4 MCPServer.spec.spiceDBSchema fragment
// that contributes the `github_repo` resource definition to the
// composed unified schema. The controller now composes the SpiceDB
// schema from scratch on each pass (scaffold + MCPServer fragments +
// grant relations) — so any test that asserts on a (github_repo, *)
// pair must seed an MCPServer carrying this fragment, otherwise the
// pair gets skipped because the resource isn't declared anywhere.
//
// The `manage` permission is named apart from the `admin` relation it
// resolves through on purpose: `relation admin` and `permission admin` on the
// SAME definition is a relation/permission NAME COLLISION — SpiceDB has one
// namespace per definition for both — which spicedb's real type system
// refuses at WriteSchema. The fake SchemaIO used throughout this file never
// validated a written schema, so this collision sat unnoticed (and untested —
// no case here ever requested a `(github_repo, admin)` pair) until
// composeFragmentSet (Task 4 of the composable-schema migration) started
// validating fragment assembly through spicedb's own type system before ever
// returning a schema string.
func githubRepoFragment() *spiceboxv1alpha1.SpiceDBSchemaFragment {
	return &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{{
			Standing: spiceboxv1alpha1.StandingSessionOnly,
			Name:     "github_repo",
			Relations: []spiceboxv1alpha1.SpiceDBRelation{
				{Name: "reader", SubjectType: "user"},
				{Name: "admin", SubjectType: "user"},
			},
			Permissions: []spiceboxv1alpha1.SpiceDBPermission{
				{Name: "read", Expr: "reader + admin"},
				{Name: "manage", Expr: "admin"},
			},
		}},
	}
}

// mustComposeWithFragment is the test-side equivalent of what the
// reconciler will write: schema.ComposeAll(fragments, pairs). It lets
// "no-change" tests seed fakeSchemaIO.cur with the exact text the
// controller would produce so that the RunAll fast-path (cur == desired)
// skips the write.
func mustComposeWithFragment(t *testing.T, frag *spiceboxv1alpha1.SpiceDBSchemaFragment, pairs []guardianschema.GrantPair) string {
	t.Helper()
	out, err := guardianschema.ComposeAll(
		[]guardianschema.IdentifiedFragment{{Fragment: frag}},
		nil,
		pairs,
	)
	require.NoError(t, err, "ComposeAll")
	return out
}

// reconcileKey is shorthand for invoking r.Reconcile against an ASG by name.
func reconcileKey(t *testing.T, r *guardian.Reconciler, name, ns string) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
	})
	require.NoError(t, err, "Reconcile(%s/%s)", ns, name)
}

// TestControllerMarksSchemaIncludedOnHappyPath: an MCPServer contributing
// a `github_repo` fragment plus a single AgentSessionGrants with a
// (github_repo, read) pair → controller writes the composed unified
// schema (scaffold + fragment + grant) and marks SchemaIncluded=True
// on the CR.
func TestControllerMarksSchemaIncludedOnHappyPath(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares

	mcps := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "github", Version: "1.0.0",
			Server:        spiceboxv1alpha1.MCPServerServer{URL: "https://example/mcp"},
			Tools:         []spiceboxv1alpha1.MCPServerTool{{Name: "noop"}},
			SpiceDBSchema: githubRepoFragment(),
		},
	}
	require.NoError(t, env.Client.Create(ctx, mcps), "create MCPServer")

	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-grants", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{
				{ResourceType: "github_repo", Permission: "read"},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, asg), "create AgentSessionGrants")

	reconcileKey(t, r, "cls-grants", "default")

	require.Equal(t, 1, io.writeCount(), "WriteSchema call count")

	var got spiceboxv1alpha1.AgentSessionGrants
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "cls-grants", Namespace: "default"}, &got),
		"Get AgentSessionGrants")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded)
	require.NotNil(t, cond, "SchemaIncluded condition; got conditions=%+v", got.Status.Conditions)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"SchemaIncluded status; reason=%s msg=%s", cond.Reason, cond.Message)
	assert.Equal(t, "AllPairsResolved", cond.Reason, "SchemaIncluded reason")
	assert.EqualValues(t, 1, got.Status.ObservedPairCount, "ObservedPairCount")
	assert.NotNil(t, got.Status.ObservedSchemaWrittenAt, "ObservedSchemaWrittenAt should be set")

	// Slice-4 invariant: the written schema MUST contain BOTH the
	// MCPServer's resource definitions AND the grant relations. This
	// is the load-bearing assertion for the T17 controller wiring.
	written := io.lastWrite()
	for _, want := range []string{
		"definition github_repo {", // from the MCPServer fragment
		"relation reader: user",    //   - reader relation
		"permission read = reader + admin",
		"definition agentsession {", // scaffold
		"relation grant_read_github_repo: github_repo with check_hash and expiration", // composed grant
		"permission check_read_github_repo = grant_read_github_repo->read",            // composed check
	} {
		assert.Containsf(t, written, want, "written schema missing %q. Full schema:\n%s", want, written)
	}
}

// TestControllerMarksSchemaIncludedFalseOnSpiceDBError: when SchemaIO
// WriteSchema returns an error, SchemaIncluded flips to False with
// reason SpiceDBWriteFailed.
func TestControllerMarksSchemaIncludedFalseOnSpiceDBError(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	io := &fakeSchemaIO{cur: "", writeErr: errors.New("spicedb boom")}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares

	mcps := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "github", Version: "1.0.0",
			Server:        spiceboxv1alpha1.MCPServerServer{URL: "https://example/mcp"},
			Tools:         []spiceboxv1alpha1.MCPServerTool{{Name: "noop"}},
			SpiceDBSchema: githubRepoFragment(),
		},
	}
	require.NoError(t, env.Client.Create(ctx, mcps), "create MCPServer")

	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-err", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{
				{ResourceType: "github_repo", Permission: "read"},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, asg), "create AgentSessionGrants")

	reconcileKey(t, r, "cls-err", "default")

	var got spiceboxv1alpha1.AgentSessionGrants
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "cls-err", Namespace: "default"}, &got),
		"Get AgentSessionGrants")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded)
	require.NotNil(t, cond, "SchemaIncluded condition; got conditions=%+v", got.Status.Conditions)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "SchemaIncluded status")
	assert.Equal(t, "SpiceDBWriteFailed", cond.Reason, "SchemaIncluded reason")
}

// TestControllerMarksSchemaIncludedFalseOnComposeFailure: when RunAll fails
// BEFORE any write is attempted — composeAllWithSkipped's own validation
// refuses the post-surgery schema, not SpiceDB — the SchemaIncluded reason
// must be SchemaComposeFailed, distinct from the write-time SpiceDBWriteFailed
// TestControllerMarksSchemaIncludedFalseOnSpiceDBError above pins. Conflating
// the two would send an operator debugging a compose-time refusal to
// SpiceDB's own logs, which show nothing, because nothing was sent.
//
// Fixture: a session-only `gadget` resource declaring a `viewer` RELATION,
// slotted for permission `viewer` — the SAME name. composeOneSlot's insert
// path (guardian/schema/slots.go: "if !hasPerm") only recognizes an EXISTING
// `permission viewer = …` line; a relation of that name doesn't match, so it
// inserts a SECOND declaration named `viewer` alongside the relation.
// ValidateComposedSchema refuses the resulting duplicate relation/permission
// name in-process — the same shape pkg/authz/guardian/schema's
// TestRunAll_DoesNotWriteWhenValidationFails exercises directly against
// RunAll; this is the controller-level proof that the reason it surfaces on
// the CR reflects it correctly.
//
// This used to be a session-only `widget` resource with no `owner` relation,
// slotted for `read`: composeOneSlot unconditionally rewrote the permission
// to `slot_grant_read->interact + owner`, which resolved to nothing on
// `widget`. composeOneSlot now checks whether the target declares `owner`
// before writing that leg (definitionDeclaresName), so that fixture composes
// cleanly and no longer exercises this path — see
// pkg/authz/guardian/schema/postsurgery_validate_internal_test.go for the
// same swap and why.
func TestControllerMarksSchemaIncludedFalseOnComposeFailure(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares

	mcps := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "gadget-server", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "gadget-server", Version: "1.0.0",
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://example/mcp"},
			Tools:  []spiceboxv1alpha1.MCPServerTool{{Name: "noop"}},
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{{
					Name:      "gadget",
					Standing:  spiceboxv1alpha1.StandingSessionOnly,
					Relations: []spiceboxv1alpha1.SpiceDBRelation{{Name: "viewer", SubjectType: "user"}},
					Permissions: []spiceboxv1alpha1.SpiceDBPermission{
						{Name: "read", Expr: "viewer"},
					},
				}},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, mcps), "create MCPServer")

	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-compose-err", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Slots: []spiceboxv1alpha1.GrantPair{
				{ResourceType: "gadget", Permission: "viewer"},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, asg), "create AgentSessionGrants")

	reconcileKey(t, r, "cls-compose-err", "default")

	var got spiceboxv1alpha1.AgentSessionGrants
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "cls-compose-err", Namespace: "default"}, &got),
		"Get AgentSessionGrants")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded)
	require.NotNil(t, cond, "SchemaIncluded condition; got conditions=%+v", got.Status.Conditions)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "SchemaIncluded status")
	assert.Equal(t, "SchemaComposeFailed", cond.Reason,
		"SchemaIncluded reason must distinguish a compose-time refusal from a write-time one")
	assert.Zero(t, io.writeCount(), "a schema that fails validation must never reach WriteSchema")
}

// TestControllerSkipsWriteWhenNoChange: when the live schema already
// exactly matches what RunAll would compose (scaffold + fragment + grant),
// WriteSchema must NOT be invoked — and the CR still flips to
// SchemaIncluded=True.
func TestControllerSkipsWriteWhenNoChange(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	frag := githubRepoFragment()
	pairs := []guardianschema.GrantPair{
		{ResourceType: "github_repo", Permission: "read"},
	}
	// Seed cur with the exact text ComposeAll would produce so RunAll's
	// idempotent fast-path (cur == desired) short-circuits the write.
	io := &fakeSchemaIO{cur: mustComposeWithFragment(t, frag, pairs)}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares

	mcps := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "github", Version: "1.0.0",
			Server:        spiceboxv1alpha1.MCPServerServer{URL: "https://example/mcp"},
			Tools:         []spiceboxv1alpha1.MCPServerTool{{Name: "noop"}},
			SpiceDBSchema: frag,
		},
	}
	require.NoError(t, env.Client.Create(ctx, mcps), "create MCPServer")

	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-nochange", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{
				{ResourceType: "github_repo", Permission: "read"},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, asg), "create AgentSessionGrants")

	reconcileKey(t, r, "cls-nochange", "default")

	assert.Equal(t, 0, io.writeCount(),
		"WriteSchema should not be called when live schema already matches desired")

	var got spiceboxv1alpha1.AgentSessionGrants
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "cls-nochange", Namespace: "default"}, &got),
		"Get AgentSessionGrants")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded)
	require.NotNil(t, cond, "SchemaIncluded condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "SchemaIncluded status")
}

// hangingSchemaIO blocks on WriteSchema until ctx is canceled or
// release is closed. Used to verify the reconciler respects request
// ctx cancellation rather than blocking forever — the silent-crash
// failure mode AGENTS.md warns about for unbounded SpiceDB calls.
type hangingSchemaIO struct {
	cur     string
	release chan struct{}
}

func (h *hangingSchemaIO) ReadSchema(ctx context.Context) (string, error) {
	return h.cur, nil
}

func (h *hangingSchemaIO) WriteSchema(ctx context.Context, _ string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.release:
		return nil
	}
}

// TestReconciler_HangingWriteRespectsContextCancel: when SchemaIO.WriteSchema
// blocks indefinitely, the reconciler must NOT block its caller forever.
// The test drives Reconcile with a context whose deadline expires before
// the unblock signal and asserts that Reconcile returns with an error
// surface (the ctx error propagates out of guardianschema.Run). Without
// this guard a stuck SpiceDB call would freeze a controller-runtime
// worker forever, blocking every other ASG reconcile behind it.
func TestReconciler_HangingWriteRespectsContextCancel(t *testing.T) {
	env := testenv.Shared(t)

	io := &hangingSchemaIO{cur: "", release: make(chan struct{})}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares

	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-hang", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{
				{ResourceType: "github_repo", Permission: "read"},
			},
		},
	}
	require.NoError(t, env.Client.Create(context.Background(), asg), "create AgentSessionGrants")

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	done := make(chan struct{}, 1)
	go func() {
		// We do not assert on the returned err — controller-runtime's
		// production contract is "the reconciler must surface failures
		// non-fatally on the CR status, then requeue." With ctx
		// cancelled here the status patch will fail too, but the only
		// invariant this test cares about is "Reconcile returns within
		// the deadline rather than hanging on WriteSchema forever."
		_, _ = r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "cls-hang", Namespace: "default"},
		})
		close(done)
	}()

	select {
	case <-done:
		// Reconcile returned within deadline + a generous margin: the
		// hanging WriteSchema was correctly bounded by ctx
		// cancellation rather than blocking the worker indefinitely.
		// We do NOT assert on the CR status because patchSchemaIncluded
		// also uses the cancelled ctx and may fail to write — what we
		// care about is the reconciler's responsiveness.
	case <-time.After(2 * time.Second):
		close(io.release) // unblock so the goroutine doesn't leak
		t.Fatal("Reconcile blocked past ctx deadline; hanging SchemaIO not surfaced")
	}
	// Release the hanging writer so a still-blocked goroutine (in
	// the unhappy path) exits before the test ends. Channel close
	// is idempotent-safe only when the channel is open; guard with a
	// non-blocking receive first.
	select {
	case <-io.release:
		// already closed somewhere above; nothing to do
	default:
		close(io.release)
	}
}

// TestReconciler_NoCRsIsNoOp: with zero AgentSessionGrants CRs and
// zero MCPServers in the cluster, the reconciler must NOT churn the
// schema when the live schema already matches the canonical
// composed output (the bare scaffold). The composer pass would
// otherwise emit a redundant WriteSchema each reconcile.
//
// Slice-4 reframes the slice-2 contract: RunAll always composes the
// schema authoritatively (scaffold + fragments + pairs), so the
// "no write" invariant now hinges on cur == desired idempotency
// rather than the absence of pairs alone.
func TestReconciler_NoCRsIsNoOp(t *testing.T) {
	env := testenv.Shared(t)

	// Seed cur with what RunAll(nil fragments, nil pairs) would produce
	// so the idempotency fast-path short-circuits the write.
	canonicalEmpty, err := guardianschema.ComposeAll(nil, nil, nil)
	require.NoError(t, err, "ComposeAll(nil,nil)")
	io := &fakeSchemaIO{cur: canonicalEmpty}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares

	// No ASG created. A reconcile event keyed to a non-existent name
	// is the most common production trigger when the matching CR has
	// been deleted between enqueue and dequeue.
	reconcileKey(t, r, "absent", "default")

	assert.Equal(t, 0, io.writeCount(),
		"WriteSchema should not be called when cur already matches the canonical-empty schema")
}

// TestReconciler_NilSchemaIOIsHandled: a Reconciler constructed with
// a nil SchemaIO must NOT panic on first Reconcile. The reconciler
// short-circuits and returns ctrl.Result{} with no error, letting
// the AgentClass controller surface AgentSessionGrantsWritten=True
// without the schema-composition side-effect.
//
// Belt-and-suspenders test for the AGENTS.md "Nil interfaces" rule:
// the operator's main.go guards on spiceDBClient != nil before
// constructing the reconciler, but a future refactor that lifts
// the guard would otherwise crash silently inside controller-runtime's
// per-reconcile panic recovery. This test fails fast in CI before
// such a regression hits production.
func TestReconciler_NilSchemaIOIsHandled(t *testing.T) {
	env := testenv.Shared(t)

	r := guardian.NewReconciler(env.Client, nil, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares

	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-nilio", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{
				{ResourceType: "github_repo", Permission: "read"},
			},
		},
	}
	require.NoError(t, env.Client.Create(context.Background(), asg), "create AgentSessionGrants")

	// Reconcile must not panic.
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("Reconcile panicked with nil SchemaIO: %v", rec)
		}
	}()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "cls-nilio", Namespace: "default"},
	})
	assert.NoError(t, err, "Reconcile with nil SchemaIO should be a no-op")
}

// TestReconciler_RunAllAppliesMCPServerFragments is the slice-4 T17
// integration test: it verifies the controller composes the unified
// SpiceDB schema from BOTH (a) the MCPServer.spec.spiceDBSchema
// fragment AND (b) the AgentSessionGrants pair — exercising the
// schema.RunAll wiring end-to-end.
//
// Uses fake.NewClientBuilder rather than testenv so the test runs in
// any developer environment without etcd/kube-apiserver binaries.
// The envtest-based TestControllerMarksSchemaIncludedOnHappyPath
// asserts the same invariant under a real apiserver; this is the
// load-bearing local-runnable counterpart.
// TestReconciler_NewGrantPairIsNotDeferredByDebounce pins that the debounce
// rate-limits REDUNDANT passes, never a pass carrying new desired state.
//
// The debounce collapses bursts of writes. It was applied to every pass,
// including the first one that saw a new grant pair — and because `lastWrite`
// advances on any completed pass, a pair created moments after an unrelated
// compose (the bootstrap tick that fires when no AgentSessionGrants exist yet)
// waited a full debounce interval before reaching the schema.
//
// That window is not cosmetic. Everything downstream already believes the
// relation exists: the AgentClass reports Valid, the tool is gated, and
// approving it writes agentsession:<sess>#grant_<perm>_<resType>. Inside the
// window SpiceDB rejects that write with FailedPrecondition, the paused
// dispatch never resumes, and the caller is eventually told the approval
// expired with nobody acting on it — while the approver did act.
//
// Runs with the PRODUCTION debounce deliberately (no SetDebounceForTest): the
// whole point is that the real interval must not gate new state.
func TestReconciler_NewGrantPairIsNotDeferredByDebounce(t *testing.T) {
	ctx := context.Background()
	scheme := testfixtures.NewScheme(t)

	mcps := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "github", Version: "1.0.0",
			Server:        spiceboxv1alpha1.MCPServerServer{URL: "https://example/mcp"},
			Tools:         []spiceboxv1alpha1.MCPServerTool{{Name: "noop"}},
			SpiceDBSchema: githubRepoFragment(),
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mcps).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSessionGrants{}).
		Build()

	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(c, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares

	// Pass 1 — the bootstrap tick: no AgentSessionGrants exist yet, so this
	// composes the scaffold + MCPServer fragments alone and stamps lastWrite.
	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "__bootstrap_tick__"},
	})
	require.NoError(t, err, "Reconcile (bootstrap tick)")
	require.Equal(t, 1, io.writeCount(), "bootstrap tick composes the fragment-only schema")
	require.NotContains(t, io.lastWrite(), "grant_read_github_repo",
		"precondition: no grant pair exists yet")

	// The AgentClass writes its grants — immediately after, i.e. inside the
	// debounce window opened by the tick above.
	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-debounce", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{
				{ResourceType: "github_repo", Permission: "read"},
			},
		},
	}
	require.NoError(t, c.Create(ctx, asg), "create AgentSessionGrants")

	// Pass 2 — carries new desired state, so it must compose NOW.
	_, err = r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "cls-debounce", Namespace: "default"},
	})
	require.NoError(t, err, "Reconcile (new pair)")

	assert.Contains(t, io.lastWrite(), "relation grant_read_github_repo",
		"a newly-declared grant pair must reach SpiceDB on the pass that first sees it; deferring it leaves approvals failing FailedPrecondition for a full debounce interval")

	// And the debounce still does its job: a third pass adds nothing new, so
	// it must not produce another write.
	writesAfterPair := io.writeCount()
	_, err = r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "cls-debounce", Namespace: "default"},
	})
	require.NoError(t, err, "Reconcile (redundant)")
	assert.Equal(t, writesAfterPair, io.writeCount(),
		"a pass with unchanged inputs must still be suppressed — the fix must not turn the debounce off")
}

func TestReconciler_RunAllAppliesMCPServerFragments(t *testing.T) {
	ctx := context.Background()

	scheme := testfixtures.NewScheme(t)

	mcps := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "github", Version: "1.0.0",
			Server:        spiceboxv1alpha1.MCPServerServer{URL: "https://example/mcp"},
			Tools:         []spiceboxv1alpha1.MCPServerTool{{Name: "noop"}},
			SpiceDBSchema: githubRepoFragment(),
		},
	}
	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-runall", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{
				{ResourceType: "github_repo", Permission: "read"},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mcps, asg).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSessionGrants{}).
		Build()

	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(c, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "cls-runall", Namespace: "default"},
	})
	require.NoError(t, err, "Reconcile")

	require.Equal(t, 1, io.writeCount(), "WriteSchema call count")

	written := io.lastWrite()
	// The unified schema MUST contain BOTH the MCPServer's fragment
	// resource definition AND the composed grant relation/permission.
	// This is the slice-4 acceptance criterion the T17 wiring delivers.
	for _, want := range []string{
		"definition github_repo {",         // from MCPServer fragment
		"relation reader: user",            //   - fragment relation
		"permission read = reader + admin", //   - fragment permission
		"definition agentsession {",        // scaffold
		"relation grant_read_github_repo: github_repo with check_hash and expiration", // composed grant
		"permission check_read_github_repo = grant_read_github_repo->read",            // composed check
	} {
		assert.Containsf(t, written, want, "written schema missing %q. Full schema:\n%s", want, written)
	}

	// Status: AllPairsResolved.
	var got spiceboxv1alpha1.AgentSessionGrants
	require.NoError(t,
		c.Get(ctx, client.ObjectKey{Name: "cls-runall", Namespace: "default"}, &got),
		"Get AgentSessionGrants")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded)
	require.NotNil(t, cond, "SchemaIncluded condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "SchemaIncluded status")
	assert.Equal(t, "AllPairsResolved", cond.Reason, "SchemaIncluded reason")
}

// TestReconciler_SteadyStateReconcilePreservesObservedSchemaWrittenAt pins
// the reconcile-storm fix: patchSchemaIncluded must NOT restamp
// ObservedSchemaWrittenAt on every included pass — only on a
// False/absent→True transition. Before the fix, a fresh metav1.Now() landed
// in status on EVERY included reconcile, so the status Patch was never a
// no-op: the watch picked the mutation back up and requeued, producing a
// ~5s-forever reconcile storm (debounce window) doing live SpiceDB reads +
// cluster-wide Lists at steady state.
//
// Uses testenv (a real envtest apiserver), not the controller-runtime fake
// client: the resourceVersion assertion below depends on the apiserver's
// genuine byte-equality no-op-skip in etcd3 GuaranteedUpdate (an empty
// merge-patch results in NO write and NO resourceVersion bump) — the fake
// client's ObjectTracker does not implement that optimization and bumps
// resourceVersion on every accepted Patch call unconditionally, which would
// make this assertion meaningless (or spuriously fail) against the fake
// client.
func TestReconciler_SteadyStateReconcilePreservesObservedSchemaWrittenAt(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	mcps := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "github", Version: "1.0.0",
			Server:        spiceboxv1alpha1.MCPServerServer{URL: "https://example/mcp"},
			Tools:         []spiceboxv1alpha1.MCPServerTool{{Name: "noop"}},
			SpiceDBSchema: githubRepoFragment(),
		},
	}
	require.NoError(t, env.Client.Create(ctx, mcps), "create MCPServer")

	frag := githubRepoFragment()
	pairs := []guardianschema.GrantPair{
		{ResourceType: "github_repo", Permission: "read"},
	}
	// Seed cur with the exact text ComposeAll would produce so the SECOND
	// reconcile's RunAll fast-path (cur == desired) skips the write —
	// isolating the steady-state patchSchemaIncluded behavior from the
	// schema-write path (same technique as TestControllerSkipsWriteWhenNoChange).
	io := &fakeSchemaIO{cur: mustComposeWithFragment(t, frag, pairs)}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares
	// Both passes must reach the write+patch path; the 5s production
	// debounce would otherwise short-circuit the second call before it
	// ever gets to patchSchemaIncluded, hiding exactly the steady-state
	// behavior this test exists to pin.
	guardian.SetDebounceForTest(r, 0)

	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-steady", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{
				{ResourceType: "github_repo", Permission: "read"},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, asg), "create AgentSessionGrants")

	reconcileKey(t, r, "cls-steady", "default")
	require.Equal(t, 0, io.writeCount(), "cur already matches desired; WriteSchema should not be called on the first pass either")

	var first spiceboxv1alpha1.AgentSessionGrants
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "cls-steady", Namespace: "default"}, &first),
		"Get after first Reconcile")
	cond1 := findCondition(first.Status.Conditions, spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded)
	require.NotNil(t, cond1, "SchemaIncluded condition after first Reconcile")
	require.Equal(t, metav1.ConditionTrue, cond1.Status, "SchemaIncluded status after first Reconcile")
	require.NotNil(t, first.Status.ObservedSchemaWrittenAt, "ObservedSchemaWrittenAt should be set on the newly-included pass")
	writtenAt1 := *first.Status.ObservedSchemaWrittenAt
	rv1 := first.ResourceVersion

	// metav1.Time marshals with 1-second (RFC3339) precision, so two
	// metav1.Now() calls in the same wall-clock second are indistinguishable
	// on the wire — without this sleep, a reintroduced "restamp every pass"
	// bug could produce byte-identical timestamps purely by timing luck and
	// this test would pass for the wrong reason.
	time.Sleep(1100 * time.Millisecond)

	// Second pass: pairs/fragments unchanged, already SchemaIncluded=True —
	// the steady state the reconcile-storm fix targets.
	reconcileKey(t, r, "cls-steady", "default")
	assert.Equal(t, 0, io.writeCount(), "steady-state pass should not call WriteSchema")

	var second spiceboxv1alpha1.AgentSessionGrants
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "cls-steady", Namespace: "default"}, &second),
		"Get after second Reconcile")
	cond2 := findCondition(second.Status.Conditions, spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded)
	require.NotNil(t, cond2, "SchemaIncluded condition after second Reconcile")
	assert.Equal(t, metav1.ConditionTrue, cond2.Status, "SchemaIncluded status after second Reconcile")
	require.NotNil(t, second.Status.ObservedSchemaWrittenAt, "ObservedSchemaWrittenAt should still be set")
	assert.True(t, writtenAt1.Equal(second.Status.ObservedSchemaWrittenAt),
		"ObservedSchemaWrittenAt must NOT be restamped on a steady-state (already-True, unchanged) pass: first=%s second=%s",
		writtenAt1, second.Status.ObservedSchemaWrittenAt)

	// A byte-identical status (same conditions, same ObservedPairCount, same
	// ObservedSchemaWrittenAt) means MergeFrom computes an empty patch;
	// against a real apiserver that is a genuine etcd no-op, so
	// resourceVersion must also be stable — this is the actual mechanism
	// that stops the watch→requeue storm.
	assert.Equal(t, rv1, second.ResourceVersion,
		"a steady-state status Patch with no field changes should not bump resourceVersion")
}

// TestReconciler_RunAllSkipsPairWithoutFragment verifies the slice-4
// skip semantics: if no MCPServer declares the resource referenced by
// an AgentSessionGrants pair, the pair is dropped from the composed
// schema and the CR status reflects SomePairsSkipped.
func TestReconciler_RunAllSkipsPairWithoutFragment(t *testing.T) {
	ctx := context.Background()

	scheme := testfixtures.NewScheme(t)

	// No MCPServer in the cluster — yet the ASG references github_repo.
	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-skip", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{
				{ResourceType: "github_repo", Permission: "read"},
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(asg).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSessionGrants{}).
		Build()

	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(c, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "cls-skip", Namespace: "default"},
	})
	require.NoError(t, err, "Reconcile")

	written := io.lastWrite()
	assert.NotContains(t, written, "grant_read_github_repo",
		"written schema should NOT contain grant_read_github_repo (no fragment declares github_repo)")

	var got spiceboxv1alpha1.AgentSessionGrants
	require.NoError(t,
		c.Get(ctx, client.ObjectKey{Name: "cls-skip", Namespace: "default"}, &got),
		"Get AgentSessionGrants")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded)
	require.NotNil(t, cond, "SchemaIncluded condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "SchemaIncluded status")
	assert.Equal(t, "SomePairsSkipped", cond.Reason, "SchemaIncluded reason")
}

// This controller computes no SpiceDB disallow surfaces: hard-deny is enforced
// by the runner's Scope hook, not here.

// TestReconciler_MCPServerWatchEnqueuesAllASGs verifies the MCPServer watch
// mapping: an MCPServer change yields a reconcile request for every
// AgentSessionGrants in the cluster.
func TestReconciler_MCPServerWatchEnqueuesAllASGs(t *testing.T) {
	ctx := context.Background()
	scheme := testfixtures.NewScheme(t)

	asg1 := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "asg1", Namespace: "default"},
	}
	asg2 := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "asg2", Namespace: "other"},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(asg1, asg2).
		Build()

	r := guardian.NewReconciler(c, &fakeSchemaIO{}, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares
	reqs := guardian.MCPServerToAllASGsForTest(r, ctx, &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "any", Namespace: "default"},
	})
	require.Len(t, reqs, 2, "mapper should produce one reconcile request per ASG")
	seen := map[string]bool{}
	for _, r := range reqs {
		seen[r.Namespace+"/"+r.Name] = true
	}
	for _, want := range []string{"default/asg1", "other/asg2"} {
		assert.Truef(t, seen[want], "mapper missing reconcile request for %s; got=%+v", want, reqs)
	}
}

// badAgentSessionFragment is a MCPServer.spec.spiceDBSchema fragment that
// redeclares the reserved `agentsession` scaffold definition — one of the
// three failure modes guardianschema.ValidateFragment rejects (syntax
// error / cross-MCPServer name conflict / reserved-definition
// redeclare). Used to simulate a single bad tenant MCPServer.
func badAgentSessionFragment() *spiceboxv1alpha1.SpiceDBSchemaFragment {
	return &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{{
			Standing:    spiceboxv1alpha1.StandingSessionOnly,
			Name:        "agentsession",
			Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "evil", Expr: "nil"}},
		}},
	}
}

// TestReconciler_IsolatesInvalidMCPServerFragment is the GREEN
// (post-fix) counterpart to schema.TestRunAll_OneBadFragmentFreezesWholeCompose
// (pkg/authz/guardian/schema/composer_test.go), which RED-confirms that feeding
// RunAll a bad fragment alongside a good one fails the WHOLE compose pass.
// Here, two MCPServers contribute fragments — MCP-A valid (github_repo),
// MCP-B invalid (redeclares agentsession) — plus one AgentSessionGrants
// referencing MCP-A's resource. The fix must:
//
//   - write MCP-A's resource into the composed schema (good tenant's
//     schema is NOT held hostage by the bad one),
//   - flip the ASG to SchemaIncluded=True (not frozen cluster-wide),
//   - mark MCP-B (and ONLY MCP-B) with the guardian-owned
//     SpiceDBSchemaValid=False/FragmentInvalid condition.
func TestReconciler_IsolatesInvalidMCPServerFragment(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares

	mcpA := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-a-good", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "mcp-a-good", Version: "1.0.0",
			Server:        spiceboxv1alpha1.MCPServerServer{URL: "https://example/mcp-a"},
			Tools:         []spiceboxv1alpha1.MCPServerTool{{Name: "noop"}},
			SpiceDBSchema: githubRepoFragment(),
		},
	}
	require.NoError(t, env.Client.Create(ctx, mcpA), "create MCP-A (good)")

	mcpB := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-b-bad", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "mcp-b-bad", Version: "1.0.0",
			Server:        spiceboxv1alpha1.MCPServerServer{URL: "https://example/mcp-b"},
			Tools:         []spiceboxv1alpha1.MCPServerTool{{Name: "noop"}},
			SpiceDBSchema: badAgentSessionFragment(),
		},
	}
	require.NoError(t, env.Client.Create(ctx, mcpB), "create MCP-B (bad: redeclares agentsession)")

	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-isolate", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{
				{ResourceType: "github_repo", Permission: "read"},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, asg), "create AgentSessionGrants")

	reconcileKey(t, r, "cls-isolate", "default")

	// (a) The written schema (what a live SpiceDB ReadSchema would
	// return) contains MCP-A's resource — the good tenant's fragment was
	// composed and written despite MCP-B's fragment being invalid.
	written := io.lastWrite()
	assert.Contains(t, written, "definition github_repo {",
		"good MCPServer's fragment must be written even though another MCPServer's fragment is invalid")
	assert.NotContains(t, written, "permission evil",
		"the bad fragment must NOT be composed into the written schema")

	// (b) The ASG is NOT frozen: SchemaIncluded=True.
	var gotASG spiceboxv1alpha1.AgentSessionGrants
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "cls-isolate", Namespace: "default"}, &gotASG),
		"Get AgentSessionGrants")
	cond := findCondition(gotASG.Status.Conditions, spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded)
	require.NotNil(t, cond, "SchemaIncluded condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"a bad MCPServer fragment elsewhere in the cluster must NOT freeze this ASG's SchemaIncluded; reason=%s msg=%s", cond.Reason, cond.Message)

	// (c) MCP-B carries the new guardian-owned SpiceDBSchemaValid=False
	// condition naming the failure.
	var gotB spiceboxv1alpha1.MCPServer
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "mcp-b-bad", Namespace: "default"}, &gotB),
		"Get MCP-B")
	condB := findCondition(gotB.Status.Conditions, spiceboxv1alpha1.MCPServerConditionSpiceDBSchemaValid)
	require.NotNil(t, condB, "MCP-B should carry SpiceDBSchemaValid; got conditions=%+v", gotB.Status.Conditions)
	assert.Equal(t, metav1.ConditionFalse, condB.Status, "MCP-B SpiceDBSchemaValid status")
	assert.Equal(t, spiceboxv1alpha1.MCPServerReasonFragmentInvalid, condB.Reason, "MCP-B SpiceDBSchemaValid reason")
	assert.Contains(t, condB.Message, "agentsession", "MCP-B SpiceDBSchemaValid message names the offending definition")

	// (d) MCP-A does NOT carry SpiceDBSchemaValid at all — a fragment
	// that has always been valid is left untouched, not stamped True.
	var gotA spiceboxv1alpha1.MCPServer
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "mcp-a-good", Namespace: "default"}, &gotA),
		"Get MCP-A")
	condA := findCondition(gotA.Status.Conditions, spiceboxv1alpha1.MCPServerConditionSpiceDBSchemaValid)
	assert.Nil(t, condA, "MCP-A (always valid) should not carry SpiceDBSchemaValid; got=%+v", condA)
}

// conflictingResourceFragment builds a fragment declaring a single
// resource `name` with one `reader`-style relation + permission whose
// names are parameterized so two fragments can share a resource NAME but
// differ in BODY — the cross-MCPServer conflict EmitSpicedbSchema rejects.
func conflictingResourceFragment(name, rel, perm string) *spiceboxv1alpha1.SpiceDBSchemaFragment {
	return &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{{
			Standing:    spiceboxv1alpha1.StandingSessionOnly,
			Name:        name,
			Relations:   []spiceboxv1alpha1.SpiceDBRelation{{Name: rel, SubjectType: "user"}},
			Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: perm, Expr: rel}},
		}},
	}
}

// TestReconciler_IsolatesConflictingMCPServerFragments is the N-way
// (Stage-2) counterpart to TestReconciler_IsolatesInvalidMCPServerFragment
// (Stage-1). THREE MCPServers each contribute a fragment that is valid ON
// ITS OWN, but two of them collide:
//
//   - mcp-a: resource `shared_res` (reader/read) — valid, sorts first.
//   - mcp-b: resource `shared_res` (writer/write) — valid alone, but
//     conflicts with mcp-a's `shared_res` (same name, different body).
//   - mcp-c: resource `other_res` (reader/read) — valid, unrelated.
//
// Fed unfiltered into RunAll (the pre-partition behavior), mcp-b's
// conflict fails compose for the WHOLE batch → every ASG frozen. The
// N-way incremental isolation must instead:
//
//   - write mcp-a's AND mcp-c's resources (maximal conflict-free subset),
//   - keep the ASG SchemaIncluded=True (NOT frozen),
//   - mark ONLY mcp-b SpiceDBSchemaValid=False/FragmentConflict (it sorts
//     after mcp-a, so mcp-a wins the tie),
//   - leave mcp-a and mcp-c unmarked.
func TestReconciler_IsolatesConflictingMCPServerFragments(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares

	newMCP := func(name string, frag *spiceboxv1alpha1.SpiceDBSchemaFragment) *spiceboxv1alpha1.MCPServer {
		return &spiceboxv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: spiceboxv1alpha1.MCPServerSpec{
				Name: name, Version: "1.0.0",
				Server:        spiceboxv1alpha1.MCPServerServer{URL: "https://example/" + name},
				Tools:         []spiceboxv1alpha1.MCPServerTool{{Name: "noop"}},
				SpiceDBSchema: frag,
			},
		}
	}

	mcpA := newMCP("mcp-a", conflictingResourceFragment("shared_res", "reader", "read"))
	mcpB := newMCP("mcp-b", conflictingResourceFragment("shared_res", "writer", "write")) // conflicts with A
	mcpC := newMCP("mcp-c", conflictingResourceFragment("other_res", "reader", "read"))
	require.NoError(t, env.Client.Create(ctx, mcpA), "create mcp-a")
	require.NoError(t, env.Client.Create(ctx, mcpB), "create mcp-b (conflicts with a)")
	require.NoError(t, env.Client.Create(ctx, mcpC), "create mcp-c")

	// The ASG references a pair that resolves against mcp-a's accepted
	// resource, so a non-frozen outcome is AllPairsResolved=True.
	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-conflict", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{
				{ResourceType: "shared_res", Permission: "read"},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, asg), "create AgentSessionGrants")

	reconcileKey(t, r, "cls-conflict", "default")

	// (a) The written schema contains mcp-a's resource (accepted, wins the
	// tie) AND mcp-c's unrelated resource — but NOT mcp-b's conflicting
	// body (its `write` permission on shared_res).
	written := io.lastWrite()
	assert.Contains(t, written, "definition shared_res {", "mcp-a's resource must be written")
	assert.Contains(t, written, "permission read = reader", "mcp-a's body wins the shared_res conflict")
	assert.Contains(t, written, "definition other_res {", "mcp-c's unrelated resource must be written")
	assert.NotContains(t, written, "permission write = writer", "mcp-b's conflicting body must NOT be written")

	// (b) The ASG is NOT frozen.
	var gotASG spiceboxv1alpha1.AgentSessionGrants
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "cls-conflict", Namespace: "default"}, &gotASG),
		"Get AgentSessionGrants")
	cond := findCondition(gotASG.Status.Conditions, spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded)
	require.NotNil(t, cond, "SchemaIncluded condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"a cross-MCPServer conflict must NOT freeze the ASG; reason=%s msg=%s", cond.Reason, cond.Message)

	// (c) ONLY mcp-b is marked, reason FragmentConflict.
	var gotB spiceboxv1alpha1.MCPServer
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "mcp-b", Namespace: "default"}, &gotB), "Get mcp-b")
	condB := findCondition(gotB.Status.Conditions, spiceboxv1alpha1.MCPServerConditionSpiceDBSchemaValid)
	require.NotNil(t, condB, "mcp-b should carry SpiceDBSchemaValid; got=%+v", gotB.Status.Conditions)
	assert.Equal(t, metav1.ConditionFalse, condB.Status, "mcp-b SpiceDBSchemaValid status")
	assert.Equal(t, spiceboxv1alpha1.MCPServerReasonFragmentConflict, condB.Reason, "mcp-b reason")
	assert.Contains(t, condB.Message, "conflicts with an already-accepted", "mcp-b message describes the conflict")

	// (d) mcp-a and mcp-c (the accepted survivors) carry NO condition.
	for _, name := range []string{"mcp-a", "mcp-c"} {
		var got spiceboxv1alpha1.MCPServer
		require.NoError(t,
			env.Client.Get(ctx, client.ObjectKey{Name: name, Namespace: "default"}, &got), "Get "+name)
		c := findCondition(got.Status.Conditions, spiceboxv1alpha1.MCPServerConditionSpiceDBSchemaValid)
		assert.Nilf(t, c, "%s (accepted) should not carry SpiceDBSchemaValid; got=%+v", name, c)
	}
}

// A tenant fragment may legitimately reference a definition a BUILT-IN TOOLKIT
// contributes. The partition trial-composes each candidate against a baseline,
// and if the built-ins are not in that baseline the reference dangles, the
// candidate is rejected, and an MCPServer that would have worked fine is
// conditioned as broken.
func TestReconciler_PartitionBaselineIncludesBuiltinToolkitDefinitions(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit {
		return []toolkit.Toolkit{{
			Name: "crate-svc",
			SpiceDBSchema: &toolkit.SpiceDBSchemaFragment{
				RawZed: "definition crate_shelf {\n\trelation keeper: user\n\tpermission browse = keeper\n}\n",
			},
		}}
	}

	// Its `source` relation points at the definition the built-in contributes.
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "widget-svc", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "widget-svc", Version: "1.0.0",
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://example/widget-svc"},
			Tools:  []spiceboxv1alpha1.MCPServerTool{{Name: "noop"}},
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				RawZed: "definition widget_crate {\n\trelation source: crate_shelf\n}\n",
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, mcp), "create widget-svc")

	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-baseline", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{{ResourceType: "crate_shelf", Permission: "browse"}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, asg), "create AgentSessionGrants")

	reconcileKey(t, r, "cls-baseline", "default")

	// The precise claim: it must not be isolated AS A FRAGMENT CONFLICT.
	var got spiceboxv1alpha1.MCPServer
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "widget-svc", Namespace: "default"}, &got),
		"Get widget-svc")
	if cond := findCondition(got.Status.Conditions,
		spiceboxv1alpha1.MCPServerConditionSpiceDBSchemaValid); cond != nil {
		assert.NotEqual(t, spiceboxv1alpha1.MCPServerReasonFragmentConflict, cond.Reason,
			"a fragment referencing a built-in toolkit's definition must not be isolated; msg=%s",
			cond.Message)
	}
	assert.Contains(t, io.lastWrite(), "definition widget_crate",
		"and it must actually reach the written schema")
}

// The composition wiring, which the conversion test in pkg/guardian/schema
// cannot reach: ToolkitFragments being correct says nothing about whether the
// reconciler actually feeds it into the schema it writes.
//
// This is the gap that let a regression through. Toolkits ship the SpiceDB types
// their own permission checks name — `gh pr view` gates on github_repo#read, and
// before this a cluster got `object definition "github_repo" not found` on every
// gated call — but the only tests covering it were the pure conversion and a
// bronze bundle three layers up. A synthetic toolkit keeps the assertion off the
// contents of toolkits/*.yaml, so editing a shipped toolkit cannot break it.
func TestReconciler_ComposesBuiltinToolkitSchemaFragments(t *testing.T) {
	env := testenv.Shared(t)
	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit {
		return []toolkit.Toolkit{{
			Name: "demokit",
			SpiceDBSchema: &toolkit.SpiceDBSchemaFragment{
				Resources: []toolkit.SpiceDBResource{{
					Name:        "demo_repo",
					Relations:   []toolkit.SpiceDBRelation{{Name: "owner", SubjectType: "user"}},
					Permissions: []toolkit.SpiceDBPermission{{Name: "fetch", Expr: "owner"}},
				}},
			},
		}}
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)

	got := io.lastWrite()
	require.NotEmpty(t, got, "the toolkit's definitions are new schema; a write must happen")
	assert.Contains(t, got, "definition demo_repo",
		"the toolkit's resource must reach the composed schema")
	assert.Contains(t, got, "fetch",
		"and its permission with it — a definition without the permission the "+
			"check names fails exactly the same way as no definition at all")
}

// TestReconciler_ComposesSidecarToolboxSchemaFragments pins that a
// SidecarToolbox's spicedbSchema fragment is gathered into the unified schema
// the guardian writes — the same treatment MCPServer/Bootstrap fragments get —
// so a declared sidecar tool's per-datum audiences resolve.
func TestReconciler_ComposesSidecarToolboxSchemaFragments(t *testing.T) {
	ctx := context.Background()
	scheme := testfixtures.NewScheme(t)

	tb := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "records", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name: "records", Version: "1",
			Tools: []spiceboxv1alpha1.MCPServerTool{{Name: "read_record"}},
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{{
					Standing:    spiceboxv1alpha1.StandingSessionOnly,
					Name:        "pde_record",
					Relations:   []spiceboxv1alpha1.SpiceDBRelation{{Name: "viewer", SubjectType: "user"}},
					Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "view", Expr: "viewer"}},
				}},
			},
		},
	}
	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-sidecar", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{{ResourceType: "pde_record", Permission: "view"}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(tb, asg).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSessionGrants{}).
		Build()

	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(c, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil }

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "cls-sidecar", Namespace: "default"},
	})
	require.NoError(t, err, "Reconcile")
	require.Equal(t, 1, io.writeCount(), "WriteSchema call count")

	written := io.lastWrite()
	for _, want := range []string{
		"definition pde_record {", // the SidecarToolbox fragment's resource
		"relation viewer: user",
		"permission view = viewer",
	} {
		assert.Containsf(t, written, want, "written schema missing %q from the SidecarToolbox fragment. Full schema:\n%s", want, written)
	}
}

// TestReconciler_IsolatesInvalidSidecarToolboxFragment is the SidecarToolbox
// mirror of TestReconciler_IsolatesInvalidMCPServerFragment: a bad sidecar
// fragment is excluded (the good one still composes) AND the offending
// SidecarToolbox carries SpiceDBSchemaValid=False — not just a log line.
func TestReconciler_IsolatesInvalidSidecarToolboxFragment(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil }

	good := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "tb-good", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name: "tb-good", Version: "1",
			Tools:         []spiceboxv1alpha1.MCPServerTool{{Name: "noop"}},
			SpiceDBSchema: githubRepoFragment(),
		},
	}
	require.NoError(t, env.Client.Create(ctx, good), "create tb-good")

	bad := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "tb-bad", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name: "tb-bad", Version: "1",
			Tools:         []spiceboxv1alpha1.MCPServerTool{{Name: "noop"}},
			SpiceDBSchema: badAgentSessionFragment(), // redeclares the reserved agentsession
		},
	}
	require.NoError(t, env.Client.Create(ctx, bad), "create tb-bad")

	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-tb-isolate", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{{ResourceType: "github_repo", Permission: "read"}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, asg), "create ASG")

	reconcileKey(t, r, "cls-tb-isolate", "default")

	// (a) The good toolbox's fragment composed despite the bad one.
	written := io.lastWrite()
	assert.Contains(t, written, "definition github_repo {", "good SidecarToolbox fragment must compose")
	assert.NotContains(t, written, "permission evil", "the bad fragment must not compose")

	// (b) The bad toolbox carries SpiceDBSchemaValid=False.
	var gotBad spiceboxv1alpha1.SidecarToolbox
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Name: "tb-bad", Namespace: "default"}, &gotBad), "Get tb-bad")
	cond := findCondition(gotBad.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionSpiceDBSchemaValid)
	require.NotNil(t, cond, "tb-bad must carry SpiceDBSchemaValid; got %+v", gotBad.Status.Conditions)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "tb-bad SpiceDBSchemaValid status")
	assert.Equal(t, spiceboxv1alpha1.SidecarToolboxReasonFragmentInvalid, cond.Reason)
}

// minimalSpiceboxToolkitSpec returns the smallest SpiceboxToolkitSpec that
// satisfies the CRD's required-field validation under the real (envtest)
// apiserver these Reconcile-level tests run against, carrying frag as
// spec.spicedbSchema. This package needs its own copy of the shape
// pkg/controllers/spiceboxtoolkit/controller_test.go's minimalSubcommand +
// fixture builds, since that one is unexported in a different package.
func minimalSpiceboxToolkitSpec(name string, frag *spiceboxv1alpha1.SpiceDBSchemaFragment) spiceboxv1alpha1.SpiceboxToolkitSpec {
	return spiceboxv1alpha1.SpiceboxToolkitSpec{
		Name: name, Version: "1", ToolkitRevision: "v1",
		Target: spiceboxv1alpha1.ToolkitTarget{Binary: "/usr/bin/" + name},
		Parser: spiceboxv1alpha1.ToolkitParserConfig{Kind: "declarative"},
		Env:    spiceboxv1alpha1.ToolkitEnv{Allowed: []spiceboxv1alpha1.ToolkitEnvVar{}},
		Subcommands: []spiceboxv1alpha1.ToolkitSubcommand{{
			Path: []string{},
			Effects: spiceboxv1alpha1.ToolkitEffects{
				Reads:      []string{},
				Writes:     []string{},
				Network:    spiceboxv1alpha1.ToolkitNetworkEffect{Destinations: []string{}},
				Filesystem: spiceboxv1alpha1.ToolkitFsEffect{Paths: []string{}},
				Creds:      spiceboxv1alpha1.ToolkitCredsEffect{Required: []string{}, Writes: []string{}},
			},
		}},
		SpiceDBSchema: frag,
	}
}

// TestReconciler_IsolatesInvalidSpiceboxToolkitFragment is the SpiceboxToolkit
// mirror of TestReconciler_IsolatesInvalidMCPServerFragment /
// TestReconciler_IsolatesInvalidSidecarToolboxFragment: a bad SpiceboxToolkit
// fragment is excluded (the good one still composes) AND the offending
// SpiceboxToolkit CR carries SpiceDBSchemaValid=False — not just a log line.
// This is the test the review found missing: driving a real Reconcile() is
// what can actually fail if the "spiceboxtoolkit:" key routing, the
// toolkitByKey lookup, or the SpiceboxToolkitList List call itself were
// wrong or removed — the prior unit test only exercised the patch helper
// directly and could not catch any of those.
//
// SpiceboxToolkit is cluster-scoped (+kubebuilder:resource:scope=Cluster),
// so its fixtures carry no namespace.
func TestReconciler_IsolatesInvalidSpiceboxToolkitFragment(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil }

	good := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "tk-good"},
		Spec:       minimalSpiceboxToolkitSpec("tk-good", githubRepoFragment()),
	}
	require.NoError(t, env.Client.Create(ctx, good), "create tk-good")

	bad := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "tk-bad"},
		Spec:       minimalSpiceboxToolkitSpec("tk-bad", badAgentSessionFragment()), // redeclares the reserved agentsession
	}
	require.NoError(t, env.Client.Create(ctx, bad), "create tk-bad")

	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-tk-isolate", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{{ResourceType: "github_repo", Permission: "read"}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, asg), "create ASG")

	reconcileKey(t, r, "cls-tk-isolate", "default")

	// (a) The good toolkit's fragment composed despite the bad one.
	written := io.lastWrite()
	assert.Contains(t, written, "definition github_repo {", "good SpiceboxToolkit fragment must compose")
	assert.NotContains(t, written, "permission evil", "the bad fragment must not compose")

	// (b) The bad toolkit carries SpiceDBSchemaValid=False.
	var gotBad spiceboxv1alpha1.SpiceboxToolkit
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Name: "tk-bad"}, &gotBad), "Get tk-bad")
	cond := findCondition(gotBad.Status.Conditions, spiceboxv1alpha1.SpiceboxToolkitConditionSpiceDBSchemaValid)
	require.NotNil(t, cond, "tk-bad must carry SpiceDBSchemaValid; got %+v", gotBad.Status.Conditions)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "tk-bad SpiceDBSchemaValid status")
	assert.Equal(t, spiceboxv1alpha1.SpiceboxToolkitReasonFragmentInvalid, cond.Reason)

	// (c) The good toolkit carries NO condition — a fragment that has
	// always been valid is left untouched, not stamped True.
	var gotGood spiceboxv1alpha1.SpiceboxToolkit
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Name: "tk-good"}, &gotGood), "Get tk-good")
	condGood := findCondition(gotGood.Status.Conditions, spiceboxv1alpha1.SpiceboxToolkitConditionSpiceDBSchemaValid)
	assert.Nil(t, condGood, "tk-good (always valid) should not carry SpiceDBSchemaValid; got=%+v", condGood)
}

// TestReconciler_IsolatesConflictingSpiceboxToolkitFragments is the N-way
// (Stage-2) counterpart, mirroring
// TestReconciler_IsolatesConflictingMCPServerFragments: two SpiceboxToolkits
// each contribute a fragment that is valid ON ITS OWN, but they collide with
// each other (same resource name, different body).
//
//   - tk-a: resource `shared_tk_res` (reader/read) — valid, sorts first
//     ("spiceboxtoolkit:tk-a" < "spiceboxtoolkit:tk-b").
//   - tk-b: resource `shared_tk_res` (writer/write) — valid alone, but
//     conflicts with tk-a's `shared_tk_res` (same name, different body).
//
// First in sort order wins; the LATER one is rejected — never both, since
// rejecting both would let a tenant knock a chosen victim's fragment out of
// the schema just by publishing a colliding one.
func TestReconciler_IsolatesConflictingSpiceboxToolkitFragments(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil }

	tkA := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "tk-a"},
		Spec:       minimalSpiceboxToolkitSpec("tk-a", conflictingResourceFragment("shared_tk_res", "reader", "read")),
	}
	tkB := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "tk-b"},
		Spec:       minimalSpiceboxToolkitSpec("tk-b", conflictingResourceFragment("shared_tk_res", "writer", "write")), // conflicts with tk-a
	}
	require.NoError(t, env.Client.Create(ctx, tkA), "create tk-a")
	require.NoError(t, env.Client.Create(ctx, tkB), "create tk-b (conflicts with tk-a)")

	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-tk-conflict", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{{ResourceType: "shared_tk_res", Permission: "read"}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, asg), "create AgentSessionGrants")

	reconcileKey(t, r, "cls-tk-conflict", "default")

	// (a) The written schema contains tk-a's body (accepted, wins the tie)
	// but NOT tk-b's conflicting body.
	written := io.lastWrite()
	assert.Contains(t, written, "definition shared_tk_res {", "tk-a's resource must be written")
	assert.Contains(t, written, "permission read = reader", "tk-a's body wins the shared_tk_res conflict")
	assert.NotContains(t, written, "permission write = writer", "tk-b's conflicting body must NOT be written")

	// (b) The ASG is NOT frozen.
	var gotASG spiceboxv1alpha1.AgentSessionGrants
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "cls-tk-conflict", Namespace: "default"}, &gotASG),
		"Get AgentSessionGrants")
	cond := findCondition(gotASG.Status.Conditions, spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded)
	require.NotNil(t, cond, "SchemaIncluded condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"a cross-SpiceboxToolkit conflict must NOT freeze the ASG; reason=%s msg=%s", cond.Reason, cond.Message)

	// (c) ONLY tk-b is marked, reason FragmentConflict — exactly one side of
	// the conflict is ever rejected, never both.
	var gotB spiceboxv1alpha1.SpiceboxToolkit
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Name: "tk-b"}, &gotB), "Get tk-b")
	condB := findCondition(gotB.Status.Conditions, spiceboxv1alpha1.SpiceboxToolkitConditionSpiceDBSchemaValid)
	require.NotNil(t, condB, "tk-b should carry SpiceDBSchemaValid; got=%+v", gotB.Status.Conditions)
	assert.Equal(t, metav1.ConditionFalse, condB.Status, "tk-b SpiceDBSchemaValid status")
	assert.Equal(t, spiceboxv1alpha1.SpiceboxToolkitReasonFragmentConflict, condB.Reason, "tk-b reason")
	assert.Contains(t, condB.Message, "conflicts with an already-accepted", "tk-b message describes the conflict")

	// (d) tk-a (the accepted survivor) carries NO condition.
	var gotA spiceboxv1alpha1.SpiceboxToolkit
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Name: "tk-a"}, &gotA), "Get tk-a")
	condA := findCondition(gotA.Status.Conditions, spiceboxv1alpha1.SpiceboxToolkitConditionSpiceDBSchemaValid)
	assert.Nil(t, condA, "tk-a (accepted) should not carry SpiceDBSchemaValid; got=%+v", condA)
}

// TestReconciler_IsolatesInvalidSpiceDBBootstrapFragment is the
// SpiceDBBootstrap mirror of TestReconciler_IsolatesInvalidMCPServerFragment /
// TestReconciler_IsolatesInvalidSidecarToolboxFragment /
// TestReconciler_IsolatesInvalidSpiceboxToolkitFragment: a bootstrap whose
// fragment is invalid ON ITS OWN (redeclares the reserved `agentsession`
// scaffold definition) is excluded from the compose pass, the offending CR
// carries the failure on its OWN status, and a sibling good bootstrap still
// composes. A unit test that calls guardianschema.PartitionCompatibleFragments
// directly (bootstrap_fragment_isolation_test.go) cannot show any of this —
// it says nothing about whether the controller ever routes a
// SpiceDBBootstrap fragment into that partition in the first place, which is
// this task's entire deliverable. Only a real Reconcile() can fail if the
// "spicedbbootstrap:" key routing, the bootstrapByKey lookup, or the
// SpiceDBBootstrapList List call itself is wrong, missing, or reordered.
//
// Unlike the MCPServer/SidecarToolbox/SpiceboxToolkit siblings,
// SpiceDBBootstrap gains NO new condition — an accepted bootstrap ends up
// SchemaIncluded=True/FragmentLanded via patchBootstrapStatus, a behavior
// that predates this fix (see TestBootstrap_SchemaFragmentLands in
// spicedbbootstrap_controller_test.go). So unlike the siblings' "survivor
// carries no condition at all" assertion, the survivor's SchemaIncluded=True
// is asserted explicitly below.
func TestReconciler_IsolatesInvalidSpiceDBBootstrapFragment(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares

	good := &spiceboxv1alpha1.SpiceDBBootstrap{
		ObjectMeta: metav1.ObjectMeta{Name: "boot-good", Namespace: "default"},
		Spec: spiceboxv1alpha1.SpiceDBBootstrapSpec{
			SpiceDBSchema: githubRepoFragment(),
		},
	}
	require.NoError(t, env.Client.Create(ctx, good), "create boot-good")

	bad := &spiceboxv1alpha1.SpiceDBBootstrap{
		ObjectMeta: metav1.ObjectMeta{Name: "boot-bad", Namespace: "default"},
		Spec: spiceboxv1alpha1.SpiceDBBootstrapSpec{
			SpiceDBSchema: badAgentSessionFragment(), // redeclares the reserved agentsession
		},
	}
	require.NoError(t, env.Client.Create(ctx, bad), "create boot-bad")

	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-boot-isolate", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{{ResourceType: "github_repo", Permission: "read"}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, asg), "create ASG")

	reconcileKey(t, r, "cls-boot-isolate", "default")

	// (a) The good bootstrap's fragment composed despite the bad one.
	written := io.lastWrite()
	assert.Contains(t, written, "definition github_repo {", "good SpiceDBBootstrap fragment must compose")
	assert.NotContains(t, written, "permission evil", "the bad fragment must not compose")

	// (b) The ASG is NOT frozen: a bad bootstrap elsewhere in the cluster
	// must not freeze cluster-wide schema composition.
	var gotASG spiceboxv1alpha1.AgentSessionGrants
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "cls-boot-isolate", Namespace: "default"}, &gotASG),
		"Get AgentSessionGrants")
	condASG := findCondition(gotASG.Status.Conditions, spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded)
	require.NotNil(t, condASG, "SchemaIncluded condition")
	assert.Equal(t, metav1.ConditionTrue, condASG.Status,
		"a bad SpiceDBBootstrap fragment must NOT freeze this ASG; reason=%s msg=%s", condASG.Reason, condASG.Message)

	// (c) boot-bad carries SchemaIncluded=False/ReasonSpicedbSchemaFragmentInvalid
	// (its fragment is bad ON ITS OWN, not in conflict with another
	// fragment — see ReasonSpicedbSchemaConflict for that case), naming the
	// offending definition — SpiceDBBootstrap reuses its existing
	// SchemaIncluded condition rather than a new parallel one.
	var gotBad spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Name: "boot-bad", Namespace: "default"}, &gotBad), "Get boot-bad")
	condBad := findCondition(gotBad.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionSchemaIncluded)
	require.NotNil(t, condBad, "boot-bad must carry SchemaIncluded; got %+v", gotBad.Status.Conditions)
	assert.Equal(t, metav1.ConditionFalse, condBad.Status, "boot-bad SchemaIncluded status")
	assert.Equal(t, spiceboxv1alpha1.ReasonSpicedbSchemaFragmentInvalid, condBad.Reason, "boot-bad SchemaIncluded reason")
	assert.Contains(t, condBad.Message, "agentsession", "boot-bad SchemaIncluded message names the offending definition")

	// (d) boot-good's fragment landed: SchemaIncluded=True.
	var gotGood spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Name: "boot-good", Namespace: "default"}, &gotGood), "Get boot-good")
	condGood := findCondition(gotGood.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionSchemaIncluded)
	require.NotNil(t, condGood, "boot-good must carry SchemaIncluded; got %+v", gotGood.Status.Conditions)
	assert.Equal(t, metav1.ConditionTrue, condGood.Status, "boot-good SchemaIncluded status")
}

// TestReconciler_ComposesRawZedOnlySpiceDBBootstrapFragment is the GREEN
// (post-fix) counterpart to the pre-fix behavior
// bootstrap_fragment_isolation_test.go used to document: a SpiceDBBootstrap
// whose spec.spicedbSchema carries ONLY RawZed (no structured Resources) is
// a legal fragment — SpiceDBSchemaFragment.RawZed is a real optional field
// on the shared type — but the candidate-gathering loop's emptiness check
// used to be `frag == nil || len(frag.Resources) == 0`, which is blind to
// RawZed and silently dropped the fragment before it ever reached
// ValidateFragment or the partition: no compose, no condition, no log line.
// That is exactly the symptom the design doc's Problem section opens with —
// "their schema silently never took effect." The predicate is now
// RawZed-aware, matching the SidecarToolbox and SpiceboxToolkit loops.
//
// The fixture also declares a (deliberately unrelated) relationship. The
// CRD's own admission-time XValidation rule
// (`spec must declare at least one of spicedbSchema.resources or
// relationships`) checks `spicedbSchema.resources` directly and has no
// RawZed branch, so a spec carrying RawZed but neither Resources nor
// Relationships is rejected by the API SERVER before it ever reaches this
// controller — a real gap, but a CRD/admission one, not the candidate-loop
// bug this test is about. Adding a relationship is what makes
// this fixture representative of a bootstrap the API will actually accept.
func TestReconciler_ComposesRawZedOnlySpiceDBBootstrapFragment(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares

	boot := &spiceboxv1alpha1.SpiceDBBootstrap{
		ObjectMeta: metav1.ObjectMeta{Name: "boot-rawzed-only", Namespace: "default"},
		Spec: spiceboxv1alpha1.SpiceDBBootstrapSpec{
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				RawZed: "definition rawzed_only_res {\n    relation viewer: user\n    permission view = viewer\n}\n",
			},
			Relationships: []spiceboxv1alpha1.SpiceDBBootstrapRelationship{relGroupMember("rawzed-fixture-canon")},
		},
	}
	require.NoError(t, env.Client.Create(ctx, boot), "create boot-rawzed-only")

	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-boot-rawzed", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{{ResourceType: "rawzed_only_res", Permission: "view"}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, asg), "create ASG")

	reconcileKey(t, r, "cls-boot-rawzed", "default")

	// (a) The RawZed-only fragment composed — it must not be silently dropped
	// before ever reaching ValidateFragment/the partition.
	written := io.lastWrite()
	assert.Contains(t, written, "definition rawzed_only_res {", "RawZed-only SpiceDBBootstrap fragment must compose")

	// (b) The ASG resolves against it: SchemaIncluded=True, not skipped.
	var gotASG spiceboxv1alpha1.AgentSessionGrants
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "cls-boot-rawzed", Namespace: "default"}, &gotASG),
		"Get AgentSessionGrants")
	condASG := findCondition(gotASG.Status.Conditions, spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded)
	require.NotNil(t, condASG, "SchemaIncluded condition")
	assert.Equal(t, metav1.ConditionTrue, condASG.Status,
		"a RawZed-only SpiceDBBootstrap fragment must resolve the ASG's pair; reason=%s msg=%s", condASG.Reason, condASG.Message)

	// (c) The bootstrap itself lands SchemaIncluded=True/FragmentLanded —
	// proof it was actually candidate-gathered and validated, not merely that
	// the resource happened to appear from some other source.
	var gotBoot spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Name: "boot-rawzed-only", Namespace: "default"}, &gotBoot), "Get boot-rawzed-only")
	condBoot := findCondition(gotBoot.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionSchemaIncluded)
	require.NotNil(t, condBoot, "boot-rawzed-only must carry SchemaIncluded; got %+v", gotBoot.Status.Conditions)
	assert.Equal(t, metav1.ConditionTrue, condBoot.Status, "boot-rawzed-only SchemaIncluded status")
}

// TestReconciler_IsolatesConflictingSpiceDBBootstrapFragments is the N-way
// (Stage-2) counterpart, mirroring
// TestReconciler_IsolatesConflictingSpiceboxToolkitFragments: two
// SpiceDBBootstraps each contribute a fragment that is valid ON ITS OWN, but
// they collide with each other (same resource name, different body).
//
//   - boot-a: resource `shared_boot_res` (reader/read) — valid, sorts first
//     ("spicedbbootstrap:default/boot-a" < "spicedbbootstrap:default/boot-b").
//   - boot-b: resource `shared_boot_res` (writer/write) — valid alone, but
//     conflicts with boot-a's `shared_boot_res` (same name, different body).
//
// First in sort order wins; the LATER one is rejected — never both. Rejecting
// both would let a tenant knock a chosen victim's fragment out of the schema
// just by publishing a colliding one — including, pre-fix, letting any
// conflicting fragment anywhere in the cluster knock out an operator's own
// good bootstrap by failing the whole union.
func TestReconciler_IsolatesConflictingSpiceDBBootstrapFragments(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil }

	bootA := &spiceboxv1alpha1.SpiceDBBootstrap{
		ObjectMeta: metav1.ObjectMeta{Name: "boot-a", Namespace: "default"},
		Spec: spiceboxv1alpha1.SpiceDBBootstrapSpec{
			SpiceDBSchema: conflictingResourceFragment("shared_boot_res", "reader", "read"),
		},
	}
	bootB := &spiceboxv1alpha1.SpiceDBBootstrap{
		ObjectMeta: metav1.ObjectMeta{Name: "boot-b", Namespace: "default"},
		Spec: spiceboxv1alpha1.SpiceDBBootstrapSpec{
			SpiceDBSchema: conflictingResourceFragment("shared_boot_res", "writer", "write"), // conflicts with boot-a
		},
	}
	require.NoError(t, env.Client.Create(ctx, bootA), "create boot-a")
	require.NoError(t, env.Client.Create(ctx, bootB), "create boot-b (conflicts with boot-a)")

	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-boot-conflict", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{{ResourceType: "shared_boot_res", Permission: "read"}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, asg), "create AgentSessionGrants")

	reconcileKey(t, r, "cls-boot-conflict", "default")

	// (a) The written schema contains boot-a's body (accepted, wins the tie)
	// but NOT boot-b's conflicting body.
	written := io.lastWrite()
	assert.Contains(t, written, "definition shared_boot_res {", "boot-a's resource must be written")
	assert.Contains(t, written, "permission read = reader", "boot-a's body wins the shared_boot_res conflict")
	assert.NotContains(t, written, "permission write = writer", "boot-b's conflicting body must NOT be written")

	// (b) The ASG is NOT frozen.
	var gotASG spiceboxv1alpha1.AgentSessionGrants
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "cls-boot-conflict", Namespace: "default"}, &gotASG),
		"Get AgentSessionGrants")
	condASG := findCondition(gotASG.Status.Conditions, spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded)
	require.NotNil(t, condASG, "SchemaIncluded condition")
	assert.Equal(t, metav1.ConditionTrue, condASG.Status,
		"a cross-SpiceDBBootstrap conflict must NOT freeze the ASG; reason=%s msg=%s", condASG.Reason, condASG.Message)

	// (c) ONLY boot-b is marked, reason ReasonSpicedbSchemaConflict — exactly
	// one side of the conflict is ever rejected, never both. Rejecting both
	// would let a tenant knock a chosen victim (boot-a) out of the schema
	// just by publishing a colliding fragment.
	var gotB spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Name: "boot-b", Namespace: "default"}, &gotB), "Get boot-b")
	condB := findCondition(gotB.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionSchemaIncluded)
	require.NotNil(t, condB, "boot-b should carry SchemaIncluded; got=%+v", gotB.Status.Conditions)
	assert.Equal(t, metav1.ConditionFalse, condB.Status, "boot-b SchemaIncluded status")
	assert.Equal(t, spiceboxv1alpha1.ReasonSpicedbSchemaConflict, condB.Reason, "boot-b reason")
	assert.Contains(t, condB.Message, "conflicts with an already-accepted", "boot-b message describes the conflict")
	// The message also names WHICH fragment displaced boot-b (design §3): the
	// winning side's own key, so an operator does not have to go hunting.
	assert.Contains(t, condB.Message, "spicedbbootstrap:default/boot-a", "boot-b message names the displacing fragment")

	// (d) boot-a (the accepted survivor) carries SchemaIncluded=True — NEVER
	// False. This is the never-reject-both-sides property the partition
	// exists to protect: a hostile or accidental conflict from boot-b must
	// not be able to knock boot-a out too.
	var gotA spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Name: "boot-a", Namespace: "default"}, &gotA), "Get boot-a")
	condA := findCondition(gotA.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionSchemaIncluded)
	require.NotNil(t, condA, "boot-a must carry SchemaIncluded; got %+v", gotA.Status.Conditions)
	assert.Equal(t, metav1.ConditionTrue, condA.Status, "boot-a (accepted) SchemaIncluded status")

	// (e) boot-a's Valid condition (a DIFFERENT check — its own spec shape)
	// must also stay True: the cross-CR conflict is not a property of
	// boot-a's own spec.
	condAValid := findCondition(gotA.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionValid)
	require.NotNil(t, condAValid, "boot-a must carry Valid")
	assert.Equal(t, metav1.ConditionTrue, condAValid.Status, "boot-a Valid status")
}

// TestReconciler_OperatorBootstrapBeatsAnEarlierSortingTenantFragment is the
// Reconcile-level proof for Task 5 (order the partition by trust tier).
// mcp-tenant (an MCPServer, key "default/mcp-tenant") and boot-operator (a
// SpiceDBBootstrap, key "spicedbbootstrap:default/boot-operator") each
// contribute a fragment that is valid ON ITS OWN, but they collide with each
// other (same resource name, different body) — the SAME shape as
// TestReconciler_IsolatesConflictingMCPServerFragments and
// TestReconciler_IsolatesConflictingSpiceDBBootstrapFragments above, except
// the two colliding candidates now sit in DIFFERENT trust tiers.
//
// mcp-tenant's key sorts lexicographically BEFORE boot-operator's key — an
// MCPServer key never carries a "spicedbbootstrap:" prefix, so this ordering
// holds for any MCPServer/SpiceDBBootstrap pair, not just this test's chosen
// names. A key-only sort (the pre-Task-5 behavior) would therefore accept
// mcp-tenant and reject boot-operator every time two such fragments collide —
// letting any agent- or bundle-authored MCPServer silently displace an
// operator-authored SpiceDBBootstrap. Only a real Reconcile() proves the
// controller actually threads guardianschema.FragmentTier through both
// candidate-construction call sites into PartitionCompatibleFragments; a unit
// test that calls PartitionCompatibleFragments directly (as
// partition_test.go's TestPartition_OperatorTierWinsAgainstAnEarlierSortingTenant
// does) cannot show the controller wires Tier correctly at either site.
func TestReconciler_OperatorBootstrapBeatsAnEarlierSortingTenantFragment(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares

	mcpTenant := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-tenant", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "mcp-tenant", Version: "1.0.0",
			Server:        spiceboxv1alpha1.MCPServerServer{URL: "https://example/mcp-tenant"},
			Tools:         []spiceboxv1alpha1.MCPServerTool{{Name: "noop"}},
			SpiceDBSchema: conflictingResourceFragment("shared_tier_res", "writer", "write"),
		},
	}
	bootOperator := &spiceboxv1alpha1.SpiceDBBootstrap{
		ObjectMeta: metav1.ObjectMeta{Name: "boot-operator", Namespace: "default"},
		Spec: spiceboxv1alpha1.SpiceDBBootstrapSpec{
			SpiceDBSchema: conflictingResourceFragment("shared_tier_res", "reader", "read"), // conflicts with mcp-tenant
		},
	}
	require.NoError(t, env.Client.Create(ctx, mcpTenant), "create mcp-tenant")
	require.NoError(t, env.Client.Create(ctx, bootOperator), "create boot-operator (conflicts with mcp-tenant)")
	require.Less(t, "default/mcp-tenant", "spicedbbootstrap:default/boot-operator",
		"mcp-tenant's key must sort BEFORE boot-operator's — this test is only meaningful if a key-only sort would pick the tenant")

	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-tier-conflict", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{{ResourceType: "shared_tier_res", Permission: "read"}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, asg), "create AgentSessionGrants")

	reconcileKey(t, r, "cls-tier-conflict", "default")

	// (a) The written schema contains boot-operator's body — NOT
	// mcp-tenant's — even though mcp-tenant's key sorts first.
	written := io.lastWrite()
	assert.Contains(t, written, "definition shared_tier_res {", "boot-operator's resource must be written")
	assert.Contains(t, written, "permission read = reader", "boot-operator's body must win the tier-ordered conflict")
	assert.NotContains(t, written, "permission write = writer", "mcp-tenant's conflicting body must NOT be written")

	// (b) The ASG is NOT frozen. Its pair (shared_tier_res, read) resolves
	// against the ACCEPTED (boot-operator's reader/read) body, so
	// SchemaIncluded stays True — a cross-tier conflict must not freeze
	// cluster-wide schema composition any more than a same-tier one does.
	var gotASG spiceboxv1alpha1.AgentSessionGrants
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Name: "cls-tier-conflict", Namespace: "default"}, &gotASG),
		"Get AgentSessionGrants")
	condASG := findCondition(gotASG.Status.Conditions, spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded)
	require.NotNil(t, condASG, "SchemaIncluded condition")
	assert.Equal(t, metav1.ConditionTrue, condASG.Status,
		"a cross-tier conflict must NOT freeze the ASG; reason=%s msg=%s", condASG.Reason, condASG.Message)

	// (c) mcp-tenant (the tenant CR, the LOSING side) carries the
	// guardian-owned SpiceDBSchemaValid=False/FragmentConflict condition —
	// the tenant CR carries the conflict condition.
	var gotTenant spiceboxv1alpha1.MCPServer
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Name: "mcp-tenant", Namespace: "default"}, &gotTenant), "Get mcp-tenant")
	condTenant := findCondition(gotTenant.Status.Conditions, spiceboxv1alpha1.MCPServerConditionSpiceDBSchemaValid)
	require.NotNil(t, condTenant, "mcp-tenant should carry SpiceDBSchemaValid; got=%+v", gotTenant.Status.Conditions)
	assert.Equal(t, metav1.ConditionFalse, condTenant.Status, "mcp-tenant SpiceDBSchemaValid status")
	assert.Equal(t, spiceboxv1alpha1.MCPServerReasonFragmentConflict, condTenant.Reason, "mcp-tenant reason")
	assert.Contains(t, condTenant.Message, "conflicts with an already-accepted", "mcp-tenant message describes the conflict")

	// (d) boot-operator (the operator CR, the WINNING side) does NOT carry
	// the conflict condition: Valid stays True (its own spec is fine) and
	// SchemaIncluded stays True/FragmentLanded — the bootstrap CR does not
	// carry the conflict condition, unlike mcp-tenant above.
	var gotOperator spiceboxv1alpha1.SpiceDBBootstrap
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Name: "boot-operator", Namespace: "default"}, &gotOperator), "Get boot-operator")
	condOperatorValid := findCondition(gotOperator.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionValid)
	require.NotNil(t, condOperatorValid, "boot-operator must carry Valid")
	assert.Equal(t, metav1.ConditionTrue, condOperatorValid.Status, "boot-operator Valid status")
	condOperatorSchema := findCondition(gotOperator.Status.Conditions, spiceboxv1alpha1.SpiceDBBootstrapConditionSchemaIncluded)
	require.NotNil(t, condOperatorSchema, "boot-operator must carry SchemaIncluded; got %+v", gotOperator.Status.Conditions)
	assert.Equal(t, metav1.ConditionTrue, condOperatorSchema.Status, "boot-operator (accepted, higher tier) SchemaIncluded status")
	assert.NotEqual(t, spiceboxv1alpha1.ReasonSpicedbSchemaConflict, condOperatorSchema.Reason,
		"boot-operator must NOT carry the conflict reason — it is the WINNING side")
}
