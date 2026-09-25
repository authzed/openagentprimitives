package identityd

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// handleIdPCallback handles GET /oidc/callback/idp — the cluster IdP's
// redirect_uri, terminating the flow beginIdPLogin started.
func (s *Server) handleIdPCallback(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())
	ctx := r.Context()

	// 1. Resolve the cluster IdP. Unlike beginIdPLogin, not-configured is a
	//    500 here too: a callback for an unconfigured IdP is anomalous.
	resolved, err := s.idp.Current(ctx)
	if err != nil {
		logger.Info("idp callback: cluster IdP unavailable", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable",
			"The cluster identity provider is not available. Ask your administrator to run `oap idp status`.")
		return
	}

	// 2. Consume the state token.
	q := r.URL.Query()
	stateTok := q.Get("state")
	if stateTok == "" {
		logger.Info("idp callback: missing state param")
		s.writeError(w, r, http.StatusBadRequest, "Sign-in session expired",
			"Sign in again by clicking the link the agent gave you.")
		return
	}
	linkRaw, next, refusal := s.consumeLoginState(r, stateTok, idpAuthenticator)
	if refusal != stateAccepted {
		s.writeStateRefusal(w, r, "idp callback", refusal)
		return
	}

	// 3. Complete the IdP flow.
	p, ts, err := resolved.provider.Complete(ctx, idp.CallbackParams{
		Code:  q.Get("code"),
		State: stateTok,
		Error: q.Get("error"),
	})
	if err != nil {
		logger.Info("idp callback: provider.Complete failed", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in failed",
			"We couldn't finish signing you in. Try the link again, or contact your administrator if this keeps happening.")
		return
	}

	// An IdP-verified principal always carries a proven email, so this never
	// hits the synthetic-subject guard — but fail closed on the impossible
	// error rather than proceed with an unresolved subject.
	pCanon, err := p.Canonical()
	if err != nil {
		logger.Info("idp callback: principal canonicalization failed", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in failed",
			"We couldn't finish signing you in. Try again.")
		return
	}
	pSubject := pCanon.Subject()

	// 4. Policy gate — enforceIdPPolicy is the canonical check every IdP login
	//    path must run.
	if msg := enforceIdPPolicy(p, resolved); msg != "" {
		logger.Info("idp callback: policy denied", "canonical", pCanon)
		s.writeError(w, r, http.StatusForbidden, "Sign-in not permitted", msg)
		return
	}

	// 4a. Capture the federation subject token (offline_access). Best-effort:
	//     a write failure must NOT block login, since it only disables
	//     federated tool minting until this user's next login. Never silent.
	if ts != nil && ts.RefreshToken != "" {
		if err := s.writeIdPIdentitySecret(ctx, pSubject.String(), ts); err != nil {
			logger.Info("idp callback: idp-identity secret write failed (federation minting unavailable until next login)",
				"subject", pCanon, "err", err.Error())
		}
	}

	// 5a. CLI branch: a CLI marker in `next` redirects the tab to the CLI's
	//     loopback listener with a one-time code. NO browser cookie is set —
	//     this path serves a program, not a browser. The target is loopback by
	//     construction (parseCLINext bounds the port; the scheme is hard-coded
	//     to http://127.0.0.1), so safeNext deliberately does not apply: that
	//     check governs relative same-origin paths, and loopback is a
	//     different trust model.
	if portStr, cliState, ok := parseCLINext(next); ok {
		code, err := s.cliCodes.Create(p, resolved.sessionTTL, cliState)
		if err != nil {
			logger.Info("idp callback: cli code create failed", "err", err.Error())
			s.writeError(w, r, http.StatusInternalServerError, "Sign-in failed",
				"We couldn't issue a sign-in code for the CLI. Try again.")
			return
		}
		loopback := "http://127.0.0.1:" + portStr + "/callback?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(cliState)
		http.Redirect(w, r, loopback, http.StatusFound)
		return
	} else if strings.HasPrefix(next, "cli|") {
		// A cli-marked next that fails parseCLINext means handleCLILogin's
		// validation and parseCLINext have diverged. Refuse rather than fall
		// through to the browser-cookie path carrying a marker value.
		logger.Info("idp callback: malformed cli marker in next; refusing", "next", next)
		s.writeError(w, r, http.StatusBadRequest, "Sign-in failed",
			"The CLI sign-in request was malformed. Run `oap login` again.")
		return
	}

	// 5. Mint the idd_session cookie, in the same shape the channel-kind OIDC
	//    path mints. IdP-verified logins get the long sessionTTL, not the
	//    short cookieTTL a trust-link shortcut earns.
	cookieRaw, err := s.deps.LinkSigner.Mint(passthroughlink.Payload{
		Subject:   pSubject,
		ExpiresAt: time.Now().Add(resolved.sessionTTL).Unix(),
	})
	if err != nil {
		logger.Info("idp callback: cookie mint failed", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in failed",
			"We couldn't issue a sign-in cookie. Try again.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    cookieRaw,
		Path:     "/",
		MaxAge:   int(resolved.sessionTTL / time.Second),
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.deps.ExternalBaseURL(), "https://"),
		SameSite: http.SameSiteLaxMode,
	})

	// 6. Redirect to next (or reconstruct /link).
	s.redirectNextOrLink(w, r, next, linkRaw)
}

// writeIdPIdentitySecret creates or overwrites the subject-keyed IdP-identity
// Secret in IdentitiesNamespace. It holds the offline_access token material
// captured at login so the broker can later mint ID-JAG tokens for headless
// sessions without the user present. A prior login's token set is replaced
// wholesale, not merged.
func (s *Server) writeIdPIdentitySecret(ctx context.Context, subject string, ts *idp.TokenSet) error {
	name := useridentity.IdPIdentitySecretName(subject)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		},
		Data: map[string][]byte{
			"access_token":   []byte(ts.AccessToken),
			"refresh_token":  []byte(ts.RefreshToken),
			"token_endpoint": []byte(ts.TokenEndpoint),
			"client_id":      []byte(ts.ClientID),
			"client_secret":  []byte(ts.ClientSecret),
			"scope":          []byte(ts.Scope),
			"expires_at":     []byte(ts.ExpiresAt.UTC().Format(time.RFC3339)),
		},
	}

	var existing corev1.Secret
	err := s.deps.K8s.Get(ctx, client.ObjectKey{Name: name, Namespace: spiceboxv1alpha1.IdentitiesNamespace}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		return s.deps.K8s.Create(ctx, secret)
	case err != nil:
		return err
	default:
		// Carry the ResourceVersion so the update keeps its optimistic lock.
		secret.ResourceVersion = existing.ResourceVersion
		return s.deps.K8s.Update(ctx, secret)
	}
}

// enforceIdPPolicy is THE policy check for IdP logins; every login path must
// pass through it. It returns a user-safe denial message, "" meaning allowed —
// so a caller that ignores the result fails OPEN.
func enforceIdPPolicy(p identity.Principal, resolved *resolvedIdP) string {
	if p.Email() == "" {
		return "Your identity provider did not supply an email address."
	}
	if !p.EmailVerified() {
		return "Your identity provider reports this email as unverified."
	}
	if resolved.allowAny {
		return ""
	}
	email := p.Email().String()
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return "This account isn't permitted on this cluster."
	}
	domain := email[at+1:]
	for _, d := range resolved.allowedDomains {
		if strings.EqualFold(d, domain) {
			return ""
		}
	}
	return "This account isn't permitted on this cluster."
}
