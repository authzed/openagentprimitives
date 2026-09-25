package mcpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/mcpserver"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	mcptest "github.com/authzed/openagentprimitives/pkg/tools/mcp/testing"
)

// newReconciler builds an mcpserver.Reconciler wired to the loopback-
// permitting http.DefaultClient. The mcptest stubs these tests probe are
// httptest servers bound to 127.0.0.1, which the production SSRF-guarded
// probe client (correctly) refuses; production omits HTTP and gets the
// guarded default. A Warn-mode SecretReader backed by the fake client is
// injected so the struct is ready when auth-secret reads are added.
func newReconciler(t *testing.T, c client.Client) *mcpserver.Reconciler {
	t.Helper()
	secretReader := adoptguard.NewSecretReader(c, c, adoptguard.Warn,
		func(types.NamespacedName) bool { return false })
	return &mcpserver.Reconciler{Client: c, HTTP: http.DefaultClient, SecretReader: secretReader}
}

func newMCPServer(url string) *spiceboxv1alpha1.MCPServer {
	return &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "ns", Generation: 1},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    "linear",
			Version: "1",
			Server:  spiceboxv1alpha1.MCPServerServer{URL: url, Transport: "streamable-http"},
			Auth:    spiceboxv1alpha1.MCPServerAuth{},
			Tools: []spiceboxv1alpha1.MCPServerTool{
				{Name: "search_issues", Permission: &authz.Permission{StateImpact: authz.Passthrough}},
			},
		},
	}
}

// buildFakeClientFor creates a fake client seeded with the given MCPServer
// and a status subresource registered for MCPServer.
func buildFakeClientFor(t *testing.T, cr *spiceboxv1alpha1.MCPServer) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithObjects(cr).
		WithStatusSubresource(&spiceboxv1alpha1.MCPServer{}).
		Build()
}

// reconcileLinear runs one Reconcile pass on the "ns/linear" object and
// returns the persisted MCPServer.
func reconcileLinear(t *testing.T, c client.Client, r *mcpserver.Reconciler) spiceboxv1alpha1.MCPServer {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "linear"},
	})
	require.NoError(t, err, "Reconcile")
	var got spiceboxv1alpha1.MCPServer
	require.NoError(t,
		c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "linear"}, &got),
		"Get MCPServer")
	return got
}

func TestReconcile_HappyPath(t *testing.T) {
	srv := mcptest.New(mcptest.Behavior{
		Tools: []mcptest.Tool{
			{Name: "search_issues", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
			{Name: "extra_unused"},
		},
	})
	defer srv.Close()

	cr := newMCPServer(srv.URL)
	c := buildFakeClientFor(t, cr)

	secretReader := adoptguard.NewSecretReader(c, c, adoptguard.Warn,
		func(types.NamespacedName) bool { return false })
	r := &mcpserver.Reconciler{Client: c, RevalidateInterval: 5 * time.Minute, HTTP: http.DefaultClient, SecretReader: secretReader}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "linear"}})
	require.NoError(t, err, "Reconcile")
	assert.Equal(t, 5*time.Minute, res.RequeueAfter, "RequeueAfter should match RevalidateInterval")

	var got spiceboxv1alpha1.MCPServer
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "linear"}, &got),
		"Get MCPServer")
	validCond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.MCPServerConditionValid)
	require.NotNil(t, validCond, "Valid condition")
	assert.Equal(t, metav1.ConditionTrue, validCond.Status, "Valid status")
	reachableCond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.MCPServerConditionReachable)
	require.NotNil(t, reachableCond, "Reachable condition")
	assert.Equal(t, metav1.ConditionTrue, reachableCond.Status, "Reachable status")
	assert.Equal(t, []string{"extra_unused", "search_issues"}, got.Status.ObservedTools, "ObservedTools")
	assert.NotNil(t, got.Status.LastValidatedAt, "LastValidatedAt should be stamped")
	assert.EqualValues(t, 1, got.Status.ObservedGeneration, "ObservedGeneration")
}

// crossSecondBoundary blocks until the wall clock ticks into the next whole
// second. metav1.Time serializes at RFC3339 whole-second granularity, so two
// stamps taken inside the same second are indistinguishable once persisted —
// the re-stamp defect only becomes observable across a boundary. In production
// that is guaranteed for any slow or blackholed endpoint, since the probe
// timeout alone is 10s.
func crossSecondBoundary(t *testing.T) {
	t.Helper()
	next := time.Now().Truncate(time.Second).Add(time.Second + 20*time.Millisecond)
	time.Sleep(time.Until(next))
}

// TestReconcile_UnchangedInputs_DoesNotRestampLastValidatedAt — status.
// lastValidatedAt is an observation, and AGENTS.md §Server-side apply requires
// observations be written with set-on-meaningful-change semantics. conditions.
// Set* is already dedupe-on-equal-state, so a fresh wall-clock stamp is the ONLY
// per-pass churn — and the controller's For(&MCPServer{}) carries no predicate,
// so its own status write re-enqueues it, re-opening a full MCP session (TCP,
// TLS, initialize, tools/list) each time.
func TestReconcile_UnchangedInputs_DoesNotRestampLastValidatedAt(t *testing.T) {
	srv := mcptest.New(mcptest.Behavior{Tools: []mcptest.Tool{{Name: "search_issues"}}})
	defer srv.Close()

	cr := newMCPServer(srv.URL)
	c := buildFakeClientFor(t, cr)
	r := newReconciler(t, c)

	first := reconcileLinear(t, c, r)
	require.NotNil(t, first.Status.LastValidatedAt, "first pass must stamp LastValidatedAt")

	crossSecondBoundary(t)
	second := reconcileLinear(t, c, r)
	require.NotNil(t, second.Status.LastValidatedAt, "LastValidatedAt must not be cleared")
	assert.True(t, first.Status.LastValidatedAt.Equal(second.Status.LastValidatedAt),
		"nothing about the spec or the probe result changed; re-stamping makes every no-op reconcile a real write that re-triggers this controller's own watch")
}

// TestReconcile_ChangedStatus_RestampsLastValidatedAt is the other half of the
// contract: the timestamp must still move when the observed state actually does.
func TestReconcile_ChangedStatus_RestampsLastValidatedAt(t *testing.T) {
	srv := mcptest.New(mcptest.Behavior{Tools: []mcptest.Tool{{Name: "search_issues"}}})
	defer srv.Close()

	cr := newMCPServer(srv.URL)
	c := buildFakeClientFor(t, cr)
	r := newReconciler(t, c)

	first := reconcileLinear(t, c, r)
	require.NotNil(t, first.Status.LastValidatedAt, "first pass must stamp LastValidatedAt")

	crossSecondBoundary(t)

	// The server grows a tool: status.observedTools changes, so this pass
	// observed something new.
	srv.SetBehavior(mcptest.Behavior{Tools: []mcptest.Tool{{Name: "search_issues"}, {Name: "create_issue"}}})
	second := reconcileLinear(t, c, r)
	require.NotNil(t, second.Status.LastValidatedAt, "LastValidatedAt must not be cleared")
	assert.False(t, first.Status.LastValidatedAt.Equal(second.Status.LastValidatedAt),
		"observed tools changed; the validation timestamp must move with it")
}

// TestReconcile_ProbeFailures collapses the three probe-failure cases
// (unreachable URL, 5xx, malformed JSON) into one table-driven sweep.
// All three must yield Reachable=False/ProbeFailed without invalidating
// the spec — Valid stays True because the spec itself is well-formed.
//
// The "shape" each case shares is: build MCPServer pointing at $URL,
// reconcile once, assert (Reachable=False/ProbeFailed, Valid=True). The
// variation is the URL/server-behavior that drives the probe outcome.
func TestReconcile_ProbeFailures(t *testing.T) {
	cases := []struct {
		name      string
		urlForCR  func(t *testing.T) (string, func())
		assertVal func(t *testing.T, cond *metav1.Condition)
	}{
		{
			name: "unreachable URL → Reachable=False/ProbeFailed, Valid=True",
			urlForCR: func(t *testing.T) (string, func()) {
				return "http://127.0.0.1:1", func() {}
			},
		},
		{
			name: "server returns HTTP 500 → Reachable=False/ProbeFailed, Valid=True",
			urlForCR: func(t *testing.T) (string, func()) {
				srv := mcptest.New(mcptest.Behavior{Status: 500})
				return srv.URL, srv.Close
			},
		},
		{
			name: "server returns malformed JSON body → Reachable=False/ProbeFailed",
			urlForCR: func(t *testing.T) (string, func()) {
				srv := mcptest.New(mcptest.Behavior{RawBody: "{not valid json"})
				return srv.URL, srv.Close
			},
			// Skip Valid=True check; the malformed-JSON path historically
			// covered only Reachable, not Valid (parity with prior test).
			assertVal: func(t *testing.T, cond *metav1.Condition) {},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, cleanup := tc.urlForCR(t)
			defer cleanup()
			cr := newMCPServer(url)
			c := buildFakeClientFor(t, cr)
			got := reconcileLinear(t, c, newReconciler(t, c))

			rc := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.MCPServerConditionReachable)
			require.NotNil(t, rc, "Reachable condition")
			assert.Equal(t, metav1.ConditionFalse, rc.Status, "Reachable status")
			assert.Equal(t, spiceboxv1alpha1.ReasonMCPServerProbeFailed, rc.Reason, "Reachable reason")

			vc := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.MCPServerConditionValid)
			if tc.assertVal != nil {
				tc.assertVal(t, vc)
				return
			}
			require.NotNil(t, vc, "Valid condition")
			assert.Equal(t, metav1.ConditionTrue, vc.Status,
				"Valid should be True (spec is well-formed) on probe failure")
		})
	}
}

func TestReconcile_AllowlistDrift(t *testing.T) {
	srv := mcptest.New(mcptest.Behavior{Tools: []mcptest.Tool{{Name: "other_tool"}}})
	defer srv.Close()
	cr := newMCPServer(srv.URL)
	c := buildFakeClientFor(t, cr)

	got := reconcileLinear(t, c, newReconciler(t, c))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.MCPServerConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid status")
	assert.Equal(t, spiceboxv1alpha1.ReasonMCPServerAllowlistDrift, cond.Reason, "Valid reason")

	rcond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.MCPServerConditionReachable)
	require.NotNil(t, rcond, "Reachable condition")
	assert.Equal(t, metav1.ConditionTrue, rcond.Status, "Reachable should be True on drift")
}

func TestReconcile_SpicedbSchemaMissing_InvalidatesMCPServer(t *testing.T) {
	// Tool references a resourceType ("crm_company") via PermissionVariants,
	// but the MCPServer declares no SpiceDBSchema fragment. The cross-ref
	// validator should stamp Valid=False with reason=SpicedbSchemaMissing
	// and a message naming the offending field path.
	cr := newMCPServer("http://127.0.0.1:1") // probe never runs (crossRefCheck short-circuits)
	cr.Spec.Tools[0].PermissionVariants = []authz.PermissionVariant{{
		When: `args.id == "X"`,
		Check: authz.Permission{
			StateImpact: authz.Readonly,
			Check: &authz.PermissionCheck{
				ResourceType:   "crm_company",
				ResourceIDExpr: `"X"`,
				Permission:     "contact_access",
			},
		},
	}}
	c := buildFakeClientFor(t, cr)

	got := reconcileLinear(t, c, newReconciler(t, c))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.MCPServerConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid status")
	assert.Equal(t, spiceboxv1alpha1.ReasonMCPServerSpicedbSchemaMissing, cond.Reason,
		"Valid reason; msg=%q", cond.Message)
	assert.Contains(t, cond.Message, "crm_company",
		"Valid message should name the offending resource")
	assert.Contains(t, cond.Message, "permissionVariants[0]",
		"Valid message should name the offending field path")
}

func TestReconcile_SpicedbSchemaPresent_NoCrossRefError(t *testing.T) {
	// Same tool/resourceType reference as above, but the MCPServer
	// declares the matching SpiceDBSchema fragment. Cross-ref validator
	// must not invalidate. (Use a reachable server so the rest of the
	// chain runs to Valid=True.)
	srv := mcptest.New(mcptest.Behavior{Tools: []mcptest.Tool{{Name: "search_issues"}}})
	defer srv.Close()
	cr := newMCPServer(srv.URL)
	cr.Spec.SpiceDBSchema = &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{{Name: "crm_company"}},
	}
	cr.Spec.Tools[0].PermissionVariants = []authz.PermissionVariant{{
		When: `args.id == "X"`,
		Check: authz.Permission{
			StateImpact: authz.Readonly,
			Check: &authz.PermissionCheck{
				ResourceType:   "crm_company",
				ResourceIDExpr: `"X"`,
				Permission:     "contact_access",
			},
		},
	}}
	c := buildFakeClientFor(t, cr)
	got := reconcileLinear(t, c, newReconciler(t, c))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.MCPServerConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"Valid should be True when resourceType is declared in SpiceDBSchema")
}

func TestReconcile_ConstraintCompileError(t *testing.T) {
	srv := mcptest.New(mcptest.Behavior{Tools: []mcptest.Tool{{Name: "search_issues"}}})
	defer srv.Close()
	cr := newMCPServer(srv.URL)
	cr.Spec.Tools[0].Args.Constraints = []spiceboxv1alpha1.MCPServerConstraint{{
		CEL: "args.q.size( < 100", Message: "broken",
	}}
	c := buildFakeClientFor(t, cr)

	got := reconcileLinear(t, c, newReconciler(t, c))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.MCPServerConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid status")
	assert.Equal(t, spiceboxv1alpha1.ReasonMCPServerConstraintCompileError, cond.Reason, "Valid reason")
}

func TestReconcile_LabelsCompileError(t *testing.T) {
	// Tool declares a labels block whose Name CEL is malformed —
	// admission validator should stamp Valid=False with the new
	// LabelsCompileError reason, naming the offending tool + block.
	cr := newMCPServer("http://127.0.0.1:1")
	cr.Spec.SpiceDBSchema = &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{{Name: "crm_company"}},
	}
	cr.Spec.Tools[0].Labels = []spiceboxv1alpha1.MCPServerLabelExtract{{
		ForEach: `result.results`,
		Label: spiceboxv1alpha1.MCPServerLabelTuple{
			ResourceType: `"crm_company"`,
			ID:           `item.id`,
			Name:         `args.! totally broken`, // syntax error
		},
	}}
	c := buildFakeClientFor(t, cr)

	got := reconcileLinear(t, c, newReconciler(t, c))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.MCPServerConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonMCPServerLabelsCompileError, cond.Reason,
		"reason; msg=%q", cond.Message)
	assert.Contains(t, cond.Message, "labels[0].label.name",
		"message should pinpoint the bad field; got %q", cond.Message)
	assert.Contains(t, cond.Message, cr.Spec.Tools[0].Name)
}

func TestReconcile_LabelsCompileOK(t *testing.T) {
	// Well-formed labels block — admission must NOT trip the new
	// reason.
	srv := mcptest.New(mcptest.Behavior{Tools: []mcptest.Tool{{Name: "search_issues"}}})
	defer srv.Close()
	cr := newMCPServer(srv.URL)
	cr.Spec.SpiceDBSchema = &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{{Name: "crm_company"}},
	}
	cr.Spec.Tools[0].Labels = []spiceboxv1alpha1.MCPServerLabelExtract{{
		ForEach: `result.results`,
		Label: spiceboxv1alpha1.MCPServerLabelTuple{
			ResourceType: `"crm_company"`,
			ID:           `item.properties.hs_object_id`,
			Name:         `item.properties.name`,
		},
	}}
	c := buildFakeClientFor(t, cr)
	got := reconcileLinear(t, c, newReconciler(t, c))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.MCPServerConditionValid)
	require.NotNil(t, cond)
	assert.NotEqual(t, spiceboxv1alpha1.ReasonMCPServerLabelsCompileError, cond.Reason,
		"valid spec should not trip LabelsCompileError; reason=%q msg=%q",
		cond.Reason, cond.Message)
}

func TestSetupWithManager_WatchesAgentIdentity(t *testing.T) {
	// We don't spin up a manager; instead we exercise the enqueue mapper
	// directly via the exported helper. With the new design, any AgentIdentity
	// change re-enqueues ALL MCPServers in the same namespace.
	id := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "bot", Namespace: "ns"},
	}
	cr := newMCPServer("http://x")
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(cr).Build()
	r := &mcpserver.Reconciler{Client: c}

	reqs := r.MapAgentIdentityToMCPServers(context.Background(), id)
	require.Len(t, reqs, 1, "expected one reconcile request: %+v", reqs)
	assert.Equal(t, "linear", reqs[0].Name, "enqueued request Name")
	assert.Equal(t, "ns", reqs[0].Namespace, "enqueued request Namespace")

	// A different identity in the same namespace also re-enqueues all servers.
	other := &spiceboxv1alpha1.AgentIdentity{ObjectMeta: metav1.ObjectMeta{Name: "different", Namespace: "ns"}}
	otherReqs := r.MapAgentIdentityToMCPServers(context.Background(), other)
	assert.Len(t, otherReqs, 1,
		"any AgentIdentity change should enqueue all MCPServers in the namespace")
}

// writesRelationships CEL must be compile-checked at APPLY time, exactly as
// its sibling `labels` surface already is.
//
// It wasn't: these expressions were first compiled during a live tool call, so
// a server carrying an uncompilable expression reported Valid=True and the
// failure surfaced mid-turn as an error the agent could not act on. Observed
// in a live cluster — the agent refused to fetch a company's contacts because
// registering the owner-access relationship failed with
// "undeclared reference to 'spicedb_user_id'".
func TestReconcile_RelationshipsCompileError(t *testing.T) {
	cr := newMCPServer("http://127.0.0.1:1")
	cr.Spec.Tools[0].WritesRelationships = []spiceboxv1alpha1.MCPServerRelationshipWrite{{
		ForEach: `result.results`,
		Tuple: spiceboxv1alpha1.MCPServerRelationshipTuple{
			Resource: `"crm_company:" + item.id`,
			Relation: `"owner"`,
			Subject:  `"user:" + no_such_function(item.email)`, // undeclared function
		},
	}}
	c := buildFakeClientFor(t, cr)

	got := reconcileLinear(t, c, newReconciler(t, c))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.MCPServerConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonMCPServerRelationshipsCompileError, cond.Reason,
		"reason; msg=%q", cond.Message)
	assert.Contains(t, cond.Message, "tuple.subject",
		"message should pinpoint the bad field; got %q", cond.Message)
	assert.Contains(t, cond.Message, cr.Spec.Tools[0].Name)
}

// The canonical form — deriving a subject from an email via spicedb_user_id —
// must VALIDATE. Validation compiles through relwrites' own env, so if that
// env ever loses the function again this fails at apply time instead of
// silently at dispatch time.
func TestReconcile_RelationshipsCompileOK_SpiceDBUserID(t *testing.T) {
	srv := mcptest.New(mcptest.Behavior{Tools: []mcptest.Tool{{Name: "search_issues"}}})
	defer srv.Close()
	cr := newMCPServer(srv.URL)
	cr.Spec.Tools[0].WritesRelationships = []spiceboxv1alpha1.MCPServerRelationshipWrite{{
		ForEach: `result.results`,
		Tuple: spiceboxv1alpha1.MCPServerRelationshipTuple{
			Resource: `"crm_company:" + item.id`,
			Relation: `"owner"`,
			Subject:  `"user:" + spicedb_user_id(item.properties.hs_email)`,
		},
	}}
	c := buildFakeClientFor(t, cr)

	got := reconcileLinear(t, c, newReconciler(t, c))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.MCPServerConditionValid)
	require.NotNil(t, cond)
	assert.NotEqual(t, spiceboxv1alpha1.ReasonMCPServerRelationshipsCompileError, cond.Reason,
		"a spec using spicedb_user_id must validate; msg=%q", cond.Message)
}

// A block that names facts but no subjects must be refused — a fact bound to
// no subject is a session-scoped boolean that would answer for every
// instance at once, which is exactly the laundering `observes` exists to
// prevent. The CRD's MinItems=1 on subjects refuses this shape at the
// apiserver; this test pins the SECOND, redundant refusal here, for a CR
// applied before that schema shipped or through a client that skips
// validation.
func TestReconcile_ObservesCompileError_NoSubjects(t *testing.T) {
	cr := newMCPServer("http://127.0.0.1:1")
	cr.Spec.Tools[0].Observes = []spiceboxv1alpha1.ObservesBlock{{
		Facts: map[string]string{"is_cross_repository": "item.isCrossRepository"},
	}}
	c := buildFakeClientFor(t, cr)

	got := reconcileLinear(t, c, newReconciler(t, c))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.MCPServerConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonMCPServerRelationshipsCompileError, cond.Reason,
		"reason; msg=%q", cond.Message)
	assert.Contains(t, cond.Message, "observes[0]",
		"message should name the offending block; got %q", cond.Message)
	assert.Contains(t, cond.Message, cr.Spec.Tools[0].Name)
	assert.Contains(t, cond.Message, "at least one subject")
}

// The canonical observes shape — one or more subjects, one or more facts,
// both derived from the same `item` — must VALIDATE.
func TestReconcile_ObservesCompileOK(t *testing.T) {
	srv := mcptest.New(mcptest.Behavior{Tools: []mcptest.Tool{{Name: "search_issues"}}})
	defer srv.Close()
	cr := newMCPServer(srv.URL)
	cr.Spec.Tools[0].Observes = []spiceboxv1alpha1.ObservesBlock{{
		ForEach: `result.results`,
		Subjects: []spiceboxv1alpha1.ObserveSubject{
			{ResourceType: `"github_pr"`, ResourceID: `item.id`},
		},
		Facts: map[string]string{"is_cross_repository": "item.isCrossRepository"},
	}}
	c := buildFakeClientFor(t, cr)

	got := reconcileLinear(t, c, newReconciler(t, c))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.MCPServerConditionValid)
	require.NotNil(t, cond)
	assert.NotEqual(t, spiceboxv1alpha1.ReasonMCPServerRelationshipsCompileError, cond.Reason,
		"a well-formed observes block must validate; msg=%q", cond.Message)
}
