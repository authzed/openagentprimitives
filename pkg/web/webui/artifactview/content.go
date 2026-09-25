package artifactview

import (
	"net/http"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// ensureUTF8Charset appends "; charset=utf-8" to a text/* content type that has
// no explicit charset. The renderer emits UTF-8 but strips the <meta charset>
// tag during head/meta sanitization, and the content handler sends nosniff — so
// without an explicit HTTP charset the browser falls back to its locale default
// (Latin-1/Windows-1252) and renders UTF-8 bytes as mojibake (e.g. "·" → "Â·").
func ensureUTF8Charset(mime string) string {
	if !strings.HasPrefix(mime, "text/") {
		return mime
	}
	if strings.Contains(strings.ToLower(mime), "charset=") {
		return mime
	}
	return mime + "; charset=utf-8"
}

func contentHandler(av Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ns, sess, renderName, err := av.VerifyContentToken(r.URL.Query().Get("ct"))
		if err != nil {
			webui.RenderInlineError(w, http.StatusForbidden, "Link expired", "This content link is invalid or has expired. Reopen the live view to continue.")
			return
		}
		out, mime, err := av.FetchRender(r.Context(), ns, sess, renderName)
		if err != nil {
			// The user already sees the 502 page, but the REAL reason (the
			// operator's status + body, carried on the error) would otherwise
			// exist nowhere, leaving a blank live-view frame undiagnosable.
			av.Logger().Error(err, "artifactview content: FetchRender errored; returning 502",
				"render", renderName, "session", ns+"/"+sess)
			webui.RenderInlineError(w, http.StatusBadGateway, "Could not load content", "The artifact render is temporarily unavailable. Please retry.")
			return
		}
		if mime == "" {
			mime = "text/html; charset=utf-8"
		}
		mime = ensureUTF8Charset(mime)
		// The artifact kind's live-view serve transform, owned by the registered
		// renderer (every kind is a no-op today — the artifact is served inert as
		// the host page's script-disabled inner frame). This handler dispatches;
		// it never branches on the kind.
		out = av.ServeTransform(r.Context(), ns, sess, renderName, out)
		// Rewrite artifact:HANDLE refs to token-gated /artifacts/a/ URLs.
		// rewriteArtifactRefs dispatches generically by render kind, so a kind
		// with no reference concept returns content unchanged and no mime-based
		// gate is needed here. A render-kind lookup failure degrades to the
		// untransformed bytes (logged): un-rewritten is more useful than unserved.
		rewritten, rerr := rewriteArtifactRefs(r.Context(), av, ns, sess, renderName, out)
		if rerr != nil {
			av.Logger().Error(rerr, "artifactview: rewriteArtifactRefs failed; serving unrewritten",
				"render", renderName, "session", ns+"/"+sess)
		} else {
			out = rewritten
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(out)
	})
}
