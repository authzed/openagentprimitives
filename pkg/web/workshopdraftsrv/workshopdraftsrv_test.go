package workshopdraftsrv_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/web/workshopdraftsrv"
	"github.com/authzed/openagentprimitives/test/oaptest"
)

// fakeBuildChecker records every call so a test can assert the route
// re-checks the LIVE tuple (workshopID, sessNS, sessName) rather than
// trusting the bearer alone.
type fakeBuildChecker struct {
	allow bool
	err   error
	calls []buildCall
}

type buildCall struct{ workshopID, sessNS, sessName string }

func (f *fakeBuildChecker) CheckWorkshopBuild(_ context.Context, workshopID, sessNS, sessName string) (bool, error) {
	f.calls = append(f.calls, buildCall{workshopID, sessNS, sessName})
	return f.allow, f.err
}

const (
	sessNS      = "default"
	sessName    = "builder-x"
	wsNamespace = "ws-abc123456789"
)

func readyWorkshop() *spiceboxv1alpha1.Workshop {
	return &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spiceboxv1alpha1.WorkshopName(sessName),
			Namespace: sessNS,
		},
		Spec: spiceboxv1alpha1.WorkshopSpec{
			Session:        spiceboxv1alpha1.NamespacedRef{Namespace: sessNS, Name: sessName},
			SidecarToolbox: "workshop",
			Limits: spiceboxv1alpha1.WorkshopLimits{
				MaxAge:              metav1.Duration{Duration: time.Hour},
				MaxObjectsPerKind:   10,
				MaxObjects:          50,
				MaxConcurrentProbes: 2,
			},
		},
		Status: spiceboxv1alpha1.WorkshopStatus{
			Namespace: wsNamespace,
			Phase:     spiceboxv1alpha1.WorkshopPhaseReady,
		},
	}
}

func provisioningWorkshop() *spiceboxv1alpha1.Workshop {
	ws := readyWorkshop()
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseProvisioning
	return ws
}

// builderSession is the builder AgentSession the route reads for the UID the
// draft render is owned by. A fixed, made-up UID.
func builderSession() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{
		Name: sessName, Namespace: sessNS, UID: types.UID("0f0e0d0c-0b0a-4908-8706-050403020100"),
	}}
}

// draftBytes packs the shared fixture bundle (agent demo-class, inherited from
// its AgentClass) into the bytes export_draft POSTs.
func draftBytes(t *testing.T) []byte {
	t.Helper()
	b, err := oap.FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err)
	raw, err := oap.Pack(b)
	require.NoError(t, err)
	return raw
}

// bodyText drains a response for an assertion on its error text.
func bodyText(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

// harness bundles the handler under test with the fakes a case inspects
// after the request.
type harness struct {
	srv     *httptest.Server
	c       client.Client
	mem     memory.Memory
	store   *blob.Store
	reg     *tokens.Registry
	checker *fakeBuildChecker
}

func newHarness(t *testing.T, checker *fakeBuildChecker, objs ...client.Object) *harness {
	t.Helper()
	scheme := testfixtures.NewScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.Workshop{}).
		Build()
	mem := memory.NewLocal(inmem.NewBackend())
	store := blob.NewMem()
	reg := tokens.NewRegistry()

	var h harness
	var handler http.Handler
	if checker == nil {
		handler = workshopdraftsrv.NewHandler(c, mem, store, reg, nil)
	} else {
		handler = workshopdraftsrv.NewHandler(c, mem, store, reg, checker)
	}
	h = harness{srv: httptest.NewServer(handler), c: c, mem: mem, store: store, reg: reg, checker: checker}
	t.Cleanup(h.srv.Close)
	return &h
}

// registerBearer registers a workshop bearer keyed the way plan 2's
// AgentSession reconciler does: the SYNTHETIC {sessNS, WorkshopName(sessName)}
// pair — never the builder session {sessNS, sessName} itself.
func (h *harness) registerBearer(token string) {
	h.reg.Set(memory.NamespacedName{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}, token, "")
}

func postDraft(t *testing.T, url, token string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err, "NewRequest")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Do")
	return resp
}

type draftResponse struct {
	ArtifactRef string `json:"artifactRef"`
	Digest      string `json:"digest"`
	Handle      string `json:"handle"`
	ArtifactID  string `json:"artifactId"`
}

func decodeDraftResponse(t *testing.T, resp *http.Response) draftResponse {
	t.Helper()
	var out draftResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out), "decode draftResponse")
	return out
}

func getWorkshop(t *testing.T, c client.Client) *spiceboxv1alpha1.Workshop {
	t.Helper()
	var ws spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}, &ws), "get Workshop")
	return &ws
}

func TestPostDraft_ValidBearerTupleHolds_StoresUnderBuilderSessionAndSetsExport(t *testing.T) {
	checker := &fakeBuildChecker{allow: true}
	h := newHarness(t, checker, readyWorkshop(), builderSession())
	h.registerBearer("tok-workshop")

	body := draftBytes(t)
	resp := postDraft(t, h.srv.URL+workshopdraftsrv.Path, "tok-workshop", body)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "tuple holds → 200")

	out := decodeDraftResponse(t, resp)
	require.NotEmpty(t, out.ArtifactRef, "artifactRef returned")
	require.NotEmpty(t, out.Digest, "digest returned")

	// The route re-checked the LIVE tuple with the values it derived from the
	// resolved Workshop CR — the workshop id (status.namespace) and the
	// BUILDER session (spec.session), not the synthetic bearer key.
	require.Len(t, checker.calls, 1, "CheckWorkshopBuild called exactly once")
	assert.Equal(t, buildCall{wsNamespace, sessNS, sessName}, checker.calls[0])

	// The bytes landed in the artifact store TWICE: once as the draft the
	// install request reads by artifactRef, once as the render's own input
	// (deleted with the render). Order is not guaranteed, so check by key.
	items, _, err := h.store.List(context.Background(), artifactstore.ListOpts{})
	require.NoError(t, err)
	require.Len(t, items, 2, "the draft and the render's own input")
	var sawDraft, sawRenderInput bool
	for _, it := range items {
		if strings.Contains(it.Key, sessNS+"/"+sessName+"/workshop-draft/") {
			sawDraft = true
		}
		if strings.Contains(it.Key, sessNS+"/"+sessName+"/workshop-draft-render/") {
			sawRenderInput = true
		}
	}
	assert.True(t, sawDraft, "the draft is stored under the builder session's own scope")
	assert.True(t, sawRenderInput, "the render's input is stored under the builder session's own scope")

	// Workshop.status.export mirrors the stored ref/digest.
	ws := getWorkshop(t, h.c)
	require.NotNil(t, ws.Status.Export, "status.export set")
	assert.Equal(t, out.ArtifactRef, ws.Status.Export.ArtifactRef)
	assert.Equal(t, out.Digest, ws.Status.Export.Digest)
	assert.False(t, ws.Status.Export.ExportedAt.IsZero())

	assert.True(t, artifacts.IsRenderName(out.Handle), "the handle is a render name the builder can artifact_await: %q", out.Handle)
	assert.Regexp(t, `^artifact-[0-9a-f]{16}$`, out.ArtifactID)

	var cr spiceboxv1alpha1.ArtifactRender
	require.NoError(t, h.c.Get(context.Background(), types.NamespacedName{Namespace: sessNS, Name: out.Handle}, &cr))
	assert.Equal(t, "oap", cr.Spec.Kind)
	// agent.name is inherited from the bundled AgentClass "demo-class".
	assert.Equal(t, "demo-class"+oap.DefaultExtension, cr.Spec.Filename)
	assert.Equal(t, out.ArtifactID, cr.Labels[artifacts.LabelArtifactID])
	assert.Equal(t, "demo-class draft", cr.Annotations[artifacts.AnnoArtifactName])
	assert.Contains(t, cr.Annotations[artifacts.AnnoArtifactDescription], "sha256:"+out.Digest)
	assert.Empty(t, cr.Annotations[artifacts.AnnoInternal], "the draft is the builder's own artifact, listed like any other")
	require.Len(t, cr.OwnerReferences, 1)
	assert.Equal(t, builderSession().UID, cr.OwnerReferences[0].UID)
	assert.NotEmpty(t, cr.Spec.PayloadRef)
	assert.NotEqual(t, out.ArtifactRef, cr.Spec.PayloadRef, "the render's input is its own object: deleting the render must not delete what the install reads")
	rc, err := h.store.Get(context.Background(), artifactstore.Ref(cr.Spec.PayloadRef))
	require.NoError(t, err, "the render's input bytes are stored")
	_ = rc.Close()
}

// TestPostDraft_TupleDoesNotHold_403DeniesAndStoresNothing is the load-bearing
// refusing-direction test: a bearer that resolves to a real, Ready Workshop —
// so LookupInfo alone would let it through — is still denied when the
// workshop#build tuple itself does not hold. The route re-checks the tuple;
// it does not trust the bearer's mere existence.
func TestPostDraft_TupleDoesNotHold_403DeniesAndStoresNothing(t *testing.T) {
	checker := &fakeBuildChecker{allow: false}
	h := newHarness(t, checker, readyWorkshop())
	h.registerBearer("tok-workshop")

	resp := postDraft(t, h.srv.URL+workshopdraftsrv.Path, "tok-workshop", []byte("bytes"))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "tuple false → 403")
	require.Len(t, checker.calls, 1, "the tuple WAS re-checked")

	items, _, err := h.store.List(context.Background(), artifactstore.ListOpts{})
	require.NoError(t, err)
	assert.Empty(t, items, "nothing stored on denial")

	ws := getWorkshop(t, h.c)
	assert.Nil(t, ws.Status.Export, "status.export not set on denial")
}

func TestPostDraft_CheckWorkshopBuildErrors_403DeniesAndStoresNothing(t *testing.T) {
	checker := &fakeBuildChecker{err: errors.New("spicedb unavailable")}
	h := newHarness(t, checker, readyWorkshop())
	h.registerBearer("tok-workshop")

	resp := postDraft(t, h.srv.URL+workshopdraftsrv.Path, "tok-workshop", []byte("bytes"))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "check error is fail-closed, not fail-open")

	items, _, err := h.store.List(context.Background(), artifactstore.ListOpts{})
	require.NoError(t, err)
	assert.Empty(t, items, "nothing stored when the check errors")
}

func TestPostDraft_NilChecker_503DeniesWithoutPanicking(t *testing.T) {
	h := newHarness(t, nil, readyWorkshop())
	h.registerBearer("tok-workshop")

	assert.NotPanics(t, func() {
		resp := postDraft(t, h.srv.URL+workshopdraftsrv.Path, "tok-workshop", []byte("bytes"))
		defer resp.Body.Close()
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "an unwired checker denies, it does not panic")
	})

	items, _, err := h.store.List(context.Background(), artifactstore.ListOpts{})
	require.NoError(t, err)
	assert.Empty(t, items, "nothing stored with no checker wired")
}

func TestPostDraft_UnknownBearer_401(t *testing.T) {
	checker := &fakeBuildChecker{allow: true}
	h := newHarness(t, checker, readyWorkshop())
	// No bearer registered at all.

	resp := postDraft(t, h.srv.URL+workshopdraftsrv.Path, "bogus-token", []byte("bytes"))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Empty(t, checker.calls, "an unauthenticated request never reaches the tuple check")
}

func TestPostDraft_NoBearerHeader_401(t *testing.T) {
	checker := &fakeBuildChecker{allow: true}
	h := newHarness(t, checker, readyWorkshop())

	resp := postDraft(t, h.srv.URL+workshopdraftsrv.Path, "", []byte("bytes"))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestPostDraft_EmptyBody_400(t *testing.T) {
	checker := &fakeBuildChecker{allow: true}
	h := newHarness(t, checker, readyWorkshop())
	h.registerBearer("tok-workshop")

	resp := postDraft(t, h.srv.URL+workshopdraftsrv.Path, "tok-workshop", nil)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestPostDraft_WorkshopNotReady_403 covers a bearer that resolves to a real
// Workshop CR that has not finished provisioning: the sidecar cannot even
// exist yet in this state in production, but a stale/racing bearer must
// still be refused rather than trusted.
func TestPostDraft_WorkshopNotReady_403(t *testing.T) {
	checker := &fakeBuildChecker{allow: true}
	h := newHarness(t, checker, provisioningWorkshop())
	h.registerBearer("tok-workshop")

	resp := postDraft(t, h.srv.URL+workshopdraftsrv.Path, "tok-workshop", []byte("bytes"))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "not-Ready workshop denies")
	assert.Empty(t, checker.calls, "never reaches the tuple check for a not-yet-provisioned workshop")
}

// TestPostDraft_BearerWithNoWorkshopCR_403 covers a registered bearer whose
// synthetic key names no Workshop CR at all (e.g. torn down between token
// registration and this request).
func TestPostDraft_BearerWithNoWorkshopCR_403(t *testing.T) {
	checker := &fakeBuildChecker{allow: true}
	h := newHarness(t, checker) // no Workshop object seeded
	h.registerBearer("tok-workshop")

	resp := postDraft(t, h.srv.URL+workshopdraftsrv.Path, "tok-workshop", []byte("bytes"))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Empty(t, checker.calls)
}

// TestPostDraft_ReExportSameDigest_StatusExportedAtUnchanged pins the
// set-once-per-digest contract: a byte-identical re-export must not churn
// ExportedAt (an SSA-idempotency concern applied to a plain status Update —
// see CLAUDE.md's "keep applied fields idempotent" rule).
func TestPostDraft_ReExportSameDigest_StatusExportedAtUnchanged(t *testing.T) {
	checker := &fakeBuildChecker{allow: true}
	h := newHarness(t, checker, readyWorkshop(), builderSession())
	h.registerBearer("tok-workshop")

	body := draftBytes(t)
	first := postDraft(t, h.srv.URL+workshopdraftsrv.Path, "tok-workshop", body)
	defer first.Body.Close()
	require.Equal(t, http.StatusOK, first.StatusCode)
	firstOut := decodeDraftResponse(t, first)
	wsAfterFirst := getWorkshop(t, h.c)
	require.NotNil(t, wsAfterFirst.Status.Export)
	firstExportedAt := wsAfterFirst.Status.Export.ExportedAt

	second := postDraft(t, h.srv.URL+workshopdraftsrv.Path, "tok-workshop", body)
	defer second.Body.Close()
	require.Equal(t, http.StatusOK, second.StatusCode)
	secondOut := decodeDraftResponse(t, second)
	assert.Equal(t, firstOut.Digest, secondOut.Digest, "identical content ⇒ identical digest")
	assert.Equal(t, firstOut.ArtifactRef, secondOut.ArtifactRef, "identical digest ⇒ same artifactstore key")

	wsAfterSecond := getWorkshop(t, h.c)
	require.NotNil(t, wsAfterSecond.Status.Export)
	assert.True(t, firstExportedAt.Equal(&wsAfterSecond.Status.Export.ExportedAt),
		"re-export of the same digest must not bump ExportedAt")
}

func TestPostDraft_NotABundle_400AndNoRender(t *testing.T) {
	h := newHarness(t, &fakeBuildChecker{allow: true}, readyWorkshop(), builderSession())
	h.registerBearer("tok")
	resp := postDraft(t, h.srv.URL+workshopdraftsrv.Path, "tok", []byte("not a bundle"))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, bodyText(t, resp), "not an agent bundle")
	var list spiceboxv1alpha1.ArtifactRenderList
	require.NoError(t, h.c.List(context.Background(), &list))
	assert.Empty(t, list.Items, "nothing is created for bytes that do not open")
	items, _, err := h.store.List(context.Background(), artifactstore.ListOpts{})
	require.NoError(t, err)
	assert.Empty(t, items, "nothing is stored either: the bundle is opened before the first write")
}

func TestPostDraft_BuilderSessionMissing_500AndNoRender(t *testing.T) {
	h := newHarness(t, &fakeBuildChecker{allow: true}, readyWorkshop()) // no AgentSession
	h.registerBearer("tok")
	resp := postDraft(t, h.srv.URL+workshopdraftsrv.Path, "tok", draftBytes(t))
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(t, bodyText(t, resp), "builder session")
	var list spiceboxv1alpha1.ArtifactRenderList
	require.NoError(t, h.c.List(context.Background(), &list))
	assert.Empty(t, list.Items)

	items, _, err := h.store.List(context.Background(), artifactstore.ListOpts{})
	require.NoError(t, err)
	assert.Empty(t, items, "nothing is stored when the session that would own the render is missing")
}

func TestPostDraft_ReExport_CreatesAFreshRenderEachTime(t *testing.T) {
	// The store key is digest-keyed (one copy of the bytes), but each export is
	// its own artifact: the builder asks for the draft again and gets a new
	// handle to await, never a stale one.
	h := newHarness(t, &fakeBuildChecker{allow: true}, readyWorkshop(), builderSession())
	h.registerBearer("tok")
	raw := draftBytes(t)
	first := postDraft(t, h.srv.URL+workshopdraftsrv.Path, "tok", raw)
	defer first.Body.Close()
	second := postDraft(t, h.srv.URL+workshopdraftsrv.Path, "tok", raw)
	defer second.Body.Close()
	require.Equal(t, http.StatusOK, first.StatusCode)
	require.Equal(t, http.StatusOK, second.StatusCode)
	a, b := decodeDraftResponse(t, first), decodeDraftResponse(t, second)
	assert.NotEqual(t, a.Handle, b.Handle)
	assert.NotEqual(t, a.ArtifactID, b.ArtifactID)
	assert.Equal(t, a.ArtifactRef, b.ArtifactRef, "the draft bytes stay digest-keyed")

	var crA, crB spiceboxv1alpha1.ArtifactRender
	require.NoError(t, h.c.Get(context.Background(), types.NamespacedName{Namespace: sessNS, Name: a.Handle}, &crA))
	require.NoError(t, h.c.Get(context.Background(), types.NamespacedName{Namespace: sessNS, Name: b.Handle}, &crB))
	assert.NotEqual(t, crA.Spec.PayloadRef, crB.Spec.PayloadRef, "each export's render input is its own object, keyed by that render's artifact id")
}
