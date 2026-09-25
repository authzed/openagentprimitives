package httpsrv_test

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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// fakeExtractor is a test double for httpsrv.AttachmentExtractor. behavior
// is set per test.
type fakeExtractor struct {
	text  string
	pages int
	err   error

	calls   int
	gotMIME string
	gotBody string
}

func (f *fakeExtractor) Extract(_ context.Context, mime string, body io.Reader) (string, int, error) {
	f.calls++
	f.gotMIME = mime
	b, _ := io.ReadAll(body)
	f.gotBody = string(b)
	if f.err != nil {
		return "", 0, f.err
	}
	return f.text, f.pages, nil
}

// newInboundAssetServer wires a handler with a fresh in-memory blob store
// (and, optionally, an AttachmentExtractor) and returns the httptest server
// plus the store, so a test can assert directly against store contents (not
// just HTTP status codes). The system token is "sys".
func newInboundAssetServer(t *testing.T, extractor httpsrv.AttachmentExtractor) (*httptest.Server, artifactstore.Store) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = spiceboxv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	astore := blobstore.NewMem()
	mem := memory.NewLocal(memoryinmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("sys")

	opts := []httpsrv.HandlerOption{httpsrv.WithArtifact(c, astore)}
	if extractor != nil {
		opts = append(opts, httpsrv.WithAttachmentExtractor(extractor))
	}
	h := httpsrv.NewHandler(mem, reg, opts...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, astore
}

func doUpload(t *testing.T, srv *httptest.Server, bearer, mime, filename string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/inbound-asset/ns1/sess1", bytes.NewReader(body))
	require.NoError(t, err, "NewRequest")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if mime != "" {
		req.Header.Set("Content-Type", mime)
	}
	if filename != "" {
		req.Header.Set("X-Attachment-Filename", filename)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Do")
	return resp
}

func decodeResponse(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out), "decode response")
	return out
}

// storeItemCount lists every item currently in store — used to assert "the
// store is untouched", not merely that a request returned the right status.
func storeItemCount(t *testing.T, store artifactstore.Store) int {
	t.Helper()
	items, _, err := store.List(context.Background(), artifactstore.ListOpts{})
	require.NoError(t, err, "List")
	return len(items)
}

func TestInboundAssetRoute_HappyPath_NoExtractorConfigured(t *testing.T) {
	srv, astore := newInboundAssetServer(t, nil)
	payload := []byte("%PDF-1.4 pretend pdf bytes")

	resp := doUpload(t, srv, "sys", "application/pdf", "report.pdf", payload)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	out := decodeResponse(t, resp)

	ref, _ := out["ref"].(string)
	require.NotEmpty(t, ref, "ref must be set even with no extractor configured")
	assert.Equal(t, false, out["extracted"], "no extractor configured ⇒ Extracted=false")
	assert.Nil(t, out["unsupported"], "no extractor configured must never claim unsupported")

	rc, err := astore.Get(context.Background(), artifactstore.Ref(ref))
	require.NoError(t, err, "Get stored bytes")
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, payload, got, "the exact uploaded bytes must be retrievable")
}

// TestInboundAssetRoute_FilenameWithPathTraversal_CannotEscapeSessionPrefix
// proves a forged X-Attachment-Filename (e.g. "../../../other-ns/other-sess/evil")
// cannot make the artifactstore key resolve outside the
// {ns}/{sess}/inbound-asset/{assetID}/ prefix — path.Join cleans ".."
// segments against the WHOLE joined path, so an unsanitized filename
// component could otherwise let one session's channelsd token write into (or
// collide with) another session's key space.
func TestInboundAssetRoute_FilenameWithPathTraversal_CannotEscapeSessionPrefix(t *testing.T) {
	srv, astore := newInboundAssetServer(t, nil)
	resp := doUpload(t, srv, "sys", "text/plain", "../../../../other-ns/other-sess/evil", []byte("payload"))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	out := decodeResponse(t, resp)

	ref, _ := out["ref"].(string)
	require.NotEmpty(t, ref)
	key, err := astore.Key(artifactstore.Ref(ref))
	require.NoError(t, err, "Key")
	// The key must stay rooted under this request's own ns/sess prefix — the
	// flattened filename segment may still contain the literal text
	// "other-ns" (harmless: it is no longer a "/"-delimited path component,
	// just underscored display text), so the meaningful assertion is the
	// prefix, not a substring exclusion. ns/sess lead the key (not
	// "inbound-asset") so refSessionKey (pkg/x/debug) recovers the right
	// session for fetch_artifact's per-session-token auth — see
	// pkg/x/debug.TestRefSessionKeyRecoversInboundAssetKey for that half.
	assert.True(t, strings.HasPrefix(key, "ns1/sess1/inbound-asset/"),
		"the stored key must stay rooted under this request's own ns/sess prefix, got %q", key)
	assert.Equal(t, 4, strings.Count(key, "/"),
		"exactly ns1/sess1/inbound-asset/{assetID}/{filename} — no extra path segment introduced by the filename")
}

// TestInboundAssetRoute_EncodedSlashInSess_Rejected proves a %2f-encoded
// {sess} segment — which http.ServeMux routes on the CLEANED path but this
// handler reads via the DECODED r.URL.Path, where %2f has already become a
// literal "/" — cannot smuggle extra path segments through the {ns}/{sess}
// split and escape into a foreign key space. This is the same trust
// boundary and severity as the filename vector above; a forged
// X-Attachment-Filename is not the only place attacker-controlled text
// reaches path.Join.
func TestInboundAssetRoute_EncodedSlashInSess_Rejected(t *testing.T) {
	srv, astore := newInboundAssetServer(t, nil)

	req, err := http.NewRequest(http.MethodPost,
		srv.URL+"/inbound-asset/ns1/sess1%2f..%2f..%2f..%2fartifact%2fvictim-ns%2fvictim-sess",
		bytes.NewReader([]byte("payload")))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer sys")
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"a {sess} that decodes to something containing \"/\" must be rejected as an invalid name, not silently accepted")
	assert.Equal(t, 0, storeItemCount(t, astore), "a rejected upload must write nothing to the store")
}

// TestInboundAssetRoute_NilPointerExtractor_TreatedAsAbsent proves the
// typed-nil-pointer-in-interface hazard AGENTS.md documents (a production
// outage: a *typed* nil pointer assigned to an interface field produces a
// NON-nil interface, so an == nil guard passes and the first method call
// panics) cannot happen here. Task 7's natural DI shape for "fail-closed if
// unset" is `var c *extractordClient; if endpoint != "" { c = New(...) };
// WithAttachmentExtractor(c)` — when endpoint is empty, c is exactly this
// shape: a nil *fakeExtractor wrapped in a non-nil AttachmentExtractor
// interface value. Go's http.Server recovers a per-request panic without
// crashing the test process, so a REGRESSION here would surface as a
// connection error or non-200 status on this otherwise-ordinary upload, not
// a Go-level panic in this test — the assertions below are the actual
// evidence of "no panic", not a require.NotPanics wrapper.
func TestInboundAssetRoute_NilPointerExtractor_TreatedAsAbsent(t *testing.T) {
	var nilExt *fakeExtractor // a nil POINTER, not a nil interface value
	scheme := runtime.NewScheme()
	_ = spiceboxv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	astore := blobstore.NewMem()
	mem := memory.NewLocal(memoryinmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("sys")
	h := httpsrv.NewHandler(mem, reg, httpsrv.WithArtifact(c, astore), httpsrv.WithAttachmentExtractor(nilExt))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp := doUpload(t, srv, "sys", "application/pdf", "report.pdf", []byte("bytes"))
	require.Equal(t, http.StatusOK, resp.StatusCode, "a nil-pointer-typed extractor must be treated as absent, not panic")
	out := decodeResponse(t, resp)
	assert.Equal(t, false, out["extracted"])
	assert.Nil(t, out["unsupported"])
}

func TestInboundAssetRoute_ExtractorSucceeds(t *testing.T) {
	ext := &fakeExtractor{text: "extracted document text", pages: 4}
	srv, astore := newInboundAssetServer(t, ext)
	payload := []byte("raw docx bytes")

	resp := doUpload(t, srv, "sys", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "report.docx", payload)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	out := decodeResponse(t, resp)

	assert.Equal(t, true, out["extracted"])
	textRef, _ := out["textRef"].(string)
	require.NotEmpty(t, textRef)
	assert.Equal(t, float64(4), out["pages"])
	assert.Equal(t, 1, ext.calls)
	assert.Equal(t, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", ext.gotMIME)
	assert.Equal(t, string(payload), ext.gotBody, "the extractor must see the exact stored bytes")

	rc, err := astore.Get(context.Background(), artifactstore.Ref(textRef))
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "extracted document text", string(got))
}

func TestInboundAssetRoute_ExtractorReportsUnsupported_BytesStillStored(t *testing.T) {
	ext := &fakeExtractor{err: httpsrv.ErrAttachmentMIMEUnsupported}
	srv, astore := newInboundAssetServer(t, ext)
	payload := []byte("some bytes")

	resp := doUpload(t, srv, "sys", "image/svg+xml", "diagram.svg", payload)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	out := decodeResponse(t, resp)

	assert.Equal(t, true, out["unsupported"])
	assert.Equal(t, false, out["extracted"])
	ref, _ := out["ref"].(string)
	require.NotEmpty(t, ref, "bytes must still be stored even when the MIME is unsupported")

	rc, err := astore.Get(context.Background(), artifactstore.Ref(ref))
	require.NoError(t, err)
	rc.Close()
	assert.Equal(t, 1, storeItemCount(t, astore), "exactly the raw bytes are stored — no text object")
}

func TestInboundAssetRoute_ExtractorTransientFailure_BytesStillStored(t *testing.T) {
	ext := &fakeExtractor{err: errors.New("extractord: connection refused")}
	srv, astore := newInboundAssetServer(t, ext)

	resp := doUpload(t, srv, "sys", "application/pdf", "report.pdf", []byte("bytes"))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	out := decodeResponse(t, resp)

	assert.Equal(t, false, out["extracted"])
	assert.Nil(t, out["unsupported"], "a transient extraction failure must never be reported as unsupported")
	ref, _ := out["ref"].(string)
	require.NotEmpty(t, ref)
	assert.Equal(t, 1, storeItemCount(t, astore))
}

// TestInboundAssetRoute_NonChannelsdBearer_RejectedAndStoreUntouched is the
// brief's explicit requirement: a bad bearer gets 401/403 AND no object is
// written — proven against the store's actual contents, not just the status
// code.
func TestInboundAssetRoute_NonChannelsdBearer_RejectedAndStoreUntouched(t *testing.T) {
	srv, astore := newInboundAssetServer(t, nil)

	resp := doUpload(t, srv, "not-the-channelsd-token", "application/pdf", "report.pdf", []byte("bytes"))
	defer resp.Body.Close()

	assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, resp.StatusCode)
	assert.Equal(t, 0, storeItemCount(t, astore), "a rejected upload must write nothing to the store")
}

// TestInboundAssetRoute_PerSessionTokenRefused_StoreUntouched holds the line
// the artifact READ routes deliberately crossed: they admit a per-session
// memory token scoped to the URL's session, and this WRITE route must not.
//
// The token registered here is the one for the very session in the upload URL
// — the most favourable case there is — so a refusal can only come from the
// route staying on the system-only check. Widening the read routes must never
// let a session credential append into any session's artifact key space.
func TestInboundAssetRoute_PerSessionTokenRefused_StoreUntouched(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = spiceboxv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	astore := blobstore.NewMem()
	mem := memory.NewLocal(memoryinmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("sys")
	reg.Set(memory.NamespacedName{Namespace: "ns1", Name: "sess1"}, "tok-sess1", "")
	h := httpsrv.NewHandler(mem, reg, httpsrv.WithArtifact(c, astore))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp := doUpload(t, srv, "tok-sess1", "application/pdf", "report.pdf", []byte("bytes"))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"a per-session memory token must be refused on the write route, even for its own session")
	assert.Equal(t, 0, storeItemCount(t, astore), "a refused upload must write nothing to the store")
}

func TestInboundAssetRoute_NoBearer_401_StoreUntouched(t *testing.T) {
	srv, astore := newInboundAssetServer(t, nil)

	resp := doUpload(t, srv, "", "application/pdf", "report.pdf", []byte("bytes"))
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, 0, storeItemCount(t, astore))
}

// TestInboundAssetRoute_WebdTokenRefused_StoreUntouched drives the REAL route
// with webd's system token. /inbound-asset is the only write route the
// operator's memory server mounts, and tokens.SetWebdToken's contract is that
// the webd bearer is "accepted for reads on any session … but never for
// writes": webd is browser-facing, so it must not hold a credential that can
// append into any session's artifact key space (`<ns>/<sess>/inbound-asset/…`
// — the exact prefix a per-session memory token authorizes fetch_artifact
// against) or drive extractord with attacker-chosen bytes.
//
// This assertion was INVERTED from an earlier test that asserted 200 and said
// in its own comment that it "documents, rather than guards against" the
// behavior. Accepting a read-only credential on a write is the defect, not the
// contract; the earlier test encoded the bug as intent.
func TestInboundAssetRoute_WebdTokenRefused_StoreUntouched(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = spiceboxv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	astore := blobstore.NewMem()
	mem := memory.NewLocal(memoryinmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("sys")
	reg.SetWebdToken("webd")
	h := httpsrv.NewHandler(mem, reg, httpsrv.WithArtifact(c, astore))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp := doUpload(t, srv, "webd", "application/pdf", "report.pdf", []byte("bytes"))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "webd's read-only token must be refused on the write route")
	assert.Equal(t, 0, storeItemCount(t, astore), "a refused upload must write nothing to the store")
}

func TestInboundAssetRoute_MissingContentType_400(t *testing.T) {
	srv, astore := newInboundAssetServer(t, nil)
	resp := doUpload(t, srv, "sys", "", "report.pdf", []byte("bytes"))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, 0, storeItemCount(t, astore))
}

func TestInboundAssetRoute_WrongMethod_405(t *testing.T) {
	srv, _ := newInboundAssetServer(t, nil)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/inbound-asset/ns1/sess1", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer sys")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestInboundAssetRoute_MalformedPath_400(t *testing.T) {
	srv, _ := newInboundAssetServer(t, nil)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/inbound-asset/onlyns", bytes.NewReader([]byte("x")))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer sys")
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestInboundAssetRoute_OverCap_413 proves the server's absolute size ceiling
// is enforced independent of any Channel-configured limit (which is applied
// upstream, in the channelsd pipeline, before the fetch even happens). No ref
// is ever returned to the caller on this path (Put's error return means the
// response is a plain 413 with no body from the caller's point of view), so
// this is not a "store untouched" assertion — pkg/platform/artifactstore/blob's mem
// driver commits whatever bytes io.Copy already wrote before the
// MaxBytesReader trip, on the best-effort w.Close() in Put's error path; that
// leftover-partial-object behavior is a property of the shared Put contract
// every caller of artifactstore.Store already lives with, not something this
// route introduces or can fix locally.
func TestInboundAssetRoute_OverCap_413(t *testing.T) {
	srv, _ := newInboundAssetServer(t, nil)
	// One byte over the 25 MiB server-side ceiling.
	huge := bytes.Repeat([]byte{'a'}, (25<<20)+1)
	resp := doUpload(t, srv, "sys", "text/plain", "big.txt", huge)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}
