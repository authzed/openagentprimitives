package llmagent

import (
	"context"
	"fmt"
	"io"

	"github.com/charmbracelet/lipgloss"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
)

// say writes one styled status line, tolerating a nil writer: this runs inside a
// tool callback on the LLM surface, where a caller need not have a terminal, and
// a missing one must not turn a verification notice into a panic mid-store.
//
// The theme is NOT defended here: a style is selected by the caller, so a nil
// theme would already have panicked evaluating th.Warn at the call site. The only
// guard that can work is the one guardStore does, before selecting any.
func say(w io.Writer, th *tui.Theme, s lipgloss.Style, text string) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "%s\n", th.Render(s, text))
}

// guardStore is the format + live-verification gate in front of every
// store_credential / local_callback persist on the LLM-agent surface. This
// surface has no human confirm loop of its own, so a definitive rejection FAILS
// CLOSED: the returned error becomes the tool error and the agent re-prompts for
// a corrected token. Indeterminate and forbidden outcomes warn on stdout and
// proceed — never block on something a re-paste cannot fix, or the re-prompt
// becomes an endless loop.
//
// Returns the attested subject id (builtins.VerifyResult.SubjectID) alongside
// the gate decision, so the caller can carry it to the eventual store without
// verifying a second time. Empty on every error return and whenever
// verification produced none (unsupported provider, no subjectIDField, an
// indeterminate or forbidden check).
func guardStore(ctx context.Context, prov *provider.Provider, stdout io.Writer, th *tui.Theme, v builtins.StoreValue) (subjectID string, err error) {
	// Normalized before any style is selected off it. The uncolored theme renders
	// every style as its input unchanged, so a caller that supplied no theme gets
	// plain text rather than a nil dereference.
	if th == nil {
		th = tui.NewTheme(tui.Caps{})
	}
	if v.Bearer != "" && prov != nil {
		if err := provider.ValidateToken(*prov, v.Bearer); err != nil {
			return "", fmt.Errorf("token format check failed: %w — ask the user for a corrected token", err)
		}
	}
	res := builtins.VerifyCredential(ctx, prov, v)
	switch res.Status {
	case builtins.VerifyRejected:
		return "", fmt.Errorf("token verification failed: %s — do NOT store this value; ask the user for a corrected token", res.Detail)
	case builtins.VerifyForbidden:
		// The one surface that must NOT fail closed on this verdict. The provider
		// took the credential and refused this one check — an SSO or scope
		// restriction, typically — and there is no "corrected token" the agent could
		// ask for that would change it. Refusing would loop the user forever
		// re-pasting a credential that authenticates fine, so warn and store.
		say(stdout, th, th.Warn, "⚠ "+builtins.ForbiddenNotice(res)+"; storing anyway")
	case builtins.VerifyIndeterminate:
		say(stdout, th, th.Warn, "⚠ could not verify the token ("+res.Detail+"); storing anyway")
	case builtins.VerifyUnsupported:
		say(stdout, th, th.Subtle, "no live verification available for this provider; stored unverified")
	case builtins.VerifyValid:
		if res.Detail != "" {
			say(stdout, th, th.Success, "✓ verified: "+res.Detail)
		}
	default:
		// Exhaustiveness backstop. Every VerifyStatus this build knows is named
		// above, so reaching here means a verdict was added without teaching this
		// surface what it means; that is not permission to store. There is no human
		// to ask, so refuse — but tell the agent to REPORT rather than re-prompt: a
		// verdict nobody understands is not something a freshly-pasted token can fix,
		// and asking for one would spin the user in a loop. This arm exists so the
		// NEXT verdict added cannot silently fall open into a store.
		return "", fmt.Errorf("%s — do NOT store this value; report this to the user instead of asking for another token",
			builtins.UnrecognizedNotice(res))
	}
	return res.SubjectID, nil
}

// subjectIDContextKey is the unexported key ContextWithSubjectID and
// SubjectIDFromContext share.
type subjectIDContextKey struct{}

// ContextWithSubjectID returns a context carrying id, so a caller on the other
// side of a callback whose signature has no room for it — like
// builtins.Request.Store, func(ctx, StoreValue) error, shared verbatim by every
// llmagent/tools callback — can still recover a value already computed on this
// side. wrappedStore (agent.go) uses this to carry guardStore's SubjectID to
// the eventual Store call without verifying a second time. A no-op (returns
// ctx unchanged) when id is empty, so an absent id reads back as absent rather
// than as an explicit empty claim.
func ContextWithSubjectID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, subjectIDContextKey{}, id)
}

// SubjectIDFromContext reads back what ContextWithSubjectID stored, or "" when
// none was set.
func SubjectIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(subjectIDContextKey{}).(string)
	return id
}
