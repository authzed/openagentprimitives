package oap

import (
	"fmt"
	"strings"
)

// ReservedQuestionPrefix namespaces the capacity-clamp questions this system
// synthesizes at install time. A bundle declaring one would collide in
// Resolve's answer map — which is keyed by name — and silently overwrite the
// synthesized answer or be overwritten by it.
//
// It is ALSO the split point SplitReserved uses, because these are the
// questions install.Install's ExtraQuestions hook resolves in a second pass —
// which is a narrower thing than "synthesized". A synthesized name that the
// manifest's own Resolve owns must NOT start with this prefix; see
// RequiredSecretQuestionPrefix.
const ReservedQuestionPrefix = "capacity."

// RequiredSecretQuestionPrefix namespaces the questions install synthesizes for
// a requires.secrets[] entry the cluster does not already satisfy — one per
// declared key, named by RequiredSecretQuestionName.
//
// Deliberately NOT ReservedQuestionPrefix: these are answered by the same
// Resolve call the bundle's own questions go through (they materialize into the
// SecretSpecs Install creates), whereas SplitReserved routes the capacity
// prefix into ExtraQuestions' separate pass. Sharing one prefix would send
// `--set requires.secrets.…=…` to the resolver that owns neither name and fail
// as an unknown answer key.
const RequiredSecretQuestionPrefix = "requires.secrets."

// reservedQuestionPrefixes is every prefix this system synthesizes question
// names under. ValidateQuestions refuses a manifest question using any of
// them, so a bundle can never collide with a synthesized answer key.
var reservedQuestionPrefixes = []string{ReservedQuestionPrefix, RequiredSecretQuestionPrefix}

// RequiredSecretQuestionName is the answer key for one declared (Secret, key)
// pair — the exact string an operator types after `--set`.
//
// Derived from the declaration rather than authored, so a bundle that names a
// Secret and its keys gets a stable, greppable key without writing a question
// at all. The Secret name is the DECLARED one, never a --name-prefixed one:
// Install renames the Secrets it creates alongside the CRs that reference them,
// so the answer key stays the same whatever instance name an operator installs
// under.
func RequiredSecretQuestionName(secret, key string) string {
	return RequiredSecretQuestionPrefix + secret + "." + key
}

// reservedQuestionPrefixFor returns the reserved prefix name starts with, or ""
// when it starts with none.
func reservedQuestionPrefixFor(name string) string {
	for _, p := range reservedQuestionPrefixes {
		if strings.HasPrefix(name, p) {
			return p
		}
	}
	return ""
}

// SplitReserved divides a --set/form-value map into the subset naming
// synthesized ReservedQuestionPrefix questions (capacity.<class>.<dim>, which
// install.Install's ExtraQuestions resolve) and everything else — a bundle's
// own manifest questions PLUS the requires.secrets questions install
// synthesizes, both of which the manifest-side Resolve owns. Each Resolve
// rejects keys it does not own, so one unfiltered map handed to both would fail
// "unknown answer key" on whichever side does not own that name. Every surface
// answering both kinds from one map
// (the CLI's --set, admind's posted form values) needs this split, so it lives
// here rather than in any one of them.
func SplitReserved(m map[string]string) (other, reserved map[string]string) {
	other = make(map[string]string, len(m))
	reserved = make(map[string]string, len(m))
	for k, v := range m {
		if strings.HasPrefix(k, ReservedQuestionPrefix) {
			reserved[k] = v
		} else {
			other[k] = v
		}
	}
	return other, reserved
}

type QuestionType string

const (
	QString       QuestionType = "string"
	QInt          QuestionType = "int"
	QBool         QuestionType = "bool"
	QEnum         QuestionType = "enum"
	QSecret       QuestionType = "secret"
	QResourceList QuestionType = "resourceList"
)

// Question is one install-time prompt whose answer overlays onto CR fields
// (non-secret) or drives Secret creation/binding (secret).
type Question struct {
	// The answer-map key, and the name a --set / --values entry must use.
	Name        string       `json:"name"`
	Type        QuestionType `json:"type"`
	Prompt      string       `json:"prompt"`
	Description string       `json:"description,omitempty"`
	// Pre-filled answer, and the non-interactive fallback when nothing else answers.
	Default any `json:"default,omitempty"`
	// nil means required — only an explicit false makes a question optional.
	Required *bool `json:"required,omitempty"`
	// The allowed values; required for type=enum, and enforced on every source.
	Enum []string `json:"enum,omitempty"`
	// EnumLabels is what the operator READS for each Enum entry, positionally.
	//
	// Empty means the value is its own label, which is the common case and what
	// every bundle manifest in the tree relies on. It exists because a value
	// that is a stable answer key is routinely not a sentence a human should be
	// asked to choose between: a channel wizard offering the Slack-app routes
	// stores "false" / "true" / "provision" — spellings `--answer slackapp=true`
	// already depends on — while what belongs on the row is "Show me the
	// manifest — I'll create it myself". Without this the operator picks from
	// raw values, and a default of "false" reads as agreeing to a row labelled
	// false.
	//
	// Non-empty MUST be the same length as Enum, checked by both validators:
	// the pairing is positional, so a short list would silently label the wrong
	// rows rather than fail.
	EnumLabels []string `json:"enumLabels,omitempty"`
	// CEL boolean over `args` (the whole answer map); empty skips validation.
	Validation string `json:"validation,omitempty"`
	// Where the answer lands; at least one is required for a non-secret question.
	Binding []Binding `json:"binding,omitempty"`
	// Required for type=secret, and rejected on every other type.
	Secret *SecretQuestion `json:"secret,omitempty"`

	// Unchanged marks a Default that merely reaffirms a value already applied on
	// the cluster rather than proposing a new one — pkg/platform/capacityfit's
	// clamp question when the installed SpiceboxClass already carries an in-range
	// value, so Default is that live value read back verbatim. Answering with it
	// is then a byte-identical SSA no-op, not a decision. json:"-" because only
	// an ExtraQuestions producer sets it: no manifest author writes it and no
	// caller reads it back off the wire. False — "not proven unchanged" — is the
	// safe default every ordinary manifest question keeps.
	Unchanged bool `json:"-"`

	// AskWhen gates whether this question is PUT to whoever is answering, on
	// the answer to an EARLIER question in the same batch. Its zero value asks
	// always, which is what every question that does not branch carries.
	//
	// It exists because a question set is stated in ONE batch, before any of it
	// is answered, and some sets genuinely branch: the slack channel wizard
	// asks whether the operator already has a Slack app, and the credentials
	// the rest of the run needs are a different PAIR for each answer. Without a
	// gate the batch can only be shaped from what a FLAG already settled, so an
	// interactive operator — who settles it at the prompt, one question later —
	// is asked for whichever pair the flags happened to imply. That is not
	// hypothetical: picking "create the app for me" asked for the two tokens
	// that route exists to mint, and never asked how to authenticate with the
	// API that mints them.
	//
	// DECLARED IS NOT THE SAME AS ASKED, and separating them is the point. A
	// gated question is still part of the set, so `--answer <name>=…` for it is
	// still accepted — a client builds its allowed key set from the declared
	// questions — and its answer still reaches whoever reads the map. Only the
	// PROMPT is conditional.
	//
	// json:"-" for the same reason Unchanged is: only a producer sets it — a
	// channel kind's Wizard.Inputs — and no manifest author writes one. A
	// bundle manifest therefore cannot declare a gate, and no manifest renderer
	// has to grow one.
	//
	// HONORED WHERE A QUESTION BECOMES A PROMPT, which is one place per client.
	// questionscreen.NewScreen turns it into tui.QuestionOpts.Skip, so a
	// terminal neither shows the screen nor records an answer for it. A
	// one-shot form that draws the whole batch at once cannot un-draw a field —
	// admind's install form renders the superset — so it applies the gate where
	// it decides what is still OWED (unansweredChannelQuestions) and leaves an
	// inapplicable field unused.
	AskWhen AskWhen `json:"-"`
}

// AskWhen is one question's gate: ask it only when an earlier question in the
// same batch was answered with one of In.
//
// DATA rather than a predicate function, so a question set stays a value a
// server can hold, validate and re-read — the same property that keeps
// Question free of any terminal type. The zero value gates nothing.
type AskWhen struct {
	// Question is the Name of an EARLIER question in the same batch. Earlier
	// matters: answers arrive in declaration order, so a gate naming a later
	// question would be read before that question had an answer and would fail
	// closed on every run. channelkinds.ValidateInputs enforces it.
	Question string
	// In is the answers this question is asked for. Empty alongside a non-empty
	// Question is a gate that can never open, which is refused rather than
	// silently hiding the question forever.
	In []string
}

// Applies reports whether this question is asked at all, given a reader of the
// answers collected so far. A question with no gate always applies.
//
// answer is a GETTER rather than a map because the clients hold their answers
// in different shapes — a *tui.State mid-run, a plain map at submit time — and
// the rule for reading a gate must not be written twice. A nil getter is a
// caller with no answers at all, which no gate can be satisfied from.
//
// The comparison trims what it reads and compares against the declared values
// verbatim: an enum's stored answer is the value the producer declared, and
// every widget in this tree trims what it records.
func (q Question) Applies(answer func(key string) string) bool {
	if q.AskWhen.Question == "" {
		return true
	}
	if answer == nil {
		return false
	}
	got := strings.TrimSpace(answer(q.AskWhen.Question))
	for _, want := range q.AskWhen.In {
		if got == want {
			return true
		}
	}
	return false
}

// Binding points an answer at one CR field. See target.go for the syntax.
type Binding struct {
	// "Kind/name#path.to.field", resolved against the bundle's own CRs.
	Target string `json:"target"`
}

// SecretQuestion declares how a secret answer is satisfied: by creating a
// Secret from the entered value, by binding an existing Secret, or both.
type SecretQuestion struct {
	// Where the answered value is written; nil makes an answered question fatal.
	CreateSecret *SecretTarget `json:"createSecret,omitempty"`
	// Accepts a secret block with no createSecret; Resolve still refuses the answer.
	OrExisting bool `json:"orExisting,omitempty"`
}

// SecretTarget is the Secret name and data key one secret answer is written to.
type SecretTarget struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// IsRequired defaults to true unless explicitly set to false.
func (q Question) IsRequired() bool { return q.Required == nil || *q.Required }

// EnumLabelFor is what the operator READS for the Enum entry at i: its
// EnumLabels entry when one was declared and non-blank, the value itself
// otherwise.
//
// One pairing rule, exported so every renderer uses it rather than each one
// deciding for itself what an absent or blank label means — a question with
// labels and one without must be presented by the same rule or the two look
// like different widgets.
func (q Question) EnumLabelFor(i int) string {
	if i < len(q.EnumLabels) && strings.TrimSpace(q.EnumLabels[i]) != "" {
		return q.EnumLabels[i]
	}
	if i < len(q.Enum) {
		return q.Enum[i]
	}
	return ""
}

// ValidateEnumLabels rejects a label list that is present but not the same
// length as Enum. The pairing is positional, so a short or long list would
// silently label the wrong rows — the operator would read one option and
// answer another — rather than fail.
//
// Shared by Manifest.ValidateQuestions and by the channel contract's own
// narrower validator, so a bundle question and a channel input are held to the
// same rule.
func (q Question) ValidateEnumLabels() error {
	if len(q.EnumLabels) == 0 || len(q.EnumLabels) == len(q.Enum) {
		return nil
	}
	return fmt.Errorf("question %q: enumLabels has %d entries but enum has %d; the pairing is positional, so they must match",
		q.Name, len(q.EnumLabels), len(q.Enum))
}

// ValidateQuestions checks structural well-formedness of every question.
func (m *Manifest) ValidateQuestions() error {
	seen := map[string]bool{}
	for i, q := range m.Questions {
		if q.Name == "" {
			return fmt.Errorf("questions[%d]: name is required", i)
		}
		if seen[q.Name] {
			return fmt.Errorf("questions[%d]: duplicate question name %q", i, q.Name)
		}
		seen[q.Name] = true
		if p := reservedQuestionPrefixFor(q.Name); p != "" {
			return fmt.Errorf("questions[%d]: name %q uses the reserved %q prefix", i, q.Name, p)
		}
		switch q.Type {
		case QString, QInt, QBool, QEnum, QSecret, QResourceList:
		default:
			return fmt.Errorf("question %q: unknown type %q", q.Name, q.Type)
		}
		if q.Type == QEnum && len(q.Enum) == 0 {
			return fmt.Errorf("question %q: type=enum requires enum values", q.Name)
		}
		if err := q.ValidateEnumLabels(); err != nil {
			return err
		}
		if q.Type == QSecret {
			if q.Secret == nil {
				return fmt.Errorf("question %q: type=secret requires a secret block", q.Name)
			}
			if q.Secret.CreateSecret == nil && !q.Secret.OrExisting {
				return fmt.Errorf("question %q: secret must allow createSecret and/or orExisting", q.Name)
			}
		} else if q.Secret != nil {
			return fmt.Errorf("question %q: secret block only valid for type=secret", q.Name)
		}
		if q.Type != QSecret && len(q.Binding) == 0 {
			return fmt.Errorf("question %q: non-secret question needs at least one binding", q.Name)
		}
	}
	return nil
}
