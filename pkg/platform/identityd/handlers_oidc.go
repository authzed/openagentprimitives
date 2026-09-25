package identityd

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// handleOIDCCallback handles GET /oidc/callback/<kind>, terminating the flow
// /oidc/login's channel-kind step began. The path suffix names the channel-kind
// authenticator that completes it.
//
// It consumes the state token (single-use, expiry-checked) to recover the signed
// deep-link the visitor was opening, converts (code, state) into a proven
// canonical subject via WebAuthenticator.Complete, mints the idd_session cookie
// for that subject, and resumes the gated path.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	// 1. Extract kind from the path suffix after "/oidc/callback/".
	kind := strings.TrimPrefix(r.URL.Path, "/oidc/callback/")
	if kind == "" || strings.Contains(kind, "/") {
		logger.Info("oidc callback: bad path", "path", r.URL.Path)
		s.writeError(w, r, http.StatusBadRequest, "Invalid callback", "The callback path is malformed.")
		return
	}

	// "idp" is a reserved suffix, not a channel kind: it is the cluster IdP's
	// own redirect_uri.
	if kind == "idp" {
		s.handleIdPCallback(w, r)
		return
	}

	// 2. Look up the authenticator for this kind.
	auth, ok := s.deps.Authenticators[kind]
	if !ok || auth == nil {
		logger.Info("oidc callback: unknown kind", "kind", kind)
		s.writeError(w, r, http.StatusNotFound, "Sign-in unavailable",
			"Sign-in for this channel kind isn't configured.")
		return
	}

	// 3. Consume the state token — single-use and expiry-checked — to recover
	//    the original signed deep-link.
	q := r.URL.Query()
	stateTok := q.Get("state")
	if stateTok == "" {
		logger.Info("oidc callback: missing state param", "kind", kind)
		s.writeError(w, r, http.StatusBadRequest, "Sign-in session expired",
			"Sign in again by clicking the link the agent gave you.")
		return
	}
	linkRaw, next, refusal := s.consumeLoginState(r, stateTok, kind)
	if refusal != stateAccepted {
		s.writeStateRefusal(w, r, "oidc callback", refusal)
		return
	}

	// 4. Complete the flow: (code, state) becomes a proven canonical subject.
	canonical, err := auth.Complete(r.Context(), channelkinds.CallbackParams{
		Code:  q.Get("code"),
		State: stateTok,
		Error: q.Get("error"),
	})
	if err != nil {
		if errors.Is(err, channelkinds.ErrAuthenticatorUnavailable) {
			logger.Info("oidc callback: authenticator unavailable", "kind", kind, "err", err.Error())
			s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable",
				"The operator hasn't configured the OAuth client for this channel yet.")
			return
		}
		logger.Info("oidc callback: authenticator.Complete failed", "kind", kind, "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in failed",
			"We couldn't finish signing you in. Try the link again, or contact your administrator if this keeps happening.")
		return
	}
	if canonical == "" {
		logger.Info("oidc callback: authenticator returned empty canonical", "kind", kind)
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in failed",
			"We couldn't determine who you are. Try again.")
		return
	}

	// 5. Mint the idd_session cookie: Subject + ExpiresAt only. The other
	//    Payload fields stay empty because an empty SessionRef is what marks
	//    a payload as a cookie rather than a deep-link (see cookieName).
	cookieRaw, err := s.deps.LinkSigner.Mint(passthroughlink.Payload{
		// WebAuthenticator.Complete returns the already-prefixed
		// "user:<base64(email)>" form as a plain string; wrap it at this
		// wire boundary rather than re-canonicalizing.
		Subject:   identity.Subject(canonical),
		ExpiresAt: time.Now().Add(cookieTTL).Unix(),
	})
	if err != nil {
		logger.Info("oidc callback: cookie mint failed", "kind", kind, "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in failed",
			"We couldn't issue a sign-in cookie. Try again.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    cookieRaw,
		Path:     "/",
		MaxAge:   int(cookieTTL / time.Second),
		HttpOnly: true,
		// Secure only under https, so plain-http local dev still works.
		Secure:   strings.HasPrefix(s.deps.ExternalBaseURL(), "https://"),
		SameSite: http.SameSiteLaxMode,
	})

	// 6. Redirect to an explicit ?next= when one was carried through, else
	//    rebuild the /link?d=&sig= destination. `next` is only a destination,
	//    never an authorization: downstream authz (the artifact view's SpiceDB
	//    gate, say) is the real guard.
	s.redirectNextOrLink(w, r, next, linkRaw)
}
