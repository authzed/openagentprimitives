//go:build integration

// Integration tests for Task 9 (spec §13): the pooled MaxDelegatedAgents
// ceiling must count a builder root's own workshop children -- living in
// the workshop namespace W -- alongside its same-namespace descendants in
// B, not just the latter (pkg/apis/v1alpha1/agentsession_closure.go's
// WorkshopNamespacesFor, and the ceiling-count call site in controller.go).
//
// These run against a REAL apiserver (envtest): the Workshop CR Get, its
// owner-ref check, the real status subresource, and the per-namespace
// ListClosure label-selector Lists all need genuine List/Get semantics that
// a fake client's own reimplementation of them is not itself proof of.
//
// No SpiceDB dependency: every "next delegation" tested here is a
// same-namespace SubagentRequest, which never reaches
// admitCrossNamespaceParent -- this file tests the pooled-ceiling COUNT,
// not the cross-namespace admission gate Task 2 already covers in
// controller_crossnamespace_integration_test.go (whose TestMain, shared by
// this whole package's integration binary, still boots a SpiceDB container
// regardless -- unused by the tests below, but not worth a second binary to
// avoid).
package subagentrequest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// wsCeilRootClass builds the builder root's own AgentClass: a roster naming
// childClassName, and an explicit Budget carrying maxDelegatedAgents under
// test. MaxTurns/MaxTokens are set only to satisfy the CRD's own
// required+minimum=1 validation on Budget's sibling fields -- the fake
// client used by this package's unit tests (controller_test.go's
// treeCeilingFixture) never enforces that, but this file's real apiserver
// does.
func wsCeilRootClass(ns, name, childClassName string, maxDelegatedAgents int32) *v1.AgentClass {
	return &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1.AgentClassSpec{
			SystemPrompt: v1.PromptSource{Inline: "you are the builder"},
			IdentityMode: v1.IdentityModeAgent,
			Subagents:    []string{childClassName},
			Budget: &v1.BudgetConfig{
				MaxTurns:           10,
				MaxTokens:          50000,
				MaxDelegatedAgents: maxDelegatedAgents,
			},
		},
	}
}

// wsCeilChildClass builds the delegated child's own class: a leaf, no
// roster of its own.
func wsCeilChildClass(ns, name string) *v1.AgentClass {
	return &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1.AgentClassSpec{
			SystemPrompt: v1.PromptSource{Inline: "you are the class under test"},
			IdentityMode: v1.IdentityModeAgent,
		},
	}
}

func wsCeilRootSession(ns, name, class string) *v1.AgentSession {
	return &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1.AgentSessionSpec{Class: class, Prompt: v1.PromptSource{Inline: "build me an agent"}},
	}
}

// wsCeilMember builds an AgentSession already labelled as a member of
// rootName's delegation closure -- standing in for a session buildChild
// would have created on some earlier reconcile pass, whether same-namespace
// (B) or in a workshop namespace (W): ListClosure finds either by the same
// label, scoped by whichever namespace it is listed in. Its phase is left
// at the zero value ("") -- isTerminalAgentSessionPhase treats that as
// live, exactly like Running or any Awaiting* phase.
func wsCeilMember(ns, name, rootName, class string) *v1.AgentSession {
	return &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels:    map[string]string{v1.LabelDelegationRoot: rootName},
		},
		Spec: v1.AgentSessionSpec{Class: class, Prompt: v1.PromptSource{Inline: "member"}},
	}
}

// wsCeilWorkshop creates root's own Workshop CR -- genuinely
// controller-owned by root via cosidecar.OwnerRef, the exact owner-ref
// shape ensureWorkshop stamps in production (pkg/controllers/agentsession/
// workshop_hook.go) -- then marks it Ready with status.namespace = the
// workshop namespace via a real status-subresource Update, mirroring
// pkg/web/workshoptranscriptsrv's own integration fixture for the identical
// CR shape (integrationWorkshop).
func wsCeilWorkshop(t *testing.T, env *testenv.Env, root *v1.AgentSession, wsNamespace string) *v1.Workshop {
	t.Helper()
	ctx := context.Background()
	ws := &v1.Workshop{
		ObjectMeta: metav1.ObjectMeta{
			Name:            v1.WorkshopName(root.Name),
			Namespace:       root.Namespace,
			OwnerReferences: cosidecar.OwnerRef(root),
		},
		Spec: v1.WorkshopSpec{
			Session:        v1.NamespacedRef{Namespace: root.Namespace, Name: root.Name},
			SidecarToolbox: "workshop",
			Limits: v1.WorkshopLimits{
				MaxAge:              metav1.Duration{Duration: time.Hour},
				MaxObjectsPerKind:   20,
				MaxObjects:          100,
				MaxConcurrentProbes: 2,
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")
	ws.Status.Namespace = wsNamespace
	ws.Status.Phase = v1.WorkshopPhaseReady
	require.NoError(t, env.Client.Status().Update(ctx, ws), "mark Workshop Ready with its namespace")
	return ws
}

// wsCeilNamespace creates the real workshop Namespace object W -- envtest
// starts with only "default" pre-created.
func wsCeilNamespace(t *testing.T, env *testenv.Env, name string) {
	t.Helper()
	err := env.Client.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	require.NoError(t, client.IgnoreAlreadyExists(err), "create workshop namespace %s", name)
}

func wsCeilRequest(ns, name, parentNS, parentName, class string) *v1.SubagentRequest {
	return &v1.SubagentRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1.SubagentRequestSpec{
			Parent: v1.NamespacedRef{Namespace: parentNS, Name: parentName},
			Class:  class,
			Task:   "run one more delegation",
		},
	}
}

// TestReconcile_TreeCeiling_WorkshopChildCountedTowardCeiling_RefusesNextDelegation
// is the load-bearing regression test: a root with maxDelegatedAgents=3, one
// same-namespace descendant already in B, and one LIVE workshop child
// already in W is already AT the ceiling (root + sibling + workshop child =
// 3) -- so the next delegation, an ordinary same-namespace request, must be
// refused. Before Task 9, ListClosure(root.Namespace, root.Name) alone never
// sees the workshop child (it lives in W, a different namespace), so the
// count reads 2 (root + sibling), under the ceiling, and this request is
// wrongly ADMITTED -- this test fails against that code and passes only
// once the workshop child is counted.
//
// A SECOND, TERMINAL (Succeeded) workshop child is also seeded to prove the
// count is LIVE-filtered rather than "every session the workshop has ever
// run": if it were counted too, the denial below would read "4 of 3", not
// "3 of 3".
func TestReconcile_TreeCeiling_WorkshopChildCountedTowardCeiling_RefusesNextDelegation(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	const (
		ns             = "default"
		rootName       = "wsceil-root-1"
		rootClassName  = "wsceil-root-class-1"
		childClassName = "wsceil-child-class-1"
		wsNamespace    = "ws-ceil-1"
	)

	require.NoError(t, env.Client.Create(ctx, wsCeilRootClass(ns, rootClassName, childClassName, 3)), "create root AgentClass")
	require.NoError(t, env.Client.Create(ctx, wsCeilChildClass(ns, childClassName)), "create child AgentClass")

	root := wsCeilRootSession(ns, rootName, rootClassName)
	require.NoError(t, env.Client.Create(ctx, root), "create root AgentSession")

	// One same-namespace descendant, already a member of root's closure.
	require.NoError(t, env.Client.Create(ctx, wsCeilMember(ns, "wsceil-sibling-1", rootName, childClassName)),
		"create the same-namespace sibling")

	// root's own Workshop, with a LIVE test child in its workshop namespace W.
	wsCeilNamespace(t, env, wsNamespace)
	wsCeilWorkshop(t, env, root, wsNamespace)
	require.NoError(t, env.Client.Create(ctx, wsCeilMember(wsNamespace, "wsceil-workshop-child-live", rootName, childClassName)),
		"create the LIVE workshop child in W")

	// A second workshop child, already resolved (Succeeded) -- must NOT count.
	terminalChild := wsCeilMember(wsNamespace, "wsceil-workshop-child-done", rootName, childClassName)
	require.NoError(t, env.Client.Create(ctx, terminalChild), "create the terminal workshop child in W")
	terminalChild.Status.Phase = v1.AgentSessionPhaseSucceeded
	require.NoError(t, env.Client.Status().Update(ctx, terminalChild), "mark the second workshop child Succeeded")

	// The next delegation: an ordinary same-namespace child of the same
	// root. It never touches admitCrossNamespaceParent (sr.Namespace ==
	// sr.Spec.Parent.Namespace == "default"), so this test needs no
	// SpiceDB/WorkshopBuildChecker at all.
	sr := wsCeilRequest(ns, "wsceil-next-1", ns, rootName, childClassName)
	require.NoError(t, env.Client.Create(ctx, sr), "create the next SubagentRequest")

	r := &Reconciler{Client: env.Client, Scheme: env.Scheme, Authz: &fakeAuthz{}, MaxDelegationDepth: 3}
	xnsReconcile(t, env, r, ns, "wsceil-next-1")

	var got v1.SubagentRequest
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "wsceil-next-1"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase,
		"the tree's live workshop child must count toward the pooled ceiling")
	assert.Equal(t, "TreeCeilingExceeded", got.Status.FailureReason)
	assert.Contains(t, got.Status.Determination, "3 of 3",
		"count must be root(1) + same-ns sibling(1) + the LIVE workshop child(1) == 3; the terminal workshop child must not add a fourth")
	assert.Empty(t, got.Status.ChildRef, "a refused ceiling must never create a child")
}

// TestReconcile_TreeCeiling_WorkshopChildUnderCeiling_StillAdmitted proves
// the fix is a genuine count, not a blanket "a workshop exists, so refuse":
// the identical tree shape (root + one same-namespace sibling + one live
// workshop child == 3) one ceiling higher (4) is one agent under the
// ceiling, so the next delegation is admitted and the child is created.
func TestReconcile_TreeCeiling_WorkshopChildUnderCeiling_StillAdmitted(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	const (
		ns             = "default"
		rootName       = "wsceil-root-2"
		rootClassName  = "wsceil-root-class-2"
		childClassName = "wsceil-child-class-2"
		wsNamespace    = "ws-ceil-2"
	)

	require.NoError(t, env.Client.Create(ctx, wsCeilRootClass(ns, rootClassName, childClassName, 4)), "create root AgentClass")
	require.NoError(t, env.Client.Create(ctx, wsCeilChildClass(ns, childClassName)), "create child AgentClass")

	root := wsCeilRootSession(ns, rootName, rootClassName)
	require.NoError(t, env.Client.Create(ctx, root), "create root AgentSession")

	require.NoError(t, env.Client.Create(ctx, wsCeilMember(ns, "wsceil-sibling-2", rootName, childClassName)),
		"create the same-namespace sibling")

	wsCeilNamespace(t, env, wsNamespace)
	wsCeilWorkshop(t, env, root, wsNamespace)
	require.NoError(t, env.Client.Create(ctx, wsCeilMember(wsNamespace, "wsceil-workshop-child-2", rootName, childClassName)),
		"create the LIVE workshop child in W")

	sr := wsCeilRequest(ns, "wsceil-next-2", ns, rootName, childClassName)
	require.NoError(t, env.Client.Create(ctx, sr), "create the next SubagentRequest")

	r := &Reconciler{Client: env.Client, Scheme: env.Scheme, Authz: &fakeAuthz{}, MaxDelegationDepth: 3}
	xnsReconcile(t, env, r, ns, "wsceil-next-2")

	var got v1.SubagentRequest
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "wsceil-next-2"}, &got))
	require.NotNil(t, got.Status.ChildRef, "one under the ceiling (with the workshop child correctly counted) must still create the child")
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase)
}

// TestReconcile_TreeCeiling_NoWorkshop_CountUnchanged pins the "byte
// identical" requirement: a root with no Workshop CR at all must behave
// exactly as it did before Task 9 -- WorkshopNamespacesFor resolves (nil,
// nil), the workshop-side loop is a no-op, and the ceiling check is
// unaffected.
func TestReconcile_TreeCeiling_NoWorkshop_CountUnchanged(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	const (
		ns             = "default"
		rootName       = "wsceil-root-3"
		rootClassName  = "wsceil-root-class-3"
		childClassName = "wsceil-child-class-3"
	)

	require.NoError(t, env.Client.Create(ctx, wsCeilRootClass(ns, rootClassName, childClassName, 3)), "create root AgentClass")
	require.NoError(t, env.Client.Create(ctx, wsCeilChildClass(ns, childClassName)), "create child AgentClass")

	root := wsCeilRootSession(ns, rootName, rootClassName)
	require.NoError(t, env.Client.Create(ctx, root), "create root AgentSession")

	// Two same-namespace descendants, at the ceiling already (root + 2 == 3).
	// No Workshop CR exists for this root at all.
	require.NoError(t, env.Client.Create(ctx, wsCeilMember(ns, "wsceil-sibling-3a", rootName, childClassName)))
	require.NoError(t, env.Client.Create(ctx, wsCeilMember(ns, "wsceil-sibling-3b", rootName, childClassName)))

	sr := wsCeilRequest(ns, "wsceil-next-3", ns, rootName, childClassName)
	require.NoError(t, env.Client.Create(ctx, sr), "create the next SubagentRequest")

	r := &Reconciler{Client: env.Client, Scheme: env.Scheme, Authz: &fakeAuthz{}, MaxDelegationDepth: 3}
	xnsReconcile(t, env, r, ns, "wsceil-next-3")

	var got v1.SubagentRequest
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "wsceil-next-3"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase)
	assert.Equal(t, "TreeCeilingExceeded", got.Status.FailureReason)
	assert.Contains(t, got.Status.Determination, "3 of 3",
		"no Workshop CR exists for this root; the count must be exactly root + 2 same-namespace descendants, unchanged by Task 9")
}
