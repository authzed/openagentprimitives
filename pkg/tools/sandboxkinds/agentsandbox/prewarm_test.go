package agentsandbox_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	sandboxextv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

// requirePrewarmer type-asserts a Runtime to sandboxkinds.Prewarmer, the
// optional interface only agent-sandbox implements. ReconcilePool is not part
// of the base sandboxkinds.Runtime interface, so every test in this file goes
// through this assertion rather than a bare method call.
func requirePrewarmer(t *testing.T, rt sandboxkinds.Runtime) sandboxkinds.Prewarmer {
	t.Helper()
	pw, ok := rt.(sandboxkinds.Prewarmer)
	require.True(t, ok, "agentsandbox.Runtime must implement sandboxkinds.Prewarmer")
	return pw
}

// poolRequest builds a fixture PoolRequest for "demo-class". Tests that need
// a specific Class or Replicas mutate the returned value.
//
// Uses REAL (non-zero) resource quantities rather than this package's shared
// testClass() helper (whose Resources field is left at the Go zero value).
// Comparing a SandboxTemplate's PodTemplate.Spec back out of the fake
// client's store round-trips every resource.Quantity through its wire
// encoding, and a zero-value Quantity's canonical string form ("0"/DecimalSI)
// differs from the Go zero value's empty internal cache -- an artifact of
// that round trip, not of ReconcilePool. Real values sidestep it entirely,
// matching the fixture shape podspec/classspec_test.go's demoClass() already
// uses for the same kind of exact-equality assertion.
func poolRequest() sandboxkinds.PoolRequest {
	return sandboxkinds.PoolRequest{
		ClassName: "demo-class",
		Namespace: "default",
		ClassUID:  types.UID("demo-class-uid"),
		Class: v1alpha1.SpiceboxClassSpec{
			Image: "demo.invalid/spicebox-sandbox:test",
			Resources: v1alpha1.SpiceboxResources{
				CPU:              resource.MustParse("1"),
				Memory:           resource.MustParse("256Mi"),
				EphemeralStorage: resource.MustParse("1Gi"),
			},
		},
		Replicas: 3,
	}
}

func listTemplates(t *testing.T, c client.Client) []sandboxextv1beta1.SandboxTemplate {
	t.Helper()
	var l sandboxextv1beta1.SandboxTemplateList
	require.NoError(t, c.List(t.Context(), &l, client.InNamespace("default")))
	return l.Items
}

func listWarmPools(t *testing.T, c client.Client) []sandboxextv1beta1.SandboxWarmPool {
	t.Helper()
	var l sandboxextv1beta1.SandboxWarmPoolList
	require.NoError(t, c.List(t.Context(), &l, client.InNamespace("default")))
	return l.Items
}

// listWarmPoolsAllNamespaces and listTemplatesAllNamespaces are the
// SweepOrphanedPools tests' equivalent of listWarmPools/listTemplates: those
// two are scoped to "default" because every OTHER test in this file uses
// poolRequest() unmodified, but a sweep is precisely about objects living in
// namespaces the class no longer names, so scoping to one namespace would
// hide the exact thing under test.
func listWarmPoolsAllNamespaces(t *testing.T, c client.Client) []sandboxextv1beta1.SandboxWarmPool {
	t.Helper()
	var l sandboxextv1beta1.SandboxWarmPoolList
	require.NoError(t, c.List(t.Context(), &l))
	return l.Items
}

func listTemplatesAllNamespaces(t *testing.T, c client.Client) []sandboxextv1beta1.SandboxTemplate {
	t.Helper()
	var l sandboxextv1beta1.SandboxTemplateList
	require.NoError(t, c.List(t.Context(), &l))
	return l.Items
}

// Case 1 (task-4-brief.md step 1.1): Replicas: 3 creates a SandboxTemplate
// whose PodTemplate.Spec equals podspec.BuildClassSpec(class), and a
// SandboxWarmPool with Replicas: 3, TemplateRef pointing at the template, and
// UpdateStrategy.Type == Recreate (Correction 2: upstream's OnReplenish
// default would let a session adopt a sandbox built from a stale class
// shape).
func TestReconcilePool_CreatesTemplateAndWarmPool(t *testing.T) {
	rt, c := newTestRuntime(t)
	pw := requirePrewarmer(t, rt)
	req := poolRequest()

	require.NoError(t, pw.ReconcilePool(t.Context(), req))

	wantSpec, err := podspec.BuildClassSpec(req.Class, req.Toolchains)
	require.NoError(t, err)

	templates := listTemplates(t, c)
	require.Len(t, templates, 1, "exactly one template")
	assert.Equal(t, wantSpec, templates[0].Spec.PodTemplate.Spec,
		"the template's rendered PodSpec must equal podspec.BuildClassSpec(class): "+
			"Task 5 uses the same function as the adoption eligibility test, so the "+
			"two can never disagree")
	assert.Equal(t, sandboxextv1beta1.NetworkPolicyManagementUnmanaged, templates[0].Spec.NetworkPolicyManagement,
		"must be Unmanaged: upstream's Managed default creates a SECOND, ADDITIVE NetworkPolicy "+
			"granting public egress, which would silently widen a networkMode: none session once it "+
			"adopted a pre-warmed sandbox -- AP's own per-session NetworkPolicy must be the sole authority")

	pools := listWarmPools(t, c)
	require.Len(t, pools, 1, "exactly one warm pool")
	pool := pools[0]
	require.NotNil(t, pool.Spec.Replicas)
	assert.EqualValues(t, 3, *pool.Spec.Replicas)
	assert.Equal(t, templates[0].Name, pool.Spec.TemplateRef.Name,
		"the pool must reference the template just created")
	require.NotNil(t, pool.Spec.UpdateStrategy, "UpdateStrategy must be set explicitly")
	assert.Equal(t, sandboxextv1beta1.RecreateSandboxWarmPoolUpdateStrategyType, pool.Spec.UpdateStrategy.Type,
		"Recreate is REQUIRED: upstream's OnReplenish default deliberately keeps stale "+
			"sandboxes in the pool until adopted, which would hand a session a sandbox "+
			"built from the previous class shape")
}

// Case 2: calling ReconcilePool twice with the identical request is
// idempotent -- one template, one pool, no error, AND the second call writes
// NOTHING to the pool (not just "ends up with the same values").
func TestReconcilePool_IsIdempotent(t *testing.T) {
	rt, c := newTestRuntime(t)
	pw := requirePrewarmer(t, rt)
	req := poolRequest()

	require.NoError(t, pw.ReconcilePool(t.Context(), req))

	poolsBefore := listWarmPools(t, c)
	require.Len(t, poolsBefore, 1)
	rvBefore := poolsBefore[0].ResourceVersion
	require.NotEmpty(t, rvBefore, "precondition: the fake client assigns a ResourceVersion on create")

	require.NoError(t, pw.ReconcilePool(t.Context(), req))

	assert.Len(t, listTemplates(t, c), 1, "a second identical reconcile must not create a second template")
	poolsAfter := listWarmPools(t, c)
	require.Len(t, poolsAfter, 1, "a second identical reconcile must not create a second pool")
	assert.Equal(t, rvBefore, poolsAfter[0].ResourceVersion,
		"the second call must not write to the pool AT ALL -- the fake client bumps "+
			"ResourceVersion on every Update, so an unchanged ResourceVersion is proof the "+
			"converged short-circuit in ensureWarmPool actually skipped the write, not merely "+
			"that a redundant write produced the same end state. A future edit that drops that "+
			"short-circuit would keep the count-based assertions above green while reintroducing "+
			"a write on every class reconcile; only this assertion catches that.")
}

// Case 3 (CORRECTION 1 -- the task's central design point): changing the
// class produces a NEW template name, re-points the SAME pool (its name is
// unchanged), and deletes the OLD template. This is the assertion that would
// have caught the brief's original defect, where the pool itself was
// hash-named: every class edit would have minted a brand new pool and
// orphaned the old one running N warm sandboxes forever.
func TestReconcilePool_ClassEditRepointsSamePoolAndDeletesOldTemplate(t *testing.T) {
	rt, c := newTestRuntime(t)
	pw := requirePrewarmer(t, rt)

	req1 := poolRequest()
	require.NoError(t, pw.ReconcilePool(t.Context(), req1))

	poolsBefore := listWarmPools(t, c)
	require.Len(t, poolsBefore, 1)
	poolNameBefore := poolsBefore[0].Name
	oldTemplateName := poolsBefore[0].Spec.TemplateRef.Name
	require.NotEmpty(t, oldTemplateName)

	req2 := poolRequest()
	req2.Class.Image = "demo.invalid/spicebox-sandbox:v2" // any change to the rendered PodSpec
	require.NoError(t, pw.ReconcilePool(t.Context(), req2))

	poolsAfter := listWarmPools(t, c)
	require.Len(t, poolsAfter, 1, "still exactly one pool object")
	assert.Equal(t, poolNameBefore, poolsAfter[0].Name,
		"the pool's own name must be UNCHANGED across a class edit -- a stable pool name is "+
			"what lets ReconcilePool re-point it instead of minting an orphaned duplicate")

	newTemplateName := poolsAfter[0].Spec.TemplateRef.Name
	assert.NotEqual(t, oldTemplateName, newTemplateName, "a class edit must mint a new template name")

	templates := listTemplates(t, c)
	require.Len(t, templates, 1, "the OLD template must be deleted, leaving exactly the new one")
	assert.Equal(t, newTemplateName, templates[0].Name)
}

// Case 4: Replicas: 0 deletes the warm pool (not the template); an
// already-absent pool is success.
func TestReconcilePool_ZeroReplicasDeletesPoolLeavesTemplate(t *testing.T) {
	rt, c := newTestRuntime(t)
	pw := requirePrewarmer(t, rt)
	req := poolRequest()
	require.NoError(t, pw.ReconcilePool(t.Context(), req))
	require.Len(t, listWarmPools(t, c), 1, "precondition: the pool exists")

	req.Replicas = 0
	require.NoError(t, pw.ReconcilePool(t.Context(), req))

	assert.Empty(t, listWarmPools(t, c), "Replicas: 0 must delete the warm pool")
	assert.Len(t, listTemplates(t, c), 1,
		"the template must be left in place: it is bounded (one per class shape) and cheap, "+
			"and re-creating it on the next non-zero reconcile would only churn it")

	// An already-absent pool is success, not an error -- this runs from a
	// class reconcile loop that re-enters on every pass.
	require.NoError(t, pw.ReconcilePool(t.Context(), req))
}

// Case 5 (ownerReferences ADDITION): an empty ClassUID is a fail-closed
// error, not a silent unowned create. Without an ownerReference, deleting the
// SpiceboxClass would leave its warm pool running N sandboxes forever.
func TestReconcilePool_EmptyClassUIDIsAFailClosedError(t *testing.T) {
	rt, c := newTestRuntime(t)
	pw := requirePrewarmer(t, rt)
	req := poolRequest()
	req.ClassUID = "" // zero-value UID

	err := pw.ReconcilePool(t.Context(), req)
	require.Error(t, err, "an unowned pool must be refused, not silently created")

	assert.Empty(t, listTemplates(t, c), "nothing must be created on the fail-closed path")
	assert.Empty(t, listWarmPools(t, c), "nothing must be created on the fail-closed path")
}

// Case 6 (ownerReferences ADDITION): both created objects carry an
// ownerReference to the SpiceboxClass, so ordinary Kubernetes garbage
// collection reclaims them when the class is deleted.
func TestReconcilePool_SetsOwnerReferenceToTheClass(t *testing.T) {
	rt, c := newTestRuntime(t)
	pw := requirePrewarmer(t, rt)
	req := poolRequest()
	require.NoError(t, pw.ReconcilePool(t.Context(), req))

	templates := listTemplates(t, c)
	require.Len(t, templates, 1)
	assertOwnedByClass(t, templates[0].OwnerReferences, req.ClassName, req.ClassUID)

	pools := listWarmPools(t, c)
	require.Len(t, pools, 1)
	assertOwnedByClass(t, pools[0].OwnerReferences, req.ClassName, req.ClassUID)
}

// Review item 5: a pool that somehow lacks the class's ownerRef must NOT be
// treated as converged -- otherwise it would never gain one, which is
// precisely the unowned-pool leak req.ClassUID exists to close. Strips the
// ownerRef directly via the client (there is no public path to create this
// state -- see the item's own "not reachable today" note) and asserts a
// second, otherwise-identical ReconcilePool call restores it rather than
// short-circuiting past it.
func TestReconcilePool_ReconcileRestoresAMissingOwnerRefOnAnExistingPool(t *testing.T) {
	rt, c := newTestRuntime(t)
	pw := requirePrewarmer(t, rt)
	req := poolRequest()
	require.NoError(t, pw.ReconcilePool(t.Context(), req))

	pools := listWarmPools(t, c)
	require.Len(t, pools, 1)
	pool := pools[0]
	require.NotEmpty(t, pool.OwnerReferences, "precondition: the create path did stamp an owner")
	pool.OwnerReferences = nil
	require.NoError(t, c.Update(t.Context(), &pool), "simulate a pool that somehow lost its ownerRef")

	require.NoError(t, pw.ReconcilePool(t.Context(), req))

	poolsAfter := listWarmPools(t, c)
	require.Len(t, poolsAfter, 1, "still exactly one pool -- the fix must repair it in place, not recreate it")
	assertOwnedByClass(t, poolsAfter[0].OwnerReferences, req.ClassName, req.ClassUID)
}

// Truncation-collision regression (review item 4): two SpiceboxClasses
// sharing a long common prefix must NOT truncate to the same pool name.
// dnsSafeName's naive truncation is not injective on its own -- cut two
// long, prefix-sharing names to the same length and they collapse to one
// string -- which for poolNameFor would mean class A's reconcile re-points
// (and interferes with) class B's warm pool.
func TestReconcilePool_TruncatedNamesStayInjectiveAcrossClasses(t *testing.T) {
	rt, c := newTestRuntime(t)
	pw := requirePrewarmer(t, rt)

	// 248 shared chars is long enough that "<prefix><suffix>-pool" exceeds the
	// 253-char DNS-1123 subdomain limit and dnsSafeName must truncate.
	prefix := strings.Repeat("a", 248)
	classA, classB := prefix+"-alpha", prefix+"-beta"

	reqA := poolRequest()
	reqA.ClassName, reqA.ClassUID = classA, types.UID("class-a-uid")
	require.NoError(t, pw.ReconcilePool(t.Context(), reqA))

	reqB := poolRequest()
	reqB.ClassName, reqB.ClassUID = classB, types.UID("class-b-uid")
	require.NoError(t, pw.ReconcilePool(t.Context(), reqB))

	pools := listWarmPools(t, c)
	require.Len(t, pools, 2,
		"two classes with a colliding truncated prefix must get TWO pools -- a naive truncation "+
			"that collapsed them to one shared pool would fail this on the very next line")
	assert.NotEqual(t, pools[0].Name, pools[1].Name,
		"truncated pool names must stay injective across different class names")
}

// Case (a) from the orphan-sweep review round: a namespace REMOVED from the
// desired set is reclaimed, while a namespace still IN it is left untouched.
// ReconcilePool's own per-namespace loop only ever visits namespaces
// CURRENTLY named, so a namespace dropped from a class's warmPool.namespaces
// would otherwise run -- and bill -- forever with nothing left pointing at
// it; SweepOrphanedPools is the only mechanism that revisits it.
func TestSweepOrphanedPools_ReclaimsANamespaceRemovedFromTheDesiredSet(t *testing.T) {
	rt, c := newTestRuntime(t)
	pw := requirePrewarmer(t, rt)

	reqA, reqB := poolRequest(), poolRequest()
	reqA.Namespace, reqB.Namespace = "ns-a", "ns-b"
	require.NoError(t, pw.ReconcilePool(t.Context(), reqA))
	require.NoError(t, pw.ReconcilePool(t.Context(), reqB))
	require.Len(t, listWarmPoolsAllNamespaces(t, c), 2, "precondition: both namespaces have a pool")

	require.NoError(t, pw.SweepOrphanedPools(t.Context(), reqA.ClassName, reqA.ClassUID, []string{"ns-a"}))

	poolsAfter := listWarmPoolsAllNamespaces(t, c)
	require.Len(t, poolsAfter, 1, "ns-b's pool must be reclaimed; ns-a's must survive")
	assert.Equal(t, "ns-a", poolsAfter[0].Namespace)

	templatesAfter := listTemplatesAllNamespaces(t, c)
	require.Len(t, templatesAfter, 1, "ns-b's template must be reclaimed too, not just its pool")
	assert.Equal(t, "ns-a", templatesAfter[0].Namespace)
}

// Case (b): an EMPTY desired set reclaims every pool the class has anywhere.
// This is what a fully-cleared warmPool stanza resolves to (as opposed to
// replicas: 0 with the namespace list KEPT, which the per-namespace loop in
// ReconcilePool already tears down on its own) -- "no list means pre-warming
// OFF, no spend" must hold even when there is no list left to read at all.
func TestSweepOrphanedPools_EmptyDesiredSetReclaimsEverything(t *testing.T) {
	rt, c := newTestRuntime(t)
	pw := requirePrewarmer(t, rt)

	reqA, reqB := poolRequest(), poolRequest()
	reqA.Namespace, reqB.Namespace = "ns-a", "ns-b"
	require.NoError(t, pw.ReconcilePool(t.Context(), reqA))
	require.NoError(t, pw.ReconcilePool(t.Context(), reqB))
	require.Len(t, listWarmPoolsAllNamespaces(t, c), 2, "precondition: both namespaces have a pool")

	require.NoError(t, pw.SweepOrphanedPools(t.Context(), reqA.ClassName, reqA.ClassUID, nil))

	assert.Empty(t, listWarmPoolsAllNamespaces(t, c),
		"an empty desired set must reclaim EVERY pool this class has, in every namespace")
	assert.Empty(t, listTemplatesAllNamespaces(t, c))
}

// Case (c), the negative control: a namespace STILL in the desired set must
// survive a sweep untouched. Without this, cases (a) and (b) alone could not
// distinguish "the sweep correctly reclaims only what it should" from "the
// sweep deletes everything unconditionally."
func TestSweepOrphanedPools_LeavesADesiredNamespaceInPlace(t *testing.T) {
	rt, c := newTestRuntime(t)
	pw := requirePrewarmer(t, rt)
	req := poolRequest()
	require.NoError(t, pw.ReconcilePool(t.Context(), req))

	require.NoError(t, pw.SweepOrphanedPools(t.Context(), req.ClassName, req.ClassUID, []string{req.Namespace}))

	assert.Len(t, listWarmPoolsAllNamespaces(t, c), 1, "a namespace still in the desired set must survive")
	assert.Len(t, listTemplatesAllNamespaces(t, c), 1)
}

// The label the sweep lists by is a fast index, not the authority: ownership
// is double-checked against the SAME ownerReference ReconcilePool stamps
// before any delete. This is deliberately NOT proven with two different
// class names — their classLabelValue hashes differ, so a List already
// excludes the other class's objects before hasOwnerRef is ever reached,
// which would pass identically with the ownerRef check deleted. Instead:
// stamp class A's OWN label onto an object whose ownerRef points somewhere
// else entirely (simulating, e.g., a hand-edited or corrupted object) and
// prove the mismatch alone is what saves it.
func TestSweepOrphanedPools_SkipsAnObjectCarryingTheLabelButAMismatchedOwnerRef(t *testing.T) {
	rt, c := newTestRuntime(t)
	pw := requirePrewarmer(t, rt)
	req := poolRequest()
	require.NoError(t, pw.ReconcilePool(t.Context(), req))

	pools := listWarmPools(t, c)
	require.Len(t, pools, 1)
	pool := pools[0]
	// Corrupt ONLY the ownerRef -- pool.Labels is left untouched, so it still
	// carries the label ReconcilePool stamped, matching req.ClassName exactly.
	pool.OwnerReferences = []metav1.OwnerReference{{
		APIVersion:         v1alpha1.SchemeGroupVersion.String(),
		Kind:               "SpiceboxClass",
		Name:               "some-other-class",
		UID:                types.UID("some-other-class-uid"),
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}}
	require.NoError(t, c.Update(t.Context(), &pool), "simulate an object carrying class A's label but a foreign ownerRef")

	// Sweep for req.ClassName/req.ClassUID with an empty desired set -- if the
	// sweep relied on the label alone, this pool (label still matches) would
	// be reclaimed; only the ownerRef mismatch can save it.
	require.NoError(t, pw.SweepOrphanedPools(t.Context(), req.ClassName, req.ClassUID, nil))

	poolsAfter := listWarmPools(t, c)
	require.Len(t, poolsAfter, 1, "an object whose label matches but whose ownerRef does NOT must survive the sweep")
}

// A cluster that serves the base Sandbox CRD but not the extensions bundle
// (SandboxClaim/SandboxWarmPool) structurally implements Prewarmer — the
// type assertion the spiceboxclass controller uses cannot see this gap —
// but cannot actually honor a pre-warm request. ReconcilePool must say so
// with the shared, detectable sentinel rather than whatever raw NoKindMatch
// the RESTMapper happens to produce, so a caller can tell "this cluster
// cannot pre-warm" apart from a transient failure.
func TestReconcilePool_PrewarmingUnavailableIsATypedError(t *testing.T) {
	rt, c := newRuntimeWithoutPrewarmCRDs(t)
	pw := requirePrewarmer(t, rt)
	req := poolRequest()

	err := pw.ReconcilePool(t.Context(), req)
	require.Error(t, err)
	assert.ErrorIs(t, err, sandboxkinds.ErrPrewarmingUnavailable)

	// refuseExtensionsGroup (wired into newRuntimeWithoutPrewarmCRDs) makes
	// any Get/Create/Update against these kinds fail loudly, so a passing
	// assertion here also proves the guard short-circuited BEFORE any such
	// call, not merely that the eventual attempt failed.
	assert.Empty(t, listTemplates(t, c), "nothing must be created when pre-warming cannot be honored")
	assert.Empty(t, listWarmPools(t, c), "nothing must be created when pre-warming cannot be honored")
}

// Replicas: 0 with pre-warming unavailable is a harmless no-op: nothing
// could exist to tear down, since the extensions CRDs a pool would live
// under are not installed. This must NOT error — a class with no warmPool
// request, or one that just turned pre-warming off, would otherwise fail
// every reconcile forever on a cluster that never had the extensions CRDs
// to begin with.
func TestReconcilePool_PrewarmingUnavailableWithZeroReplicasIsANoOp(t *testing.T) {
	rt, _ := newRuntimeWithoutPrewarmCRDs(t)
	pw := requirePrewarmer(t, rt)
	req := poolRequest()
	req.Replicas = 0

	require.NoError(t, pw.ReconcilePool(t.Context(), req))
}

func assertOwnedByClass(t *testing.T, refs []metav1.OwnerReference, className string, classUID types.UID) {
	t.Helper()
	require.Len(t, refs, 1, "exactly one owner")
	ref := refs[0]
	// APIVersion is checked, not just Kind/Name/UID: a wrong APIVersion
	// produces an ownerRef Kubernetes GC cannot resolve, so the object is
	// NEVER reclaimed when the class is deleted -- the exact leak this
	// ownerRef was added to close, and Kind/Name/UID alone cannot detect it.
	assert.Equal(t, v1alpha1.SchemeGroupVersion.String(), ref.APIVersion,
		"a wrong APIVersion makes this ownerRef unresolvable by GC")
	assert.Equal(t, "SpiceboxClass", ref.Kind)
	assert.Equal(t, className, ref.Name)
	assert.Equal(t, classUID, ref.UID)
	require.NotNil(t, ref.Controller)
	assert.True(t, *ref.Controller)
	require.NotNil(t, ref.BlockOwnerDeletion)
	assert.True(t, *ref.BlockOwnerDeletion)
}
