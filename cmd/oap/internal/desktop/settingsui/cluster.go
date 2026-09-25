package settingsui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/modeltoken"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/settingswizard"
	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/settingseditor"
)

// clusterSettingsFieldManager is the fixed SSA field manager the settings
// editor applies under. It is distinct from "ap-settings-wizard" — the
// manager both settingswizard.Apply and modeltoken.EnsureSecret use — because
// the editor is a different writer and must own its own fields, so an
// editor-authored value is distinguishable from a wizard-authored one in
// managedFields (see clusterSettingsResponse.Managers and the take-ownership
// flow below).
const clusterSettingsFieldManager = "oap-settings-editor"

// clusterSettingsResponse is the GET /api/cluster/settings wire shape: the
// singleton's spec as both typed JSON and raw YAML (the editor UI offers
// both views), plus the distinct set of managedFields managers so the UI can
// warn "also edited by X" before a save. ClusterDown is a STATE (the desktop
// cluster is not up), not an error — see handleClusterSettingsGet.
type clusterSettingsResponse struct {
	ClusterDown bool            `json:"clusterDown"`
	Found       bool            `json:"found"`
	Spec        json.RawMessage `json:"spec,omitempty"`
	YAML        string          `json:"yaml,omitempty"`
	Managers    []string        `json:"managers,omitempty"` // distinct managedFields managers, sorted
}

// clusterSettingsUpdateRequest is the PUT /api/cluster/settings body, and —
// minus Tokens/TakeOwnership, which it simply ignores — the POST
// /api/cluster/settings/validate body too. Exactly one of Spec/YAML must be
// set: the editor UI offers a typed form and a raw-YAML view of the SAME
// document, never both at once.
type clusterSettingsUpdateRequest struct {
	Spec          *v1alpha1.SettingsSpec `json:"spec,omitempty"` // exactly one of Spec/YAML
	YAML          string                 `json:"yaml,omitempty"`
	Tokens        []tokenWrite           `json:"tokens,omitempty"`
	TakeOwnership bool                   `json:"takeOwnership,omitempty"`
}

// tokenWrite is one central model-token Secret write. Written BEFORE the
// spec itself applies, so a model-catalog entry the spec adds can reference
// a token that already exists by the time the apply lands.
type tokenWrite struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Key       string `json:"key"`
	Value     string `json:"value"`
}

// clusterSettingsUpdateResponse is the PUT /api/cluster/settings response.
type clusterSettingsUpdateResponse struct {
	Applied    bool                  `json:"applied"`
	Validation settingseditor.Result `json:"validation"`
	Drift      []string              `json:"drift,omitempty"`
}

// writeJSON is a small helper shared by the cluster-settings handlers: sets
// the content type and status, then best-effort logs an encode failure —
// headers are already sent by then, so logging is all that's left to do
// (see AGENTS.md's no-silent-errors rule).
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.deps.Logf("settingsui: encode response: %v", err)
	}
}

// writeJSONError writes {"error": msg} at status.
func (s *Server) writeJSONError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"error": msg})
}

// specFromUpdateRequest extracts the intended SettingsSpec from a decoded
// request. Exactly one of Spec/YAML must be set — both or neither is a
// caller error, not a "prefer one" default, because silently picking one
// would apply something other than what the user believes they submitted.
// Raw YAML goes through settingseditor.SpecFromYAML, which rejects
// multi-document input and unknown fields.
func specFromUpdateRequest(req clusterSettingsUpdateRequest) (*v1alpha1.SettingsSpec, error) {
	hasSpec := req.Spec != nil
	hasYAML := req.YAML != ""
	if hasSpec == hasYAML {
		return nil, fmt.Errorf("exactly one of spec or yaml must be set")
	}
	if hasSpec {
		return req.Spec, nil
	}
	return settingseditor.SpecFromYAML([]byte(req.YAML))
}

// managersOf returns the distinct, sorted set of managedFields managers on
// an object's ObjectMeta.ManagedFields.
func managersOf(mf []metav1.ManagedFieldsEntry) []string {
	seen := map[string]struct{}{}
	for _, e := range mf {
		if e.Manager != "" {
			seen[e.Manager] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// handleClusterSettingsGet serves the current ClusterAgentSettings singleton
// as both typed JSON and raw YAML, plus its managedFields managers. A down
// cluster is a STATE, not an error: it reports 200 clusterDown:true rather
// than a 5xx, so the settings UI can render "cluster not running" instead of
// a generic error screen — mirrors handleClusterSettingsPut's read side,
// which instead 409s (see its doc) because a write against a down cluster
// IS a caller error.
func (s *Server) handleClusterSettingsGet(w http.ResponseWriter, r *http.Request) {
	b, err := s.deps.Clients()
	if err != nil {
		s.writeJSON(w, http.StatusOK, clusterSettingsResponse{ClusterDown: true})
		return
	}

	cas, err := settingswizard.LoadExisting(r.Context(), b.Controller)
	if err != nil {
		s.deps.Logf("settingsui: load cluster settings: %v", err)
		http.Error(w, "settings: failed to load cluster settings", http.StatusInternalServerError)
		return
	}

	specJSON, err := json.Marshal(cas.Spec)
	if err != nil {
		s.deps.Logf("settingsui: marshal cluster settings spec: %v", err)
		http.Error(w, "settings: failed to encode cluster settings", http.StatusInternalServerError)
		return
	}
	specYAML, err := settingseditor.SpecToYAML(&cas.Spec)
	if err != nil {
		s.deps.Logf("settingsui: render cluster settings YAML: %v", err)
		http.Error(w, "settings: failed to render cluster settings YAML", http.StatusInternalServerError)
		return
	}

	s.writeJSON(w, http.StatusOK, clusterSettingsResponse{
		// LoadExisting returns a zero-value, ResourceVersion-less object when
		// the singleton doesn't exist yet (see its doc) — a real object always
		// carries a non-empty ResourceVersion once read back from the API
		// server (or a fake tracker that actually stored it), so this is a
		// reliable found/not-found signal without a second Get.
		Found:    cas.ResourceVersion != "",
		Spec:     specJSON,
		YAML:     string(specYAML),
		Managers: managersOf(cas.ManagedFields),
	})
}

// handleClusterSettingsValidate runs settingseditor.Validate over the
// request's spec/yaml WITHOUT ever touching the cluster (it never calls
// Deps.Clients) — it must keep working while the cluster is down, since
// it's how the editor gives live feedback before the user attempts to save.
func (s *Server) handleClusterSettingsValidate(w http.ResponseWriter, r *http.Request) {
	var req clusterSettingsUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("settings: decode request body: %v", err), http.StatusBadRequest)
		return
	}
	spec, err := specFromUpdateRequest(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("settings: %v", err), http.StatusBadRequest)
		return
	}

	res := settingseditor.Validate(spec, true)
	status := http.StatusOK
	if len(res.Errors) > 0 {
		status = http.StatusUnprocessableEntity
	}
	s.writeJSON(w, status, res)
}

// handleClusterSettingsPut validates the request first — a failing
// validation 422s with nothing written, no exceptions, per AGENTS.md's
// server-side-apply purity rule and the brief's "nothing written" gate. Only
// once validation passes does it look at the cluster at all: a down cluster
// 409s (a write against a down cluster is a caller error; contrast the GET
// side, which reports the same condition as a 200 state). Then it writes any
// token Secrets, then the spec itself, then reads back the result and
// reports drift.
//
// The spec write takes one of two forms:
//
//   - Default (TakeOwnership false): an SSA apply under the editor's own
//     field manager (applyClusterSettings). SSA deliberately cannot remove a
//     field the payload omits but a DIFFERENT manager owns — such fields
//     survive and are reported as drift (see settingseditor.Drift's doc), so
//     an ordinary save never silently destroys another writer's config.
//   - TakeOwnership true: a whole-spec-REPLACING Update
//     (replaceClusterSettings). The user has explicitly said "make the
//     cluster match my editor exactly", and only a replace can honor that —
//     see replaceClusterSettings's doc for why an SSA-shaped mechanism
//     structurally cannot.
func (s *Server) handleClusterSettingsPut(w http.ResponseWriter, r *http.Request) {
	var req clusterSettingsUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("settings: decode request body: %v", err), http.StatusBadRequest)
		return
	}
	spec, err := specFromUpdateRequest(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("settings: %v", err), http.StatusBadRequest)
		return
	}

	res := settingseditor.Validate(spec, true)
	if len(res.Errors) > 0 {
		s.writeJSON(w, http.StatusUnprocessableEntity, clusterSettingsUpdateResponse{Validation: res})
		return
	}

	b, err := s.deps.Clients()
	if err != nil {
		s.writeJSONError(w, http.StatusConflict, "cluster not running")
		return
	}
	ctx := r.Context()

	for _, tok := range req.Tokens {
		ref := modeltoken.SecretRef{Namespace: tok.Namespace, Name: tok.Name, Key: tok.Key}
		if err := modeltoken.EnsureSecret(ctx, b.Dynamic, ref, tok.Value); err != nil {
			s.deps.Logf("settingsui: ensure model token secret %s/%s: %v", tok.Namespace, tok.Name, err)
			http.Error(w, fmt.Sprintf("settings: write token secret %s/%s: %v", tok.Namespace, tok.Name, err), http.StatusInternalServerError)
			return
		}
	}

	if req.TakeOwnership {
		if err := s.replaceClusterSettings(ctx, b, spec); err != nil {
			s.deps.Logf("settingsui: replace cluster settings: %v", err)
			http.Error(w, fmt.Sprintf("settings: replace cluster settings: %v", err), http.StatusInternalServerError)
			return
		}
	} else if err := s.applyClusterSettings(ctx, b.Dynamic, spec); err != nil {
		s.deps.Logf("settingsui: apply cluster settings: %v", err)
		http.Error(w, fmt.Sprintf("settings: apply cluster settings: %v", err), http.StatusInternalServerError)
		return
	}

	drift, err := s.readBackAndDiff(ctx, b, spec)
	if err != nil {
		s.deps.Logf("settingsui: %v", err)
		http.Error(w, fmt.Sprintf("settings: %v", err), http.StatusInternalServerError)
		return
	}

	s.writeJSON(w, http.StatusOK, clusterSettingsUpdateResponse{Applied: true, Validation: res, Drift: drift})
}

// readBackAndDiff re-reads the ClusterAgentSettings singleton and computes
// Drift against the intended spec. Factored out because the take-ownership
// path runs it twice.
func (s *Server) readBackAndDiff(ctx context.Context, b *kube.Bundle, intended *v1alpha1.SettingsSpec) ([]string, error) {
	readback, err := settingswizard.LoadExisting(ctx, b.Controller)
	if err != nil {
		return nil, fmt.Errorf("read back cluster settings: %w", err)
	}
	drift, err := settingseditor.Drift(intended, &readback.Spec)
	if err != nil {
		return nil, fmt.Errorf("compute drift: %w", err)
	}
	return drift, nil
}

// applyClusterSettings SSA-applies the full spec as a ClusterAgentSettings,
// under the editor's fixed field manager. The applied object is a pure
// function of spec — no timestamps, no random values — and status is
// stripped before applying, since a client never applies status (see
// AGENTS.md, "Server-side apply: keep applied fields idempotent").
func (s *Server) applyClusterSettings(ctx context.Context, dyn dynamic.Interface, spec *v1alpha1.SettingsSpec) error {
	cas := &v1alpha1.ClusterAgentSettings{
		TypeMeta:   metav1.TypeMeta{APIVersion: "agentprimitives.authzed.com/v1alpha1", Kind: "ClusterAgentSettings"},
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ClusterAgentSettingsName},
		Spec:       *spec,
	}
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cas)
	if err != nil {
		return fmt.Errorf("convert ClusterAgentSettings to unstructured: %w", err)
	}
	unstructured.RemoveNestedField(u, "status")
	unstructured.RemoveNestedField(u, "metadata", "creationTimestamp")
	return s.applyFn(ctx, dyn, &unstructured.Unstructured{Object: u}, clusterSettingsFieldManager)
}

// replaceClusterSettings is the TakeOwnership write: a whole-spec-REPLACING
// Update under the editor's field owner, not an SSA apply. The distinction
// is structural: SSA cannot remove a field the intended spec omits but a
// different manager keeps alive, and clearing managedFields before
// re-applying the same omitting payload does not change that — it only
// reassigns the surviving field's OWNERSHIP (to the apiserver's synthetic
// "before-first-apply" bucket), never deletes its VALUE. "Make the cluster
// match my editor exactly" therefore needs PUT semantics: an Update replaces
// the spec wholesale, so omitted fields are deleted and field ownership of
// the entire spec transfers to the editor's manager in one write.
//
// Only Spec is replaced; the live object's metadata (labels, annotations,
// other writers' non-spec state) is preserved as read. When the singleton
// does not exist yet there is nothing to take ownership OF, so this falls
// back to the normal SSA apply path, which creates it.
func (s *Server) replaceClusterSettings(ctx context.Context, b *kube.Bundle, spec *v1alpha1.SettingsSpec) error {
	var cur v1alpha1.ClusterAgentSettings
	err := b.Controller.Get(ctx, ctrlclient.ObjectKey{Name: v1alpha1.ClusterAgentSettingsName}, &cur)
	if apierrors.IsNotFound(err) {
		return s.applyClusterSettings(ctx, b.Dynamic, spec)
	}
	if err != nil {
		return fmt.Errorf("get ClusterAgentSettings %q: %w", v1alpha1.ClusterAgentSettingsName, err)
	}
	cur.Spec = *spec
	if err := b.Controller.Update(ctx, &cur, ctrlclient.FieldOwner(clusterSettingsFieldManager)); err != nil {
		return fmt.Errorf("replace ClusterAgentSettings %q spec: %w", v1alpha1.ClusterAgentSettingsName, err)
	}
	return nil
}
