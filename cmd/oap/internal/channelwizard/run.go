// The driver for a channel kind's setup flow (channelkinds.Wizard): from the
// answers a client can supply, to the manifests the kind hands back.
//
//	Inputs → Handoff → the declared answer-key set → render the questions no
//	answer already covers → drive them → refuse a name collision → run the
//	browser handoff → Resolve → Result
//
// It sits beside the commands rather than inside one because TWO of them drive
// that sequence: `oap channel create`, which asks an operator for a kind and
// walks them through it, and `oap agent install`, which walks the channels a
// bundle declares. A second implementation would drift on precisely the steps
// whose ORDER is not derivable from the contract and each of which cost a bug
// to establish — a setup failure that must not spend the fallback detour, and
// the SatisfiedBy subtraction. Each of those is documented at the line that
// implements it; that is the payload of this package.
//
// THE TAIL OF THE SEQUENCE IS NOT HERE. From the collision check onwards —
// collision, handoff, Resolve, Result, and the apply — the steps are
// pkg/channels/channelkinds/wizardrun's, because admind drives the same tail
// from an HTTP handler and Go's import rule keeps it out of cmd/oap/internal.
// What stays here is everything terminal: rendering a question, driving a
// screen, reading an answer back out of tui.State, and running a handoff
// against a loopback listener.
//
// It names no kind: every dispatch goes through channelkinds. And it owns no
// terminal decision: how a run is PRESENTED — the theme, the chrome's title,
// which streams it reads and writes, whether it may prompt at all — arrives as
// tui.Options from whichever command built it.
package channelwizard

import (
	"context"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardrun"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/x/browser"
)

// Seeded is what the flags answered before anything was asked, in the
// two shapes the run needs it in.
//
// One value rather than two returns because the two must always agree: Values
// decides which of a kind's questions are already answered (and so are never
// asked), while Keys is what an unknown --answer key is checked against. A
// caller that built one and forgot the other would either ask a question it
// had the answer to or reject a key it had accepted.
type Seeded struct {
	// Values is every seeded answer, keyed the way a question names it.
	// --name is IN HERE, under wizardkeys.KeyChannelName, even though it
	// is not an --answer: to a kind that declares a channel-name question it
	// is that question's answer, and leaving it out would leave the question
	// rendered — taking a step on the rail that the run then skips without
	// ever asking.
	Values map[string]string
	// Keys is what --answer named, in the order given, and NOT --name. It
	// feeds checkAnswerKeys, whose refusal quotes the flag the user typed;
	// --name has its own flag and its own always-allowed rule there.
	Keys []string
}

// Seed turns the flags into the State a run starts from.
//
// The keys are the kind's own — the Names of the questions it declares —
// rather than a per-question flag apiece. That is what keeps this dispatcher
// free of any one kind's vocabulary: a question added to a flow is seedable
// the day it lands, and a key nothing reads is simply never consulted.
func Seed(name string, answers []string) (*tui.State, Seeded, error) {
	st := tui.NewState()
	seeded := Seeded{Values: map[string]string{}}
	if name != "" {
		st.Set(wizardkeys.KeyChannelName, name)
		seeded.Values[wizardkeys.KeyChannelName] = name
	}
	for _, raw := range answers {
		key, val, found := strings.Cut(raw, "=")
		// Both halves are trimmed: every wizard TrimSpaces what it reads back
		// out of State, so a value with stray whitespace would otherwise be
		// stored one way and compared another.
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if !found || key == "" {
			return nil, Seeded{}, fmt.Errorf("--answer %q must be in key=value form", raw)
		}
		if key == wizardkeys.KeyChannelName && name != "" && val != name {
			return nil, Seeded{}, fmt.Errorf("--name %q and --answer %s=%q give two different names for the same Channel; supply one of them",
				name, wizardkeys.KeyChannelName, val)
		}
		seedAnswer(st, key, val)
		seeded.Values[key] = val
		seeded.Keys = append(seeded.Keys, key)
	}
	return st, seeded, nil
}

// Run presents a kind's setup flow and reads the manifests back
// out of it: ask the kind what it needs, render the questions no flag already
// answered, run those, drive the handoff if it declared one, and hand every
// answer back for the manifests.
//
// role is the ChannelSpec.Role this Channel must end up with — a bundle's
// declared one for `oap agent install`, `--role` for `oap channel create` —
// or empty to leave whatever the kind's own Result set. It is NOT an answer:
// no kind asks for a role. It is written onto the WizardInput below, so the
// kind can shape its question set around it, and stamped from that same value
// by wizardrun.Finish, which is where both this client's and admind's runs
// converge. See channelkinds.WizardInput.Role for what it costs to omit.
//
// It sits between st and seeded rather than beside kindName so that no
// transposition of it compiles: its neighbours are a *tui.State and a Seeded,
// and a run misconfigured by two swapped string arguments is exactly the class
// of defect this branch keeps finding.
//
// It returns the answered State so the caller can render the summary the run
// accumulated — including on the failure path, where what the run had already
// done is as much a fact as why it stopped.
func Run(
	ctx context.Context,
	w channelkinds.Wizard,
	wizIn channelkinds.WizardInput,
	kindName string,
	st *tui.State,
	role string,
	seeded Seeded,
	opts tui.Options,
) (channelkinds.WizardOutput, *tui.State, error) {
	// Carried on the input BEFORE the kind is asked anything, because a kind
	// shapes its question set from this call: slack asks a role=output Channel
	// where to post, and a role that only reached the stamp would arrive after
	// the questions had already been declared without it. wizardrun.Finish
	// reads the stamp off this same value, so the role the kind was asked with
	// and the role its Channel gets cannot differ.
	wizIn.Role = role

	inputs, err := w.Inputs(ctx, wizIn)
	if err != nil {
		return channelkinds.WizardOutput{}, st, err
	}

	// Asked here, alongside Inputs and before anything is rendered, because
	// the questions a handoff falls back on are part of the set this run may
	// be seeded for — see the declared set below. What the handoff needs from
	// the ANSWERS it gets later, through the closures on the spec, which is
	// why nothing is driven yet.
	handoff, err := w.Handoff(ctx, wizIn)
	if err != nil {
		return channelkinds.WizardOutput{}, st, err
	}

	// Declared from the QUESTIONS, not from the screens rendered below.
	// renderQuestions drops a question the flags already answered — that is
	// what keeps the step rail honest — so a key the operator just supplied is
	// absent from the surviving screens, and deriving the declared set from
	// them would reject `--answer <name>=<value>` for one of this kind's own
	// inputs.
	//
	// A handoff's fallback questions are declared too, and for the same
	// reason: they are questions this kind asks, on the route a headless host
	// or a blocked port takes, and `--answer app-id=…` is exactly how an
	// unattended run supplies what the browser step would have produced.
	// Deriving the set from Inputs alone would refuse the flag that makes
	// those runs possible at all.
	declared := inputAnswerKeys(wizardrun.AllInputs(inputs, handoff))
	if err := checkAnswerKeys(kindName, seeded.Keys, declared); err != nil {
		return channelkinds.WizardOutput{}, st, err
	}

	screens, err := renderQuestions(inputs, seeded.Values, st, screenCaps(opts))
	if err != nil {
		return channelkinds.WizardOutput{}, st, err
	}
	// No "this run asks nothing" guard: a kind whose output is fully
	// determined by its WizardInput legitimately asks nothing (see
	// channelkinds.Wizard.Inputs), and a fully-seeded run of any kind
	// legitimately has nothing left to ask.
	byScreen := declaredAnswerKeys(screens)

	answered, err := driveChannelScreens(ctx, screens, st, byScreen, opts)
	if err != nil {
		return channelkinds.WizardOutput{}, answered, err
	}

	answers := answersFrom(inputs, answered)

	// Everything from here on — the collision check in FRONT of the two
	// irreversible steps (P5-R21), the handoff before Resolve because Resolve
	// reads what it produced, the two overwrite rules, Result last — is
	// wizardrun.Finish's, and its doc is where each position's reason lives.
	// It is shared rather than stated here because admind drives the same tail
	// and cannot import this package; two copies would drift on exactly the
	// steps whose order the contract does not imply.
	//
	// The one thing this client still owns is HOW a handoff is driven: the CLI
	// opens a loopback listener and waits for the operator's browser inside a
	// single call, which is a shape no server-rendered client can use.
	//
	// The collision check reads answers[name] out of the same map built above,
	// which is why the typed answer has to exist first: the pre-wizard check at
	// the top of runChannelCreate no-ops on an empty name, so it only ever sees
	// a --name flag. The operator re-runs rather than being re-asked in place,
	// and that is the accepted cost of a question set stated as data — a plain
	// QString carries no validator this side evaluates (see
	// channelkinds.ValidateInputs), so there is nowhere for a per-field "that
	// name is taken, try another" to live.
	out, err := wizardrun.Finish(ctx, wizardrun.Params{
		Wizard:  w,
		In:      wizIn,
		Answers: answers,
		Handoff: handoff,
		DriveHandoff: func(ctx context.Context, spec *channelkinds.HandoffSpec, answers map[string]string) (map[string]string, error) {
			return runHandoff(ctx, handoffRun{
				spec:    spec,
				answers: answers,
				seeded:  seeded.Values,
				state:   answered,
				opts:    opts,
				// browser.Open, not a package var this file can swap: it
				// suppresses itself inside a test binary on its own, and the one
				// test that drives a handoff end to end installs a browsertest
				// recorder instead. Tests of runHandoff itself still pass their
				// own function through handoffRun.openBrowser, which is genuine
				// constructor injection and stays.
				openBrowser: browser.Open,
			})
		},
	})
	return out, answered, err
}

// driveChannelScreens presents screens and returns the answered State.
//
// It is the ONE place a run's screens are driven — the up-front batch here,
// and the handoff's fallback questions in askFallback — so both get the same
// driver selection, the same up-front interaction check and the same
// user-facing framing of a refusal. A second driving loop for the detour is
// how the two would come to ask the same operator different things.
//
// byScreen maps a screen's ID to the answer keys it declares, so a
// fail-closed refusal can name the flags that would have answered it.
func driveChannelScreens(
	ctx context.Context,
	screens []tui.Screen,
	st *tui.State,
	byScreen map[string][]string,
	opts tui.Options,
) (*tui.State, error) {
	// Interactive runs leave Options.Driver nil, so tui.Run resolves its own —
	// which is also what assembles the chrome from the title and the screens.
	// A non-interactive run still lets DriverFor make that choice, and only
	// wraps the result so a refusal can name the flag that would have answered
	// it. Chrome is nil there because the fail-closed driver owns no IO.
	if opts.NonInteractive {
		// Asked before the driver is built, so a run that cannot succeed says
		// why up front rather than reaching the screen that cannot be answered.
		if err := CheckInteractionRequired(screens, st); err != nil {
			return st, err
		}
		opts.Driver = answerKeyHints{
			inner: tui.DriverFor(tui.DriverParams{
				Theme:          opts.Theme,
				In:             opts.In,
				Out:            opts.Out,
				NonInteractive: true,
			}),
			keys: byScreen,
		}
	}

	answered, err := tui.RunWith(ctx, screens, opts, st)
	if err != nil {
		return answered, tui.UserFacing(err)
	}
	return answered, nil
}

// inputAnswerKeys returns the keys a kind's questions ask for, in
// declaration order, deduplicated.
//
// Dedup is defensive rather than expected — channelkinds.ValidateInputs, which
// renderQuestions runs before building anything, already refuses a duplicate
// Name — but this function is also what a refusal LISTS back to the user, and
// a doubled key there would read as a bug in the CLI.
func inputAnswerKeys(qs []oap.Question) []string {
	out := make([]string, 0, len(qs))
	seen := make(map[string]bool, len(qs))
	for _, q := range qs {
		if seen[q.Name] {
			continue
		}
		seen[q.Name] = true
		out = append(out, q.Name)
	}
	return out
}

// answersFrom reads each declared question's answer back out of the run's
// State, keyed by Question.Name, in the map shape Wizard.Result takes.
//
// It has ONE special arm, and it is the enum-constrained resourceList: that
// is the only question shape questionscreen.NewScreen renders as a
// multi-select, and the only widget that records its answer as a LIST
// (SetAll/All) rather than a string. Everything else lands in State as a
// string and is read back with Get — including QEnum, NewScreen's other
// non-default arm, whose single-choice widget records the chosen value with
// Set, and including QBool, which renders as free text here rather than as a
// confirm. Reading the multi-select with Get would hand Result an empty
// string for the one type that never stores one.
//
// A question left unanswered is ABSENT from the map rather than
// present-and-empty, so a kind can still tell "answered with nothing" (an
// optional field the operator cleared) from "never asked".
//
// The Channel name is carried whenever State holds one, DECLARED OR NOT, for
// the same reason checkAnswerKeys always allows it as a key: `--name` is a
// flag of this command rather than an answer to any kind's question, and a
// kind that does not declare a name question this run can still legitimately
// have been told one. Dropping it made slack's manual route print a
// come-back command missing the `--name` the operator had just supplied —
// silently, since the flag was accepted and then read by nothing.
func answersFrom(qs []oap.Question, st *tui.State) map[string]string {
	answers := make(map[string]string, len(qs)+1)
	if st.Has(wizardkeys.KeyChannelName) {
		answers[wizardkeys.KeyChannelName] = st.Get(wizardkeys.KeyChannelName)
	}
	for _, q := range qs {
		if !st.Has(q.Name) {
			continue
		}
		if q.Type == oap.QResourceList && len(q.Enum) > 0 {
			answers[q.Name] = strings.Join(st.All(q.Name), ",")
			continue
		}
		answers[q.Name] = st.Get(q.Name)
	}
	return answers
}
