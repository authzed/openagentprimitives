// Package tui is agentprimitives' terminal design system.
//
// It is a thin layer over charmbracelet/huh. huh owns the form FIELDS
// (Input, Select, MultiSelect, Confirm, Note, and their validation and
// keymaps); this package owns SEQUENCING, CHROME, THEME, and DRIVER
// SELECTION. Wrapping huh's fields in homegrown types would mean
// re-implementing validation and accessible rendering for no gain.
//
// # Screens, not one big form
//
// A wizard is a []Screen, not a single huh.Form, for two independent reasons:
//
//  1. huh's Form.runAccessible walks every group ignoring WithHideFunc, so a
//     branching wizard expressed as one form silently asks every question in
//     non-TTY mode.
//  2. Real wizards do work BETWEEN questions — list resources from the
//     cluster, call an upstream API, check for name collisions — which a
//     single declarative form cannot express.
//
// So branching lives in ordinary Go, in Screen.Prepare, where it is testable
// without a terminal.
//
// # Drivers
//
// The same []Screen renders three ways:
//
//	TTY             bubbletea with a persistent step rail
//	Plain           huh accessible mode over an io.Reader/io.Writer;
//	                used off-TTY, under NO_COLOR, and by every test
//	NonInteractive  fails closed when any screen is unanswered
//
// DriverFor makes that choice, from Caps plus the --non-interactive flag, and
// Run calls it. No caller branches on isTTY itself: Detect is the one place
// TTY-ness is established and DriverFor is the one place it is acted on, so
// two commands cannot disagree about which renderer a terminal gets.
//
// The TTY driver has two shapes, and which one a run gets is the run's own
// declaration rather than a fourth driver: Options.Inline draws in the
// terminal's ordinary buffer, for a run whose answers depend on the output
// around it, while the default takes the alternate screen. Options.Inline
// carries the rule that decides it.
//
// # The State convention
//
// Every Screen MUST consult State before building a group and return a nil
// group when its key is already answered. That one convention is what makes
// non-interactive mode work: seed State from flags, set
// Options.NonInteractive, and any screen that still wants to ask proves an
// input was missing.
//
// # Cancellation
//
// Ctrl+C never reads as an answer, under either interactive driver, but it
// reaches them by different routes. Plain leaves the terminal in its normal
// cooked mode, so the keypress is delivered to the process as SIGINT and no
// Go code sees it. TTY puts the terminal in raw mode, so huh receives the
// keypress and signals it by setting the form's state rather than by
// returning an error — which is why the driver reads that state and returns
// huh.ErrUserAborted instead of letting the sequencer apply a screen the user
// abandoned.
package tui
