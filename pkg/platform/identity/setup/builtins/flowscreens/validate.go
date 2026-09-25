package flowscreens

import (
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
)

// docsLabel is what a provider's documentation address is called, in both the
// guidance line and the summary line that carries it when a note cannot. Named
// once so the two cannot drift apart.
const docsLabel = "Docs"

// DocsAddress returns p's documentation address, suitable for
// tui.QuestionOpts.Address, or the zero Address when there is no provider to
// take one from. One nil check here rather than the same one in every flow.
func DocsAddress(p *provider.Provider) tui.Address {
	if p == nil {
		return tui.Address{}
	}
	return tui.Address{Label: docsLabel, URL: p.DocsURL}
}

// TokenShapeCheck returns the validator for a provider's declared token shape,
// suitable for tui.TextOpts.Check.
//
// It delegates to provider.ValidateToken, the single format gate the paste forms
// and the put-token CLI also go through, so a credential typed into a setup flow
// is held to the same declared format as one pasted anywhere else, and a wrong
// token is refused where the user can retype it rather than as an opaque 401 the
// first time the agent uses it.
//
// A nil provider, or one declaring no shape, accepts anything: never refuse a
// credential whose format we do not know. The State argument goes unused — a
// token's shape is a property of the token alone — but the signature is the
// shared one, so a check that DOES need an earlier answer needs no new plumbing.
func TokenShapeCheck(p *provider.Provider) func(*tui.State, string) error {
	return func(_ *tui.State, v string) error {
		if p == nil {
			return nil
		}
		return provider.ValidateToken(*p, v)
	}
}
