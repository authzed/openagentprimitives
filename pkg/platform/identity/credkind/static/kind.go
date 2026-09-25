// Package static implements the credkind.Kind for type=static: a single value
// read from one named key of one Secret. It never expires and is never minted,
// which is why it is the only type whose value the operator can pin into a
// token grant at reconcile time.
package static

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
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

func init() { registry.Register(Kind{}) }

// Kind is the type=static driver.
type Kind struct{}

var _ credkind.Kind = Kind{}

func (Kind) Type() string        { return "static" }
func (Kind) Minted() bool        { return false }
func (Kind) NeedsRefresh() bool  { return false }
func (Kind) DisplayName() string { return "Static token" }

// Projectable is true: a static credential is a single value under one
// named key, exactly the shape a per-session projected Secret (keyed by
// credential name) needs.
func (Kind) Projectable() bool { return true }

// RequiredSecretKeys is the credential's own single named key. nil when the
// static block is absent — a malformed spec ValidateSpec already rejects,
// but this must still fail closed rather than dereference a nil block.
func (Kind) RequiredSecretKeys(c spiceboxv1alpha1.AgentCredential) []string {
	if c.Static == nil {
		return nil
	}
	return []string{c.Static.SecretRef.Key}
}

// PublicSecretKeys is nil: a static credential's Secret holds exactly one key
// and that key IS the credential. There is nothing here that is public.
func (Kind) PublicSecretKeys(spiceboxv1alpha1.AgentCredential) []string { return nil }

// ValidOn is every identity CR: a stored token is equally meaningful for a bot
// and for a human's passthrough credential.
func (Kind) ValidOn() []credkind.Scope {
	return []credkind.Scope{
		credkind.ScopeAgentIdentity,
		credkind.ScopeUserIdentity,
		credkind.ScopeSessionUserIdentity,
	}
}

func (Kind) ValidateSpec(c spiceboxv1alpha1.AgentCredential) error {
	if c.Static == nil || c.Static.SecretRef.Name == "" || c.Static.SecretRef.Key == "" {
		return fmt.Errorf("credentials[%s].static: secretRef must set both name and key", c.Name)
	}
	return nil
}

// HasBlock reports whether the static block is populated. Sibling exclusivity
// is registry.ValidateExclusive's job off this answer, not a list of the other
// kinds' blocks written out here — see credkind.Kind.HasBlock.
func (Kind) HasBlock(c spiceboxv1alpha1.AgentCredential) bool { return c.Static != nil }

// SecretRef reports both the Secret and the key, because a static value lives
// under one named key — which is what lets a caller check that the key exists.
// A malformed credential returns nil rather than a half-populated ref;
// ValidateSpec is what reports the malformation.
func (Kind) SecretRef(c spiceboxv1alpha1.AgentCredential) *credkind.SecretRef {
	if c.Static == nil {
		return nil
	}
	return &credkind.SecretRef{Name: c.Static.SecretRef.Name, Key: c.Static.SecretRef.Key}
}

// SecretRefPath is where SecretRef above reads the Secret NAME from, spelled
// as a field path for callers rewriting an unstructured CR.
func (Kind) SecretRefPath() []string { return []string{"static", "secretRef", "name"} }

func (Kind) BuildCredential(name, secretName, secretKey string) (spiceboxv1alpha1.AgentCredential, error) {
	return spiceboxv1alpha1.AgentCredential{
		Name: name,
		Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secretName, Key: secretKey},
		},
	}, nil
}

// Resolve reads the Secret value. A static credential carries no expiry, so it
// returns the zero time and the broker bounds the cache entry itself — which is
// what makes a credential withdrawn without an explicit revoke stop being
// served within the broker's TTL rather than never.
func (k Kind) Resolve(ctx context.Context, deps credkind.Deps, src spiceboxv1alpha1.CredentialSource) (authkind.ResolvedCredential, time.Time, error) {
	cred, err := credresolve.AgentCredentialFromSource(src)
	if err != nil {
		return authkind.ResolvedCredential{}, time.Time{}, err
	}
	val, err := k.ReadStoredValue(ctx, deps.Client, src.Namespace, cred)
	if err != nil {
		return authkind.ResolvedCredential{}, time.Time{}, err
	}
	return authkind.ResolvedCredential{AccessToken: val}, time.Time{}, nil
}

// ReadStoredValue reads the Secret value the credential's static block
// points at. credresolve.ResolveSecretValue dispatches to this through the
// registry instead of switching on cred.Type itself.
func (Kind) ReadStoredValue(ctx context.Context, c client.Reader, ns string, cred spiceboxv1alpha1.AgentCredential) (sensitive.SensitiveValue, error) {
	if cred.Static == nil {
		return sensitive.SensitiveValue{}, fmt.Errorf("credresolve: %q has type=static but static block is nil", cred.Name)
	}
	sec, err := credresolve.GetSecret(ctx, c, ns, cred.Static.SecretRef.Name)
	if err != nil {
		return sensitive.SensitiveValue{}, err
	}
	val, ok := sec.Data[cred.Static.SecretRef.Key]
	if !ok {
		return sensitive.SensitiveValue{}, fmt.Errorf("%w: %s/%s key=%q",
			credresolve.ErrSecretKeyMissing, ns, cred.Static.SecretRef.Name, cred.Static.SecretRef.Key)
	}
	if len(val) == 0 {
		return sensitive.SensitiveValue{}, fmt.Errorf("%w: %s/%s key=%q",
			credresolve.ErrSecretValueEmpty, ns, cred.Static.SecretRef.Name, cred.Static.SecretRef.Key)
	}
	return sensitive.NewSensitiveValue(val), nil
}
