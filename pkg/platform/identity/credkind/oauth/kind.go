// Package oauth implements the credkind.Kind for type=oauth: a token bundle
// stored under a FIXED multi-key Secret shape (access_token, refresh_token,
// expires_at, token_endpoint, client_id, scope).
//
// It is the only stored type that expires, which is why it is also the only one
// that owns a just-in-time refresh. Both the expiry gate and that refresh live
// here rather than in the broker: they are facts about this credential type,
// not about caching.
package oauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/refresh"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

func init() { registry.Register(Kind{}) }

// Kind is the type=oauth driver.
type Kind struct{}

var _ credkind.Kind = Kind{}

func (Kind) Type() string        { return "oauth" }
func (Kind) Minted() bool        { return false }
func (Kind) DisplayName() string { return "OAuth credential" }

// NeedsRefresh is true: the stored access token expires and the
// identity-refresh controller renews it out of band.
func (Kind) NeedsRefresh() bool { return true }

// Projectable is false: an oauth credential needs its own fixed multi-key
// Secret shape (access_token/refresh_token/expires_at/...), not a single
// value under one projected key.
func (Kind) Projectable() bool { return false }

// RequiredSecretKeys is just access_token: the rest of the bundle
// (refresh_token, expires_at, token_endpoint, client_id, scope) is optional
// or, for expires_at, checked separately by the expiry gate in
// ReadStoredValue — a Secret's shape and a stored value's freshness are
// different facts.
func (Kind) RequiredSecretKeys(spiceboxv1alpha1.AgentCredential) []string {
	return []string{"access_token"}
}

// PublicSecretKeys is nil, and deliberately so even though this type's Secret
// does hold keys nobody would call a credential (expires_at is a timestamp,
// token_endpoint a URL). Naming a key here is a promise about every Secret of
// this type on every cluster, and the OAuth bundle's shape is set by whichever
// provider wrote it — client_id is public for one provider and paired with a
// confidential client for the next. Nothing here has to be declared public for
// anything to work, so nothing is: see the fail-closed rule on credkind.Kind.
func (Kind) PublicSecretKeys(spiceboxv1alpha1.AgentCredential) []string { return nil }

func (Kind) ValidOn() []credkind.Scope {
	return []credkind.Scope{
		credkind.ScopeAgentIdentity,
		credkind.ScopeUserIdentity,
		credkind.ScopeSessionUserIdentity,
	}
}

func (Kind) ValidateSpec(c spiceboxv1alpha1.AgentCredential) error {
	if c.OAuth == nil || c.OAuth.SecretRef.Name == "" {
		return fmt.Errorf("credentials[%s].oauth: secretRef.name required", c.Name)
	}
	return nil
}

// HasBlock reports whether the oauth block is populated. Sibling exclusivity
// is registry.ValidateExclusive's job off this answer, not a list of the other
// kinds' blocks written out here — see credkind.Kind.HasBlock.
func (Kind) HasBlock(c spiceboxv1alpha1.AgentCredential) bool { return c.OAuth != nil }

// SecretRef reports the Secret with an EMPTY key. The oauth Secret's shape is
// fixed and multi-key, so there is no single key to name — and an empty Key is
// precisely how a caller learns not to run a "does this key exist" check that
// would fail for a perfectly valid credential.
func (Kind) SecretRef(c spiceboxv1alpha1.AgentCredential) *credkind.SecretRef {
	if c.OAuth == nil {
		return nil
	}
	return &credkind.SecretRef{Name: c.OAuth.SecretRef.Name}
}

// SecretRefPath is where SecretRef above reads the Secret NAME from, spelled
// as a field path for callers rewriting an unstructured CR.
func (Kind) SecretRefPath() []string { return []string{"oauth", "secretRef", "name"} }

// BuildCredential ignores secretKey: an oauth credential has no single key.
func (Kind) BuildCredential(name, secretName, _ string) (spiceboxv1alpha1.AgentCredential, error) {
	return spiceboxv1alpha1.AgentCredential{
		Name:  name,
		Type:  "oauth",
		OAuth: &spiceboxv1alpha1.OAuthCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: secretName}},
	}, nil
}

// Resolve reads the Secret and, when the stored token has passed its
// expires_at, runs the just-in-time refresh once and re-reads.
//
// Returns the zero time as the expiry: the value's own expires_at has already
// been enforced by ReadStoredValue, and the broker bounds the cache entry so
// that every re-resolve re-applies that gate.
func (k Kind) Resolve(ctx context.Context, deps credkind.Deps, src spiceboxv1alpha1.CredentialSource) (authkind.ResolvedCredential, time.Time, error) {
	cred, err := credresolve.AgentCredentialFromSource(src)
	if err != nil {
		return authkind.ResolvedCredential{}, time.Time{}, err
	}

	val, err := k.ReadStoredValue(ctx, deps.Client, src.Namespace, cred)
	if err == nil {
		return authkind.ResolvedCredential{AccessToken: val}, time.Time{}, nil
	}
	if !errors.Is(err, credresolve.ErrExpired) {
		return authkind.ResolvedCredential{}, time.Time{}, err
	}

	if rerr := refresh.Run(ctx, deps.Client, src.Namespace, cred); rerr != nil {
		// Report BOTH: the refresh failure explains why we could not recover,
		// and the original expiry explains why we tried.
		return authkind.ResolvedCredential{}, time.Time{},
			fmt.Errorf("jit refresh failed: %w (original: %w)", rerr, err)
	}
	val, err = k.ReadStoredValue(ctx, deps.Client, src.Namespace, cred)
	if err != nil {
		return authkind.ResolvedCredential{}, time.Time{}, err
	}
	return authkind.ResolvedCredential{AccessToken: val}, time.Time{}, nil
}

// ReadStoredValue reads the Secret and gates on expires_at — past expiration
// returns credresolve.ErrExpired without exposing token bytes, unwrapped, so
// Resolve's errors.Is(err, credresolve.ErrExpired) check above keeps working.
// credresolve.ResolveSecretValue dispatches to this through the registry
// instead of switching on cred.Type itself.
func (Kind) ReadStoredValue(ctx context.Context, c client.Reader, ns string, cred spiceboxv1alpha1.AgentCredential) (sensitive.SensitiveValue, error) {
	if cred.OAuth == nil {
		return sensitive.SensitiveValue{}, fmt.Errorf("credresolve: %q has type=oauth but oauth block is nil", cred.Name)
	}
	sec, err := credresolve.GetSecret(ctx, c, ns, cred.OAuth.SecretRef.Name)
	if err != nil {
		return sensitive.SensitiveValue{}, err
	}
	if exp, ok := sec.Data["expires_at"]; ok && len(exp) > 0 {
		t, perr := time.Parse(time.RFC3339, string(exp))
		if perr != nil {
			return sensitive.SensitiveValue{}, fmt.Errorf("credresolve: %q expires_at not RFC3339: %w", cred.Name, perr)
		}
		if !t.After(time.Now()) {
			return sensitive.SensitiveValue{}, credresolve.ErrExpired
		}
	}
	at, ok := sec.Data["access_token"]
	if !ok {
		return sensitive.SensitiveValue{}, fmt.Errorf("%w: %s/%s key=access_token",
			credresolve.ErrSecretKeyMissing, ns, cred.OAuth.SecretRef.Name)
	}
	if len(at) == 0 {
		return sensitive.SensitiveValue{}, fmt.Errorf("%w: %s/%s key=access_token",
			credresolve.ErrSecretValueEmpty, ns, cred.OAuth.SecretRef.Name)
	}
	return sensitive.NewSensitiveValue(at), nil
}
