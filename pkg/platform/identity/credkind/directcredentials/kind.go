// This file implements the credkind.Kind for type=directCredentials: a
// website login the browser toolbox's form filler fetches at the moment it
// fills a page, never resolved into an injectable env var or HTTP header.
//
// Every other credkind.Kind ends at Resolve producing a value the broker
// hands to a tool call or an MCP server. This one is the deliberate
// exception: Resolve errors unconditionally, because a directCredentials
// value must never reach the broker's cache, a tool's env/header, or a log
// line — only the browser toolbox's filler, reading directly from the
// backend Source names, at the instant it types the value into a page. This
// package is the CRD-anchoring half only: it validates the spec and points
// at the backing Secret. The fetch itself belongs to
// pkg/platform/identity/directcred, called from the filler — never from
// here.
package directcredentials

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

func init() { registry.Register(Kind{}) }

// Kind is the type=directCredentials driver.
type Kind struct{}

var _ credkind.Kind = Kind{}

func (Kind) Type() string        { return "directCredentials" }
func (Kind) Minted() bool        { return true }
func (Kind) NeedsRefresh() bool  { return false }
func (Kind) DisplayName() string { return "Direct credentials (website logins)" }

// Projectable is false: a directCredentials value never becomes a stored
// value at all, so there is nothing to project into a per-session Secret
// keyed by credential name. See Resolve.
func (Kind) Projectable() bool { return false }

// ValidOn is AgentIdentity only: the login is authored directly on the bot's
// own identity, not passed through from a human subject the way a
// UserIdentity/SessionUserIdentity credential is.
func (Kind) ValidOn() []credkind.Scope {
	return []credkind.Scope{credkind.ScopeAgentIdentity}
}

// ValidateSpec checks the directCredentials block's own shape: Source must
// be a recognized backend, SecretRef.Name must be set, and Vault is required
// for source=onepassword and forbidden for source=secret. SecretRef.Key is
// similarly backend-shaped: "" or "token" for onepassword (its Connect token
// lives under one named key) and "" only for secret (its login Secret is a
// multi-key shape with no single named value key).
func (Kind) ValidateSpec(c spiceboxv1alpha1.AgentCredential) error {
	if c.DirectCredentials == nil {
		return fmt.Errorf("credentials[%s].directCredentials: block required for type=directCredentials", c.Name)
	}
	dc := c.DirectCredentials

	// Like every sibling ValidateSpec, the messages below name the offending
	// FIELD (and the credential), never the field's VALUE: they are surfaced
	// verbatim on a SpecInvalid condition, and the convention is uniform
	// across kinds.
	switch dc.Source {
	case directcred.SourceOnePassword, directcred.SourceSecret:
	default:
		return fmt.Errorf("credentials[%s].directCredentials: source must be %q or %q",
			c.Name, directcred.SourceOnePassword, directcred.SourceSecret)
	}

	if dc.SecretRef.Name == "" {
		return fmt.Errorf("credentials[%s].directCredentials: secretRef.name required", c.Name)
	}

	switch dc.Source {
	case directcred.SourceOnePassword:
		if dc.Vault == "" {
			return fmt.Errorf("credentials[%s].directCredentials: vault required when source=%s",
				c.Name, directcred.SourceOnePassword)
		}
		if dc.SecretRef.Key != "" && dc.SecretRef.Key != "token" {
			return fmt.Errorf("credentials[%s].directCredentials: secretRef.key must be \"token\" (or empty) when source=%s",
				c.Name, directcred.SourceOnePassword)
		}
	case directcred.SourceSecret:
		if dc.Vault != "" {
			return fmt.Errorf("credentials[%s].directCredentials: vault must be empty when source=%s",
				c.Name, directcred.SourceSecret)
		}
		if dc.SecretRef.Key != "" {
			return fmt.Errorf("credentials[%s].directCredentials: secretRef.key must be empty when source=%s",
				c.Name, directcred.SourceSecret)
		}
	}
	return nil
}

// HasBlock reports whether the directCredentials block is populated. Sibling
// exclusivity is registry.ValidateExclusive's job off this answer, not a
// list of the other kinds' blocks written out here — see
// credkind.Kind.HasBlock.
func (Kind) HasBlock(c spiceboxv1alpha1.AgentCredential) bool { return c.DirectCredentials != nil }

// SecretRef returns the backend's backing Secret. onepassword's Connect
// token lives under one named key ("token"); secret's login Secret is a
// multi-key shape, so its Key is empty — the same shape githubApp's own
// multi-key SecretRef reports.
func (Kind) SecretRef(c spiceboxv1alpha1.AgentCredential) *credkind.SecretRef {
	if c.DirectCredentials == nil {
		return nil
	}
	key := ""
	if c.DirectCredentials.Source == directcred.SourceOnePassword {
		key = "token"
	}
	return &credkind.SecretRef{Name: c.DirectCredentials.SecretRef.Name, Key: key}
}

// SecretRefPath is where SecretRef above reads the Secret NAME from, spelled
// as a field path for callers rewriting an unstructured CR (`oap agent
// install --name <prefix>` relocating a bundled Secret).
func (Kind) SecretRefPath() []string { return []string{"directCredentials", "secretRef", "name"} }

// RequiredSecretKeys is the Connect token key for onepassword; nil for
// secret, whose login Secret has no single fixed key the generic checker in
// pkg/controllers/agentidentity could validate — that host-keyed shape is
// meaningful only to the secret backend's own Fetch.
func (Kind) RequiredSecretKeys(c spiceboxv1alpha1.AgentCredential) []string {
	if c.DirectCredentials == nil {
		return nil
	}
	if c.DirectCredentials.Source == directcred.SourceOnePassword {
		return []string{"token"}
	}
	return nil
}

// PublicSecretKeys is nil, unconditionally: fail closed, every key is
// secret. Unlike a GitHub App's app-id/installation-id, nothing in a website
// login's backing Secret is an identifier published independently of the
// credential itself.
func (Kind) PublicSecretKeys(spiceboxv1alpha1.AgentCredential) []string { return nil }

// BuildCredential always errors: a directCredentials credential is authored
// directly on the identity — its Source/SecretRef/Vault are the spec itself
// — not written by the `oap identity` setup flow the way a static token is.
// The setup flow's only role for this type is filling its token Secret,
// never constructing the block.
func (Kind) BuildCredential(name, _, _ string) (spiceboxv1alpha1.AgentCredential, error) {
	return spiceboxv1alpha1.AgentCredential{}, fmt.Errorf(
		"credential %q: type=directCredentials is authored on the identity directly; the setup flow only fills its token Secret", name)
}

// Resolve always errors: a directCredentials value never becomes an
// injectable credential. It is fetched at use time, directly by the browser
// toolbox's form filler, and must never reach the broker's cache, a tool
// call's env var or header, or a log line.
func (Kind) Resolve(_ context.Context, _ credkind.Deps, _ spiceboxv1alpha1.CredentialSource) (authkind.ResolvedCredential, time.Time, error) {
	return authkind.ResolvedCredential{}, time.Time{}, fmt.Errorf(
		"directCredentials never project into env vars or headers; they are fetched at use time by the browser filler")
}

// ReadStoredValue always errors: this type has no single stored value to
// read the way a static or oauth credential does — Resolve's own doc
// explains why nothing here ever becomes a readable value at all.
func (Kind) ReadStoredValue(_ context.Context, _ client.Reader, _ string, cred spiceboxv1alpha1.AgentCredential) (sensitive.SensitiveValue, error) {
	return sensitive.SensitiveValue{}, fmt.Errorf(
		"credential %q: type=directCredentials is fetched at use time by the browser filler and has no stored value to read", cred.Name)
}
