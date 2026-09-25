package settingsui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/settingseditor"
)

// secretsGVR is the GVR the fake dynamic client needs to know about to serve
// modeltoken.EnsureSecret's applies.
var secretsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}

// clusterTestScheme mirrors settingswizard/apply_test.go's testScheme: the
// v1alpha1 types plus corev1 (for the Secret objects modeltoken applies).
func clusterTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, v1alpha1.AddToScheme(s))
	return s
}

// newClusterBundle builds a *kube.Bundle whose Controller is a
// controller-runtime fake seeded with seed, and whose Dynamic is a
// client-go dynamic fake that knows the GVRs the cluster-settings handlers
// touch directly (Secrets, via modeltoken.EnsureSecret). It returns the
// Controller client too, since the test-only applyFn stub (see newFakeApply)
// writes through it, not through Dynamic.
func newClusterBundle(t *testing.T, seed ...client.Object) (*kube.Bundle, client.Client) {
	t.Helper()
	// WithReturnManagedFields: by default the fake client strips
	// metadata.managedFields from every read (Get/List/Update all zero it
	// out — see client.go's returnManagedFields gate), which would make
	// managersOf and the drift/take-ownership flow untestable.
	c := fake.NewClientBuilder().WithScheme(clusterTestScheme(t)).WithReturnManagedFields().WithObjects(seed...).Build()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		aptest.ClusterAgentSettingsGVR: "ClusterAgentSettingsList",
		secretsGVR:                     "SecretList",
	})
	return &kube.Bundle{Controller: c, Dynamic: dyn, Namespace: "default"}, c
}

// appliedCall captures one applyFn invocation for assertion.
type appliedCall struct {
	obj          unstructured.Unstructured
	fieldManager string
}

// clusterAgentSettingsAPIVersion is the APIVersion every ManagedFieldsEntry
// this test file constructs must carry — the fake client's
// FieldManagedObjectTracker validates it's non-empty, same as a real
// apiserver would populate it.
const clusterAgentSettingsAPIVersion = "agentprimitives.authzed.com/v1alpha1"

// newFakeApply returns an applyFn stub that writes through the CONTROLLER
// fake client instead of the dynamic client. Two reasons:
//
//  1. The dynamic fake's ApplyPatchType support cannot serve an SSA apply
//     against an ALREADY-EXISTING *unstructured.Unstructured object (see
//     kube/apply_test.go's TestApply and kube.SetApplyUpdateFallbackForTest's
//     doc) — and that fallback switch is package-internal to kube, in a
//     _test.go file, unreachable from here.
//  2. The endpoint's readback goes through the CONTROLLER client
//     (settingswizard.LoadExisting), so an apply that only reached the
//     dynamic fake would be invisible to it regardless.
//
// Unlike a hand-rolled merge, this drives the controller-runtime fake's OWN
// real apply-patch path (client.Apply / types.ApplyPatchType), which is
// backed by a genuine k8s.io/apimachinery/pkg/util/managedfields tracker
// (see client.go's FieldManagedObjectTracker) — so field-level ownership,
// "can't remove a foreign-owned field by omission", and managedFields
// population are the REAL SSA behavior, not an approximation of it. Every
// call is additionally recorded in *calls for flow/purity assertions.
func newFakeApply(t *testing.T, c client.Client, calls *[]appliedCall) func(ctx context.Context, dyn dynamic.Interface, obj *unstructured.Unstructured, fieldManager string) error {
	t.Helper()
	return func(ctx context.Context, dyn dynamic.Interface, obj *unstructured.Unstructured, fieldManager string) error {
		*calls = append(*calls, appliedCall{obj: *obj.DeepCopy(), fieldManager: fieldManager})
		applyObj := obj.DeepCopy()
		return c.Patch(ctx, applyObj, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership)
	}
}

// wireClusterFakes points s's applyFn at newFakeApply, writing through c.
// Returns the call log. The TakeOwnership replacing-Update path needs no
// seam at all: it already goes through the controller client
// (replaceClusterSettings), which in tests IS the fake — with real
// managedFields semantics via its FieldManagedObjectTracker.
func wireClusterFakes(t *testing.T, s *Server, c client.Client) (applyCalls *[]appliedCall) {
	t.Helper()
	applyCalls = &[]appliedCall{}
	s.applyFn = newFakeApply(t, c, applyCalls)
	return applyCalls
}

func doJSON(t *testing.T, cl *http.Client, method, url string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	require.NoError(t, err)
	resp, err := cl.Do(req)
	require.NoError(t, err)
	return resp
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return b
}

// validClusterSpec is a SettingsSpec that passes settingseditor.Validate(_,
// true) cleanly: a single, complete cluster-tier model catalog entry.
func validClusterSpec() *v1alpha1.SettingsSpec {
	return &v1alpha1.SettingsSpec{
		ModelCatalog: &[]v1alpha1.ModelCatalogEntry{{
			Name: "claude-sonnet-5", Provider: "anthropic", Default: true,
			TokenRef: &v1alpha1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "model-default-token", Key: "token"},
		}},
	}
}

// --- cluster-down-200-flag ---

func TestClusterSettingsGet_ClusterDown_ReturnsDownFlag200(t *testing.T) {
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return nil, assert.AnError }})
	c := authedClient(t, s)

	resp := doJSON(t, c, http.MethodGet, "http://"+s.Addr()+"/api/cluster/settings", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got clusterSettingsResponse
	require.NoError(t, json.Unmarshal(readBody(t, resp), &got))
	assert.True(t, got.ClusterDown)
	assert.False(t, got.Found)
	assert.Empty(t, got.Spec)
	assert.Empty(t, got.YAML)
	assert.Empty(t, got.Managers)
}

// --- not-found-empty-spec ---

func TestClusterSettingsGet_NotFound_EmptySpec(t *testing.T) {
	b, _ := newClusterBundle(t) // no seed: singleton doesn't exist
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return b, nil }})
	c := authedClient(t, s)

	resp := doJSON(t, c, http.MethodGet, "http://"+s.Addr()+"/api/cluster/settings", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got clusterSettingsResponse
	require.NoError(t, json.Unmarshal(readBody(t, resp), &got))
	assert.False(t, got.ClusterDown)
	assert.False(t, got.Found)
	assert.JSONEq(t, `{}`, string(got.Spec))
	assert.Empty(t, got.Managers)
}

// --- roundtrip-get-spec-and-yaml ---

func TestClusterSettingsGet_RoundtripSpecAndYAML(t *testing.T) {
	existing := &v1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{
			Name: v1alpha1.ClusterAgentSettingsName,
			ManagedFields: []metav1.ManagedFieldsEntry{
				{Manager: "ap-settings-wizard", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: clusterAgentSettingsAPIVersion, FieldsType: "FieldsV1"},
				{Manager: "kubectl-edit", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: clusterAgentSettingsAPIVersion, FieldsType: "FieldsV1"},
				{Manager: "ap-settings-wizard", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: clusterAgentSettingsAPIVersion, FieldsType: "FieldsV1"}, // duplicate on purpose: dedup check
			},
		},
		Spec: v1alpha1.SettingsSpec{
			Limits: &v1alpha1.SettingsLimits{Budget: &v1alpha1.SettingsBudgetCeiling{MaxTurns: 50}},
		},
	}
	b, _ := newClusterBundle(t, existing)
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return b, nil }})
	c := authedClient(t, s)

	resp := doJSON(t, c, http.MethodGet, "http://"+s.Addr()+"/api/cluster/settings", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got clusterSettingsResponse
	require.NoError(t, json.Unmarshal(readBody(t, resp), &got))
	assert.True(t, got.Found)
	assert.Contains(t, string(got.Spec), `"maxTurns":50`)
	assert.Contains(t, got.YAML, "maxTurns: 50")
	assert.Equal(t, []string{"ap-settings-wizard", "kubectl-edit"}, got.Managers)
}

// --- validate-rejects-bad-spec-422-no-write ---

func TestClusterSettingsPut_InvalidSpec_422_NothingWritten(t *testing.T) {
	b, c := newClusterBundle(t) // no pre-existing singleton
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return b, nil }})
	applyCalls := wireClusterFakes(t, s, c)
	cl := authedClient(t, s)

	// Cluster-tier catalog entry missing tokenRef -> ModelCatalogError fires.
	badSpec := &v1alpha1.SettingsSpec{
		ModelCatalog: &[]v1alpha1.ModelCatalogEntry{{Name: "claude-sonnet-5", Provider: "anthropic"}},
	}
	req := clusterSettingsUpdateRequest{
		Spec:   badSpec,
		Tokens: []tokenWrite{{Namespace: "agentprimitives-system", Name: "model-default-token", Key: "token", Value: "sk-should-not-be-written"}},
	}
	resp := doJSON(t, cl, http.MethodPut, "http://"+s.Addr()+"/api/cluster/settings", req)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)

	var got clusterSettingsUpdateResponse
	require.NoError(t, json.Unmarshal(readBody(t, resp), &got))
	assert.False(t, got.Applied)
	assert.NotEmpty(t, got.Validation.Errors)

	assert.Empty(t, *applyCalls, "apply must not be called when validation fails")
	_, secretErr := b.Dynamic.Resource(secretsGVR).Namespace("agentprimitives-system").Get(context.Background(), "model-default-token", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(secretErr), "token secret must not be written when validation fails")

	var cas v1alpha1.ClusterAgentSettings
	getErr := c.Get(context.Background(), client.ObjectKey{Name: v1alpha1.ClusterAgentSettingsName}, &cas)
	assert.True(t, apierrors.IsNotFound(getErr), "ClusterAgentSettings must not be created when validation fails")
}

// --- put-applies-and-no-drift ---

func TestClusterSettingsPut_Applies_NoDrift(t *testing.T) {
	b, c := newClusterBundle(t) // no pre-existing singleton
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return b, nil }})
	applyCalls := wireClusterFakes(t, s, c)
	cl := authedClient(t, s)

	spec := validClusterSpec()
	req := clusterSettingsUpdateRequest{Spec: spec}
	resp := doJSON(t, cl, http.MethodPut, "http://"+s.Addr()+"/api/cluster/settings", req)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	var got clusterSettingsUpdateResponse
	require.NoError(t, json.Unmarshal(body, &got))
	assert.True(t, got.Applied)
	assert.Empty(t, got.Validation.Errors)
	assert.Empty(t, got.Drift)

	require.Len(t, *applyCalls, 1)
	applied := (*applyCalls)[0]
	assert.Equal(t, clusterSettingsFieldManager, applied.fieldManager)
	assert.Equal(t, "agentprimitives.authzed.com/v1alpha1", applied.obj.Object["apiVersion"])
	assert.Equal(t, "ClusterAgentSettings", applied.obj.Object["kind"])
	_, hasStatus := applied.obj.Object["status"]
	assert.False(t, hasStatus, "status must be stripped before apply")
	_, hasCreationTS, _ := unstructured.NestedFieldNoCopy(applied.obj.Object, "metadata", "creationTimestamp")
	assert.False(t, hasCreationTS, "metadata.creationTimestamp must be stripped before apply")
}

// --- put-writes-token-secret-before-spec ---

func TestClusterSettingsPut_WritesTokenSecretBeforeSpec(t *testing.T) {
	b, c := newClusterBundle(t)
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return b, nil }})
	applyCalls := wireClusterFakes(t, s, c)

	var secretExistedAtApplyTime bool
	baseApply := s.applyFn
	s.applyFn = func(ctx context.Context, dyn dynamic.Interface, obj *unstructured.Unstructured, fieldManager string) error {
		_, getErr := dyn.Resource(secretsGVR).Namespace("agentprimitives-system").Get(ctx, "model-default-token", metav1.GetOptions{})
		secretExistedAtApplyTime = getErr == nil
		return baseApply(ctx, dyn, obj, fieldManager)
	}
	cl := authedClient(t, s)

	req := clusterSettingsUpdateRequest{
		Spec:   validClusterSpec(),
		Tokens: []tokenWrite{{Namespace: "agentprimitives-system", Name: "model-default-token", Key: "token", Value: "sk-abc123"}},
	}
	resp := doJSON(t, cl, http.MethodPut, "http://"+s.Addr()+"/api/cluster/settings", req)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	assert.True(t, secretExistedAtApplyTime, "token secret must exist by the time the spec applies")
	require.Len(t, *applyCalls, 1)

	got, err := b.Dynamic.Resource(secretsGVR).Namespace("agentprimitives-system").Get(context.Background(), "model-default-token", metav1.GetOptions{})
	require.NoError(t, err)
	val, found, err := unstructured.NestedString(got.Object, "stringData", "token")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "sk-abc123", val)
}

// --- put-with-foreign-manager-field-reports-drift ---

// seedForeignOwnedCAS creates the ClusterAgentSettings singleton via a real
// apply-patch under a DIFFERENT field manager ("foreign-tool") — not a plain
// Create — so the fake client's FieldManagedObjectTracker actually attributes
// the Limits field to it. A plain Create/hand-set ManagedFields would be
// ignored: the tracker recomputes managedFields from the FieldManager create
// /update/patch option, not from whatever's on the object's ObjectMeta (see
// newFakeApply's doc).
func seedForeignOwnedCAS(t *testing.T, c client.Client) {
	t.Helper()
	obj := &v1alpha1.ClusterAgentSettings{
		TypeMeta:   metav1.TypeMeta{APIVersion: clusterAgentSettingsAPIVersion, Kind: "ClusterAgentSettings"},
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ClusterAgentSettingsName},
		Spec: v1alpha1.SettingsSpec{
			Limits: &v1alpha1.SettingsLimits{Budget: &v1alpha1.SettingsBudgetCeiling{MaxTurns: 99}},
		},
	}
	require.NoError(t, c.Patch(context.Background(), obj, client.Apply, client.FieldOwner("foreign-tool"), client.ForceOwnership))
}

func TestClusterSettingsPut_ForeignManagerField_ReportsDrift(t *testing.T) {
	b, c := newClusterBundle(t)
	seedForeignOwnedCAS(t, c)
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return b, nil }})
	applyCalls := wireClusterFakes(t, s, c)
	cl := authedClient(t, s)

	// Intended spec omits Limits entirely -> the foreign-owned field cannot
	// be removed by omission (see settingseditor.Drift's doc).
	req := clusterSettingsUpdateRequest{Spec: &v1alpha1.SettingsSpec{}}
	resp := doJSON(t, cl, http.MethodPut, "http://"+s.Addr()+"/api/cluster/settings", req)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	var got clusterSettingsUpdateResponse
	require.NoError(t, json.Unmarshal(body, &got))
	assert.True(t, got.Applied)
	// Exact drift SHAPE (which sibling paths under limits.budget also show up,
	// e.g. metav1.Duration's zero-value marshaling) is settingseditor.Drift's
	// own concern (Task 2, unit-tested there) — this only needs to prove the
	// foreign-owned field survived the apply and got reported.
	assert.NotEmpty(t, got.Drift)
	assert.Contains(t, got.Drift, "limits.budget.maxTurns")

	assert.Len(t, *applyCalls, 1, "no TakeOwnership -> exactly one apply")

	// And the foreign field's VALUE is untouched — an ordinary save never
	// destroys another writer's config.
	var cur v1alpha1.ClusterAgentSettings
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: v1alpha1.ClusterAgentSettingsName}, &cur))
	require.NotNil(t, cur.Spec.Limits)
	assert.EqualValues(t, 99, cur.Spec.Limits.Budget.MaxTurns)
}

// --- take-ownership: spec-replacing update ---

func TestClusterSettingsPut_TakeOwnership_ReplacesSpecAndOwnership(t *testing.T) {
	b, c := newClusterBundle(t)
	seedForeignOwnedCAS(t, c) // foreign-tool owns spec.limits.budget.maxTurns=99
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return b, nil }})
	applyCalls := wireClusterFakes(t, s, c)
	cl := authedClient(t, s)

	// Intended spec has a modelCatalog and NO limits: the replacing update
	// must delete the foreign-kept limits AND land the catalog in one write.
	req := clusterSettingsUpdateRequest{Spec: validClusterSpec(), TakeOwnership: true}
	resp := doJSON(t, cl, http.MethodPut, "http://"+s.Addr()+"/api/cluster/settings", req)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	var got clusterSettingsUpdateResponse
	require.NoError(t, json.Unmarshal(body, &got))
	assert.True(t, got.Applied)
	// The replacing Update deletes the foreign-kept field outright (PUT
	// semantics: the spec becomes exactly the intended spec), so — unlike
	// the SSA path above — readback converges on intended and drift is
	// EMPTY. This runs against the fake's real FieldManagedObjectTracker,
	// not an approximation.
	assert.Empty(t, got.Drift, "replacing update must converge readback on the intended spec")

	assert.Empty(t, *applyCalls, "takeOwnership on an existing CR uses the replacing Update, not the SSA apply")

	// Readback: the foreign-kept field is GONE and the spec is exactly the
	// intended spec.
	var cur v1alpha1.ClusterAgentSettings
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: v1alpha1.ClusterAgentSettingsName}, &cur))
	assert.Nil(t, cur.Spec.Limits, "the foreign-kept limits field must be deleted by the replacing update")
	require.NotNil(t, cur.Spec.ModelCatalog)
	require.Len(t, *cur.Spec.ModelCatalog, 1)
	assert.Equal(t, "claude-sonnet-5", (*cur.Spec.ModelCatalog)[0].Name)

	// NOTE deliberately NOT asserted: foreign-tool's absence from Managers.
	// Its managedFields ENTRY legitimately survives — it still owns fields
	// the replace didn't touch (metadata.name at minimum, since it created
	// the object) — on a real apiserver exactly as with this fake. What
	// take-ownership guarantees is spec-level: no foreign-owned SPEC field
	// survives, which the value assertions above prove.
}

func TestClusterSettingsPut_TakeOwnership_NotFound_FallsBackToApply(t *testing.T) {
	b, c := newClusterBundle(t) // singleton absent: nothing to take ownership OF
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return b, nil }})
	applyCalls := wireClusterFakes(t, s, c)
	cl := authedClient(t, s)

	req := clusterSettingsUpdateRequest{Spec: validClusterSpec(), TakeOwnership: true}
	resp := doJSON(t, cl, http.MethodPut, "http://"+s.Addr()+"/api/cluster/settings", req)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	var got clusterSettingsUpdateResponse
	require.NoError(t, json.Unmarshal(body, &got))
	assert.True(t, got.Applied)
	assert.Empty(t, got.Drift)
	assert.Len(t, *applyCalls, 1, "not-found falls back to the normal SSA apply, which creates the CR")

	var cur v1alpha1.ClusterAgentSettings
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: v1alpha1.ClusterAgentSettingsName}, &cur))
	require.NotNil(t, cur.Spec.ModelCatalog)
}

// --- raw-yaml-multi-doc-400 ---

func TestClusterSettingsPut_RawYAMLMultiDoc_400(t *testing.T) {
	// Deps.Clients would fail the test if ever called: extraction happens
	// before any cluster access, so a malformed YAML body must 400 without
	// ever reaching it.
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) {
		t.Fatal("Clients must not be called for a request that fails extraction")
		return nil, nil
	}})
	cl := authedClient(t, s)

	req := clusterSettingsUpdateRequest{YAML: "limits:\n  budget:\n    maxTurns: 5\n---\nlimits:\n  budget:\n    maxTurns: 10\n"}
	resp := doJSON(t, cl, http.MethodPut, "http://"+s.Addr()+"/api/cluster/settings", req)
	readBody(t, resp)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// --- bonus: PUT/POST-vs-cluster-down and POST /validate purity ---

func TestClusterSettingsPut_ClusterDown_409(t *testing.T) {
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return nil, assert.AnError }})
	cl := authedClient(t, s)

	req := clusterSettingsUpdateRequest{Spec: validClusterSpec()}
	resp := doJSON(t, cl, http.MethodPut, "http://"+s.Addr()+"/api/cluster/settings", req)
	body := readBody(t, resp)
	require.Equal(t, http.StatusConflict, resp.StatusCode, "body: %s", body)
	var got map[string]string
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "cluster not running", got["error"])
}

func TestClusterSettingsValidate_NeverTouchesCluster(t *testing.T) {
	clientsCalled := false
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) {
		clientsCalled = true
		return nil, assert.AnError
	}})
	cl := authedClient(t, s)

	// Valid spec: validate should succeed (200) purely from the request body.
	req := clusterSettingsUpdateRequest{Spec: validClusterSpec()}
	resp := doJSON(t, cl, http.MethodPost, "http://"+s.Addr()+"/api/cluster/settings/validate", req)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var res settingseditor.Result
	require.NoError(t, json.Unmarshal(body, &res))
	assert.Empty(t, res.Errors)
	assert.False(t, clientsCalled, "POST /validate must never call Deps.Clients")

	// Invalid spec: 422, still without touching the cluster.
	badReq := clusterSettingsUpdateRequest{Spec: &v1alpha1.SettingsSpec{
		ModelCatalog: &[]v1alpha1.ModelCatalogEntry{{Name: "x", Provider: "anthropic"}},
	}}
	resp2 := doJSON(t, cl, http.MethodPost, "http://"+s.Addr()+"/api/cluster/settings/validate", badReq)
	body2 := readBody(t, resp2)
	require.Equal(t, http.StatusUnprocessableEntity, resp2.StatusCode, "body: %s", body2)
	var res2 settingseditor.Result
	require.NoError(t, json.Unmarshal(body2, &res2))
	assert.NotEmpty(t, res2.Errors)
	assert.False(t, clientsCalled, "POST /validate must never call Deps.Clients")
}

func TestClusterSettingsUpdateRequest_ExactlyOneOfSpecOrYAML(t *testing.T) {
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) {
		t.Fatal("Clients must not be called for a request that fails extraction")
		return nil, nil
	}})
	cl := authedClient(t, s)

	cases := []struct {
		name string
		req  clusterSettingsUpdateRequest
	}{
		{name: "neither set", req: clusterSettingsUpdateRequest{}},
		{name: "both set", req: clusterSettingsUpdateRequest{Spec: validClusterSpec(), YAML: "limits: {}\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doJSON(t, cl, http.MethodPut, "http://"+s.Addr()+"/api/cluster/settings", tc.req)
			readBody(t, resp)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

// --- error paths: token-write / apply / load failures, malformed body ---

// TestClusterSettingsPut_TokenWriteFails_500_NothingApplied covers the
// contract "token writes happen BEFORE the spec apply; a token-write error →
// 500, spec not applied" — for BOTH write mechanisms, since each has its own
// way of touching the spec (applyFn vs the replacing Update through the
// controller client).
func TestClusterSettingsPut_TokenWriteFails_500_NothingApplied(t *testing.T) {
	cases := []struct {
		name          string
		seed          bool
		takeOwnership bool
		check         func(t *testing.T, c client.Client)
	}{
		{
			name: "SSA path: 500, CR never created",
			check: func(t *testing.T, c client.Client) {
				var cas v1alpha1.ClusterAgentSettings
				err := c.Get(context.Background(), client.ObjectKey{Name: v1alpha1.ClusterAgentSettingsName}, &cas)
				assert.True(t, apierrors.IsNotFound(err), "spec must not be applied after a token-write failure")
			},
		},
		{
			name:          "takeOwnership path: 500, existing spec untouched",
			seed:          true,
			takeOwnership: true,
			check: func(t *testing.T, c client.Client) {
				var cas v1alpha1.ClusterAgentSettings
				require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: v1alpha1.ClusterAgentSettingsName}, &cas))
				require.NotNil(t, cas.Spec.Limits, "replacing update must not run after a token-write failure")
				assert.EqualValues(t, 99, cas.Spec.Limits.Budget.MaxTurns)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, c := newClusterBundle(t)
			if tc.seed {
				seedForeignOwnedCAS(t, c)
			}
			// Fail every Secret operation on the dynamic fake — the verb is
			// "*" because kube.Apply tries an apply-patch first and falls back
			// to Create when the fake reports the object missing.
			b.Dynamic.(*dynfake.FakeDynamicClient).PrependReactor("*", "secrets",
				func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, assert.AnError })
			s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return b, nil }})
			applyCalls := wireClusterFakes(t, s, c)
			cl := authedClient(t, s)

			req := clusterSettingsUpdateRequest{
				Spec:          validClusterSpec(),
				Tokens:        []tokenWrite{{Namespace: "agentprimitives-system", Name: "model-default-token", Key: "token", Value: "sk-abc"}},
				TakeOwnership: tc.takeOwnership,
			}
			resp := doJSON(t, cl, http.MethodPut, "http://"+s.Addr()+"/api/cluster/settings", req)
			body := readBody(t, resp)
			assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, "body: %s", body)
			assert.Contains(t, string(body), "write token secret")
			assert.Empty(t, *applyCalls, "apply must not run after a token-write failure")
			tc.check(t, c)
		})
	}
}

func TestClusterSettingsPut_ApplyFails_500(t *testing.T) {
	b, c := newClusterBundle(t)
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return b, nil }})
	s.applyFn = func(context.Context, dynamic.Interface, *unstructured.Unstructured, string) error {
		return assert.AnError
	}
	cl := authedClient(t, s)

	resp := doJSON(t, cl, http.MethodPut, "http://"+s.Addr()+"/api/cluster/settings",
		clusterSettingsUpdateRequest{Spec: validClusterSpec()})
	body := readBody(t, resp)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, "body: %s", body)
	assert.Contains(t, string(body), "apply cluster settings")

	var cas v1alpha1.ClusterAgentSettings
	err := c.Get(context.Background(), client.ObjectKey{Name: v1alpha1.ClusterAgentSettingsName}, &cas)
	assert.True(t, apierrors.IsNotFound(err), "a failed apply must leave nothing behind")
}

func TestClusterSettingsGet_LoadError_500(t *testing.T) {
	// A non-NotFound read failure (RBAC, timeout, ...) with the cluster UP is
	// an ERROR, unlike the two states GET renders as 200 (down, not-found).
	c := fake.NewClientBuilder().WithScheme(clusterTestScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return assert.AnError
			},
		}).Build()
	b := &kube.Bundle{Controller: c}
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) { return b, nil }})
	cl := authedClient(t, s)

	resp := doJSON(t, cl, http.MethodGet, "http://"+s.Addr()+"/api/cluster/settings", nil)
	body := readBody(t, resp)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, "body: %s", body)
}

func TestClusterSettings_MalformedJSONBody_400(t *testing.T) {
	s := startTestServer(t, Deps{Clients: func() (*kube.Bundle, error) {
		t.Fatal("Clients must not be called for a request that fails decoding")
		return nil, nil
	}})
	cl := authedClient(t, s)

	for _, tc := range []struct{ method, path string }{
		{http.MethodPut, "/api/cluster/settings"},
		{http.MethodPost, "/api/cluster/settings/validate"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, "http://"+s.Addr()+tc.path, strings.NewReader("{not json"))
			require.NoError(t, err)
			resp, err := cl.Do(req)
			require.NoError(t, err)
			readBody(t, resp)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}
