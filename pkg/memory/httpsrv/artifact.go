package httpsrv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

// serveArtifact handles GET /artifact/{ns}/{sess}/{render}/output.
//
// Auth: Bearer, from channelsd's system token, webd's read-only one, or a
// per-session memory token authorized for the {ns}/{sess} this path names —
// see checkArtifactReadBearer for why the session token belongs on a read.
//
// The bearer is checked BEFORE any K8s lookup, so the route never leaks to an
// unauthenticated client whether a CR exists. The path is split first, but that
// is pure string work against the caller's own URL and reveals no server state;
// a path that does not split yields empty segments, which no token authorizes.
//
// Statuses: 401 missing/unknown bearer, or a session token not authorized for
// this path's session; 405 non-GET; 400 malformed path; 404 no ArtifactRender
// CR; 403 an ownerReference of Kind=AgentSession names something other than
// {sess} (generic, to reveal no further state); 409 Phase != Ready, so the bytes
// are not materialized; 500 empty OutputRef or a failed store Get.
//
// On success the bytes stream with Content-Type: <OutputMIME> and
// Content-Disposition: attachment; filename="<OutputFilename>".
func (h *handler) serveArtifact(w http.ResponseWriter, r *http.Request) {
	ns, sess, render, pathOK := parseArtifactPath(r.URL.Path, "/artifact/", "output")
	if _, ok := h.checkArtifactReadBearer(w, r, ns, sess); !ok {
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !pathOK {
		http.Error(w, "expected /artifact/{ns}/{sess}/{render}/output", http.StatusBadRequest)
		return
	}
	if ns == "" || sess == "" || render == "" {
		http.Error(w, "empty path segment", http.StatusBadRequest)
		return
	}

	cr, outcome := h.fetchOwnedReadyCR(r.Context(), ns, sess, render)
	switch outcome {
	case crOutcomeNotFound:
		http.Error(w, "not found", http.StatusNotFound)
		return
	case crOutcomeLookupFailed:
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	case crOutcomeForbidden:
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	case crOutcomeNotReady:
		http.Error(w, fmt.Sprintf("artifact not ready: phase=%s", cr.Status.Phase), http.StatusConflict)
		return
	}

	if cr.Status.OutputRef == "" {
		http.Error(w, "render is Ready but OutputRef is empty", http.StatusInternalServerError)
		return
	}

	h.streamRawOutput(w, r, cr)
}

// streamRawOutput fetches cr.Status.OutputRef from the artifact store and
// streams it verbatim, defaulting Content-Type to application/octet-stream and
// setting Content-Length only when OutputSize > 0. Shared with serveBundle's
// passthrough. writeRawBytes is the sibling for when the bytes are already in
// memory.
func (h *handler) streamRawOutput(w http.ResponseWriter, r *http.Request, cr spiceboxv1alpha1.ArtifactRender) {
	rc, err := h.artifactStore.Get(r.Context(), artifactstore.Ref(cr.Status.OutputRef))
	if err != nil {
		if errors.Is(err, artifactstore.ErrNotFound) {
			http.Error(w, "artifact bytes not in store", http.StatusInternalServerError)
			return
		}
		http.Error(w, "fetch failed", http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	if mime := cr.Status.OutputMIME; mime != "" {
		w.Header().Set("Content-Type", mime)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if cr.Status.OutputSize > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", cr.Status.OutputSize))
	}
	w.Header().Set("Content-Disposition", contentDisposition(cr.Status.OutputFilename))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// contentDisposition builds an `attachment; filename="..."` header. The
// renderer already enforces the filename charset, so stripping CR/LF/quote here
// is defence in depth: a future renderer bug must not be able to smuggle
// headers through the filename slot.
func contentDisposition(name string) string {
	if name == "" {
		return `attachment`
	}
	safe := strings.NewReplacer(
		"\r", "",
		"\n", "",
		`"`, "",
	).Replace(name)
	return fmt.Sprintf(`attachment; filename=%q`, safe)
}

// access declares what a route does with the caller's authority, and is a
// REQUIRED argument to checkSystemBearer. That is the point: the webd token is
// read-only, and read-only-ness must be enforced by the token check itself, not
// by a list of route names a new route has to remember to join. A shared check
// that cannot tell a read from a write accepts the browser-facing read-only
// credential on the server's write route. Requiring the argument makes a new
// route state its side effects in order to compile.
type access int

const (
	// readAccess: the route only reads. The webd token is accepted (that is
	// webd's whole job — serving the browser artifact view).
	readAccess access = iota
	// writeAccess: the route mutates durable state (stores bytes, appends
	// memory, registers a key). The webd token is refused.
	writeAccess
)

// checkSystemBearer validates the Authorization header carries a registered
// system bearer token authorized for this route's access. It accepts system
// tokens and nothing else; a route that also serves per-session callers reaches
// it through checkArtifactReadBearer, which tries the session branch first and
// falls through to here.
//
//   - channelsd's token: accepted for both readAccess and writeAccess.
//   - webd's token: accepted ONLY for readAccess on a safe method (GET/HEAD).
//     It is browser-facing and must never hold a credential that can write. The
//     method condition is deliberately REDUNDANT with a: a future route that
//     declares readAccess but mutates via POST still refuses webd, so the
//     mistake has to be made twice to matter.
//
// Writes 401 + WWW-Authenticate when the header is missing or matches nothing,
// and 403 when a recognized-but-read-only token asks for a write —
// authenticated, not authorized. Callers must return immediately on false.
func (h *handler) checkSystemBearer(w http.ResponseWriter, r *http.Request, a access) bool {
	token, present := bearerToken(r)
	if !present {
		w.Header().Set("WWW-Authenticate", `Bearer realm="artifact"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	if h.reg.IsChannelsdToken(token) {
		return true
	}
	if h.reg.IsWebdToken(token) {
		if a == readAccess && isSafeMethod(r.Method) {
			return true
		}
		log.FromContext(r.Context()).Info("refused webd read-only token on a write route",
			"path", r.URL.Path, "method", r.Method)
		http.Error(w, "forbidden: webd token is read-only", http.StatusForbidden)
		return false
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="artifact"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

// checkArtifactReadBearer authenticates a caller on the artifact READ routes
// (/artifact/… and /artifact-bundle/…), whose real callers are wider than the
// system components: `oap artifact get` and the runner's artifact_offer_view
// preview both authenticate with the per-session memory token from the
// <session>-memory-token Secret, and neither holds a component credential.
//
// Accepted, in order:
//
//   - a per-session memory token whose authorized-session set covers the
//     {ns}/{sess} named IN THIS PATH — the same dual-auth shape /debug/artifact
//     uses. That set is the caller's own AgentSession plus the per-bundle
//     SpiceboxSession scopes the operator registered alongside it, all readable.
//   - whatever checkSystemBearer admits for readAccess: channelsd's token, and
//     webd's read-only one on a safe method.
//
// Admitting a session token grants it no authority it did not already hold:
// fetchOwnedReadyCR refuses every render whose Kind=AgentSession owner is not
// {sess}, and refuses one with no AgentSession owner at all, so the reachable
// set is exactly the renders of the session the token authorizes.
//
// This is a READ-side widening only. /inbound-asset, the one write route this
// server mounts, stays on checkSystemBearer.
//
// sessionToken is the admitted per-session token, or "" when a system component
// was admitted — the distinction a route needs to scope any memory capability
// it mints to the caller rather than handing a session credential the wildcard
// system approval. Callers must return immediately on ok=false: whichever
// branch refused has already written the 401/403.
func (h *handler) checkArtifactReadBearer(w http.ResponseWriter, r *http.Request, ns, sess string) (sessionToken string, ok bool) {
	// Empty ns/sess (a path that did not split) reaches Authorizes as a key no
	// registration can hold, so a malformed path can never widen this check. A
	// session token on one is therefore refused rather than told its path is
	// malformed — fail-closed, and it concerns only a URL the caller built.
	if ns != "" && sess != "" {
		if token, present := bearerToken(r); present &&
			h.reg.Authorizes(token, memory.NamespacedName{Namespace: ns, Name: sess}) {
			return token, true
		}
	}
	// Not a session token for this path — fall through so checkSystemBearer
	// stays the single writer of the refusal for every rejected caller. A
	// registered token aimed at another session is therefore refused with the
	// same 401 as an unknown one, telling a cross-session prober nothing about
	// which of the two it holds.
	if !h.checkSystemBearer(w, r, readAccess) {
		return "", false
	}
	return "", true
}

// parseArtifactPath splits an artifact route's path — {prefix}{ns}/{sess}/
// {render}/{suffix} — into its three variable segments. ok is false when the
// path is not exactly four segments ending in suffix, in which case all three
// strings are empty; a segment that is present but empty comes back empty with
// ok true, so each route can keep its own distinct 400 message.
//
// Both artifact routes split before authenticating, because scoping a
// per-session token needs the {ns}/{sess} the caller asked for.
func parseArtifactPath(path, prefix, suffix string) (ns, sess, render string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) != 4 || parts[3] != suffix {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// bearerToken extracts the Authorization header's Bearer credential. present is
// false when the header is absent or is not a Bearer one. The token itself may
// be empty, which every registry lookup rejects.
func bearerToken(r *http.Request) (token string, present bool) {
	const prefix = "Bearer "
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, prefix) {
		return "", false
	}
	return strings.TrimPrefix(authz, prefix), true
}

// isSafeMethod reports whether method cannot, by HTTP's own semantics, be a
// write (RFC 9110 §9.2.1). OPTIONS is not listed: no route here serves it, so
// admitting it would only widen what a read-only token may reach.
func isSafeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// crOutcome classifies the result of fetchOwnedReadyCR. An enum rather than a
// bare error so each caller can map it its own way: serveArtifact to a distinct
// HTTP status and message per outcome, serveBundle's asset resolver to a single
// "drop this ref".
type crOutcome int

const (
	crOutcomeOK crOutcome = iota
	crOutcomeNotFound
	crOutcomeLookupFailed
	crOutcomeForbidden
	crOutcomeNotReady
)

// fetchOwnedReadyCR fetches the {ns}/{render} ArtifactRender CR and validates it
// identically for every caller: owned by {sess} — an ownerReference with
// Kind=AgentSession must have Name=sess, and a CR with NO AgentSession owner is
// also refused, since these routes serve session-owned renders only — and
// Phase=Ready. It never writes to the response; callers decide what a non-OK
// outcome means for them.
func (h *handler) fetchOwnedReadyCR(ctx context.Context, ns, sess, render string) (spiceboxv1alpha1.ArtifactRender, crOutcome) {
	var cr spiceboxv1alpha1.ArtifactRender
	if err := h.k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: render}, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			return cr, crOutcomeNotFound
		}
		return cr, crOutcomeLookupFailed
	}

	sawAgentSession := false
	for _, or := range cr.OwnerReferences {
		if or.Kind == "AgentSession" {
			sawAgentSession = true
			if or.Name != sess {
				return cr, crOutcomeForbidden
			}
		}
	}
	if !sawAgentSession {
		return cr, crOutcomeForbidden
	}

	if cr.Status.Phase != spiceboxv1alpha1.ArtifactRenderPhaseReady {
		return cr, crOutcomeNotReady
	}
	return cr, crOutcomeOK
}
