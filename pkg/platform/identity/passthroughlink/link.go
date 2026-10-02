// Package passthroughlink mints + verifies HMAC-signed deep-links that
// channelsd sends a passthrough-session's starter and identityd consumes.
// The link is tamper-proof + expiry-gated, but identity proof is OIDC —
// see pkg/channels/channelkinds.WebAuthenticator. The link only scopes WHAT and
// for which session; the channel-kind OIDC proves WHO.
//
// Payload uses JWT-style claim names (iss / aud / iat / nbf / exp / jti
// / sub) so the security semantics are familiar to operators auditing
// the flow. The wire format is HMAC-SHA256 with the body base64url-
// encoded and the signature hex-encoded — NOT a JWT itself; we never
// negotiate algorithms via the header (a known JWT footgun).
package passthroughlink

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// ErrExpired is returned when Verify is called on a link whose ExpiresAt
// has passed.
var ErrExpired = errors.New("passthroughlink: link expired")

// ErrInvalidSignature is returned when the HMAC does not match.
var ErrInvalidSignature = errors.New("passthroughlink: invalid signature")

// ErrMalformed is returned when the encoded link is structurally invalid.
var ErrMalformed = errors.New("passthroughlink: malformed link")

// ErrNotYetValid is returned when Verify runs before Payload.NotBefore.
// Surfaces clock-skew problems on either end so operators can fix the
// drift rather than chase phantom "invalid link" reports.
var ErrNotYetValid = errors.New("passthroughlink: link not yet valid (nbf)")

// ErrIssuedInFuture is returned when Payload.IssuedAt is more than the
// configured clock-skew window in the future — a tampered or
// catastrophically-misclocked minter.
var ErrIssuedInFuture = errors.New("passthroughlink: link iat is in the future")

// ErrWrongIssuer is returned when Verify is called with an expected
// issuer that doesn't match the payload's iss.
var ErrWrongIssuer = errors.New("passthroughlink: link iss does not match expected issuer")

// ErrWrongAudience is returned when Verify is called with an expected
// audience that doesn't match the payload's aud.
var ErrWrongAudience = errors.New("passthroughlink: link aud does not match expected audience")

// ErrMissingJTI is returned when Verify sees a payload with no jti. Mint
// always sets a jti, so a jti-less token cannot have come from a legitimate
// minter (buggy/forged minter, or a hand-rolled token) and is refused.
var ErrMissingJTI = errors.New("passthroughlink: link has no jti")

// defaultClockSkew bounds how far in the past Verify will accept a
// NotBefore claim and how far in the future Verify will accept an
// IssuedAt claim. 60 seconds covers normal NTP drift without being so
// loose that an attacker with a stolen key gets meaningful replay
// headroom.
const defaultClockSkew = 60 * time.Second

// Payload is the JSON-encoded content the signed link carries. It is
// **tamper-proof but not confidential** — anyone who can read the link
// can read these fields. Do NOT put secrets here.
//
// Standard JWT-style claims (iss/aud/iat/nbf/exp/jti/sub) are mirrored
// here so the schema is familiar and amenable to security audit. We
// don't actually emit RFC 7519 JWTs because the JWS header's alg
// negotiation is a known footgun (alg=none, key confusion); we sign
// the JSON bytes directly with a fixed HMAC-SHA256.
type Payload struct {
	// Issuer ("iss") is the identifier of the component that minted
	// the link. Populated by Mint (auto-filled when Signer was
	// constructed with WithIssuer). Verifiers MAY require a specific
	// value via VerifyOption WithExpectedIssuer.
	Issuer string `json:"iss,omitempty"`
	// Audience ("aud") is the identifier of the component intended
	// to consume the link. Populated by Mint (auto-filled when Signer
	// was constructed with WithAudience). Verifiers MAY require a
	// specific value via VerifyOption WithExpectedAudience.
	Audience string `json:"aud,omitempty"`
	// SessionRef identifies the parked session ("<ns>/<name>"). identityd
	// resolves it to look up the AgentSession + the starter's canonical
	// subject for the OIDC gate.
	SessionRef string `json:"sessionRef,omitempty"`
	// Subject ("sub") is the canonical SpiceDB subject the link is intended for.
	// identityd compares it to the OIDC-proven subject; a mismatch is "this link
	// isn't for you". Empty means the link is not bound to one addressee, and the
	// click must then be authorized at identityd instead.
	Subject identity.Subject `json:"sub,omitempty"`
	// SubjectVerified marks that the MINTER proved the Subject's email
	// (IdP login or trusted-workspace channel lookup). identityd does NOT
	// treat it as authentication: it records what the minter knew about the
	// subject, not who holds the link, so it can never by itself buy a
	// session. Set ONLY from identity.Principal.EmailVerified.
	SubjectVerified bool `json:"subVerified,omitempty"`
	// RequiredCredentials lists the credential names the user must link
	// for the parked session to proceed. identityd renders the linking
	// UI from this list. Empty for portal/cookie payloads.
	RequiredCredentials []string `json:"requiredCredentials,omitempty"`
	// Purpose distinguishes link kinds, read together with SessionRef: empty
	// purpose + a SessionRef is the default credential-request deep-link;
	// "portal" + no SessionRef is standing-portal access; empty + empty is a
	// cookie payload (the idd_session cookie from the OIDC callback or the
	// trust-link path). See the Purpose* constants for the rest.
	Purpose string `json:"purpose,omitempty"`
	// ArtifactID is the logical-artifact head id an artifact_view link
	// points at (Purpose == PurposeArtifactView). webd resolves the
	// artifact's SpiceDB `view` permission for the visitor.
	ArtifactID string `json:"artifactId,omitempty"`
	// BackLink is an optional, opaque URL the consuming web UI renders as a
	// "back to origin" link (e.g. a Slack thread permalink for artifact_view
	// links). Empty when the minting channel kind has no permalink concept.
	// Tamper-proof (it rides inside the signed body) but not confidential.
	BackLink string `json:"backLink,omitempty"`
	// AgentIdentityRef is "<workshopNamespace>/<agentIdentityName>" — the
	// AgentIdentity in the workshop namespace W whose credential a
	// PurposeWorkshopCredential link connects. Empty for every other purpose.
	AgentIdentityRef string `json:"agentIdentityRef,omitempty"`
	// IssuedAt ("iat") is the Unix-epoch second the link was minted.
	// Auto-populated by Mint. Verify rejects values more than the
	// configured clock skew in the future — a defense against tampered
	// minters that try to extend the effective TTL by setting iat in
	// the future.
	IssuedAt int64 `json:"iat,omitempty"`
	// NotBefore ("nbf") is the Unix-epoch second before which the link
	// is invalid. Auto-populated by Mint to iat-skew so a legitimate
	// minter just inside skew tolerance still verifies. Verify rejects
	// values strictly greater than now+skew.
	NotBefore int64 `json:"nbf,omitempty"`
	// ExpiresAt ("exp") is the Unix-epoch second the link becomes
	// invalid. Required at Mint.
	ExpiresAt int64 `json:"exp"`
	// JTI ("jti") is an opaque unique identifier for the link, used for
	// audit log correlation. Auto-populated by Mint as 16 random bytes
	// base64url-encoded. Required at Verify — a token without a jti
	// likely came from a buggy minter and should not be trusted.
	JTI string `json:"jti,omitempty"`
}

// Signer mints and verifies signed links with a single HMAC key. It is
// optionally configured with an issuer + audience that are auto-
// populated into every minted Payload.
type Signer struct {
	key       []byte
	issuer    string
	audience  string
	clockSkew time.Duration
	// now is a test seam — production uses time.Now.
	now func() time.Time
}

// New returns a Signer that uses key for HMAC-SHA256. key SHOULD be at
// least 32 bytes of cryptographically-random data — the operator should
// generate it at install time (oap install creates a Secret).
func New(key []byte, opts ...Option) *Signer {
	s := &Signer{
		key:       key,
		clockSkew: defaultClockSkew,
		now:       time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Option configures a Signer at construction.
type Option func(*Signer)

// WithIssuer sets the iss claim Mint auto-populates onto every
// Payload. Constants like IssuerChannelsd / IssuerIdentityd document
// the convention.
func WithIssuer(iss string) Option { return func(s *Signer) { s.issuer = iss } }

// WithAudience sets the aud claim Mint auto-populates onto every
// Payload. Constants like AudienceIdentityd document the convention.
func WithAudience(aud string) Option { return func(s *Signer) { s.audience = aud } }

// WithClockSkew overrides the default 60s skew tolerance.
func WithClockSkew(d time.Duration) Option { return func(s *Signer) { s.clockSkew = d } }

// Issuer constants — kept here so the producers + verifiers
// reference a single source of truth.
const (
	// IssuerChannelsd is the iss value channelsd sets on every link
	// it mints (credential_request deep-link, portal deep-link).
	IssuerChannelsd = "channelsd"
	// IssuerIdentityd is the iss value identityd sets on the
	// idd_session cookie it mints (in /oidc/callback or the
	// trust-link path).
	IssuerIdentityd = "identityd"
)

// Audience constants — kept here so the producers + verifiers
// reference a single source of truth.
const (
	// AudienceIdentityd is the aud value for every deep-link the
	// user follows from a chat client into identityd.
	AudienceIdentityd = "identityd"
	// AudienceWebd is the aud value for deep-links the user follows into webd.
	AudienceWebd = "webd"
	// AudienceCLIIdentity is the aud value for the CLI identity
	// assertion minted by identityd's /cli/exchange. Distinct from
	// cookies/deep-links so an assertion never verifies in another
	// token's slot.
	AudienceCLIIdentity = "cli-identity"
)

// Purpose constants — kept here so the producers + verifiers
// reference a single source of truth.
const (
	// PurposeArtifactView marks a deep-link that opens an artifact live-view.
	PurposeArtifactView = "artifact_view"
	// PurposeCLIIdentity marks the CLI identity assertion payload.
	PurposeCLIIdentity = "cli_identity"
	// PurposeAdminLogin marks a session-less admin-UI login link: identityd
	// skips AgentSession/channel-kind resolution and dispatches straight to a
	// registered WebAuthenticator. The link carries NO Subject — identity is
	// proven by the OIDC flow; authorization happens at the /admin routes via
	// the SpiceDB platform permissions, never at login.
	PurposeAdminLogin = "admin_login"
	// PurposeSessionView marks a deep-link that opens a session-scoped live view
	// (pkg/web/webui/sessionview): the shared-transcript mirror of a source
	// channel's conversation, gated on SessionRef alone (no ArtifactID — a
	// session isn't an artifact). Long-lived and shareable (7 days), unlike
	// PurposeArtifactView's 30 minutes.
	PurposeSessionView = "session_view"
	// PurposeCredentialUpdate marks a deep-link for a card that REPLACES a
	// credential the platform verified is dead. Minted by channelsd's
	// CredentialUpdateWatcher, which is the sole custodian of the signing key;
	// the operator's credentialupdaterequest reconciler only DETERMINES.
	//
	// Subject-bound when the card has ONE addressee — the credential's own user,
	// or the turn author holding agentidentity#update_credential — and then
	// renders the same identityd /link menu page as the empty-purpose
	// credential-request deep-link. The monitoring-channel broadcast of an
	// agent-owned credential's card has no single addressee, so that variant
	// carries NO Subject and its click is authorized at identityd instead (cookie
	// plus a live update_credential check).
	//
	// Its own purpose value rather than the default one so audit and request logs
	// can tell "first-time connect" from "replacing a credential a human was
	// asked to fix" without inspecting RequiredCredentials.
	PurposeCredentialUpdate = "credential_update"
	// PurposeOAuthClient marks a STATELESS OAuth 2.0 client_id: identityd's
	// POST /oauth/register (RFC 7591 Dynamic Client Registration) mints one of
	// these instead of writing to a store, so the registration itself —
	// {client_name, redirect_uris}, JSON-encoded into BackLink — survives an
	// identityd restart or a different replica verifying it, with no shared
	// state at all. 2-year expiry (see oauthClientIDTTL in
	// pkg/platform/identityd/handlers_oauthas.go): long enough that a
	// registered local tool keeps working across routine signer-key
	// rotations' overlap window, but not eternal.
	PurposeOAuthClient = "oauth_client"
	// PurposeWorkshopCredential marks a deep-link that connects a bot
	// credential for an AgentIdentity living in a workshop namespace W. It
	// reuses SessionRef for the builder session ("<B>/<X>") that requested
	// the credential and RequiredCredentials for the single credential name
	// being connected; AgentIdentityRef ("<W>/<agentIdentityName>") names
	// which AgentIdentity in W the connected credential belongs to.
	PurposeWorkshopCredential = "workshop_credential"
)

// VerifyOption configures a single Verify call.
type VerifyOption func(*verifyConfig)

type verifyConfig struct {
	expectedIssuer   string
	expectedAudience string
}

// WithExpectedIssuer adds an "iss must equal this" check to Verify.
// Empty string disables the check (default).
func WithExpectedIssuer(iss string) VerifyOption {
	return func(c *verifyConfig) { c.expectedIssuer = iss }
}

// WithExpectedAudience adds an "aud must equal this" check to Verify.
// Empty string disables the check (default).
func WithExpectedAudience(aud string) VerifyOption {
	return func(c *verifyConfig) { c.expectedAudience = aud }
}

// Mint encodes p as base64url JSON, appends ".", then the HMAC-SHA256 of
// the base64 chunk (hex-encoded). Output shape: "<b64>.<hex-mac>".
//
// Auto-populated fields (when zero in the input):
//   - Issuer  — from the Signer's configured issuer.
//   - Audience — from the Signer's configured audience.
//   - IssuedAt — now.
//   - NotBefore — now - clockSkew (so a verifier within skew tolerance
//     still accepts the link).
//   - JTI — 16 cryptographically-random bytes, base64url-encoded.
//
// Caller-supplied non-zero values pass through unchanged so tests can
// pin exact claims.
func (s *Signer) Mint(p Payload) (string, error) {
	if p.ExpiresAt == 0 {
		return "", fmt.Errorf("passthroughlink: Mint requires ExpiresAt")
	}
	now := s.now()
	if p.IssuedAt == 0 {
		p.IssuedAt = now.Unix()
	}
	if p.NotBefore == 0 {
		p.NotBefore = now.Add(-s.clockSkew).Unix()
	}
	if p.Issuer == "" {
		p.Issuer = s.issuer
	}
	if p.Audience == "" {
		p.Audience = s.audience
	}
	if p.JTI == "" {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", fmt.Errorf("passthroughlink: jti random: %w", err)
		}
		p.JTI = base64.RawURLEncoding.EncodeToString(raw[:])
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("passthroughlink: marshal payload: %w", err)
	}
	b64 := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(b64))
	sig := hex.EncodeToString(mac.Sum(nil))
	return b64 + "." + sig, nil
}

// Verify checks the signature + expiry and returns the decoded Payload.
//
// Returns:
//   - ErrMalformed         if structure / encoding is wrong.
//   - ErrInvalidSignature  if the HMAC doesn't match.
//   - ErrExpired           if now > ExpiresAt.
//   - ErrNotYetValid       if now < NotBefore - clockSkew.
//   - ErrIssuedInFuture    if IssuedAt > now + clockSkew.
//   - ErrWrongIssuer       if WithExpectedIssuer set and payload's iss differs.
//   - ErrWrongAudience     if WithExpectedAudience set and payload's aud differs.
//   - ErrMissingJTI        if the payload has no jti (no legit minter omits it).
func (s *Signer) Verify(raw string, opts ...VerifyOption) (Payload, error) {
	cfg := &verifyConfig{}
	for _, o := range opts {
		o(cfg)
	}
	parts := strings.SplitN(raw, ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Payload{}, ErrMalformed
	}
	expected := hmac.New(sha256.New, s.key)
	expected.Write([]byte(parts[0]))
	expectedHex := hex.EncodeToString(expected.Sum(nil))
	if !hmac.Equal([]byte(expectedHex), []byte(parts[1])) {
		return Payload{}, ErrInvalidSignature
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Payload{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return Payload{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	now := s.now()
	// exp check first (most-likely failure mode for stale links).
	if now.Unix() > p.ExpiresAt {
		return Payload{}, ErrExpired
	}
	// iat: in the future beyond skew → either a tampered payload or a
	// catastrophically-misclocked minter. Either way: refuse.
	if p.IssuedAt != 0 && p.IssuedAt-now.Unix() > int64(s.clockSkew.Seconds()) {
		return Payload{}, ErrIssuedInFuture
	}
	// nbf: in the future beyond skew → not-yet-valid.
	if p.NotBefore != 0 && p.NotBefore-now.Unix() > int64(s.clockSkew.Seconds()) {
		return Payload{}, ErrNotYetValid
	}
	if cfg.expectedIssuer != "" && p.Issuer != cfg.expectedIssuer {
		return Payload{}, fmt.Errorf("%w: got %q, want %q", ErrWrongIssuer, p.Issuer, cfg.expectedIssuer)
	}
	if cfg.expectedAudience != "" && p.Audience != cfg.expectedAudience {
		return Payload{}, fmt.Errorf("%w: got %q, want %q", ErrWrongAudience, p.Audience, cfg.expectedAudience)
	}
	// jti is required at Verify (the Payload.JTI doc): Mint always sets it, so a
	// token reaching here without one came from a buggy or forged minter.
	if p.JTI == "" {
		return Payload{}, ErrMissingJTI
	}
	return p, nil
}
