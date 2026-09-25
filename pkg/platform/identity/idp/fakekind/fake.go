// Package fakekind registers an in-process idp kind for tests and the e2e
// harness. Begin returns "<issuer>/authorize?state=<state>"; Complete requires
// Code == "fake-code" (asserting code threading) and returns the
// package-configured principal.
//
// NextPrincipal, NextTokenSet and NextErr are package-level test seams set before
// calling Complete, so tests sharing this package must not run in parallel.
package fakekind

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/registry"
)

// NextPrincipal is the Principal Complete() returns on success.
// Tests set this before invoking the flow.
var NextPrincipal identity.Principal

// NextTokenSet, when non-nil, is the *idp.TokenSet Complete() returns alongside
// NextPrincipal. Nil by default; tests exercising the federated-login path set it
// before invoking the flow.
var NextTokenSet *idp.TokenSet

// NextErr, when non-nil, is returned by Complete() instead of NextPrincipal.
// Tests set this to simulate IdP errors. Reset to nil after use.
var NextErr error

func init() {
	registry.Register(&Kind{})
}

// Kind is the fake idp.Kind.
type Kind struct{}

func (k *Kind) Name() string { return "fake" }

func (k *Kind) New(_ context.Context, cfg idp.Config) (idp.Provider, error) {
	return &provider{cfg: cfg}, nil
}

func (k *Kind) Wizard() idp.Wizard {
	return idp.UnavailableWizard("the fake identity provider is built by tests, not by `oap idp setup`")
}

// ValidateSpec has no test-kind-specific rule; always valid.
func (k *Kind) ValidateSpec(_ spiceboxv1alpha1.ClusterIdentityProviderSpec) string {
	return ""
}

// DiscoveryURL is "": the fake kind is in-process only and never talks to a
// remote issuer.
func (k *Kind) DiscoveryURL(_ spiceboxv1alpha1.ClusterIdentityProviderSpec) string {
	return ""
}

// AllowedNonLocal is true: the fake kind exists only for tests and the e2e
// harness, never a real deployment, so it is not a credential surface at all.
func (k *Kind) AllowedNonLocal() bool { return true }

// provider implements idp.Provider for the fake kind.
type provider struct {
	cfg idp.Config
}

func (p *provider) Begin(_ context.Context, state string) (string, error) {
	return p.cfg.Issuer + "/authorize?state=" + url.QueryEscape(state), nil
}

func (p *provider) Complete(_ context.Context, cb idp.CallbackParams) (identity.Principal, *idp.TokenSet, error) {
	if NextErr != nil {
		return identity.Principal{}, nil, NextErr
	}
	if cb.Error != "" {
		return identity.Principal{}, nil, fmt.Errorf("idp reported error: %s", cb.Error)
	}
	if cb.Code != "fake-code" {
		return identity.Principal{}, nil, errors.New("unexpected code")
	}
	return NextPrincipal, NextTokenSet, nil
}
