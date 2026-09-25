package artifactview

import (
	"net/http"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// assetHandler serves a same-session SECONDARY artifact's raw bytes at
// /artifacts/a/ — the target of a rewritten `artifact:HANDLE` reference in a
// primary's live-view HTML (img[src]/link[href]). Sandbox origin, cookie-less,
// AuthNone, gated ENTIRELY by the asset capability token, which is minted
// server-side during the primary's serve-time rewrite (ResolveAssetURL), never
// by the browser.
//
// Unlike downloadHandler this sets NO Content-Disposition: attachment — the
// browser is fetching inline (an <img> or <link rel=stylesheet>). Safe for the
// same reason /content is: the sandbox origin is isolated (separate hostname, no
// cookie, its own restrictive CSP / sandboxed iframe), so bytes rendered here
// cannot act on the trusted origin or its session.
//
// AUTHORIZATION INVARIANT — no per-secondary CheckView, on purpose:
// `artifact#view` is SESSION-granular in the SpiceDB schema
// (pkg/authz/spicedb/schema/schema.zed: `view = parent->interact +
// platform->view_audit`; every artifact's `parent` is its owning agentsession,
// with no per-artifact ACL). The token is minted only during a primary serve
// that already passed CheckView on the trusted origin, and only for a secondary
// in the SAME (ns, sess), so re-checking here would reduce to the identical
// result. If the schema ever adds a per-artifact `view` component, this route
// MUST add a per-secondary CheckView and the mint path must re-check — the
// same-session token alone would no longer suffice.
func assetHandler(av Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ns, sess, renderName, err := av.VerifyAssetToken(r.URL.Query().Get("ct"))
		if err != nil {
			webui.RenderInlineError(w, http.StatusForbidden, "Link expired", "This asset link is invalid or has expired.")
			return
		}
		out, mime, err := av.FetchRender(r.Context(), ns, sess, renderName)
		if err != nil {
			// The broken <img>/<link> is visible to the user, but the
			// operator-side reason for it would otherwise be dropped entirely.
			av.Logger().Error(err, "artifactview asset: FetchRender errored; returning 502",
				"render", renderName, "session", ns+"/"+sess)
			webui.RenderInlineError(w, http.StatusBadGateway, "Could not load asset", "The referenced artifact is temporarily unavailable. Please retry.")
			return
		}
		if mime == "" {
			mime = "application/octet-stream"
		}
		// nosniff below means a missing HTTP charset on text/* content falls back
		// to the browser's locale default rather than UTF-8.
		mime = ensureUTF8Charset(mime)
		w.Header().Set("Content-Type", mime)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(out)
	})
}
