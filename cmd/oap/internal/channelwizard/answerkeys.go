package channelwizard

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// answerKeyer is implemented by a screen that asks the user for something, as
// opposed to a note screen or one that derives its values from an earlier
// answer.
//
// Duck-typed rather than imported: it is a single method, declared by
// tui.Question (every question this package renders) and by the identity
// setup flows' own screens, so asking for it here couples this dispatcher to
// neither package — and a screen that does not implement it simply declares
// nothing, which is handled below.
type answerKeyer interface {
	// AnswerKeys returns the State keys the screen may ask for ON THIS RUN.
	AnswerKeys() []string
}

// interactionRequirer is implemented by a screen that some runs cannot reach
// without a human at the terminal — not because an answer is missing, but
// because the step itself happens somewhere no flag reaches.
//
// Duck-typed for the same reason answerKeyer is: consulting it here couples
// this dispatcher to no other package, and a screen that declares nothing is
// simply never asked.
type interactionRequirer interface {
	// RequiresInteraction returns why this screen cannot run without a human,
	// given the answers seeded so far, or "" when it can.
	RequiresInteraction(st *tui.State) string
}

// CheckInteractionRequired refuses a --non-interactive run whose seeded answers
// lead to a step no flag can complete, BEFORE any screen runs.
//
// Without it the fail-closed driver stops at that screen with "has no answer;
// supply it via flags" — which for a step like "authorize this MCP server in
// your browser" is not merely terse, it is FALSE: there is no flag that would
// have answered it, so the message sends the user looking for one that does
// not exist. Refusing up front, in the screen's own words, is the difference
// between that hunt and knowing to drop the flag.
//
// The live implementers are the AgentIdentity setup flows' screens, which
// reach this through the exported name from cmd/oap/internal/identitycmd; no
// question a channel kind declares implements it, because a channel kind
// refuses such a step itself, from Inputs or Resolve, reading
// channelkinds.WizardInput.NonInteractive. The check still runs on every
// non-interactive channel run: it is a property of the screens, not of who
// built them, and a channel question that ever needs it must not have to
// re-wire the dispatcher to be heard.
func CheckInteractionRequired(screens []tui.Screen, st *tui.State) error {
	for _, s := range screens {
		r, ok := s.(interactionRequirer)
		if !ok {
			continue
		}
		if why := r.RequiresInteraction(st); why != "" {
			return errors.New(why)
		}
	}
	return nil
}

// declaredAnswerKeys indexes the keys each screen says it asks for, by screen
// ID, so a fail-closed refusal can name the flags that would have answered the
// screen it stopped on.
//
// It is deliberately NOT the run's declared answer set: checkAnswerKeys builds
// that from the QUESTIONS (inputAnswerKeys), because renderQuestions drops a
// question a flag already answered, so a set derived from the surviving
// screens would reject `--answer <name>=<value>` for one of the kind's own
// inputs. See runChannelWizard's own note on that.
func declaredAnswerKeys(screens []tui.Screen) map[string][]string {
	byScreen := map[string][]string{}
	for _, s := range screens {
		a, ok := s.(answerKeyer)
		if !ok {
			continue
		}
		if keys := a.AnswerKeys(); len(keys) > 0 {
			byScreen[s.ID()] = keys
		}
	}
	return byScreen
}

// checkAnswerKeys rejects an --answer key this kind's wizard does not ask for,
// so a typo fails at the flag rather than resurfacing later as some other
// question's refusal — or, in an interactive run, as a question the user
// thought they had already answered.
//
// A KIND THAT DECLARES NOTHING IS STILL CHECKED, and every --answer to it is
// refused. Inputs is the whole question set, stated in one call, so an empty
// declared set means "this kind asks nothing" and not "this kind did not say"
// — the fake test kind is exactly that, every value in its manifests being
// fixed (channelkinds.Wizard.Inputs). Waving those keys through would put
// the one kind that can never use an answer outside the check that exists to
// catch a key nothing reads.
//
// The Channel name is always allowed, declared or not, for the same reason
// answersFrom carries it regardless: `--name` is a flag of this command
// rather than an answer to any kind's question, and a kind that declares no
// name question this run can still legitimately have been told one — which is
// exactly what `--monitoring --name <existing>` is.
func checkAnswerKeys(kindName string, seeded, declared []string) error {
	allowed := make(map[string]bool, len(declared)+1)
	for _, k := range declared {
		allowed[k] = true
	}
	allowed[wizardkeys.KeyChannelName] = true
	for _, k := range seeded {
		if !allowed[k] {
			return fmt.Errorf("--answer %s=… is not a question the %s wizard asks; %s",
				k, kindName, asksList(declared))
		}
	}
	return nil
}

// asksList is the tail of that refusal: what the kind DOES ask, or that it
// asks nothing at all. Spelled out rather than joining an empty slice, which
// would end the sentence on a dangling "it asks: " and read as a bug in the
// CLI rather than as the answer to the user's next question.
func asksList(declared []string) string {
	if len(declared) == 0 {
		return "it asks nothing — every value in this kind's Channel is fixed"
	}
	return "it asks: " + strings.Join(declared, ", ")
}

// answerFlagHint renders the flags that would have answered a screen, so a
// fail-closed refusal points at something the user can type. The Channel name
// gets its own flag rather than the generic form.
func answerFlagHint(keys []string) string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == wizardkeys.KeyChannelName {
			parts = append(parts, "--name <value>")
			continue
		}
		parts = append(parts, "--answer "+k+"=<value>")
	}
	return strings.Join(parts, " ")
}

// answerKeyHints re-words a fail-closed refusal to name the flags that would
// have answered the screen.
//
// It decorates rather than replaces: the driver underneath is whatever
// DriverFor chose, so the decision of WHICH driver a run gets stays in the one
// place that makes it. The screen ID a Driver is handed is the structural link
// between "which screen refused" and "which keys it declared" — matching on
// the sequencer's error text would be a second, brittle answer to that.
type answerKeyHints struct {
	inner tui.Driver
	keys  map[string][]string
}

func (d answerKeyHints) Present(ctx context.Context, screenID string, g *huh.Group) error {
	err := d.inner.Present(ctx, screenID, g)
	if err == nil || !errors.Is(err, tui.ErrUnanswered) {
		return err
	}
	keys := d.keys[screenID]
	if len(keys) == 0 {
		// A screen that declared nothing gets the driver's own wording; there
		// is no flag to name.
		return err
	}
	return fmt.Errorf("%w: screen %q has no answer; supply %s, or drop --non-interactive",
		tui.ErrUnanswered, screenID, answerFlagHint(keys))
}

// seedAnswer records one flag-supplied answer in every shape State holds.
//
// A screen reads exactly one of Get, Bool or All, and which one is the
// screen's business, not the flag's — a caller typing --answer slackapp=true
// has answered the question whether the screen stores it as a string or a
// bool. Recording all three is what makes a seeded answer satisfy the screen
// that asked for it without this dispatcher knowing which kind of field it is.
func seedAnswer(st *tui.State, key, val string) {
	st.Set(key, val)
	if b, err := strconv.ParseBool(val); err == nil {
		st.SetBool(key, b)
	}
	st.SetAll(key, splitList(val))
}

// splitList turns a comma-separated flag value into a multi-select answer.
//
// Each element is trimmed and blanks are dropped, because "a, b" is what
// someone separating a list actually types and the elements are compared
// against exact constants downstream — an untrimmed " b" is not a rejected
// answer there, it is an option silently left unchecked with no error
// anywhere. Dropping blanks makes a trailing comma harmless for the same
// reason.
//
// A value that resolves to nothing yields a nil list rather than no entry at
// all: the caller has still recorded the answer, so State.Has stays true and
// "answered with nothing" remains distinguishable from "never asked" — only
// the latter is a fail-closed error.
func splitList(val string) []string {
	var out []string
	for _, part := range strings.Split(val, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
