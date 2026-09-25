package workshopmcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// newApplyTestServer builds a Server around c, scoped to the workshop
// namespace ns.
func newApplyTestServer(ns string, c client.Client) *Server {
	return &Server{
		K8s: c,
		Identity: WorkshopIdentity{
			Namespace:        ns,
			SessionNamespace: "b",
			SessionName:      "x",
			WorkshopID:       ns,
		},
		FieldOwner: defaultFieldOwner,
	}
}

// shrinkApplyPollForTest lowers applyCR's poll interval/timeout so a test
// exercising the poll loop itself doesn't have to wait out the real 30s
// window, and restores the production values on cleanup.
func shrinkApplyPollForTest(t *testing.T) {
	t.Helper()
	origInterval, origTimeout := applyPollInterval, applyPollTimeout
	applyPollInterval = 5 * time.Millisecond
	applyPollTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		applyPollInterval, applyPollTimeout = origInterval, origTimeout
	})
}

// candidateManifest builds a minimal, well-formed MCPServer candidate
// manifest map, the shape workshop_apply's own args carry a CR in.
func candidateManifest(namespace, name, intent string) map[string]any {
	return map[string]any{
		"apiVersion": spiceboxv1alpha1.SchemeGroupVersion.String(),
		"kind":       "MCPServer",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{
			"name":    name,
			"version": "v1",
			"intent":  intent,
			"server":  map[string]any{"url": "https://example.test/mcp", "transport": "streamable-http"},
		},
	}
}

func callApply(t *testing.T, s *Server, manifest map[string]any) *mcp.CallToolResult {
	t.Helper()
	raw, err := json.Marshal(applyArgs{Manifest: manifest})
	require.NoError(t, err)
	res, err := s.handleApply(context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Arguments: raw},
	})
	require.NoError(t, err, "handleApply must not return a protocol-level error")
	return res
}

func decodeApplyResult(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok, "result content block must be text")
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(text.Text), &body), "result body must be valid JSON")
	return body
}

// TestHandleApply_SSAAppliesAndReturnsConditions seeds an already-reconciled
// MCPServer (status.conditions already carries Valid=True, as a real
// controller would have set it on a prior reconcile) and re-applies it with
// a changed intent. SSA touches only spec/metadata — status is a different
// field manager's territory — so applyCR's very first Get already finds the
// stamped condition and returns without ever needing to wait on the poll
// loop. Asserts BOTH that the object exists with the new spec AND that the
// result carries its conditions.
func TestHandleApply_SSAAppliesAndReturnsConditions(t *testing.T) {
	ns, name := "ws-demo123", "demo-tool"
	existing := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: name, Version: "v1", Intent: "original intent",
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://example.test/mcp", Transport: "streamable-http"},
		},
		Status: spiceboxv1alpha1.MCPServerStatus{
			Conditions: []metav1.Condition{
				{Type: conditionTypeValid, Status: metav1.ConditionTrue, Reason: "Valid", Message: "ok", LastTransitionTime: metav1.Now()},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(existing).Build()
	s := newApplyTestServer(ns, c)

	res := callApply(t, s, candidateManifest(ns, name, "updated intent"))
	require.False(t, res.IsError, "a clean apply must not return an error result")
	body := decodeApplyResult(t, res)

	assert.Equal(t, "MCPServer", body["kind"])
	assert.Equal(t, name, body["name"])
	assert.Equal(t, ns, body["namespace"])
	conds, ok := body["conditions"].([]any)
	require.True(t, ok, "conditions must be a list")
	require.NotEmpty(t, conds, "workshop_apply must return the object's conditions")
	first, ok := conds[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, conditionTypeValid, first["type"])

	var applied spiceboxv1alpha1.MCPServer
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &applied))
	assert.Equal(t, "updated intent", applied.Spec.Intent, "the SSA apply must actually have landed")
}

// TestHandleApply_RefusesAdmissionDenial_SurfacesVerbatim is the REFUSING
// test: a fake apiserver that denies the Patch call (an admission-webhook
// style Forbidden) must come back as {denied:true, message:<verbatim>} —
// never reworded, and never claimed as a partial success (the object must
// not exist afterward).
func TestHandleApply_RefusesAdmissionDenial_SurfacesVerbatim(t *testing.T) {
	ns, name := "ws-demo123", "demo-tool"
	denyErr := apierrors.NewForbidden(
		schema.GroupResource{Group: spiceboxv1alpha1.GroupName, Resource: "mcpservers"},
		name, errors.New("admission webhook \"validate.mcpserver.agentprimitives.authzed.com\" denied the request: workshop policy forbids this"))

	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				return denyErr
			},
		}).Build()
	s := newApplyTestServer(ns, c)

	res := callApply(t, s, candidateManifest(ns, name, "some intent"))
	require.True(t, res.IsError, "a denied apply must be an error result")
	body := decodeApplyResult(t, res)
	assert.Equal(t, true, body["denied"], "denial must be flagged, not silently retried or absorbed")
	assert.Equal(t, denyErr.Error(), body["message"], "the apiserver's own denial text must be surfaced VERBATIM, never reworded")
	_, hasKind := body["kind"]
	assert.False(t, hasKind, "a denied apply must never carry apply-succeeded fields")

	var check spiceboxv1alpha1.MCPServer
	err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &check)
	assert.True(t, apierrors.IsNotFound(err), "a denied apply must leave no object behind — no partial success")
}

// TestHandleApply_TimesOutWithoutConditions_StillReportsSuccess proves the
// poll-timeout path: when no controller ever stamps a condition, applyCR
// returns the object as it stands (conditions empty) rather than hanging or
// turning a successful SSA into a reported failure.
func TestHandleApply_TimesOutWithoutConditions_StillReportsSuccess(t *testing.T) {
	shrinkApplyPollForTest(t)
	ns, name := "ws-demo123", "demo-tool-unready"
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
	s := newApplyTestServer(ns, c)

	start := time.Now()
	res := callApply(t, s, candidateManifest(ns, name, "some intent"))
	elapsed := time.Since(start)

	require.False(t, res.IsError, "a successful apply with no controller yet must not be reported as a failure")
	assert.Less(t, elapsed, 2*time.Second, "applyCR must respect the shrunk poll timeout, not the real 30s default")
	body := decodeApplyResult(t, res)
	conds, ok := body["conditions"].([]any)
	require.True(t, ok)
	assert.Empty(t, conds, "no controller ever stamped a condition, so conditions must be empty, not fabricated")
}

// TestHandleApply_RejectsMismatchedNamespace pins the fail-closed namespace
// guard: a manifest naming a namespace other than the workshop's own is
// refused before anything is ever applied, rather than silently redirected
// or silently accepted.
func TestHandleApply_RejectsMismatchedNamespace(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
	s := newApplyTestServer("ws-demo123", c)

	res := callApply(t, s, candidateManifest("ws-someone-else", "demo-tool", "intent"))
	require.True(t, res.IsError)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, text.Text, "ws-someone-else")
}

// The live wall on oap-desktop 2026-09-13: the builder has no way to learn the
// platform's API group, so it guessed — "oap.dev/v1", then "v1" — and each
// guess cost the person an approval card before the apiserver answered "no
// matches for kind". The kind is the builder's whole contract; the group and
// version are stamped from it, whatever the manifest carried.
func TestHandleApply_FillsGroupVersionFromKind(t *testing.T) {
	shrinkApplyPollForTest(t)
	for _, tc := range []struct {
		name       string
		apiVersion any // nil = absent
	}{
		{name: "apiVersion absent: applied under the platform group", apiVersion: nil},
		{name: "apiVersion guessed wrong (oap.dev/v1): overwritten, applied", apiVersion: "oap.dev/v1"},
		{name: "apiVersion guessed bare (v1): overwritten, applied", apiVersion: "v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, name := "ws-demo123", "demo-tool"
			c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
			s := newApplyTestServer(ns, c)
			m := candidateManifest(ns, name, "guessed group")
			delete(m, "apiVersion")
			if tc.apiVersion != nil {
				m["apiVersion"] = tc.apiVersion
			}
			res := callApply(t, s, m)
			require.False(t, res.IsError, "the kind alone must be enough to apply: %v", res.Content)
			var applied spiceboxv1alpha1.MCPServer
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &applied),
				"the object must land as a typed MCPServer under the platform group")
			assert.Equal(t, "guessed group", applied.Spec.Intent)
		})
	}
}

// A kind the workshop cannot author is refused by NAME, with the kinds it
// can, before anything reaches the apiserver — the same answer list/get/delete
// give, instead of the apiserver's "no matches for kind" that told the builder
// nothing about what it could have asked for.
func TestHandleApply_RefusesKindOutsideTheWorkshop(t *testing.T) {
	ns := "ws-demo123"
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
	s := newApplyTestServer(ns, c)
	res := callApply(t, s, map[string]any{
		"kind":     "Secret",
		"metadata": map[string]any{"name": "github-token"},
		"spec":     map[string]any{},
	})
	require.True(t, res.IsError, "an unsupported kind must be an error result")
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, text.Text, "Secret")
	for _, k := range workshopKindNames() {
		assert.Contains(t, text.Text, k, "the refusal must name every kind the workshop can apply")
	}
}
