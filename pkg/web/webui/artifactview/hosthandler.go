package artifactview

import (
	"net/http"
	"slices"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// annotationBatchInteraction is the session_views interaction kind that grants
// the browser annotator UI (pkg/channels/interact's annotationBatchKind.Name()).
// A literal on both sides, matching the trusted-origin shell's own gate: this is
// a wire-format string, not something either side exports as a shared constant.
const annotationBatchInteraction = "annotation_batch"

// hostHandler serves the sandbox-origin host page for a render. It verifies the
// SAME content-capability token /content verifies — the host page self-embeds
// /content?ct=<that token>, so authorization is enforced once at the token
// boundary and the inner frame re-verifies on its own fetch. AuthNone: the
// capability token IS the authorization, exactly like /content.
func hostHandler(av Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct := r.URL.Query().Get("ct")
		ns, sess, renderName, err := av.VerifyContentToken(ct)
		if err != nil {
			webui.RenderInlineError(w, http.StatusForbidden, "Link expired",
				"This content link is invalid or has expired. Reopen the live view to continue.")
			return
		}
		// UX only — the real gate is /interact's own session_views check
		// (pkg/channels/interact). This cannot bypass that check; it only avoids
		// shipping a control (and the annotator bundle) the server would refuse.
		// A resolution failure or absence degrades to annotate=false.
		annotate := slices.Contains(av.SessionViews(r.Context(), ns, sess), annotationBatchInteraction)
		body, csp, err := hostPage(ct, av.TrustedOrigin(), annotate)
		if err != nil {
			av.Logger().Error(err, "artifactview hosthandler: hostPage failed; returning 500",
				"ns", ns, "session", sess, "render", renderName)
			webui.RenderInlineError(w, http.StatusInternalServerError, "Could not load content",
				"The live view is temporarily unavailable. Please retry.")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", csp)
		_, _ = w.Write(body)
	})
}
