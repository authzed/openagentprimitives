package admind

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// artifactRow is one ArtifactRender projected for the Audit › Artifacts table.
// Field names are the JSON tags the ArtifactsView consumes.
type artifactRow struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Session is "ns/name" of the owning AgentSession (the controller owner
	// reference stamped at create time), or "" when the link is absent.
	Session string `json:"session"`
	Kind    string `json:"kind"`  // spec.kind — the renderer plug-in
	Phase   string `json:"phase"` // status.phase — Pending/Rendering/Ready/Failed
	MIME    string `json:"mime"`  // status.outputMIME
	Size    int64  `json:"size"`  // status.outputSize
	// ArtifactID is the logical artifact id (the artifact-id label) shared by
	// every revision of one artifact; falls back to the render's own name when
	// unlabeled. The UI groups revisions into git-graph "tracks" by (namespace,
	// artifactId).
	ArtifactID string `json:"artifactId"`
	// Revisions is the number of renders that share this artifact's logical id
	// label (the CR status exposes no revision counter). 1 when unlabeled.
	Revisions int    `json:"revisions"`
	Created   string `json:"created"` // metadata.creationTimestamp, RFC3339
}

// ownerSession returns "ns/name" of the ArtifactRender's owning AgentSession,
// resolved from the controller owner reference the runner stamps at create
// time (artifact_prepare / artifact_offer_view). "" when no such owner exists.
func ownerSession(ar *spiceboxv1alpha1.ArtifactRender) string {
	for _, o := range ar.OwnerReferences {
		if o.Kind == "AgentSession" {
			return ar.Namespace + "/" + o.Name
		}
	}
	return ""
}

// handleArtifacts lists every ArtifactRender cluster-wide via the cached client
// (the operator watches artifactrenders) newest-first. Revisions per logical
// artifact are derived from the artifact-id label grouping, since the CR status
// carries no revision count.
func (a *Admind) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	var list spiceboxv1alpha1.ArtifactRenderList
	if err := a.cfg.K8s.List(r.Context(), &list); err != nil {
		a.cfg.Logger.Info("admind: artifacts list failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "artifacts list failed: "+err.Error())
		return
	}

	// Count renders per (namespace, artifact-id) so each row can report how many
	// revisions its logical artifact has.
	revsByArtifact := map[string]int{}
	for i := range list.Items {
		if id := list.Items[i].Labels[artifacts.LabelArtifactID]; id != "" {
			revsByArtifact[list.Items[i].Namespace+"/"+id]++
		}
	}

	items := list.Items
	sort.Slice(items, func(i, j int) bool {
		ti, tj := items[i].CreationTimestamp.Time, items[j].CreationTimestamp.Time
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return items[i].Name < items[j].Name
	})

	rows := make([]artifactRow, 0, len(items))
	for i := range items {
		it := &items[i]
		revs := 1
		artifactID := it.Name // unlabeled render is its own single-revision track
		if id := it.Labels[artifacts.LabelArtifactID]; id != "" {
			revs = revsByArtifact[it.Namespace+"/"+id]
			artifactID = id
		}
		rows = append(rows, artifactRow{
			Name:       it.Name,
			Namespace:  it.Namespace,
			Session:    ownerSession(it),
			Kind:       it.Spec.Kind,
			Phase:      string(it.Status.Phase),
			MIME:       it.Status.OutputMIME,
			Size:       it.Status.OutputSize,
			ArtifactID: artifactID,
			Revisions:  revs,
			Created:    it.CreationTimestamp.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, rows)
}

// artifactRevision is one ArtifactRender that shares a logical artifact's id
// label — a revision of that artifact. Field names are the JSON tags the
// Artifact detail view consumes.
type artifactRevision struct {
	Name    string `json:"name"`
	Phase   string `json:"phase"` // status.phase — Pending/Rendering/Ready/Failed
	MIME    string `json:"mime"`  // status.outputMIME
	Size    int64  `json:"size"`  // status.outputSize, in bytes; 0 until Ready
	Created string `json:"created"`
}

// artifactDetail is the GET /admin/v1/artifacts/{ns}/{name} payload: the CR's
// own metadata, every render sharing its artifact-id label (newest-first), a
// best-effort viewer path, and the content ref for a future download.
type artifactDetail struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Session is "ns/name" of the owning AgentSession, or "" when absent.
	Session string `json:"session"`
	Kind    string `json:"kind"`  // spec.kind — the renderer plug-in
	Phase   string `json:"phase"` // status.phase — Pending/Rendering/Ready/Failed
	MIME    string `json:"mime"`  // status.outputMIME
	Size    int64  `json:"size"`  // status.outputSize, in bytes; 0 until Ready
	Created string `json:"created"`
	// Revisions is every render sharing this artifact-id label, NEWEST FIRST;
	// degrades to this CR alone when the sibling list fails.
	Revisions []artifactRevision `json:"revisions"`
	// ViewPath is the platform-admin live-view URL for this artifact:
	// /artifact-view?artifactId=<logical id>&sessionRef=<ns/name>. The viewer
	// authenticates the admin via their IdP cookie and authorizes via CheckView —
	// a platform admin views any artifact through the SpiceDB platform->view_audit
	// tie — so no signed passthrough link is needed, just the logical artifact id
	// and owning-session ref as plain query params.
	ViewPath string `json:"viewPath"`
	// OutputRef is status.OutputRef — the artifactstore content ref the frontend
	// links for download. Empty until the render reaches Ready.
	OutputRef string `json:"outputRef"`
}

// artifactViewPath builds the platform-admin live-view URL for an artifact. The
// viewer authenticates the admin via their IdP cookie and authorizes via
// CheckView (a platform admin views any artifact through the SpiceDB
// platform->view_audit tie), so no signed link is needed — the logical artifact
// id and the owning-session ref ("ns/name") ride as plain query params. When
// sessionRef is "" (no owning AgentSession) the link is unusable, but the
// viewer's CheckView rejects it rather than mis-serving.
func artifactViewPath(artifactID, sessionRef string) string {
	return "/artifact-view?artifactId=" + url.QueryEscape(artifactID) +
		"&sessionRef=" + url.QueryEscape(sessionRef)
}

// revisionRow projects an ArtifactRender into a revision row.
func revisionRow(ar *spiceboxv1alpha1.ArtifactRender) artifactRevision {
	return artifactRevision{
		Name:    ar.Name,
		Phase:   string(ar.Status.Phase),
		MIME:    ar.Status.OutputMIME,
		Size:    ar.Status.OutputSize,
		Created: ar.CreationTimestamp.UTC().Format(time.RFC3339),
	}
}

// handleArtifactDetail serves GET /admin/v1/artifacts/{ns}/{name}: fetch the
// ArtifactRender, then list its sibling revisions (every render sharing its
// artifact-id label) newest-first. NotFound → 404. A revisions-list failure is
// logged and degrades to the single CR as its own sole revision rather than
// failing the whole request.
func (a *Admind) handleArtifactDetail(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var ar spiceboxv1alpha1.ArtifactRender
	if err := a.cfg.K8s.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &ar); err != nil {
		if apierrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("artifact %s/%s not found", ns, name))
			return
		}
		a.cfg.Logger.Info("admind: artifact detail get failed", "artifact", ns+"/"+name, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "artifact get failed: "+err.Error())
		return
	}

	id := ar.Labels[artifacts.LabelArtifactID]
	// logicalArtifactID is the id the viewer resolves (GetHead / CheckView): the
	// artifact-id label shared by every revision, or the render's own name when
	// unlabeled (matching the Artifacts table's artifactId column).
	logicalArtifactID := id
	if logicalArtifactID == "" {
		logicalArtifactID = ar.Name
	}

	detail := artifactDetail{
		Name:      ar.Name,
		Namespace: ar.Namespace,
		Session:   ownerSession(&ar),
		Kind:      ar.Spec.Kind,
		Phase:     string(ar.Status.Phase),
		MIME:      ar.Status.OutputMIME,
		Size:      ar.Status.OutputSize,
		Created:   ar.CreationTimestamp.UTC().Format(time.RFC3339),
		OutputRef: ar.Status.OutputRef,
		ViewPath:  artifactViewPath(logicalArtifactID, ownerSession(&ar)),
		Revisions: []artifactRevision{},
	}

	if id == "" {
		// Unlabeled render: it is its own sole revision.
		detail.Revisions = append(detail.Revisions, revisionRow(&ar))
		writeJSON(w, http.StatusOK, detail)
		return
	}

	var list spiceboxv1alpha1.ArtifactRenderList
	if err := a.cfg.K8s.List(r.Context(), &list,
		client.InNamespace(ns), client.MatchingLabels{artifacts.LabelArtifactID: id}); err != nil {
		// A revisions-list error must not sink the detail — log and fall back to
		// the single CR as its own sole revision.
		a.cfg.Logger.Info("admind: artifact revisions list failed",
			"artifact", ns+"/"+name, "artifactID", id, "err", err.Error())
		detail.Revisions = append(detail.Revisions, revisionRow(&ar))
		writeJSON(w, http.StatusOK, detail)
		return
	}

	revs := list.Items
	sort.Slice(revs, func(i, j int) bool {
		ti, tj := revs[i].CreationTimestamp.Time, revs[j].CreationTimestamp.Time
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return revs[i].Name < revs[j].Name
	})
	for i := range revs {
		detail.Revisions = append(detail.Revisions, revisionRow(&revs[i]))
	}
	writeJSON(w, http.StatusOK, detail)
}
