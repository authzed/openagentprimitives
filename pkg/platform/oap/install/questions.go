// Package install implements the fail-closed checks and answer-resolution
// that gate a .oap install before any cluster write happens.
package install

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/questionscreen"
	"github.com/authzed/openagentprimitives/pkg/x/celbudget"
)

// SecretSpec is one Secret+key to create at apply time from a resolved
// QSecret answer. Value is the plaintext the user entered — it is never
// placed in oap.Answers, never written to a manifest, and must never be
// logged.
type SecretSpec struct {
	Name  string
	Key   string
	Value string
}

// MissingAnswersError is Resolve's non-interactive refusal: the required
// questions nothing answered, by name, sorted.
//
// Typed rather than a bare fmt.Errorf so a caller can add the way ITS OWN
// surface would answer them — `oap agent install` names --set and --values,
// admind renders the fields into a form, the desktop installer routes to its
// config flow. The resolver cannot name any of those without being wrong on the
// other two, so it names the questions and lets the caller name the remedy.
//
// Error() is the sentence this refusal has always produced; the type is what is
// new.
type MissingAnswersError struct {
	// Names are the question names, which for a synthesized required-secret
	// question spell out the Secret and key it creates.
	Names []string
	// Questions carries the safe, unanswered field shapes for form-based
	// surfaces. A missing question cannot have a default (Resolve would have
	// consumed it), so this never retains a secret answer.
	Questions []oap.Question
}

func (e *MissingAnswersError) Error() string {
	return fmt.Sprintf("missing required question(s) (non-interactive): %v", e.Names)
}

// ResolveOption customizes how Resolve presents the questions it has to ask.
// Options are ignored entirely when interactive is false, which never asks
// anything.
type ResolveOption func(*resolveConfig)

// resolveConfig is how an interactive Resolve reaches the person answering.
type resolveConfig struct {
	driver tui.Driver
	theme  *tui.Theme
}

// PresentOver routes the questions over a caller-owned driver and the theme it
// was built with, instead of the one Resolve resolves for itself.
//
// It exists for a caller that already presents questions of its own over the
// same terminal. The line-oriented driver buffers the stream it reads, so two
// drivers over one piped stdin lose every answer after the first: the second
// finds the bytes already pulled into a buffer it cannot see, reads EOF, and
// takes each field's default — which huh's accessible renderer reports as a
// completed form with a nil error.
func PresentOver(d tui.Driver, th *tui.Theme) ResolveOption {
	return func(c *resolveConfig) { c.driver, c.theme = d, th }
}

// Resolve fills every question's answer from, in priority order, --set
// overrides, --values YAML, and (when interactive) a presented question, then
// CEL-validates each answered question against its Validation rule.
// Secret-typed answers are split out into SecretSpecs and excluded from the
// returned oap.Answers.
//
// Two invariants hold across every answer source:
//
//   - An answer's Go type comes from the question's declared Type, never from
//     the source that supplied it — see putAnswer, which every write into the
//     answer map goes through.
//   - A secret answer this resolver cannot materialize into a Secret is an
//     error, not a silent drop: a question that collects a credential must name
//     the Secret name+key the bundled CRs read.
//
// When interactive is false, any still-unanswered *required* question is a hard
// error listing every one by name — install never hangs waiting for input it
// cannot obtain.
func Resolve(qs []oap.Question, valuesFile string, sets map[string]string, interactive bool, opts ...ResolveOption) (oap.Answers, []SecretSpec, error) {
	seed, err := loadValues(valuesFile)
	if err != nil {
		return nil, nil, err
	}
	return ResolveValues(qs, seed, sets, interactive, opts...)
}

// ResolveValues is Resolve's map-based core. Callers coordinating a dependency
// graph project one node's local values into seed and keep file loading at the
// surface; direct installs retain Resolve's existing file-based API.
func ResolveValues(qs []oap.Question, seed map[string]any, sets map[string]string, interactive bool, opts ...ResolveOption) (oap.Answers, []SecretSpec, error) {
	answers, secrets, missing, err := resolveValues(qs, seed, sets, interactive, false, opts...)
	if err != nil {
		return nil, nil, err
	}
	if missing != nil {
		return nil, nil, missing
	}
	return answers, secrets, nil
}

// resolveValuesPartial is used only by Workflow's opt-in decision-discovery
// pass. It validates every supplied/default value but retains the safe,
// resolved subset when other required answers are absent.
func resolveValuesPartial(qs []oap.Question, seed map[string]any, sets map[string]string, opts ...ResolveOption) (oap.Answers, []SecretSpec, *MissingAnswersError, error) {
	return resolveValues(qs, seed, sets, false, true, opts...)
}

func resolveValues(qs []oap.Question, seed map[string]any, sets map[string]string, interactive, allowMissing bool, opts ...ResolveOption) (oap.Answers, []SecretSpec, *MissingAnswersError, error) {
	var cfg resolveConfig
	for _, o := range opts {
		o(&cfg)
	}
	if seed == nil {
		seed = map[string]any{}
	}

	raw := make(map[string]any, len(qs))
	// Iterated over the questions rather than over seed so a manifest with two
	// bad --values entries reports the first in manifest order, not whichever one
	// map iteration reached first. A seed key naming no question is left out of
	// raw and reported by rejectUnknownKeys below, which reads seed itself.
	for _, q := range qs {
		v, ok := seed[q.Name]
		if !ok {
			continue
		}
		if err := putAnswer(raw, q, v); err != nil {
			return nil, nil, nil, fmt.Errorf("question %q: --values %s: %w", q.Name, q.Name, err)
		}
	}

	for _, q := range qs {
		s, ok := sets[q.Name]
		if !ok {
			continue
		}
		if err := putAnswer(raw, q, s); err != nil {
			return nil, nil, nil, fmt.Errorf("question %q: --set %s: %w", q.Name, q.Name, err)
		}
	}

	// Fail closed on a --set / --values key that names no question — a typo
	// there would otherwise silently no-op (the value is dropped, the question
	// stays unanswered, and the install proceeds with the wrong config). Both
	// sources are checked against the manifest's question names.
	if err := rejectUnknownKeys(qs, sets, seed); err != nil {
		return nil, nil, nil, err
	}

	var missingRequired []string
	var missingQuestions []oap.Question
	var pending []oap.Question
	for _, q := range qs {
		if _, ok := raw[q.Name]; ok {
			continue
		}
		if interactive {
			// Collected rather than asked here: every question this run still
			// needs is presented as one sequence over ONE driver, because that
			// driver is what keeps a line-oriented stream readable past its first
			// answer. Asking each through its own run would start a fresh buffer
			// over bytes the previous one already pulled in, and every question
			// after the first would silently take its default.
			pending = append(pending, q)
			continue
		}
		// Non-interactive: fall back to the manifest's declared Default before
		// treating a required question as missing — a required question WITH a
		// Default is answerable non-interactively without a --set/--values entry.
		if q.Default != nil {
			if err := putAnswer(raw, q, q.Default); err != nil {
				return nil, nil, nil, fmt.Errorf("question %q: default: %w", q.Name, err)
			}
			continue
		}
		if q.IsRequired() {
			missingRequired = append(missingRequired, q.Name)
			missingQuestions = append(missingQuestions, q)
		}
	}
	var missing *MissingAnswersError
	if len(missingRequired) > 0 {
		sort.SliceStable(missingQuestions, func(i, j int) bool { return missingQuestions[i].Name < missingQuestions[j].Name })
		missingRequired = missingRequired[:0]
		for _, question := range missingQuestions {
			missingRequired = append(missingRequired, question.Name)
		}
		missing = &MissingAnswersError{Names: missingRequired, Questions: missingQuestions}
		if !allowMissing {
			return nil, nil, missing, nil
		}
	}

	if len(pending) > 0 {
		asked, err := ask(pending, cfg)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, q := range pending {
			v, ok := asked[q.Name]
			if !ok {
				continue
			}
			// An optional question answered with nothing stays unanswered, so
			// it neither overlays a CR field nor creates an empty Secret.
			if isEmptyAnswer(v) && !q.IsRequired() {
				continue
			}
			if err := putAnswer(raw, q, v); err != nil {
				return nil, nil, nil, fmt.Errorf("question %q: %w", q.Name, err)
			}
		}
	}

	if err := validateEnums(qs, raw); err != nil {
		return nil, nil, nil, err
	}

	var validationErr error
	if allowMissing && missing != nil {
		validationErr = validateAllPartial(qs, raw, missing.Names)
	} else {
		validationErr = validateAll(qs, raw)
	}
	if validationErr != nil {
		return nil, nil, nil, validationErr
	}

	ans := make(oap.Answers, len(raw))
	var secrets []SecretSpec
	for _, q := range qs {
		v, ok := raw[q.Name]
		if !ok {
			continue
		}
		if q.Type != oap.QSecret {
			ans[q.Name] = v
			continue
		}
		sv, ok := v.(string)
		if !ok {
			return nil, nil, nil, fmt.Errorf("question %q: secret answer must be a string, got %T", q.Name, v)
		}
		if q.Secret == nil || q.Secret.CreateSecret == nil {
			// Fail closed rather than dropping the value. The answer was typed by
			// an operator (or supplied as --set/--values) and a secret answer
			// never reaches oap.Answers, so continuing would leave the credential
			// nowhere, the install reporting success, and the applied CRs
			// referencing a Secret nothing created. `orExisting` alone does not
			// make a question answerable: nothing reads it, and binding a
			// differently-named existing Secret needs a secretRef rewrite this
			// installer does not do.
			//
			// The message names the question and never the value — the answer map
			// is still plaintext, and this error reaches `oap`'s stderr and
			// admind's 400 body.
			return nil, nil, nil, fmt.Errorf("question %q: secret question declares no createSecret target, so its answer cannot be materialized into a Secret", q.Name)
		}
		secrets = append(secrets, SecretSpec{
			Name:  q.Secret.CreateSecret.Name,
			Key:   q.Secret.CreateSecret.Key,
			Value: sv,
		})
	}

	return ans, secrets, missing, nil
}

// rejectUnknownKeys errors if any --set or --values key does not name a
// manifest question. Each offending key is reported with its source so a typo
// is easy to locate. Returns nil when every key maps to a question.
func rejectUnknownKeys(qs []oap.Question, sets map[string]string, seed map[string]any) error {
	known := make(map[string]bool, len(qs))
	for _, q := range qs {
		known[q.Name] = true
	}
	var unknown []string
	for k := range sets {
		if !known[k] {
			unknown = append(unknown, "--set "+k)
		}
	}
	for k := range seed {
		if !known[k] {
			unknown = append(unknown, "--values "+k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("unknown answer key(s) with no matching manifest question: %v", unknown)
}

// loadValues reads a YAML map[string]any from valuesFile. Empty path
// returns an empty seed map — --values is optional.
func loadValues(valuesFile string) (map[string]any, error) {
	if valuesFile == "" {
		return map[string]any{}, nil
	}
	data, err := os.ReadFile(valuesFile)
	if err != nil {
		return nil, fmt.Errorf("read --values %s: %w", valuesFile, err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse --values %s: %w", valuesFile, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// putAnswer records q's answer in raw, converted to the Go type q.Type
// promises.
//
// EVERY answer source funnels through here — the --values seed, a --set string,
// the non-interactive Default fallback, and an answer typed at the prompt —
// because which source supplied an answer must not decide its Go type. Convert
// in only some of them and a QInt answered at the prompt reaches the CR overlay
// as the string "5" and the apply is REJECTED on the integer field, while
// `--set n=5` applies cleanly. One converter is what keeps a source added later
// typed by construction rather than by remembering to call it.
func putAnswer(raw map[string]any, q oap.Question, v any) error {
	tv, err := typedAnswer(q, v)
	if err != nil {
		return err
	}
	raw[q.Name] = tv
	return nil
}

// typedAnswer converts one answer into the Go type q.Type declares, whatever
// shape its source handed it in: a string (--set, and every interactive widget,
// which records through tui.State as text), or an already-decoded
// --values/Default scalar (YAML numbers decode as float64).
//
// QString/QEnum/QSecret/QResourceList pass through unchanged — a string is
// already their type, and QResourceList's list shapes ([]string from the
// multi-select, []any from --values) are normalized by oap.Apply. A
// QResourceList takes a single value from --set; building a list from repeated
// --set flags is the CLI layer's concern, not this resolver's.
func typedAnswer(q oap.Question, v any) (any, error) {
	switch q.Type {
	case oap.QInt:
		return int64Answer(v)
	case oap.QBool:
		return boolAnswer(v)
	default:
		return v, nil
	}
}

// int64Answer renders any source's answer to a QInt as the int64 the CR
// overlay needs. int64 rather than int because that is what the unstructured
// tree (and therefore server-side apply) requires for an integer field.
func int64Answer(v any) (int64, error) {
	switch n := v.(type) {
	case string:
		i, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("value %q is not a valid int: %w", n, err)
		}
		return i, nil
	case int:
		return int64(n), nil
	case int32:
		return int64(n), nil
	case int64:
		return n, nil
	case float64:
		// Every number in a --values document arrives here: sigs.k8s.io/yaml
		// converts YAML to JSON and json.Unmarshal decodes a number into any
		// as float64. A fractional or out-of-range one is refused rather than
		// silently truncated to a different answer than the one written.
		//
		// Both bounds are compared in float64, where MaxInt64 rounds UP to
		// 2^63 — so the limit itself is already out of range and the guard is
		// >=, keeping the conversion below inside int64 rather than in Go's
		// implementation-defined territory. MinInt64 is exactly representable,
		// so its own value is in range.
		if n >= math.MaxInt64 || n < math.MinInt64 {
			return 0, fmt.Errorf("value %v is out of range for an int", n)
		}
		i := int64(n)
		if float64(i) != n {
			return 0, fmt.Errorf("value %v is not a whole number", n)
		}
		return i, nil
	default:
		return 0, fmt.Errorf("value of type %T is not a valid int", v)
	}
}

// boolAnswer renders any source's answer to a QBool as a bool.
func boolAnswer(v any) (bool, error) {
	switch b := v.(type) {
	case bool:
		return b, nil
	case string:
		parsed, err := strconv.ParseBool(b)
		if err != nil {
			return false, fmt.Errorf("value %q is not a valid bool: %w", b, err)
		}
		return parsed, nil
	default:
		return false, fmt.Errorf("value of type %T is not a valid bool", v)
	}
}

// questionsTitle is the chrome's title bar for a run of install questions.
// Named for what the operator is doing rather than for the command they got
// here by, because three different surfaces reach this resolver.
const questionsTitle = "Install questions"

// ask presents every still-unanswered question as one sequence and returns
// the answers by question name.
//
// The return values keep the shapes the rest of this file expects: a string
// for the scalar questions, a []string for the multi-choice one. A question
// the user was asked always appears in the map, including one answered with
// nothing — "answered with nothing" and "never asked" must stay
// distinguishable, because only the first can legitimately drop an optional
// question.
func ask(qs []oap.Question, cfg resolveConfig) (map[string]any, error) {
	theme := cfg.theme
	if theme == nil {
		// The fallback for a caller that supplied no presentation at all. It
		// cannot see that caller's --no-color, which is why PresentOver exists
		// and why every interactive caller should use it: a command that asks
		// questions of its own must hand its driver in, or the two disagree
		// about color, width and which stdin buffer is authoritative.
		theme = tui.NewTheme(tui.Detect(os.Stderr, false))
	}

	// The theme's own Caps, resolved BEFORE the screens are built: they are the
	// capabilities this run's driver was chosen from, and a secret question
	// masks only where that driver can honor masking.
	screens := make([]tui.Screen, 0, len(qs))
	for _, q := range qs {
		screens = append(screens, questionscreen.NewScreen(q, theme.Caps))
	}

	// Rooted at Background: Resolve carries no context. A question here blocks
	// on a person and outlives nothing.
	st, err := tui.Run(context.Background(), screens, askOptions(theme, cfg.driver))
	if err != nil {
		// Stripped at the call site that produced the framing: `tui: present
		// screen "cpu":` in front of a sentence the screen already wrote is
		// this resolver's plumbing showing through.
		return nil, fmt.Errorf("ask install questions: %w", tui.UserFacing(err))
	}

	out := make(map[string]any, len(qs))
	for _, q := range qs {
		if q.Type == oap.QResourceList && len(q.Enum) > 0 {
			out[q.Name] = st.All(q.Name)
			continue
		}
		out[q.Name] = st.Get(q.Name)
	}
	return out, nil
}

// askOptions is how a run of install questions is presented.
//
// In is os.Stdin so the fallback driver, like a supplied one, buffers the stream
// ONCE for the whole run. huh builds a fresh greedy scanner per field, so
// leaving In unset — huh's "read os.Stdin yourself" signal — would let the first
// question's scanner swallow every line behind it and leave each later question
// reading EOF, which the accessible renderer reports as a completed form with a
// nil error.
//
// INLINE: a manifest question is asked from the middle of an install, so what
// the caller printed above and below it is what the operator answers against.
// Like In, it is consulted only on the fallback path — a caller that supplied a
// driver has already made this decision, and the two must not disagree.
func askOptions(theme *tui.Theme, driver tui.Driver) tui.Options {
	return tui.Options{
		Theme:  theme,
		Title:  questionsTitle,
		In:     os.Stdin,
		Out:    os.Stderr,
		Inline: true,
		Driver: driver,
	}
}

// isEmptyAnswer reports whether an interactive answer counts as "unanswered",
// so an optional question can be skipped: an empty/absent string, an empty
// slice, or nil.
func isEmptyAnswer(v any) bool {
	switch a := v.(type) {
	case nil:
		return true
	case string:
		return a == ""
	case []string:
		return len(a) == 0
	case []any:
		return len(a) == 0
	default:
		return false
	}
}

// validateEnums enforces enum membership for every ANSWERED question that
// declares an Enum, across ALL answer sources (default / --set / --values /
// interactive). The interactive huh widgets already constrain input to q.Enum,
// but a --set or --values answer would otherwise carry an out-of-enum value
// straight through to the CR overlay. A QEnum answer (a scalar) must equal one
// of q.Enum; an enum-constrained QResourceList answer (a list — or a degenerate
// single --set string) must have every element in q.Enum. Questions with no
// Enum, or with no answer, are skipped.
func validateEnums(qs []oap.Question, raw map[string]any) error {
	for _, q := range qs {
		if len(q.Enum) == 0 {
			continue
		}
		v, ok := raw[q.Name]
		if !ok {
			continue
		}
		allowed := make(map[string]bool, len(q.Enum))
		for _, e := range q.Enum {
			allowed[e] = true
		}
		for _, val := range questionscreen.EnumAnswerValues(v) {
			if allowed[val] {
				continue
			}
			// A secret answer is still plaintext here — Resolve splits QSecret
			// into SecretSpec only after validation — and this error reaches
			// `oap`'s stderr and admind's 400 body. ValidateQuestions permits
			// `enum` on a secret question, so a bundle declaring an enum no
			// answer can satisfy would otherwise get the credential printed back.
			// Name the question and the allowed set, never the typed value.
			if q.Type == oap.QSecret {
				return fmt.Errorf("question %q: value is not one of the allowed values %v", q.Name, q.Enum)
			}
			return fmt.Errorf("question %q: value %q is not one of the allowed values %v", q.Name, val, q.Enum)
		}
	}
	return nil
}

// validateAll CEL-validates every question that has both an answer and a
// non-empty Validation rule, over a single `args` variable bound to the full
// answer map (raw, still-secret values included — a rule that checks a secret's
// shape is legitimate).
//
// The rule's boolean result carries nothing, but an EVALUATION ERROR does:
// cel-go returns a function's error to the caller, and it is wrapped out to
// `oap`'s stderr and admind's 400 body. Every custom function on this env must
// therefore keep its argument out of its error text (see quantityCELFunc). The
// wrap below quotes only the question name and the rule, both bundle-authored.
func validateAll(qs []oap.Question, raw map[string]any) error {
	return validateAllPartial(qs, raw, nil)
}

func validateAllPartial(qs []oap.Question, raw map[string]any, missing []string) error {
	for _, q := range qs {
		if q.Validation == "" {
			continue
		}
		if _, ok := raw[q.Name]; !ok {
			continue
		}
		ok, err := evalValidation(q.Validation, raw)
		if err != nil {
			if validationOnlyNeedsMissingAnswer(err, missing) {
				continue
			}
			return fmt.Errorf("question %q: validation %q: %w", q.Name, q.Validation, err)
		}
		if !ok {
			return fmt.Errorf("question %q: value fails validation rule %q", q.Name, q.Validation)
		}
	}
	return nil
}

func validationOnlyNeedsMissingAnswer(err error, missing []string) bool {
	if err == nil || len(missing) == 0 {
		return false
	}
	message := err.Error()
	for _, name := range missing {
		if strings.Contains(message, "no such key: "+name) || strings.Contains(message, "no such key: '"+name+"'") {
			return true
		}
	}
	return false
}

// evalValidation compiles + runs a CEL boolean expression against the args
// map. Mirrors pkg/authz/scope.evalCEL's env shape (a single `args` dyn
// variable) so question Validation rules read the same as toolguard's arg
// constraints, plus the quantity() function so a rule can range-check a
// Kubernetes quantity answer (e.g. a capacity clamp).
func evalValidation(expr string, args map[string]any) (bool, error) {
	env, err := cel.NewEnv(cel.Variable("args", cel.DynType), quantityCELFunction())
	if err != nil {
		return false, err
	}
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		return false, iss.Err()
	}
	prg, err := env.Program(ast, celbudget.ProgramOptions()...)
	if err != nil {
		return false, err
	}
	out, _, err := prg.Eval(map[string]any{"args": args})
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("must return bool, got %T", out.Value())
	}
	return b, nil
}

// quantityCELFunction registers `quantity(string) -> int` on a CEL env.
// Without it a Validation rule cannot compare Kubernetes quantities at all —
// CEL sees opaque strings, and a lexical compare is silently wrong ("900Mi" >
// "1Gi" as text). MilliValue() is used uniformly so one function serves both
// cpu ("500m") and memory ("1Gi") answers; the ceiling is ~9.2e18 milli-units,
// far above any pod resource quantity.
func quantityCELFunction() cel.EnvOption {
	return cel.Function("quantity",
		cel.Overload("quantity_string",
			[]*cel.Type{cel.StringType},
			cel.IntType,
			cel.UnaryBinding(func(v ref.Val) ref.Val {
				s, ok := v.Value().(string)
				if !ok {
					return types.NewErr("quantity: arg must be string, got %T", v.Value())
				}
				n, err := quantityCELFunc(s)
				if err != nil {
					return types.NewErr("%v", err)
				}
				return types.Int(n)
			}),
		),
	)
}

// quantityCELFunc parses a Kubernetes resource.Quantity and returns its
// milli-scaled value. An unparseable input returns an ERROR rather than a zero:
// a rule that silently evaluated false on a typo'd quantity would reject a good
// answer with a misleading "fails validation rule" instead of the parse error.
//
// The error does NOT quote the input. The bundle chooses which answer its rule
// feeds to quantity(), so `quantity(args.someSecret)` would printf the user's
// credential onto `oap`'s stderr and into admind's 400 body — the answer map is
// still plaintext at validation time. ParseQuantity's own message states the
// expected form, and validateAll already names the question and the rule.
func quantityCELFunc(s string) (int64, error) {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, fmt.Errorf("quantity(): %w", err)
	}
	return q.MilliValue(), nil
}
