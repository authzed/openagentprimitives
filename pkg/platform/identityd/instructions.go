// pkg/platform/identityd/instructions.go — resolve a passthrough credential's
// user-facing setup instructions for the enter-token UIs.
//
// A toolkit-backed credential names its provider in the toolkit env var's
// `provider:` field (GITHUB_TOKEN → github-pat), so credential name → provider
// id comes from walking the embedded toolkit catalog; the provider then
// supplies Instructions/DocsURL.
package identityd

import (
	"context"
	"errors"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
)

// credInstructions returns the user-facing instructions and docs URL for a
// credential, resolved via its provider in the embedded toolkit catalog.
// Returns empty strings when the credential maps to no provider (or the
// provider declares no instructions) — callers render nothing in that case.
func credInstructions(credName string) (instructions, docsURL string) {
	providerID := providerForCredential(credName)
	if providerID == "" {
		return "", ""
	}
	p, ok := provider.ByID(providerID)
	if !ok {
		return "", ""
	}
	return p.Instructions, p.DocsURL
}

// providerForCredential maps a credential name to its provider id. A thin alias
// over the shared resolver, so identityd and channelsd cannot disagree on the
// derivation.
func providerForCredential(credName string) string {
	return passthroughcatalog.ProviderIDFromToolkits(credName)
}

// validatePastedToken rejects a token that doesn't match the format its
// provider declares, BEFORE storing it, so a wrong paste is an inline error at
// the form rather than an opaque 401 when the agent later uses the credential.
// Permissive by design: a credential resolving to no provider, or to one
// declaring no format, is never blocked.
func (s *Server) validatePastedToken(ctx context.Context, credName, token string) error {
	p, ok := passthroughcatalog.ProviderForCredential(ctx, s.deps.K8s, credName)
	if !ok {
		return nil
	}
	return provider.ValidateToken(*p, token)
}

// verifyPastedToken live-checks a pasted token via the credential's
// provider. Runs AFTER validatePastedToken's format gate. Outcomes:
// Valid → store; Indeterminate → log + store (warn notice); Unsupported →
// log + store quietly (linked, no warning); Rejected and Forbidden → the
// handler renders the confirm page instead of storing, and so does any
// verdict it does not recognise.
func (s *Server) verifyPastedToken(ctx context.Context, credName, token string) builtins.VerifyResult {
	p, ok := passthroughcatalog.ProviderForCredential(ctx, s.deps.K8s, credName)
	if !ok {
		return builtins.VerifyResult{Status: builtins.VerifyUnsupported, Detail: "no provider associated with credential " + credName}
	}
	return builtins.VerifyCredential(ctx, p, builtins.StoreValue{Bearer: token})
}

// tokenRejectMessage renders a user-facing sentence for a rejected paste. A
// *provider.TokenFormatError carries the expected-format hint, which we splice
// in so the user knows what a correct token looks like; anything else gets a
// generic-but-actionable fallback.
func tokenRejectMessage(err error) string {
	var fe *provider.TokenFormatError
	if errors.As(err, &fe) && fe.Hint != "" {
		return "That doesn't look like the right credential. Expected " + fe.Hint +
			". Double-check you pasted the correct token, then try again."
	}
	return "That value doesn't match the expected format for this credential. " +
		"Double-check you pasted the correct token, then try again."
}
