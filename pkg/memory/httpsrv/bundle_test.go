package httpsrv

import (
	"archive/zip"
	"bytes"
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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	// Blank-imported for its registry.Register(New()) init() side effect only
	// — TestServeBundle_RegisteredKindWithoutRefRewriter_PassthroughRaw needs
	// a REAL registered kind that does not implement channelassets.RefRewriter
	// to exercise the type-assert-miss half of refRewriterFor's dispatch (the
	// registry-miss half is covered by TestServeBundle_UnregisteredKind_404-
	// adjacent unknown-render coverage already in this file).
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/image"
	"github.com/authzed/openagentprimitives/pkg/memory"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// ---------------------------------------------------------------------
// buildBundle: pure-helper coverage. No k8s/httptest fixture, just the
// resolve callback — exercises the parse/rewrite/zip logic directly.
// ---------------------------------------------------------------------

// unzip is a small test helper: unpacks b and returns a name->bytes map.
func unzip(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	require.NoError(t, err, "zip.NewReader on buildBundle output")
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err, "open zip entry %s", f.Name)
		data, err := io.ReadAll(rc)
		require.NoError(t, err, "read zip entry %s", f.Name)
		_ = rc.Close()
		out[f.Name] = data
	}
	return out
}

func TestBuildBundle_ResolvesAndRewritesRef(t *testing.T) {
	primary := []byte(`<html><body><img src="artifact:sec-1"></body></html>`)
	resolve := func(handle string) ([]byte, string, bool) {
		require.Equal(t, "sec-1", handle)
		return []byte("PNGBYTES"), "image/png", true
	}

	out, assetCount, err := buildBundle(html.New(), primary, resolve)
	require.NoError(t, err)
	assert.Equal(t, 1, assetCount, "one resolved asset")

	files := unzip(t, out)
	idx, ok := files["index.html"]
	require.True(t, ok, "index.html must be present")
	assert.NotContains(t, string(idx), "artifact:", "no artifact: scheme must survive the rewrite")
	assert.Contains(t, string(idx), "assets/sec-1.png", "src must point at the rewritten relative path")

	asset, ok := files["assets/sec-1.png"]
	require.True(t, ok, "assets/sec-1.png must be present")
	assert.Equal(t, []byte("PNGBYTES"), asset, "asset bytes must equal what resolve returned")

	assert.Len(t, files, 2, "exactly index.html + one asset entry")
}

func TestBuildBundle_DropsUnresolvedRef(t *testing.T) {
	primary := []byte(`<html><body><img src="artifact:missing" alt="x"></body></html>`)
	resolve := func(handle string) ([]byte, string, bool) { return nil, "", false }

	out, assetCount, err := buildBundle(html.New(), primary, resolve)
	require.NoError(t, err)
	assert.Equal(t, 0, assetCount, "no asset resolved — the only ref was dropped")

	files := unzip(t, out)
	require.Len(t, files, 1, "only index.html — no asset entry for a dropped ref")
	idx := string(files["index.html"])
	assert.NotContains(t, idx, "artifact:", "unresolvable ref must be dropped, not merely left unresolved")
	assert.NotContains(t, idx, "src=", "the whole src attribute is removed, not just its value")
	// The rest of the element must survive.
	assert.Contains(t, idx, `alt="x"`, "sibling attributes on the same element must be untouched")
}

func TestBuildBundle_DedupesRepeatedHandle(t *testing.T) {
	primary := []byte(`<html><body><img src="artifact:sec-1"><img src="artifact:sec-1"></body></html>`)
	calls := 0
	resolve := func(handle string) ([]byte, string, bool) {
		calls++
		return []byte("DATA"), "image/png", true
	}

	out, assetCount, err := buildBundle(html.New(), primary, resolve)
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "resolve must be called once per distinct handle, not once per reference")
	assert.Equal(t, 1, assetCount, "one distinct asset, shared by both references")

	files := unzip(t, out)
	assert.Len(t, files, 2, "index.html + exactly one asset entry, shared by both references")
	idx := string(files["index.html"])
	assert.Equal(t, 2, strings.Count(idx, "assets/sec-1.png"), "both img tags must point at the same rewritten path")
}

func TestBuildBundle_SanitizesHandleForAssetName(t *testing.T) {
	primary := []byte(`<html><body><img src="artifact:foo/bar baz#tag"></body></html>`)
	resolve := func(handle string) ([]byte, string, bool) {
		assert.Equal(t, "foo/bar baz#tag", handle, "resolve receives the raw handle, unsanitized")
		return []byte("D"), "text/plain", true
	}

	out, _, err := buildBundle(html.New(), primary, resolve)
	require.NoError(t, err)

	files := unzip(t, out)
	var assetName string
	for name := range files {
		if name != "index.html" {
			assetName = name
		}
	}
	require.NotEmpty(t, assetName, "an asset entry must exist")
	require.True(t, strings.HasPrefix(assetName, "assets/"), "asset path must live under assets/")
	base := strings.TrimSuffix(strings.TrimPrefix(assetName, "assets/"), ".txt")
	assert.NotContains(t, base, "/", "sanitized base name must not itself contain a path separator")
	for _, r := range base {
		assert.True(t, r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-',
			"sanitized asset base name must only contain filesystem-safe characters, got %q in %q", r, assetName)
	}
}

func TestBuildBundle_DisambiguatesCollidingSanitizedNames(t *testing.T) {
	// "a.b" and "a_b" both sanitize to "a_b" ('.' is replaced with '_'; '_' is
	// already a valid character and passes through unchanged) — both must
	// resolve to distinct entries, not one clobbering the other.
	primary := []byte(`<html><body><img src="artifact:a.b"><img src="artifact:a_b"></body></html>`)
	resolve := func(handle string) ([]byte, string, bool) {
		switch handle {
		case "a.b":
			return []byte("FIRST"), "text/plain", true
		case "a_b":
			return []byte("SECOND"), "text/plain", true
		default:
			t.Fatalf("unexpected handle %q", handle)
			return nil, "", false
		}
	}

	out, assetCount, err := buildBundle(html.New(), primary, resolve)
	require.NoError(t, err)
	assert.Equal(t, 2, assetCount, "two distinct assets")

	files := unzip(t, out)
	assert.Len(t, files, 3, "index.html + two distinct asset entries")
	var payloads [][]byte
	for name, data := range files {
		if name != "index.html" {
			payloads = append(payloads, data)
		}
	}
	assert.ElementsMatch(t, [][]byte{[]byte("FIRST"), []byte("SECOND")}, payloads, "both distinct handles must retain their own bytes")
}

func TestBuildBundle_NonArtifactRefsUntouched(t *testing.T) {
	primary := []byte(`<html><head><link rel="stylesheet" href="https://example.com/x.css"></head>` +
		`<body><img src="data:image/png;base64,AAAA"><img src="artifact:sec-1"></body></html>`)
	resolve := func(handle string) ([]byte, string, bool) { return []byte("D"), "image/png", true }

	out, _, err := buildBundle(html.New(), primary, resolve)
	require.NoError(t, err)

	files := unzip(t, out)
	idx := string(files["index.html"])
	assert.Contains(t, idx, "https://example.com/x.css", "non-artifact: href must be left untouched")
	assert.Contains(t, idx, "data:image/png;base64,AAAA", "non-artifact: src must be left untouched")
	assert.Contains(t, idx, "assets/sec-1.png", "the artifact: ref must still be rewritten")
}

func TestBuildBundle_ExtensionFromMIME(t *testing.T) {
	cases := []struct {
		mime, wantExt string
	}{
		{"image/png", ".png"},
		{"text/css", ".css"},
		{"image/svg+xml", ".svg"},
		{"application/x-unknown", ""},
	}
	for _, tc := range cases {
		t.Run(tc.mime, func(t *testing.T) {
			primary := []byte(`<html><body><img src="artifact:sec"></body></html>`)
			out, _, err := buildBundle(html.New(), primary, func(string) ([]byte, string, bool) { return []byte("D"), tc.mime, true })
			require.NoError(t, err)
			files := unzip(t, out)
			wantPath := "assets/sec" + tc.wantExt
			_, ok := files[wantPath]
			assert.True(t, ok, "expected asset path %q for mime %q; got %v", wantPath, tc.mime, keysOf(files))
		})
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestRefRewriterFor covers the three dispatch outcomes refRewriterFor must
// distinguish: an unregistered kind, a registered kind whose renderer does
// not implement channelassets.RefRewriter (the "image" kind, blank-imported
// above — a real renderer with no reference concept), and a registered kind
// that does (html). This is the sole gate serveBundle uses in place of a
// hardcoded "if kind == html" branch.
func TestRefRewriterFor(t *testing.T) {
	t.Run("unregistered kind: not ok", func(t *testing.T) {
		_, ok := refRewriterFor("no-such-kind")
		assert.False(t, ok)
	})
	t.Run("registered kind without RefRewriter: not ok", func(t *testing.T) {
		_, ok := refRewriterFor("image")
		assert.False(t, ok)
	})
	t.Run("registered kind with RefRewriter: ok", func(t *testing.T) {
		rw, ok := refRewriterFor("html")
		require.True(t, ok)
		require.NotNil(t, rw)
	})
}

// ---------------------------------------------------------------------
// serveBundle: handler-level coverage over the real httptest + fake k8s +
// in-mem artifactstore + in-mem memory stack, mirroring artifact_test.go's
// harness for serveArtifact.
// ---------------------------------------------------------------------

func bundleScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = spiceboxv1alpha1.AddToScheme(s)
	return s
}

// bundleRenderCR builds a Ready html-kind ArtifactRender CR owned by sess
// (when sess != ""), with payload already populated in store under its
// OutputRef. Thin wrapper over bundleRenderCRWithKind for the (overwhelming
// majority) html-kind callers.
func bundleRenderCR(t *testing.T, store artifactstore.Store, ns, name, sess, mime string, payload []byte) *spiceboxv1alpha1.ArtifactRender {
	t.Helper()
	return bundleRenderCRWithKind(t, store, ns, name, sess, "html", mime, payload)
}

// bundleRenderCRWithKind is bundleRenderCR with an explicit Spec.Kind — used
// by the passthrough tests to build a non-html primary (serveBundle must
// never attempt to parse/bundle one).
func bundleRenderCRWithKind(t *testing.T, store artifactstore.Store, ns, name, sess, kind, mime string, payload []byte) *spiceboxv1alpha1.ArtifactRender {
	t.Helper()
	ref, err := store.Put(context.Background(), ns+"/"+sess+"/"+name+"/output", bytes.NewReader(payload))
	require.NoError(t, err, "artifactstore.Put")
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       spiceboxv1alpha1.ArtifactRenderSpec{Kind: kind},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase:          spiceboxv1alpha1.ArtifactRenderPhaseReady,
			OutputRef:      string(ref),
			OutputMIME:     mime,
			OutputFilename: name + ".bin",
		},
	}
	if sess != "" {
		cr.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: spiceboxv1alpha1.SchemeGroupVersion.String(),
			Kind:       "AgentSession",
			Name:       sess,
			UID:        types.UID("uid-" + sess),
		}}
	}
	return cr
}

// finalizeBundleRevision seeds memory (via the same artifacts.Service path
// the runner uses) with a revision entry linking renderName to scope, so
// artifacts.Service.ResolveToRender(scope, renderName) succeeds. Returns the
// minted revision ID (an "artrev-…" handle) for tests that resolve by
// revision rather than by CR name.
func finalizeBundleRevision(t *testing.T, svc *artifacts.Service, scope memory.Scope, renderName string) (revisionID string) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name:        renderName,
			Labels:      map[string]string{artifacts.LabelArtifactID: svc.NewArtifactID()},
			Annotations: map[string]string{},
		},
		Spec: spiceboxv1alpha1.ArtifactRenderSpec{Kind: "html"},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase: spiceboxv1alpha1.ArtifactRenderPhaseReady,
		},
	}
	result, err := svc.FinalizeRevision(ctx, scope, cr)
	require.NoError(t, err, "finalize revision for %s", renderName)
	return result.RevisionID
}

func TestServeBundle_HappyPath_ResolvesSameSessionAsset(t *testing.T) {
	scheme := bundleScheme(t)
	astore := blobstore.NewMem()
	mem := memory.NewLocal(memoryinmem.NewBackend())
	svc := artifacts.NewService(mem, nil)
	scope := memory.Scope{Kind: "session", ID: "ns1/sess1"}

	primaryHTML := []byte(`<html><body><img src="artifact:render-secondary"></body></html>`)
	primaryCR := bundleRenderCR(t, astore, "ns1", "render-primary", "sess1", "text/html; charset=utf-8", primaryHTML)
	primaryCR.Status.OutputFilename = "report.html"

	secondaryPayload := []byte("PNGDATA")
	secondaryCR := bundleRenderCR(t, astore, "ns1", "render-secondary", "sess1", "image/png", secondaryPayload)
	finalizeBundleRevision(t, svc, scope, "render-secondary")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(primaryCR, secondaryCR).Build()
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("sys")
	h := NewHandler(mem, reg, WithArtifact(c, astore))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/artifact-bundle/ns1/sess1/render-primary/bundle", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer sys")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/zip", resp.Header.Get("Content-Type"))
	assert.Contains(t, resp.Header.Get("Content-Disposition"), `filename="report.zip"`)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	files := unzip(t, body)

	idx, ok := files["index.html"]
	require.True(t, ok, "index.html must be present")
	assert.NotContains(t, string(idx), "artifact:", "no artifact: scheme must reach the ZIP")
	assert.Contains(t, string(idx), "assets/render-secondary.png", "src must be rewritten to the relative asset path")

	asset, ok := files["assets/render-secondary.png"]
	require.True(t, ok, "assets/render-secondary.png must be present")
	assert.Equal(t, secondaryPayload, asset, "asset bytes must equal the secondary's stored bytes")
}

// TestServeBundle_RegisteredKindWithoutRefRewriter_PassthroughRaw is the
// Task 7 genericity proof for the bundler: the route never branches on a
// kind name (no "if kind == html" anywhere in this package) — it dispatches
// through refRewriterFor, which type-asserts channelassets.RefRewriter on
// whatever renderer the kind registers. The "image" kind (blank-imported
// above) is a REAL registered renderer that does not implement RefRewriter —
// it has no reference concept — so this exercises the type-assert-miss half
// of that dispatch (not merely an unregistered-kind miss), proving the
// bundle route serves it exactly like GET
// /artifact/{ns}/{sess}/{render}/output, never as a ZIP.
func TestServeBundle_RegisteredKindWithoutRefRewriter_PassthroughRaw(t *testing.T) {
	scheme := bundleScheme(t)
	astore := blobstore.NewMem()
	mem := memory.NewLocal(memoryinmem.NewBackend())

	payload := []byte("\x89PNG-fake-bytes")
	primaryCR := bundleRenderCRWithKind(t, astore, "ns1", "render-primary", "sess1", "image", "image/png", payload)
	primaryCR.Status.OutputFilename = "photo.png"

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(primaryCR).Build()
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("sys")
	h := NewHandler(mem, reg, WithArtifact(c, astore))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/artifact-bundle/ns1/sess1/render-primary/bundle", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer sys")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "image/png", resp.Header.Get("Content-Type"), "non-html kind must be served with its own MIME, never zipped")
	assert.Contains(t, resp.Header.Get("Content-Disposition"), `filename="photo.png"`, "filename must be the original, not swapped to .zip")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, payload, body, "bytes must be the raw, untouched payload")
}

// TestServeBundle_HTMLNoRefs_PassthroughRaw covers the other passthrough
// branch: an html primary with zero `artifact:` references has nothing to
// bundle either — it must be served as the original, UNMODIFIED bytes (not
// a rewritten copy, and not a 1-entry ZIP).
func TestServeBundle_HTMLNoRefs_PassthroughRaw(t *testing.T) {
	scheme := bundleScheme(t)
	astore := blobstore.NewMem()
	mem := memory.NewLocal(memoryinmem.NewBackend())

	primaryHTML := []byte(`<html><body><p>no refs here</p></body></html>`)
	primaryCR := bundleRenderCR(t, astore, "ns1", "render-primary", "sess1", "text/html; charset=utf-8", primaryHTML)
	primaryCR.Status.OutputFilename = "report.html"

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(primaryCR).Build()
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("sys")
	h := NewHandler(mem, reg, WithArtifact(c, astore))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/artifact-bundle/ns1/sess1/render-primary/bundle", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer sys")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"), "a ref-less html primary must not be zipped")
	assert.Contains(t, resp.Header.Get("Content-Disposition"), `filename="report.html"`, "filename must stay .html, not .zip")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, primaryHTML, body, "bytes must be byte-identical to the original — not even re-serialized by the HTML parser")
}

// TestServeBundle_HTMLUnresolvableRefOnly_PassthroughRaw is the same
// passthrough branch as above, but for an html primary whose only ref
// exists yet fails to resolve (cross-session) — proving "zero RESOLVED
// refs", not merely "zero refs present", is what triggers the raw fallback.
func TestServeBundle_HTMLUnresolvableRefOnly_PassthroughRaw(t *testing.T) {
	scheme := bundleScheme(t)
	astore := blobstore.NewMem()
	mem := memory.NewLocal(memoryinmem.NewBackend())

	primaryHTML := []byte(`<html><body><img src="artifact:does-not-exist"></body></html>`)
	primaryCR := bundleRenderCR(t, astore, "ns1", "render-primary", "sess1", "text/html", primaryHTML)
	primaryCR.Status.OutputFilename = "report.html"

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(primaryCR).Build()
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("sys")
	h := NewHandler(mem, reg, WithArtifact(c, astore))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/artifact-bundle/ns1/sess1/render-primary/bundle", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer sys")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/html", resp.Header.Get("Content-Type"), "an html primary with no RESOLVED ref must not be zipped")
	assert.NotEqual(t, "application/zip", resp.Header.Get("Content-Type"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, primaryHTML, body, "bytes must be byte-identical to the original, including the unresolvable artifact: ref — not a rewritten-with-drops copy")
}

// TestServeBundle_TagSuffixResolves covers the fix noted in the task brief:
// pkg/platform/artifacts/resolve.go only strips a "#tag" suffix for the "artifact-…"
// handle form; the "artrev-…" and bare-CR-name forms treat it as part of the
// id/name and fail to resolve. resolveBundleAsset must strip it before
// calling ResolveToRender for those two forms so "artifact:HANDLE#tag" refs
// on either form still resolve.
func TestServeBundle_TagSuffixResolves(t *testing.T) {
	scheme := bundleScheme(t)
	astore := blobstore.NewMem()
	mem := memory.NewLocal(memoryinmem.NewBackend())
	svc := artifacts.NewService(mem, nil)
	scope := memory.Scope{Kind: "session", ID: "ns1/sess1"}

	secondaryPayload := []byte("PNGDATA")
	secondaryCR := bundleRenderCR(t, astore, "ns1", "render-secondary", "sess1", "image/png", secondaryPayload)
	revID := finalizeBundleRevision(t, svc, scope, "render-secondary")
	require.True(t, strings.HasPrefix(revID, "artrev-"), "sanity: FinalizeRevision must mint an artrev- id")

	cases := []struct {
		name       string
		primaryCRN string
		handle     string
	}{
		{"CR-name form with #tag suffix", "render-primary-crname", "render-secondary#anything"},
		{"artrev- form with #tag suffix", "render-primary-artrev", revID + "#anything"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			primaryHTML := []byte(`<html><body><img src="artifact:` + tc.handle + `"></body></html>`)
			primaryCR := bundleRenderCR(t, astore, "ns1", tc.primaryCRN, "sess1", "text/html", primaryHTML)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(primaryCR, secondaryCR).Build()
			reg := tokens.NewRegistry()
			reg.SetChannelsdToken("sys")
			h := NewHandler(mem, reg, WithArtifact(c, astore))
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)

			req, err := http.NewRequest(http.MethodGet, srv.URL+"/artifact-bundle/ns1/sess1/"+primaryCR.Name+"/bundle", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer sys")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			files := unzip(t, body)

			// The resolved asset's filename is derived from the raw handle
			// (tag suffix included, sanitized) — not the point under test here.
			// What matters is that resolution SUCCEEDED (an assets/ entry with
			// the secondary's exact bytes exists) rather than the ref being
			// dropped, and that the raw artifact: value doesn't reach index.html.
			var assetBytes []byte
			var assetCount int
			for name, data := range files {
				if strings.HasPrefix(name, "assets/") {
					assetCount++
					assetBytes = data
				}
			}
			require.Equal(t, 1, assetCount, "the #tag-suffixed handle %q must still resolve to exactly one asset entry; got %v", tc.handle, keysOf(files))
			assert.Equal(t, secondaryPayload, assetBytes)
			assert.NotContains(t, string(files["index.html"]), "artifact:")
		})
	}
}

func TestServeBundle_CrossSessionHandle_DroppedNotFailed(t *testing.T) {
	scheme := bundleScheme(t)
	astore := blobstore.NewMem()
	mem := memory.NewLocal(memoryinmem.NewBackend())
	svc := artifacts.NewService(mem, nil)
	otherScope := memory.Scope{Kind: "session", ID: "ns1/sessOther"}

	primaryHTML := []byte(`<html><body><img src="artifact:render-foreign"></body></html>`)
	primaryCR := bundleRenderCR(t, astore, "ns1", "render-primary", "sess1", "text/html", primaryHTML)

	// The foreign secondary's revision lives in a DIFFERENT session's memory
	// scope. No CR named "render-foreign" is even created — resolution must
	// fail at the memory-scoped query, before any k8s lookup would occur,
	// proving the same-session boundary is enforced there.
	finalizeBundleRevision(t, svc, otherScope, "render-foreign")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(primaryCR).Build()
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("sys")
	h := NewHandler(mem, reg, WithArtifact(c, astore))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/artifact-bundle/ns1/sess1/render-primary/bundle", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer sys")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode, "a cross-session ref must not fail the whole bundle")
	// Task 6: zero RESOLVED refs (this one is dropped as cross-session) means
	// there is nothing to bundle — served as the original, byte-identical
	// html rather than a 1-entry ZIP. See
	// TestServeBundle_HTMLUnresolvableRefOnly_PassthroughRaw for the
	// dedicated passthrough coverage; this test's own job is proving the
	// same-session boundary itself is what causes the drop.
	assert.Equal(t, "text/html", resp.Header.Get("Content-Type"), "nothing resolved → raw passthrough, not a ZIP")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, primaryHTML, body, "byte-identical original, including the dropped-at-resolution artifact: ref")
}

func TestServeBundle_MissingBearer_401(t *testing.T) {
	scheme := bundleScheme(t)
	astore := blobstore.NewMem()
	mem := memory.NewLocal(memoryinmem.NewBackend())
	primaryCR := bundleRenderCR(t, astore, "ns1", "render-primary", "sess1", "text/html", []byte("<html></html>"))

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(primaryCR).Build()
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("sys")
	h := NewHandler(mem, reg, WithArtifact(c, astore))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/artifact-bundle/ns1/sess1/render-primary/bundle", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Also cover a present-but-wrong bearer, not just an absent header.
	req2, err := http.NewRequest(http.MethodGet, srv.URL+"/artifact-bundle/ns1/sess1/render-primary/bundle", nil)
	require.NoError(t, err)
	req2.Header.Set("Authorization", "Bearer wrong-token")
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp2.StatusCode)
}

func TestServeBundle_UnknownRender_404(t *testing.T) {
	scheme := bundleScheme(t)
	astore := blobstore.NewMem()
	mem := memory.NewLocal(memoryinmem.NewBackend())
	c := fake.NewClientBuilder().WithScheme(scheme).Build() // no CR at all
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("sys")
	h := NewHandler(mem, reg, WithArtifact(c, astore))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/artifact-bundle/ns1/sess1/missing/bundle", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer sys")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestServeBundle_NonOwningSession_403(t *testing.T) {
	scheme := bundleScheme(t)
	astore := blobstore.NewMem()
	mem := memory.NewLocal(memoryinmem.NewBackend())
	primaryCR := bundleRenderCR(t, astore, "ns1", "render-primary", "sessA", "text/html", []byte("<html></html>"))

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(primaryCR).Build()
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("sys")
	h := NewHandler(mem, reg, WithArtifact(c, astore))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/artifact-bundle/ns1/sessB/render-primary/bundle", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer sys")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// TestServeBundle_PerSessionTokenScoping is the bundle route's half of
// TestArtifactRoute_PerSessionTokenScoping: the per-session memory token is
// admitted for the session named in the URL and refused for any other.
//
// It drives the FULL ref-resolving path rather than a passthrough, because
// resolving a ref reaches memory: the route mints its own approval for the
// facade's read door, and a session token must get one scoped to (ns, sess) —
// so a bundle whose asset resolves is the proof that the scoped mint actually
// opens the door it has to open.
func TestServeBundle_PerSessionTokenScoping(t *testing.T) {
	scheme := bundleScheme(t)
	astore := blobstore.NewMem()
	mem := memory.NewLocal(memoryinmem.NewBackend())
	svc := artifacts.NewService(mem, nil)
	scope := memory.Scope{Kind: "session", ID: "ns1/sessA"}

	primaryHTML := []byte(`<html><body><img src="artifact:render-secondary"></body></html>`)
	primaryCR := bundleRenderCR(t, astore, "ns1", "render-primary", "sessA", "text/html; charset=utf-8", primaryHTML)
	primaryCR.Status.OutputFilename = "report.html"
	secondaryPayload := []byte("PNGDATA")
	secondaryCR := bundleRenderCR(t, astore, "ns1", "render-secondary", "sessA", "image/png", secondaryPayload)
	finalizeBundleRevision(t, svc, scope, "render-secondary")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(primaryCR, secondaryCR).Build()
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken("sys")
	reg.Set(memory.NamespacedName{Namespace: "ns1", Name: "sessA"}, "tok-a", "")
	// tok-other is a live token for a session this URL never names.
	reg.Set(memory.NamespacedName{Namespace: "ns1", Name: "sessOther"}, "tok-other", "")

	h := NewHandler(mem, reg, WithArtifact(c, astore))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	get := func(t *testing.T, bearer string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/artifact-bundle/ns1/sessA/render-primary/bundle", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}

	t.Run("token registered for the URL's session: 200 with the asset resolved into the ZIP", func(t *testing.T) {
		resp := get(t, "tok-a")
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "application/zip", resp.Header.Get("Content-Type"))
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		files := unzip(t, body)
		asset, ok := files["assets/render-secondary.png"]
		require.True(t, ok, "the resolved secondary must be in the ZIP — the scoped approval opened the memory read door")
		assert.Equal(t, secondaryPayload, asset)
	})

	t.Run("live token for a DIFFERENT session: 401 (the row above is its control)", func(t *testing.T) {
		resp := get(t, "tok-other")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
}
