package tui

import (
	"errors"
	"strings"

	"github.com/charmbracelet/huh"
)

// errPrefix is the marker every frame this package adds to an error carries.
// The sequencer and the drivers both build their frames from it, and
// UserFacing takes them off again, so the three cannot drift apart.
const errPrefix = "tui: "

// canceledMessage is what a deliberate Ctrl+C reads as.
//
// The frames underneath say "user aborted", which is huh's vocabulary for a
// decision the person at the terminal just made on purpose: it reads like a
// third party filing a report about them, and "abort" overstates what
// happened — a run stopped at a question has written nothing yet. One quiet
// word is the whole message a deliberate cancellation deserves.
const canceledMessage = "canceled"

// UserFacing strips this package's own framing off an error a run produced, so
// what reaches a person is the sentence a screen wrote.
//
// Every frame here is added by run.go or driver_tty.go as `tui: <verb> screen
// "<id>": …`. That is the right shape for a log and the wrong one for a
// terminal: a screen refusing a token the provider rejected, or naming a flag
// an operator forgot, has already written the sentence they need, and `tui:
// apply screen "collect-token":` in front of it is our plumbing showing
// through — including a screen ID, which names a step in this sequencer and
// nothing anyone operating the command can act on.
//
// It lives here, rather than in each command that presents a wizard, because
// the frames are this package's: a command cannot know which framings exist
// without transcribing the list, and a transcribed list is one that goes stale
// the next time a frame is added. A copy in cmd/oap did go stale exactly that
// way on the cancellation case.
//
// Two properties are load-bearing:
//
//   - Only frames that merely PREFIXED the inner message are removed. That is
//     what distinguishes a wrapper from an error using a sentinel as its own
//     opening clause: ErrUnanswered reads "tui: required input not supplied"
//     and the driver's message continues from it, so unwrapping there would
//     throw away the half that says WHICH answer is missing.
//   - The original chain is kept, so errors.Is still matches downstream. A
//     caller distinguishing a cancellation from a failure, or a fail-closed
//     refusal from a rejected answer, needs the sentinel — replacing the error
//     outright would forbid that, which is why cancellation is re-worded here
//     rather than swapped for a fresh error.
func UserFacing(err error) error {
	if err == nil {
		return nil
	}
	// Checked before the peel rather than after, because a cancellation's
	// message is replaced outright rather than uncovered: the innermost error
	// IS huh.ErrUserAborted, so peeling to it would arrive at the wording this
	// case exists to avoid.
	if errors.Is(err, huh.ErrUserAborted) {
		return reframed{msg: canceledMessage, err: err}
	}
	msg := err.Error()
	for strings.HasPrefix(msg, errPrefix) {
		inner := errors.Unwrap(err)
		if inner == nil || !strings.HasSuffix(msg, inner.Error()) {
			break
		}
		err, msg = inner, inner.Error()
	}
	return reframed{msg: strings.TrimPrefix(msg, errPrefix), err: err}
}

// reframed presents a cleaned message while keeping the original chain, so
// errors.Is on a sentinel still matches downstream.
type reframed struct {
	msg string
	err error
}

func (e reframed) Error() string { return e.msg }
func (e reframed) Unwrap() error { return e.err }
