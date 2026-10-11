// Package directcred defines the pluggable "website login source" contract
// for the browser toolbox's direct-credential path: a Source lists and
// fetches logins held by some external vault (1Password, a plain
// Kubernetes Secret, …) so the browser toolbox can fill them into a page
// without the agent ever seeing — or the runner ever persisting — the
// values outside the moment of use.
//
// This package is pure contract + registry: no Kubernetes or client-go
// imports, no backend logic. The two backends (1Password, Secret) live in
// their own packages under pkg/platform/identity/directcred/<kind>/ and
// register themselves here via an init(), mirroring every other kind
// registry in this repo (pkg/tools/kinds/registry, authkind/registry, …).
//
// Security posture, load-bearing for every caller of this package:
//
//   - Ref carries NOTHING from a login item's fields — only an opaque ID, a
//     display label, and the hosts it matches. A List call must be safe to
//     log and to show in a UI without exposing any secret.
//   - Credential values are for IMMEDIATE USE ONLY (Phase C's form filler).
//     They must never be logged, cached beyond the call that produced them,
//     or written to any store — memory, status, or otherwise.
//   - Config.SecretData is supplied by the CALLER. The operator/runner reads
//     the backing Secret (via credresolve + the adopted-secret reader) and
//     passes its data in; a Source implementation never touches the API
//     server, and never holds a client.Client.
package directcred

import "context"

// Kind strings a Source registers under. Every later task (the two
// backends, the AgentIdentity/AgentSession controllers, the setup flow)
// matches against these constants rather than a literal string.
const (
	// SourceOnePassword is the kind string for the 1Password-backed Source.
	SourceOnePassword = "onepassword"
	// SourceSecret is the kind string for the plain-Secret-backed Source.
	SourceSecret = "secret"
)

// Config carries everything a Source needs for one call. The CALLER reads
// the referenced Secret (via credresolve + the adopted-secret reader) and
// the declared vault name/id from the CRD spec/status — a Source never
// touches the API server itself.
type Config struct {
	// SecretData is the referenced Secret's data, verbatim. Its expected
	// keys are backend-specific (e.g. a 1Password Connect token, or the
	// username/password pair for the secret backend).
	SecretData map[string][]byte
	// Vault is the declared vault name. Onepassword only; ignored by the
	// secret backend.
	Vault string
	// VaultID is the pinned vault id from a prior Verify call, or "" before
	// the first Verify. Onepassword only; ignored by the secret backend.
	VaultID string
}

// Host is one hostname pattern a Ref is scoped to.
type Host struct {
	// Name is the canonical form: lowercase, no trailing dot.
	Name string
	// Exact is true when Name must match the page host exactly; false when
	// Name also matches subdomains.
	Exact bool
}

// Ref is one login a Source lists. It carries NOTHING from the underlying
// item's fields — no username, no password, no notes — only what is needed
// to show the entry in a picker and to later Fetch it.
type Ref struct {
	// ID is opaque to everyone but the Source that issued it. Callers must
	// not parse or infer structure from it.
	ID string
	// Label is the display name shown to a human picking a login.
	Label string
	// Hosts is the set of hostname patterns this login applies to.
	Hosts []Host
}

// Credential is one login's values, for immediate use by Phase C's form
// filler. A Credential must never be logged or persisted — not to memory,
// not to status, not to a log line — by any caller of Fetch.
type Credential struct {
	// Username is the login's username or email value.
	Username string
	// Password is the login's password value.
	Password string
	// OTP, when non-nil, produces the login's current one-time code on
	// demand. Nil when the entry has no one-time code configured.
	OTP func() (string, error)
}

// Pin is what Verify proves: which vault a configuration's credentials are
// scoped to. Sources with no vault concept (secret) return a zero Pin.
type Pin struct {
	// VaultID is the backend's stable identifier for the vault.
	VaultID string
	// VaultName is the human-readable vault name, as confirmed by the
	// backend — may differ in case/whitespace from the declared Vault.
	VaultName string
}

// Source is the per-backend plug-in surface. One impl per login-storage
// backend (1Password, a plain Secret, …), registered under its Kind() via
// Register.
type Source interface {
	// Kind returns the registration key — one of the Source* constants
	// above for the backends shipped in this repo.
	Kind() string

	// Verify checks the configuration against the live backend and returns
	// the vault it resolves to. For a Source with no vault concept (secret),
	// Pin is zero and a non-nil error reports only malformed configuration
	// (missing/invalid SecretData), never a vault mismatch.
	Verify(ctx context.Context, cfg Config) (Pin, error)

	// List returns the credential references in scope for cfg. Never a
	// value — see Ref's doc comment.
	List(ctx context.Context, cfg Config) ([]Ref, error)

	// Fetch returns one credential's values, for immediate use. id is a Ref.ID
	// previously returned by List for the same cfg.
	Fetch(ctx context.Context, cfg Config, id string) (Credential, error)
}
