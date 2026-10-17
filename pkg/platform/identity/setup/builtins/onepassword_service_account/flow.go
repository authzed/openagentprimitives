// Package onepassword_service_account is the builtin Go flow for the
// onepassword-service-account provider. Points the user at 1Password's
// service-account docs, captures the pasted token, and stores it — then, on
// demand, verifies the token resolves to exactly one visible vault, which is
// the scoping invariant the browser toolbox's direct-credential path depends
// on (see pkg/platform/identity/directcred's doc comment).
//
// As a screen sequence:
//
//	browser   open 1Password's service-account documentation
//	token     say which scope to grant, take the pasted token
//
// Unlike github_pat, which collects a user-scoped PAT, this credential
// authenticates a SERVICE ACCOUNT and is deliberately scoped to read_items on
// exactly one vault. The vault's NAME is authored on the AgentIdentity's
// credential block, not asked for here — and VerifyRequest carries only the
// provider and the about-to-be-stored value, so this flow has no way to see
// it even if it wanted to. The name-match and id-pin happen controller-side,
// against live backend state, once the credential is stored.
package onepassword_service_account

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/flowscreens"
	"github.com/authzed/openagentprimitives/pkg/x/credmask"
)

// KeyToken is the State key the pasted token lands under. Stable: a caller
// answering this flow ahead of time addresses the screen by it.
const KeyToken = "token"

// setupPage is 1Password's own service-account documentation — the page that
// walks an operator through creating one and scoping it to a single vault.
// Matches providers/onepassword-service-account.yaml's docsURL.
const setupPage = "https://developer.1password.com/docs/service-accounts/"

// verifyFunc resolves a bearer token to the vault it is scoped to, via the
// onepassword directcred.Source. Injected so Flow.Verify never makes a
// network call in a test: New wires defaultVerify (the real registry lookup);
// NewWithVerify takes a test double.
type verifyFunc func(ctx context.Context, token string) (directcred.Pin, error)

// Flow is the onepassword-service-account builtin.
type Flow struct {
	// open opens a page in the operator's browser. INJECTED, not read from a
	// package-level hook — see onepassword_scim.Flow's doc comment for why.
	// init.go passes browser.Open; tests pass a recorder.
	open func(string) error

	// verify resolves a token to its vault. Defaults to defaultVerify, which
	// dispatches through the directcred registry to the real onepassword
	// Source — the binary must blank-import
	// pkg/platform/identity/directcred/onepassword for that lookup to
	// succeed (cmd/oap/main.go does; see that file's import block).
	verify verifyFunc
}

// New returns a Flow that opens pages with open and verifies against the real
// onepassword directcred.Source.
func New(open func(string) error) *Flow {
	return &Flow{open: open, verify: defaultVerify}
}

// NewWithVerify returns a Flow whose Verify calls verify instead of the real
// source lookup. Test-only seam: it is how Flow.Verify is exercised without
// ever reaching the network or the 1Password SDK.
func NewWithVerify(open func(string) error, verify verifyFunc) *Flow {
	if verify == nil {
		verify = defaultVerify
	}
	return &Flow{open: open, verify: verify}
}

// defaultVerify is verify's production value: look up the registered
// "onepassword" directcred.Source and run its Verify with the token as the
// service-account credential. No vault name is passed — this flow never has
// one to pass (see the package doc comment) — so a cfg.Vault mismatch branch
// in the source never triggers here; only the "how many vaults are visible"
// check does.
func defaultVerify(ctx context.Context, token string) (directcred.Pin, error) {
	src, ok := directcred.Get(directcred.SourceOnePassword)
	if !ok {
		return directcred.Pin{}, errors.New(
			"onepassword-service-account: no onepassword directcred source registered " +
				"(the binary is missing its blank import of pkg/platform/identity/directcred/onepassword)")
	}
	return src.Verify(ctx, directcred.Config{SecretData: map[string][]byte{"token": []byte(token)}})
}

// Name returns the registry name for this flow.
func (*Flow) Name() string { return "onepassword-service-account" }

// Screens describes the flow: open the service-account documentation, then
// take the token it describes. The browser step is a screen of its own
// because it is work rather than a question — which lets a run that cannot
// prompt refuse it up front instead of opening a tab and failing at the
// screen after it.
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
				Title:     "Paste the service-account token",
				Guidance:  func(*tui.State) string { return guidance(req, page.Err()) },
				Addresses: []tui.Address{flowscreens.DocsAddress(req.Provider)},
				NoteLabel: "1Password service-account token",
				NoteValue: credmask.Mask,
			},
			Check: flowscreens.TokenShapeCheck(req.Provider),
		}),
	}, nil
}

// Result stores the pasted token as a bearer credential.
//
// The format check runs through provider.ValidateToken against the catalog's
// declared tokenShape (ops_...) before the emptiness guard even matters in
// practice, but the guard stays: huh's accessible renderer cannot report a
// read error, so an input script that runs out reaches here as an unanswered
// State and a nil error, which without this check would store a credential
// authenticating as nobody behind a CLI that reported success.
func (*Flow) Result(ctx context.Context, req builtins.Request, st *tui.State) error {
	if st == nil {
		return errors.New("onepassword-service-account: no token was supplied")
	}
	token := strings.TrimSpace(st.Get(KeyToken))
	if token == "" {
		return errors.New("onepassword-service-account: no token was supplied")
	}
	if req.Provider != nil {
		if err := provider.ValidateToken(*req.Provider, token); err != nil {
			return fmt.Errorf("onepassword-service-account: %w", err)
		}
	}
	if req.Store == nil {
		return errors.New("onepassword-service-account: nowhere to store the token")
	}
	return req.Store(ctx, builtins.StoreValue{Bearer: token})
}

// vaultCountRe extracts the vault count from onepassword.Source.Verify's
// exact failure message for its scoping invariant (source.go: "expected
// exactly one accessible vault, found %d: %v"). This is a textual coupling to
// another package's error message, not a typed signal — Source.Verify
// returns a single error for EVERY failure mode (malformed config, SDK
// client-construction failure, a failed vaults listing, and this count
// check), so the text is the only way to tell "the backend answered but the
// scoping invariant failed" apart from "the backend never answered". If
// source.go's wording changes, this regexp needs updating alongside it.
//
// Pinned on the EMISSION side by
// pkg/platform/identity/directcred/onepassword/source_test.go's
// TestVerifyErrorTextMatchesSetupFlowCoupling, so a rewording there breaks
// CI instead of silently degrading this classification to Indeterminate.
var vaultCountRe = regexp.MustCompile(`accessible vault, found (\d+)`)

// sdkClientErrPrefix is the literal prefix onepassword.Source.Verify puts on
// an error from constructing the 1Password SDK client (source.go:
// wrapErr("creating client", err)). The SDK validates a service-account
// token's structure locally, with no network round trip, when building the
// client — so an error at this exact step is the one case this flow can
// treat as a definitive "the backend's auth layer rejected this token"
// rather than an ambiguous network/timeout/rate-limit failure. Everything
// past this step (listing vaults) crosses the network, so a failure there
// could be either, and is reported as indeterminate rather than guessed at.
//
// Pinned on the EMISSION side by the same
// TestVerifyErrorTextMatchesSetupFlowCoupling referenced above.
const sdkClientErrPrefix = "onepassword: creating client:"

// Verify live-checks the token via the onepassword directcred.Source,
// confirming it resolves to exactly one visible vault — the invariant the
// browser toolbox's direct-credential path depends on (see
// pkg/platform/identity/directcred's doc comment) — rather than merely that
// some endpoint accepts it.
//
// The token is never placed in Detail or Metadata verbatim: classifyVerifyErr
// redacts it from any underlying error text before building a message, as a
// second line of defense alongside onepassword.Source's own scrubToken.
func (f *Flow) Verify(ctx context.Context, req builtins.VerifyRequest) (builtins.VerifyResult, error) {
	token := req.Value.Bearer
	if token == "" {
		return builtins.VerifyResult{
			Status: builtins.VerifyIndeterminate,
			Detail: "no token was supplied to verify",
		}, nil
	}

	verify := f.verify
	if verify == nil {
		verify = defaultVerify
	}
	pin, err := verify(ctx, token)
	if err != nil {
		return classifyVerifyErr(err, token), nil
	}
	if pin.VaultName == "" {
		return builtins.VerifyResult{
			Status: builtins.VerifyIndeterminate,
			Detail: "1Password reported success but returned no vault name",
		}, nil
	}
	return builtins.VerifyResult{
		Status:   builtins.VerifyValid,
		Detail:   fmt.Sprintf("confirmed scoped to exactly one vault: %q", pin.VaultName),
		Metadata: map[string]string{"vault": pin.VaultName},
	}, nil
}

// classifyVerifyErr turns a verifyFn failure into a VerifyResult. See
// vaultCountRe and sdkClientErrPrefix for what each branch recognizes and
// why; anything else is reported as indeterminate rather than guessed at —
// the safe default for a failure mode this flow cannot name.
func classifyVerifyErr(err error, token string) builtins.VerifyResult {
	msg := err.Error()
	if token != "" {
		// Defense in depth: onepassword.Source already scrubs the token from
		// its own errors, but this flow never trusts that guarantee alone.
		msg = strings.ReplaceAll(msg, token, "[redacted]")
	}

	if m := vaultCountRe.FindStringSubmatch(msg); m != nil {
		return builtins.VerifyResult{
			Status: builtins.VerifyForbidden,
			Detail: fmt.Sprintf("1Password reports %s accessible vault(s) for this token; it must see exactly one", m[1]),
		}
	}
	if strings.HasPrefix(msg, sdkClientErrPrefix) {
		return builtins.VerifyResult{
			Status: builtins.VerifyRejected,
			Detail: "1Password's SDK rejected this token while authenticating with it",
		}
	}
	return builtins.VerifyResult{
		Status: builtins.VerifyIndeterminate,
		Detail: "could not verify this token against 1Password: " + msg,
	}
}

// guidance is what the user reads above the field: where the token is
// minted, the single scoping requirement it must satisfy, and what this
// screen is asking for — repeated in text because a browser that refused to
// open leaves the address as the only way through.
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
	fmt.Fprintf(&b, "Create a 1Password service account for %q.\n\n", req.IdentityName)
	b.WriteString("Grant it read_items on exactly ONE vault — the one\n")
	b.WriteString("holding the logins this agent should use — and\n")
	b.WriteString("nothing else. A token scoped to more than one vault,\n")
	b.WriteString("or to write access, is rejected at verification time.\n")
	b.WriteString("\nPaste the token (starts with ops_) below.\n")
	if len(req.ScopeHint) > 0 {
		b.WriteString("\nThe token needs:\n")
		for _, line := range req.ScopeHint {
			b.WriteString("  " + line + "\n")
		}
	}
	b.WriteString("\nPage: " + setupPage + "\n")
	return strings.TrimRight(b.String(), "\n")
}
