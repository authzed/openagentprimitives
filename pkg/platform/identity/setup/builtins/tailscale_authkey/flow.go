// Package tailscale_authkey is the builtin Go flow for the
// tailscale-authkey provider. Walks the user through creating a tagged
// ephemeral auth key in the Tailscale admin console, captures the pasted
// value, validates the shape, and stores it.
//
// As a screen sequence:
//
//	browser   open the admin console's auth-keys page
//	key       spell out the settings the key needs, take the pasted key
package tailscale_authkey

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/flowscreens"
	"github.com/authzed/openagentprimitives/pkg/x/browser"
	"github.com/authzed/openagentprimitives/pkg/x/credmask"
)

// KeyAuthKey is the State key the pasted auth key lands under. Stable: a
// caller answering this flow ahead of time addresses the screen by it.
const KeyAuthKey = "authkey"

// keysURL is the Tailscale admin console page where auth keys are minted.
const keysURL = "https://login.tailscale.com/admin/settings/keys"

// Flow is the tailscale-authkey builtin.
type Flow struct{}

// New returns a new Flow.
func New() *Flow { return &Flow{} }

// Name returns the registry name for this flow.
func (Flow) Name() string { return "tailscale-authkey" }

// Screens describes the flow: open the console page keys are minted on, then
// take the key minted there. The browser step is a screen of its own because it
// is work rather than a question — which lets a run that cannot prompt refuse it
// up front instead of opening a tab and failing at the screen after it.
func (Flow) Screens(_ context.Context, req builtins.Request) ([]tui.Screen, error) {
	browserStep := flowscreens.NewBrowser(flowscreens.BrowserOpts{
		ID:    "browser",
		Label: "Browser",
		URL:   keysURL,
		Guard: KeyAuthKey,
		// browser.Open, not a package var of our own: it already suppresses
		// itself inside a test binary, so a test that installs nothing cannot
		// open a window here, and a test that wants to assert the URL installs
		// one recorder from browsertest instead of a seam per flow.
		Open:      browser.Open,
		NoteLabel: "Browser",
	})

	return []tui.Screen{
		browserStep,
		tui.NewText(tui.TextOpts{
			QuestionOpts: tui.QuestionOpts{
				ID:        "key",
				Label:     "Auth key",
				Key:       KeyAuthKey,
				Title:     "Paste the auth key",
				Guidance:  func(*tui.State) string { return guidance(req, browserStep.Err()) },
				Addresses: []tui.Address{flowscreens.DocsAddress(req.Provider)},
				NoteLabel: "Tailscale auth key",
				NoteValue: credmask.Mask,
			},
			Check: flowscreens.TokenShapeCheck(req.Provider),
		}),
	}, nil
}

// Result stores the pasted key as a bearer credential.
//
// The emptiness check is not belt-and-braces: huh's accessible renderer has no
// error channel, so an input that runs out arrives here as an unanswered State
// and a nil error. Storing that leaves a credential authenticating as nobody
// behind a CLI that reported success.
func (Flow) Result(ctx context.Context, req builtins.Request, st *tui.State) error {
	if st == nil {
		return errors.New("tailscale-authkey: no auth key was supplied")
	}
	key := strings.TrimSpace(st.Get(KeyAuthKey))
	if key == "" {
		return errors.New("tailscale-authkey: no auth key was supplied")
	}
	if req.Provider != nil {
		if err := provider.ValidateToken(*req.Provider, key); err != nil {
			return fmt.Errorf("tailscale-authkey: %w", err)
		}
	}
	if req.Store == nil {
		return errors.New("tailscale-authkey: nowhere to store the auth key")
	}
	return req.Store(ctx, builtins.StoreValue{Bearer: key})
}

// Verify returns VerifyUnsupported since there's no live verification endpoint
// for Tailscale auth keys.
func (Flow) Verify(ctx context.Context, req builtins.VerifyRequest) (builtins.VerifyResult, error) {
	return builtins.VerifyResult{Status: builtins.VerifyUnsupported, Detail: "tailscale auth keys cannot be verified live"}, nil
}

// guidance is what the user reads above the field: the settings the key must
// carry, the tailnet-policy line that has to exist for its tag, and where to
// mint it — repeated in text because a browser that refused to open leaves the
// address as the only way through.
//
// Every line is kept inside the note's column budget, which this package's tests
// assert: the policy snippet and the address have to survive being copied, and
// huh wraps an over-long line with no sign the halves belong together.
func guidance(req builtins.Request, browserErr error) string {
	var b strings.Builder
	if browserErr != nil {
		// The reason comes from the operating system and can be any length; on a
		// line of its own, a long one cannot drag the next instruction into a wrap.
		fmt.Fprintf(&b, "Your browser didn't open:\n  %s\nVisit the page below yourself.\n\n", browserErr)
	}
	fmt.Fprintf(&b, "Creating a Tailscale auth key for %q.\n\n", req.IdentityName)
	b.WriteString("Give the key these settings:\n")
	b.WriteString("  Reusable         ON\n")
	b.WriteString("  Ephemeral        ON\n")
	b.WriteString("  Pre-authorized   ON  (if your tailnet approves devices)\n")
	b.WriteString("  Tags             one your ACLs scope, e.g. tag:sre-bot\n")
	b.WriteString("\nYour tailnet policy must declare that tag:\n")
	b.WriteString(`  "tagOwners": { "tag:sre-bot": ["autogroup:admin"] }` + "\n")
	b.WriteString("\nKeys expire (90 days at most); re-run setup to rotate.\n")
	b.WriteString("\nPage: " + keysURL + "\n")
	return strings.TrimRight(b.String(), "\n")
}
