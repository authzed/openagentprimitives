// pkg/platform/identityd/handlers_link.go — `/link` (GET) and `/link/submit` (POST).
//
// /link verifies the channelsd-minted signed deep-link, gates on the session's
// starter canonical and on the OIDC cookie, then renders one input per required
// credential. /link/submit re-verifies both gates and persists each non-empty
// bearer token via useridentity.PutToken (idempotent — a re-submit replaces).
//
// Defense in depth: the cookie alone is never trusted. Every state-changing path
// re-verifies the HMAC-signed payload and re-checks cookie subject == payload.Subject.
package identityd

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/pkg/web/admind/agentcred"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// cookieName is the HttpOnly cookie marking the visitor as OIDC-authenticated.
// Its value is a passthroughlink-signed payload carrying only Subject +
// ExpiresAt; an empty SessionRef is the convention that identifies a payload as
// a cookie rather than a deep-link.
const cookieName = "idd_session"

// cookieTTL bounds a cookie-only OIDC session before re-auth is required,
// enforced by passthroughlink's ExpiresAt check on every gated hit. IdP-verified
// logins get the longer sessionTTL instead.
const cookieTTL = 10 * time.Minute

// staticMenuWhy is the neutral per-row fallback rendered when a credential's SUI
// item declares no Why. identityd has no LLM explainer — that gap-fill is
// channelsd-only, applied to the Slack DM.
const staticMenuWhy = "This agent needs to call this service as you to complete the work you asked for."

// authKindForSession returns the channel-kind name identityd dispatches OIDC
// to: the session's spec.inputChannel.kind, so each channel kind owns its
// users' OIDC ceremony.
//
// Deliberately names no kind: dispatch goes through the channelkinds registry,
// so a kind name lives only on the session and in the registry blank imports.
//
// There is deliberately NO query override. One existed — `?auth=<kind>`,
// documented as an E2E and integration affordance — and it was reachable in
// production while NOTHING in the tree used it, not even the tests it was
// written for. A caller could name any registered authenticator, including the
// always-registered `fake` one, which made this one code change away from an
// auth bypass rather than one today. A test affordance no test uses is
// production attack surface with no compensating benefit.
func authKindForSession(_ *http.Request, sess *spiceboxv1alpha1.AgentSession) string {
	if sess != nil && sess.Spec.InputChannel != nil {
		return sess.Spec.InputChannel.Kind
	}
	return ""
}

// handleLinkGet implements GET /link?d=<base64>&sig=<hex>.
//
// Check order is significant: signature, then session lookup, then the
// starter-recipient gate, then the cookie gate. The OIDC redirect fires only
// after all of them pass, so a tampered URL never kicks off a real OAuth flow.
func (s *Server) handleLinkGet(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	// 1. Verify the signed link.
	d := r.URL.Query().Get("d")
	sig := r.URL.Query().Get("sig")
	if d == "" || sig == "" {
		logger.Info("link: missing d/sig query params", "path", r.URL.Path)
		s.writeError(w, r, http.StatusBadRequest, "Invalid link", "This link is missing required parameters.")
		return
	}
	raw := d + "." + sig
	// Deep-links are minted by channelsd with iss=channelsd, aud=identityd.
	// A cookie payload (iss=identityd) presented in the URL is a sign of
	// a confusion attack — refuse explicitly.
	payload, err := s.deps.LinkSigner.Verify(raw,
		passthroughlink.WithExpectedIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithExpectedAudience(passthroughlink.AudienceIdentityd))
	if err != nil {
		s.writeLinkVerifyError(w, r, err, logger)
		return
	}

	// 2. Resolve session. The payload is trustworthy in the integrity sense,
	//    but its claimed Subject is not yet proven to be THIS visitor —
	//    step 5's cookie gate is what proves that.
	ns, name, ok := splitSessionRef(payload.SessionRef)
	if !ok {
		logger.Info("link: malformed SessionRef", "ref", payload.SessionRef)
		s.writeError(w, r, http.StatusBadRequest, "Invalid link", "This link refers to a session in an unexpected format.")
		return
	}
	var sess spiceboxv1alpha1.AgentSession
	if err := s.deps.K8s.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		logger.Info("link: AgentSession lookup failed", "ref", payload.SessionRef, "err", err.Error())
		s.writeError(w, r, http.StatusNotFound, "Session not found",
			"This session no longer exists. The work you started has likely been cancelled or has timed out.")
		return
	}

	// 3. Whose credential is this? A credential_update link can cover the
	//    AGENT's own shared credential, which routes to a different gate and
	//    write path (credupdate_agentowned.go). Resolved BEFORE the starter
	//    gate: the monitoring-channel variant carries no Subject, so the
	//    starter comparison would refuse it — and the permission check that
	//    replaces it is strictly stronger, not a relaxation.
	agentOwned, err := s.resolveAgentOwnedTarget(r.Context(), payload)
	if err != nil {
		s.failAgentOwnedResolve(w, r, payload.Purpose, err, logger)
		return
	}
	if agentOwned != nil {
		s.handleAgentOwnedLinkGet(w, r, payload, agentOwned, raw, d, sig)
		return
	}

	// 4. Recipient gate: only the canonical the operator recorded as the
	//    session's starter may link its credentials. A forwarded link is
	//    harmless anyway — the cookie gate below won't match.
	starter := spiceboxv1alpha1.StartedBySubject(&sess)
	if starter != payload.Subject {
		logger.Info("link: starter mismatch",
			"link_subject", payload.Subject, "session_starter", starter, "session", payload.SessionRef)
		s.writeError(w, r, http.StatusForbidden, "Not your session", "This link was meant for a different user.")
		return
	}

	// 5. OIDC cookie gate. No cookie: delegate to /oidc/login, the single
	//    entry running the full chain (cluster-IdP → channel-kind →
	//    trust-links), which returns here with a cookie set. The
	//    subject-equality check below then proves the signed-in visitor IS
	//    the link's (starter's) subject.
	cookieSubject, cookieOK := s.checkOIDCCookie(r)
	if !cookieOK {
		http.Redirect(w, r, "/oidc/login?d="+url.QueryEscape(d)+"&sig="+url.QueryEscape(sig), http.StatusFound)
		return
	}
	if cookieSubject != payload.Subject {
		logger.Info("link: cookie subject mismatch", "cookie", cookieSubject, "expected", payload.Subject)
		s.writeError(w, r, http.StatusForbidden, "Not your session",
			"You're signed in as a different user than this link is for.")
		return
	}

	// 6. Render the menu. Per-credential items from SessionUserIdentity status
	//    are best-effort; fall back to the raw credential names so the page
	//    still renders before the operator has written an explanation.
	items := s.explanationFor(r.Context(), ns, name)
	what := make([]string, 0, len(payload.RequiredCredentials))
	for _, cred := range payload.RequiredCredentials {
		if it, ok := items[cred]; ok && it.Title != "" {
			what = append(what, it.Title)
		}
	}
	if len(what) == 0 {
		what = payload.RequiredCredentials
	}

	rows := s.buildMenuRows(r.Context(), payload.Subject.String(), payload.RequiredCredentials, items, logger)

	if err := s.renderApp(r, w, "identity-link", linkPageData{
		SessionRef: payload.SessionRef,
		What:       what,
		SignedLink: raw,
		Rows:       rows,
	}, webui.PageMeta{Title: "Connect your credentials"}); err != nil {
		// Headers are already sent, so logging is all that is left; the
		// visitor sees a truncated page.
		logger.Info("link: app render failed", "err", err.Error())
	}
}

// buildMenuRows projects the parked session's required credentials into menu
// rows: label, "linked"/"missing" status from the subject's UserIdentity, and
// "oauth"/"pat" kind from the backing MCPServer's Spec.Auth.Type. A credential
// with no backing MCPServer defaults to "pat" — envvar bindings only carry
// static tokens.
//
// Every lookup is cluster-wide and best-effort: a failure degrades one row
// rather than aborting the menu. subject is trustworthy because handleLinkGet
// has already cleared the signature + cookie gates.
func (s *Server) buildMenuRows(ctx context.Context, subject string, credNames []string, items map[string]spiceboxv1alpha1.CredentialExplanationItem, logger logr.Logger) []linkMenuRow {
	labels := portalLabelMap(ctx, s.deps.K8s, logger)
	authKinds := portalAuthTypeMap(ctx, s.deps.K8s, logger)
	linked := s.linkedCredentialSet(ctx, subject, logger)
	base := strings.TrimRight(s.deps.ExternalBaseURL(), "/")
	rows := make([]linkMenuRow, 0, len(credNames))
	for _, name := range credNames {
		// Prefer the operator-resolved Title (tool field → provider catalog →
		// humanized name); fall back to the portal's provider-label lookup.
		label := items[name].Title
		if label == "" {
			label = labels[name]
		}
		row := linkMenuRow{
			CredentialName: name,
			Label:          label,
			Kind:           "pat",
			IconURL:        "/icon/" + name,
		}
		// An empty Why is the deliberate "no reason declared" signal, not a
		// lookup miss; identityd substitutes a neutral static sentence.
		why := items[name].Why
		if strings.TrimSpace(why) == "" {
			why = staticMenuWhy
		}
		row.Why = why
		row.Instructions, row.DocsURL = credInstructions(name)
		if authKinds[name] == "oauth" {
			row.Kind = "oauth"
			row.OAuthURL = base + "/link/oauth/" + name
		}
		if linked[name] {
			row.Status = "linked"
		} else {
			row.Status = "missing"
		}
		rows = append(rows, row)
	}
	return rows
}

// linkedCredentialSet returns the credentials already linked for subject. A
// subject with no UserIdentity yet maps to an empty set, which the caller
// correctly reads as "everything is missing".
func (s *Server) linkedCredentialSet(ctx context.Context, subject string, logger logr.Logger) map[string]bool {
	out := map[string]bool{}
	uiName := useridentity.NameForSubject(identity.Subject(subject))
	var ui spiceboxv1alpha1.UserIdentity
	if err := s.deps.K8s.Get(ctx, client.ObjectKey{Name: uiName}, &ui); err != nil {
		// Usually NotFound (no creds linked yet); either way the menu falls
		// back to "everything missing". Logged so real Get errors stay visible.
		logger.V(1).Info("link/menu: UserIdentity load skipped", "subject", subject, "err", err.Error())
		return out
	}
	for _, c := range ui.Spec.Credentials {
		out[c.Name] = true
	}
	return out
}

// handleLinkSubmit implements POST /link/submit — one credential per submit,
// posting `link` (the signed deep-link), `credential`, and `token`.
//
// Every gate from handleLinkGet is repeated: never trust the cookie alone, the
// link alone, or the form alone. The signed link is deliberately REUSABLE across
// per-credential submits — the cookie gate, not single-use, is what defends
// against link forwarding here. On success the visitor is redirected back to the
// menu so the linked row's badge flips and the rest stay reachable.
func (s *Server) handleLinkSubmit(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	if err := r.ParseForm(); err != nil {
		logger.Info("link/submit: ParseForm failed", "err", err.Error())
		s.writeError(w, r, http.StatusBadRequest, "Invalid form", "Could not read the form submission.")
		return
	}
	raw := r.FormValue("link")
	if raw == "" {
		logger.Info("link/submit: missing link form field")
		s.writeError(w, r, http.StatusBadRequest, "Invalid form", "Missing link.")
		return
	}
	payload, err := s.deps.LinkSigner.Verify(raw,
		passthroughlink.WithExpectedIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithExpectedAudience(passthroughlink.AudienceIdentityd))
	if err != nil {
		s.writeLinkVerifyError(w, r, err, logger)
		return
	}

	// Same split as handleLinkGet: the agent-owned route authorizes by
	// permission rather than subject-equality and writes a different object,
	// so it is resolved BEFORE the subject gate — a subject-less monitoring
	// link must not be refused by a comparison that cannot apply to it.
	agentOwned, resolveErr := s.resolveAgentOwnedTarget(r.Context(), payload)
	if resolveErr != nil {
		s.failAgentOwnedResolve(w, r, payload.Purpose, resolveErr, logger)
		return
	}

	cookieSubject, cookieOK := s.checkOIDCCookie(r)
	switch {
	case agentOwned != nil:
		if !cookieOK {
			// A POST body cannot be replayed through a sign-in redirect, so
			// this refuses where the GET path redirects.
			logger.Info("link/submit: no valid cookie on an agent-owned credential update")
			s.writeError(w, r, http.StatusForbidden, "Not signed in",
				"Open the link again to sign in, then re-submit the form.")
			return
		}
		if ok, why := s.mayUpdateAgentCredential(r.Context(), cookieSubject, payload, agentOwned, logger); !ok {
			s.refuseAgentOwned(w, r, payload.Purpose, why, logger)
			return
		}
	case !cookieOK || cookieSubject != payload.Subject:
		logger.Info("link/submit: cookie gate failed",
			"cookie_present", cookieOK, "cookie_subject", cookieSubject, "link_subject", payload.Subject)
		s.writeError(w, r, http.StatusForbidden, "Not your session",
			"Sign in again and re-submit the form.")
		return
	}

	credName := strings.TrimSpace(r.FormValue("credential"))
	token := strings.TrimSpace(r.FormValue("token"))
	if credName == "" || token == "" {
		logger.Info("link/submit: missing credential or token",
			"credPresent", credName != "", "tokenPresent", token != "")
		s.writeError(w, r, http.StatusBadRequest, "Invalid form",
			"Missing credential name or token value.")
		return
	}
	// The signed link bounds which credentials this submit may write. Without
	// this, an attacker holding both the link AND the cookie could overwrite a
	// credential the session never asked for.
	if !contains(payload.RequiredCredentials, credName) {
		logger.Info("link/submit: credential not in signed payload",
			"credential", credName, "allowed", payload.RequiredCredentials)
		s.writeError(w, r, http.StatusForbidden, "Credential not allowed",
			"This link doesn't cover that credential.")
		return
	}
	// Defense in depth: the contains() check above already implies this, since
	// an agent-owned link must cover exactly one credential. But an agent-owned
	// write picks its destination from the TARGET, not the form, and no future
	// loosening may let one credential's value land under another's name.
	if agentOwned != nil && credName != agentOwned.Credential {
		logger.Info("link/submit: form credential does not match the resolved agent-owned credential",
			"form", credName, "resolved", agentOwned.Credential)
		s.writeError(w, r, http.StatusForbidden, "Credential not allowed",
			"This link doesn't cover that credential.")
		return
	}

	// Reject a wrong/malformed paste at the door rather than letting it fail
	// opaquely the first time the agent uses it.
	if err := s.validatePastedToken(r.Context(), credName, token); err != nil {
		logger.Info("link/submit: token format rejected",
			"credential", credName, "subject", payload.Subject, "err", err.Error())
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
		logger.Info("link/submit: storing provider-rejected token on user confirmation",
			"credential", credName, "subject", payload.Subject)
	} else {
		// askToConfirm re-renders the paste as the verify-warn page: nothing is
		// stored until the visitor presses "store anyway", which returns as
		// verifyConfirm=1. Shared by every arm needing a human decision so they
		// cannot drift apart on where the form posts.
		askToConfirm := func(warning string) {
			cancel := "/my/accounts"
			if d, sig, ok := splitSignedLink(raw); ok {
				cancel = "/link?d=" + d + "&sig=" + sig
			}
			s.renderVerifyWarn(w, r, verifyWarnData{
				Warning: warning,
				Action:  "/link/submit",
				Hidden: map[string]string{
					"link": raw, "credential": credName, "token": token, "verifyConfirm": "1",
				},
				CancelURL: cancel,
			})
		}
		verified = s.verifyPastedToken(r.Context(), credName, token)
		switch verified.Status {
		case builtins.VerifyRejected:
			logger.Info("link/submit: token failed live verification",
				"credential", credName, "subject", payload.Subject, "detail", verified.Detail)
			askToConfirm(verified.Detail)
			return
		case builtins.VerifyForbidden:
			// The provider took the credential and refused only this check —
			// not a rejection. The visitor is right here, so let them decide
			// rather than storing something the check could not confirm.
			logger.Info("link/submit: provider accepted the credential but refused the verification check; asking the user to confirm",
				"credential", credName, "subject", payload.Subject, "detail", verified.Detail)
			askToConfirm(builtins.ForbiddenNotice(verified))
			return
		case builtins.VerifyIndeterminate:
			logger.Info("link/submit: could not verify token; storing anyway",
				"credential", credName, "detail", verified.Detail)
			noticeVerb = "linkedunverified"
		case builtins.VerifyUnsupported:
			logger.Info("link/submit: no live verification available; storing",
				"credential", credName, "detail", verified.Detail)
		case builtins.VerifyValid:
			logger.Info("link/submit: token verified",
				"credential", credName, "subject", verified.Subject)
		default:
			// Fail-closed exhaustiveness backstop: reaching here means a
			// VerifyStatus was added without teaching this handler what it
			// means, so nothing is stored until the visitor confirms. Without
			// this arm the next new verdict would fall open into a store.
			logger.Info("link/submit: unrecognised verification verdict; asking the user to confirm",
				"credential", credName, "subject", payload.Subject,
				"status", string(verified.Status), "detail", verified.Detail)
			askToConfirm(builtins.UnrecognizedNotice(verified))
			return
		}
	}

	// The write lands on a different object — and, for the agent-owned case, in
	// a different PROCESS.
	//
	// A person's own credential is written here to their master Secret in the
	// identities namespace (the only namespace webd has Secret access to). An
	// agent's shared credential is submitted to the OPERATOR, which re-resolves
	// the target, re-checks agentidentity#update_credential on the asserted
	// subject, and patches the AgentIdentity's Secret. identityd never touches
	// that Secret, nor the submitter's UserIdentity, on the agent-owned path.
	if agentOwned != nil {
		written, err := s.submitAgentOwned(r.Context(), cookieSubject, payload, agentOwned, credName, token)
		if errors.Is(err, agentcred.ErrRefused) {
			// The OPERATOR said no even though identityd's gate said yes: its
			// check is FullyConsistent, runs later, and is authoritative.
			// Render the permission page, not "try a different value".
			s.refuseAgentOwned(w, r, payload.Purpose, "the operator refused: "+err.Error(), logger)
			return
		}
		if err != nil {
			logger.Info("link/submit: agent-owned credential replacement failed",
				"identity", agentOwned.String(), "subject", cookieSubject.String(), "err", err.Error())
			// Default to a conflict between the pasted value and what the
			// credential accepts — actionable by the admin. A missing operator
			// client is instead a deployment wiring fault that no paste or
			// retry can resolve, so it gets a server-error status.
			status, title := http.StatusConflict, "Couldn't replace the credential"
			if errors.Is(err, errNoCredentialWriter) {
				status, title = http.StatusInternalServerError, "This can't be saved here yet"
			}
			s.writeError(w, r, status, title, agentOwnedResolveMessage(err))
			return
		}
		logger.Info("link/submit: the operator replaced an agent's own shared credential",
			"identity", agentOwned.String(), "secret", written.SecretRef.Namespace+"/"+written.SecretRef.Name,
			"subject", cookieSubject.String())
	} else if err := useridentity.PutToken(r.Context(), s.deps.K8s, useridentity.PutTokenRequest{
		Subject:        payload.Subject,
		CredentialName: credName,
		Token:          token,
		// What the live check observed about WHOSE account this is, recorded
		// with the credential so the UserIdentity reconciler can bind it to
		// this catalog's declared owner. Both halves come from the
		// verification that just ran; neither is re-derived here.
		ProviderID: verified.ProviderID,
		SubjectID:  verified.SubjectID,
	}); err != nil {
		logger.Info("link/submit: PutToken failed",
			"credential", credName, "subject", payload.Subject, "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Couldn't link",
			"Could not save the credential. Please try again.")
		return
	}

	// Reconstruct the menu GET URL from the signed link's halves.
	d, sig, ok := splitSignedLink(raw)
	if ok {
		http.Redirect(w, r, "/link?d="+d+"&sig="+sig+"&notice="+url.QueryEscape(noticeVerb+":"+credName), http.StatusFound)
		return
	}
	// Verify already validated the structure, so this is unreachable in
	// practice; the credential IS saved, so land the visitor on the portal
	// rather than a blank tab.
	http.Redirect(w, r, "/my/accounts", http.StatusSeeOther)
}

// contains reports whether needle appears in haystack.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// splitSignedLink reverses Mint's "<d>.<sig>" join so a redirect URL can be
// rebuilt without re-encoding. ok=false on a missing dot or an empty half.
func splitSignedLink(raw string) (d, sig string, ok bool) {
	idx := strings.IndexByte(raw, '.')
	if idx <= 0 || idx == len(raw)-1 {
		return "", "", false
	}
	return raw[:idx], raw[idx+1:], true
}

// checkOIDCCookie reads and verifies the idd_session cookie, returning the
// proven canonical subject. Absent, expired, forged, and non-cookie payloads all
// collapse to ("", false) — indistinguishable from "no cookie" to every caller.
//
// The cookie's iss MUST be identityd, the issuer we mint under. A channelsd
// deep-link pasted into the cookie field carries iss=channelsd and is refused,
// so a stolen deep-link cannot be forged into a session cookie.
func (s *Server) checkOIDCCookie(r *http.Request) (subject identity.Subject, ok bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	p, err := s.deps.LinkSigner.Verify(c.Value,
		passthroughlink.WithExpectedIssuer(passthroughlink.IssuerIdentityd),
		passthroughlink.WithExpectedAudience(passthroughlink.AudienceIdentityd))
	if err != nil {
		// Forged, expired, or not from our signer — fall through to re-auth.
		return "", false
	}
	if p.Subject == "" {
		// An attacker cannot forge a valid signature over an empty Subject,
		// but a buggy minter could. Never authenticate as the empty string.
		return "", false
	}
	return p.Subject, true
}

// linkSessionPurposes is the fail-closed allowlist of link Purposes that may be
// exchanged for an idd_session cookie without a real login. A purpose belongs
// here only when the session it produces grants no more than the link already
// does.
//
// "portal" qualifies because that IS its grant: the link exists to let its
// holder manage that subject's own connected accounts, which is exactly what the
// cookie unlocks — behind the single-use control in the callers.
//
// Nothing else qualifies. artifact_view / session_view grant one artifact or
// transcript; cli_identity is a CLI bearer with its own audience; the
// empty-purpose credential-request link and credential_update are cookie-gated
// precisely so a forwarded link is harmless. Trading any of those for a session
// would hand its bearer a credential far wider than the link — see
// setTrustLinkCookie.
var linkSessionPurposes = map[string]struct{}{
	purposePortal: {},
}

// linkSessionRefusal names why a link may not be exchanged for a session, so a
// caller that wants to explain itself can render the right page. "" means the
// link may.
type linkSessionRefusal string

const (
	// refusalPurpose — the Purpose is not in linkSessionPurposes.
	refusalPurpose linkSessionRefusal = "purpose"
	// refusalOrigin — not a channelsd-issued, identityd-audience deep-link.
	refusalOrigin linkSessionRefusal = "origin"
	// refusalSubject — no Subject to mint a session for.
	refusalSubject linkSessionRefusal = "subject"
)

// linkMayMintSession is the ONE gate on turning a signed link into an
// idd_session, shared by every entry that can do it (/oidc/login, /my/accounts).
// Both must agree, or the stricter one is just the long way round to the laxer.
//
// It pins issuer + audience itself rather than leaning on the caller's Verify:
// /oidc/login deliberately verifies integrity only (it also receives aud=webd
// artifact links and iss=identityd admin-login links, which must reach the real
// login chain), so this is the only place those claims are checked on the
// cookie-minting path.
//
// SubjectVerified is deliberately NOT consulted: it records that the MINTER
// proved the subject's email, and says nothing about who HOLDS the link — the
// only question here. Consulting it would let possession of any link carrying it
// (a shared artifact-view link, the on-disk CLI assertion) buy a session as that
// link's subject.
func linkMayMintSession(p passthroughlink.Payload) (bool, linkSessionRefusal) {
	if _, ok := linkSessionPurposes[p.Purpose]; !ok {
		return false, refusalPurpose
	}
	if p.Issuer != passthroughlink.IssuerChannelsd || p.Audience != passthroughlink.AudienceIdentityd {
		return false, refusalOrigin
	}
	if p.Subject == "" {
		return false, refusalSubject
	}
	return true, ""
}

// setTrustLinkCookie mints + sets the idd_session cookie from a signed
// deep-link's claimed Subject. Callers MUST have cleared linkMayMintSession
// first — or be /oidc/login's --insecure-trust-links fallback, the dev-only door
// that trusts any link by explicit operator opt-in. The value is the same
// Subject+ExpiresAt payload the OIDC callback sets, and downstream gates cannot
// tell which path established it.
//
// That indifference is why the gate matters: this cookie is a GENERAL
// authentication credential at Path=/, honored by every cookie-gated route here
// and by webd's framework-level authenticate (/admin included, where it decides
// whose SpiceDB platform permissions are evaluated). A link with a narrower
// grant must never be traded for one.
//
// A mint error is logged and non-fatal: the caller still renders with the
// in-memory subject, and the next request re-enters here for a fresh cookie.
func (s *Server) setTrustLinkCookie(w http.ResponseWriter, subject identity.Subject, logger logr.Logger) {
	raw, err := s.deps.LinkSigner.Mint(passthroughlink.Payload{
		Subject:   subject,
		ExpiresAt: time.Now().Add(cookieTTL).Unix(),
	})
	if err != nil {
		logger.Info("link: trust-link cookie mint failed", "err", err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    raw,
		Path:     "/",
		MaxAge:   int(cookieTTL / time.Second),
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.deps.ExternalBaseURL(), "https://"),
		SameSite: http.SameSiteLaxMode,
	})
}

// writeLinkVerifyError maps passthroughlink.Verify's sentinel errors — part of
// its public contract — to user-facing pages: expired, bad signature (usually a
// truncated paste), or malformed.
func (s *Server) writeLinkVerifyError(w http.ResponseWriter, r *http.Request, err error, logger logr.Logger) {
	logger.Info("link: signature verify failed", "err", err.Error())
	switch {
	case errors.Is(err, passthroughlink.ErrExpired):
		s.writeError(w, r, http.StatusBadRequest, "Link expired",
			"This connection link is no longer valid. Ask your agent to start the work again.")
	case errors.Is(err, passthroughlink.ErrInvalidSignature):
		s.writeError(w, r, http.StatusBadRequest, "Invalid link",
			"This link's signature is invalid. Make sure you used the link the agent gave you in full.")
	default:
		s.writeError(w, r, http.StatusBadRequest, "Invalid link", "This link is malformed.")
	}
}

// writeError is the single chokepoint for error HTML responses, routing through
// the webui system page so every identityd error shares webd's styled surface.
// The nil-renderer branch is a plain-text last resort, reachable only when the
// handler was called outside webui.Server.ServeHTTP (a direct unit test).
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, title, msg string) {
	// Never cache: a transient 403 (e.g. before an IdP is configured) was
	// otherwise held by the browser / shared LB across the user's retries.
	w.Header().Set("Cache-Control", "no-store")
	rndr := webui.RendererFromContext(r.Context())
	if rndr == nil {
		http.Error(w, title+": "+msg, status) // last-resort: not reached through Server.ServeHTTP (e.g. a direct unit-test call)
		return
	}
	rndr.RenderError(w, &webui.PageError{Status: status, Kind: errorKind(status), Title: title, Message: msg})
}

// renderApp is the page-render chokepoint mirroring writeError, rendering React
// app `app` through the renderer the Server injected on the request context.
//
// Like writeError, the nil-renderer branch is reachable only outside
// webui.Server.ServeHTTP (a direct unit test); it returns an error rather than
// panicking on the nil interface so those tests fail cleanly.
func (s *Server) renderApp(r *http.Request, w http.ResponseWriter, app string, props any, meta webui.PageMeta) error {
	rndr := webui.RendererFromContext(r.Context())
	if rndr == nil {
		return errNoRenderer
	}
	// These pages are per-user and auth-gated: a shared LB keyed on URL alone
	// could otherwise serve one user's rendered page to another.
	w.Header().Set("Cache-Control", "no-store")
	return rndr.RenderApp(w, app, props, meta, false)
}

// errNoRenderer is returned by renderApp when the request context carries no
// framework renderer — only reachable off the Server path.
var errNoRenderer = errors.New("identityd: no webui renderer on request context")

// verifyWarnData is the props payload for the identity-verify-warn app: the
// warning plus a re-submit form carrying every original field back with
// verifyConfirm=1.
type verifyWarnData struct {
	// Warning is the user-facing reason live verification did not confirm the
	// pasted credential; shown verbatim above the confirm button.
	Warning string `json:"warning"`
	// Action is the same-origin path the confirm form re-POSTs to.
	Action string `json:"action"`
	// Hidden replays the original form fields, INCLUDING the pasted secret, so
	// the confirm POST can store it; it is never persisted server-side.
	Hidden map[string]string `json:"hidden"`
	// CancelURL is where "don't store this" lands the visitor.
	CancelURL string `json:"cancelUrl"`
}

func (s *Server) renderVerifyWarn(w http.ResponseWriter, r *http.Request, d verifyWarnData) {
	if err := s.renderApp(r, w, "identity-verify-warn", d, webui.PageMeta{Title: "Verify token"}); err != nil {
		log.FromContext(r.Context()).Info("verify-warn: render failed", "err", err.Error())
	}
}

// errorKind maps an HTTP status to the webui system page's Kind so the styled
// page picks the right treatment; anything unmapped falls back to "error".
func errorKind(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "notFound"
	default:
		return "error"
	}
}

// splitSessionRef parses "<ns>/<name>". ok=false on the wrong segment count or
// an empty segment.
func splitSessionRef(ref string) (ns, name string, ok bool) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// explanationFor returns the parked session's per-credential explanation items
// keyed by credential name, or nil when the SessionUserIdentity is absent, has
// no explanation, or errors. Best-effort: the menu must render before the
// operator has written one, and the caller's fallback makes the nil case
// routine rather than a condition worth logging.
func (s *Server) explanationFor(ctx context.Context, ns, name string) map[string]spiceboxv1alpha1.CredentialExplanationItem {
	var suid spiceboxv1alpha1.SessionUserIdentity
	if err := s.deps.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &suid); err != nil {
		return nil
	}
	if suid.Status.Explanation == nil {
		return nil
	}
	out := make(map[string]spiceboxv1alpha1.CredentialExplanationItem, len(suid.Status.Explanation.Items))
	for _, it := range suid.Status.Explanation.Items {
		out[it.Credential] = it
	}
	return out
}
