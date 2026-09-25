package tui

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The framings this package emits, reproduced verbatim from the code that
// emits them. Written out here rather than provoked through a real run so that
// a frame added to run.go or driver_tty.go without a row here is visible as a
// missing constructor rather than as a test that silently still passes.
//
// Every one of these is a real shape a caller can be handed. UserFacing must
// leave nothing of any of them in front of the sentence a screen wrote — the
// whole class of defect being guarded against is a stripper with no case for a
// framing its own package produces.
func frameCanceledBefore(id string, err error) error {
	return fmt.Errorf("tui: canceled before screen %q: %w", id, err) // run.go
}
func framePrepare(id string, err error) error {
	return fmt.Errorf("tui: prepare screen %q: %w", id, err) // run.go
}
func framePresent(id string, err error) error {
	return fmt.Errorf("tui: present screen %q: %w", id, err) // run.go
}
func frameApply(id string, err error) error {
	return fmt.Errorf("tui: apply screen %q: %w", id, err) // run.go
}
func frameTTYCanceled(id string) error {
	return fmt.Errorf("tui: screen %q canceled: %w", id, huh.ErrUserAborted) // driver_tty.go
}
func frameRun(id string, err error) error {
	return fmt.Errorf("tui: run screen %q: %w", id, err) // driver_tty.go
}
func frameFieldErrors(id string, errs ...error) error {
	return fmt.Errorf("tui: screen %q: %w", id, errors.Join(errs...)) // driver_tty.go
}

func TestUserFacing(t *testing.T) {
	// screenID is what must never survive into a message a person reads. It is
	// deliberately a string no screen's own sentence would contain, so a row
	// asserting its absence is asserting about the framing and nothing else.
	const screenID = "collect-token"

	// tokenRejected is the kind of error a screen writes for itself: a whole
	// sentence, already in the user's terms.
	tokenRejected := errors.New("the workspace rejected that token; generate a new one and paste it again")

	cases := []struct {
		name string
		in   error
		// want is the exact message a person must see.
		want string
		// wantIs is the sentinel the returned error must still match, so a
		// caller can tell cancellation from failure and a fail-closed refusal
		// from a rejected answer.
		wantIs error
	}{
		{
			name: "nil: passes through",
		},
		{
			name:   "apply frame: only the screen's own sentence survives",
			in:     frameApply(screenID, tokenRejected),
			want:   tokenRejected.Error(),
			wantIs: tokenRejected,
		},
		{
			name:   "prepare frame: only the screen's own sentence survives",
			in:     framePrepare(screenID, tokenRejected),
			want:   tokenRejected.Error(),
			wantIs: tokenRejected,
		},
		{
			name:   "present frame: only the screen's own sentence survives",
			in:     framePresent(screenID, tokenRejected),
			want:   tokenRejected.Error(),
			wantIs: tokenRejected,
		},
		{
			name:   "canceled-before-screen frame: the context's own words, no screen ID",
			in:     frameCanceledBefore(screenID, context.Canceled),
			want:   context.Canceled.Error(),
			wantIs: context.Canceled,
		},
		{
			name:   "run-screen frame: only the renderer's own failure survives",
			in:     frameRun(screenID, errors.New("the terminal went away")),
			want:   "the terminal went away",
			wantIs: nil,
		},
		{
			name:   "field-errors frame: the field's own complaint survives",
			in:     frameFieldErrors(screenID, errors.New("that is not an email address")),
			want:   "that is not an email address",
			wantIs: nil,
		},
		{
			// The shape a real Ctrl+C arrives in: the TTY driver's own frame,
			// wrapped again by the sequencer.
			name:   "Ctrl+C on a TTY: one plain word, and still matchable as an abort",
			in:     framePresent(screenID, frameTTYCanceled(screenID)),
			want:   "canceled",
			wantIs: huh.ErrUserAborted,
		},
		{
			name:   "Ctrl+C frame on its own: same word, same sentinel",
			in:     frameTTYCanceled(screenID),
			want:   "canceled",
			wantIs: huh.ErrUserAborted,
		},
		{
			// ErrUnanswered opens the message rather than terminating it, so
			// unwrapping past it would throw away the half that says WHICH
			// answer was missing. The screen ID here is the driver's own
			// wording, not framing, and is meant to survive.
			name:   "fail-closed refusal: the sentinel's own clause is kept whole",
			in:     unansweredFor(screenID),
			want:   `required input not supplied: screen "collect-token" has no answer; supply it via flags`,
			wantIs: ErrUnanswered,
		},
		{
			name:   "fail-closed refusal inside a present frame: same, with the frame gone",
			in:     framePresent(screenID, unansweredFor(screenID)),
			want:   `required input not supplied: screen "collect-token" has no answer; supply it via flags`,
			wantIs: ErrUnanswered,
		},
		{
			// A screen's refusal is often a paragraph, not a clause. The peel
			// must not stop at the first newline, and the blank line the
			// screen used to separate cause from remedy must survive.
			name: "a multi-line refusal: the whole body survives, framing does not",
			in: frameApply(screenID, errors.New(
				"the workspace rejected that token.\n\nCheck it was copied from this app's own settings page.")),
			want: "the workspace rejected that token.\n\nCheck it was copied from this app's own settings page.",
		},
		{
			name:   "an error this package never framed: returned as written",
			in:     tokenRejected,
			want:   tokenRejected.Error(),
			wantIs: tokenRejected,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UserFacing(tc.in)
			if tc.want == "" {
				assert.NoError(t, got)
				return
			}
			require.Error(t, got)
			assert.Equal(t, tc.want, got.Error())
			assert.NotContains(t, got.Error(), errPrefix, "this package's own framing must not reach a person")
			if tc.wantIs != nil {
				assert.ErrorIs(t, got, tc.wantIs, "the original chain must stay matchable")
			}
		})
	}
}

// unansweredFor reproduces what the fail-closed driver writes: ErrUnanswered
// as the OPENING clause of a longer sentence, not as its tail.
func unansweredFor(id string) error {
	return fmt.Errorf("%w: screen %q has no answer; supply it via flags", ErrUnanswered, id)
}

// A screen ID names a step in our sequencer and nothing a person operating the
// command can act on. This walks every framing at once so a new one cannot be
// added with a case here quietly missing.
func TestUserFacingNeverLeaksAScreenID(t *testing.T) {
	const screenID = "collect-token"
	inner := errors.New("that answer cannot be used")

	framings := map[string]error{
		"canceled before": frameCanceledBefore(screenID, context.Canceled),
		"prepare":         framePrepare(screenID, inner),
		"present":         framePresent(screenID, inner),
		"apply":           frameApply(screenID, inner),
		"tty canceled":    frameTTYCanceled(screenID),
		"run":             frameRun(screenID, inner),
		"field errors":    frameFieldErrors(screenID, inner),
		"nested tty":      framePresent(screenID, frameTTYCanceled(screenID)),
	}
	for name, framed := range framings {
		t.Run(name+" frame: the screen ID is gone", func(t *testing.T) {
			assert.NotContains(t, UserFacing(framed).Error(), screenID)
		})
	}
}
