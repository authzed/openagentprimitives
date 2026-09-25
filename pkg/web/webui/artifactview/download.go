package artifactview

import (
	"errors"
	"net/http"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// downloadHandler serves an artifact's bytes as a direct browser download,
// through the same bindView choke point as the shell and revision routes — the
// same link verification, CheckView gate, and artifact→session resolution — then
// streams the render with Content-Disposition: attachment.
//
// A downloaded file leaves webd entirely, so an html primary's `artifact:HANDLE`
// refs must be bundled INTO the download rather than left as dangling links (the
// live-view instead rewrites them to same-origin /artifacts/a/ URLs).
// FetchRenderBundle does exactly that: a ref-bearing html primary comes back as
// a self-contained ZIP, anything else comes back unchanged.
//
// SECURITY: the bytes are untrusted, agent-generated content (e.g. arbitrary
// HTML). Content-Disposition: attachment forces the browser to DOWNLOAD rather
// than render them, so they never execute in webd's trusted origin; nosniff
// blocks MIME-sniffing a text/plain body into executable HTML. That pairing is
// why a download may be served from the trusted origin at all, while inline
// render is sandbox-origin only.
func downloadHandler(av Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		b, denial := bindView(r.Context(), av, q)
		if denial != nil {
			denial.writeHTTP(w)
			return
		}
		artifactID, ns, sess, sessionRef := b.artifactID, b.ns, b.sess, b.sessionRef()
		// ?rev pins a specific revision; absent, the newest render is served.
		// Either way the bytes come from the session bindView RESOLVED, never a
		// caller-named one — hence a pinned revision is looked up in the binding's
		// own revision list rather than re-listed against a requested session.
		var renderName string
		var err error
		if rev := q.Get("rev"); rev != "" {
			for _, rm := range b.revisions {
				if rm.RevisionID == rev {
					renderName = rm.RenderName
					break
				}
			}
			if renderName == "" {
				http.Error(w, "revision not found", http.StatusNotFound)
				return
			}
		} else {
			renderName, err = av.ResolveRender(r.Context(), ns, sess, artifactID)
			if err != nil {
				if errors.Is(err, artifacts.ErrNotFound) {
					http.Error(w, "artifact unavailable", http.StatusNotFound)
					return
				}
				av.Logger().Error(err, "artifactview download: ResolveRender errored; returning 500",
					"artifactID", artifactID, "sessionRef", sessionRef)
				http.Error(w, "could not resolve the artifact render", http.StatusInternalServerError)
				return
			}
		}
		// The bundle route, not FetchRender: a download must be self-contained.
		out, mime, err := av.FetchRenderBundle(r.Context(), ns, sess, renderName)
		if err != nil {
			// The error carries the operator's status + body; a bare 502 with no
			// log is the silent failure that makes a broken download
			// undiagnosable.
			av.Logger().Error(err, "artifactview download: FetchRenderBundle errored; returning 502",
				"artifactID", artifactID, "sessionRef", sessionRef, "renderName", renderName)
			http.Error(w, "could not load the artifact", http.StatusBadGateway)
			return
		}
		if mime == "" {
			mime = "application/octet-stream"
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", `attachment; filename="`+downloadFilename(q.Get("fn"), mime, artifactID)+`"`)
		_, _ = w.Write(out)
	})
}

// downloadFilename picks the saved filename: the (sanitized) ?fn hint when
// present — the attachment's own filename, which does not affect authorization,
// so an unsigned value is fine once sanitized — else the artifact id, with a
// MIME-derived extension appended when the base has none.
//
// application/zip is the one MIME whose extension REPLACES an existing one
// rather than filling in a missing one: ?fn is minted client-side from the
// artifact's own (non-bundled) kind, while the bundle-vs-raw decision is made
// server-side per request, so ZIP bytes can arrive under a stale "report.html"
// hint. Saving them under ".html" yields a file that opens as neither, so the
// ACTUAL delivered MIME wins.
func downloadFilename(fn, mime, artifactID string) string {
	base := sanitizeFilename(fn)
	if base == "" {
		base = sanitizeFilename(artifactID)
	}
	if base == "" {
		base = "artifact"
	}
	ext := extForMIME(mime)
	if ext == ".zip" {
		return replaceExt(base, ext)
	}
	if !strings.Contains(base, ".") && ext != "" {
		base += ext
	}
	return base
}

// replaceExt swaps base's existing extension (everything from the last '.'
// onward, when present) for ext; with no existing extension it simply
// appends ext.
func replaceExt(base, ext string) string {
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	return base + ext
}

// sanitizeFilename strips anything that could break the Content-Disposition
// header or escape a directory: control chars (incl CR/LF), the quote/backslash
// that would terminate filename="…", and path separators. The result is a bare
// filename safe to embed verbatim.
func sanitizeFilename(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20: // control chars, incl CR/LF header-injection
			return -1
		case r == '"' || r == '\\' || r == '/' || r == ':':
			return -1
		default:
			return r
		}
	}, s)
	return strings.TrimSpace(s)
}

// extForMIME returns the canonical file extension for the renderer MIME types
// the platform ships. An explicit map (not mime.ExtensionsByType) keeps the
// downloaded filename deterministic across platforms; unknown types get none.
func extForMIME(mime string) string {
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
	case "image/svg+xml":
		return ".svg"
	case "text/css":
		return ".css"
	case "text/plain":
		return ".txt"
	case "application/json":
		return ".json"
	case "application/zip":
		return ".zip"
	default:
		return ""
	}
}
