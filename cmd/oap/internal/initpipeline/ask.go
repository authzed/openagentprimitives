// cmd/oap/internal/initpipeline/ask.go
//
// The ASK phase: turning every component's declared Inputs into a Resolved map
// keyed by flag.
//
// One screen per Input, driven by the shared sequencer, rather than a
// hand-built form per question. That is what makes a flag-supplied value and a
// typed one the same thing to everything downstream: the flag seeds the run's
// State, the screen whose answer is already there asks nothing, and Apply is
// the single place a value is recorded — with the Default fallback and the
// Required check on it.
//
// Reading the returned map is how an answer travels: the executor's
// Secrets/Manifests closures are handed none of it. Its production caller today
// is `oap install`'s external-access ASK
// (cmd/oap/internal/installcmd/routing_ask.go), whose answers RunInstall
// consumes itself. See Component.Secrets for why that seam is deliberately the
// narrow one.
//
// A blank line separates this file header from the package clause on purpose:
// without one, `go doc` would take the header — starting with a bare file path
// — as the package's documentation and hide component.go's real one.

package initpipeline

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// askTitle is the chrome's title bar while the inputs are collected. A
// constant rather than a literal at the one call site so the phase cannot
// title itself differently if it grows a second entry point.
const askTitle = "oap · install · inputs"

// AskOptions is how this phase is presented. The caller states its terminal
// and its intent; the sequencer resolves the driver from them.
type AskOptions struct {
	// Interactive asks the operator for an input that no flag answered. False
	// takes the Input's Default instead, and fails only when a Required input
	// has neither.
	Interactive bool

	// In and Out are the streams the prompts run over. Left nil, huh falls
	// back to the process's own stdin, so a caller that means the terminal
	// should say so rather than relying on that.
	In  io.Reader
	Out io.Writer

	// Theme styles the run and, through its Caps, decides whether the
	// questions are a full-screen form or line-oriented prompts. Nil is the
	// uncolored theme — an incomplete caller must not abort an install.
	Theme *tui.Theme
}

// Resolve gathers every component's Inputs into a Resolved map. flags carries
// values already parsed from the CLI (flag name → value, "" if unset).
//
// Two components naming the same flag are declaring ONE input, because
// Resolved is keyed by flag: the first declaration is asked, and the second
// reads the answer out of State rather than asking again.
func Resolve(ctx context.Context, comps []Component, flags map[string]string, opts AskOptions) (Resolved, error) {
	// Flags are read into State before anything runs: a screen whose answer is
	// already there asks nothing, which is what makes both pre-filling and a
	// non-interactive run work off one mechanism. An EMPTY flag value is not an
	// answer — it is the flag being unset — so it is left out and falls through
	// to the Default like any other unsupplied input.
	st := tui.NewState()
	keys := make(map[string]struct{}, len(flags))
	for flag, val := range flags {
		keys[flag] = struct{}{}
		if val != "" {
			st.Set(flag, val)
		}
	}
	for _, comp := range comps {
		for _, inp := range comp.Inputs {
			keys[inp.Flag] = struct{}{}
		}
	}
	// soFar projects State back onto the flag vocabulary an Input.AskWhen
	// predicate speaks. The key set is the caller's seeded flags UNION the
	// declared Inputs, not just the latter: a flag no screen asks for can
	// still decide whether another is asked (--tls-issuer is the reason
	// --acme-email is not), and it can only do that if the predicate can see
	// it.
	soFar := func(st *tui.State) Resolved {
		r := make(Resolved, len(keys))
		for k := range keys {
			if st.Has(k) {
				r[k] = st.Get(k)
			}
		}
		return r
	}

	var screens []tui.Screen
	for _, comp := range comps {
		for _, inp := range comp.Inputs {
			screens = append(screens, &inputScreen{comp: comp.Name, inp: inp, interactive: opts.Interactive, soFar: soFar})
		}
	}
	if len(screens) == 0 {
		// Nothing declared anything. Running the sequencer here would still
		// build chrome and hand it to a driver, printing a title bar for a
		// phase with no questions in it.
		return Resolved{}, nil
	}

	theme := opts.Theme
	if theme == nil {
		theme = tui.NewTheme(tui.Caps{})
	}

	answered, err := tui.RunWith(ctx, screens, askRunOptions(opts, theme), st)
	if err != nil {
		return nil, tui.UserFacing(err)
	}

	r := make(Resolved, len(screens))
	for _, comp := range comps {
		for _, inp := range comp.Inputs {
			r[inp.Flag] = answered.Get(inp.Flag)
		}
	}
	return r, nil
}

// askRunOptions is what this phase declares to the sequencer.
//
// A named function rather than a literal at the call site because Inline is
// invisible in the rendered output of every driver a test can build: this is
// the only place the decision below can be read back.
//
// NonInteractive is set from the same flag the screens consult, so the
// fail-closed driver is a backstop rather than the mechanism: every screen
// returns a nil group on a non-interactive run, so nothing is ever presented
// to it. It matters if that ever stops being true — a group reaching a driver
// with no terminal behind it would hang, and this turns that into a loud error
// instead.
//
// INLINE, per the rule on tui.Options.Inline, which turns on whether the
// answers depend on output the run does not itself render. They do, twice
// over. These questions are asked from the middle of `oap install`'s own
// narration — the image-registry resolution above them, the plan summary and
// the component checklist below — and the run is a sequence of separate
// presentations, so by the time the second hostname is asked (it must differ
// from the first) the form that captured the first has already torn down.
// Inline leaves that answer in the terminal's ordinary buffer; the alternate
// screen would discard it and make the scrollback holding it unreachable for
// as long as the next question is up.
//
// One driver, one run: the shared-driver clause of that rule — where the unit
// is the driver rather than the run — does not apply here, and this phase does
// not present over anyone else's driver.
func askRunOptions(opts AskOptions, theme *tui.Theme) tui.Options {
	return tui.Options{
		Theme:          theme,
		Title:          askTitle,
		In:             opts.In,
		Out:            opts.Out,
		NonInteractive: !opts.Interactive,
		Inline:         true,
	}
}

// inputScreen asks for one component Input.
type inputScreen struct {
	// comp names the component that declared this Input. It is part of the
	// screen ID rather than of anything the operator reads: two components can
	// declare the same flag, and the rail resolves a screen by ID.
	comp string
	inp  Input

	// interactive is false when this run must never prompt. The screen still
	// takes part — its Apply is what records the Default and enforces Required
	// — it simply asks nothing.
	interactive bool

	// soFar reads the run's answers back in the flag vocabulary Input.AskWhen
	// speaks. Handed in by Resolve, which is the only thing that knows every
	// flag this run can answer a predicate from.
	soFar func(*tui.State) Resolved

	// value is bound to the field while it is presented.
	value string
}

// ID names the screen to machines. The component qualifies the flag so that
// two components declaring the same Input do not collide in the rail.
func (s *inputScreen) ID() string { return s.comp + "/" + s.inp.Flag }

// Label names the screen to the operator — as the flag they would have passed,
// which is the vocabulary the rest of this phase's messages use.
func (s *inputScreen) Label() string { return "--" + s.inp.Flag }

// AnswerKeys declares the State key this screen asks for. Duck-typed by
// callers that map a missing answer back to the flag that would have supplied
// it.
func (s *inputScreen) AnswerKeys() []string { return []string{s.inp.Flag} }

func (s *inputScreen) Prepare(_ context.Context, st *tui.State) (*huh.Group, error) {
	// State first. An input a flag already answered — or that an earlier
	// component's identical Input already resolved — asks nothing, and neither
	// does any input on a run that was told not to prompt. Apply still runs in
	// both cases: it is what records the value.
	if st.Has(s.inp.Flag) || !s.interactive {
		return nil, nil
	}
	// Then the input's own condition, which reads the answers the screens
	// ahead of this one have already applied. A nil group is how a conditional
	// question declines to be asked.
	if !s.wanted(st) {
		return nil, nil
	}
	s.value = s.inp.Default
	return huh.NewGroup(
		huh.NewInput().
			Key(s.inp.Flag).
			Title(s.title()).
			Value(&s.value).
			Validate(s.validateTyped),
	), nil
}

func (s *inputScreen) Apply(_ context.Context, st *tui.State) error {
	v := strings.TrimSpace(s.value)
	switch {
	case st.Has(s.inp.Flag):
		// The question was never asked: a flag supplied it, or an earlier
		// component's Input for the same flag already resolved it.
		v = strings.TrimSpace(st.Get(s.inp.Flag))
	case v == "":
		// The operator cleared a pre-filled field, or their input ran out.
		v = s.inp.Default
	}
	// wanted is re-evaluated rather than remembered from Prepare: it reads the
	// same State, which no screen has touched in between, and a screen that
	// carried the answer as a field would have two places to keep it in sync.
	// An input this run decided against asking for is not one it can then
	// refuse to run without.
	if s.inp.Required && v == "" && s.wanted(st) {
		return errRequiredFlag(s.inp.Flag)
	}
	st.Set(s.inp.Flag, v)
	return nil
}

// wanted reports whether this Input's own condition admits it. An Input with
// no AskWhen is always wanted, which is what keeps a plain data-only Input
// exactly as it was.
func (s *inputScreen) wanted(st *tui.State) bool {
	if s.inp.AskWhen == nil {
		return true
	}
	return s.inp.AskWhen(s.soFar(st))
}

// title is the question the operator reads. A component that declared no
// Prompt still has to ask something answerable, and the flag is the one name
// the operator can act on.
func (s *inputScreen) title() string {
	if p := strings.TrimSpace(s.inp.Prompt); p != "" {
		return p
	}
	return "Value for --" + s.inp.Flag
}

// validateTyped checks the line as it was typed.
//
// An empty line is accepted whenever the field was pre-filled or the input is
// not required, because that is how an operator accepts the default: huh hands
// the validator the typed line, not the bound value, so rejecting "" would
// reject the bare Enter that means "keep it" — and, under the accessible
// renderer, would re-prompt and eat the next input's line.
func (s *inputScreen) validateTyped(in string) error {
	if strings.TrimSpace(in) != "" {
		return nil
	}
	if s.inp.Default != "" || !s.inp.Required {
		return nil
	}
	return errRequiredFlag(s.inp.Flag)
}

// errRequiredFlag is the refusal for an input that never arrived. It names the
// flag that would have supplied it, which is the only thing the operator can
// do about it.
func errRequiredFlag(flag string) error {
	return fmt.Errorf("required flag --%s must be provided", flag)
}
