// Package anthropic_oauth is the builtin Go flow for the anthropic-oauth
// provider. It does NOT drive an OAuth dance: Claude Code's OAuth client is
// loopback-only and audience-bound, so AP cannot mint the token. It shows the
// user how to generate one with `claude setup-token` and captures the paste.
//
// As a screen sequence:
//
//	token   show the provider's instructions, take the pasted token
package anthropic_oauth

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

// Flow is the anthropic-oauth builtin.
type Flow struct{}

// New returns a new Flow.
func New() *Flow { return &Flow{} }

// Name returns the registry name; must match the provider's `builtin:` field.
func (Flow) Name() string { return "anthropic-oauth" }

// Screens describes the flow: one question, preceded by the provider's own
// instructions for generating the token being asked for.
func (Flow) Screens(_ context.Context, req builtins.Request) ([]tui.Screen, error) {
	return []tui.Screen{
		tui.NewText(tui.TextOpts{
			QuestionOpts: tui.QuestionOpts{
				ID:        "token",
				Label:     "Token",
				Key:       KeyToken,
				Title:     "Paste the token",
				Guidance:  func(*tui.State) string { return guidance(req) },
				Addresses: []tui.Address{flowscreens.DocsAddress(req.Provider)},
				NoteLabel: "Claude token",
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
		return errors.New("anthropic-oauth: no token was supplied")
	}
	token := strings.TrimSpace(st.Get(KeyToken))
	if token == "" {
		return errors.New("anthropic-oauth: no token was supplied")
	}
	if req.Provider != nil {
		if err := provider.ValidateToken(*req.Provider, token); err != nil {
			return fmt.Errorf("anthropic-oauth: %w", err)
		}
	}
	if req.Store == nil {
		return errors.New("anthropic-oauth: nowhere to store the token")
	}
	return req.Store(ctx, builtins.StoreValue{Bearer: token})
}

// Verify reports unsupported: Claude Code OAuth tokens (sk-ant-oat…) are
// audience-bound and no stable public endpoint accepts them for a cheap
// authenticated ping. The format gate (^sk-ant-oat) is the only entry check.
func (Flow) Verify(ctx context.Context, req builtins.VerifyRequest) (builtins.VerifyResult, error) {
	return builtins.VerifyResult{
		Status: builtins.VerifyUnsupported,
		Detail: "Claude Code tokens cannot be pinged without running Claude Code; stored unverified",
	}, nil
}

// guidance is what the user reads above the field: who the token is for,
// followed by the provider's own verbatim instructions for generating one.
//
// The provider's instructions are prose we do not author and do not reflow — a
// line that wraps is untidy, not broken. What this function composes itself is
// kept inside the note's column budget.
func guidance(req builtins.Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Connecting a Claude Code token for %q.\n", req.IdentityName)
	if req.Provider != nil && strings.TrimSpace(req.Provider.Instructions) != "" {
		b.WriteString("\n")
		b.WriteString(strings.TrimRight(req.Provider.Instructions, "\n"))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
