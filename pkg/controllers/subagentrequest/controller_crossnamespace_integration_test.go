//go:build integration

// Integration tests for step 0's ONE relaxation of the same-namespace rule:
// a SubagentRequest whose parent is in a different namespace is admitted only
// when that namespace is a workshop provisioned for exactly the claimed
// parent AND the parent holds the workshop's real SpiceDB workshop:<W>#build
// tuple (spec §2.6). These run against a REAL apiserver (envtest) and a REAL
// SpiceDB (testspicedb) — a fake WorkshopBuildChecker could be made to say
// anything, so the refusing direction is only worth something proven against
// the genuine authorization backend: EnsureWorkshopSubjects really writes the
// tuple, CheckWorkshopBuild really reads it back, and the no-tuple case
// proves the refusal holds against SpiceDB itself, not a stand-in for it.
package subagentrequest

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

func TestMain(m *testing.M) {
	code := testenv.RunPackage(m)
	// These tests boot a shared SpiceDB container; stop it after the
	// apiserver so a package run leaves no container behind — mirrors
	// pkg/controllers/workshopprobe/controller_integration_test.go.
	testspicedb.StopShared()
	os.Exit(code)
}

// newSpiceDBClient boots a per-test datastore loaded with the canonical
// schema and returns a client bound to it. Mirrors pkg/controllers/
// workshopprobe/controller_integration_test.go's helper of the same
// name/shape.
func newSpiceDBClient(t *testing.T) *spicedb.Client {
	t.Helper()
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	c, err := spicedb.NewClient(endpoint, token, true /* insecure */)
	require.NoError(t, err, "spicedb.NewClient")
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// spyWorkshopBuildChecker wraps a real WorkshopBuildChecker and records
// whether it was ever consulted — the property that tells "never a workshop
// request" (denied before the checker is reached, e.g. a namespace-label
// mismatch) apart from "a workshop request whose tuple check failed" (denied
// only after consulting the real backend).
type spyWorkshopBuildChecker struct {
	inner  WorkshopBuildChecker
	called bool
}

func (s *spyWorkshopBuildChecker) CheckWorkshopBuild(ctx context.Context, workshopID, sessNS, sessName string) (bool, error) {
	s.called = true
	return s.inner.CheckWorkshopBuild(ctx, workshopID, sessNS, sessName)
}

// xnsNamespace builds the workshop namespace W, labeled for the session it
// was provisioned for — the ONLY thing admitCrossNamespaceParent trusts to
// attribute sr.Namespace to a session, per LabelWorkshopSessionNamespace/
// LabelWorkshopSessionName's own doc.
func xnsNamespace(name, sessNS, sessName string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				v1.LabelWorkshopSessionNamespace: sessNS,
				v1.LabelWorkshopSessionName:      sessName,
			},
		},
	}
}

// xnsParentClass builds the delegating (builder) session's AgentClass, in B,
// with a roster naming exactly the workshop child class.
func xnsParentClass(ns, name, childClassName string) *v1.AgentClass {
	return &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1.AgentClassSpec{
			SystemPrompt: v1.PromptSource{Inline: "you are the builder"},
			IdentityMode: v1.IdentityModeAgent,
			Subagents:    []string{childClassName},
		},
	}
}

// xnsChildClass builds the workshop's own class-under-test, in W, a leaf on
// the delegation graph (no further roster of its own).
func xnsChildClass(ns, name string) *v1.AgentClass {
	return &v1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1.AgentClassSpec{
			SystemPrompt: v1.PromptSource{Inline: "you are the class under test"},
			IdentityMode: v1.IdentityModeAgent,
		},
	}
}

// xnsParentSession builds the delegating (builder) AgentSession B/X.
func xnsParentSession(ns, name, class string) *v1.AgentSession {
	return &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1.AgentSessionSpec{
			Class:  class,
			Prompt: v1.PromptSource{Inline: "build me an agent"},
		},
	}
}

// xnsRequest builds a SubagentRequest IN the workshop namespace ns, naming a
// parent that may live in a different namespace entirely.
func xnsRequest(ns, name, parentNS, parentName, class string) *v1.SubagentRequest {
	return &v1.SubagentRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1.SubagentRequestSpec{
			Parent: v1.NamespacedRef{Namespace: parentNS, Name: parentName},
			Class:  class,
			Task:   "run the workshop's test",
		},
	}
}

func xnsReconcile(t *testing.T, env *testenv.Env, r *Reconciler, ns, name string) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	require.NoError(t, err, "a policy refusal or admission is a recorded result, not a reconcile error")
}

// TestReconcile_CrossNamespaceWorkshopChild_TupleHolds_Admitted is the
// positive control (spec §2.6): with the workshop namespace genuinely
// labeled for the claimed parent AND the workshop:<W>#build tuple genuinely
// written in SpiceDB, a cross-namespace SubagentRequest is admitted — step 0
// falls through rather than denying — and the child AgentSession is built IN
// the workshop namespace, parented at the builder session across the
// namespace boundary.
func TestReconcile_CrossNamespaceWorkshopChild_TupleHolds_Admitted(t *testing.T) {
	env := testenv.Shared(t)
	spdb := newSpiceDBClient(t)
	ctx := context.Background()

	const (
		wsNamespace     = "ws-xns-admit"
		sessNS          = "default"
		sessName        = "xns-builder-admit"
		parentClassName = "xns-parent-class-admit"
		childClassName  = "xns-child-class-admit"
	)
	require.NoError(t, env.Client.Create(ctx, xnsNamespace(wsNamespace, sessNS, sessName)), "create workshop namespace")
	require.NoError(t, env.Client.Create(ctx, xnsParentClass(sessNS, parentClassName, childClassName)), "create parent AgentClass in B")
	require.NoError(t, env.Client.Create(ctx, xnsChildClass(wsNamespace, childClassName)), "create child AgentClass in W")
	require.NoError(t, env.Client.Create(ctx, xnsParentSession(sessNS, sessName, parentClassName)), "create parent AgentSession B/X")
	require.NoError(t, spdb.EnsureWorkshopSubjects(ctx, wsNamespace, sessNS, sessName, identity.CanonicalUserID{}), "seed the workshop#build tuple")

	sr := xnsRequest(wsNamespace, "req-admit", sessNS, sessName, childClassName)
	require.NoError(t, env.Client.Create(ctx, sr), "create SubagentRequest in W")

	r := &Reconciler{
		Client:             env.Client,
		Scheme:             env.Scheme,
		Authz:              &fakeAuthz{},
		WorkshopBuild:      spdb,
		MaxDelegationDepth: 3,
	}
	xnsReconcile(t, env, r, wsNamespace, "req-admit")

	var got v1.SubagentRequest
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: wsNamespace, Name: "req-admit"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase,
		"a workshop cross-namespace delegation with a genuine tuple must proceed past step 0")
	require.NotNil(t, got.Status.ChildRef, "an admitted request must create a child")
	assert.Equal(t, wsNamespace, got.Status.ChildRef.Namespace, "the child must live in the workshop namespace, not the parent's")

	var child v1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{
		Namespace: got.Status.ChildRef.Namespace, Name: got.Status.ChildRef.Name,
	}, &child))
	assert.Equal(t, childClassName, child.Spec.Class)
	require.NotNil(t, child.Spec.Parent, "the child must record its cross-namespace parent")
	assert.Equal(t, sessNS, child.Spec.Parent.Namespace)
	assert.Equal(t, sessName, child.Spec.Parent.Name)
}

// TestReconcile_CrossNamespaceWorkshopChild_NoTuple_Denied is the load-bearing
// refusal: the workshop namespace is genuinely labeled for the claimed
// parent, but NO workshop:<W>#build tuple was ever written. The request must
// be denied ReasonWorkshopBuildDenied, no child may exist, and — proven
// against the real backend via the spy — the checker must actually have been
// consulted, which is what tells this refusal apart from one that never
// reached SpiceDB at all.
func TestReconcile_CrossNamespaceWorkshopChild_NoTuple_Denied(t *testing.T) {
	env := testenv.Shared(t)
	spdb := newSpiceDBClient(t)
	ctx := context.Background()

	const (
		wsNamespace     = "ws-xns-notuple"
		sessNS          = "default"
		sessName        = "xns-builder-notuple"
		parentClassName = "xns-parent-class-notuple"
		childClassName  = "xns-child-class-notuple"
	)
	require.NoError(t, env.Client.Create(ctx, xnsNamespace(wsNamespace, sessNS, sessName)), "create workshop namespace")
	require.NoError(t, env.Client.Create(ctx, xnsParentClass(sessNS, parentClassName, childClassName)), "create parent AgentClass in B")
	require.NoError(t, env.Client.Create(ctx, xnsChildClass(wsNamespace, childClassName)), "create child AgentClass in W")
	require.NoError(t, env.Client.Create(ctx, xnsParentSession(sessNS, sessName, parentClassName)), "create parent AgentSession B/X")
	// Deliberately NO spdb.EnsureWorkshopSubjects call: the tuple is absent.

	sr := xnsRequest(wsNamespace, "req-notuple", sessNS, sessName, childClassName)
	require.NoError(t, env.Client.Create(ctx, sr), "create SubagentRequest in W")

	spy := &spyWorkshopBuildChecker{inner: spdb}
	r := &Reconciler{
		Client:             env.Client,
		Scheme:             env.Scheme,
		Authz:              &fakeAuthz{},
		WorkshopBuild:      spy,
		MaxDelegationDepth: 3,
	}
	xnsReconcile(t, env, r, wsNamespace, "req-notuple")

	var got v1.SubagentRequest
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: wsNamespace, Name: "req-notuple"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase)
	assert.Equal(t, ReasonWorkshopBuildDenied, got.Status.FailureReason)
	assert.Empty(t, got.Status.ChildRef, "a request without the workshop#build tuple must never get a child")
	assert.True(t, spy.called, "a genuine workshop cross-namespace attempt must consult the real WorkshopBuild checker")

	var sessions v1.AgentSessionList
	require.NoError(t, env.Client.List(ctx, &sessions, client.InNamespace(wsNamespace)))
	assert.Empty(t, sessions.Items, "no child session may exist in W without the tuple")
}

// TestReconcile_CrossNamespaceWorkshopChild_LabelMismatch_Denied proves the
// namespace's own labels — not the SubagentRequest's caller-supplied
// spec.parent — are what gate the workshop path: the workshop namespace here
// is labeled for a DIFFERENT session than the one the request claims as its
// parent, so this was never a workshop delegation attempt for THIS claimed
// parent at all. It must fall back to the pre-existing "ParentCrossNamespace"
// refusal, and the WorkshopBuild checker must never even be consulted — a
// caller cannot forge its way to a SpiceDB check by naming an arbitrary
// parent underneath someone else's workshop.
func TestReconcile_CrossNamespaceWorkshopChild_LabelMismatch_Denied(t *testing.T) {
	env := testenv.Shared(t)
	spdb := newSpiceDBClient(t)
	ctx := context.Background()

	const (
		wsNamespace     = "ws-xns-mismatch"
		sessNS          = "default"
		trueSessName    = "xns-builder-mismatch-true"
		claimedSessName = "xns-builder-mismatch-claimed"
		parentClassName = "xns-parent-class-mismatch"
		childClassName  = "xns-child-class-mismatch"
	)
	// The workshop namespace is labeled for the TRUE session...
	require.NoError(t, env.Client.Create(ctx, xnsNamespace(wsNamespace, sessNS, trueSessName)), "create workshop namespace")
	require.NoError(t, env.Client.Create(ctx, xnsParentClass(sessNS, parentClassName, childClassName)), "create parent AgentClass in B")
	require.NoError(t, env.Client.Create(ctx, xnsChildClass(wsNamespace, childClassName)), "create child AgentClass in W")
	// ...but the request claims a DIFFERENT session as its parent.
	require.NoError(t, env.Client.Create(ctx, xnsParentSession(sessNS, claimedSessName, parentClassName)), "create the CLAIMED parent AgentSession")
	// The tuple exists for the TRUE session — irrelevant, since the claimed
	// parent must never reach the checker at all.
	require.NoError(t, spdb.EnsureWorkshopSubjects(ctx, wsNamespace, sessNS, trueSessName, identity.CanonicalUserID{}), "seed the tuple for the TRUE session")

	sr := xnsRequest(wsNamespace, "req-mismatch", sessNS, claimedSessName, childClassName)
	require.NoError(t, env.Client.Create(ctx, sr), "create SubagentRequest in W claiming the WRONG parent")

	spy := &spyWorkshopBuildChecker{inner: spdb}
	r := &Reconciler{
		Client:             env.Client,
		Scheme:             env.Scheme,
		Authz:              &fakeAuthz{},
		WorkshopBuild:      spy,
		MaxDelegationDepth: 3,
	}
	xnsReconcile(t, env, r, wsNamespace, "req-mismatch")

	var got v1.SubagentRequest
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: wsNamespace, Name: "req-mismatch"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase)
	assert.Equal(t, "ParentCrossNamespace", got.Status.FailureReason,
		"a namespace labeled for a DIFFERENT session falls back to the pre-existing refusal, not the workshop-specific one")
	assert.Empty(t, got.Status.ChildRef)
	assert.False(t, spy.called, "a claimed parent the namespace's own labels do not name must never reach the WorkshopBuild checker")
}

// TestReconcile_CrossNamespaceWorkshopChild_NilChecker_Denied proves the
// typed-nil rule holds at this seam: with no WorkshopBuildChecker wired at
// all (the zero value of the Reconciler's WorkshopBuild field — a true nil
// interface, per CLAUDE.md's typed-nil rule and the field's own doc), a
// cross-namespace request into an otherwise-genuine, correctly-labeled
// workshop namespace must still be denied, not admitted and not panic. No
// SpiceDB dependency is needed for this test at all — the checker is never
// reached.
func TestReconcile_CrossNamespaceWorkshopChild_NilChecker_Denied(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	const (
		wsNamespace     = "ws-xns-nilchecker"
		sessNS          = "default"
		sessName        = "xns-builder-nilchecker"
		parentClassName = "xns-parent-class-nilchecker"
		childClassName  = "xns-child-class-nilchecker"
	)
	require.NoError(t, env.Client.Create(ctx, xnsNamespace(wsNamespace, sessNS, sessName)), "create workshop namespace")
	require.NoError(t, env.Client.Create(ctx, xnsParentClass(sessNS, parentClassName, childClassName)), "create parent AgentClass in B")
	require.NoError(t, env.Client.Create(ctx, xnsChildClass(wsNamespace, childClassName)), "create child AgentClass in W")
	require.NoError(t, env.Client.Create(ctx, xnsParentSession(sessNS, sessName, parentClassName)), "create parent AgentSession B/X")

	sr := xnsRequest(wsNamespace, "req-nilchecker", sessNS, sessName, childClassName)
	require.NoError(t, env.Client.Create(ctx, sr), "create SubagentRequest in W")

	r := &Reconciler{
		Client:             env.Client,
		Scheme:             env.Scheme,
		Authz:              &fakeAuthz{},
		MaxDelegationDepth: 3,
		// WorkshopBuild deliberately left as the zero value (nil interface).
	}
	assert.NotPanics(t, func() { xnsReconcile(t, env, r, wsNamespace, "req-nilchecker") })

	var got v1.SubagentRequest
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: wsNamespace, Name: "req-nilchecker"}, &got))
	assert.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase)
	assert.Equal(t, ReasonWorkshopBuildDenied, got.Status.FailureReason)
	assert.Empty(t, got.Status.ChildRef, "an unwired WorkshopBuild checker must deny, never admit by default")
}
