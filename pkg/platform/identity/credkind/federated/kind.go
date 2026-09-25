// Package federated implements the credkind.Kind for type=federated: a
// credential minted per resolve via an ID-JAG token exchange rather than read
// from a Secret. No Secret holds the upstream token; the only Secret involved
// is the user's IdP-identity Secret, which supplies the subject material for
// the exchange and is never this credential's backing Secret.
package federated

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

func init() { registry.Register(Kind{}) }

// Kind is the type=federated driver.
type Kind struct{}

var _ credkind.Kind = Kind{}

func (Kind) Type() string        { return "federated" }
func (Kind) Minted() bool        { return true }
func (Kind) NeedsRefresh() bool  { return false }
func (Kind) DisplayName() string { return "Federated (ID-JAG)" }

// Projectable is false: nothing is stored for a minted type, so there is no
// value to project into a per-session Secret.
func (Kind) Projectable() bool { return false }

// RequiredSecretKeys is nil: nothing is stored for a federated credential —
// see SecretRef's own comment for why the IdP-identity Secret a mint reads
// is subject material, not this credential's backing Secret.
func (Kind) RequiredSecretKeys(spiceboxv1alpha1.AgentCredential) []string { return nil }

// PublicSecretKeys is nil for the same reason RequiredSecretKeys is: a
// federated credential has no backing Secret at all, so it has no data keys to
// call public or secret.
func (Kind) PublicSecretKeys(spiceboxv1alpha1.AgentCredential) []string { return nil }

// ValidOn is SessionUserIdentity only. AgentIdentity is excluded because a
// bot identity has no human subject to assert in the token exchange.
// UserIdentity is excluded too: the only producer of a type=federated
// credential, passthrough.go's BuildSessionUserIdentity, writes it directly
// onto a SessionUserIdentity, never onto a UserIdentity, and nothing in this
// repo validates a UserIdentity's credentials against ValidOn at all — so
// including UserIdentity here would silently accept a shape nothing ever
// produces or checks, rather than making that branch unreachable.
func (Kind) ValidOn() []credkind.Scope {
	return []credkind.Scope{credkind.ScopeSessionUserIdentity}
}

func (Kind) ValidateSpec(c spiceboxv1alpha1.AgentCredential) error {
	if c.Federated == nil {
		return fmt.Errorf("credentials[%s].federated: block required for type=federated", c.Name)
	}
	return nil
}

// HasBlock reports whether the federated block is populated. Sibling
// exclusivity is registry.ValidateExclusive's job off this answer, not a list
// of the other kinds' blocks written out here — see credkind.Kind.HasBlock.
func (Kind) HasBlock(c spiceboxv1alpha1.AgentCredential) bool { return c.Federated != nil }

// SecretRef returns nil: nothing is stored. The IdP-identity Secret the mint
// reads is the SUBJECT source, not this credential's backing Secret, and
// treating it as one would let a caller adopt or key-check the wrong object.
func (Kind) SecretRef(spiceboxv1alpha1.AgentCredential) *credkind.SecretRef { return nil }

// SecretRefPath is nil for the same reason SecretRef is: a federated
// credential is minted on demand and has no backing Secret of its own. (The
// IdP-identity Secret it reads subject material from is deliberately not this
// credential's store — see SecretRef's doc.)
func (Kind) SecretRefPath() []string { return nil }

// BuildCredential always errors: a federated credential is minted on demand
// and has no stored value for the setup flow to write.
func (Kind) BuildCredential(name, _, _ string) (spiceboxv1alpha1.AgentCredential, error) {
	return spiceboxv1alpha1.AgentCredential{}, fmt.Errorf(
		"credential %q: type=federated is minted on demand and has no stored value for the setup flow to write", name)
}

// Resolve performs the ID-JAG two-legged exchange: it resolves the user's
// IdP-identity Secret to fresh subject material, then mints an upstream
// access token scoped to src.Resource / src.ResourceServerURL / src.Scopes.
//
// Returns minted.ExpiresAt as-is, including a zero value. The zero-expiry
// fail-closed check is deliberately NOT here — it lives at the broker's
// single choke point, so it is enforced once for every minted kind rather
// than trusted to each implementation.
func (Kind) Resolve(ctx context.Context, deps credkind.Deps, src spiceboxv1alpha1.CredentialSource) (authkind.ResolvedCredential, time.Time, error) {
	if deps.Federation == nil {
		return authkind.ResolvedCredential{}, time.Time{},
			fmt.Errorf("federated credential %s/%s but no federation minter configured", src.Namespace, src.Name)
	}
	subj, err := credresolve.SubjectMaterial(ctx, deps.Client, src.Namespace, src.Name)
	if err != nil {
		return authkind.ResolvedCredential{}, time.Time{}, fmt.Errorf("federated %s: idp subject: %w", src.Name, err)
	}
	minted, err := deps.Federation.Mint(ctx, federation.MintRequest{
		Subject: subj, Resource: src.Resource, ResourceServerURL: src.ResourceServerURL, Scopes: src.Scopes,
	})
	if err != nil {
		return authkind.ResolvedCredential{}, time.Time{}, fmt.Errorf("federated %s mint: %w", src.Name, err)
	}
	return authkind.ResolvedCredential{AccessToken: minted.AccessToken}, minted.ExpiresAt, nil
}

// ReadStoredValue always errors: a federated credential is minted per resolve
// via an ID-JAG exchange, never read from a Secret. The only Secret involved
// (the user's IdP-identity Secret) supplies subject material for the mint —
// it is not this credential's backing store, so there is nothing to read.
func (Kind) ReadStoredValue(_ context.Context, _ client.Reader, _ string, cred spiceboxv1alpha1.AgentCredential) (sensitive.SensitiveValue, error) {
	return sensitive.SensitiveValue{}, fmt.Errorf(
		"credential %q: type=federated is minted on demand and has no stored value to read", cred.Name)
}
