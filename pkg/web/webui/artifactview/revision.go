package artifactview

import (
	"encoding/json"
	"net/http"
)

func revisionHandler(av Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// The client forwards window.location.search verbatim, so whichever param
		// form the shell was opened with reaches here; bindView gates the result
		// and resolves the session the pinned revision is looked up in.
		b, denial := bindView(r.Context(), av, q)
		if denial != nil {
			denial.writeHTTP(w)
			return
		}
		artifactID, ns, sess, sessionRef := b.artifactID, b.ns, b.sess, b.sessionRef()
		rev := q.Get("rev")
		// bindView's probe already listed this artifact's revisions in the bound
		// session; the pinned revision is found in that same list.
		revs := b.revisions
		idx := -1
		for i := range revs {
			if revs[i].RevisionID == rev {
				idx = i
				break
			}
		}
		if idx < 0 {
			http.Error(w, "revision not found", http.StatusNotFound)
			return
		}

		// Resolve the render to FRAME for this pinned revision. The NEWEST revision
		// goes through ContentRender so a bundled-only kind (svg/css) frames its
		// internal html preview child — raw bytes are NEVER framed — and yields no
		// content URL while that preview is still generating (the client tolerates
		// an empty contentUrl). For an older revision, a standalone artifact serves
		// its own render directly and a bundled-only one resolves THAT revision's
		// own preview child, so the empty-URL path is reached only while a
		// bundled-only revision's preview is still generating.
		var hostURL, url string
		var err error
		if idx == len(revs)-1 {
			rn, ready, cerr := av.ContentRender(r.Context(), ns, sess, artifactID)
			if cerr != nil {
				av.Logger().Error(cerr, "artifactview revision: ContentRender errored; returning 500",
					"artifactID", artifactID, "sessionRef", sessionRef)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if ready && rn != "" {
				if hostURL, url, err = urlsFor(av, ns, sess, rn, artifactID); err != nil {
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
			}
		} else if av.RenderIsBundledOnly(r.Context(), ns, sess, artifactID) {
			// Older revision of a bundled-only artifact: frame ITS OWN preview child
			// (never the raw svg/css bytes). url stays "" while that revision's
			// preview is still generating — the client keeps the current frame.
			rn, ready, perr := av.PreviewChildRender(r.Context(), ns, sess, revs[idx].RevisionID)
			if perr != nil {
				av.Logger().Error(perr, "artifactview revision: PreviewChildRender errored; returning 500",
					"artifactID", artifactID, "sessionRef", sessionRef, "revisionID", revs[idx].RevisionID)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if ready && rn != "" {
				if hostURL, url, err = urlsFor(av, ns, sess, rn, artifactID); err != nil {
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
			}
		} else {
			// Older revision of a standalone artifact: serve its render directly.
			if hostURL, url, err = urlsFor(av, ns, sess, revs[idx].RenderName, artifactID); err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"hostUrl": hostURL, "contentUrl": url})
	})
}
