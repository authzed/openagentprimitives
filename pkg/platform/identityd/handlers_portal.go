// pkg/platform/identityd/handlers_portal.go — the standing portal at
// `/my/accounts`, the ongoing-management counterpart to handlers_link.go's
// reactive deep-link flow:
//
//	GET  /my/accounts                     — list linked + suggested creds
//	GET  /my/accounts/<cred>/link         — one-credential PAT form
//	POST /my/accounts/<cred>/link/submit  — store via useridentity.PutToken
//	POST /my/accounts/<cred>/revoke       — delete Secret + UserIdentity entry
//
// Trust roots: only GET /my/accounts may bootstrap a session, from a portal-
// purpose signed link in ?d=&sig= (single-use), which is how channelsd can DM a
// one-click portal link. The other three are strictly cookie-gated.
//
// Revocation is best-effort and idempotent, and it does NOT clear in-flight
// state: running pods may still hold the cached token until they finish.
package identityd

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// purposePortal is the Payload.Purpose channelsd stamps on a portal-access link
// — the only purpose linkSessionPurposes admits. Any other value means
// /my/accounts refuses to bootstrap a cookie from the link.
const purposePortal = "portal"

// handlePortalGet handles GET /my/accounts: use the idd_session cookie's subject
// if present, else bootstrap one from a single-use portal-purpose link in
// ?d=&sig=, else 401 with a hint to ask the agent for a portal link. It then
// renders the subject's linked credentials plus suggestions.
func (s *Server) handlePortalGet(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	cookieSubject, cookieOK := s.checkOIDCCookie(r)
	if !cookieOK {
		// Try the portal-link query string.
		if d := r.URL.Query().Get("d"); d != "" {
			sig := r.URL.Query().Get("sig")
			if sig == "" {
				logger.Info("portal: link missing sig", "path", r.URL.Path)
				s.writeError(w, r, http.StatusBadRequest, "Invalid link", "This link is missing required parameters.")
				return
			}
			payload, err := s.deps.LinkSigner.Verify(d+"."+sig,
				passthroughlink.WithExpectedIssuer(passthroughlink.IssuerChannelsd),
				passthroughlink.WithExpectedAudience(passthroughlink.AudienceIdentityd))
			if err != nil {
				s.writeLinkVerifyError(w, r, err, logger)
				return
			}
			// Whether a signed link may become a session is one decision,
			// shared with /oidc/login; only the refusal wording is local.
			if ok, refusal := linkMayMintSession(payload); !ok {
				logger.Info("portal: link may not bootstrap a session",
					"purpose", payload.Purpose, "refusal", string(refusal))
				if refusal == refusalSubject {
					s.writeError(w, r, http.StatusBadRequest, "Invalid link",
						"This link is missing the user identity.")
					return
				}
				s.writeError(w, r, http.StatusBadRequest, "Invalid link",
					"This link isn't a portal-access link.")
				return
			}
			// Single-use: a portal link bootstraps a cookie once, after which
			// the cookie alone carries the visitor. This is what makes a
			// forwarded portal link harmless — only one holder gets a session.
			rawLink := d + "." + sig
			first, cerr := s.consumedLinks.markConsumed(r.Context(), rawLink)
			if cerr != nil {
				// Fail closed, but say what actually happened: reporting a
				// storage outage as "already used" is a lie the user cannot
				// act on, and treating it as a first use is the exact replay
				// single-use exists to stop.
				logger.Info("portal: could not verify single-use for link; refusing",
					"subject", payload.Subject, "err", cerr.Error())
				s.writeError(w, r, http.StatusServiceUnavailable, "Couldn't verify this link",
					"We couldn't check whether this link has already been used. Please try again in a moment.")
				return
			}
			if !first {
				logger.Info("portal: link already consumed", "subject", payload.Subject)
				s.writeError(w, r, http.StatusBadRequest, "Link already used",
					"This portal-access link has already been used. Ask your agent for a fresh one (e.g. 'manage my accounts').")
				return
			}
			s.setTrustLinkCookie(w, payload.Subject, logger)
			cookieSubject = payload.Subject
		} else {
			// No cookie and no portal link. Explain rather than redirect to
			// OIDC: with no session there is nothing to gate the return-to.
			logger.Info("portal: no cookie + no portal link", "path", r.URL.Path)
			s.writeError(w, r, http.StatusUnauthorized, "Sign-in required",
				"Open the portal by asking your agent (e.g. 'manage my accounts') — it will send you a sign-in link.")
			return
		}
	}

	uiName := useridentity.NameForSubject(cookieSubject)
	var ui spiceboxv1alpha1.UserIdentity
	err := s.deps.K8s.Get(r.Context(), client.ObjectKey{Name: uiName}, &ui)
	var linked []portalCredential
	switch {
	case err == nil:
		linked = portalCredentialsFromUserIdentity(r.Context(), s.deps.K8s, &ui, logger)
	case apierrors.IsNotFound(err):
		// A user who has linked nothing yet; the empty list is correct.
	default:
		logger.Info("portal: UserIdentity Get failed", "name", uiName, "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in error",
			"Couldn't load your linked accounts. Try again.")
		return
	}

	// Suggestions are the credentials any passthrough AgentClass references,
	// minus what this user already linked.
	alreadyLinked := map[string]bool{}
	for _, cred := range linked {
		alreadyLinked[cred.Name] = true
	}
	suggested, listErr := s.suggestedCredentialsForUserCached(r.Context(), alreadyLinked, logger)
	if listErr != nil {
		logger.Info("portal: suggested credentials lookup failed; rendering empty suggestions", "err", listErr.Error())
		suggested = []portalCredential{}
	}

	if err := s.renderApp(r, w, "identity-portal", portalPageData{
		Subject:     cookieSubject.String(),
		DisplayName: identity.DecodeForDisplay(cookieSubject.String()),
		Linked:      linked,
		Suggested:   suggested,
	}, webui.PageMeta{Title: "Your accounts"}); err != nil {
		logger.Info("portal: render failed", "err", err.Error())
	}
}

// handlePortalLinkForm handles GET /my/accounts/<credname>/link, rendering the
// one-credential PAT form. Cookie-gated: it never bootstraps a session itself,
// so the visitor must arrive from /my/accounts.
func (s *Server) handlePortalLinkForm(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())
	if _, ok := s.checkOIDCCookie(r); !ok {
		logger.Info("portal/link: no cookie", "path", r.URL.Path)
		s.writeError(w, r, http.StatusUnauthorized, "Sign-in required",
			"Open /my/accounts first to sign in.")
		return
	}
	credName, ok := portalCredNameFromPath(r.URL.Path, "/link")
	if !ok {
		logger.Info("portal/link: invalid path", "path", r.URL.Path)
		s.writeError(w, r, http.StatusBadRequest, "Invalid path", "Credential name missing from URL.")
		return
	}

	instr, docsURL := credInstructions(credName)
	if err := s.renderApp(r, w, "identity-link-form", portalLinkFormData{
		CredentialName:  credName,
		CredentialLabel: portalCredentialLabel(r.Context(), s.deps.K8s, credName, logger),
		Instructions:    instr,
		DocsURL:         docsURL,
	}, webui.PageMeta{Title: "Link credential"}); err != nil {
		logger.Info("portal/link: render failed", "err", err.Error())
	}
}

// handlePortalLinkSubmit handles POST /my/accounts/<credname>/link/submit,
// persisting the PAT via useridentity.PutToken.
//
// Known limit: no consumedLinks single-use gate applies here, so this POST is
// replayable. The cookie is the only authentication source and is HttpOnly +
// short-lived, which is what keeps that acceptable.
func (s *Server) handlePortalLinkSubmit(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())
	subject, ok := s.checkOIDCCookie(r)
	if !ok {
		logger.Info("portal/link/submit: no cookie", "path", r.URL.Path)
		s.writeError(w, r, http.StatusForbidden, "Not signed in", "Open /my/accounts first to sign in.")
		return
	}
	credName, ok := portalCredNameFromPath(r.URL.Path, "/link/submit")
	if !ok {
		logger.Info("portal/link/submit: invalid path", "path", r.URL.Path)
		s.writeError(w, r, http.StatusBadRequest, "Invalid path", "Credential name missing from URL.")
		return
	}
	if err := r.ParseForm(); err != nil {
		logger.Info("portal/link/submit: ParseForm failed", "err", err.Error())
		s.writeError(w, r, http.StatusBadRequest, "Invalid form", "Could not read the form submission.")
		return
	}
	token := strings.TrimSpace(r.FormValue("token"))
	if token == "" {
		logger.Info("portal/link/submit: missing token", "cred", credName)
		s.writeError(w, r, http.StatusBadRequest, "Missing token", "Paste the credential value before submitting.")
		return
	}

	// Reject a wrong/malformed paste at the door, the same gate the reactive
	// /link/submit path applies.
	if err := s.validatePastedToken(r.Context(), credName, token); err != nil {
		logger.Info("portal/link/submit: token format rejected",
			"cred", credName, "subject", subject, "err", err.Error())
		s.writeError(w, r, http.StatusBadRequest, "That doesn't look right",
			tokenRejectMessage(err))
		return
	}

	// Live verification. verifyConfirm=1 means the user already saw the
	// rejection on the confirm page and chose to store anyway.
	//
	// verified is declared out here because the store below records what the
	// check observed — which provider account the token authenticated as. On
	// the verifyConfirm path it stays zero, which is the honest answer: the
	// credential is being stored on a human's say-so, and no account was
	// established for it.
	var verified builtins.VerifyResult
	noticeVerb := "linked"
	if r.FormValue("verifyConfirm") == "1" {
		noticeVerb = "linkedunverified"
		logger.Info("portal/link/submit: storing provider-rejected token on user confirmation",
			"cred", credName, "subject", subject)
	} else {
		// askToConfirm re-renders the paste as the verify-warn page: nothing is
		// stored until the visitor presses "store anyway", which returns as
		// verifyConfirm=1. Shared by every arm needing a human decision so they
		// cannot drift apart on where the form posts.
		askToConfirm := func(warning string) {
			s.renderVerifyWarn(w, r, verifyWarnData{
				Warning:   warning,
				Action:    "/my/accounts/" + url.PathEscape(credName) + "/link/submit",
				Hidden:    map[string]string{"token": token, "verifyConfirm": "1"},
				CancelURL: "/my/accounts",
			})
		}
		verified = s.verifyPastedToken(r.Context(), credName, token)
		switch verified.Status {
		case builtins.VerifyRejected:
			logger.Info("portal/link/submit: token failed live verification",
				"cred", credName, "subject", subject, "detail", verified.Detail)
			askToConfirm(verified.Detail)
			return
		case builtins.VerifyForbidden:
			// The provider took the credential and refused only this check —
			// not a rejection. The visitor is right here, so let them decide
			// rather than storing something the check could not confirm.
			logger.Info("portal/link/submit: provider accepted the credential but refused the verification check; asking the user to confirm",
				"cred", credName, "subject", subject, "detail", verified.Detail)
			askToConfirm(builtins.ForbiddenNotice(verified))
			return
		case builtins.VerifyIndeterminate:
			logger.Info("portal/link/submit: could not verify token; storing anyway",
				"cred", credName, "detail", verified.Detail)
			noticeVerb = "linkedunverified"
		case builtins.VerifyUnsupported:
			logger.Info("portal/link/submit: no live verification available; storing",
				"cred", credName, "detail", verified.Detail)
		case builtins.VerifyValid:
			logger.Info("portal/link/submit: token verified",
				"cred", credName, "subject", verified.Subject)
		default:
			// Fail-closed exhaustiveness backstop: reaching here means a
			// VerifyStatus was added without teaching this handler what it
			// means, so nothing is stored until the visitor confirms. Without
			// this arm the next new verdict would fall open into a store.
			logger.Info("portal/link/submit: unrecognised verification verdict; asking the user to confirm",
				"cred", credName, "subject", subject,
				"status", string(verified.Status), "detail", verified.Detail)
			askToConfirm(builtins.UnrecognizedNotice(verified))
			return
		}
	}

	if err := useridentity.PutToken(r.Context(), s.deps.K8s, useridentity.PutTokenRequest{
		Subject:        subject,
		CredentialName: credName,
		Token:          token,
		// What the live check observed about WHOSE account this is, recorded
		// with the credential so the UserIdentity reconciler can bind it to
		// this catalog's declared owner. Both halves come from the
		// verification that just ran; neither is re-derived here.
		ProviderID: verified.ProviderID,
		SubjectID:  verified.SubjectID,
	}); err != nil {
		logger.Info("portal/link/submit: PutToken failed",
			"cred", credName, "subject", subject, "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Couldn't link",
			"Couldn't store the credential. Try again, or contact your administrator.")
		return
	}

	// Post/Redirect/Get, so a refresh re-GETs the portal rather than re-POSTing
	// the pasted secret. The notice query becomes a banner.
	http.Redirect(w, r, "/my/accounts?notice="+noticeVerb+":"+url.QueryEscape(credName), http.StatusSeeOther)
}

// handlePortalRevoke handles POST /my/accounts/<credname>/revoke: deletes the
// master Secret and drops the entry from UserIdentity.Spec.Credentials.
// Idempotent — NotFound on either side counts as success, because the intent
// ("forget this credential") is met either way.
//
// Running sessions keep their cached token until they finish; the portal banner
// says so.
func (s *Server) handlePortalRevoke(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())
	subject, ok := s.checkOIDCCookie(r)
	if !ok {
		logger.Info("portal/revoke: no cookie", "path", r.URL.Path)
		s.writeError(w, r, http.StatusForbidden, "Not signed in", "Open /my/accounts first to sign in.")
		return
	}
	credName, ok := portalCredNameFromPath(r.URL.Path, "/revoke")
	if !ok {
		logger.Info("portal/revoke: invalid path", "path", r.URL.Path)
		s.writeError(w, r, http.StatusBadRequest, "Invalid path", "Credential name missing from URL.")
		return
	}

	uiName := useridentity.NameForSubject(subject)
	secretName := useridentity.MasterSecretName(uiName, credName)

	// 1. Delete the master Secret; NotFound means it is already gone.
	if err := s.deps.K8s.Delete(r.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: spiceboxv1alpha1.IdentitiesNamespace},
	}); err != nil && !apierrors.IsNotFound(err) {
		logger.Info("portal/revoke: master Secret delete failed",
			"secret", secretName, "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Couldn't revoke",
			"Couldn't delete the credential's master Secret. Try again.")
		return
	}

	// 2. Remove the entry from UserIdentity.Spec.Credentials.
	var ui spiceboxv1alpha1.UserIdentity
	switch err := s.deps.K8s.Get(r.Context(), client.ObjectKey{Name: uiName}, &ui); {
	case apierrors.IsNotFound(err):
		// No UserIdentity, so nothing left to remove.
		redirectRevoked(w, r, credName)
		return
	case err != nil:
		logger.Info("portal/revoke: UserIdentity Get failed",
			"name", uiName, "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Couldn't revoke",
			"Couldn't load your identity. Try again.")
		return
	}

	kept := make([]spiceboxv1alpha1.AgentCredential, 0, len(ui.Spec.Credentials))
	for _, c := range ui.Spec.Credentials {
		if c.Name != credName {
			kept = append(kept, c)
		}
	}
	ui.Spec.Credentials = kept
	if err := s.deps.K8s.Update(r.Context(), &ui); err != nil {
		logger.Info("portal/revoke: UserIdentity Update failed",
			"name", uiName, "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Couldn't revoke",
			"Couldn't update your identity. Try again.")
		return
	}

	redirectRevoked(w, r, credName)
}

// redirectRevoked Post/Redirect/Gets back to the portal with the notice query it
// surfaces as a banner — including the in-flight-sessions caveat, whose wording
// lives in that banner copy.
func redirectRevoked(w http.ResponseWriter, r *http.Request, credName string) {
	http.Redirect(w, r, "/my/accounts?notice=revoked:"+url.QueryEscape(credName), http.StatusSeeOther)
}

// handlePortalSubrouter dispatches /my/accounts/<credname>/<action> on method +
// URL suffix, so one ServeMux entry covers every credential.
func (s *Server) handlePortalSubrouter(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/link"):
		s.handlePortalLinkForm(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/link/submit"):
		s.handlePortalLinkSubmit(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/revoke"):
		s.handlePortalRevoke(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		s.writeError(w, r, http.StatusMethodNotAllowed, "Method or route not allowed",
			"Try /my/accounts.")
	}
}

// portalCredNameFromPath extracts <credname> from
// /my/accounts/<credname><suffix>. ("", false) on a wrong prefix/suffix, an
// empty segment, or a segment containing a slash — a multi-segment name could
// otherwise mask a sub-route.
func portalCredNameFromPath(path, suffix string) (string, bool) {
	const prefix = "/my/accounts/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	if !strings.HasSuffix(rest, suffix) {
		return "", false
	}
	rest = strings.TrimSuffix(rest, suffix)
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// portalIconURL returns the relative icon URL. identityd serves both the portal
// HTML and /icon/, so same-origin is sufficient here; cross-origin consumers
// such as Slack must use passthrough.IconURL instead.
func portalIconURL(credName string) string {
	return "/icon/" + credName
}

// portalCredentialsFromUserIdentity projects ui.Spec.Credentials into the
// render-friendly list. Labels come from one cluster-wide MCPServer list —
// affordable only because the portal is low-traffic — and a List error degrades
// to an empty Label, which the page renders as the credential name.
func portalCredentialsFromUserIdentity(ctx context.Context, c client.Client, ui *spiceboxv1alpha1.UserIdentity, logger logr.Logger) []portalCredential {
	labels := portalLabelMap(ctx, c, logger)
	out := make([]portalCredential, 0, len(ui.Spec.Credentials))
	for _, cred := range ui.Spec.Credentials {
		out = append(out, portalCredential{
			Name:         cred.Name,
			Label:        labels[cred.Name],
			NeedsRefresh: credentialNeedsRefresh(cred, logger),
			IconURL:      portalIconURL(cred.Name),
		})
	}
	return out
}

// credentialNeedsRefresh answers the portal's real question for a Linked
// credential — can it be re-authorized through the OAuth ceremony, or does
// Replace mean pasting a new value — via the credkind registry instead of a
// literal type-string comparison against the oauth discriminator.
//
// An unregistered type logs at INFO (never silently dropped) and degrades to
// false: the same PAT-form route a legacy/untyped credential already gets,
// which is the safe default — routing an unrecognized type into a
// nonexistent OAuth ceremony would be worse than offering a paste form that
// turns out not to apply.
func credentialNeedsRefresh(cred spiceboxv1alpha1.AgentCredential, logger logr.Logger) bool {
	k, err := credkindregistry.Get(cred.Type)
	if err != nil {
		logger.Info("portal: unrecognized credential type; routing Replace to the PAT form",
			"credential", cred.Name, "type", cred.Type, "err", err.Error())
		return false
	}
	return k.NeedsRefresh()
}

// portalCredentialLabel resolves one credential's display label through the same
// index portalLabelMap builds. "" on error or no match.
func portalCredentialLabel(ctx context.Context, c client.Client, credName string, logger logr.Logger) string {
	labels := portalLabelMap(ctx, c, logger)
	return labels[credName]
}

// portalLabelMap indexes credential name → provider label across all
// MCPServers, keyed by CredentialNameForServer so resolution stays in lock-step
// with passthroughcatalog. Duplicates are first-writer-wins, deterministic
// because the apiserver lists alphabetically by name.
func portalLabelMap(ctx context.Context, c client.Client, logger logr.Logger) map[string]string {
	return mcpServerIndex(ctx, c, logger, "label", func(m *spiceboxv1alpha1.MCPServer) string {
		// An empty Provider contributes nothing: there is no display name to
		// show, and the caller falls back to the credential name.
		return m.Spec.Auth.Provider
	})
}

// portalAuthTypeMap indexes credential name → auth type ("static"|"oauth"|""),
// with the same resolution and dedup contract as portalLabelMap.
func portalAuthTypeMap(ctx context.Context, c client.Client, logger logr.Logger) map[string]string {
	return mcpServerIndex(ctx, c, logger, "auth-type", func(m *spiceboxv1alpha1.MCPServer) string {
		return m.Spec.Auth.Type
	})
}

// mcpServerIndex lists MCPServers cluster-wide, keys each by
// CredentialNameForServer, and keeps the FIRST non-empty extract value per
// credential name. Empty values are skipped so a server missing the field cannot
// shadow a later one that has it. Callers may rely on first-writer-wins.
//
// kind names the lookup in the List-error log so an operator can tell which one
// failed. A List error yields an empty index rather than an error.
func mcpServerIndex(
	ctx context.Context,
	c client.Client,
	logger logr.Logger,
	kind string,
	extract func(*spiceboxv1alpha1.MCPServer) string,
) map[string]string {
	out := map[string]string{}
	var list spiceboxv1alpha1.MCPServerList
	if err := c.List(ctx, &list); err != nil {
		logger.Info("portal: MCPServer list failed for "+kind+" resolution", "err", err.Error())
		return out
	}
	for i := range list.Items {
		m := &list.Items[i]
		val := extract(m)
		if val == "" {
			continue
		}
		credName := passthroughcatalog.CredentialNameForServer(m)
		if _, present := out[credName]; !present {
			out[credName] = val
		}
	}
	return out
}
