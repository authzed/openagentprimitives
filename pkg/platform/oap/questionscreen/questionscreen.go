// Package questionscreen builds the tui.Screen that asks one oap.Question,
// whatever collected the question in the first place.
//
// It exists because a bundle's manifest questions
// (pkg/platform/oap/install.Resolve) and a channel wizard's declared inputs
// (cmd/oap/internal/channelwizard's renderQuestions) are the same shape from
// here down: one Question in, one Screen out, the QuestionType deciding the
// widget. The mapping used to live only in install, as a private
// newQuestionScreen; a second caller earned it a package of its own rather
// than a second copy of the same six-way switch.
package questionscreen

import (
	"strings"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// NewScreen builds the question suited to one manifest question's type:
//   - QEnum                      -> one choice from q.Enum (string)
//   - QResourceList with an Enum -> any subset of q.Enum, its initial selection
//     preseeded from q.Default ([]string)
//   - QSecret                    -> one line of text whose value is hidden
//     wherever caps says the driver can hide it (tui.NewSecret)
//   - everything else            -> one line of free text (string), preseeded
//     from q.Default
//
// caps is the run's terminal capabilities — the same ones its theme and driver
// were resolved from. It decides masking and nothing else, and it is taken as
// an argument rather than re-detected here because a screen that measured the
// terminal for itself would be a second opinion about a stream it never sees:
// tui.DriverFor already owns that decision, and the two disagreeing is how a
// question ends up masked over a driver that cannot mask. Under such a driver
// masking does NOT degrade to an echoed field, it degrades to no field at all —
// see tui.NewSecret for the huh mechanics — so the zero Caps, which is what a
// caller with nothing to measure passes, echoes.
//
// A QSecret's answer is TRIMMED, like every other free-text answer here. A
// secret is the one answer type whose surrounding whitespace could conceivably
// be part of the value: a token pasted with a stray space loses it, which is
// what an operator wants, and a passphrase deliberately ending in one cannot be
// entered.
//
// No question keeps a summary line. A manifest's answers are recorded on the
// resources this install applies, which is where an operator reads them back —
// and one of them is a secret, which plain scrollback must never carry.
func NewScreen(q oap.Question, caps tui.Caps) tui.Screen {
	switch {
	case q.Type == oap.QEnum:
		return tui.NewChoice(tui.ChoiceOpts{
			QuestionOpts: questionOpts(q),
			Options:      enumChoices(q),
			Default:      func() string { return DefaultString(q) },
		})
	case q.Type == oap.QResourceList && len(q.Enum) > 0:
		// This is the one question type that does NOT honor q.IsRequired: a
		// required list picked empty records the empty list, which Resolve keeps.
		// Left alone rather than tightened here — "required" for a list could
		// mean "answered" or "non-empty", the manifests in the tree do not settle
		// which, and a stricter reading would refuse installs that work today.
		return tui.NewMultiChoice(tui.MultiChoiceOpts{
			QuestionOpts: questionOpts(q),
			Options:      enumChoices(q),
			// Preseed the initial selection from Default, so a declared default
			// is offered rather than silently dropped.
			Default: func() []string { return EnumAnswerValues(q.Default) },
		})
	default:
		o := tui.TextOpts{
			QuestionOpts: questionOpts(q),
			Default:      func() string { return DefaultString(q) },
			// An optional question may be answered with nothing, which Resolve
			// then leaves unanswered rather than overlaying a CR field with an
			// empty value. A required one with no default is refused instead:
			// recording the blank would put an empty value on the resource and
			// report the install a success.
			Optional: !q.IsRequired(),
		}
		if q.Type == oap.QSecret {
			// The same field in every respect except what is drawn, so a
			// secret cannot drift from a plain text answer as TextOpts grows.
			return tui.NewSecret(caps, o)
		}
		return tui.NewText(o)
	}
}

// questionOpts is what every manifest question shares, whatever its type.
//
// Skip is where oap.Question.AskWhen becomes behavior, and this is the ONE
// place a terminal client turns a declared question into a prompt — so a
// branching question set is honored identically wherever it came from. A
// question with no gate reads its answer out of a State that has none and
// applies, so the closure costs an ungated question nothing but a nil check.
func questionOpts(q oap.Question) tui.QuestionOpts {
	return tui.QuestionOpts{
		ID:    q.Name,
		Label: railLabel(q),
		Key:   q.Name,
		Title: q.Prompt,
		// Read from the LIVE State at Prepare time, not from a value captured
		// here: the answer this gate reads is recorded by a screen earlier in
		// the same run, which is precisely the answer that does not exist when
		// the question set is stated.
		Skip: func(st *tui.State) bool { return !q.Applies(st.Get) },
		// The description is the bundle author's prose, so it is left to wrap
		// rather than cut: a wrapped sentence is untidy, and cutting one would
		// drop the half that says what the answer is for.
		Guidance: func(*tui.State) string { return q.Description },
	}
}

// enumChoices offers a question's declared values as rows, each carrying the
// label the operator reads.
//
// The label is the value itself unless the question declared EnumLabels, which
// is the common case: a manifest names each value once and shows it verbatim.
// A question whose stored answer is a stable key rather than a sentence — a
// channel wizard's route, whose "false"/"true"/"provision" spellings
// `--answer` already depends on — declares labels instead, and the pairing
// rule lives on oap.Question so every renderer applies the same one.
func enumChoices(q oap.Question) []tui.Choice {
	out := make([]tui.Choice, 0, len(q.Enum))
	for i, e := range q.Enum {
		out = append(out, tui.Choice{Label: q.EnumLabelFor(i), Value: e})
	}
	return out
}

// railLabel names a question in the step rail: its prompt, cut to what the rail
// can carry. Falling back to Name is only for a manifest that gave the question
// no prompt — showing it otherwise puts `capacity.memory` in the rail while the
// field beside it asks "How much memory should this agent get?", and the rail
// is the one part of the frame meant to say where the user is.
//
// The cut itself is tui.RailLabel's, not this package's: the budget belongs to
// the rail that has to draw the label, and a second vocabulary now feeds the
// same rail (a declared Channel's name, in `oap agent install`'s channels
// pass). Two copies of the number would let one of them grow and take the rail
// away from every step of both.
func railLabel(q oap.Question) string {
	p := strings.TrimSpace(q.Prompt)
	if p == "" {
		return q.Name
	}
	return tui.RailLabel(p)
}

// DefaultString returns q.Default as the string a scalar question is preseeded
// with. Without it a question carrying a Default (a synthesized capacityfit
// clamp) shows an EMPTY field on the interactive path — worse, an empty answer
// to a required QString then fails its own Validation with a raw CEL/quantity
// error instead of quietly accepting the value the question already proposed.
// Non-string or absent Defaults preseed "": a QString's Default is a string by
// construction, so this is defensive, not a swallowed type mismatch.
//
// Exported because a question is preseeded by a FORM as well as by a terminal:
// admind's channel-setup route has to answer an unsupplied question with the
// same value this screen would have shown, or the same declaration produces a
// different Channel depending on which client set it up.
func DefaultString(q oap.Question) string {
	v, _ := q.Default.(string)
	return v
}

// EnumAnswerValues flattens an answer into the string values to check against
// an enum (or to seed a multi-select): a scalar string yields one value; a
// []string / []any yields each of its string elements. Non-string elements and
// non-slice/non-string answers yield nothing.
//
// Exported because pkg/platform/oap/install's validateEnums checks a --set /
// --values answer against the same enum this package seeds a multi-select's
// Default from — one flattening rule serves both rather than drifting into
// two the day either caller's answer shapes diverge.
func EnumAnswerValues(v any) []string {
	switch a := v.(type) {
	case string:
		return []string{a}
	case []string:
		return append([]string(nil), a...)
	case []any:
		out := make([]string, 0, len(a))
		for _, e := range a {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
