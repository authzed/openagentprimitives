package tui

import (
	"context"
	"errors"

	"github.com/charmbracelet/huh"
)

// ErrSkip is returned from Screen.Prepare to say "this branch was not taken".
// The sequencer skips the screen entirely — it is neither presented nor
// applied. Distinct from returning a nil group, which means "nothing to ask,
// but still do your work".
var ErrSkip = errors.New("tui: screen skipped")

// Screen is one step of a wizard: a huh group plus the Go that runs around it.
//
// Screens exist so that branching and I/O happen BETWEEN groups, in ordinary
// testable Go. That is not a stylistic preference: huh's own accessible
// renderer walks every group ignoring WithHideFunc, so a branching wizard
// expressed as one monolithic huh.Form silently asks every question off-TTY.
//
// Prepare has three outcomes:
//
//	(group, nil)     present it, then Apply
//	(nil, nil)       present nothing, still Apply — work-only screens, and
//	                 screens already answered from a pre-seeded State
//	(nil, ErrSkip)   skip entirely; Apply is NOT called
//
// Any other error aborts the run, wrapped with the screen's ID.
//
// Every screen MUST consult State before building a group, and return a nil
// group when its key is already answered. That single convention is what
// makes non-interactive mode work.
//
// A *huh.Group is stateful — cursor, viewport, active-field state — and
// presenting it mutates it in place. Prepare MUST build a NEW group on every
// call; a group must never be cached or reused across calls or across Runs.
type Screen interface {
	// ID names the screen to machines: it keys the rail, and it is what
	// every error the sequencer wraps is attributed to. Terse and stable.
	ID() string

	// Label names the screen to the user — the text of its step in the rail.
	// Title case, a word or two ("Agent", "Slack app", "Review"). Splitting
	// it from ID is what lets the rail be derived from the screens instead of
	// transcribed into a parallel list that desyncs on the next insertion.
	Label() string

	Prepare(ctx context.Context, st *State) (*huh.Group, error)
	Apply(ctx context.Context, st *State) error
}
