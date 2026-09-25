package admind

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/channelplan"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
)

// maxWorkshopInstallBody caps the workshop-install POST body. Unlike
// oap-install, this body NEVER carries a bundle upload — the bundle bytes come
// from the artifactstore by the workshop-recorded ref — so it holds only the
// install target, answer values, and adopt list, and 1 MiB is generous.
const maxWorkshopInstallBody = 1 << 20 // 1 MiB

// workshopRow is one Workshop projected for the admin Workshops table. Field
// names are the JSON tags the WorkshopsView consumes. Nothing here carries a
// secret: it is namespace/name/starter metadata and the install/capability
// request STATE, never any answered value.
type workshopRow struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Session is "ns/name" of the builder AgentSession this workshop belongs to.
	Session string `json:"session"`
	// Starter is the canonical id of the person who started the builder session.
	Starter string `json:"starter"`
	// Phase is the workshop lifecycle phase (status.phase), not the install phase.
	Phase string `json:"phase"`
	// InstallPhase is status.install.phase (Requested→Installed/Declined/Failed),
	// or "" when no install has been requested or observed yet.
	InstallPhase string `json:"installPhase,omitempty"`
	// InstalledRef is "<namespace>/<name>" of the installed AgentClass, set once
	// an install succeeds.
	InstalledRef string `json:"installedRef,omitempty"`
	// SuggestedName is the name the builder proposed for the installed agent.
	SuggestedName string `json:"suggestedName,omitempty"`
	// PendingInstall is true when the builder asked to install and no admin has
	// reached a terminal decision (Installed or Declined) yet — the workshops
	// the page surfaces as awaiting action.
	PendingInstall bool `json:"pendingInstall"`
	// PendingCapability is true when the builder recommended a new capability.
	PendingCapability bool `json:"pendingCapability"`
	// Exported is true once the workshop has a drafted bundle in the store.
	Exported bool `json:"exported"`
	// Created is metadata.creationTimestamp, RFC3339.
	Created string `json:"created"`
}

// handleWorkshopList lists every Workshop cluster-wide (the operator watches
// workshops) newest-first, projecting only non-secret metadata + request state.
func (a *Admind) handleWorkshopList(w http.ResponseWriter, r *http.Request) {
	var list spiceboxv1alpha1.WorkshopList
	if err := a.cfg.K8s.List(r.Context(), &list); err != nil {
		a.cfg.Logger.Info("admind: workshops list failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "workshops list failed: "+err.Error())
		return
	}

	items := list.Items
	sort.Slice(items, func(i, j int) bool {
		ti, tj := items[i].CreationTimestamp.Time, items[j].CreationTimestamp.Time
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		if items[i].Namespace != items[j].Namespace {
			return items[i].Namespace < items[j].Namespace
		}
		return items[i].Name < items[j].Name
	})

	rows := make([]workshopRow, 0, len(items))
	for i := range items {
		rows = append(rows, projectWorkshop(&items[i]))
	}
	writeJSON(w, http.StatusOK, rows)
}

// projectWorkshop builds one table row from a Workshop.
func projectWorkshop(ws *spiceboxv1alpha1.Workshop) workshopRow {
	row := workshopRow{
		Namespace:         ws.Namespace,
		Name:              ws.Name,
		Session:           ws.Spec.Session.Namespace + "/" + ws.Spec.Session.Name,
		Starter:           ws.Spec.StarterCanonical,
		Phase:             ws.Status.Phase,
		Exported:          ws.Status.Export != nil,
		PendingInstall:    ws.Spec.InstallRequest != nil && installPending(ws.Status.Install),
		PendingCapability: ws.Spec.CapabilityRequest != nil,
		Created:           ws.CreationTimestamp.UTC().Format(time.RFC3339),
	}
	if ws.Status.Install != nil {
		row.InstallPhase = ws.Status.Install.Phase
		row.InstalledRef = ws.Status.Install.InstalledRef
	}
	if ws.Spec.InstallRequest != nil {
		row.SuggestedName = ws.Spec.InstallRequest.SuggestedName
	}
	return row
}

// installPending reports whether an install still awaits an admin's terminal
// decision. Installed and Declined are terminal; Requested, Approved, Failed
// (retryable) and an absent status all still need attention.
func installPending(st *spiceboxv1alpha1.WorkshopInstallStatus) bool {
	if st == nil {
		return true
	}
	switch st.Phase {
	case spiceboxv1alpha1.WorkshopInstallPhaseInstalled, spiceboxv1alpha1.WorkshopInstallPhaseDeclined:
		return false
	default:
		return true
	}
}

// workshopInstallRequest is the workshop-install POST body: the admin-chosen
// install TARGET (namespace/name), the live answer values, and the adopt list.
// It deliberately carries NO bundle ref or bytes — the bytes come from the
// workshop's controller-owned status.export, so an admin cannot be steered into
// installing arbitrary bytes by anything in this body.
type workshopInstallRequest struct {
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Values    map[string]string `json:"values"`
	Adopt     []string          `json:"adopt"`
}

// handleWorkshopInstall installs a workshop's drafted .oap agent under the
// OPERATOR SA (a.cfg.K8s) — the builder's identity NEVER touches the install.
// The route is gated on platform#install_agent, so a non-admin is refused
// before this body runs. This is the security core:
//
//   - The bytes come from ws.Status.Export.ArtifactRef (controller-owned
//     status), never a caller-supplied ref, so the POST body cannot aim the
//     install at arbitrary bytes.
//   - The stored bytes are digest-bound to ws.Status.Export.Digest (written
//     atomically with the ref) — a mismatch is store corruption and refuses.
//   - validateInstallTarget refuses system namespaces, so a builder-authored
//     bundle can never land in kube-system / the control-plane namespace.
//   - No secret VALUE ever reaches a response, a log line, or status.message.
//
// The capacity hook (newCapacityQuestions / ExtraQuestions) that oap-install
// runs is DROPPED here for scope: a too-large agent installs unschedulable
// rather than getting a capacity clamp. Conscious degradation for this task.
func (a *Admind) handleWorkshopInstall(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	wsNS, wsName := r.PathValue("ns"), r.PathValue("name")

	// The admin subject `require` already proved, read again here for
	// status.install.approvedBy (the permission cannot distinguish two admins,
	// but the record must name the one who acted). channelSetupOwner writes the
	// 401 and returns ok=false on a bad/absent subject.
	owner, ok := a.channelSetupOwner(w, r)
	if !ok {
		return
	}
	approvedBy := owner.String()

	var body workshopInstallRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxWorkshopInstallBody)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "parse JSON body: "+err.Error())
		return
	}

	// Refuse a malformed target AND a system namespace BEFORE loading anything:
	// a builder-authored bundle must never be installable into kube-system or
	// the control-plane namespace. Load-bearing, not optional.
	if err := validateInstallTarget(body.Namespace, body.Name); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Fail closed on a cluster with no artifact store configured, rather than
	// nil-deref panicking on the Get below.
	if a.cfg.ArtifactStore == nil {
		a.cfg.Logger.Info("admind: workshop install refused; no artifact store configured",
			"workshop", wsNS+"/"+wsName)
		writeJSONError(w, http.StatusInternalServerError,
			"artifact store not configured; cannot load the drafted bundle")
		return
	}

	var ws spiceboxv1alpha1.Workshop
	if err := a.cfg.K8s.Get(ctx, client.ObjectKey{Namespace: wsNS, Name: wsName}, &ws); err != nil {
		if apierrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "workshop not found")
			return
		}
		a.cfg.Logger.Info("admind: workshop install get failed", "workshop", wsNS+"/"+wsName, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "get workshop: "+err.Error())
		return
	}

	if ws.Status.Export == nil {
		writeJSONError(w, http.StatusBadRequest, "this workshop has not exported a bundle yet; nothing to install")
		return
	}

	// Load the bundle BYTES by the workshop-recorded ref. The err is HANDLED:
	// a nil rc on an unhandled error would panic io.ReadAll.
	rc, err := a.cfg.ArtifactStore.Get(ctx, artifactstore.Ref(ws.Status.Export.ArtifactRef))
	if err != nil {
		if errors.Is(err, artifactstore.ErrNotFound) {
			a.cfg.Logger.Info("admind: workshop bundle bytes absent from store",
				"workshop", wsNS+"/"+wsName, "ref", ws.Status.Export.ArtifactRef)
			writeJSONError(w, http.StatusNotFound, "the workshop's exported bundle is no longer in the artifact store")
			return
		}
		a.cfg.Logger.Info("admind: workshop bundle load failed",
			"workshop", wsNS+"/"+wsName, "ref", ws.Status.Export.ArtifactRef, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "load bundle from store: "+err.Error())
		return
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		a.cfg.Logger.Info("admind: workshop bundle read failed", "workshop", wsNS+"/"+wsName, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "read bundle from store: "+err.Error())
		return
	}

	// Digest binding (defense-in-depth): the ref and digest were written
	// atomically to controller-owned status, so a mismatch means the store's
	// bytes were corrupted or swapped underneath the recorded ref. Refuse rather
	// than install unknown bytes.
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != ws.Status.Export.Digest {
		a.cfg.Logger.Info("admind: workshop bundle digest mismatch; refusing install",
			"workshop", wsNS+"/"+wsName, "want", ws.Status.Export.Digest, "got", got)
		writeJSONError(w, http.StatusInternalServerError,
			"the stored bundle's digest does not match the workshop's recorded digest; refusing to install")
		return
	}

	b, err := oap.Unpack(raw)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "unpack bundle: "+err.Error())
		return
	}
	if err := install.Preflight(ctx, b, ""); err != nil {
		writeJSONError(w, http.StatusBadRequest, "preflight: "+err.Error())
		return
	}
	// The declared-channels gate, judged before anything is written — the same
	// function `oap agent install` and handleOapInstall call, in the same order.
	if err := channelplan.CheckDeclared(b, body.Name); err != nil {
		a.cfg.Logger.Info("admind: refused a workshop bundle whose declared channels cannot be installed",
			"workshop", wsNS+"/"+wsName, "namespace", body.Namespace, "name", body.Name, "err", err.Error())
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Merge answer values: params.Values over the builder-supplied non-secret
	// base (ws.spec.installRequest.answers), so the admin need not re-answer
	// what the builder already answered. The admin's values win.
	values := mergeInstallAnswers(ws.Spec.InstallRequest, body.Values)

	// Every requires.secrets declaration this cluster does not already satisfy
	// becomes a field on the same question set the bundle's own questions are
	// resolved over — same probe/rename semantics as handleOapInstall.
	namePrefix := ""
	if body.Name != "" {
		namePrefix = body.Name + "-"
	}
	secretQuestions, _, err := install.RequiredSecretQuestions(ctx, a.cfg.K8s, b.Manifest, body.Namespace, namePrefix)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	questions := append(append([]oap.Question(nil), b.Manifest.Questions...), secretQuestions...)

	answers, secrets, err := install.Resolve(questions, "", values, false)
	if err != nil {
		// A missing-required-question failure gets the structured body so the UI
		// can render exactly those fields and re-POST. The body carries question
		// SHAPE only (never an answered value), so it is safe even for a
		// secret-typed question.
		if missing := missingRequiredQuestions(questions, values); len(missing) > 0 {
			writeJSON(w, http.StatusBadRequest, oapInstallMissingQuestionsResponse{
				Error:     "missing required question(s)",
				Questions: missing,
			})
			return
		}
		writeJSONError(w, http.StatusBadRequest, "resolve install questions: "+err.Error())
		return
	}

	// The install runs under the OPERATOR SA (a.cfg.K8s). SourceRef/SourceDigest
	// are the stable, controller-recorded values, so the stamped provenance
	// annotation stays SSA-idempotent across re-installs of the same digest.
	result, err := install.Install(ctx, a.cfg.K8s, b, answers, secrets, install.InstallOpts{
		Name:         body.Name,
		Namespace:    body.Namespace,
		SourceKind:   "workshop",
		SourceRef:    ws.Status.Export.ArtifactRef,
		SourceDigest: ws.Status.Export.Digest,
		Adopt:        body.Adopt,
	})
	if err != nil {
		var ce *install.ConflictError
		if errors.As(err, &ce) {
			// The cluster holds objects this install would seize. A retryable
			// prompt, not a terminal failure — the UI re-POSTs with `adopt` — so
			// status.install is NOT stamped Failed here.
			a.cfg.Logger.Info("admind: workshop install blocked by pre-existing objects",
				"workshop", wsNS+"/"+wsName, "namespace", body.Namespace, "name", body.Name, "conflicts", len(ce.Conflicts))
			writeJSON(w, http.StatusConflict, oapInstallConflictsResponse{
				Error:     "install would overwrite pre-existing object(s)",
				Conflicts: toWireConflicts(ce.Conflicts),
			})
			return
		}
		// A terminal install failure: record it on status with a GENERIC,
		// non-secret message (install errors are CR/apply/dep-shaped and never
		// echo a SecretSpec value, but the durable, admin-visible status.message
		// gets a fixed string regardless so no future error-text change can leak
		// a value into it — invariant #5). The detail goes to the operator log.
		a.cfg.Logger.Info("admind: workshop install failed",
			"workshop", wsNS+"/"+wsName, "namespace", body.Namespace, "name", body.Name, "err", err.Error())
		if werr := a.writeWorkshopInstallStatus(ctx, &ws, spiceboxv1alpha1.WorkshopInstallPhaseFailed, approvedBy, "",
			"install failed; see operator logs"); werr != nil {
			a.cfg.Logger.Info("admind: could not record workshop install failure on status",
				"workshop", wsNS+"/"+wsName, "err", werr.Error())
		}
		writeJSONError(w, http.StatusInternalServerError, "install failed: "+err.Error())
		return
	}

	if len(result.Adopted) > 0 {
		a.cfg.Logger.Info("admind: workshop install adopted pre-existing objects",
			"workshop", wsNS+"/"+wsName, "namespace", body.Namespace, "name", result.Name, "adopted", result.Adopted)
	}

	installedRef := body.Namespace + "/" + result.Name
	if err := a.writeWorkshopInstallStatus(ctx, &ws, spiceboxv1alpha1.WorkshopInstallPhaseInstalled, approvedBy, installedRef, ""); err != nil {
		// The install SUCCEEDED; only the status record failed. Log loudly and
		// still return 200 — the agent is installed, and a stale status is
		// recoverable, whereas failing the request would imply the install did
		// not happen.
		a.cfg.Logger.Info("admind: workshop installed but recording status.install failed",
			"workshop", wsNS+"/"+wsName, "installedRef", installedRef, "err", err.Error())
	}

	a.cfg.Logger.Info("admind: workshop agent installed by admin",
		"workshop", wsNS+"/"+wsName, "installedRef", installedRef, "subject", approvedBy)

	writeJSON(w, http.StatusOK, oapInstallResponse{
		Name:           result.Name,
		AppliedKinds:   result.AppliedKinds,
		SecretsCreated: result.SecretsCreated,
		Warnings:       result.Warnings,
		Adopted:        result.Adopted,
	})
}

// mergeInstallAnswers overlays the admin's live answer values over the builder's
// non-secret answer base (spec.installRequest.answers). The admin's values win.
// The builder base never carries a secret (spec is applied, and a secret value
// there would be a leak the sidecar cannot make).
func mergeInstallAnswers(req *spiceboxv1alpha1.WorkshopInstallRequest, override map[string]string) map[string]string {
	out := make(map[string]string, len(override))
	if req != nil {
		for k, v := range req.Answers {
			out[k] = v
		}
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

// workshopDecisionResponse is the 200 body for decline (and any non-install
// state write), so the SPA can confirm the phase it landed on.
type workshopDecisionResponse struct {
	Phase      string `json:"phase"`
	ApprovedBy string `json:"approvedBy"`
}

// handleWorkshopDecline records status.install.phase=Declined + approvedBy for
// a workshop the admin chose not to install. Runs under the OPERATOR SA; gated
// on install_agent (declining an install is part of the install decision).
func (a *Admind) handleWorkshopDecline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	wsNS, wsName := r.PathValue("ns"), r.PathValue("name")

	owner, ok := a.channelSetupOwner(w, r)
	if !ok {
		return
	}
	approvedBy := owner.String()

	var ws spiceboxv1alpha1.Workshop
	if err := a.cfg.K8s.Get(ctx, client.ObjectKey{Namespace: wsNS, Name: wsName}, &ws); err != nil {
		if apierrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "workshop not found")
			return
		}
		a.cfg.Logger.Info("admind: workshop decline get failed", "workshop", wsNS+"/"+wsName, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "get workshop: "+err.Error())
		return
	}

	if err := a.writeWorkshopInstallStatus(ctx, &ws, spiceboxv1alpha1.WorkshopInstallPhaseDeclined, approvedBy, "", ""); err != nil {
		a.cfg.Logger.Info("admind: workshop decline status write failed", "workshop", wsNS+"/"+wsName, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "record decline: "+err.Error())
		return
	}

	a.cfg.Logger.Info("admind: workshop install declined by admin", "workshop", wsNS+"/"+wsName, "subject", approvedBy)
	writeJSON(w, http.StatusOK, workshopDecisionResponse{Phase: spiceboxv1alpha1.WorkshopInstallPhaseDeclined, ApprovedBy: approvedBy})
}

// handleWorkshopKill deletes a Workshop CR (the workshop controller tears down
// the provisioned namespace on its own). Gated on kill_session, mirroring
// handleSessionKill.
func (a *Admind) handleWorkshopKill(w http.ResponseWriter, r *http.Request) {
	wsNS, wsName := r.PathValue("ns"), r.PathValue("name")
	subject := r.Header.Get("X-Admin-Subject")

	obj := &spiceboxv1alpha1.Workshop{}
	obj.Namespace, obj.Name = wsNS, wsName
	if err := a.cfg.K8s.Delete(r.Context(), obj); err != nil {
		if apierrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "workshop already gone")
			return
		}
		a.cfg.Logger.Info("admind: workshop kill failed", "workshop", wsNS+"/"+wsName, "subject", subject, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "delete failed: "+err.Error())
		return
	}
	a.cfg.Logger.Info("admind: workshop killed by admin", "workshop", wsNS+"/"+wsName, "subject", subject)
	w.WriteHeader(http.StatusNoContent)
}

// writeWorkshopInstallStatus records admind's half of Workshop.status.install
// (Phase and, when non-empty, ApprovedBy/InstalledRef/Message) without
// clobbering the channelsd WorkshopHandoffWatcher's disjoint fields
// (RequestedAt/DeliveredAt).
//
// It Gets the Workshop FRESH (the watcher may have written since the caller's
// own Get), then mutates the EXISTING status.install pointer IN PLACE off a
// DeepCopy patch base and Status().Patch's a client.MergeFrom diff. Replacing
// the pointer with a fresh struct would make the merge patch emit
// requestedAt:null / deliveredAt:null (both *metav1.Time,omitempty), DELETING
// the watcher's stamps — which un-dedups the watcher into re-delivering the
// card. Mutating in place is what lets the two writers coexist.
func (a *Admind) writeWorkshopInstallStatus(ctx context.Context, ws *spiceboxv1alpha1.Workshop, phase, approvedBy, installedRef, message string) error {
	var fresh spiceboxv1alpha1.Workshop
	if err := a.cfg.K8s.Get(ctx, client.ObjectKeyFromObject(ws), &fresh); err != nil {
		return err
	}
	original := fresh.DeepCopy()

	if fresh.Status.Install == nil {
		fresh.Status.Install = &spiceboxv1alpha1.WorkshopInstallStatus{}
	}
	fresh.Status.Install.Phase = phase
	if approvedBy != "" {
		fresh.Status.Install.ApprovedBy = approvedBy
	}
	if installedRef != "" {
		fresh.Status.Install.InstalledRef = installedRef
	}
	fresh.Status.Install.Message = message

	return a.cfg.K8s.Status().Patch(ctx, &fresh, client.MergeFrom(original))
}
