// Package github_pat is the builtin Go flow for the github-pat provider.
// Walks the user through generating a fine-grained PAT in the browser,
// captures the pasted value, validates the shape, and stores it.
//
// As a screen sequence:
//
//	browser   open the token-creation page, pre-filled from the intent
//	token     say which scopes to grant, take the pasted token
package github_pat

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/flowscreens"
	"github.com/authzed/openagentprimitives/pkg/x/browser"
	"github.com/authzed/openagentprimitives/pkg/x/credmask"
)

// KeyToken is the State key the pasted token lands under. Stable: a caller
// answering this flow ahead of time addresses the screen by it.
const KeyToken = "token"

// patPage is the token-creation page as a user would type it. The browser is
// pointed at this plus pre-filled query parameters (see buildPATURL); the bare
// form is what the guidance shows, because only it is short enough for a note.
const patPage = "https://github.com/settings/personal-access-tokens/new"

// Flow is the github-pat builtin.
type Flow struct{}

// New returns a new Flow.
func New() *Flow { return &Flow{} }

// Name returns the registry name for this flow.
func (Flow) Name() string { return "github-pat" }

// Screens describes the flow: open the page the token is minted on, then take
// what the user minted there. The browser step is a screen of its own because it
// is work rather than a question — which lets a run that cannot prompt refuse it
// up front instead of opening a tab and failing at the screen after it.
//
// req.Provider.Prompt is LLM-system-prompt content for the fallback agent. Do
// NOT show it to the user; the guidance below is composed from structured fields.
func (Flow) Screens(_ context.Context, req builtins.Request) ([]tui.Screen, error) {
	patURL, err := buildPATURL(req)
	if err != nil {
		return nil, fmt.Errorf("github-pat: build PAT URL: %w", err)
	}

	browserStep := flowscreens.NewBrowser(flowscreens.BrowserOpts{
		ID:    "browser",
		Label: "Browser",
		URL:   patURL,
		Guard: KeyToken,
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
				ID:        "token",
				Label:     "Token",
				Key:       KeyToken,
				Title:     "Paste the token",
				Guidance:  func(*tui.State) string { return guidance(req, browserStep.Err()) },
				Addresses: []tui.Address{flowscreens.DocsAddress(req.Provider)},
				NoteLabel: "GitHub token",
				NoteValue: credmask.Mask,
			},
			Check: flowscreens.TokenShapeCheck(req.Provider),
		}),
	}, nil
}

// Result stores the pasted token as a bearer credential.
//
// The emptiness check is not belt-and-braces: huh's accessible renderer has no
// error channel, so an input that runs out arrives here as an unanswered State
// and a nil error. Storing that leaves a credential authenticating as nobody
// behind a CLI that reported success.
func (Flow) Result(ctx context.Context, req builtins.Request, st *tui.State) error {
	if st == nil {
		return errors.New("github-pat: no token was supplied")
	}
	token := strings.TrimSpace(st.Get(KeyToken))
	if token == "" {
		return errors.New("github-pat: no token was supplied")
	}
	if req.Provider != nil {
		if err := provider.ValidateToken(*req.Provider, token); err != nil {
			return fmt.Errorf("github-pat: %w", err)
		}
	}
	if req.Store == nil {
		return errors.New("github-pat: nowhere to store the token")
	}
	return req.Store(ctx, builtins.StoreValue{Bearer: token})
}

// Verify live-checks the PAT via the provider's declarative bearer probe
// (GET https://api.github.com/user).
func (Flow) Verify(ctx context.Context, req builtins.VerifyRequest) (builtins.VerifyResult, error) {
	return builtins.VerifyHTTPBearer(ctx, req.Provider, req.Value.Bearer), nil
}

// guidance is what the user reads above the field: which token to make, which
// scopes it needs, and where — repeated in text because a browser that refused
// to open leaves the address as the only way through.
//
// Every line is kept inside the note's column budget, which this package's tests
// assert: the address has to survive being copied, and huh wraps an over-long
// line with no sign the halves belong together.
func guidance(req builtins.Request, browserErr error) string {
	var b strings.Builder
	if browserErr != nil {
		// The reason comes from the operating system and can be any length; on a
		// line of its own, a long one cannot drag the next instruction into a wrap.
		fmt.Fprintf(&b, "Your browser didn't open:\n  %s\nVisit the page below yourself.\n\n", browserErr)
	}
	fmt.Fprintf(&b, "Create a fine-grained access token for %q,\nthen paste it below.\n\n", req.IdentityName)
	b.WriteString("Suggested scopes:\n")
	for _, line := range recommendedScopes(req) {
		b.WriteString("  " + line + "\n")
	}
	b.WriteString("\nPage: " + patPage + "\n")
	return strings.TrimRight(b.String(), "\n")
}

// buildPATURL builds a github.com/settings/personal-access-tokens/new URL
// pre-filled with intent-derived parameters. Best-effort: no error is fatal.
func buildPATURL(req builtins.Request) (string, error) {
	u, err := url.Parse(patPage)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("name", req.IdentityName)
	if intent := strings.ToLower(req.UserIntent); intent != "" {
		// Crude scope heuristic, matching scopeHint below.
		switch {
		case strings.Contains(intent, "read") && !strings.Contains(intent, "write"):
			q.Set("description", "read-only access for "+req.IdentityName)
		case strings.Contains(intent, "write") || strings.Contains(intent, "create"):
			q.Set("description", "read+write access for "+req.IdentityName)
		default:
			q.Set("description", "agent: "+req.IdentityName)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// recommendedScopes is what the guidance actually prints: the caller's own
// lines when it supplied any, and otherwise this flow's guess from intent.
//
// A caller that knows its call surface beats a heuristic over prose, and the
// gap is not cosmetic. scopeHint only ever names REPOSITORY permissions,
// because that is what this flow's usual callers want; a caller reading an
// organization's members and teams needs an Organization permission, which
// none of those lines grants — and Verify cannot catch the mistake, because
// its probe (GET /user) is answered by any token at all. See
// Request.ScopeHint.

func recommendedScopes(req builtins.Request) []string {
	if len(req.ScopeHint) > 0 {
		return req.ScopeHint
	}
	return scopeHint(req.UserIntent)
}

// scopeHint maps a coarse user-intent string to a concise scope recommendation,
// one short line per entry. Mirrors buildPATURL's description heuristic. Split
// across lines because the note rendering it has roughly sixty columns, and a
// scope list that wraps mid-identifier reads as a scope nobody can look up.
func scopeHint(intent string) []string {
	low := strings.ToLower(intent)
	if strings.Contains(low, "write") || strings.Contains(low, "create") {
		return []string{
			"contents: read & write",
			"pull_requests: read & write",
			"metadata: read",
		}
	}
	return []string{
		"contents: read",
		"pull_requests: read",
		"metadata: read",
	}
}
