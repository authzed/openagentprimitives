// Package onepassword_scim is the builtin Go flow for the onepassword-scim
// provider. Points the user at where their SCIM bridge's bearer token was
// issued, captures the pasted value, and stores it.
//
// As a screen sequence:
//
//	browser   open 1Password's SCIM bridge setup documentation
//	token     say which of the two setup artifacts to paste, take it
//
// The bridge's own ADDRESS is deliberately not collected here. It is
// per-install configuration (spec.baseURL on the RelationshipSource) rather
// than part of the credential, the CLI wizard that reaches this flow asks for
// it on its own screen, and a second copy asked for here is how the two come
// to disagree.
package onepassword_scim

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/flowscreens"
	"github.com/authzed/openagentprimitives/pkg/x/credmask"
)

// KeyToken is the State key the pasted token lands under. Stable: a caller
// answering this flow ahead of time addresses the screen by it.
const KeyToken = "token"

// setupPage is 1Password's own automated-provisioning setup documentation.
//
// Unlike the other bearer flows this is NOT a page the credential is minted
// on: the bridge's bearer token is issued once, at deployment time, alongside
// the scimsession file, so there is no "generate" button to send anyone to.
// The page is where an operator finds out where their own deployment put it,
// which is the only thing this flow can usefully open.
const setupPage = "https://support.1password.com/scim/"

// Flow is the onepassword-scim builtin.
type Flow struct {
	// open opens a page in the operator's browser.
	//
	// INJECTED, not read from a package-level hook. The difference is which
	// path you get by default: with a global hook plus a setter, the safe path
	// is opt-in and a test that merely forgets to opt out launches a browser on
	// the machine running the suite. Here a caller must hand over an opener to
	// have one at all, so a test cannot reach a real browser without asking for
	// it. init() passes browser.Open; tests pass a recorder.
	//
	// Nil is tolerated: flowscreens.Browser reports that this build cannot open
	// a browser, and the screen after it repeats the address in its guidance.
	open func(string) error
}

// New returns a Flow that opens pages with open. Registration passes
// browser.Open (init.go); a test passes its own func.
func New(open func(string) error) *Flow { return &Flow{open: open} }

// Name returns the registry name for this flow.
func (*Flow) Name() string { return "onepassword-scim" }

// Screens describes the flow: open the setup documentation, then take the
// token it describes. The browser step is a screen of its own because it is
// work rather than a question — which lets a run that cannot prompt refuse it
// up front instead of opening a tab and failing at the screen after it.
//
// req.Provider.Prompt is LLM-system-prompt content for the fallback agent. Do
// NOT show it to the user; the guidance below is composed from structured
// fields.
func (f *Flow) Screens(_ context.Context, req builtins.Request) ([]tui.Screen, error) {
	page := flowscreens.NewBrowser(flowscreens.BrowserOpts{
		ID:        "browser",
		Label:     "Browser",
		URL:       setupPage,
		Guard:     KeyToken,
		Open:      f.open,
		NoteLabel: "Browser",
	})

	return []tui.Screen{
		page,
		tui.NewText(tui.TextOpts{
			QuestionOpts: tui.QuestionOpts{
				ID:        "token",
				Label:     "Token",
				Key:       KeyToken,
				Title:     "Paste the SCIM bridge bearer token",
				Guidance:  func(*tui.State) string { return guidance(req, page.Err()) },
				Addresses: []tui.Address{flowscreens.DocsAddress(req.Provider)},
				NoteLabel: "1Password SCIM token",
				NoteValue: credmask.Mask,
			},
			Check: flowscreens.TokenShapeCheck(req.Provider),
		}),
	}, nil
}

// Result stores the pasted token as a bearer credential.
//
// The emptiness check is the ONLY gate on this flow's answer, which is why it
// is spelled out rather than left to the provider's format check: 1Password
// documents no stable shape for this token, so the provider declares no
// tokenShape and provider.ValidateToken — deliberately permissive about a
// format we do not know — accepts anything, including "". And huh's accessible
// renderer has no error channel, so an input that runs out arrives here as an
// unanswered State and a nil error, which without this check would store a
// credential authenticating as nobody behind a CLI that reported success.
func (*Flow) Result(ctx context.Context, req builtins.Request, st *tui.State) error {
	if st == nil {
		return errors.New("onepassword-scim: no token was supplied")
	}
	token := strings.TrimSpace(st.Get(KeyToken))
	if token == "" {
		return errors.New("onepassword-scim: no token was supplied")
	}
	if req.Provider != nil {
		if err := provider.ValidateToken(*req.Provider, token); err != nil {
			return fmt.Errorf("onepassword-scim: %w", err)
		}
	}
	if req.Store == nil {
		return errors.New("onepassword-scim: nowhere to store the token")
	}
	return req.Store(ctx, builtins.StoreValue{Bearer: token})
}

// Verify reports VerifyUnsupported: there is nowhere constant to probe.
//
// The bridge is customer-hosted, so the address this token authenticates
// against is per-install (spec.baseURL) rather than a catalog constant, and
// provider.VerifyConfig.Endpoint is catalog-supplied on purpose — never from
// input — so no declarative probe can be written for it. Saying so is the
// honest answer; a probe against a guessed host would confirm nothing.
func (*Flow) Verify(_ context.Context, _ builtins.VerifyRequest) (builtins.VerifyResult, error) {
	return builtins.VerifyResult{
		Status: builtins.VerifyUnsupported,
		Detail: "the SCIM bridge is customer-hosted, so there is no fixed endpoint to verify this token against",
	}, nil
}

// guidance is what the user reads above the field: which of the two artifacts
// their bridge setup produced is the credential, where to find it, and what
// this screen is NOT asking for — repeated in text because a browser that
// refused to open leaves the address as the only way through.
//
// Every line is kept inside the note's column budget, which this package's
// tests assert: the address has to survive being copied, and huh wraps an
// over-long line with no sign the halves belong together.
func guidance(req builtins.Request, browserErr error) string {
	var b strings.Builder
	if browserErr != nil {
		// The reason comes from the operating system and can be any length; on a
		// line of its own, a long one cannot drag the next instruction into a wrap.
		fmt.Fprintf(&b, "Your browser didn't open:\n  %s\nVisit the page below yourself.\n\n", browserErr)
	}
	fmt.Fprintf(&b, "SCIM bridge token for %q.\n\n", req.IdentityName)
	b.WriteString("Your bridge issued it at deployment time, next to\n")
	b.WriteString("the scimsession file. Find it in whichever secret\n")
	b.WriteString("your deployment reads.\n")
	b.WriteString("\nPaste the BEARER TOKEN only:\n")
	b.WriteString("  - not the scimsession file, which is a\n")
	b.WriteString("    deployment artifact, not a credential\n")
	b.WriteString("  - not the bridge's address, which is asked\n")
	b.WriteString("    for on its own\n")
	// Printed rather than dropped. A SCIM bridge token carries no per-use
	// scope to narrow, so no shipped caller supplies these — but a caller that
	// did would otherwise have them silently discarded, which is how a person
	// ends up granting something nobody told them to grant.
	if len(req.ScopeHint) > 0 {
		b.WriteString("\nThe token needs:\n")
		for _, line := range req.ScopeHint {
			b.WriteString("  " + line + "\n")
		}
	}
	b.WriteString("\nPage: " + setupPage + "\n")
	return strings.TrimRight(b.String(), "\n")
}
