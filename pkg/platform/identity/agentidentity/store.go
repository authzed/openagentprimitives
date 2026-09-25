// Package agentidentity persists a replacement value for a credential an
// AgentIdentity owns — the agent's OWN shared secret, as distinct from the
// per-person credentials pkg/platform/identity/useridentity writes.
//
// The two are NOT interchangeable. useridentity.PutToken always writes the
// SUBMITTING SUBJECT's master Secret in the identities namespace; an
// AgentIdentity's credential lives in a Secret the AgentIdentity references, in
// its own namespace, under a key the identity chooses. Routing an agent-owned
// update through PutToken silently writes the admin's personal credential,
// leaves the shared one dead, and reports success.
//
// # Everything here fails closed
//
// A replacement is written ONLY when the identity, the credential, its type, its
// Secret, and (when supplied) the Secret recorded as this credential's backing
// store ALL line up. Every other outcome is a typed error, never a best-effort
// write elsewhere: a write that lands on the wrong object is worse than no write
// at all, because the human walks away believing the credential is fixed.
package agentidentity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
)

// ErrIdentityNotFound is returned when the named AgentIdentity does not exist.
var ErrIdentityNotFound = errors.New("agentidentity: AgentIdentity not found")

// ErrCredentialMissing is returned when the AgentIdentity declares no
// credential of the requested name.
var ErrCredentialMissing = errors.New("agentidentity: credential absent from the AgentIdentity")

// ErrNotReplaceable is returned for a credential whose value cannot be
// replaced by pasting a bearer token: type=oauth (a multi-key token bundle
// only the OAuth ceremony can mint), type=federated (minted per-use from an
// IdP assertion — nothing is stored to replace), or a malformed static
// credential with no Secret name/key to write.
var ErrNotReplaceable = errors.New("agentidentity: credential cannot be replaced by pasting a value")

// ErrSecretRefMismatch is returned when the Secret this credential resolves to
// today is NOT the Secret the caller recorded as its backing store.
//
// This invariant keeps the flow honest. The CredentialUpdateRequest reconciler
// decides a request is Fulfilled by watching exactly ONE Secret — the one
// recorded on status.credentialSecretRef when the card opened — for a content
// change. A write landing anywhere else leaves the request to expire announcing
// "nobody updated the credential" after a human did. So when the two disagree
// (the identity was edited while a human held the card), refuse rather than
// write to a Secret nothing is watching.
var ErrSecretRefMismatch = errors.New("agentidentity: the credential's backing Secret is not the one recorded for this request")

// ErrSecretMissing is returned when the credential's backing Secret does not
// exist. Deliberately NOT auto-created: a credential whose Secret has vanished
// is a broken identity, and inventing the Secret here would paper over that
// while writing an object no controller expects this component to author.
var ErrSecretMissing = errors.New("agentidentity: the credential's backing Secret does not exist")

// ErrNotOAuthCredential is returned when the credential named for
// PutOAuthToken exists but is not type=oauth (or declares no OAuth block).
//
// The connect-flow handler that calls PutOAuthToken is expected to have
// already validated the credential's type before starting the OAuth dance —
// this check is defense-in-depth, not the primary gate, and it fires only
// when the AgentIdentity changed shape underneath an in-flight flow.
var ErrNotOAuthCredential = errors.New("agentidentity: credential is not type=oauth")

// ErrValueUnchanged is returned when the submitted value is byte-identical to
// the value already stored.
//
// It is an ERROR, not a no-op success, because the Secret's content hash is
// what marks the CredentialUpdateRequest Fulfilled. Accepting an unchanged
// value would leave the request to sit out its whole window and then report
// that nobody acted — a human DID act, and was told it worked. Refusing lets
// the caller say the one useful thing instead: this is the value the platform
// already verified no longer authenticates, so paste the new one.
var ErrValueUnchanged = errors.New("agentidentity: the submitted value is identical to the stored one")

// Target is where a replacement value for an AgentIdentity credential lands:
// the backing Secret and the key within it.
type Target struct {
	// SecretRef is the backing Secret (namespace + name).
	SecretRef spiceboxv1alpha1.NamespacedRef
	// Key is the key within that Secret's Data the credential's value occupies.
	Key string
}

// Resolve reports where credName's value lives for the AgentIdentity ns/name,
// or a typed error when it cannot be replaced by pasting a value.
//
// It derives the Secret through credresolve.SourceFor — the SAME derivation the
// CredentialUpdateRequest reconciler uses to record status.credentialSecretRef —
// rather than re-reading spec.credentials by hand. Two independent derivations
// of "which Secret backs this credential" is exactly how a write ends up on a
// Secret nobody is watching.
func Resolve(ctx context.Context, c client.Reader, ns, name, credName string) (Target, error) {
	if ns == "" || name == "" || credName == "" {
		return Target{}, fmt.Errorf("agentidentity.Resolve: namespace, name, and credentialName are required")
	}
	var ai spiceboxv1alpha1.AgentIdentity
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &ai); err != nil {
		if apierrors.IsNotFound(err) {
			return Target{}, fmt.Errorf("%w: %s/%s", ErrIdentityNotFound, ns, name)
		}
		return Target{}, fmt.Errorf("get AgentIdentity %s/%s: %w", ns, name, err)
	}

	var cred *spiceboxv1alpha1.AgentCredential
	for i := range ai.Spec.Credentials {
		if ai.Spec.Credentials[i].Name == credName {
			cred = &ai.Spec.Credentials[i]
			break
		}
	}
	if cred == nil {
		return Target{}, fmt.Errorf("%w: %q not in AgentIdentity %s/%s", ErrCredentialMissing, credName, ns, name)
	}
	// Only a credential kind whose Secret holds a SINGLE pasteable value under
	// one named key can be replaced this way. type=oauth's Secret is a
	// multi-key bundle (access_token/refresh_token/token_endpoint/…) that only
	// the OAuth ceremony can produce coherently — it reports a SecretRef with
	// an empty Key — and type=federated (minted) reports no SecretRef at all.
	// Writing a pasted string into either would produce a Secret that parses
	// but cannot authenticate.
	k, err := credkindregistry.Get(cred.Type)
	if err != nil {
		return Target{}, fmt.Errorf("%w: %q on AgentIdentity %s/%s: %w", ErrNotReplaceable, credName, ns, name, err)
	}
	if ref := k.SecretRef(*cred); ref == nil || ref.Key == "" {
		return Target{}, fmt.Errorf("%w: %q on AgentIdentity %s/%s is type=%s", ErrNotReplaceable, credName, ns, name, cred.Type)
	}

	src, err := credresolve.SourceFor(cred, credresolve.RuntimeIdentityFromAgentIdentity(&ai))
	if err != nil {
		return Target{}, fmt.Errorf("%w: %q on AgentIdentity %s/%s: %w", ErrNotReplaceable, credName, ns, name, err)
	}
	if src.Name == "" || src.Key == "" || src.Namespace == "" {
		return Target{}, fmt.Errorf("%w: %q on AgentIdentity %s/%s resolves to secret %q key %q in namespace %q",
			ErrNotReplaceable, credName, ns, name, src.Name, src.Key, src.Namespace)
	}
	return Target{
		SecretRef: spiceboxv1alpha1.NamespacedRef{Namespace: src.Namespace, Name: src.Name},
		Key:       src.Key,
	}, nil
}

// PutTokenRequest is the input to PutToken.
type PutTokenRequest struct {
	// Namespace + Name identify the AgentIdentity CR that owns the credential.
	Namespace string
	Name      string

	// CredentialName is the credential's name within that AgentIdentity.
	CredentialName string

	// Token is the replacement value.
	Token string

	// ExpectSecret, when non-nil, is the Secret the platform already recorded
	// as this credential's backing store (a CredentialUpdateRequest's
	// status.credentialSecretRef). PutToken refuses with ErrSecretRefMismatch
	// when the credential resolves anywhere else — see that error's doc for
	// why writing "somewhere reasonable" is the bug, not the fallback.
	ExpectSecret *spiceboxv1alpha1.NamespacedRef
}

// PutToken replaces the stored value of an AgentIdentity-owned credential and
// returns where it wrote. It updates ONLY the backing Secret: the
// AgentIdentity's spec already declares this credential (Resolve proved it),
// so there is nothing to upsert on the CR.
//
// Not idempotent by design — a re-submission of the SAME value returns
// ErrValueUnchanged rather than succeeding. See that error's doc.
func PutToken(ctx context.Context, c client.Client, req PutTokenRequest) (Target, error) {
	if req.Token == "" {
		return Target{}, fmt.Errorf("agentidentity.PutToken: token is required")
	}
	target, err := Resolve(ctx, c, req.Namespace, req.Name, req.CredentialName)
	if err != nil {
		return Target{}, err
	}
	if req.ExpectSecret != nil && (req.ExpectSecret.Namespace != target.SecretRef.Namespace || req.ExpectSecret.Name != target.SecretRef.Name) {
		return Target{}, fmt.Errorf("%w: credential %q resolves to %s/%s but the request recorded %s/%s",
			ErrSecretRefMismatch, req.CredentialName,
			target.SecretRef.Namespace, target.SecretRef.Name,
			req.ExpectSecret.Namespace, req.ExpectSecret.Name)
	}

	var sec corev1.Secret
	secKey := client.ObjectKey{Namespace: target.SecretRef.Namespace, Name: target.SecretRef.Name}
	if err := c.Get(ctx, secKey, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return Target{}, fmt.Errorf("%w: %s", ErrSecretMissing, secKey)
		}
		return Target{}, fmt.Errorf("get backing Secret %s: %w", secKey, err)
	}
	if bytes.Equal(sec.Data[target.Key], []byte(req.Token)) {
		return Target{}, fmt.Errorf("%w: %s key %q", ErrValueUnchanged, secKey, target.Key)
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	sec.Data[target.Key] = []byte(req.Token)
	if err := c.Update(ctx, &sec); err != nil {
		return Target{}, fmt.Errorf("update backing Secret %s: %w", secKey, err)
	}
	return target, nil
}

// PutOAuthTokenRequest is the input to PutOAuthToken.
type PutOAuthTokenRequest struct {
	// Namespace + Name identify the AgentIdentity CR that owns the credential.
	Namespace string
	Name      string

	// CredentialName is the credential's name within that AgentIdentity. It
	// must already exist as type=oauth — PutOAuthToken fills a pre-declared
	// credential; it never declares one.
	CredentialName string

	// AccessToken is the current OAuth access token. Required.
	AccessToken string

	// RefreshToken is the long-lived refresh token. Absent from the Secret
	// when empty. Requires TokenEndpoint be set — see PutOAuthToken's doc.
	RefreshToken string

	// TokenType is typically "Bearer". Written when non-empty.
	TokenType string

	// Scope is the space-separated granted scopes string. Absent from the
	// Secret when empty.
	Scope string

	// TokenEndpoint is the provider's RFC 6749 token endpoint. Required
	// whenever RefreshToken is set: it is the only durable home for the
	// endpoint, and pkg/platform/identity/refresh reads it from there at
	// every redemption. Absent from the Secret when empty.
	TokenEndpoint string

	// ClientID is the OAuth client the tokens were issued to. Absent from
	// the Secret when empty.
	ClientID string

	// ClientSecret is the client's secret, for confidential clients only.
	// Absent from the Secret when empty.
	ClientSecret string

	// ExpiresAt is the expiry as a Unix epoch seconds value. Zero means
	// "unknown / never expires"; the expires_at key is omitted when zero.
	ExpiresAt int64
}

// PutOAuthToken writes an OAuth token bundle onto an AgentIdentity's
// pre-declared type=oauth credential — the write half of the shared-bot
// OAuth connect flow. A builder pre-declares the credential (name + type +
// SecretRef.Name) when wiring up the connection; PutOAuthToken fills the
// value in once the OAuth dance completes. It never declares the credential
// itself, and it refuses — writing nothing — when the named credential is
// absent or is not type=oauth.
//
// Unlike PutToken, PutOAuthToken GET-OR-CREATES the backing Secret: a
// freshly pre-declared credential names a Secret that does not exist yet
// until the first successful OAuth round trip produces one. This is a
// deliberate divergence, not an oversight — PutToken's ErrSecretMissing
// exists because a REPLACEMENT presumes something to replace, but a fresh
// connect has nothing to get-or-create from.
//
// The co-located oauth key set (access_token/refresh_token/expires_at/
// token_type/scope/token_endpoint/client_id/client_secret) is the union of the
// keys pkg/platform/identity/setup's writeValueIntoSecret co-locates and
// useridentity.PutOAuthToken's — the former omits token_type, the latter writes
// it, and token_type is harmless non-redemption metadata refresh.Run never
// reads. It is assigned to sec.Data WHOLESALE, never merged into whatever was
// already there. A re-connect that returns fewer keys (no refresh_token this time, a
// dropped client_secret) must leave none of the stale ones behind — a stale
// refresh_token or client_secret co-located here is live redemption material
// pkg/platform/identity/refresh.Run would happily redeem with, so surviving a
// wholesale-replace bug silently is worse than the write simply failing.
func PutOAuthToken(ctx context.Context, c client.Client, req PutOAuthTokenRequest) (spiceboxv1alpha1.NamespacedRef, error) {
	if req.Namespace == "" || req.Name == "" || req.CredentialName == "" {
		return spiceboxv1alpha1.NamespacedRef{}, fmt.Errorf("agentidentity.PutOAuthToken: namespace, name, and credentialName are required")
	}
	if req.AccessToken == "" {
		return spiceboxv1alpha1.NamespacedRef{}, fmt.Errorf("agentidentity.PutOAuthToken: accessToken is required")
	}
	// Mirror useridentity.PutOAuthToken's check: a refresh token with nowhere
	// to redeem it can never be refreshed. Refuse at write time rather than
	// persist a credential parked at Refresh=False forever.
	if req.RefreshToken != "" && req.TokenEndpoint == "" {
		return spiceboxv1alpha1.NamespacedRef{}, fmt.Errorf(
			"agentidentity.PutOAuthToken: %q carries a refreshToken but no tokenEndpoint; "+
				"the credential could never be refreshed", req.CredentialName)
	}

	var ai spiceboxv1alpha1.AgentIdentity
	if err := c.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, &ai); err != nil {
		if apierrors.IsNotFound(err) {
			return spiceboxv1alpha1.NamespacedRef{}, fmt.Errorf("%w: %s/%s", ErrIdentityNotFound, req.Namespace, req.Name)
		}
		return spiceboxv1alpha1.NamespacedRef{}, fmt.Errorf("get AgentIdentity %s/%s: %w", req.Namespace, req.Name, err)
	}

	var cred *spiceboxv1alpha1.AgentCredential
	for i := range ai.Spec.Credentials {
		if ai.Spec.Credentials[i].Name == req.CredentialName {
			cred = &ai.Spec.Credentials[i]
			break
		}
	}
	if cred == nil {
		return spiceboxv1alpha1.NamespacedRef{}, fmt.Errorf("%w: %q not in AgentIdentity %s/%s", ErrCredentialMissing, req.CredentialName, req.Namespace, req.Name)
	}
	// The OAuth block's PRESENCE is the oauth signal — the CRD's type
	// discriminator makes cred.OAuth non-nil iff type=oauth. Checking the block
	// rather than comparing cred.Type to the literal "oauth" keeps credential-type
	// dispatch out of this package (the credkind registry owns type strings;
	// TestNoCredentialTypeSwitchesOutsideCredkind enforces it). cred.Type is still
	// read for the error message, which the guard permits.
	if cred.OAuth == nil {
		return spiceboxv1alpha1.NamespacedRef{}, fmt.Errorf("%w: %q on AgentIdentity %s/%s is type=%s", ErrNotOAuthCredential, req.CredentialName, req.Namespace, req.Name, cred.Type)
	}
	secretName := cred.OAuth.SecretRef.Name
	if secretName == "" {
		return spiceboxv1alpha1.NamespacedRef{}, fmt.Errorf("%w: %q on AgentIdentity %s/%s declares no secretRef.name", ErrNotOAuthCredential, req.CredentialName, req.Namespace, req.Name)
	}

	// Build the co-located key set FRESH. Assigned to sec.Data wholesale
	// below — never merge it into an existing Secret's Data.
	data := map[string][]byte{
		"access_token": []byte(req.AccessToken),
	}
	if req.RefreshToken != "" {
		data["refresh_token"] = []byte(req.RefreshToken)
	}
	if req.ExpiresAt != 0 {
		data["expires_at"] = []byte(time.Unix(req.ExpiresAt, 0).UTC().Format(time.RFC3339))
	}
	if req.TokenType != "" {
		data["token_type"] = []byte(req.TokenType)
	}
	if req.Scope != "" {
		data["scope"] = []byte(req.Scope)
	}
	if req.TokenEndpoint != "" {
		data["token_endpoint"] = []byte(req.TokenEndpoint)
	}
	if req.ClientID != "" {
		data["client_id"] = []byte(req.ClientID)
	}
	if req.ClientSecret != "" {
		data["client_secret"] = []byte(req.ClientSecret)
	}

	secKey := client.ObjectKey{Namespace: req.Namespace, Name: secretName}
	var sec corev1.Secret
	switch err := c.Get(ctx, secKey, &sec); {
	case apierrors.IsNotFound(err):
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: req.Namespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}
		if err := c.Create(ctx, &sec); err != nil {
			return spiceboxv1alpha1.NamespacedRef{}, fmt.Errorf("agentidentity.PutOAuthToken: create Secret %s: %w", secKey, err)
		}
	case err != nil:
		return spiceboxv1alpha1.NamespacedRef{}, fmt.Errorf("agentidentity.PutOAuthToken: get Secret %s: %w", secKey, err)
	default:
		sec.Data = data // wholesale replace — see PutOAuthToken's doc comment.
		if err := c.Update(ctx, &sec); err != nil {
			return spiceboxv1alpha1.NamespacedRef{}, fmt.Errorf("agentidentity.PutOAuthToken: update Secret %s: %w", secKey, err)
		}
	}

	return spiceboxv1alpha1.NamespacedRef{Namespace: req.Namespace, Name: secretName}, nil
}

// Failure codes. Every refusal on this path crosses an HTTP boundary — the
// operator decides and writes, a browser-facing component renders the sentence
// the human reads — so the reason must survive the hop as something better than
// a status line. Each code maps 1:1 to a sentinel above, and each sentinel
// names a DIFFERENT next action for the human; collapsing them to one
// "couldn't save" is what sends an admin off to fix the wrong thing.
const (
	CodeIdentityNotFound   = "identity_not_found"
	CodeCredentialMissing  = "credential_missing"
	CodeNotReplaceable     = "not_replaceable"
	CodeSecretRefMismatch  = "secret_ref_mismatch"
	CodeSecretMissing      = "secret_missing"
	CodeValueUnchanged     = "value_unchanged"
	CodeAmbiguousOwner     = "ambiguous_owner"
	CodeNotOAuthCredential = "not_oauth_credential"
)

// CodeFor returns the wire code for err, or "" when err is not one of this
// package's typed refusals (an infrastructure fault the caller should surface
// as a generic failure rather than a specific instruction).
func CodeFor(err error) string {
	switch {
	case errors.Is(err, ErrIdentityNotFound):
		return CodeIdentityNotFound
	case errors.Is(err, ErrCredentialMissing):
		return CodeCredentialMissing
	case errors.Is(err, ErrNotReplaceable):
		return CodeNotReplaceable
	case errors.Is(err, ErrSecretRefMismatch):
		return CodeSecretRefMismatch
	case errors.Is(err, ErrSecretMissing):
		return CodeSecretMissing
	case errors.Is(err, ErrValueUnchanged):
		return CodeValueUnchanged
	case errors.Is(err, ErrAmbiguousOwner):
		return CodeAmbiguousOwner
	case errors.Is(err, ErrNotOAuthCredential):
		return CodeNotOAuthCredential
	default:
		return ""
	}
}

// ErrorForCode reverses CodeFor, so a client that received a code over HTTP can
// branch with errors.Is exactly as an in-process caller would. detail is
// wrapped in for logs. An unrecognized code yields a plain error carrying the
// detail — never nil, so a caller can never mistake an unknown failure for
// success.
func ErrorForCode(code, detail string) error {
	if detail == "" {
		detail = "no detail supplied"
	}
	for _, s := range []error{
		ErrIdentityNotFound, ErrCredentialMissing, ErrNotReplaceable,
		ErrSecretRefMismatch, ErrSecretMissing, ErrValueUnchanged, ErrAmbiguousOwner,
		ErrNotOAuthCredential,
	} {
		if CodeFor(s) == code {
			return fmt.Errorf("%w: %s", s, detail)
		}
	}
	return fmt.Errorf("agentidentity: %s", detail)
}
