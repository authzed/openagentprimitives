package spiceboxtoolspec_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxtoolspec"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/registry"
)

// echo is the only built-in we exercise; its revision lives in the embedded
// echo.yaml. If the YAML changes, update this constant.
const (
	builtinEchoName     = "echo"
	builtinEchoRevision = "2026-04-25"
)

// newToolspec builds a minimally-valid SpiceboxToolspec referencing the echo
// builtin. Tests mutate the returned object before passing it in.
func newToolspec(name string) *spiceboxv1alpha1.SpiceboxToolspec {
	return &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:             name,
			Version:          "1",
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: builtinEchoName, Revision: builtinEchoRevision},
			AllowSubcommands: []string{""}, // empty path = root subcommand of echo
		},
	}
}

// newReconciler returns a Reconciler wired against a fake client + a registry
// seeded from the project's compile-time built-in toolkits (which include the
// echo toolkit used by these tests).
func newReconciler(t *testing.T, objs ...client.Object) (*spiceboxtoolspec.Reconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.SpiceboxToolspec{}).
		Build()
	reg, err := registry.NewWithBuiltins(c)
	require.NoError(t, err, "registry.NewWithBuiltins")
	return &spiceboxtoolspec.Reconciler{Client: c, Registry: reg}, c
}

func reconcileGet(t *testing.T, r *spiceboxtoolspec.Reconciler, c client.Client, name string) *spiceboxv1alpha1.SpiceboxToolspec {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	require.NoError(t, err, "Reconcile %s", name)
	var got spiceboxv1alpha1.SpiceboxToolspec
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name}, &got),
		"Get %s after reconcile", name)
	return &got
}

func wantValidFalse(t *testing.T, ts *spiceboxv1alpha1.SpiceboxToolspec, reason string) {
	t.Helper()
	cond := meta.FindStatusCondition(ts.Status.Conditions, spiceboxv1alpha1.SpiceboxToolspecConditionValid)
	require.NotNil(t, cond, "Valid condition should be set")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid.Status")
	assert.Equal(t, reason, cond.Reason, "Valid.Reason; msg=%q", cond.Message)
}

func wantValidTrue(t *testing.T, ts *spiceboxv1alpha1.SpiceboxToolspec) {
	t.Helper()
	cond := meta.FindStatusCondition(ts.Status.Conditions, spiceboxv1alpha1.SpiceboxToolspecConditionValid)
	require.NotNil(t, cond, "Valid condition should be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "Valid.Status; got=%+v", cond)
}

func TestReconcile_HappyPath(t *testing.T) {
	ts := newToolspec("echo-default")
	r, c := newReconciler(t, ts)
	got := reconcileGet(t, r, c, ts.Name)
	wantValidTrue(t, got)
	assert.Equal(t, "<builtin>", got.Status.ResolvedToolkit, "ResolvedToolkit")
	assert.EqualValues(t, 1, got.Status.ObservedGeneration, "ObservedGeneration")
}

func TestReconcile_ToolkitNotFound(t *testing.T) {
	ts := newToolspec("ghost-spec")
	ts.Spec.Toolkit.Name = "ghost"
	r, c := newReconciler(t, ts)
	got := reconcileGet(t, r, c, ts.Name)
	wantValidFalse(t, got, spiceboxv1alpha1.ReasonToolkitMissing)
}

func TestReconcile_ToolkitWrongRevision(t *testing.T) {
	ts := newToolspec("echo-bad-rev")
	ts.Spec.Toolkit.Revision = "9999-99-99"
	r, c := newReconciler(t, ts)
	got := reconcileGet(t, r, c, ts.Name)
	wantValidFalse(t, got, spiceboxv1alpha1.ReasonToolkitMissing)
}

func TestReconcile_RegistryEmpty(t *testing.T) {
	// Toolkit name doesn't match any built-in AND no SpiceboxToolkit CRs
	// exist — exercise the registry-fallback path returning ErrNotFound.
	ts := newToolspec("nothing-registered")
	ts.Spec.Toolkit.Name = "nonexistent-tool"
	ts.Spec.Toolkit.Revision = "v0"
	r, c := newReconciler(t, ts)
	got := reconcileGet(t, r, c, ts.Name)
	wantValidFalse(t, got, spiceboxv1alpha1.ReasonToolkitMissing)
}

func TestReconcile_SpecLoadFailed(t *testing.T) {
	// LoadBytes requires Name and Version. Strip them.
	ts := newToolspec("missing-fields")
	ts.Spec.Name = ""
	ts.Spec.Version = ""
	r, c := newReconciler(t, ts)
	got := reconcileGet(t, r, c, ts.Name)
	wantValidFalse(t, got, spiceboxv1alpha1.ReasonSpecLoadFailed)
}

func TestReconcile_ConstraintCELCompileError(t *testing.T) {
	ts := newToolspec("bad-cel")
	ts.Spec.Constraints = []spiceboxv1alpha1.ToolspecConstraint{
		{CEL: "this is not valid CEL ! @#$"},
	}
	r, c := newReconciler(t, ts)
	got := reconcileGet(t, r, c, ts.Name)
	wantValidFalse(t, got, spiceboxv1alpha1.ReasonCELCompileError)
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxToolspecConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Contains(t, cond.Message, "constraints[0]", "error message should name the offending CEL field path")
}

func TestReconcile_ExceptionWhenCompileError(t *testing.T) {
	ts := newToolspec("bad-exception")
	ts.Spec.Exceptions = []spiceboxv1alpha1.ToolspecException{
		{
			Overrides: []string{"deny.effects.destructive"},
			When:      "this is not valid CEL ! @#$",
			Message:   "x",
		},
	}
	r, c := newReconciler(t, ts)
	got := reconcileGet(t, r, c, ts.Name)
	wantValidFalse(t, got, spiceboxv1alpha1.ReasonCELCompileError)
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxToolspecConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Contains(t, cond.Message, "exceptions[0].when",
		"error message should name the offending CEL field path")
}

func TestReconcile_ExceptionWhenEmptyIsSkipped(t *testing.T) {
	// LoadBytes rejects exceptions with When=="" before we ever reach the
	// CEL compile loop, so a truly empty-When can't make it past the
	// loadSpec phase. Construct a CR with a valid exception so we exercise
	// the "valid exception with non-empty When" path and confirm we still
	// reach Valid=True. (The compileExceptions phase explicitly contains a
	// `if e.When == "" { continue }` skip path that's defensive.)
	ts := newToolspec("good-exception")
	ts.Spec.Exceptions = []spiceboxv1alpha1.ToolspecException{
		{
			Overrides: []string{"deny.effects.destructive"},
			When:      "true",
			Message:   "x",
		},
	}
	r, c := newReconciler(t, ts)
	got := reconcileGet(t, r, c, ts.Name)
	wantValidTrue(t, got)
}

// A block that names facts but no subjects must be refused — a fact bound
// to no subject is a session-scoped boolean that would answer for every
// instance at once. The CRD's MinItems=1 on subjects refuses this shape at
// the apiserver; the fake client used here does not enforce CRD schema
// validation, so this test exercises the SECOND, redundant refusal — the
// one that must still hold for a CR applied before that schema shipped or
// through a client that skips validation. This is the motivating case for
// `observes`: a toolspec, not an MCPServer.
func TestReconcile_ObservesCompileError_NoSubjects(t *testing.T) {
	ts := newToolspec("bad-observes")
	ts.Spec.Observes = []spiceboxv1alpha1.ObservesBlock{{
		Facts: map[string]string{"is_cross_repository": "item.isCrossRepository"},
	}}
	r, c := newReconciler(t, ts)
	got := reconcileGet(t, r, c, ts.Name)
	wantValidFalse(t, got, spiceboxv1alpha1.ReasonCELCompileError)
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxToolspecConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Contains(t, cond.Message, "observes[0]", "error message should name the offending block")
	assert.Contains(t, cond.Message, "at least one subject")
}

func TestReconcile_ObservesCompileOK(t *testing.T) {
	ts := newToolspec("good-observes")
	ts.Spec.Observes = []spiceboxv1alpha1.ObservesBlock{{
		ForEach: `result.results`,
		Subjects: []spiceboxv1alpha1.ObserveSubject{
			{ResourceType: `"github_pr"`, ResourceID: `item.id`},
		},
		Facts: map[string]string{"is_cross_repository": "item.isCrossRepository"},
	}}
	r, c := newReconciler(t, ts)
	got := reconcileGet(t, r, c, ts.Name)
	wantValidTrue(t, got)
}

func TestReconcile_UnknownSubcommand(t *testing.T) {
	ts := newToolspec("bad-subcmd")
	// echo only declares one subcommand (the empty-path root). "bogus" is
	// definitely not a subcommand.
	ts.Spec.AllowSubcommands = []string{"bogus"}
	r, c := newReconciler(t, ts)
	got := reconcileGet(t, r, c, ts.Name)
	wantValidFalse(t, got, spiceboxv1alpha1.ReasonUnknownSubcommand)
}

func TestReconcile_ResolvedToolkitFromList(t *testing.T) {
	// Reference a non-builtin (name, revision) backed by a SpiceboxToolkit CR.
	// finalSuccess should resolve the CR's metadata.name into ResolvedToolkit.
	tk := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-tool-v1"},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name:            "custom-tool",
			Version:         "1",
			ToolkitRevision: "v1",
			Target:          spiceboxv1alpha1.ToolkitTarget{Binary: "/usr/bin/custom"},
			Parser:          spiceboxv1alpha1.ToolkitParserConfig{Kind: "declarative"},
			Env:             spiceboxv1alpha1.ToolkitEnv{Allowed: []spiceboxv1alpha1.ToolkitEnvVar{}},
			Subcommands: []spiceboxv1alpha1.ToolkitSubcommand{
				{
					Path: []string{},
					Effects: spiceboxv1alpha1.ToolkitEffects{
						Reads:      []string{},
						Writes:     []string{},
						Network:    spiceboxv1alpha1.ToolkitNetworkEffect{Destinations: []string{}},
						Filesystem: spiceboxv1alpha1.ToolkitFsEffect{Paths: []string{}},
						Creds:      spiceboxv1alpha1.ToolkitCredsEffect{Required: []string{}, Writes: []string{}},
					},
				},
			},
		},
	}
	ts := newToolspec("custom-spec")
	ts.Spec.Toolkit.Name = "custom-tool"
	ts.Spec.Toolkit.Revision = "v1"

	r, c := newReconciler(t, tk, ts)
	got := reconcileGet(t, r, c, ts.Name)
	wantValidTrue(t, got)
	assert.Equal(t, "custom-tool-v1", got.Status.ResolvedToolkit,
		"ResolvedToolkit should match the SpiceboxToolkit CR's metadata.name")
}

func TestReconcile_DeletionTimestampSkips(t *testing.T) {
	now := metav1.Now()
	ts := newToolspec("being-deleted")
	ts.DeletionTimestamp = &now
	ts.Finalizers = []string{"keep-me-around"} // fake client requires a finalizer for non-zero DeletionTimestamp
	r, c := newReconciler(t, ts)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: ts.Name}})
	require.NoError(t, err, "Reconcile on a deleting toolspec should not error")
	assert.Zero(t, res.RequeueAfter, "no RequeueAfter on deletion")
	assert.False(t, res.Requeue, "no Requeue on deletion")
	var got spiceboxv1alpha1.SpiceboxToolspec
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: ts.Name}, &got), "Get after reconcile")
	assert.Empty(t, got.Status.Conditions,
		"Status.Conditions should be untouched on delete; got=%+v", got.Status.Conditions)
}

func TestReconcile_NotFoundIsNoop(t *testing.T) {
	r, _ := newReconciler(t)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "missing"}})
	require.NoError(t, err, "Reconcile on missing toolspec should not error")
	assert.Zero(t, res.RequeueAfter, "no RequeueAfter on NotFound")
	assert.False(t, res.Requeue, "no Requeue on NotFound")
}
