// Package credkind is the pluggable driver surface for identity credential
// types — one Kind per value of AgentCredential.Type / CredentialSource.Type.
//
// Every fact a consumer needs about a credential type is a method here. That
// is deliberate. Before this package the same handful of facts were re-derived
// by roughly forty-five switch statements spread across the broker, the
// credential resolvers, four controllers, the setup flow, the portal and the
// CLI — and three of those sites failed SILENTLY when a new type appeared: a
// missing token-grant arm fails the grant closed so the agent never dispatches,
// a missing revocation arm means rotating a key does not revoke, and a missing
// update-policy arm offers a human a "paste a new token" card for a credential
// that stores no token at all.
//
// Adding a credential type is now one package plus one blank import.
package credkind

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

// Scope is an identity CR a credential type may legally be declared on.
type Scope string

const (
	ScopeAgentIdentity       Scope = "AgentIdentity"
	ScopeUserIdentity        Scope = "UserIdentity"
	ScopeSessionUserIdentity Scope = "SessionUserIdentity"
)

// SecretRef locates a credential's backing Secret and, when the type stores its
// value under one named key, that key.
//
// Key is empty for a type whose Secret has a FIXED multi-key shape (oauth's
// access_token/refresh_token/expires_at/..., or a GitHub App's
// app-id/private-key/installation-id). Callers that check "does the named key
// exist" must treat an empty Key as "this type has no single key to check"
// rather than as a missing value.
type SecretRef struct {
	Name string
	Key  string
}

// GitHubAppMinter mints a GitHub App installation access token for
// Deps.GitHubApp.
//
// Its signature is deliberately primitive-typed (a bare appID/privateKeyPEM/
// installationID triple) rather than reusing credkind/githubapp's own
// MintRequest/MintedToken: that package's Kind implementation
// (githubapp/kind.go) must import THIS package to satisfy the Kind
// interface, so this package importing githubapp's types back would be the
// reverse edge of that same cycle — the same shape federation.Minter avoids
// by living outside the credkind tree entirely. credkind/githubapp.Adapt
// bridges a real githubapp.Minter into this shape.
type GitHubAppMinter interface {
	Mint(ctx context.Context, appID string, privateKeyPEM []byte, installationID string) (sensitive.SensitiveValue, time.Time, error)
}

// Deps carries the collaborators a Kind may need at Resolve time.
//
// Every field is declared as an INTERFACE, never as a pointer that is assigned
// in later. Go's interface representation is a {type, value} tuple, so a
// typed-nil pointer stored in an interface field compares != nil and panics on
// the first method call. A Kind whose collaborator is genuinely absent must
// detect that and fail closed inside its own package.
type Deps struct {
	// Client reads Secrets and, for types that support it, patches them during
	// a just-in-time refresh.
	Client client.Client

	// Federation mints ID-JAG credentials. Nil when the cluster IdP has no
	// federation configured; the federated Kind fails closed on nil rather
	// than resolving an empty credential.
	Federation federation.Minter

	// GitHubApp mints GitHub App installation access tokens. Nil when no
	// GitHub App minter is configured; the githubApp Kind fails closed on
	// nil rather than resolving an empty credential — the exact mirror of
	// Federation above.
	GitHubApp GitHubAppMinter
}

// Kind is one credential source type. The registry key is Type().
type Kind interface {
	// Type is the registry key and the CRD enum value.
	Type() string

	// Resolve produces the injectable credential and the instant it stops
	// being usable.
	//
	// A Minted Kind MUST return a real expiry. A stored Kind returns the zero
	// time, and the broker bounds the cache entry itself. The zero-expiry rule
	// is enforced once, at the broker's choke point, rather than trusted to
	// each implementation.
	Resolve(ctx context.Context, deps Deps, src spiceboxv1alpha1.CredentialSource) (authkind.ResolvedCredential, time.Time, error)

	// Minted reports that each Resolve produces a FRESH short-lived token
	// rather than reading a stored one.
	//
	// This single fact used to be three independent switches: the broker's
	// expiry gate, identity-only token grants (a minted value cannot be pinned
	// at reconcile time, so a value-bound caveat would false-deny after every
	// mint), and "there is no stored token for a human to re-paste".
	Minted() bool

	// ValidOn says which identity CRs may declare this type. It is where two
	// pieces of prose that used to live in two validators become one answer:
	// federated is passthrough-only and rejected on an AgentIdentity, and
	// githubApp is its exact mirror — valid ONLY on an AgentIdentity.
	ValidOn() []Scope

	// ValidateSpec checks that THIS type's own block is present and
	// well-formed. The returned message is surfaced verbatim on a SpecInvalid
	// condition, so it must name the offending field.
	//
	// It deliberately says nothing about sibling blocks: "only my own block
	// may be set" is a cross-kind rule, enforced once by
	// registry.ValidateExclusive off HasBlock. Re-asserting it here is what
	// made adding a fourth type require editing the other three.
	ValidateSpec(c spiceboxv1alpha1.AgentCredential) error

	// SecretRef locates the backing Secret and the key that must exist.
	// nil means this type has no backing Secret at all — nothing to adopt,
	// nothing to key-check, nothing to revoke by Secret name.
	SecretRef(c spiceboxv1alpha1.AgentCredential) *SecretRef

	// SecretRefPath is the field path, relative to ONE element of
	// spec.credentials, at which this type stores its backing Secret's NAME —
	// e.g. {"static", "secretRef", "name"}. nil for a type with no backing
	// Secret, mirroring SecretRef returning nil.
	//
	// It exists for the callers that must REWRITE the reference rather than
	// read it, working on an unstructured CR where SecretRef's typed accessor
	// does not reach: `oap agent install --name <prefix>` relocates every
	// bundled Secret and has to repoint each credential at its prefixed copy.
	// That rewrite used to enumerate "static" and "oauth" as path literals,
	// which silently left every githubApp credential pointing at a Secret the
	// prefixed install never created — Valid=False/SecretMissing, no token
	// ever minted. Neither credential-dispatch guard could see it: the arms
	// were unstructured path strings, not a .Type switch and not
	// .Static/.OAuth field selectors.
	//
	// TestSecretRefPathAgreesWithSecretRef pins each implementation's path
	// against its own SecretRef, so the two halves cannot drift.
	SecretRefPath() []string

	// HasBlock reports whether c carries THIS type's own union block,
	// regardless of what c.Type says.
	//
	// It is the read every cross-kind rule needs and no single kind can
	// answer for another. Exclusivity — "a credential must carry only the
	// block its declared type owns" — used to be written out inside each
	// kind's ValidateSpec as a list of its SIBLINGS' blocks, which is O(n^2)
	// coupling in the one package that exists to remove it: a fourth kind
	// meant editing the other three, and the three that were not edited
	// silently accepted a stray githubApp block. With HasBlock the rule is
	// asked of the registry instead (registry.ValidateExclusive), and a fifth
	// kind implements one method rather than provoking four edits.
	HasBlock(c spiceboxv1alpha1.AgentCredential) bool

	// BuildCredential constructs the typed spec block — the inverse of
	// SecretRef. A type the setup flow cannot write returns an error rather
	// than a credential with every block left nil.
	BuildCredential(name, secretName, secretKey string) (spiceboxv1alpha1.AgentCredential, error)

	// NeedsRefresh reports that stored values of this type expire and are
	// renewed out of band by the identity-refresh controller. It is false for
	// minted types: nothing is stored, so nothing can be refreshed.
	NeedsRefresh() bool

	// DisplayName is the human-facing label used by the CLI and the portal.
	DisplayName() string

	// ReadStoredValue reads this credential's stored value from its backing
	// Secret. It is the read half of the type, separate from Resolve: Resolve
	// may mint, cache, and refresh, whereas this only reads what is already
	// stored. A minted type has nothing stored and returns an error.
	ReadStoredValue(ctx context.Context, c client.Reader, ns string, cred spiceboxv1alpha1.AgentCredential) (sensitive.SensitiveValue, error)

	// Projectable reports that this type's VALUE can be projected into a
	// per-session Secret keyed by credential name (userPassthrough). False
	// for a type that needs its own multi-key Secret shape (oauth's
	// access_token/refresh_token/expires_at/...), and for minted types,
	// which store no value to project in the first place.
	Projectable() bool

	// RequiredSecretKeys lists the Secret data keys that MUST be present and
	// non-empty for this credential to be usable. The identity controllers check
	// them generically instead of each knowing any type's Secret layout.
	//
	// nil means "no per-key requirement" — either the type stores nothing
	// (federated) or its Secret's shape is not a fixed key list.
	RequiredSecretKeys(c spiceboxv1alpha1.AgentCredential) []string

	// PublicSecretKeys names the data keys of this credential's backing Secret
	// that hold a PUBLIC identifier rather than secret material.
	//
	// A credential bundle routinely mixes the two. A GitHub App's Secret holds
	// a private key and a webhook secret alongside an app id and an
	// installation id, and neither of the latter two is a credential: the app
	// id appears in the App's own URL, and GitHub puts the installation id in
	// the body of every webhook it sends. Nothing outside this package can tell
	// them apart — "the bytes came out of a Secret" is provenance, not
	// sensitivity — so the type that defines the Secret's shape says which is
	// which, and a consumer that has to make the distinction asks rather than
	// carrying a list of key names it would have to keep in step.
	//
	// FAIL CLOSED. nil is the answer for every type that has not thought about
	// it, and nil means EVERY key is secret. This is an opt-out for named
	// public keys, never an opt-in for protection: a credential type added
	// later, or one whose author never considered this method, gets the safe
	// answer without doing anything. Consumers must treat an unregistered type,
	// an error, and nil identically — as "all of it is secret".
	//
	// A key named here must be public in EVERY Secret of this type, not merely
	// in the one in front of the author. When that is not true of a key, leave
	// it out: the cost of omitting it is a consumer being more careful than it
	// needed to be, and the cost of including it wrongly is a credential
	// treated as public.
	PublicSecretKeys(c spiceboxv1alpha1.AgentCredential) []string
}

// ValidOnScope reports whether k may be declared on scope. Consumers call this
// rather than ranging over ValidOn themselves, so the membership test has one
// implementation.
func ValidOnScope(k Kind, scope Scope) bool {
	for _, s := range k.ValidOn() {
		if s == scope {
			return true
		}
	}
	return false
}
