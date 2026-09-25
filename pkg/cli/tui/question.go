package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"
)

// Question is a screen with the shape nearly every wizard step in this project
// turned out to have: a block of guidance the user reads, then exactly ONE
// thing they answer.
//
// It exists because that shape was implemented five times — in flowscreens, in
// idpscreens, in the channel wizard, in the bundle-manifest questions and in
// `oap agent install`'s build questions — and the copies had already drifted on
// the details that matter: whether an answer already in State was re-asked,
// whether a value was re-checked when no validator had run, whether a
// credential reached the summary, and how an address too wide for a note was
// delivered. Those are not per-caller decisions; they are one set of answers,
// and this is where they live.
//
// A screen with two interactive fields is deliberately NOT expressible here.
// The one place that wanted several (`oap settings wizard`) reads better as its
// own screens, and a group with two fields cannot report which of them a
// fail-closed run was missing.
type Question struct {
	opts QuestionOpts

	// values is what a multi-choice question binds. Declared beside the scalar
	// bindings below rather than in a separate type, because which one a screen
	// uses is settled by its constructor and nothing else varies.
	values []string

	// field builds the huh field on each Prepare, bound to this screen's value.
	// A *huh.Group is stateful and must never be reused, so this is a factory
	// rather than a built field.
	field func() huh.Field

	// prepareValue binds the starting value, applyValue records the answer, and
	// validate rejects a screen that cannot be built at all. Set by the
	// constructors, which is what makes the four question kinds one type
	// rather than four with a copied Prepare.
	prepareValue func(*State)
	applyValue   func(*State) error
	validate     func() error

	// state is the run's State, captured at Prepare so a field validator can
	// check a typed value against an EARLIER answer. huh's Validate takes only
	// the typed string, so there is nowhere else for it to come from.
	state *State

	// value and boolValue are what the field binds; which one depends on the
	// constructor. dflt is what value was pre-filled with, captured at Prepare
	// so Apply and the validator agree on what a bare Enter meant even if the
	// caller's Default is not a pure function.
	value     string
	boolValue bool
	dflt      string

	// overflowed records the addresses the question WAS asked with that the note
	// could only carry cut, so Apply owes the summary a line carrying each one
	// whole.
	//
	// Set in Prepare and read in Apply rather than noted directly: Prepare
	// composes what the user reads and must not mutate the run's record, and a
	// question answered ahead of time renders no guidance and so has no address
	// to have cut.
	overflowed []Address
}

// Address is a URL offered alongside a question — where a token is minted, the
// redirect URI to register, a page of documentation.
//
// It is carried here rather than composed into Guidance so that ONE rule
// decides how it is delivered:
//
//   - It always goes into the guidance block: whole when the note can carry it,
//     and cut at the budget with the cut marked when it cannot.
//   - When it was cut, the whole address also goes into the post-run summary,
//     which RenderSummary writes to plain scrollback and wraps nothing.
//
// A question must name what it is about. Withholding an over-wide address
// entirely leaves the user acting on something the question does not identify —
// "Open this page in your browser?" naming no page, or guidance reading "add
// the address below" with nothing below it. A cut address still says which
// host and which page, and the trailing ellipsis says it is incomplete, so
// nobody copies it believing it whole. A WRAPPED address is the one that is
// actually dangerous: huh breaks an over-long line at the budget with no sign
// that the second half belongs to the first, so what reaches the user looks
// like two addresses and neither of them works.
//
// A caller whose user must COPY the address rather than merely recognise it —
// a redirect URI being registered with a provider, as against a page being
// opened — asks Fits before the run and prints it itself, where a terminal
// soft-wraps it and a copy still yields one usable line. That is a caller's
// decision because only the caller knows whether its question can be answered
// without the address in hand.
type Address struct {
	// Label names the address to the user ("Docs", "Redirect URI").
	Label string
	// URL is the address itself.
	URL string
}

// Fits reports whether a note can carry this address whole, at the given
// budget. Callers on both sides of that decision — the screen composing
// guidance and a command deciding what to print outside the form — go through
// it, so the two cannot disagree about which addresses fit.
func (a Address) Fits(b NoteBudget) bool {
	return strings.TrimSpace(a.URL) != "" && len(b.Overflows(a.whole())) == 0
}

// whole renders the address as its own two lines, uncut. It is what Fits
// measures and what the summary carries.
//
// The label goes on a line of its own so the URL gets the note's whole width.
// Inline, a label like "Redirect URI: " spends fourteen columns on text nobody
// copies — enough to push an ordinary cluster address over the budget, and
// enough to cost the path segment that says which page it points at.
func (a Address) whole() string {
	url := strings.TrimSpace(a.URL)
	if url == "" {
		return ""
	}
	label := strings.TrimSpace(a.Label)
	if label == "" {
		return url
	}
	return label + ":\n" + url
}

// block renders the address for a note, cutting each line to the budget.
//
// Cutting rather than wrapping, through the one Fit in this package: a marked
// cut is honest about being incomplete where a wrap looks whole.
func (a Address) block(b NoteBudget) string { return b.Fit(a.whole()) }

// QuestionOpts is everything the four question kinds share.
type QuestionOpts struct {
	// ID names the screen to machines — the step rail keys off it, and every
	// error the sequencer wraps is attributed to it. Label names it to the user
	// and is the rail's step text.
	//
	// Keep Label at 12 columns or fewer: the rail is
	// max(railMinWidth, longest label + 2) with railMinWidth 14, so a label of
	// 12 or less costs the body nothing, and the body is what RailedNoteBudget
	// measures guidance against. A 13-column label silently narrows every note
	// in the wizard by one column.
	ID, Label string

	// Key is the State key the answer lands under. It is also what a caller
	// seeds to answer this screen ahead of time, so it is part of the flow's
	// public vocabulary and must stay stable.
	Key string

	// Title is the question itself, rendered on the field.
	Title string

	// Guidance is the block shown above the field. A function of State rather
	// than a string so a screen can say something different depending on what
	// happened earlier in the run — that a browser refused to open, say. Nil
	// renders no block.
	//
	// Every line of it must fit Budget; callers assert that in their own tests,
	// because only they know what their guidance says.
	Guidance func(*State) string

	// Addresses are the URLs offered with the question, in the order they are
	// shown. Empty offers none, and an entry with a blank URL is skipped — so a
	// caller whose address is conditional passes the zero Address rather than
	// branching on the slice.
	//
	// A slice rather than one address because a single question can
	// legitimately be about more than one — an OAuth confirmation naming both
	// the server being logged into and the redirect URI that server must
	// already know about — and each entry is subject to the same delivery rule.
	Addresses []Address

	// Budget is the note budget this screen's guidance is measured against.
	// Zero means RailedNoteBudget, which is what a wizard with a step rail
	// gets — and every Flow and Wizard in this project has one.
	Budget NoteBudget

	// Skip reports that this question does not apply to the run in progress, as
	// decided by what earlier screens answered or discovered. Nil asks always.
	//
	// Distinct from an answered Key, which means "asked and already known": a
	// skipped screen is not part of this run at all, so it neither prompts nor
	// records, and a fail-closed run must not report its Key missing.
	Skip func(*State) bool

	// NoteLabel is the summary line's label. Empty records no line.
	NoteLabel string

	// NoteValue renders the summary line's value from a NON-EMPTY answer. An
	// empty one never reaches it; see NoteAbsent.
	//
	// Nil records the answer verbatim, which is right for an issuer or a client
	// id and CATASTROPHIC for a credential: the summary is written to plain
	// scrollback after the run, where it outlives the terminal the value was
	// typed into and lands in any redirected log. A field that collects
	// something authenticating supplies a function that describes the outcome,
	// or masks it — masking being insufficient for a value like a password,
	// whose last four characters are four characters of the password.
	//
	// Empty answers are withheld from it deliberately, and the reason is a bug
	// this shape used to have: every masking caller passes credmask.Mask, which
	// returns "****" for anything under twelve characters INCLUDING "". A
	// masker handed an empty value therefore reports a redacted secret where
	// there is no secret at all — hiding nothing and misstating something. A
	// caller that has a real answer to give for "empty" gives it as NoteAbsent,
	// which is a different question and now looks like one.
	NoteValue func(string) string

	// NoteAbsent is the summary line's value when the answer is empty. Empty
	// means "not set".
	//
	// It exists because an empty answer is sometimes the informative one: a
	// blank client secret says the client is public rather than confidential,
	// and a blank password says the stored one was kept. Both are what a user
	// re-reads the summary to find out, and neither is expressible by a
	// function that never sees the value.
	NoteAbsent string
}

// TextOpts describes a question answered by typing.
type TextOpts struct {
	QuestionOpts

	// Default pre-fills the field, which makes an empty answer mean "keep it".
	// Nil, or a function returning "", makes the answer required.
	Default func() string

	// Optional accepts an empty answer as an answer, for a field the run itself
	// makes conditional — an OAuth client secret that public clients do not
	// have, a stored password being kept.
	//
	// The recorded answer is the empty string rather than nothing at all, so
	// State.Has stays true and "answered with nothing" stays distinguishable
	// from "never asked".
	Optional bool

	// Check rejects an unusable answer, and is what the user reads when they
	// mistype one.
	//
	// It takes the run's State because some checks are about an EARLIER answer
	// rather than about the value alone — a password confirmation is only
	// correct relative to the password above it. Nil accepts anything non-empty.
	Check func(st *State, v string) error
}

// ChoiceOpts describes a question answered by picking one of a fixed set.
type ChoiceOpts struct {
	QuestionOpts

	// Options are the choices, in the order the user sees them.
	Options []Choice

	// Default picks the initially-selected option by value. Nil, or a value
	// matching no option, selects the first.
	//
	// Whichever option this resolves to is the one SILENCE chooses: huh's
	// accessible renderer returns the bound value when its input runs out, and
	// reports no error doing so. A screen whose choices are not equally safe
	// must make sure this resolves to the safe one — and when it is nil, that
	// the first option is.
	Default func() string
}

// MultiChoiceOpts describes a question answered by picking any number of a
// fixed set — none of them included.
type MultiChoiceOpts struct {
	QuestionOpts

	// Options are the choices, in the order the user sees them.
	Options []Choice

	// Default pre-selects options by value; a value matching no option is
	// dropped rather than pre-selecting nothing, so one stale entry in a stored
	// answer cannot silently discard the rest. Nil pre-selects nothing.
	//
	// Whatever this resolves to is what SILENCE answers with: huh's accessible
	// renderer returns the bound selection when its input runs out, and reports
	// no error doing so. A question whose selections are not equally safe must
	// make sure this resolves to the safe set — and when it is nil, that the
	// empty set is.
	Default func() []string
}

// Choice is one option offered by a choice question.
type Choice struct{ Label, Value string }

// ConfirmOpts describes a yes/no question.
//
// It carries no Check: there is no unusable answer to a question with two
// answers. Default is the value silence takes, so a confirm guarding something
// destructive defaults to false — see ChoiceOpts.Default for why that matters.
type ConfirmOpts struct {
	QuestionOpts
	Default bool
}

// NewText returns a question answered by typing.
func NewText(o TextOpts) *Question {
	q := &Question{opts: o.QuestionOpts}
	q.field = func() huh.Field {
		// No EchoModePassword, even for a credential. That mode needs a reader
		// with a terminal file descriptor, and huh's accessible renderer
		// discards the error when there is none: off-TTY the field would ask
		// nothing, accept nothing and report nothing, leaving an empty
		// credential behind.
		return huh.NewInput().
			Key(o.Key).
			Title(o.Title).
			Value(&q.value).
			Validate(func(in string) error { return q.checkTyped(o, in) })
	}
	q.prepareValue = func(st *State) {
		q.dflt = ""
		if o.Default != nil {
			q.dflt = strings.TrimSpace(o.Default())
		}
		q.value = q.dflt
	}
	q.applyValue = func(st *State) error { return q.applyText(o, st) }
	return q
}

// NewChoice returns a question answered by picking one of a fixed set.
func NewChoice(o ChoiceOpts) *Question {
	q := &Question{opts: o.QuestionOpts}
	q.field = func() huh.Field {
		return huh.NewSelect[string]().
			Key(o.Key).
			Title(o.Title).
			Options(huhOptions(o.Options)...).
			Value(&q.value)
	}
	q.prepareValue = func(st *State) {
		if len(o.Options) > 0 {
			q.value = o.Options[0].Value
		}
		if o.Default != nil {
			if d := strings.TrimSpace(o.Default()); knownChoice(o.Options, d) {
				q.value = d
			}
		}
	}
	q.applyValue = func(st *State) error { return q.applyChoice(o, st) }
	q.validate = func() error { return requireOptions(o.Label, o.Options) }
	return q
}

// NewMultiChoice returns a question answered by picking any number of a fixed
// set.
func NewMultiChoice(o MultiChoiceOpts) *Question {
	q := &Question{opts: o.QuestionOpts}
	q.field = func() huh.Field {
		return huh.NewMultiSelect[string]().
			Key(o.Key).
			Title(o.Title).
			Options(huhOptions(o.Options)...).
			Value(&q.values)
	}
	q.prepareValue = func(st *State) {
		q.values = nil
		if o.Default == nil {
			return
		}
		for _, v := range o.Default() {
			if v = strings.TrimSpace(v); knownChoice(o.Options, v) {
				q.values = append(q.values, v)
			}
		}
	}
	q.applyValue = func(st *State) error { return q.applyMultiChoice(o, st) }
	q.validate = func() error { return requireOptions(o.Label, o.Options) }
	return q
}

// NewConfirm returns a yes/no question.
func NewConfirm(o ConfirmOpts) *Question {
	q := &Question{opts: o.QuestionOpts}
	q.field = func() huh.Field {
		return huh.NewConfirm().
			Key(o.Key).
			Title(o.Title).
			Value(&q.boolValue)
	}
	q.prepareValue = func(st *State) { q.boolValue = o.Default }
	q.applyValue = func(st *State) error {
		if st.Has(o.Key) {
			q.boolValue = st.Bool(o.Key)
		}
		st.SetBool(o.Key, q.boolValue)
		q.note(st, yesNo(q.boolValue))
		// Same debt the other kinds settle: guidanceBlock cuts an address a
		// note cannot carry whole, so something has to record the whole one or
		// the tail of it is lost. A confirm carries addresses as readily as a
		// text question does, so omitting this here would be a trap laid for
		// the next caller rather than a case that cannot arise.
		q.noteOverflowedAddresses(st)
		return nil
	}
	return q
}

func (s *Question) ID() string { return s.opts.ID }

func (s *Question) Label() string { return s.opts.Label }

// AnswerKeys declares the key this screen asks for, so a caller can tell a user
// which answer a fail-closed run was missing, and so a test can seed it without
// transcribing a second list. Duck-typed by callers.
func (s *Question) AnswerKeys() []string { return []string{s.opts.Key} }

func (s *Question) Prepare(_ context.Context, st *State) (*huh.Group, error) {
	if s.opts.Skip != nil && s.opts.Skip(st) {
		return nil, ErrSkip
	}
	if s.validate != nil {
		if err := s.validate(); err != nil {
			return nil, err
		}
	}
	s.state = st
	// Run before the answered check: Apply reads the default on the path where
	// the user cleared a pre-filled field.
	s.prepareValue(st)
	if st.Has(s.opts.Key) {
		return nil, nil
	}

	fields := make([]huh.Field, 0, 2)
	if g := s.guidanceBlock(st); g != "" {
		// A note's TITLE, never its Description. Not for visibility — huh's
		// accessible renderer does print a static description — but because on
		// a TTY huh runs a description through a markdown renderer that reads
		// '_' as an italic toggle, which mangles URLs, token prefixes and flag
		// names. A title renders verbatim under both drivers.
		fields = append(fields, huh.NewNote().Title(g))
	}
	fields = append(fields, s.field())
	return huh.NewGroup(fields...), nil
}

func (s *Question) Apply(_ context.Context, st *State) error {
	return s.applyValue(st)
}

// budget is the note budget this screen measures against.
func (s *Question) budget() NoteBudget {
	if s.opts.Budget == 0 {
		return RailedNoteBudget()
	}
	return s.opts.Budget
}

// guidanceBlock composes what the user reads above the field: the caller's
// guidance, then each address, cut to the budget when it does not fit whole.
//
// It also records, for Apply, which addresses had to be cut — the one decision
// this function makes that outlives the render.
func (s *Question) guidanceBlock(st *State) string {
	s.overflowed = nil
	var blocks []string
	if s.opts.Guidance != nil {
		if g := strings.TrimSpace(s.opts.Guidance(st)); g != "" {
			blocks = append(blocks, g)
		}
	}
	budget := s.budget()
	for _, a := range s.opts.Addresses {
		block := a.block(budget)
		if block == "" {
			continue
		}
		if !a.Fits(budget) {
			s.overflowed = append(s.overflowed, a)
		}
		blocks = append(blocks, block)
	}
	return strings.Join(blocks, "\n\n")
}

// checkTyped checks a typed line as the user typed it.
//
// An empty line is accepted when the field was pre-filled, because that is how
// a user accepts the default: huh hands the validator the typed line, not the
// bound value, so rejecting "" would reject the bare Enter that means "keep
// it" — and, under the accessible renderer, would re-prompt and eat the next
// scripted line instead.
func (s *Question) checkTyped(o TextOpts, in string) error {
	v := strings.TrimSpace(in)
	if v == "" {
		if s.dflt != "" || o.Optional {
			return nil
		}
		return s.missing()
	}
	if o.Check == nil {
		return nil
	}
	return o.Check(s.state, v)
}

func (s *Question) applyText(o TextOpts, st *State) error {
	v := strings.TrimSpace(s.value)
	switch {
	case st.Has(o.Key):
		// The question was never asked. A pre-supplied answer still has to be
		// usable: seeding a value is a way to skip the question, not a way to
		// skip the check.
		v = strings.TrimSpace(st.Get(o.Key))
	case v == "":
		// The user cleared a pre-filled field, or their input ran out.
		v = s.dflt
	}
	if v == "" && !o.Optional {
		return s.missing()
	}
	// Re-checked rather than trusted: huh's accessible renderer keeps the last
	// REJECTED value bound when its input runs out mid-field, so a value
	// reaching here has not necessarily passed the validator above. An optional
	// field left empty is not run through it — the check describes the shape of
	// a value, and "no value" is a legitimate answer for such a field.
	if o.Check != nil && v != "" {
		if err := o.Check(st, v); err != nil {
			return err
		}
	}
	st.Set(o.Key, v)
	s.note(st, v)
	s.noteOverflowedAddresses(st)
	return nil
}

func (s *Question) applyChoice(o ChoiceOpts, st *State) error {
	v := strings.TrimSpace(s.value)
	if st.Has(o.Key) {
		v = strings.TrimSpace(st.Get(o.Key))
	}
	// Fail closed on a value no option offered. Reached by a seeded answer that
	// was mistyped, which would otherwise land in the result as a choice nobody
	// made.
	if !knownChoice(o.Options, v) {
		return s.notOffered(o.Title, v, o.Options)
	}
	st.Set(o.Key, v)
	s.note(st, v)
	s.noteOverflowedAddresses(st)
	return nil
}

// applyMultiChoice records the selected set.
//
// The empty set is recorded as an answer rather than as nothing, so "picked
// none of them" stays distinguishable from "never asked" — which is the whole
// difference between an optional question legitimately declined and a
// fail-closed run that lost one.
func (s *Question) applyMultiChoice(o MultiChoiceOpts, st *State) error {
	selected := s.values
	if st.Has(o.Key) {
		selected = st.All(o.Key)
	}
	chosen := make([]string, 0, len(selected))
	for _, v := range selected {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		// Fail closed on a value no option offered, for the same reason the
		// single-choice question does: a seeded answer never went through the
		// rows, and one mistyped entry would land in the result as a selection
		// nobody made.
		if !knownChoice(o.Options, v) {
			return s.notOffered(o.Title, v, o.Options)
		}
		chosen = append(chosen, v)
	}
	st.SetAll(o.Key, chosen)
	s.note(st, strings.Join(chosen, ", "))
	s.noteOverflowedAddresses(st)
	return nil
}

// note records the summary line for v, if this screen keeps one.
func (s *Question) note(st *State, v string) {
	if s.opts.NoteLabel == "" {
		return
	}
	// The empty answer is resolved FIRST, so NoteValue never sees one. See
	// NoteValue for why that ordering is load-bearing rather than tidy.
	if strings.TrimSpace(v) == "" {
		st.Note(s.opts.NoteLabel, orNotSet(s.opts.NoteAbsent))
		return
	}
	shown := v
	if s.opts.NoteValue != nil {
		shown = s.opts.NoteValue(v)
	}
	st.Note(s.opts.NoteLabel, orNotSet(shown))
}

// noteOverflowedAddresses puts every address the note could only carry cut into
// the summary, which is plain scrollback and so carries an address of any
// length whole — the whole reason the guidance block cut it.
func (s *Question) noteOverflowedAddresses(st *State) {
	for _, a := range s.overflowed {
		label := strings.TrimSpace(a.Label)
		if label == "" {
			label = "Address"
		}
		st.Note(label, strings.TrimSpace(a.URL))
	}
}

// missing is the refusal for an answer that never arrived. It names the
// question in the user's words rather than the State key, which is ours.
func (s *Question) missing() error {
	label := s.opts.Title
	if label == "" {
		label = s.opts.Label
	}
	if label == "" {
		return errors.New("a required answer was not supplied")
	}
	return fmt.Errorf("%s: nothing was supplied", label)
}

// notOffered is the refusal both choice kinds give for a value no row offered.
// It names the choices, because the value came from somewhere the user cannot
// see the rows from — a seeded answer, a flag, a stored setting.
func (s *Question) notOffered(title, v string, options []Choice) error {
	return fmt.Errorf("%s: %q is not one of the choices (%s)",
		title, v, strings.Join(choiceValues(options), ", "))
}

// requireOptions refuses a choice question with nothing to choose from, which
// would otherwise render an empty list and record whatever the zero value is.
func requireOptions(label string, options []Choice) error {
	if len(options) == 0 {
		return fmt.Errorf("%s: there is nothing to choose from", label)
	}
	return nil
}

// huhOptions projects this package's rows onto huh's.
func huhOptions(options []Choice) []huh.Option[string] {
	out := make([]huh.Option[string], 0, len(options))
	for _, c := range options {
		out = append(out, huh.NewOption(c.Label, c.Value))
	}
	return out
}

func knownChoice(options []Choice, v string) bool {
	for _, c := range options {
		if c.Value == v {
			return true
		}
	}
	return false
}

func choiceValues(options []Choice) []string {
	out := make([]string, 0, len(options))
	for _, c := range options {
		out = append(out, c.Value)
	}
	return out
}

// orNotSet substitutes a readable stand-in for a blank summary value, which
// otherwise reads as a rendering fault rather than as the answer it is.
func orNotSet(shown string) string {
	if shown == "" {
		return "not set"
	}
	return shown
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}
