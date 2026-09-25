package interact

import (
	"encoding/json"
	"net/http"

	"github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	ikind "github.com/authzed/openagentprimitives/pkg/channels/interact"
	"github.com/authzed/openagentprimitives/pkg/web/viewurn"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// maxRequestBody bounds POST /interact bodies — an interaction payload is
// always a small structured message, never a bulk upload.
const maxRequestBody = 64 << 10

// interactRequest is the wire shape of POST /interact's body. There is
// deliberately NO "via" field: the server mints the Via from the re-checked
// artifactId (interactHandler step 8), because a client-supplied via would let
// a caller impersonate a different originating surface.
type interactRequest struct {
	// Kind names a registered interaction kind (pkg/channels/interact).
	Kind string `json:"kind"`
	// ArtifactID scopes the interaction to one artifact; empty means it is
	// session-scoped, which takes a different Via and one fewer check.
	ArtifactID string `json:"artifactId"`
	// Payload is the kind's own body, decoded by its Submit.
	Payload json.RawMessage `json:"payload"`
}

type interactResponse struct {
	// Outcome is the kind's own result word, as its Submit reported it.
	Outcome string `json:"outcome"`
	// Notice is optional user-facing copy to show alongside the outcome.
	Notice *channelevents.NoticeWire `json:"notice,omitempty"`
}

// --- POST /session/{ns}/{name}/interact -----------------------------------

// interactHandler dispatches a decoded interaction kind through layered,
// fail-closed gates, checked IN ORDER (return early on each):
//
//  1. webui cookie auth → subject; empty ⇒ 401.
//  2. Origin pinned to the trusted origin (CSRF) ⇒ mismatch ⇒ 403.
//  3. Decode {kind, artifactId, payload}; unknown kind ⇒ 400.
//  4. CheckInteract(ns, name, subject) — the send authorization. An error is
//     NEVER an allow: it fails closed as 503, never 403 (a SpiceDB outage
//     must not read as "denied", which would look like a real decision to
//     the user rather than an infra failure).
//  5. session_views capability must be granted+active on the resolved
//     AgentClass (opt-in; defaultOn=false). A class that can't be resolved
//     fails closed the same as an absent grant.
//  6. The requested kind must be listed in session_views.interactions.
//  7. The kind's required permission must already be satisfied — for
//     Permission()=="interact" that is step 4; a future "approve" kind needs
//     a CheckApprove gate not yet wired (follow-up).
//  8. The Via is minted SERVER-SIDE. An empty artifactId is a session-scoped
//     interaction (the session-view page has no artifact): the Via comes from
//     viewurn.TypeSession(ns/name), gated on step 4's CheckInteract ONLY. A
//     non-empty artifactId takes TWO checks, not one — CheckArtifactView
//     proves the subject may view that artifact (a global permission), and
//     the ListRevisions binding probe proves it lives in THIS session.
//     Neither implies the other, and only both together justify the artifact
//     Via. interactRequest decodes no client-supplied "via" at all.
//  9. k.Submit dispatches to the interaction kind; a transport failure is
//     502, never silently dropped.
func interactHandler(d Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ns := r.PathValue("ns")
		name := r.PathValue("name")

		// 1. Authentication.
		subject := webui.SubjectFromContext(ctx)
		if subject == "" {
			writeError(w, http.StatusUnauthorized, "not authenticated")
			return
		}

		// 2. Origin pin (CSRF) — the shared webui.TrustedOriginMatch, which
		// fails closed when the trusted origin is unset; an inline compare
		// would read that as a match for a request sending no Origin.
		if !webui.TrustedOriginMatch(r, d.TrustedOrigin()) {
			d.Logger().Info("interact: origin mismatch", "ns", ns, "name", name, "origin", r.Header.Get("Origin"))
			writeError(w, http.StatusForbidden, "origin not trusted")
			return
		}

		// 3. Decode the body; unknown kind fails closed as 400.
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		var req interactRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		k, ok := ikind.Get(req.Kind)
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown interaction kind")
			return
		}

		// 4. Send authorization: agentsession#interact. NEVER artifact#view, a
		// strict superset (parent->interact + parent->artifact_org_view +
		// platform->view_audit) that would admit platform admins — and, on an
		// org-visible session, any authenticated user — into sending as a
		// participant. An error is
		// fail-closed 503, never 403: an authz error is not a denial decision.
		okInteract, err := d.CheckInteract(ctx, ns, name, subject)
		if err != nil {
			d.Logger().Info("interact: CheckInteract errored", "ns", ns, "name", name, "subject", subject, "err", err.Error())
			writeError(w, http.StatusServiceUnavailable, "authorization check failed")
			return
		}
		if !okInteract {
			writeError(w, http.StatusForbidden, "not authorized to interact with this session")
			return
		}

		// 5. session_views capability, independent of step 4: CheckInteract says
		// who may talk to the session at all, session_views whether THIS class
		// permits any browser-view interaction in the first place.
		class, err := d.AgentClassOf(ctx, ns, name)
		if err != nil {
			d.Logger().Info("interact: AgentClassOf failed; failing closed", "ns", ns, "name", name, "err", err.Error())
			writeError(w, http.StatusForbidden, "session views are not enabled for this session")
			return
		}
		views, verr := agentcaps.ResolveSessionViews(class)
		if verr != nil {
			d.Logger().Info("interact: session_views grant unresolvable; failing closed", "ns", ns, "name", name, "err", verr.Error())
			writeError(w, http.StatusForbidden, "session views configuration is invalid")
			return
		}
		if !views.Active {
			writeError(w, http.StatusForbidden, "session views are not enabled for this class")
			return
		}

		// 6. The requested kind must be explicitly listed.
		if !containsString(views.Interactions, req.Kind) {
			writeError(w, http.StatusForbidden, "this interaction kind is not permitted for this session")
			return
		}

		// 7. Permission follow-up: only Permission()=="interact" is wired today
		// (already checked in step 4). A future "approve" kind needs a
		// CheckApprove gate — tracked as a follow-up, not implemented here.
		if perm := k.Permission(); perm != "interact" {
			d.Logger().Info("interact: kind requires an unsupported permission", "kind", req.Kind, "permission", perm)
			writeError(w, http.StatusForbidden, "this interaction kind is not supported yet")
			return
		}

		// 8. Mint the Via server-side, with the kind's own sub-facet (k.ViaSub()
		// — e.g. "annotations" for an annotation batch, "" for a plain
		// message). Encoding the kind in the server-minted, signed Via is what
		// lets the runner recognize an annotation batch verifiably rather than
		// by content, and the Via below is the only one that ever reaches
		// k.Submit.
		//
		// Two sub-paths, branching on whether the request names an artifact:
		//
		//   - ArtifactID == "": session-scoped. Gated on step 4's CheckInteract
		//     ONLY — CheckArtifactView is a strict superset (parent->interact +
		//     platform->view_audit) that would admit platform admins as
		//     participants, and "does this artifact belong to this session" has
		//     no meaning when there is no artifact.
		//   - ArtifactID != "": CheckArtifactView confirms the subject may view
		//     the artifact, the ListRevisions probe confirms it lives in THIS
		//     session, and only then is the Via minted from it.
		var via string
		if req.ArtifactID == "" {
			v, verr := viewurn.Format(viewurn.TypeSession, ns+"/"+name, k.ViaSub())
			if verr != nil {
				d.Logger().Info("interact: viewurn.Format failed", "ns", ns, "name", name, "err", verr.Error())
				writeError(w, http.StatusBadRequest, "invalid session id")
				return
			}
			via = v
		} else {
			artifactOK, err := d.CheckArtifactView(ctx, req.ArtifactID, subject)
			if err != nil {
				d.Logger().Info("interact: CheckArtifactView errored", "artifactId", req.ArtifactID, "subject", subject, "err", err.Error())
				writeError(w, http.StatusServiceUnavailable, "authorization check failed")
				return
			}
			if !artifactOK {
				writeError(w, http.StatusForbidden, "artifact does not belong to a session you can view")
				return
			}
			// CheckArtifactView is GLOBAL: it proves this subject may view this
			// artifact and says nothing about WHICH session it lives in. Both
			// halves are needed, because the two permissions can be held over
			// different sessions — interact on this one, view on an artifact
			// belonging to another the subject also participates in. Without
			// the binding probe the turn is durably attributed to an artifact
			// this session does not own, inside the server-minted Via the audit
			// log treats as authoritative.
			//
			// Fail closed on ANY error: an unbound artifact and a transient
			// store failure are indistinguishable here, and a store blip must
			// not mint an unbound Via. The refusal matches the plain
			// artifact#view denial above word for word — telling "wrong
			// session" from "no access" apart would let a caller probe which
			// artifact ids live in which session. The log carries which.
			if _, rerr := d.ListRevisions(ctx, ns, name, req.ArtifactID); rerr != nil {
				d.Logger().Info("interact: artifact does not resolve in the target session; refusing",
					"artifactId", req.ArtifactID, "ns", ns, "name", name, "subject", subject, "err", rerr.Error())
				writeError(w, http.StatusForbidden, "artifact does not belong to a session you can view")
				return
			}
			v, verr := viewurn.Format(viewurn.TypeArtifact, req.ArtifactID, k.ViaSub())
			if verr != nil {
				d.Logger().Info("interact: viewurn.Format failed", "artifactId", req.ArtifactID, "err", verr.Error())
				writeError(w, http.StatusBadRequest, "invalid artifact id")
				return
			}
			via = v
		}

		// 9. Submit.
		res, err := k.Submit(ctx, ikind.Deps{NATSRequest: d.NATSRequest()}, ns, name, subject, via, req.Payload)
		if err != nil {
			d.Logger().Info("interact: Submit failed", "kind", req.Kind, "ns", ns, "name", name, "err", err.Error())
			writeError(w, http.StatusBadGateway, "failed to submit interaction: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, interactResponse{Outcome: res.Outcome, Notice: res.Notice})
	})
}

// --- GET /session/{ns}/{name}/interactions --------------------------------

type interactionsResponse struct {
	// Kinds is what this session accepts; empty means none, including when the
	// class could not be resolved (the fail-closed answer).
	Kinds []string `json:"kinds"`
}

// interactionsHandler reports which interaction kinds this session's class
// permits: the intersection of session_views.interactions with the registered
// interact.Names(). Gated on the same send authorization as the POST endpoint
// (CheckInteract) — a subject who may not interact with a session must not
// learn what kinds that session would accept.
func interactionsHandler(d Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ns := r.PathValue("ns")
		name := r.PathValue("name")

		subject := webui.SubjectFromContext(ctx)
		if subject == "" {
			writeError(w, http.StatusUnauthorized, "not authenticated")
			return
		}

		okInteract, err := d.CheckInteract(ctx, ns, name, subject)
		if err != nil {
			d.Logger().Info("interact: CheckInteract errored", "ns", ns, "name", name, "subject", subject, "err", err.Error())
			writeError(w, http.StatusServiceUnavailable, "authorization check failed")
			return
		}
		if !okInteract {
			writeError(w, http.StatusForbidden, "not authorized to interact with this session")
			return
		}

		class, err := d.AgentClassOf(ctx, ns, name)
		if err != nil {
			d.Logger().Info("interact: AgentClassOf failed; failing closed", "ns", ns, "name", name, "err", err.Error())
			writeJSON(w, http.StatusOK, interactionsResponse{Kinds: []string{}})
			return
		}
		views, verr := agentcaps.ResolveSessionViews(class)
		if verr != nil {
			d.Logger().Info("interact: session_views grant unresolvable; failing closed", "ns", ns, "name", name, "err", verr.Error())
			writeJSON(w, http.StatusOK, interactionsResponse{Kinds: []string{}})
			return
		}
		if !views.Active {
			writeJSON(w, http.StatusOK, interactionsResponse{Kinds: []string{}})
			return
		}

		registered := ikind.Names()
		out := make([]string, 0, len(views.Interactions))
		for _, kind := range views.Interactions {
			if containsString(registered, kind) {
				out = append(out, kind)
			}
		}
		writeJSON(w, http.StatusOK, interactionsResponse{Kinds: out})
	})
}

// --- shared helpers --------------------------------------------------------

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

type errorResponse struct {
	// Error is browser-facing copy, never a wrapped internal error.
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
