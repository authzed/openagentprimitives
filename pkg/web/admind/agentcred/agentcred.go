// Package agentcred is the wire contract for replacing an agent's OWN shared
// credential, plus the client half of it.
//
// # Why this crosses a process boundary at all
//
// The human doing the replacing is in a browser, and the browser-facing
// component is webd (which hosts identityd's link handlers). The value being
// replaced lives in a Secret in the AgentIdentity's own namespace. Letting webd
// write it would mean granting the internet-facing pod cluster-wide Secret
// access — standing RBAC that outlives any single request and, on compromise,
// reads and overwrites the SpiceDB preshared key, the passthroughlink HMAC key,
// and every stored credential in the cluster. So webd holds NO Secret access at
// all; the write happens in the operator, which already has it.
//
// # The trust boundary, precisely
//
// webd owns the idd_session cookie, so it is the right component to assert
// WHICH SUBJECT is signed in. It is NOT the right component to decide whether
// that subject may write. This request therefore carries:
//
//   - the proven subject, in the X-Admin-Subject header (the same header and
//     the same shape the admin UI's proxy already uses);
//   - the session + credential the signed deep-link covered (or, for the
//     workshop flow below, the AgentIdentity + credential directly);
//   - the pasted value.
//
// It deliberately carries NO Secret reference and NO "already authorized"
// flag — the backing Secret is always resolved operator-side, and there is no
// field a caller could set to skip the check. The operator re-resolves the
// target and runs its own FullyConsistent authorization check on the asserted
// subject before writing. A compromised webd can therefore assert a subject,
// but cannot make the operator write for a subject that does not hold the
// permission, and cannot aim the write at an identity the request never named.
//
// Two target shapes exist, each with its own authority (Request.SessionRef
// vs. Request.AgentIdentityRef doc below): SessionRef resolves via a
// CredentialUpdateRequest and is authorized by the per-resource
// agentidentity#update_credential; AgentIdentityRef names an AgentIdentity
// directly and is authorized by the caller having started the workshop that
// owns its namespace. Exactly one of the two is set on any request.
//
// This mirrors pkg/web/admind's existing two-factor shape (service token + a
// webd-forwarded subject the operator independently checks); the only
// difference is that the permission here is per-RESOURCE rather than
// platform-wide, so the check runs in the handler, after the operator has
// resolved which resource — and which authority — applies.
package agentcred

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/agentidentity"
)

// ErrRefused is returned when the operator REFUSED the replacement (401/403)
// without a more specific code — the subject does not hold
// agentidentity#update_credential, or the caller could not authenticate.
//
// It is distinguished from the coded failures below because the caller's
// response differs in kind: a refusal is "you may not do this" (render the
// permission page), while a coded failure is "this cannot be done this way"
// (render the instruction that names the actual next step). Collapsing them
// would tell a denied visitor to go find a different credential value.
var ErrRefused = errors.New("agentcred: the operator refused the credential replacement")

// Path is the operator-side route. Registered by pkg/web/admind, called by the
// Client below — declared once here so the two can never disagree.
const Path = "/admin/v1/credentials/agent-update"

// SubjectHeader carries the subject webd proved via the idd_session cookie.
// Same header pkg/web/adminui's proxy sets for the admin console, so the operator
// reads a forwarded subject exactly one way.
const SubjectHeader = "X-Admin-Subject"

// Request is the POST body.
//
// Note what is ABSENT: no Secret ref, no authorization verdict. Both are the
// operator's to determine — see the package doc. Exactly one of SessionRef /
// AgentIdentityRef is set; each carries a DIFFERENT authorization authority
// (see AgentIdentityRef's doc below).
type Request struct {
	// SessionRef is the AgentSession the signed deep-link covered.
	SessionRef spiceboxv1alpha1.NamespacedRef `json:"sessionRef"`
	// AgentIdentityRef, when set, targets an AgentIdentity directly (by
	// namespace+name) rather than resolving a CredentialUpdateRequest from
	// SessionRef. Used by the workshop bot-credential flow (plan 5a), whose
	// target is an AgentIdentity in the workshop namespace W with no
	// CredentialUpdateRequest at all. Exactly one of SessionRef /
	// AgentIdentityRef is set.
	//
	// The two carry DIFFERENT authorities. SessionRef is authorized by
	// agentidentity#update_credential — editor + platform->can_admin — which
	// resolves only for platform admins today. AgentIdentityRef is authorized
	// by the caller having STARTED the workshop the target namespace belongs
	// to (re-derived by the operator from ref.Namespace itself, never trusted
	// from this field), which is the authority a non-admin builder actually
	// holds. Do not read this as a weaker check than SessionRef's — it is a
	// different one, scoped to the one namespace a builder session owns.
	// +optional
	AgentIdentityRef *spiceboxv1alpha1.NamespacedRef `json:"agentIdentityRef,omitempty"`
	// Credential is the credential name the link covered.
	Credential string `json:"credential"`
	// Token is the replacement value for a type=static-style pasteable
	// credential, written via agentidentity.PutToken. Exactly one of Token /
	// OAuth is set.
	Token string `json:"token"`
	// OAuth is the exchanged OAuth token bundle for a type=oauth credential,
	// written via agentidentity.PutOAuthToken instead of PutToken. Exactly
	// one of Token / OAuth is set. Only the AgentIdentityRef branch (the
	// workshop bot-credential flow, plan 5a) accepts this today — the
	// SessionRef branch's credentials are all pasteable static values.
	// +optional
	OAuth *OAuthBundle `json:"oauth,omitempty"`
}

// OAuthBundle is the exchanged OAuth token bundle for a type=oauth
// credential. Its fields mirror agentidentity.PutOAuthTokenRequest one for
// one so the handler passes them straight through without renaming.
type OAuthBundle struct {
	// AccessToken is the current OAuth access token. Required.
	AccessToken string `json:"accessToken"`
	// RefreshToken is the long-lived refresh token, when the provider issued
	// one. Requires TokenEndpoint be set.
	// +optional
	RefreshToken string `json:"refreshToken,omitempty"`
	// TokenType is typically "Bearer".
	// +optional
	TokenType string `json:"tokenType,omitempty"`
	// Scope is the space-separated granted scopes string.
	// +optional
	Scope string `json:"scope,omitempty"`
	// TokenEndpoint is the provider's RFC 6749 token endpoint. Required
	// whenever RefreshToken is set — it is the only durable home for the
	// endpoint refresh.Run reads at redemption time.
	// +optional
	TokenEndpoint string `json:"tokenEndpoint,omitempty"`
	// ClientID is the OAuth client the tokens were issued to.
	// +optional
	ClientID string `json:"clientId,omitempty"`
	// ClientSecret is the client's secret, for confidential clients only.
	// +optional
	ClientSecret string `json:"clientSecret,omitempty"`
	// ExpiresAt is the expiry as Unix epoch seconds. Zero means "unknown /
	// never expires".
	// +optional
	ExpiresAt int64 `json:"expiresAt,omitempty"`
}

// Response is the 200 body: where the operator actually wrote, echoed back so
// the caller can log the destination it never got to choose.
type Response struct {
	SecretRef spiceboxv1alpha1.NamespacedRef `json:"secretRef"`
}

// ErrorBody is the non-2xx body. Code is one of agentidentity's wire codes (or
// empty for a fault with no specific instruction for the human).
type ErrorBody struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// Client calls the operator's agent-credential-update endpoint.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a Client for the operator at baseURL, authenticating with the
// admind service token. Both are required; a Client built with either empty
// refuses to send rather than issuing an unauthenticated write attempt.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Replace submits req on behalf of subject and returns where the operator
// wrote. subject is an ASSERTION, not an authorization: the operator checks it.
//
// Refusals come back as the corresponding agentidentity sentinel error (via
// agentidentity.ErrorForCode), so a caller branches with errors.Is exactly as
// it would against an in-process write — which is what lets the browser-side
// error copy stay a single switch, unaware that a network hop happened.
func (c *Client) Replace(ctx context.Context, subject identity.Subject, req Request) (Response, error) {
	if c.baseURL == "" || c.token == "" {
		return Response{}, fmt.Errorf("agentcred: client is not configured (operator URL or service token missing)")
	}
	if subject == "" {
		return Response{}, fmt.Errorf("agentcred: a proven subject is required")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("agentcred: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+Path, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("agentcred: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpReq.Header.Set(SubjectHeader, subject.String())

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("agentcred: call operator: %w", err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		var eb ErrorBody
		// A body that does not decode is not a reason to lose the status: fall
		// back to the raw bytes so an operator reading webd's log still sees
		// what the operator said.
		if readErr != nil || json.Unmarshal(raw, &eb) != nil || eb.Error == "" {
			eb = ErrorBody{Error: fmt.Sprintf("operator returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))}
		}
		if eb.Code == "" && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized) {
			return Response{}, fmt.Errorf("%w: %s", ErrRefused, eb.Error)
		}
		return Response{}, agentidentity.ErrorForCode(eb.Code, eb.Error)
	}
	if readErr != nil {
		return Response{}, fmt.Errorf("agentcred: read response: %w", readErr)
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, fmt.Errorf("agentcred: decode response: %w", err)
	}
	return out, nil
}
