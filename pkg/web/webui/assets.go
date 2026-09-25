package webui

import (
	"bytes"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	webassets "github.com/authzed/openagentprimitives/pkg/web/webui/webassets"
)

// manifest is appKey -> built asset URLs, loaded once from the embedded build.
type manifest map[string]webassets.Entry

func loadManifest() (manifest, error) {
	m, err := webassets.Manifest()
	if err != nil {
		return nil, err
	}
	return manifest(m), nil
}

// SetManifestEntryForTest injects a fake manifest entry for app into s, so a
// Page whose App key isn't yet in the committed
// pkg/web/webui/webassets/dist/manifest.json — a new page landing ahead of the
// `mage web:build` that populates its entry — can still be exercised end-to-end
// through the real Server.ServeHTTP, WITHOUT hand-editing the committed,
// generated manifest.json to make a test pass. Mirrors WithSubjectForTest
// (contract.go); production code never calls this.
func (s *Server) SetManifestEntryForTest(app string, entry webassets.Entry) {
	if s.manifest == nil {
		s.manifest = manifest{}
	}
	s.manifest[app] = entry
}

// assetHandler serves /assets/* from the embedded dist with immutable caching
// (filenames are content-hashed). Trusted-origin, AuthNone, GET-only — the
// bytes are public JS/CSS/fonts with no secrets.
func assetHandler() http.Handler {
	sub, _ := fs.Sub(webassets.FS(), "dist")
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/assets")
		fileServer.ServeHTTP(w, r2)
	})
}

// BundleHandler serves ONE built Vite entry from the embedded dist at a
// caller-chosen, STABLE path — the counterpart to assetHandler for bundles that
// cannot be fetched from /assets, which is trusted-origin only: a host page
// rendered on the SANDBOX origin can load only a same-origin script URL, so each
// such bundle gets its own sandbox-origin route. appKey is the entry's key in
// the built manifest ("artifact-host", "mcpui-host"), and each caller mounts the
// result at the matching stable "/<appKey>.js".
//
// A stable path is what forces the ETag to carry the version: the content-hashed
// built filename becomes the ETag, so browsers revalidate whenever the bundle
// changes across builds.
//
// An unbuilt or renamed entry is an ERROR, never a nil handler: callers log it
// and omit the route, so the host page's <script src> 404s — visible and
// diagnosable — rather than the route silently disappearing.
func BundleHandler(appKey string) (http.Handler, error) {
	m, err := webassets.Manifest()
	if err != nil {
		return nil, fmt.Errorf("webui: load manifest: %w", err)
	}
	entry, ok := m[appKey]
	if !ok || len(entry.Scripts) == 0 {
		return nil, fmt.Errorf("webui: manifest has no %q entry (build the UI)", appKey)
	}
	rel := strings.TrimPrefix(entry.Scripts[0], "/assets/") // e.g. artifact-host-ab12.js
	// Built files live directly under "dist"; only the URL carries the "/assets/"
	// prefix. There is no "dist/assets" subdirectory, despite the manifest URL.
	data, err := fs.ReadFile(webassets.FS(), path.Join("dist", rel))
	if err != nil {
		return nil, fmt.Errorf("webui: read %q bundle %q: %w", appKey, rel, err)
	}
	etag := `"` + rel + `"`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
		// The name argument only feeds ServeContent's Content-Type sniffer, which
		// the explicit header above already bypasses. The zero modtime is
		// deliberate: the ETag drives revalidation, and it avoids a wall-clock
		// read.
		http.ServeContent(w, r, appKey+".js", time.Time{}, bytes.NewReader(data))
	}), nil
}
