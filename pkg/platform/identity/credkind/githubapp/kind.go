// This file implements the credkind.Kind for type=githubApp: a credential
// minted on demand from a stored GitHub App private key rather than read
// directly from a Secret.
//
// This is the exact mirror of type=federated: a federated credential is
// minted for a HUMAN subject and is rejected on an AgentIdentity, while a
// GitHub App has no human subject at all and is valid ONLY on an
// AgentIdentity. Both share the same shape (Minted, no stored value, no
// NeedsRefresh) but sit on opposite sides of ValidOn.
package githubapp

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

func init() { registry.Register(Kind{}) }

// Kind is the type=githubApp driver.
type Kind struct{}

var _ credkind.Kind = Kind{}

func (Kind) Type() string        { return "githubApp" }
func (Kind) Minted() bool        { return true }
func (Kind) DisplayName() string { return "GitHub App installation" }

// NeedsRefresh is false: nothing stored expires, because nothing is stored —
// a fresh installation token is minted on every Resolve instead.
func (Kind) NeedsRefresh() bool { return false }

// Projectable is false: this type's Secret holds multi-key App material
// (app-id/private-key/installation-id), not a single value a per-session
// projected Secret could key by credential name.
func (Kind) Projectable() bool { return false }

// RequiredSecretKeys is the three keys Resolve reads to mint a token.
// webhook-secret is deliberately excluded: it is consumed by the GitHub
// channel kind's webhook verification, not by credential resolution, so
// requiring it here would make an otherwise-valid AgentIdentity invalid
// whenever its Secret predates the webhook.
func (Kind) RequiredSecretKeys(c spiceboxv1alpha1.AgentCredential) []string {
	if c.GitHubApp == nil {
		return nil
	}
	return []string{"app-id", "private-key", "installation-id"}
}

// PublicSecretKeys is app-id and installation-id. Both are identifiers GitHub
// itself publishes, and neither authenticates anything on its own:
//
//   - app-id is the App's own numeric id. It is in the URL of the App's public
//     settings page, and it is one of the two halves of a JWT this kind signs —
//     the half that is not the secret.
//   - installation-id names one installation of that App. GitHub puts it in
//     the body of every webhook delivery it sends, so it is inherent to the
//     payload rather than something a holder of it could keep private.
//
// private-key and webhook-secret are deliberately ABSENT: the first signs the
// App JWT and the second authenticates every inbound delivery. Adding either
// here would tell a consumer that a live credential is safe to write down.
func (Kind) PublicSecretKeys(c spiceboxv1alpha1.AgentCredential) []string {
	if c.GitHubApp == nil {
		return nil
	}
	return []string{"app-id", "installation-id"}
}

// ValidOn is AgentIdentity only — the exact inverse of federated. A GitHub
// App has no human subject at all, so a user-scoped identity could never own
// one.
func (Kind) ValidOn() []credkind.Scope {
	return []credkind.Scope{credkind.ScopeAgentIdentity}
}

func (Kind) ValidateSpec(c spiceboxv1alpha1.AgentCredential) error {
	if c.GitHubApp == nil || c.GitHubApp.SecretRef.Name == "" {
		return fmt.Errorf("credentials[%s].githubApp: secretRef.name required", c.Name)
	}
	return nil
}

// HasBlock reports whether the githubApp block is populated. Sibling
// exclusivity is registry.ValidateExclusive's job off this answer — and it is
// the answer the other three kinds could not give about THIS one, which is why
// all three accepted a stray githubApp block until the rule moved.
func (Kind) HasBlock(c spiceboxv1alpha1.AgentCredential) bool { return c.GitHubApp != nil }

// SecretRef returns the Secret with an EMPTY key: the App material is a
// fixed multi-key shape (app-id/private-key/installation-id), not a single
// named value.
func (Kind) SecretRef(c spiceboxv1alpha1.AgentCredential) *credkind.SecretRef {
	if c.GitHubApp == nil {
		return nil
	}
	return &credkind.SecretRef{Name: c.GitHubApp.SecretRef.Name}
}

// SecretRefPath is where SecretRef above reads the Secret NAME from, spelled
// as a field path for callers rewriting an unstructured CR. This is the
// answer `oap agent install --name` was missing: without it the bundle's
// githubApp credentials kept pointing at the un-prefixed Secret name, which
// the prefixed install never creates.
func (Kind) SecretRefPath() []string { return []string{"githubApp", "secretRef", "name"} }

// BuildCredential always errors: a githubApp credential's Secret is written
// by the GitHub channel wizard, not by the `oap identity` setup flow.
func (Kind) BuildCredential(name, _, _ string) (spiceboxv1alpha1.AgentCredential, error) {
	return spiceboxv1alpha1.AgentCredential{}, fmt.Errorf(
		"credential %q: type=githubApp's Secret is written by the channel wizard, not the setup flow", name)
}

// Resolve reads the App material from the Secret at src.Namespace/src.Name
// and mints a fresh installation access token via deps.GitHubApp.
//
// Returns the minter's expiry as-is. The zero-expiry fail-closed check is
// deliberately NOT here — it lives at the broker's single choke point
// (pkg/platform/identity/broker/inproc), so it is enforced once for every
// minted kind rather than trusted to each implementation.
func (Kind) Resolve(ctx context.Context, deps credkind.Deps, src spiceboxv1alpha1.CredentialSource) (authkind.ResolvedCredential, time.Time, error) {
	if deps.GitHubApp == nil {
		return authkind.ResolvedCredential{}, time.Time{},
			fmt.Errorf("githubApp credential %s/%s but no GitHub App minter configured", src.Namespace, src.Name)
	}

	sec, err := credresolve.GetSecret(ctx, deps.Client, src.Namespace, src.Name)
	if err != nil {
		return authkind.ResolvedCredential{}, time.Time{}, err
	}
	appID, err := requiredSecretValue(sec, src.Namespace, src.Name, "app-id")
	if err != nil {
		return authkind.ResolvedCredential{}, time.Time{}, err
	}
	privateKey, err := requiredSecretValue(sec, src.Namespace, src.Name, "private-key")
	if err != nil {
		return authkind.ResolvedCredential{}, time.Time{}, err
	}
	installationID, err := requiredSecretValue(sec, src.Namespace, src.Name, "installation-id")
	if err != nil {
		return authkind.ResolvedCredential{}, time.Time{}, err
	}

	token, exp, err := deps.GitHubApp.Mint(ctx, string(appID), privateKey, string(installationID))
	if err != nil {
		return authkind.ResolvedCredential{}, time.Time{}, fmt.Errorf("githubApp %s/%s mint: %w", src.Namespace, src.Name, err)
	}
	return authkind.ResolvedCredential{AccessToken: token}, exp, nil
}

// ReadStoredValue always errors: a githubApp credential is minted per
// resolve from stored App material, never itself read as a value. The
// Secret it points at holds inputs to a mint, not a credential value.
func (Kind) ReadStoredValue(_ context.Context, _ client.Reader, _ string, cred spiceboxv1alpha1.AgentCredential) (sensitive.SensitiveValue, error) {
	return sensitive.SensitiveValue{}, fmt.Errorf(
		"credential %q: type=githubApp is minted on demand and has no stored value to read", cred.Name)
}

// requiredSecretValue reads key from sec, naming the missing/empty key in
// the returned error (rather than failing opaquely) using the same
// ErrSecretKeyMissing/ErrSecretValueEmpty sentinels static and oauth use, so
// callers can distinguish the two failure modes uniformly across types.
func requiredSecretValue(sec *corev1.Secret, ns, secretName, key string) ([]byte, error) {
	val, ok := sec.Data[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s key=%q", credresolve.ErrSecretKeyMissing, ns, secretName, key)
	}
	if len(val) == 0 {
		return nil, fmt.Errorf("%w: %s/%s key=%q", credresolve.ErrSecretValueEmpty, ns, secretName, key)
	}
	return val, nil
}
