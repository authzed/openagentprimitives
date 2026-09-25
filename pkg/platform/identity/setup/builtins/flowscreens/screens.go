// Package flowscreens holds the steps more than one credential-setup flow takes:
// "open the page where this is generated", plus the flow-specific vocabulary a
// tui.Question is configured with — which address a provider documents itself
// at, and what shape its token has. A step lands here when a second flow needs
// it; steps specific to one provider stay in that provider's own package.
package flowscreens

import (
	"context"
	"errors"

	"github.com/charmbracelet/huh"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// BrowserOpts describes a step that opens a page for the user.
type BrowserOpts struct {
	ID, Label string

	// URL is the page to open. It may carry pre-filled query parameters the user
	// never has to see; what they are shown to type by hand belongs in the
	// following screen's guidance, which has the note's column budget to respect.
	URL string

	// Guard is the State key holding the credential this page exists to generate.
	// When something earlier in the run already put it there, no page is opened.
	//
	// No shipped flow reaches that state — Browser is the first screen in both
	// flows that use it, and no `oap` command accepts a setup answer from a flag —
	// but the check is kept for a flow whose earlier screens derive the key
	// themselves, which would otherwise open a page for a credential it held.
	Guard string

	// Open opens the URL. Nil means the step cannot run, which is reported the
	// same way a failed open is rather than panicking mid-flow.
	Open func(string) error

	// NoteLabel is the summary line's label, recorded only when the browser
	// did not open. Empty records no line.
	NoteLabel string
}

// Browser opens a page in the user's browser.
//
// It asks nothing: Prepare returns no group and Apply does the work, the shape
// tui.Screen gives a step that is neither a question nor a summary. A failed open
// is not fatal — the following screen repeats the address and says the browser
// did not open, which is the only place a user can act on it.
type Browser struct {
	opts BrowserOpts
	err  error
}

// NewBrowser returns the screen described by o.
func NewBrowser(o BrowserOpts) *Browser { return &Browser{opts: o} }

func (s *Browser) ID() string { return s.opts.ID }

func (s *Browser) Label() string { return s.opts.Label }

// Err reports why the browser did not open, or nil. The screen that follows
// reads it so the user is told where to go by hand.
func (s *Browser) Err() error { return s.err }

func (s *Browser) Prepare(_ context.Context, st *tui.State) (*huh.Group, error) {
	if st.Has(s.opts.Guard) {
		return nil, tui.ErrSkip
	}
	return nil, nil
}

// RequiresInteraction reports that this step needs a human, and says why.
// Consulted by the CLI before a run that was told not to prompt.
//
// Without it, a run with no credential in hand opens a browser tab and only THEN
// refuses at the screen that would have taken what the page produced — this
// screen asks nothing, so the fail-closed driver never sees it, and opening a
// page at somebody who is not there is the one visible effect of a run whose
// whole purpose was to change nothing and say so.
//
// The refusal names only a way out that exists: no `oap` command accepts a setup
// answer from a flag, so naming one would send the reader hunting for something
// never built. The early return is for a flow whose earlier screens derive the
// guard key themselves; no shipped flow does today.
func (s *Browser) RequiresInteraction(st *tui.State) string {
	if st.Has(s.opts.Guard) {
		return ""
	}
	return "this credential is minted on a web page and pasted back, so nothing can stand in " +
		"for you here: opening the page and copying what it produces is the entire step. " +
		"Re-run without asking to skip prompts."
}

func (s *Browser) Apply(_ context.Context, st *tui.State) error {
	switch {
	case s.opts.URL == "":
		s.err = errors.New("there is no address to open")
	case s.opts.Open == nil:
		s.err = errors.New("this build cannot open a browser")
	default:
		s.err = s.opts.Open(s.opts.URL)
	}
	if s.err != nil && s.opts.NoteLabel != "" {
		st.Note(s.opts.NoteLabel, "did not open — the address was shown instead")
	}
	return nil
}
