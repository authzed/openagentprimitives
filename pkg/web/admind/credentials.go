// credentials.go — POST /admin/v1/credentials/agent-update: replacing an
// agent's OWN shared credential.
//
// This is the ONLY admind route whose permission is per-RESOURCE
// (agentidentity#update_credential on one AgentIdentity) rather than
// platform-wide, and that difference drives its whole shape. `require` cannot
// gate it: the resource is not knowable until the body is decoded AND the
// operator has resolved the session's CredentialUpdateRequest, so the check has
// to happen inside the handler, after that resolution.
//
// # This handler is the security gate
//
// webd (identityd) runs its own cookie + permission gate before rendering the
// form. That is a UX gate — it stops an unauthorized visitor typing a token
// into a page that will refuse it. It is NOT what authorizes the write, and
// this handler must behave identically whether or not it ran. Concretely:
//
//   - The subject arrives as an ASSERTION in X-Admin-Subject. webd owns the
//     idd_session cookie, so it is the right component to say which subject is
//     signed in — and the wrong component to decide whether that subject may
//     write.
//   - There is deliberately NO "already authorized" field on the request, and
//     if one were added it must not be read. Trusting a caller's verdict would
//     make the browser-facing pod the authorization authority, which is the
//     precise thing routing this through the operator exists to prevent.
//   - The AgentIdentity, its namespace, and its backing Secret are resolved
//     HERE from the CredentialUpdateRequest's own status — never taken from the
//     request body — so a caller cannot aim the write at an identity the
//     session's request never named.
package admind

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/agentidentity"
	"github.com/authzed/openagentprimitives/pkg/web/admind/agentcred"
)

// requireProvenSubject enforces the bearer service token and extracts the
// forwarded subject, then hands it to h. It performs NO permission check.
//
// It exists for exactly one route — the per-resource one below — and h MUST
// perform its own authorization. The subject is passed as an argument rather
// than left in a header precisely so a handler cannot silently forget to
// establish who is asking; what it still owes is the check.
func (a *Admind) requireProvenSubject(h func(http.ResponseWriter, *http.Request, identity.CanonicalUserID)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" || subtleCompare(tok, a.cfg.Token) != 1 {
			a.cfg.Logger.Info("admind: request with missing/wrong service token — check the spicebox-admind-token Secret mounts",
				"path", r.URL.Path)
			writeJSONError(w, http.StatusUnauthorized, "missing or invalid admind service token")
			return
		}
		subject := r.Header.Get(agentcred.SubjectHeader)
		canonical, err := identity.Subject(subject).CanonicalUserID()
		if err != nil || canonical.IsZero() {
			writeJSONError(w, http.StatusUnauthorized, "missing "+agentcred.SubjectHeader+" (want \"user:<canonical>\")")
			return
		}
		h(w, r, canonical)
	})
}

// handleAgentCredentialUpdate authorizes and performs the replacement.
//
// Order is the point: resolve the target, THEN check the permission on the
// resolved target, THEN write. Checking before resolving would mean checking
// against something the caller named.
func (a *Admind) handleAgentCredentialUpdate(w http.ResponseWriter, r *http.Request, canonical identity.CanonicalUserID) {
	defer r.Body.Close()
	var req agentcred.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "decode request: "+err.Error())
		return
	}

	// Exactly one of the two target shapes — never both (an ambiguous target
	// with two different authorities behind it), never neither (nothing to
	// authorize against at all).
	sessionRefSet := req.SessionRef.Namespace != "" || req.SessionRef.Name != ""
	if sessionRefSet == (req.AgentIdentityRef != nil) {
		writeJSONError(w, http.StatusBadRequest, "exactly one of sessionRef or agentIdentityRef is required")
		return
	}

	if req.AgentIdentityRef != nil {
		a.handleAgentIdentityRefCredentialUpdate(w, r, canonical, req)
		return
	}

	// --- Everything below is the existing SessionRef path, unchanged. ---

	if req.SessionRef.Namespace == "" || req.SessionRef.Name == "" || req.Credential == "" || req.Token == "" {
		writeJSONError(w, http.StatusBadRequest, "sessionRef, credential, and token are all required")
		return
	}

	// The operator's OWN resolution of what it is being asked to touch. The
	// request named a session and a credential (both of which rode inside the
	// caller's HMAC-signed deep-link); everything below — which AgentIdentity,
	// which namespace, which Secret — comes from the CredentialUpdateRequest
	// the operator itself wrote.
	target, err := agentidentity.ResolveRequestTarget(r.Context(), a.cfg.K8s,
		req.SessionRef.Namespace, req.SessionRef.Name, []string{req.Credential})
	if err != nil {
		if errors.Is(err, agentidentity.ErrAmbiguousOwner) {
			a.cfg.Logger.Info("admind: agent credential update refused — ambiguous owner",
				"session", req.SessionRef.Namespace+"/"+req.SessionRef.Name, "credential", req.Credential, "err", err.Error())
			writeJSONErrorCode(w, http.StatusForbidden, agentidentity.CodeAmbiguousOwner,
				"this request does not resolve to a single credential owner")
			return
		}
		a.cfg.Logger.Info("admind: resolving the credential-update target failed",
			"session", req.SessionRef.Namespace+"/"+req.SessionRef.Name, "credential", req.Credential, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "could not resolve the credential-update request; see operator logs")
		return
	}
	if target == nil {
		// A user-owned credential does not belong on this route: its value goes
		// to the person's OWN UserIdentity master Secret, which the calling
		// component writes itself. Refusing keeps this endpoint incapable of
		// touching anything but an AgentIdentity.
		a.cfg.Logger.Info("admind: agent credential update refused — not an agent-owned credential",
			"session", req.SessionRef.Namespace+"/"+req.SessionRef.Name, "credential", req.Credential)
		writeJSONErrorCode(w, http.StatusForbidden, "",
			"this credential is not an agent's own shared credential")
		return
	}

	// THE authorization decision. FullyConsistent by construction — the
	// interface takes no consistency parameter; see
	// spicedb.Client.CheckAgentIdentityUpdateCredential for why a stale read
	// here is indistinguishable from "not permitted" and would silently refuse
	// a legitimate admin. An ERROR is a fault, never a grant and never a
	// denial: 500, so the caller retries rather than telling the human they
	// lack a permission they may well hold.
	ok, err := a.cfg.Checker.CheckAgentIdentityUpdateCredential(r.Context(), target.Namespace, target.Name, canonical)
	if err != nil {
		a.cfg.Logger.Info("admind: agentidentity#update_credential check errored; refusing (fault, not a denial)",
			"identity", target.String(), "subject", canonical.String(), "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "authorization check failed; see operator logs")
		return
	}
	if !ok {
		a.cfg.Logger.Info("admind: agentidentity#update_credential denied",
			"identity", target.String(), "subject", canonical.String())
		writeJSONErrorCode(w, http.StatusForbidden, "",
			"subject lacks agentidentity#update_credential on "+target.String())
		return
	}

	written, err := agentidentity.PutToken(r.Context(), a.cfg.K8s, agentidentity.PutTokenRequest{
		Namespace:      target.Namespace,
		Name:           target.Name,
		CredentialName: target.Credential,
		Token:          req.Token,
		ExpectSecret:   target.SecretRef,
	})
	if err != nil {
		code := agentidentity.CodeFor(err)
		status := http.StatusConflict
		if code == "" {
			status = http.StatusInternalServerError
		}
		a.cfg.Logger.Info("admind: agent credential write failed",
			"identity", target.String(), "subject", canonical.String(), "code", code, "err", err.Error())
		writeJSONErrorCode(w, status, code, err.Error())
		return
	}

	a.cfg.Logger.Info("admind: replaced an agent's own shared credential",
		"identity", target.String(), "subject", canonical.String(),
		"secret", written.SecretRef.Namespace+"/"+written.SecretRef.Name)
	writeJSON(w, http.StatusOK, agentcred.Response{SecretRef: written.SecretRef})
}

// handleAgentIdentityRefCredentialUpdate authorizes and performs the
// AgentIdentityRef branch of handleAgentCredentialUpdate: a direct write to
// an AgentIdentity's credential with no CredentialUpdateRequest behind it —
// the workshop bot-credential flow (plan 5a).
//
// THIS IS NOT agentidentity#update_credential. That permission is
// editor + platform->can_admin, and `editor` ships unpopulated while every
// AgentIdentity links to #platform regardless of namespace, so it resolves
// ONLY for platform admins — a legitimate non-admin builder would be
// rejected. The authority a builder actually holds is that they started the
// workshop the target namespace belongs to, so that is what this branch
// checks — and it re-derives that workshop from req.AgentIdentityRef.Namespace
// itself, on the cluster, rather than trusting anything the caller supplied.
// Skipping that re-derivation is exactly how workshop-A's starter could aim a
// write at workshop-B's (or a production) AgentIdentity.
func (a *Admind) handleAgentIdentityRefCredentialUpdate(w http.ResponseWriter, r *http.Request, canonical identity.CanonicalUserID, req agentcred.Request) {
	ref := req.AgentIdentityRef
	// Deliberately NOT req.Token here: an OAuth request has Token=="" and
	// carries req.OAuth instead. The exactly-one(Token, OAuth) check happens
	// at the terminal write below, after the starter gate — see this
	// function's doc.
	if ref.Namespace == "" || ref.Name == "" || req.Credential == "" {
		writeJSONError(w, http.StatusBadRequest, "agentIdentityRef (namespace and name) and credential are all required")
		return
	}
	ctx := r.Context()

	if a.cfg.K8s == nil {
		// A nil client here would panic on the very first Get below. Refuse
		// explicitly instead of trusting the interface to be non-nil — see
		// CLAUDE.md's typed-nil rule; a.cfg.K8s is a real client.Client
		// interface field, so this compares true nil rather than a typed-nil
		// wrapper, but a fault here must still never silently pass through.
		a.cfg.Logger.Info("admind: agent-identity credential update refused — no cluster client configured",
			"agentIdentity", ref.Namespace+"/"+ref.Name)
		writeJSONError(w, http.StatusInternalServerError, "cluster client is not configured; see operator logs")
		return
	}

	// Re-derive the workshop that owns ref.Namespace. The target namespace
	// itself must carry the session-attribution labels the AgentSession
	// reconciler stamps on every workshop namespace it provisions.
	var wsNs corev1.Namespace
	if err := a.cfg.K8s.Get(ctx, client.ObjectKey{Name: ref.Namespace}, &wsNs); err != nil {
		a.cfg.Logger.Info("admind: agent-identity credential update refused — target namespace not found",
			"namespace", ref.Namespace, "agentIdentity", ref.Namespace+"/"+ref.Name, "err", err.Error())
		writeJSONErrorCode(w, http.StatusForbidden, "", "target namespace does not resolve to a workshop")
		return
	}
	sessNS := wsNs.Labels[spiceboxv1alpha1.LabelWorkshopSessionNamespace]
	sessName := wsNs.Labels[spiceboxv1alpha1.LabelWorkshopSessionName]
	if sessNS == "" || sessName == "" {
		a.cfg.Logger.Info("admind: agent-identity credential update refused — target namespace carries no workshop-session labels",
			"namespace", ref.Namespace)
		writeJSONErrorCode(w, http.StatusForbidden, "", "target namespace is not a workshop namespace")
		return
	}

	// ...and the Workshop CR that labeled session names must, in turn, claim
	// ref.Namespace as its OWN provisioned namespace. This is the check that
	// stops a forged/stale label from aiming the write at a namespace some
	// other Workshop actually owns.
	var ws spiceboxv1alpha1.Workshop
	wsKey := client.ObjectKey{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}
	if err := a.cfg.K8s.Get(ctx, wsKey, &ws); err != nil {
		a.cfg.Logger.Info("admind: agent-identity credential update refused — labeled session's Workshop not found",
			"namespace", ref.Namespace, "workshop", wsKey.String(), "err", err.Error())
		writeJSONErrorCode(w, http.StatusForbidden, "", "target namespace does not resolve to a workshop")
		return
	}
	if ws.Status.Namespace != ref.Namespace {
		a.cfg.Logger.Info("admind: agent-identity credential update refused — Workshop does not claim the target namespace as its own",
			"namespace", ref.Namespace, "workshop", wsKey.String(), "workshopStatusNamespace", ws.Status.Namespace)
		writeJSONErrorCode(w, http.StatusForbidden, "", "target namespace does not belong to this workshop")
		return
	}

	// THE authorization decision: the caller must be the person who started
	// THIS workshop. Both sides are the bare canonical id (no "user:" prefix):
	// identity.CanonicalUserID is documented as prefix-stripped, and
	// spec.starterCanonical is written the same way (workshop_hook.go stores
	// canonical.String() verbatim; teardown.go re-adds "user:" only when
	// building a Subject to notify). An empty starter must never compare
	// equal to an empty/absent canonical — fail closed on both.
	if ws.Spec.StarterCanonical == "" || canonical.IsZero() || canonical.String() != ws.Spec.StarterCanonical {
		a.cfg.Logger.Info("admind: agent-identity credential update denied — caller did not start this workshop",
			"namespace", ref.Namespace, "workshop", wsKey.String(), "subject", canonical.String())
		writeJSONErrorCode(w, http.StatusForbidden, "", "subject did not start this workshop")
		return
	}

	// Resolve + write, via the SAME Secret-write PutToken/PutOAuthToken use
	// for the SessionRef branch above — no ExpectSecret, because there is no
	// CredentialUpdateRequest here to have recorded one.
	//
	// Exactly one of req.Token / req.OAuth selects which write path runs —
	// and, transitively, which credential TYPE this call may touch: PutToken
	// writes a single pasteable value, PutOAuthToken writes a multi-key OAuth
	// bundle onto a pre-declared type=oauth credential. Each refuses the
	// other credential type on its own (PutOAuthToken's ErrNotOAuthCredential
	// check), so a type mismatch fails closed at the write, not here.
	var secretRef spiceboxv1alpha1.NamespacedRef
	switch {
	case req.OAuth != nil && req.Token == "":
		written, err := agentidentity.PutOAuthToken(ctx, a.cfg.K8s, agentidentity.PutOAuthTokenRequest{
			Namespace:      ref.Namespace,
			Name:           ref.Name,
			CredentialName: req.Credential,
			AccessToken:    req.OAuth.AccessToken,
			RefreshToken:   req.OAuth.RefreshToken,
			TokenType:      req.OAuth.TokenType,
			Scope:          req.OAuth.Scope,
			TokenEndpoint:  req.OAuth.TokenEndpoint,
			ClientID:       req.OAuth.ClientID,
			ClientSecret:   req.OAuth.ClientSecret,
			ExpiresAt:      req.OAuth.ExpiresAt,
		})
		if err != nil {
			code := agentidentity.CodeFor(err)
			status := http.StatusConflict
			if code == "" {
				status = http.StatusInternalServerError
			}
			a.cfg.Logger.Info("admind: workshop agent-identity oauth credential write failed",
				"identity", ref.Namespace+"/"+ref.Name, "workshop", wsKey.String(), "subject", canonical.String(),
				"code", code, "err", err.Error())
			writeJSONErrorCode(w, status, code, err.Error())
			return
		}
		secretRef = written
	case req.Token != "" && req.OAuth == nil:
		written, err := agentidentity.PutToken(ctx, a.cfg.K8s, agentidentity.PutTokenRequest{
			Namespace:      ref.Namespace,
			Name:           ref.Name,
			CredentialName: req.Credential,
			Token:          req.Token,
		})
		if err != nil {
			code := agentidentity.CodeFor(err)
			status := http.StatusConflict
			if code == "" {
				status = http.StatusInternalServerError
			}
			a.cfg.Logger.Info("admind: workshop agent-identity credential write failed",
				"identity", ref.Namespace+"/"+ref.Name, "workshop", wsKey.String(), "subject", canonical.String(),
				"code", code, "err", err.Error())
			writeJSONErrorCode(w, status, code, err.Error())
			return
		}
		secretRef = written.SecretRef
	default:
		writeJSONError(w, http.StatusBadRequest, "exactly one of token or oauth is required")
		return
	}

	a.cfg.Logger.Info("admind: replaced a workshop AgentIdentity's shared credential",
		"identity", ref.Namespace+"/"+ref.Name, "workshop", wsKey.String(), "subject", canonical.String(),
		"secret", secretRef.Namespace+"/"+secretRef.Name)
	writeJSON(w, http.StatusOK, agentcred.Response{SecretRef: secretRef})
}

// writeJSONErrorCode is writeJSONError plus a machine-readable code the caller
// maps back to a typed error (agentidentity.ErrorForCode). Each code names a
// different next action for the human on the other side of the browser; a bare
// status line collapses them all to "it didn't work".
func writeJSONErrorCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, agentcred.ErrorBody{Error: msg, Code: code})
}

// subtleCompare is crypto/subtle.ConstantTimeCompare over strings, matching the
// service-token comparison `require` performs. Extracted so both middlewares
// compare the token exactly one way.
func subtleCompare(a, b string) int {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b))
}
