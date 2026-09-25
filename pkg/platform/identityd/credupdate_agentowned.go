// The AGENT-OWNED half of the credential_update deep-link flow.
//
// A credential_update link (passthroughlink.PurposeCredentialUpdate) covers one
// of two very different things, and identityd must not confuse them:
//
//   - A PERSON's credential (UserIdentity / SessionUserIdentity): the click
//     writes that person's own master Secret via useridentity.PutToken —
//     handlers_link.go's path.
//   - The AGENT's own SHARED credential (an AgentIdentity): replacing it changes
//     what every session running that agent authenticates as. Permission is
//     `agentidentity#update_credential`, and the value belongs in the
//     AgentIdentity's backing Secret, NOT in the clicker's UserIdentity.
//
// Routing the second through the first overwrites the admin's personal
// credential, leaves the shared one dead, says "Connected", and later expires
// announcing that nobody updated it.
//
// # identityd does not write the credential, and holds no CLUSTER-WIDE Secret access
//
// The write happens in the OPERATOR (POST /admin/v1/credentials/agent-update),
// reached through pkg/web/admind/agentcred. identityd's own Secret grant is a
// namespaced Role over spiceboxv1alpha1.IdentitiesNamespace (config/webd/
// role.yaml), enough for the per-person master Secrets it writes on the
// user-owned path and nothing else. The agent-owned write would need Secret
// get+update across every namespace — standing RBAC on a browser-facing process
// that outlives every request, and on compromise reads and overwrites the
// SpiceDB preshared key, the passthroughlink HMAC key, and every stored
// credential in the cluster.
//
// # Two gates; only the operator's is the security gate
//
// identityd's is a UX gate: prove a subject from the idd_session cookie, ask
// whether it looks permitted, and refuse early rather than invite someone to
// paste a live credential into a page that will reject it. The operator
// independently re-resolves the target and re-runs a FullyConsistent
// `agentidentity#update_credential` check on the subject identityd ASSERTS, and
// refuses a caller that lied. Deleting this gate costs UX; deleting the
// operator's costs safety.
//
// # Whose credential it is comes from the CR, not the link
//
// The signed payload carries no identity reference — only SessionRef, the
// required credential names, and the purpose. The authority is the
// CredentialUpdateRequest's own status.resolvedCredential. identityd and the
// operator resolve it through the SAME function
// (agentidentity.ResolveRequestTarget), so the object the browser was shown and
// the object the operator authorizes cannot drift apart.
//
// # Authorization happens at the CLICK
//
// The monitoring-channel variant has NO Subject — a monitoring channel has no
// single addressee — so anyone who can see the channel, or is forwarded the URL,
// holds the link. A click-time check is the only possible control, and it is
// re-run on every request: someone who held the permission when the card
// published and lost it before clicking is refused.
package identityd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/agentidentity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/web/admind/agentcred"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// AgentIdentityAuthz answers "may this human replace the agent's own shared
// credential?" — `agentidentity:<ns>/<name>#update_credential`.
//
// This backs identityd's UX gate ONLY; the authoritative check runs in the
// operator (see the package doc). Declared here as a consumer-owned one-method
// interface rather than imported from channelsd's pipeline package, which asks
// the same question of the same implementation for a different reason.
//
// Consistency is deliberately NOT a parameter: the #platform tuple that makes
// this permission satisfiable is written by the very reconcile that surfaces the
// dead credential, so a MinimizeLatency read can land on an older snapshot and
// refuse a legitimate admin indistinguishably from "not permitted".
type AgentIdentityAuthz interface {
	CheckAgentIdentityUpdateCredential(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) (bool, error)
}

// AgentCredentialWriter submits a replacement value to the OPERATOR, which
// independently authorizes it and performs the write. identityd never touches a
// Secret.
//
// The subject argument is an ASSERTION of who is signed in — identityd owns the
// idd_session cookie, so it is the right component to make that claim — and is
// explicitly NOT a statement that the write is permitted. Implementations must
// not carry an "already authorized" flag, and the operator must not read one if
// they did.
type AgentCredentialWriter interface {
	Replace(ctx context.Context, subject identity.Subject, req agentcred.Request) (agentcred.Response, error)
}

// resolveAgentOwnedTarget reports which AgentIdentity-owned credential this
// link covers, or (nil, nil) when it covers a user-owned one (or is not a
// credential_update / workshop_credential link at all, in which case nothing
// is read).
//
// A non-nil error means "refuse": the caller must NOT fall through to the
// user-owned write path, because "I could not tell whose credential this is"
// and "it is the clicker's own" are not the same statement, and treating them
// alike is how a pasted bot token lands in an admin's personal identity.
func (s *Server) resolveAgentOwnedTarget(ctx context.Context, payload passthroughlink.Payload) (*agentidentity.RequestTarget, error) {
	switch payload.Purpose {
	case passthroughlink.PurposeCredentialUpdate:
		ns, name, ok := splitSessionRef(payload.SessionRef)
		if !ok {
			return nil, fmt.Errorf("%w: malformed sessionRef %q", agentidentity.ErrAmbiguousOwner, payload.SessionRef)
		}
		return agentidentity.ResolveRequestTarget(ctx, s.deps.K8s, ns, name, payload.RequiredCredentials)
	case passthroughlink.PurposeWorkshopCredential:
		return resolveWorkshopCredentialTarget(payload)
	default:
		// Every other link kind (the plain credential-request deep-link, the
		// portal link) means "the clicker's own credential" — no List, no new
		// failure mode on those paths.
		return nil, nil
	}
}

// resolveWorkshopCredentialTarget names the AgentIdentity a
// PurposeWorkshopCredential link covers, straight from the signed payload's
// own AgentIdentityRef + RequiredCredentials.
//
// Deliberately NOT agentidentity.ResolveRequestTarget: that function exists
// to disambiguate a link that names only a SESSION across however many
// CredentialUpdateRequests it produced. A workshop-credential link carries no
// CredentialUpdateRequest at all — AgentIdentityRef names the identity
// directly — so there is nothing to disambiguate, only a well-formedness
// check to make. No I/O: the payload alone answers the question.
func resolveWorkshopCredentialTarget(payload passthroughlink.Payload) (*agentidentity.RequestTarget, error) {
	ns, name, ok := splitSessionRef(payload.AgentIdentityRef)
	if !ok {
		return nil, fmt.Errorf("%w: malformed agentIdentityRef %q", agentidentity.ErrAmbiguousOwner, payload.AgentIdentityRef)
	}
	if len(payload.RequiredCredentials) != 1 {
		return nil, fmt.Errorf("%w: a workshop-credential link must cover exactly one credential, got %v",
			agentidentity.ErrAmbiguousOwner, payload.RequiredCredentials)
	}
	return &agentidentity.RequestTarget{
		Namespace:  ns,
		Name:       name,
		Credential: payload.RequiredCredentials[0],
	}, nil
}

// mayUpdateAgentCredential runs the click-time UX gate and returns a reason when
// it says no. The operator re-runs the same question authoritatively; this one
// exists so an unauthorized visitor is refused BEFORE being invited to paste a
// live credential.
//
// Purpose changes WHICH question is asked: everything below still checks
// agentidentity#update_credential (platform-admin-only) exactly as before;
// PurposeWorkshopCredential is dispatched to a different authority entirely —
// see mayUpdateWorkshopCredential.
//
// FAIL-CLOSED in every branch that is not an outright "yes": no authorization
// client wired, a non-user cookie subject, or a failed check all refuse. Each
// branch logs its own cause — an operator whose webd lost its SpiceDB client
// must not be sent off to grant a permission that was never the problem.
func (s *Server) mayUpdateAgentCredential(ctx context.Context, subject identity.Subject, payload passthroughlink.Payload, t *agentidentity.RequestTarget, logger logr.Logger) (bool, string) {
	if payload.Purpose == passthroughlink.PurposeWorkshopCredential {
		return s.mayUpdateWorkshopCredential(ctx, subject, payload, logger)
	}
	if s.deps.AgentIdentityAuthz == nil {
		logger.Info("link: no authorization client is wired into identityd, so every agent-owned credential update " +
			"is refused (fail-closed); this is a wiring bug, not a missing grant")
		return false, "no authorization client is wired into identityd"
	}
	canonical, err := subject.CanonicalUserID()
	if err != nil {
		logger.Info("link: cookie subject is not a user subject; refusing the agent-owned credential update",
			"subject", subject.String(), "err", err.Error())
		return false, "the signed-in subject is not a user subject"
	}
	ok, err := s.deps.AgentIdentityAuthz.CheckAgentIdentityUpdateCredential(ctx, t.Namespace, t.Name, canonical)
	if err != nil {
		logger.Info("link: update_credential check FAILED; refusing fail-closed (an authorization-service fault, not a denial)",
			"identity", t.String(), "subject", subject.String(), "err", err.Error())
		return false, "the permission check could not be completed"
	}
	if !ok {
		logger.Info("link: subject does not hold agentidentity#update_credential",
			"identity", t.String(), "subject", subject.String())
		return false, "the signed-in user does not hold agentidentity#update_credential"
	}
	return true, ""
}

// mayUpdateWorkshopCredential is mayUpdateAgentCredential's counterpart for
// PurposeWorkshopCredential. The authority here is NOT
// agentidentity#update_credential — that permission is editor +
// platform->can_admin, and `editor` ships unpopulated, so it resolves only
// for a platform administrator, which a legitimate workshop builder is not.
// The authority a builder actually holds is having STARTED the workshop that
// owns the target AgentIdentity's namespace — the SAME fact
// pkg/web/admind's handleAgentIdentityRefCredentialUpdate re-checks on the
// write (Task 6). The two MUST assert identical facts — bare canonical
// compared to spec.starterCanonical, both un-prefixed — or the UX gate admits
// a click the write then rejects, or (worse) the reverse.
//
// FAIL-CLOSED on every branch that is not an outright "yes": a non-user
// cookie subject, a malformed builder SessionRef, a Workshop the platform
// cannot read, or an empty/mismatched StarterCanonical. An empty
// StarterCanonical must never compare equal to an empty/absent canonical.
func (s *Server) mayUpdateWorkshopCredential(ctx context.Context, subject identity.Subject, payload passthroughlink.Payload, logger logr.Logger) (bool, string) {
	canonical, err := subject.CanonicalUserID()
	if err != nil {
		logger.Info("link: cookie subject is not a user subject; refusing the workshop credential update",
			"subject", subject.String(), "err", err.Error())
		return false, "the signed-in subject is not a user subject"
	}
	builderNS, builderName, ok := splitSessionRef(payload.SessionRef)
	if !ok {
		logger.Info("link: malformed builder sessionRef on a workshop-credential link; refusing",
			"sessionRef", payload.SessionRef)
		return false, "this link does not name a valid workshop session"
	}
	var ws spiceboxv1alpha1.Workshop
	key := client.ObjectKey{Namespace: builderNS, Name: spiceboxv1alpha1.WorkshopName(builderName)}
	if err := s.deps.K8s.Get(ctx, key, &ws); err != nil {
		logger.Info("link: workshop lookup failed; refusing the workshop-credential update fail-closed",
			"workshop", key.String(), "err", err.Error())
		return false, "the workshop for this link could not be found"
	}
	if ws.Spec.StarterCanonical == "" || canonical.String() != ws.Spec.StarterCanonical {
		logger.Info("link: subject did not start this workshop",
			"workshop", key.String(), "subject", subject.String())
		return false, "the signed-in user did not start this workshop"
	}
	return true, ""
}

// refuseAgentOwned writes the single fail-closed 403 every refusal on this path
// shares. The page text is deliberately uniform PER PURPOSE: distinguishing
// "you are not an admin" from "this link doesn't resolve" for an unauthorized
// visitor would leak which agent identities exist and who administers them.
// The CAUSE is logged, where an operator can read it.
//
// purpose picks WHICH uniform copy renders. PurposeCredentialUpdate's
// authority is a platform administrator; PurposeWorkshopCredential's is "the
// person who started this workshop" — a builder refused on the workshop path
// and told to "ask a platform administrator" would be pointed at the wrong
// person entirely, since a non-admin builder legitimately holds this
// authority. Every other purpose value falls back to the
// PurposeCredentialUpdate copy, matching this function's behavior before this
// purpose existed.
func (s *Server) refuseAgentOwned(w http.ResponseWriter, r *http.Request, purpose, why string, logger logr.Logger) {
	logger.Info("link: refusing an agent-owned credential update", "purpose", purpose, "reason", why)
	if purpose == passthroughlink.PurposeWorkshopCredential {
		s.writeError(w, r, http.StatusForbidden, "Not permitted",
			"This link connects a bot credential for an agent identity in a workshop. Only the person who started "+
				"that workshop can connect its credentials.")
		return
	}
	// The remedy names the ROLE, not the permission behind it. `update_credential`
	// resolves as `editor + platform->can_admin`, and `editor` ships unpopulated
	// with nothing in the product that writes it — so platform administrator is
	// the only standing that exists, and asking an operator to "grant
	// update_credential" would name a grant nobody can perform. It also keeps the
	// authorization model's vocabulary off a page shown to whoever holds the link.
	s.writeError(w, r, http.StatusForbidden, "Not permitted",
		"This link replaces a credential the agent itself uses, which only a platform administrator may do. "+
			"If you believe you should have access, ask a platform administrator.")
}

// failAgentOwnedResolve renders the outcome of a resolveAgentOwnedTarget error,
// which arrives in two shapes that must not read alike.
//
// An ErrAmbiguousOwner-wrapped error is a statement ABOUT THE REQUEST — the
// platform could not pin down which credential this link replaces — and refusing
// is the correct fail-closed answer. Anything else is the apiserver failing to
// answer at all, which says nothing about the visitor: rendering it as the
// refusal page tells an administrator they lack a permission they hold, sending
// them to chase a grant that was never missing while the real fault goes
// unreported. The refusal page is uniform for every genuine refusal, so an
// infrastructure fault routed into it is indistinguishable.
//
// The operator applies the same split to the same resolver on its own route.
// purpose is forwarded to refuseAgentOwned so the copy names the right
// authority — see its doc.
func (s *Server) failAgentOwnedResolve(w http.ResponseWriter, r *http.Request, purpose string, err error, logger logr.Logger) {
	if errors.Is(err, agentidentity.ErrAmbiguousOwner) {
		s.refuseAgentOwned(w, r, purpose, err.Error(), logger)
		return
	}
	logger.Info("link: could not resolve whose credential this link replaces; failing as a server error rather than a refusal",
		"err", err.Error())
	s.writeError(w, r, http.StatusInternalServerError, "Something went wrong",
		"We couldn't look up which credential this link replaces, so nothing was changed. "+
			"This is a problem on our side, not with your access — wait a moment and open the link again.")
}

// errNoCredentialWriter means no operator client is wired into identityd, so
// nothing in this process can perform an agent-owned credential write.
//
// A SENTINEL rather than an ad-hoc fmt.Errorf because it is the one failure on
// this path the reader can do nothing about — a deployment wiring fault, not a
// permission problem and not a conflict with the value typed. Callers MUST match
// on it: falling into the generic "please try again" copy is advice that can
// never succeed, and an admin retypes the credential forever while the real fix
// is an operator finishing the install.
var errNoCredentialWriter = errors.New("identityd has no operator client wired, so an agent's own shared credential cannot be replaced")

// submitAgentOwned hands the pasted value to the operator and returns the
// destination it wrote, or an error already mapped back to this package's typed
// refusals (agentidentity.Err*, errNoCredentialWriter), so the caller's error
// copy is identical whether the failure happened locally or across the hop.
//
// payload.Purpose picks WHICH target shape the request carries — exactly one
// of agentcred.Request's SessionRef / AgentIdentityRef, matching the two
// authorities admind's handler branches on. Getting this wrong sends the
// write down the WRONG branch: a workshop-credential submit that set
// SessionRef instead of AgentIdentityRef would resolve via
// ResolveRequestTarget against a CredentialUpdateRequest that link never
// has, refusing the write outright rather than silently landing it
// somewhere unintended — but refusing a legitimate builder is still the bug.
func (s *Server) submitAgentOwned(
	ctx context.Context, subject identity.Subject, payload passthroughlink.Payload, t *agentidentity.RequestTarget,
	credName, token string,
) (agentcred.Response, error) {
	if s.deps.AgentCredentialWriter == nil {
		// Nothing in identityd can perform this write, by design. Say so
		// explicitly rather than failing in a way that reads as a permission
		// problem — the fix is operator wiring, not a grant.
		return agentcred.Response{}, errNoCredentialWriter
	}
	req := agentcred.Request{Credential: credName, Token: token}
	if payload.Purpose == passthroughlink.PurposeWorkshopCredential {
		// The AgentIdentity lives directly in the workshop namespace W the
		// signed link named — no CredentialUpdateRequest behind it to resolve a
		// SessionRef through. t.Namespace/t.Name ARE W/name already:
		// resolveWorkshopCredentialTarget built them straight from the
		// payload's own AgentIdentityRef.
		req.AgentIdentityRef = &spiceboxv1alpha1.NamespacedRef{Namespace: t.Namespace, Name: t.Name}
	} else {
		ns, name, ok := splitSessionRef(payload.SessionRef)
		if !ok {
			return agentcred.Response{}, fmt.Errorf("%w: malformed sessionRef %q", agentidentity.ErrAmbiguousOwner, payload.SessionRef)
		}
		// The request names ONLY the session + credential (both of which rode
		// inside the signed link) and the value. The AgentIdentity, its Secret,
		// and the verdict are all the operator's to determine — see
		// pkg/web/admind/agentcred.
		req.SessionRef = namespacedRef(ns, name)
	}
	return s.deps.AgentCredentialWriter.Replace(ctx, subject, req)
}

// gateAgentOwned runs the shared click gate for BOTH /link and /link/submit:
// the OIDC cookie must prove a subject, and that subject must pass the UX
// permission gate. payload is forwarded to mayUpdateAgentCredential, which
// asks a DIFFERENT question depending on payload.Purpose — see its doc.
//
// noCookie is reported separately from a refusal because the two callers differ
// in what they do about it: the GET redirects the browser into sign-in, the
// POST cannot (there is no form to replay) and refuses.
func (s *Server) gateAgentOwned(r *http.Request, payload passthroughlink.Payload, t *agentidentity.RequestTarget, logger logr.Logger) (subject identity.Subject, noCookie bool, why string) {
	cookieSubject, cookieOK := s.checkOIDCCookie(r)
	if !cookieOK {
		return "", true, "no valid idd_session cookie"
	}
	if ok, reason := s.mayUpdateAgentCredential(r.Context(), cookieSubject, payload, t, logger); !ok {
		return cookieSubject, false, reason
	}
	return cookieSubject, false, ""
}

// handleAgentOwnedLinkGet renders the replacement form for an agent's own
// shared credential.
//
// Order is significant and matches handleLinkGet's own discipline: cookie
// (cheap, no I/O) → permission (one check) → replaceability (one Get). Checking
// replaceability last means an unauthorized visitor learns nothing about the
// identity's credential shape.
func (s *Server) handleAgentOwnedLinkGet(
	w http.ResponseWriter, r *http.Request, payload passthroughlink.Payload,
	t *agentidentity.RequestTarget, raw, d, sig string,
) {
	logger := log.FromContext(r.Context()).WithValues("identity", t.Namespace+"/"+t.Name, "credential", t.Credential)

	_, noCookie, why := s.gateAgentOwned(r, payload, t, logger)
	if noCookie {
		// Same treatment the user-owned path gives a cookie-less visitor:
		// delegate to /oidc/login, which runs the full sign-in chain and
		// returns the browser here with a cookie set. A 500 (or a bare 403)
		// would dead-end an admin who simply has not signed in yet.
		http.Redirect(w, r, "/oidc/login?d="+url.QueryEscape(d)+"&sig="+url.QueryEscape(sig), http.StatusFound)
		return
	}
	if why != "" {
		s.refuseAgentOwned(w, r, payload.Purpose, why, logger)
		return
	}

	// A workshop-credential link whose target is DECLARED type=oauth cannot go
	// through the paste-form path below: agentidentity.Resolve refuses
	// type=oauth with ErrNotReplaceable (its Secret is a multi-key bundle only
	// an OAuth ceremony can produce), and — more importantly — a type=oauth
	// credential must NEVER be routed through /link/oauth/<cred>, which links
	// the VISITOR's OWN account. So this peek forks BEFORE calling Resolve at
	// all: PurposeCredentialUpdate (the platform-admin path) is unaffected —
	// every agent-owned credential update outside a workshop is still
	// pasteable — and a workshop-credential link whose type is static/pat
	// falls straight through to the unchanged paste-form path below.
	var rows []linkMenuRow
	if payload.Purpose == passthroughlink.PurposeWorkshopCredential {
		isOAuth, err := s.agentCredentialIsOAuth(r.Context(), t.Namespace, t.Name, t.Credential)
		if err != nil {
			logger.Info("link: could not read the AgentIdentity to determine the credential's declared type", "err", err.Error())
			s.writeError(w, r, http.StatusInternalServerError, "Something went wrong",
				"We couldn't look up this credential, so nothing was changed. Wait a moment and open the link again.")
			return
		}
		if isOAuth {
			rows = []linkMenuRow{s.agentOAuthConnectMenuRow(r.Context(), t, d, sig, logger)}
		}
	}

	if rows == nil {
		// A read of the AgentIdentity CR (secret REFERENCES only — no secret
		// material, and identityd cannot read the Secret itself). Its purpose is
		// to refuse an unreplaceable credential BEFORE the admin types a live
		// token into a form the operator would reject.
		if _, err := agentidentity.Resolve(r.Context(), s.deps.K8s, t.Namespace, t.Name, t.Credential); err != nil {
			logger.Info("link: agent-owned credential is not replaceable by pasting a value", "err", err.Error())
			s.writeError(w, r, http.StatusConflict, "This credential can't be replaced here",
				agentOwnedResolveMessage(err))
			return
		}
		rows = []linkMenuRow{s.agentOwnedMenuRow(r.Context(), t, logger)}
	}

	if err := s.renderApp(r, w, "identity-link", linkPageData{
		SessionRef: payload.SessionRef,
		What: []string{fmt.Sprintf("Replace the %q credential that %s %s uses. This credential is SHARED: "+
			"every session running this agent authenticates with it.",
			t.Credential, spiceboxIdentityKindAgent, t.Name)},
		SignedLink: raw,
		Rows:       rows,
	}, webui.PageMeta{Title: "Replace a shared agent credential"}); err != nil {
		logger.Info("link: app render failed", "err", err.Error())
	}
}

// agentOwnedMenuRow builds the single row the agent-owned page renders.
//
// Status is pinned to "missing" — the form only renders for a "missing" row
// (pkg/platform/identityd/ui/link/LinkMenu.tsx), and the platform has independently
// verified this credential no longer authenticates, so "needs replacing" is
// both the honest badge and the only one that lets the admin act. Critically it
// is NOT derived from the VISITOR's own UserIdentity the way the user-owned
// menu does it: an admin who happens to hold a personal credential of the same
// name would otherwise see a "linked" badge and NO form at all.
//
// Kind is pinned to "pat" for the same reason. The OAuth variant of a row
// points at /link/oauth/<cred>, which links the VISITOR's own account — the
// exact wrong-destination write this file exists to prevent. An agent-owned
// credential that is genuinely type=oauth never reaches here: for
// PurposeCredentialUpdate, the caller's agentidentity.Resolve refuses it
// first; for PurposeWorkshopCredential, handleAgentOwnedLinkGet's own type
// peek forks to agentOAuthConnectMenuRow before this function is ever
// called.
func (s *Server) agentOwnedMenuRow(ctx context.Context, t *agentidentity.RequestTarget, logger logr.Logger) linkMenuRow {
	label := portalCredentialLabel(ctx, s.deps.K8s, t.Credential, logger)
	row := linkMenuRow{
		CredentialName: t.Credential,
		Label:          label,
		Kind:           "pat",
		Status:         "missing",
		IconURL:        "/icon/" + t.Credential,
		Why: fmt.Sprintf("The platform verified this credential no longer works. It belongs to %s %s/%s and is shared "+
			"by every session running that agent — replacing it here replaces it for all of them.",
			spiceboxIdentityKindAgent, t.Namespace, t.Name),
	}
	row.Instructions, row.DocsURL = credInstructions(t.Credential)
	return row
}

// agentCredentialIsOAuth peeks the DECLARED type of an AgentIdentity's
// credential — the same Get + linear scan agentidentity.Resolve performs —
// WITHOUT calling Resolve itself, which throws ErrNotReplaceable for
// type=oauth. The peek exists so handleAgentOwnedLinkGet can fork to the
// Connect page for a type=oauth workshop credential BEFORE hitting the error
// Resolve would otherwise produce.
//
// Returns an error only for an infrastructure fault (the Get failing) or a
// credential the AgentIdentity no longer declares — both handled by the
// caller as a fail-closed server error, never as "assume static".
func (s *Server) agentCredentialIsOAuth(ctx context.Context, ns, name, credName string) (bool, error) {
	var ai spiceboxv1alpha1.AgentIdentity
	if err := s.deps.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &ai); err != nil {
		return false, fmt.Errorf("get AgentIdentity %s/%s: %w", ns, name, err)
	}
	for i := range ai.Spec.Credentials {
		if ai.Spec.Credentials[i].Name == credName {
			// The OAuth block's presence IS the type signal — the CRD discriminator
			// makes cred.OAuth non-nil iff type=oauth. Check the block rather than
			// comparing the type string (even a named constant), keeping
			// credential-type dispatch out of this package; the credkind registry
			// owns type strings. Same idiom as agentidentity.PutOAuthToken.
			return ai.Spec.Credentials[i].OAuth != nil, nil
		}
	}
	return false, fmt.Errorf("credential %q not declared on AgentIdentity %s/%s", credName, ns, name)
}

// agentOAuthConnectMenuRow builds the single row a workshop credential's
// Connect page renders for a type=oauth target. Unlike agentOwnedMenuRow
// (Kind "pat", pointing nowhere but the paste form), its OAuthURL points at
// THIS PACKAGE's own agent authorize entry — /link/agent-oauth/<cred> — never
// the per-user /link/oauth/<cred>, which would begin the OAuth dance under
// the VISITOR's own account rather than the workshop's AgentIdentity.
//
// The query carries the SAME d/sig this card-link page itself was opened
// with: the authorize step re-verifies that signed payload and re-runs the
// starter gate on its own, so nothing here needs to be trusted beyond "this
// is the credential the card named". No DCR or provider I/O happens at
// render time — that starts only once the starter clicks through to
// handleLinkAgentOAuthGet.
func (s *Server) agentOAuthConnectMenuRow(ctx context.Context, t *agentidentity.RequestTarget, d, sig string, logger logr.Logger) linkMenuRow {
	label := portalCredentialLabel(ctx, s.deps.K8s, t.Credential, logger)
	row := linkMenuRow{
		CredentialName: t.Credential,
		Label:          label,
		Kind:           "oauth",
		Status:         "missing",
		IconURL:        "/icon/" + t.Credential,
		OAuthURL:       "/link/agent-oauth/" + url.PathEscape(t.Credential) + "?d=" + url.QueryEscape(d) + "&sig=" + url.QueryEscape(sig),
		Why: fmt.Sprintf("The platform verified this credential no longer works. It belongs to %s %s/%s and is shared "+
			"by every session running that agent — connecting it here replaces it for all of them.",
			spiceboxIdentityKindAgent, t.Namespace, t.Name),
	}
	row.Instructions, row.DocsURL = credInstructions(t.Credential)
	return row
}

// agentOwnedResolveMessage maps agentidentity refusals to the sentence the human
// reads. Each one names a DIFFERENT next action, which is the whole reason they
// are distinct errors rather than one "couldn't save" — and the reason the
// operator returns a machine-readable code the client maps back to these
// sentinels rather than a bare status.
func agentOwnedResolveMessage(err error) string {
	switch {
	case errors.Is(err, errNoCredentialWriter):
		return "Replacing a credential the agent itself uses isn't available on this deployment. Nothing was saved, " +
			"and retrying won't change that — ask your platform administrator to finish setting this up."
	case errors.Is(err, agentidentity.ErrNotReplaceable):
		return "This credential isn't a pasted value — it's issued by an OAuth or federated flow, so it has to be " +
			"re-authorized by an operator rather than typed in here."
	case errors.Is(err, agentidentity.ErrValueUnchanged):
		return "That's the value that's already stored, and the platform verified it no longer works. " +
			"Paste the NEW credential."
	case errors.Is(err, agentidentity.ErrSecretRefMismatch):
		return "This agent identity now points at a different Secret than the one this request was opened against, " +
			"so the update was refused rather than written somewhere nothing is watching. Ask the agent to try again."
	case errors.Is(err, agentidentity.ErrIdentityNotFound), errors.Is(err, agentidentity.ErrCredentialMissing):
		return "The agent identity this link names no longer declares this credential, so there is nothing to replace."
	case errors.Is(err, agentidentity.ErrSecretMissing):
		return "The Secret backing this credential doesn't exist. An operator needs to recreate it before it can be replaced."
	case errors.Is(err, agentidentity.ErrAmbiguousOwner):
		return "The platform couldn't confirm which credential this link replaces, so it refused rather than risk " +
			"writing the wrong one. Ask your operator to check the credential-update request."
	default:
		return "The credential couldn't be saved. Please try again, or contact your administrator."
	}
}

// spiceboxIdentityKindAgent is the AgentIdentity Kind spelling, used only in
// the human-facing copy above. Aliased from the API package so the page text
// and the routing predicate name the same thing.
const spiceboxIdentityKindAgent = spiceboxv1alpha1.IdentityKindAgentIdentity

// namespacedRef is a tiny constructor keeping the wire-DTO build readable at
// the call site.
func namespacedRef(ns, name string) spiceboxv1alpha1.NamespacedRef {
	return spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: name}
}
