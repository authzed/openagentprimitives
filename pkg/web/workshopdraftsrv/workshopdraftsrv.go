// Package workshopdraftsrv exposes the ONE operator route the agent-builder
// workshop sidecar's export_draft tool (plan 3b, Task 7) posts a drafted
// .oap bundle to:
//
//	POST /workshop/draft   body: raw .oap bytes  →
//	  {"artifactRef","digest","handle","artifactId"}
//
// The route creates an ArtifactRender CR of the operator-only oap kind,
// owned by the builder session, so the draft is a real artifact of that
// session's memory: the builder's runner later finalizes it with
// artifact_await (unchanged code) and can attach it with respond_to_user,
// exactly as for any other artifact. This route never writes the artifact
// memory kinds itself — the runner stays the only writer of those.
//
// # Ruling A — a narrow tuple-authorized route, not a broadened bearer
//
// The workshop sidecar's operator bearer is registered (plan 2) keyed to the
// SYNTHETIC {builderSessionNamespace, WorkshopName(builderSessionName)} pair
// — never to the builder session {B, X} itself — so LookupInfo alone proves
// only "this bearer belongs to some workshop", not "this session may write
// its draft". Re-registering the bearer with primary scope {B, X} would grant
// it the token registry's ordinary read/write standing over the WHOLE
// builder session's memory, which is far more than "store one .oap". Instead
// this route:
//
//  1. authenticates the bearer (LookupInfo) to learn which Workshop CR it
//     belongs to;
//  2. resolves THAT Workshop CR and reads spec.session (B/X) and
//     status.namespace (the workshop id W) from it — never from the URL or
//     the request body, both of which a caller could otherwise use to name
//     ANY session;
//  3. re-checks workshop:<W>#build@agentsession:<B>/<X> against SpiceDB,
//     FullyConsistent, moments before the write it gates;
//  4. stores the bytes under {B, X}'s own scope and mirrors the result onto
//     the resolved Workshop's status.export.
//
// Every step denies closed: a missing/invalid bearer, an unresolvable or
// not-Ready Workshop, a nil or erroring build checker, and a false tuple all
// refuse the write. A bearer can therefore never be asked — and can never be
// tricked — into storing bytes under any session but its own.
package workshopdraftsrv

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// Path is the route this package mounts.
const Path = "/workshop/draft"

// MaxDraftBytes hard-caps a single .oap upload, mirroring
// pkg/memory/httpsrv's MaxInboundAssetBytes discipline: a drafted agent graph
// is CR YAML plus a handful of small skill/asset files, never anywhere near
// this large, so the cap exists only to keep a malformed or hostile POST from
// hanging the operator or filling the artifact store.
const MaxDraftBytes = 64 << 20 // 64 MiB

// WorkshopBuildChecker answers workshop:<workshopID>#build for a session —
// satisfied by (*pkg/authz/spicedb.Client).CheckWorkshopBuild. Declared
// locally (mirroring pkg/controllers/workshopprobe.WorkshopBuildChecker and
// pkg/controllers/webhooks/workshop.WorkshopBuildChecker) rather than a
// *spicedb.Client field, for the same two reasons those packages give: this
// package's tests inject a fake with no live SpiceDB, and the handler never
// carries a typed pointer that could be assigned nil into an interface field
// (CLAUDE.md's typed-nil rule) — a genuinely unwired dependency is a true nil
// interface, caught by the nil check in ServeHTTP, not a panic.
type WorkshopBuildChecker interface {
	CheckWorkshopBuild(ctx context.Context, workshopID, sessNS, sessName string) (bool, error)
}

// draftResponse is the 200 response body: the artifactstore ref the bytes
// landed at, plus their sha256 digest — sufficient for the caller to record
// both on the Workshop CR it already holds (and for load_draft, a later
// task, to fetch the exact bytes back) — plus the render's handle and
// artifact id, so the builder can artifact_await it.
type draftResponse struct {
	ArtifactRef string `json:"artifactRef"`
	Digest      string `json:"digest"`
	Handle      string `json:"handle"`
	ArtifactID  string `json:"artifactId"`
}

// NewHandler returns the POST /workshop/draft handler.
//
//   - c resolves the caller's own Workshop CR and mirrors the stored result
//     onto its status, and is also where the draft's ArtifactRender CR is
//     created, owned by the builder AgentSession. Reads, one status-subresource
//     write, and one Create — never a write to the Workshop's own spec or
//     metadata.
//   - mem backs the artifact service that mints ids and render names — the
//     route never writes to memory itself; the builder's runner finalizes the
//     render, as for every artifact.
//   - store holds the raw .oap bytes: once under the draft's own key (what
//     the install request reads), and a second time under the render's
//     PayloadRef (deleted with the render).
//   - reg authenticates the bearer.
//   - build answers the workshop#build tuple; nil is handled at request time
//     (denied, not panicked) per the typed-nil rule on WorkshopBuildChecker.
func NewHandler(c client.Client, mem memory.Memory, store artifactstore.Store, reg *tokens.Registry, build WorkshopBuildChecker) http.Handler {
	h := &handler{c: c, mem: mem, store: store, reg: reg, build: build, art: artifacts.NewService(mem, nil)}
	mux := http.NewServeMux()
	mux.Handle(Path, h)
	return mux
}

type handler struct {
	c     client.Client
	mem   memory.Memory
	store artifactstore.Store
	reg   *tokens.Registry
	build WorkshopBuildChecker
	art   *artifacts.Service
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	authz := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(authz, prefix) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="workshop-draft"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(authz, prefix)

	// The bearer resolves to the SYNTHETIC workshop key
	// {builderSessionNamespace, WorkshopName(builderSessionName)} the
	// AgentSession reconciler registered it under (plan 2) — this proves the
	// bearer belongs to SOME workshop, nothing about the tuple yet.
	info, ok := h.reg.LookupInfo(token)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="workshop-draft"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()
	var ws spiceboxv1alpha1.Workshop
	if err := h.c.Get(ctx, types.NamespacedName{Namespace: info.Session.Namespace, Name: info.Session.Name}, &ws); err != nil {
		http.Error(w, fmt.Sprintf("no workshop resolves for this bearer: %v", err), http.StatusForbidden)
		return
	}
	if ws.Status.Namespace == "" || ws.Status.Phase != spiceboxv1alpha1.WorkshopPhaseReady {
		http.Error(w, fmt.Sprintf("workshop %s/%s is not Ready (phase %q)", ws.Namespace, ws.Name, ws.Status.Phase), http.StatusForbidden)
		return
	}
	// B/X and W come ENTIRELY from the resolved CR — never the URL (this
	// route takes none) and never the request body.
	sessNS, sessName, workshopID := ws.Spec.Session.Namespace, ws.Spec.Session.Name, ws.Status.Namespace

	// Fail-closed on a nil checker: an unwired dependency must never read as
	// "no tuple to check, so allow" (CLAUDE.md's typed-nil rule; mirrors
	// workshopprobectrl.Reconciler's r.Build == nil guard).
	if h.build == nil {
		http.Error(w, "no workshop build-authorization backend is wired; refusing to store", http.StatusServiceUnavailable)
		return
	}
	// FullyConsistent per CheckWorkshopBuild's own contract: this is a
	// privilege gate asked moments before the write it protects. An error is
	// fail-closed too — an unconfirmable tuple is a no, not a maybe.
	allowed, err := h.build.CheckWorkshopBuild(ctx, workshopID, sessNS, sessName)
	if err != nil {
		http.Error(w, fmt.Sprintf("checking workshop:%s#build for session %s/%s: %v", workshopID, sessNS, sessName, err), http.StatusForbidden)
		return
	}
	if !allowed {
		http.Error(w, fmt.Sprintf("session %s/%s does not hold workshop:%s#build", sessNS, sessName, workshopID), http.StatusForbidden)
		return
	}

	defer r.Body.Close()
	limited := http.MaxBytesReader(w, r.Body, MaxDraftBytes)
	raw, err := io.ReadAll(limited)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "draft bundle too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read draft body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(raw) == 0 {
		http.Error(w, "empty draft body", http.StatusBadRequest)
		return
	}

	// Refuse bytes that are not a bundle before storing anything: the render
	// would fail the same way later, but a builder should hear it now, from
	// the tool it called. The agent's name from the manifest names the file.
	bundle, err := oap.Unpack(raw)
	if err != nil {
		http.Error(w, "the draft is not an agent bundle: "+err.Error(), http.StatusBadRequest)
		return
	}
	agentName := "agent"
	if bundle.Manifest != nil && bundle.Manifest.Agent.Name != "" {
		agentName = bundle.Manifest.Agent.Name
	}

	// The draft is an artifact of the BUILDER session, delivered through the
	// same pipeline as every artifact: a render owned by that session, of the
	// operator-only oap kind, which the builder's runner finalizes with
	// artifact_await and attaches with respond_to_user. Read before any write:
	// a missing session 500s write-free, the same as the 400 paths above.
	var bs spiceboxv1alpha1.AgentSession
	if err := h.c.Get(ctx, types.NamespacedName{Namespace: sessNS, Name: sessName}, &bs); err != nil {
		http.Error(w, fmt.Sprintf("builder session %s/%s: %v", sessNS, sessName, err), http.StatusInternalServerError)
		return
	}

	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])

	// Digest-keyed: a byte-identical re-export (a retried export_draft call,
	// or an unchanged workshop re-exported) lands on the SAME artifactstore
	// key instead of accumulating a copy per attempt.
	key := path.Join(sessNS, sessName, "workshop-draft", digest+oap.DefaultExtension)
	ref, err := h.store.Put(ctx, key, bytes.NewReader(raw))
	if err != nil {
		http.Error(w, "store draft: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// The render's input is its own object, keyed by the render's artifact id,
	// so two exports of identical bytes never share a payloadRef the
	// controller's finalizer would delete out from under the other.
	artifactID := h.art.NewArtifactID()
	inKey := path.Join(sessNS, sessName, "workshop-draft-render", artifactID+oap.DefaultExtension)
	inRef, err := h.store.Put(ctx, inKey, bytes.NewReader(raw))
	if err != nil {
		http.Error(w, "store draft render input: "+err.Error(), http.StatusInternalServerError)
		return
	}
	cr := artifacts.NewRender(h.art.NewRenderName(sessName), sessNS, sessName, bs.UID, artifactID,
		spiceboxv1alpha1.ArtifactRenderSpec{
			Kind:           "oap",
			Filename:       agentName + oap.DefaultExtension,
			PayloadRef:     string(inRef),
			TimeoutSeconds: 30,
		},
		map[string]string{
			artifacts.AnnoArtifactName:        agentName + " draft",
			artifacts.AnnoArtifactDescription: "Exported bundle (sha256:" + digest + ")",
		})
	if err := h.c.Create(ctx, cr); err != nil {
		http.Error(w, "create draft render: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := recordExportStatus(ctx, h.c, ws.Namespace, ws.Name, string(ref), digest); err != nil {
		http.Error(w, "record workshop export status: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(draftResponse{ArtifactRef: string(ref), Digest: digest, Handle: cr.Name, ArtifactID: artifactID})
}

// recordExportStatus mirrors the just-stored ref/digest onto
// Workshop.status.export, set-once per digest: a fresh Get (rather than
// reusing the object ServeHTTP already holds, which may now be stale — the
// store + memory writes above take real time) followed by a status-only
// Update. When the resolved Workshop's export already names this exact
// digest, the write is skipped entirely — a byte-identical re-export must
// not churn ExportedAt or the object's resourceVersion on a controller-owned
// field.
func recordExportStatus(ctx context.Context, c client.Client, ns, name, ref, digest string) error {
	var ws spiceboxv1alpha1.Workshop
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ws); err != nil {
		return fmt.Errorf("get Workshop %s/%s: %w", ns, name, err)
	}
	if ws.Status.Export != nil && ws.Status.Export.Digest == digest {
		return nil
	}
	ws.Status.Export = &spiceboxv1alpha1.WorkshopExport{
		ArtifactRef: ref,
		Digest:      digest,
		ExportedAt:  metav1.Now(),
	}
	if err := c.Status().Update(ctx, &ws); err != nil {
		return fmt.Errorf("update Workshop %s/%s status.export: %w", ns, name, err)
	}
	return nil
}
