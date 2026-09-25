package httpsrv

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	assetregistry "github.com/authzed/openagentprimitives/pkg/channels/channelassets/registry"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

// serveBundle handles GET /artifact-bundle/{ns}/{sess}/{render}/bundle.
//
// Auth and CR resolution are identical to serveArtifact: checkArtifactReadBearer
// — a system bearer, or a per-session memory token authorized for this path's
// {ns}/{sess} — then ownership + Ready checks on the PRIMARY render. The body
// differs — it is
// a SMART passthrough, not an unconditional ZIP, so callers can always hit this
// route and get whatever is appropriate:
//
//   - A primary whose renderer does not implement channelassets.RefRewriter
//     cannot carry a reference this route resolves, so it is streamed raw. The
//     route NEVER branches on a kind name; RefRewriter is the generic gate.
//   - A RefRewriter primary with zero resolvable references is streamed raw and
//     UNMODIFIED, not as a rewritten-with-drops copy.
//   - A RefRewriter primary with at least one resolvable reference gets every
//     reference resolved through artifacts.Service scoped to (ns, sess) — the
//     same same-session boundary the live view uses — and returns a
//     self-contained ZIP: the rewritten primary as index.html plus one assets/…
//     entry per resolved secondary.
//
// A handle resolving outside (ns, sess), or not at all, has its reference
// DROPPED rather than failing the whole bundle — the "degrade, don't fail"
// policy channelassets.RefRewriter documents.
//
// Statuses: 401 bad bearer; 405 non-GET; 400 malformed path; 404 no primary CR;
// 403 primary not owned by {sess}; 409 primary not Ready; 500 empty OutputRef,
// unfetchable bytes, or an unbuildable ZIP — a ZIP download has no
// "serve unrewritten" fallback, unlike the live view.
func (h *handler) serveBundle(w http.ResponseWriter, r *http.Request) {
	ns, sess, render, pathOK := parseArtifactPath(r.URL.Path, "/artifact-bundle/", "bundle")
	sessionToken, ok := h.checkArtifactReadBearer(w, r, ns, sess)
	if !ok {
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !pathOK {
		http.Error(w, "expected /artifact-bundle/{ns}/{sess}/{render}/bundle", http.StatusBadRequest)
		return
	}
	if ns == "" || sess == "" || render == "" {
		http.Error(w, "empty path segment", http.StatusBadRequest)
		return
	}

	cr, outcome := h.fetchOwnedReadyCR(r.Context(), ns, sess, render)
	switch outcome {
	case crOutcomeNotFound:
		http.Error(w, "not found", http.StatusNotFound)
		return
	case crOutcomeLookupFailed:
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	case crOutcomeForbidden:
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	case crOutcomeNotReady:
		http.Error(w, fmt.Sprintf("artifact not ready: phase=%s", cr.Status.Phase), http.StatusConflict)
		return
	}

	if cr.Status.OutputRef == "" {
		http.Error(w, "render is Ready but OutputRef is empty", http.StatusInternalServerError)
		return
	}

	// Passthrough branch 1: a kind whose renderer doesn't implement
	// RefRewriter (unregistered kind, or registered but no reference
	// concept) can never carry a reference this route knows how to resolve —
	// stream it exactly like serveArtifact.
	rw, ok := refRewriterFor(cr.Spec.Kind)
	if !ok {
		h.streamRawOutput(w, r, cr)
		return
	}

	primary, err := h.readArtifactBytes(r.Context(), artifactstore.Ref(cr.Status.OutputRef))
	if err != nil {
		if errors.Is(err, artifactstore.ErrNotFound) {
			http.Error(w, "artifact bytes not in store", http.StatusInternalServerError)
			return
		}
		http.Error(w, "fetch failed", http.StatusInternalServerError)
		return
	}

	// The bearer was already confirmed, so mint the approval the facade's
	// ReadMemory door needs to admit the scoped handle-resolution queries below.
	// This route is mounted outside ServeHTTP's /memory/ dispatch, which would
	// otherwise mint it, so it must mint its own.
	//
	// A system component gets the wildcard approval it carries everywhere. A
	// per-session token gets one bound to THIS (ns, sess) instead: the wildcard
	// satisfies any non-internal permission for any resource, bounded only by
	// this handler remembering to pin the scope below, and a user credential
	// should not depend on that memory. Both open exactly the one door
	// resolveBundleAsset knocks on.
	ctx := memory.WithSystemApproval(r.Context(), "system:artifact-bundle")
	if sessionToken != "" {
		ctx = memory.WithApproval(r.Context(),
			memory.ForBearerToken(memory.ReadMemory, ns+"/"+sess, tokenIDFor(sessionToken)))
	}
	svc := artifacts.NewService(h.mem, nil)
	scope := memory.Scope{Kind: "session", ID: ns + "/" + sess}

	zipBytes, assetCount, err := buildBundle(rw, primary, func(handle string) ([]byte, string, bool) {
		return h.resolveBundleAsset(ctx, svc, scope, ns, sess, handle)
	})
	if err != nil {
		log.FromContext(r.Context()).Error(err, "artifact-bundle: build failed",
			"ns", ns, "sess", sess, "render", render)
		http.Error(w, "bundle build failed", http.StatusInternalServerError)
		return
	}

	// Passthrough branch 2: nothing resolved (no refs, or all dropped), so serve
	// the untouched original bytes rather than a 1-entry ZIP differing from the
	// original only by dropped references.
	if assetCount == 0 {
		writeRawBytes(w, primary, cr.Status.OutputMIME, cr.Status.OutputFilename)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", strconv.Itoa(len(zipBytes)))
	w.Header().Set("Content-Disposition", contentDisposition(bundleFilename(cr.Status.OutputFilename, render)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(zipBytes)
}

// refRewriterFor looks up kind's registered renderer and type-asserts
// channelassets.RefRewriter — the generic gate serveBundle uses instead of a
// hardcoded kind check, so a new kind opts into bundling by implementing the
// interface in its own package. ok=false when the kind is unregistered or its
// renderer does not implement RefRewriter.
func refRewriterFor(kind string) (channelassets.RefRewriter, bool) {
	renderer, ok := assetregistry.ByKind(kind)
	if !ok {
		return nil, false
	}
	rw, ok := renderer.(channelassets.RefRewriter)
	return rw, ok
}

// writeRawBytes writes data verbatim as an attachment, defaulting mime to
// application/octet-stream. serveBundle's passthrough uses it because the bytes
// are already in memory from the RefRewriter call, avoiding the second
// artifactstore.Get that reusing streamRawOutput would cost.
func writeRawBytes(w http.ResponseWriter, data []byte, mime, filename string) {
	if mime == "" {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Content-Disposition", contentDisposition(filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// readArtifactBytes fetches and fully reads an artifactstore ref.
func (h *handler) readArtifactBytes(ctx context.Context, ref artifactstore.Ref) ([]byte, error) {
	rc, err := h.artifactStore.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// resolveBundleAsset resolves handle to its secondary's bytes and MIME, SCOPED
// to (ns, sess): svc.ResolveToRender queries memory filtered by that exact
// scope, so a handle belonging to another session simply is not found. THAT is
// the enforcement point for "same-session only".
//
// ok=false means "drop this reference" — an unknown handle, one resolving
// outside (ns, sess), a secondary that is not Ready, or a store miss. A genuine
// backend error is logged so a systematically broken bundle is diagnosable, but
// is still ok=false: one bad ref must never fail the whole bundle.
func (h *handler) resolveBundleAsset(ctx context.Context, svc *artifacts.Service, scope memory.Scope, ns, sess, handle string) (data []byte, mime string, ok bool) {
	// #tag stripping for revision-specific handles is centralized in
	// artifacts.ResolveToRender — deliberately not repeated here.
	renderName, err := svc.ResolveToRender(ctx, scope, handle)
	if err != nil {
		if errors.Is(err, artifacts.ErrNotFound) {
			// Degrade, not fail — but never SILENTLY: a broken or foreign
			// artifact: ref is worth an operator log rather than vanishing.
			log.FromContext(ctx).Info("artifact-bundle: ref did not resolve in this session; dropping from bundle",
				"ns", ns, "sess", sess, "handle", handle)
		} else {
			log.FromContext(ctx).Error(err, "artifact-bundle: ResolveToRender errored; dropping ref",
				"ns", ns, "sess", sess, "handle", handle)
		}
		return nil, "", false
	}

	cr, outcome := h.fetchOwnedReadyCR(ctx, ns, sess, renderName)
	if outcome != crOutcomeOK {
		log.FromContext(ctx).Info("artifact-bundle: referenced secondary not owned/ready; dropping from bundle",
			"ns", ns, "sess", sess, "handle", handle, "render", renderName, "outcome", outcome)
		return nil, "", false
	}
	if cr.Status.OutputRef == "" {
		log.FromContext(ctx).Info("artifact-bundle: referenced secondary has no output bytes; dropping from bundle",
			"ns", ns, "sess", sess, "handle", handle, "render", renderName)
		return nil, "", false
	}

	out, err := h.readArtifactBytes(ctx, artifactstore.Ref(cr.Status.OutputRef))
	if err != nil {
		log.FromContext(ctx).Error(err, "artifact-bundle: fetch secondary bytes failed; dropping ref",
			"ns", ns, "sess", sess, "handle", handle, "render", renderName)
		return nil, "", false
	}
	return out, cr.Status.OutputMIME, true
}

// bundleFilename picks the ZIP's Content-Disposition filename: the primary's
// output filename with its extension swapped for .zip, falling back to the
// render name when OutputFilename is empty.
func bundleFilename(outputFilename, render string) string {
	base := outputFilename
	if base == "" {
		base = render
	}
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	if base == "" {
		base = "artifact"
	}
	return base + ".zip"
}

// buildBundle rewrites every reference rw finds in primary via resolve and
// returns a ZIP holding the rewritten primary as index.html — the bundle's
// browsable entry point regardless of the primary's kind — plus one assets/…
// entry per resolved secondary, with assetCount being that entry count.
// serveBundle reads assetCount == 0 as "nothing resolved" and serves the
// primary's raw bytes instead.
//
// resolve returning ok=false drops that one reference and leaves the rest of
// the primary untouched, never failing the bundle. rw.RewriteRefs guarantees
// resolve is called at most once per distinct handle, so a handle referenced
// many times yields exactly one assets/ entry shared by all of them. The only
// error returned is a ZIP that could not be written.
func buildBundle(rw channelassets.RefRewriter, primary []byte, resolve func(handle string) (data []byte, mime string, ok bool)) (zipBytes []byte, assetCount int, err error) {
	var assets []bundleAsset
	usedPaths := map[string]bool{}

	rewritten, resolvedHandles := rw.RewriteRefs(primary, func(handle string) (string, bool) {
		data, mime, ok := resolve(handle)
		if !ok {
			return "", false
		}
		path := uniqueBundleAssetPath(handle, mime, usedPaths)
		assets = append(assets, bundleAsset{path: path, data: data})
		return path, true
	})

	zipBytes, err = zipBundle(rewritten, assets)
	if err != nil {
		return nil, 0, err
	}
	return zipBytes, len(resolvedHandles), nil
}

// bundleAsset is one resolved secondary awaiting a zip.Writer.Create call.
// Kept as an ordered slice (not a map), assigned in first-referenced order by
// buildBundle's resolve closure, so the ZIP's asset entries are written in a
// deterministic order rather than Go's randomized map iteration order.
type bundleAsset struct {
	path string
	data []byte
}

// zipBundle writes indexBytes as index.html plus one entry per asset into a
// ZIP archive.
func zipBundle(indexBytes []byte, assets []bundleAsset) ([]byte, error) {
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	idxW, err := zw.Create("index.html")
	if err != nil {
		return nil, fmt.Errorf("httpsrv: create index.html zip entry: %w", err)
	}
	if _, err := idxW.Write(indexBytes); err != nil {
		return nil, fmt.Errorf("httpsrv: write index.html zip entry: %w", err)
	}

	for _, a := range assets {
		aw, err := zw.Create(a.path)
		if err != nil {
			return nil, fmt.Errorf("httpsrv: create %s zip entry: %w", a.path, err)
		}
		if _, err := aw.Write(a.data); err != nil {
			return nil, fmt.Errorf("httpsrv: write %s zip entry: %w", a.path, err)
		}
	}

	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("httpsrv: close bundle zip: %w", err)
	}
	return zipBuf.Bytes(), nil
}

// uniqueBundleAssetPath maps a resolved handle to a stable, filesystem-safe
// relative path under assets/, disambiguating against usedPaths (marked as a
// side effect) when two different handles sanitize to the same base name.
func uniqueBundleAssetPath(handle, mime string, usedPaths map[string]bool) string {
	base := sanitizeBundleAssetName(handle)
	ext := extForBundleMIME(mime)
	candidate := "assets/" + base + ext
	for n := 2; usedPaths[candidate]; n++ {
		candidate = fmt.Sprintf("assets/%s-%d%s", base, n, ext)
	}
	usedPaths[candidate] = true
	return candidate
}

// sanitizeBundleAssetName maps handle to a filesystem-safe base name:
// alphanumerics, '-', and '_' pass through; everything else (incl. '#', the
// tag separator) becomes '_'. Never returns "".
func sanitizeBundleAssetName(handle string) string {
	var b strings.Builder
	for _, r := range handle {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	name := b.String()
	if name == "" {
		return "asset"
	}
	return name
}

// extForBundleMIME returns the file extension for a secondary's MIME type.
// An explicit map (not mime.ExtensionsByType) keeps bundled asset filenames
// deterministic across platforms, mirroring
// pkg/web/webui/artifactview/download.go's extForMIME; unknown types get none.
func extForBundleMIME(mime string) string {
	if i := strings.IndexByte(mime, ';'); i >= 0 { // drop "; charset=…"
		mime = mime[:i]
	}
	switch strings.TrimSpace(strings.ToLower(mime)) {
	case "text/html":
		return ".html"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml":
		return ".svg"
	case "text/css":
		return ".css"
	case "text/plain":
		return ".txt"
	case "application/json":
		return ".json"
	default:
		return ""
	}
}
