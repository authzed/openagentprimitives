// pkg/cli/tui/secret.go
//
// The secret-input constructor: a text question whose value is hidden where
// the run's driver can hide it, and echoed where it cannot.
//
// It lives here, taking Caps, rather than in each kind's wizard, because the
// choice is about the DRIVER and not about the question. A wizard that set the
// echo mode itself would be re-deriving TTY-ness at every screen — which is
// what DriverFor exists to prevent — and would get it wrong in the direction
// that loses the credential entirely. See NewSecret.
package tui

import (
	"github.com/charmbracelet/huh"
)

// NewSecret returns a question answered by typing a credential.
//
// The value is masked under a driver that can honor masking and echoed under
// one that cannot, and the asymmetry is deliberate: masking off-TTY does not
// degrade to an echoed field, it degrades to NO field.
//
// huh's accessible renderer takes its password branch only when the reader
// implements Fd() uintptr (field_input.go); tui.Plain wraps its reader in
// lineReader, which does not. Form.runAccessible then discards the field's
// error, so a masked field off-TTY prints no prompt, consumes no input line —
// the NEXT field eats it — leaves the bound value empty, and returns nil. The
// run reports success having collected an empty credential and skipped
// whatever that credential was going to authenticate.
//
// Echoing a secret into a terminal that cannot hide it is a real cost, and it
// is the smaller one: the operator can see what happened and act on it, where
// the alternative fails silently and blames something downstream.
func NewSecret(caps Caps, o TextOpts) *Question {
	q := NewText(o)
	if !caps.TTY {
		return q
	}
	// Wraps rather than replaces NewText's field builder so a secret question
	// stays a text question in every other respect — validation, defaults, how
	// the answer is recorded — and cannot drift from one as TextOpts grows.
	plain := q.field
	q.field = func() huh.Field {
		if in, ok := plain().(*huh.Input); ok {
			return in.EchoMode(huh.EchoModePassword)
		}
		// NewText builds an *huh.Input today. If that ever changes, echo
		// normally rather than guess: an echoed credential is recoverable and
		// a dropped one is not.
		return plain()
	}
	return q
}
