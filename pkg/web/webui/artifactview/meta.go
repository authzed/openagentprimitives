package artifactview

import (
	"encoding/json"
	"net/http"
)

// artifactMetaResponse is what /artifact-view/meta answers: the artifact's
// name and its newest revision's file facts — what ap:attachment shows as a
// file row before the person clicks download.
type artifactMetaResponse struct {
	Name     string `json:"name"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	MIME     string `json:"mime"`
}

// metaHandler serves /artifact-view/meta. Gated by bindView exactly as the
// download is — the artifact must belong to the session named and the viewer
// must hold view on it — so a page fill naming another session's artifact is
// refused the same way, and the browser never learns a file's name for an
// artifact it could not download.
func metaHandler(av Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, denial := bindView(r.Context(), av, r.URL.Query())
		if denial != nil {
			denial.writeHTTP(w)
			return
		}
		if len(b.revisions) == 0 {
			http.Error(w, "artifact unavailable", http.StatusNotFound)
			return
		}
		newest := b.revisions[len(b.revisions)-1]
		name, _, err := av.ArtifactMeta(r.Context(), b.ns, b.sess, b.artifactID)
		if err != nil {
			av.Logger().Error(err, "artifactview meta: ArtifactMeta errored; returning 500",
				"artifactID", b.artifactID, "sessionRef", b.sessionRef())
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(artifactMetaResponse{Name: name, Filename: newest.Filename, Size: newest.Size, MIME: newest.MIME})
	})
}
