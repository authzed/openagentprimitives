// pkg/platform/identityd/handlers_heartbeat.go — POST /heartbeat handler.
//
// Bumps SessionUserIdentity.status.lastInteractionAt while the user is active
// on the credential-link form. The operator's passthrough reaper starts its
// deadline at max(ParkedAt, LastInteractionAt), so each heartbeat extends the
// window.
//
// Cookie-gated: the idd_session cookie's Subject must match the session's
// started-by canonical, or one user's activity would hold another's session
// open. The link page fires this every ~30s and ignores the response, so the
// endpoint is best-effort — hence every error path logs, or a heartbeat that
// silently stopped working would be undiagnosable.
package identityd

import (
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// handleHeartbeat handles POST /heartbeat?session=<ns>/<name>. On success it
// sets SessionUserIdentity.status.lastInteractionAt and returns 204; failures
// are 4xx/5xx with a plain-text body the caller ignores, so the log line is how
// an operator learns a heartbeat is failing.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	subject, ok := s.checkOIDCCookie(r)
	if !ok {
		logger.Info("heartbeat: cookie missing/invalid")
		http.Error(w, "no session cookie", http.StatusForbidden)
		return
	}

	sessionRef := r.URL.Query().Get("session")
	parts := strings.SplitN(sessionRef, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		logger.Info("heartbeat: malformed session ref", "ref", sessionRef)
		http.Error(w, "session=<ns>/<name> required", http.StatusBadRequest)
		return
	}
	ns, name := parts[0], parts[1]

	// Gate: the cookie's subject must match the session's started-by.
	var sess spiceboxv1alpha1.AgentSession
	if err := s.deps.K8s.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("heartbeat: session not found", "ref", sessionRef)
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		logger.Info("heartbeat: session Get failed", "ref", sessionRef, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if spiceboxv1alpha1.StartedBySubject(&sess) != subject {
		logger.Info("heartbeat: cookie subject mismatch",
			"cookie_subject", subject,
			"session_started_by", spiceboxv1alpha1.StartedBySubject(&sess),
			"ref", sessionRef)
		http.Error(w, "session belongs to a different user", http.StatusForbidden)
		return
	}

	// The SUI is same-namespace-same-name as its session, by convention.
	var sui spiceboxv1alpha1.SessionUserIdentity
	if err := s.deps.K8s.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &sui); err != nil {
		if apierrors.IsNotFound(err) {
			// No SUI yet: the operator hasn't parked the session, so the link
			// form should never have rendered. 404 makes that visible in
			// DevTools even though the page ignores the response.
			logger.Info("heartbeat: SUI not found", "ref", sessionRef)
			http.Error(w, "no session-user-identity", http.StatusNotFound)
			return
		}
		logger.Info("heartbeat: SUI Get failed", "ref", sessionRef, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	now := metav1.Now()
	sui.Status.LastInteractionAt = &now
	if err := s.deps.K8s.Status().Update(r.Context(), &sui); err != nil {
		logger.Info("heartbeat: SUI status update failed", "ref", sessionRef, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
