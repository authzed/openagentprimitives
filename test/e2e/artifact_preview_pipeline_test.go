//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/agent/preview/markup"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	assetregistry "github.com/authzed/openagentprimitives/pkg/channels/channelassets/registry"
	"github.com/authzed/openagentprimitives/pkg/memory"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all" // register artifact + artifactrevision memory kinds
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/artifactview"

	// Register the real asset renderer kinds so Delivery()/PreviewHTML/SupportsLiveView
	// are the production implementations: css + svg are BundledOnly; html is Standalone.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/css"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/svg"
)

// TestArtifactPreviewPipeline_BundledOnly_OfferViewToWebd exercises the
// bundled-only artifact preview pipeline end to end across its real seams:
//
//	artifact_offer_view (the REAL meta tool) on a css/svg BundledOnly source
//	  → async generation of an internal html "preview child" artifact
//	  → finalized in the REAL artifacts.Service over the harness memory backend,
//	    linked to the source revision (Service.GetPreviewChild becomes ok=true)
//	  → webd's content path (the REAL pkg/web/webui/artifactview HTTP server) frames
//	    the preview child's html — NEVER the raw svg/css bytes.
//
// The headline invariant — a bundled-only artifact's raw bytes are never framed
// in the browser; its html preview child is — is asserted two ways:
//
//  1. ContentRender / ResolveRender route the bundled-only head to the preview
//     child render, and a content token is minted ONLY for that html render. The
//     raw text/css (or image/svg+xml) source render name is recorded and asserted
//     to NEVER appear in the set of tokenized renders.
//  2. A real GET /content?ct=<token> through the artifactview HTTP handler serves
//     the preview-child html bytes (the css :target tabs + the embedded source
//     CSS, or the svg <img> data-URI shell) and never the raw source bytes.
//
// TIER: this is the strongest scope the in-process harness supports. True
// "drive-the-agent" e2e is NOT feasible: InProcessRunnerFactory.buildLoop
// assembles tools through capability.Assemble but leaves the artifacts
// capability's RunnerEnv deps (Client/RenderFetch/MarkupGen) unset — it is
// opt-in and no in-process fixture grants it — so buildLoop does not wire
// artifact_offer_view's deps the way internal/cmd/runner/main.go does, and the harness's
// startManager deliberately omits the
// ArtifactRender render controller. So we invoke the REAL offer_view tool
// directly (markup.Fake, a render-capturing fake client, a RenderFetch over the
// source bytes) against the REAL Service + memory, then drive the REAL webui
// content path. Only internal/cmd/webd's package-main artifactViewDeps struct is
// replicated here (it can't be imported); its ContentRender/ResolveRender/token
// bodies are reproduced verbatim over the same Service + registry. All names are
// fake per AGENTS.md.
func TestArtifactPreviewPipeline_BundledOnly_OfferViewToWebd(t *testing.T) {
	cases := []struct {
		name string
		// kind is the BundledOnly source renderer kind (css or svg).
		kind string
		// sourceBytes is the raw artifact payload RenderFetch returns; offer_view
		// feeds it to the kind's PreviewHTML composer.
		sourceBytes []byte
		// sourceMIME is the raw render's MIME — the kind that must NEVER be framed.
		sourceMIME string
		// wantInPreview are substrings the generated preview-child html must
		// contain (proves the real composer ran), e.g. the css source / target tabs.
		wantInPreview []string
		// mustNotServeRaw is the raw source content-type that must NEVER be the
		// framed content-type (the browser must receive html, not the raw bytes).
		mustNotServeRaw string
	}{
		{
			name:        "css → :target-tabbed html preview child framed; raw text/css never tokenized",
			kind:        "css",
			sourceBytes: []byte(".ap-headline{color:#bada55;font-weight:700}"),
			sourceMIME:  "text/css",
			wantInPreview: []string{
				"ap-tabs-nav",     // the :target tab chrome the css composer emits
				`href="#preview"`, // tab links
				".ap-headline",    // the source CSS is embedded in the preview doc
			},
			mustNotServeRaw: "text/css", // a raw css document would be served as text/css; we serve html
		},
		{
			name:        "svg → <img> data-URI html shell framed; raw image/svg+xml never tokenized",
			kind:        "svg",
			sourceBytes: []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><rect width="10" height="10"/></svg>`),
			sourceMIME:  "image/svg+xml",
			wantInPreview: []string{
				"<img", // svg preview is an <img> data-URI shell
				"data:image/svg+xml;base64,",
			},
			mustNotServeRaw: "image/svg+xml",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Sanity: the kind is registered and really BundledOnly + live-view
			// capable. If this drifts (kind dropped from the registry, Delivery
			// flipped), the whole premise is void — fail loud here.
			r, ok := assetregistry.ByKind(tc.kind)
			require.True(t, ok, "renderer kind %q must be registered", tc.kind)
			require.Equal(t, channelassets.DeliveryBundledOnly, r.Delivery(),
				"%q must be a BundledOnly kind for this pipeline", tc.kind)
			require.True(t, r.SupportsLiveView(), "%q must support live-view", tc.kind)

			ctx := context.Background()
			const (
				ns       = "default"
				sessName = "artifact-preview-pipeline"
			)
			scope := memory.Scope{Kind: "session", ID: ns + "/" + sessName}

			// REAL artifacts.Service over a real in-memory backend (the same
			// facade type the harness + operator use). No envtest/SpiceDB needed
			// for the framing invariant — it is about WHICH render is framed.
			svc := artifacts.NewService(sysApprovedMem{memory.NewLocal(memoryinmem.NewBackend())}, nil)

			// 1. Seed a finalized BundledOnly SOURCE revision (as artifact_prepare
			//    would have). Its render name is what must NEVER be framed.
			head := svc.NewArtifactID()
			srcRenderName := "ar-src-" + tc.kind
			sourceRevID := finalizeBundledSource(t, svc, scope, head, tc.kind, srcRenderName, tc.sourceMIME, types.UID("uid-src-"+tc.kind))

			// 2. A render store the webd content path fetches from. The
			//    offer_view's preview CR carries the composed html in Spec.Payload;
			//    the capturing fake client records it under the CR name (the render
			//    name) so FetchRender can serve it — standing in for the operator's
			//    render store. The raw source bytes are ALSO registered under the
			//    source render name, precisely so the test can prove the content
			//    path never reaches for them.
			rs := newRenderStore()
			rs.put(srcRenderName, tc.sourceBytes, tc.sourceMIME)

			// Capturing fake client: flips each created preview ArtifactRender to
			// Ready (so offer_view's poll completes) AND records its Spec.Payload
			// html into the render store under the CR name.
			fakeCli := clientfake.NewClientBuilder().
				WithScheme(previewPipelineScheme(t)).
				WithInterceptorFuncs(readyAndCapturePreview(rs, types.UID("uid-preview-"+tc.kind))).
				Build()

			// 3. Drive the REAL artifact_offer_view tool. It publishes the offer
			//    synchronously and generates the preview child asynchronously.
			var published [][]byte
			offerView := meta.NewArtifactOfferView(meta.ArtifactOfferViewConfig{
				Artifacts:      svc,
				AvailableKinds: []string{tc.kind},
				NATSPublish: func(_ context.Context, _ string, payload []byte) error {
					published = append(published, payload)
					return nil
				},
				NATSSubjectPrefix: "ap.session." + ns + "." + sessName,
				Client:            fakeCli,
				PollInterval:      time.Millisecond,
				RenderFetch: func(_ context.Context, _, _, render string) ([]byte, string, error) {
					b, mime, ok := rs.get(render)
					if !ok {
						return nil, "", fmt.Errorf("renderFetch: unknown render %q", render)
					}
					return b, mime, nil
				},
				MarkupGen: markup.Fake{}.Generate,
			})

			args := []byte(fmt.Sprintf(`{"artifact_id": %q}`, head))
			res, err := offerView.Execute(ctx, args, &tool.SessionContext{Namespace: ns, Name: sessName})
			require.NoError(t, err, "offer_view Execute")
			require.False(t, res.IsError, "offer_view result: %s", res.Content)
			require.Len(t, published, 1, "the live-view offer must publish synchronously")
			assert.Contains(t, string(published[0]), "live_view_offer", "offer envelope kind")

			// 4. The preview child must land in memory, linked to the source
			//    revision. Bounded poll mirrors the offer_view unit test.
			previewRenderName := eventuallyPreviewChildRender(t, svc, scope, sourceRevID)
			require.NotEqual(t, srcRenderName, previewRenderName,
				"the preview child render must be a NEW html render, not the raw source render")

			// The preview child is an html artifact (its head kind), stamped
			// internal + linked to the source — never the bundled-only kind itself.
			childHead, ok, err := svc.GetHead(ctx, scope, svc.PreviewChildID(sourceRevID))
			require.NoError(t, err)
			require.True(t, ok, "preview child head must exist")
			assert.Equal(t, "html", childHead.RendererKind, "preview child is an html artifact")
			assert.True(t, childHead.Internal, "preview child must be internal")

			// 5. Stand up the REAL pkg/web/webui/artifactview HTTP server over deps
			//    that faithfully replicate internal/cmd/webd's artifactViewDeps content
			//    path (ContentRender/ResolveRender/token mint+verify/FetchRender)
			//    against this same Service + render store.
			deps := &previewPipelineDeps{
				svc:    svc,
				rs:     rs,
				logger: logr.Discard(),
			}
			ts := startPreviewPipelineWebd(t, deps)
			deps.sandboxBase = ts.URL

			// ResolveRender goes through ContentRender — for a BundledOnly head it
			// MUST resolve the preview child render, never the raw source render.
			gotRender, err := deps.ResolveRender(ctx, ns, sessName, head)
			require.NoError(t, err, "ResolveRender for the bundled-only head")
			assert.Equal(t, previewRenderName, gotRender,
				"ResolveRender must frame the preview child render, not the raw source")

			// Mint the content URL exactly as the page shell does (urlsFor →
			// SignContentToken). This records the tokenized render in the deps.
			contentURL, err := deps.mintTestContentURL(ns, sessName, gotRender, head)
			require.NoError(t, err, "mint content URL")

			// === Invariant 1: a content token is minted ONLY for the preview
			//     child render; the raw source render is NEVER tokenized. ===
			tokenized := deps.tokenizedRenders()
			assert.Contains(t, tokenized, previewRenderName,
				"the preview child render must be tokenized for framing")
			assert.NotContains(t, tokenized, srcRenderName,
				"INVARIANT: the raw %s source render must never be tokenized/framed", tc.sourceMIME)

			// === Invariant 2: GET /content serves the preview-child html bytes,
			//     never the raw source bytes. ===
			resp, err := http.Get(contentURL)
			require.NoError(t, err, "GET %s", contentURL)
			body := readPreviewBody(t, resp)
			require.Equal(t, http.StatusOK, resp.StatusCode, "content fetch must 200; body=%s", body)

			ctype := resp.Header.Get("Content-Type")
			assert.True(t, strings.HasPrefix(ctype, "text/html"),
				"the framed content must be served as html, not the raw %s; got %q", tc.sourceMIME, ctype)
			assert.NotEqual(t, tc.mustNotServeRaw, ctype,
				"INVARIANT: raw %s must never be the framed content-type", tc.mustNotServeRaw)
			for _, want := range tc.wantInPreview {
				assert.Contains(t, body, want,
					"the framed preview html must contain %q (real composer output)", want)
			}

			// Belt-and-suspenders: the raw source bytes must not be reachable by
			// minting a token for the source render — the webd path never does
			// this, but prove the source render, even if tokenized by a buggy
			// caller, is a DIFFERENT document than what gets framed. We assert the
			// framed body is NOT byte-identical to the raw source.
			assert.NotEqual(t, string(tc.sourceBytes), body,
				"the framed body must be the html preview, never the raw source bytes")
		})
	}
}

// --- helpers ---

// previewPipelineScheme is the scheme the capturing fake client needs:
// ArtifactRender (spicebox) + corev1 (Secret, for parity with offer_view's
// client usage). Mirrors newOfferViewScheme in the meta package's test.
func previewPipelineScheme(t *testing.T) *apiruntime.Scheme {
	t.Helper()
	s := apiruntime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s), "corev1.AddToScheme")
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "spiceboxv1alpha1.AddToScheme")
	return s
}

// finalizeBundledSource records a Ready BundledOnly source revision (as
// artifact_prepare would) and returns its source revision ID. The OutputMIME is
// the kind's raw MIME — the type that must never be framed in the browser.
func finalizeBundledSource(t *testing.T, svc *artifacts.Service, scope memory.Scope, head, kind, renderName, mime string, uid types.UID) string {
	t.Helper()
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name: renderName, Namespace: "default", UID: uid,
			Labels: map[string]string{artifacts.LabelArtifactID: head},
		},
		Spec: spiceboxv1alpha1.ArtifactRenderSpec{Kind: kind},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase:     spiceboxv1alpha1.ArtifactRenderPhaseReady,
			OutputRef: "mem://" + renderName, OutputMIME: mime,
		},
	}
	rev, err := svc.FinalizeRevision(context.Background(), scope, cr)
	require.NoError(t, err, "finalize bundled-only source revision")
	return rev.RevisionID
}

// readyAndCapturePreview is the capturing interceptor: it flips each created
// preview ArtifactRender to Ready (no status subresource on the fake client, so
// Create persists the status the offer_view poll reads) AND records the composed
// preview html (cr.Spec.Payload) into rs under the CR name — standing in for the
// operator render store the webd content path fetches from.
func readyAndCapturePreview(rs *renderStore, uid types.UID) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if ar, ok := obj.(*spiceboxv1alpha1.ArtifactRender); ok {
				ar.UID = uid
				ar.Status.Phase = spiceboxv1alpha1.ArtifactRenderPhaseReady
				ar.Status.OutputRef = "mem://" + ar.Name
				ar.Status.OutputMIME = "text/html"
				ar.Status.OutputSize = int64(len(ar.Spec.Payload))
				ar.Status.OutputFilename = "preview.html"
				// Record the composed preview html so FetchRender can serve it.
				rs.put(ar.Name, ar.Spec.Payload, "text/html; charset=utf-8")
			}
			return c.Create(ctx, obj, opts...)
		},
	}
}

// eventuallyPreviewChildRender polls GetPreviewChild until ok=true or a bounded
// deadline, returning the preview child's render name. Mirrors the offer_view
// unit test's eventuallyPreviewChild; deterministic + non-flaky.
func eventuallyPreviewChildRender(t *testing.T, svc *artifacts.Service, scope memory.Scope, sourceRevID string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		name, ok, err := svc.GetPreviewChild(context.Background(), scope, sourceRevID)
		require.NoError(t, err, "GetPreviewChild poll")
		if ok {
			return name
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("preview child for source revision %q never appeared", sourceRevID)
	return ""
}

// renderStore is a tiny in-test stand-in for the operator render store: render
// name → (bytes, mime). Concurrency-safe because offer_view's capturing
// interceptor writes from the async generation goroutine while the test reads.
type renderStore struct {
	mu sync.Mutex
	m  map[string]renderEntry
}

type renderEntry struct {
	body []byte
	mime string
}

func newRenderStore() *renderStore { return &renderStore{m: map[string]renderEntry{}} }

func (s *renderStore) put(name string, body []byte, mime string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[name] = renderEntry{body: append([]byte(nil), body...), mime: mime}
}

func (s *renderStore) get(name string) ([]byte, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[name]
	if !ok {
		return nil, "", false
	}
	return e.body, e.mime, true
}

// previewPipelineDeps faithfully replicates internal/cmd/webd's artifactViewDeps content
// path — the methods can't be imported (package main), so the load-bearing
// bodies (ContentRender, ResolveRender, SignContentToken/VerifyContentToken,
// FetchRender, ServeTransform) are reproduced over the SAME real artifacts.Service
// + channelassets registry + render store. Authz/link methods are stubbed to
// pass (the framing invariant is independent of authz; the authz chain has its
// own e2e in artifact_view_authz_test.go).
type previewPipelineDeps struct {
	svc *artifacts.Service
	rs  *renderStore

	sandboxBase string
	logger      logr.Logger

	// tokenizedMu guards the record of every render a content token was minted
	// for — the evidence for the "raw bytes never framed" invariant.
	tokenizedMu sync.Mutex
	tokenized   map[string]struct{}
}

var _ artifactview.Deps = (*previewPipelineDeps)(nil)

// ContentRender is reproduced VERBATIM from internal/cmd/webd's artifactViewDeps: for a
// BundledOnly head it resolves the internal html preview child (never the raw
// bytes); for a standalone head it resolves the newest render.
func (d *previewPipelineDeps) ContentRender(ctx context.Context, ns, sess, artifactID string) (string, bool, error) {
	scope := memory.Scope{Kind: "session", ID: ns + "/" + sess}
	head, ok, err := d.svc.GetHead(ctx, scope, artifactID)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}
	if previewPipelineKindIsBundledOnly(head.RendererKind) {
		sourceRevID := head.Tags[artifacts.TagLatest]
		if sourceRevID == "" {
			return "", false, nil
		}
		return d.svc.GetPreviewChild(ctx, scope, sourceRevID)
	}
	rn, err := d.svc.ResolveToRender(ctx, scope, artifactID)
	if err != nil {
		return "", false, err
	}
	return rn, true, nil
}

// ResolveRender goes through ContentRender so the initial page never frames the
// raw bytes of a bundled-only artifact (verbatim from internal/cmd/webd).
func (d *previewPipelineDeps) ResolveRender(ctx context.Context, ns, sess, artifactID string) (string, error) {
	rn, ready, err := d.ContentRender(ctx, ns, sess, artifactID)
	if err != nil {
		return "", err
	}
	if !ready {
		return "", nil
	}
	return rn, nil
}

// RenderKind resolves renderName's kind from the same render-store lookup
// FetchRender uses, mapping the recorded MIME back to the ArtifactRender
// Spec.Kind that produced it. That mapping is exact for this suite's
// fixtures: the preview child is always finalized with Spec.Kind "html" +
// mime "text/html..." (readyAndCapturePreview / artifact_offer_view.go), and
// each BundledOnly source is finalized with Spec.Kind == tc.kind + mime ==
// tc.sourceMIME (finalizeBundledSource) — one MIME, one kind, no ambiguity.
// In practice only the preview child render is ever tokenized/served here
// (that is the "raw bytes never framed" invariant this test asserts), so on
// the real GET /content path this always resolves "html"; the source-render
// branch exists for per-renderName correctness, not because the test drives
// it.
func (d *previewPipelineDeps) RenderKind(_ context.Context, _, _, renderName string) (string, error) {
	_, mime, ok := d.rs.get(renderName)
	if !ok {
		return "", fmt.Errorf("renderKind: unknown render %q", renderName)
	}
	switch {
	case strings.HasPrefix(mime, "text/html"):
		return "html", nil
	case strings.HasPrefix(mime, "text/css"):
		return "css", nil
	case strings.HasPrefix(mime, "image/svg+xml"):
		return "svg", nil
	default:
		return "", fmt.Errorf("renderKind: unmapped mime %q for render %q", mime, renderName)
	}
}

func (d *previewPipelineDeps) RenderIsBundledOnly(ctx context.Context, ns, sess, artifactID string) bool {
	scope := memory.Scope{Kind: "session", ID: ns + "/" + sess}
	head, ok, err := d.svc.GetHead(ctx, scope, artifactID)
	if err != nil || !ok {
		return false
	}
	return previewPipelineKindIsBundledOnly(head.RendererKind)
}

// PreviewChildRender resolves a specific source revision's internal html preview
// child via the real Service (verbatim from internal/cmd/webd), so the pipeline E2E can
// assert per-revision serving when extended.
func (d *previewPipelineDeps) PreviewChildRender(ctx context.Context, ns, sess, revID string) (string, bool, error) {
	return d.svc.GetPreviewChild(ctx, memory.Scope{Kind: "session", ID: ns + "/" + sess}, revID)
}

func previewPipelineKindIsBundledOnly(rendererKind string) bool {
	r, found := assetregistry.ByKind(rendererKind)
	return found && r.Delivery() == channelassets.DeliveryBundledOnly
}

// SignContentToken records the render being tokenized (the invariant evidence)
// and returns a deterministic opaque token encoding (ns, sess, renderName).
func (d *previewPipelineDeps) SignContentToken(ns, sess, renderName, artifactID string) (string, error) {
	d.tokenizedMu.Lock()
	if d.tokenized == nil {
		d.tokenized = map[string]struct{}{}
	}
	d.tokenized[renderName] = struct{}{}
	d.tokenizedMu.Unlock()
	raw := ns + "\x00" + sess + "\x00" + renderName
	return base64.RawURLEncoding.EncodeToString([]byte(raw)), nil
}

func (d *previewPipelineDeps) VerifyContentToken(tok string) (string, string, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return "", "", "", err
	}
	parts := strings.SplitN(string(raw), "\x00", 3)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("bad content token")
	}
	return parts[0], parts[1], parts[2], nil
}

// FetchRender serves bytes from the render store — the preview-child html for a
// bundled-only artifact. There is no operator hop in-test; the store is the
// stand-in for it.
func (d *previewPipelineDeps) FetchRender(_ context.Context, _, _, renderName string) ([]byte, string, error) {
	b, mime, ok := d.rs.get(renderName)
	if !ok {
		return nil, "", fmt.Errorf("fetchRender: unknown render %q", renderName)
	}
	return b, mime, nil
}

// FetchRenderBundle: this suite exercises the bundled-only preview pipeline
// (the framing invariant), never the download path's bundle-vs-raw
// distinction — delegates to FetchRender's same render-store stand-in.
func (d *previewPipelineDeps) FetchRenderBundle(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
	return d.FetchRender(ctx, ns, sess, renderName)
}

// ServeTransform dispatches to the registered renderer's live-view transform
// (a no-op for every kind today: the artifact is served inert — see plan D1).
// Mirrors internal/cmd/webd's type-agnostic dispatch.
func (d *previewPipelineDeps) ServeTransform(_ context.Context, _, _, _ string, content []byte) []byte {
	return content
}

func (d *previewPipelineDeps) tokenizedRenders() []string {
	d.tokenizedMu.Lock()
	defer d.tokenizedMu.Unlock()
	out := make([]string, 0, len(d.tokenized))
	for k := range d.tokenized {
		out = append(out, k)
	}
	return out
}

// mintTestContentURL mirrors the page shell's urlsFor (content-URL half): mint
// a token and build the sandbox-origin /content URL.
func (d *previewPipelineDeps) mintTestContentURL(ns, sess, renderName, artifactID string) (string, error) {
	ct, err := d.SignContentToken(ns, sess, renderName, artifactID)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(d.sandboxBase, "/") + "/content?ct=" + ct, nil
}

// --- stubbed (authz/link/meta) methods: not exercised by the framing
//     invariant; the authz chain has its own e2e. ---

func (d *previewPipelineDeps) VerifyLink(string) (string, string, string, error) {
	return "", "", "", nil
}
func (d *previewPipelineDeps) CheckView(context.Context, string, string) (bool, error) {
	return true, nil
}

// CheckInteract gates the session-scoped live mirrors. This scenario exercises
// the render pipeline, not authorization, so it allows like CheckView above.
func (d *previewPipelineDeps) CheckInteract(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (d *previewPipelineDeps) ArtifactMeta(context.Context, string, string, string) (string, string, error) {
	return "", "", nil
}
func (d *previewPipelineDeps) ChannelKind(context.Context, string, string) (string, error) {
	return "", nil
}
func (d *previewPipelineDeps) SessionViews(context.Context, string, string) []string {
	return nil // this e2e suite exercises the preview pipeline, not session_views
}

// ResolveAssetURL / VerifyAssetToken: this suite exercises the bundled-only
// preview pipeline (svg/css → internal html preview child), never an
// `artifact:HANDLE` reference inside a primary's rendered HTML.
func (d *previewPipelineDeps) ResolveAssetURL(context.Context, string, string, string) (string, bool, error) {
	return "", false, nil
}
func (d *previewPipelineDeps) VerifyAssetToken(string) (string, string, string, error) {
	return "", "", "", nil
}
func (d *previewPipelineDeps) ListRevisions(context.Context, string, string, string) ([]artifactview.RevisionMeta, error) {
	return nil, nil
}
func (d *previewPipelineDeps) WatchSessionStatus(context.Context, string, string) (<-chan artifactview.StatusSnapshot, error) {
	return nil, nil
}
func (d *previewPipelineDeps) WatchMessages(context.Context, string, string) (<-chan artifactview.MirrorMessage, error) {
	return nil, nil // this e2e suite exercises the preview pipeline, not the chat mirror
}
func (d *previewPipelineDeps) TrustedOrigin() string  { return d.sandboxBase }
func (d *previewPipelineDeps) SandboxBaseURL() string { return d.sandboxBase }
func (d *previewPipelineDeps) Logger() logr.Logger    { return d.logger }

// startPreviewPipelineWebd mounts the REAL pkg/web/webui/artifactview WebUI on an
// httptest server. /content is OriginSandbox + AuthNone, so it is reachable with
// just a content token (no cookie). Trusted == sandbox host (single-host test).
func startPreviewPipelineWebd(t *testing.T, deps *previewPipelineDeps) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	ts := httptest.NewUnstartedServer(mux)
	ts.Start()
	t.Cleanup(ts.Close)

	host := strings.TrimPrefix(ts.URL, "http://")
	hostGetter := func() string { return host }
	deps.sandboxBase = ts.URL

	noAuth := func(*http.Request) (string, bool) { return "", false }
	server, err := webui.NewServer(
		noAuth, noAuth,
		hostGetter, hostGetter, // trusted == sandbox (single-host test)
		func(string) bool { return true }, // sharedOriginOK: collapse origins onto one host
		deps,
		[]webui.WebUI{artifactview.New()},
	)
	require.NoError(t, err, "build webui server")
	mux.Handle("/", server)
	return ts
}

// readPreviewBody reads + closes resp.Body and returns it as a string.
func readPreviewBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read response body")
	return string(b)
}
