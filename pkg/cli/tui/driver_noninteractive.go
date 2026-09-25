package tui

import (
	"context"
	"errors"
	"fmt"

	"github.com/charmbracelet/huh"
)

// ErrUnanswered is returned when a non-interactive run reaches a screen whose
// answer was not supplied. Callers match it with errors.Is to print
// flag-specific guidance.
var ErrUnanswered = errors.New("tui: required input not supplied")

// nonInteractiveDriver refuses to prompt. It owns no IO whatsoever, so it can
// never block waiting on a terminal that isn't there.
type nonInteractiveDriver struct{}

// NonInteractive returns a Driver for `--non-interactive` runs.
//
// It relies on the Screen contract: a screen whose answer is already in State
// returns a nil group and is never presented. So a group ARRIVING here proves
// an input was missing, and the only correct response is to fail — inventing a
// default would silently produce a resource the user did not describe.
func NonInteractive() Driver { return nonInteractiveDriver{} }

func (nonInteractiveDriver) Present(_ context.Context, screenID string, _ *huh.Group) error {
	return fmt.Errorf("%w: screen %q has no answer; supply it via flags or drop --non-interactive",
		ErrUnanswered, screenID)
}
