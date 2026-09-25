package httpsrv_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
)

func newArtifactScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = spiceboxv1alpha1.AddToScheme(s)
	return s
}

func putBlob(t *testing.T, store artifactstore.Store, key string, payload []byte) artifactstore.Ref {
	t.Helper()
	ref, err := store.Put(context.Background(), key, strings.NewReader(string(payload)))
	require.NoError(t, err, "artifactstore.Put")
	return ref
}

// readyCR builds an ArtifactRender in Phase=Ready owned by the given session.
func readyCR(ns, name, sess string, ref artifactstore.Ref) *spiceboxv1alpha1.ArtifactRender {
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
		},
		Spec: spiceboxv1alpha1.ArtifactRenderSpec{Kind: "html"},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase:          spiceboxv1alpha1.ArtifactRenderPhaseReady,
			OutputRef:      string(ref),
			OutputMIME:     "text/html; charset=utf-8",
			OutputSize:     0,
			OutputFilename: "report.html",
		},
	}
	if sess != "" {
		cr.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: spiceboxv1alpha1.SchemeGroupVersion.String(),
			Kind:       "AgentSession",
			Name:       sess,
			UID:        "uid-1",
		}}
	}
	return cr
}

// newArtifactServer wires up a handler with a CR (optional) and a
// blob (optional) and returns the httptest server, ready to receive
// GETs. The system token is "sys".
func newArtifactServer(t *testing.T, cr *spiceboxv1alpha1.ArtifactRender) *httptest.Server {
	t.Helper()
	scheme := newArtifactScheme(t)
	astore := blobstore.NewMem()
	if cr != nil && cr.Status.OutputRef != "" {
		// CR status already references a key; populate the store with it
		// for happy-path tests. Tests that exercise 404 on missing CR
		// don't pass a CR at all.
		_ = putBlob(t, astore, cr.Status.OutputRef, []byte("data"))
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	if cr != nil {
		builder = builder.WithObjects(cr)
	}
	c := builder.Build()
	mem := memory.NewLocal(memoryinmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("sys")
	h := httpsrv.NewHandler(mem, reg, httpsrv.WithArtifact(c, astore))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestArtifactRoute_HappyPath(t *testing.T) {
	scheme := newArtifactScheme(t)
	astore := blobstore.NewMem()
	payload := []byte("<html><body>hi</body></html>")
	ref := putBlob(t, astore, "ns1/sess1/render-abc/output", payload)

	cr := readyCR("ns1", "render-abc", "sess1", ref)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()
	mem := memory.NewLocal(memoryinmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("sys")

	h := httpsrv.NewHandler(mem, reg, httpsrv.WithArtifact(c, astore))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/artifact/ns1/sess1/render-abc/output", nil)
	require.NoError(t, err, "NewRequest")
	req.Header.Set("Authorization", "Bearer sys")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Do")
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode, "happy path: 200")
	assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"), "Content-Type echoed")
	assert.Contains(t, resp.Header.Get("Content-Disposition"), `filename="report.html"`, "Content-Disposition filename")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "ReadAll body")
	assert.Equal(t, payload, body, "body bytes")
}

// TestArtifactRoute_WebdTokenAccepted proves webd's read-only system token is
// accepted on the artifact-bytes GET — that's webd's whole job (serving the
// browser artifact view), and the route is a read.
func TestArtifactRoute_WebdTokenAccepted(t *testing.T) {
	scheme := newArtifactScheme(t)
	astore := blobstore.NewMem()
	payload := []byte("<html><body>hi</body></html>")
	ref := putBlob(t, astore, "ns1/sess1/render-abc/output", payload)
	cr := readyCR("ns1", "render-abc", "sess1", ref)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()
	mem := memory.NewLocal(memoryinmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.SetWebdToken("webd") // NOT the channelsd token

	h := httpsrv.NewHandler(mem, reg, httpsrv.WithArtifact(c, astore))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/artifact/ns1/sess1/render-abc/output", nil)
	require.NoError(t, err, "NewRequest")
	req.Header.Set("Authorization", "Bearer webd")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Do")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "webd read-only token must be accepted on the artifact read")
}

// TestArtifactRoute_UnsafeMethodWithWebdToken_403 pins checkSystemBearer's
// method backstop. /artifact declares readAccess and separately answers 405 to
// anything but GET, so this case is doubly covered today — which is the point:
// the read-only token is refused at the BEARER check on an unsafe method,
// before the route's own method check, so a future read-declaring route that
// forgets its 405 still cannot be written through with webd's credential. The
// channelsd row is the control: it is not read-only, so it reaches the 405.
func TestArtifactRoute_UnsafeMethodWithWebdToken_403(t *testing.T) {
	scheme := newArtifactScheme(t)
	astore := blobstore.NewMem()
	ref := putBlob(t, astore, "ns1/sess1/render-abc/output", []byte("<html></html>"))
	cr := readyCR("ns1", "render-abc", "sess1", ref)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()
	mem := memory.NewLocal(memoryinmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("sys")
	reg.SetWebdToken("webd")

	h := httpsrv.NewHandler(mem, reg, httpsrv.WithArtifact(c, astore))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	cases := []struct {
		name       string
		bearer     string
		wantStatus int
	}{
		{
			name:       "webd token on POST: 403 at the bearer check, never reaching the route's 405",
			bearer:     "webd",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "channelsd token on POST: authorized to write, so the route's own 405 answers",
			bearer:     "sys",
			wantStatus: http.StatusMethodNotAllowed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/artifact/ns1/sess1/render-abc/output", nil)
			require.NoError(t, err, "NewRequest")
			req.Header.Set("Authorization", "Bearer "+tc.bearer)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err, "Do")
			defer resp.Body.Close()
			assert.Equal(t, tc.wantStatus, resp.StatusCode)
		})
	}
}

// TestArtifactRoute_ErrorCases covers the three non-2xx paths
// (unknown CR → 404, mismatched session → 403, no auth → 401). Each
// row tweaks one variable while sharing the rest of the fixture.
func TestArtifactRoute_ErrorCases(t *testing.T) {
	const sysToken = "sys"
	ref := artifactstore.Ref("ns1/sessA/render-x/output")
	ownedCR := readyCR("ns1", "render-x", "sessA", ref)

	cases := []struct {
		name       string
		cr         *spiceboxv1alpha1.ArtifactRender // installed in fake client; nil means none
		path       string
		authHeader string // empty means no Authorization header set
		wantStatus int
	}{
		{
			name:       "unknown CR returns 404",
			cr:         nil,
			path:       "/artifact/ns1/sess1/missing/output",
			authHeader: "Bearer " + sysToken,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "non-owning session returns 403",
			cr:         ownedCR,
			path:       "/artifact/ns1/sessB/render-x/output",
			authHeader: "Bearer " + sysToken,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "missing token returns 401",
			cr:         ownedCR,
			path:       "/artifact/ns1/sessA/render-x/output",
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newArtifactServer(t, tc.cr)
			req, err := http.NewRequest(http.MethodGet, srv.URL+tc.path, nil)
			require.NoError(t, err, "NewRequest")
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err, "Do")
			defer resp.Body.Close()
			assert.Equal(t, tc.wantStatus, resp.StatusCode, "status code")
		})
	}
}

// TestArtifactRoute_PerSessionTokenScoping pins the artifact read route's
// per-session-token branch, in both directions.
//
// The route's real callers hold the per-session memory token minted into the
// <session>-memory-token Secret, not a component credential: `oap artifact get`
// reads it through memclient, and the runner passes it to fetchRenderBytes when
// artifact_offer_view builds a preview. Admitting that token is what makes
// those two work at all.
//
// The cross-session rows are the load-bearing half — they prove the branch is
// SCOPED to the {ns}/{sess} spelled in the URL rather than open to any live
// session token. Every token in the table is REGISTERED, so a 401 on one of
// those rows can only come from the scope check; an unregistered token would
// 401 as well and would prove nothing.
func TestArtifactRoute_PerSessionTokenScoping(t *testing.T) {
	scheme := newArtifactScheme(t)
	astore := blobstore.NewMem()
	payloadA := []byte("<html><body>A</body></html>")
	payloadB := []byte("<html><body>B</body></html>")
	refA := putBlob(t, astore, "ns1/sessA/render-a/output", payloadA)
	refB := putBlob(t, astore, "ns1/sessB/render-b/output", payloadB)
	crA := readyCR("ns1", "render-a", "sessA", refA)
	crB := readyCR("ns1", "render-b", "sessB", refB)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(crA, crB).Build()
	mem := memory.NewLocal(memoryinmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("sys")
	// tok-a authorizes sessA only. tok-b authorizes sessB and carries sessA
	// among its read-only extras — the shape the operator registers for a
	// runner whose ToolCalls ran against per-bundle sessions.
	reg.Set(memory.NamespacedName{Namespace: "ns1", Name: "sessA"}, "tok-a", "")
	reg.Set(memory.NamespacedName{Namespace: "ns1", Name: "sessB"}, "tok-b", "",
		memory.NamespacedName{Namespace: "ns1", Name: "sessA"})

	h := httpsrv.NewHandler(mem, reg, httpsrv.WithArtifact(c, astore))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	cases := []struct {
		name       string
		bearer     string
		path       string
		wantStatus int
		wantBody   []byte // nil means "don't inspect the body"
	}{
		{
			name:       "token registered for the URL's session: 200 with that render's bytes",
			bearer:     "tok-a",
			path:       "/artifact/ns1/sessA/render-a/output",
			wantStatus: http.StatusOK,
			wantBody:   payloadA,
		},
		{
			name:       "same live token against a DIFFERENT session: 401 (the row above is its control)",
			bearer:     "tok-a",
			path:       "/artifact/ns1/sessB/render-b/output",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "token whose registered read extras cover the URL's session: 200",
			bearer:     "tok-b",
			path:       "/artifact/ns1/sessA/render-a/output",
			wantStatus: http.StatusOK,
			wantBody:   payloadA,
		},
		{
			name:       "token registered for the URL's session, second session: 200",
			bearer:     "tok-b",
			path:       "/artifact/ns1/sessB/render-b/output",
			wantStatus: http.StatusOK,
			wantBody:   payloadB,
		},
		{
			name:       "authorized session but a render another session owns: 403 from the ownership check",
			bearer:     "tok-a",
			path:       "/artifact/ns1/sessA/render-b/output",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "token in no registry entry at all: 401",
			bearer:     "tok-unregistered",
			path:       "/artifact/ns1/sessA/render-a/output",
			wantStatus: http.StatusUnauthorized,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, srv.URL+tc.path, nil)
			require.NoError(t, err, "NewRequest")
			req.Header.Set("Authorization", "Bearer "+tc.bearer)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err, "Do")
			defer resp.Body.Close()

			assert.Equal(t, tc.wantStatus, resp.StatusCode, "status code")
			if tc.wantBody != nil {
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err, "ReadAll body")
				assert.Equal(t, tc.wantBody, body, "body bytes")
			}
		})
	}
}
