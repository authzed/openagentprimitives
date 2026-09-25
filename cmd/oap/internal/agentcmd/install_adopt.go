package agentcmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
)

// adoptAllSentinel is the value pflag records for a BARE `--adopt` (via the
// flag's NoOptDefVal). It cannot collide with a targeted value because every
// targeted value is a "Kind/Name" pair and must contain a slash — splitAdopt
// enforces that.
const adoptAllSentinel = "all"

// splitAdopt turns the raw --adopt values into the two InstallOpts fields:
// whether a blanket adopt was requested, and the explicit "Kind/Name" keys.
// A value that is neither the sentinel nor a well-formed Kind/Name pair is a
// hard error rather than a silently-ignored entry — `--adopt AgentClass` (a
// forgotten name) must not read as "adopt everything", and a structurally
// broken value ("/foo", "Foo/", "Foo/Bar/Baz") must not silently become a key
// that can never match a real Conflict.Key() (see resolveConflicts, which
// only ever consults opts.Adopt when there IS a conflict to check it
// against — a garbage key against a conflict-free install would otherwise
// produce no error and no adoption, with nothing telling the operator their
// value was nonsense).
func splitAdopt(vals []string) (all bool, keys []string, err error) {
	for _, v := range vals {
		if v == adoptAllSentinel {
			all = true
			continue
		}
		if _, _, ok := parseKindName(v); !ok {
			return false, nil, fmt.Errorf("--adopt value %q must be Kind/Name (e.g. --adopt=AgentClass/my-agent), or bare --adopt to adopt every conflicting object", v)
		}
		keys = append(keys, v)
	}
	return all, keys, nil
}

// adoptAwareArgs is ExactArgs(1) plus a rescue for the one mistake --adopt's
// NoOptDefVal makes easy. pflag only accepts a value for such a flag in the
// `--adopt=X` form; written with a space, `--adopt AgentClass/my-agent` parses
// as a bare --adopt PLUS a stray positional, and the bare arity error ("accepts
// 1 arg(s), received 2") gives the operator no idea what actually went wrong.
func adoptAwareArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 1 && cmd.Flags().Changed("adopt") {
		for _, a := range args {
			if kindNamePositional(a) {
				return fmt.Errorf("unexpected argument %q: write a targeted adopt with '=' (--adopt=%s), or bare --adopt to adopt every conflicting object", a, a)
			}
		}
	}
	if len(args) != 1 {
		return fmt.Errorf("accepts 1 arg(s), received %d", len(args))
	}
	return nil
}

// parseKindName splits "Kind/Name" into its two parts, enforcing only the
// bare structural shape every "Kind/Name" consumer needs: exactly one slash,
// with a non-empty side on each end. It is the single shared predicate for
// both splitAdopt's validity check and kindNamePositional's stricter
// look-like-a-mistake heuristic below, so the two can never drift apart on
// what counts as a well-formed pair.
func parseKindName(s string) (kind, name string, ok bool) {
	kind, name, ok = strings.Cut(s, "/")
	if !ok || kind == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return kind, name, true
}

// kindNamePositional reports whether a stray positional looks like the
// "Kind/Name" an operator meant to pass to --adopt: a CapitalCase Kind, one
// slash, then a DNS-1123-ish name. A bundle path ("./demo.oap", "/tmp/x.oap")
// or a registry ref ("ghcr.io/acme/bot:1.0.0") never matches. This is
// deliberately stricter than parseKindName's bare shape check — it must NOT
// fire on a real path/ref, so it layers on the CapitalCase-Kind and
// no-dot/colon-in-Name checks a merely well-formed pair doesn't need.
func kindNamePositional(s string) bool {
	kind, name, ok := parseKindName(s)
	if !ok {
		return false
	}
	if strings.ContainsAny(name, ":.") {
		return false
	}
	if kind[0] < 'A' || kind[0] > 'Z' {
		return false
	}
	for _, r := range kind {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// adoptedNotice renders the seized-objects block for the install summary.
// Adoption overwrites an object this install did not create and is recorded
// nowhere on the object itself, so it is always printed — never folded into
// the "kinds applied" line.
func adoptedNotice(adopted []string) string {
	if len(adopted) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  adopted:         %s\n", strings.Join(adopted, ", "))
	b.WriteString("    (pre-existing objects seized by this install; `oap agent uninstall` will delete them)\n")
	return b.String()
}

// adoptOptionLabel renders one conflict as a multi-select row. A Secret says
// what adopting it actually does — overwriting its data is a different act
// from converging a CR's spec, and the row is the last place to say so.
func adoptOptionLabel(c install.Conflict) string {
	if c.Secret {
		return fmt.Sprintf("%s  — overwrites this Secret's data", c)
	}
	return c.String()
}

// keyAdopt is the State key the choice lands under.
const keyAdopt = "adopt"

// adoptQuestion is the question itself, asked of the operator.
const adoptQuestion = "Adopt these pre-existing objects?"

// adoptGuidance is the block shown above the rows: what answering costs, and
// what answering with nothing means.
//
// It describes the DECISION rather than the keystrokes, because the keystrokes
// differ by driver — the bubbletea form toggles rows with space, the
// line-oriented one takes a row number — and guidance naming one of them is
// wrong on the other.
//
// It also carries what adopting COSTS, which the rows cannot say: a row names
// one object, and "the spec is overwritten and uninstall will delete it" is
// true of the whole answer. Its lines are kept inside the note's column budget;
// TestAdoptGuidanceFitsTheNoteWidth measures that, because huh wraps an
// over-long note line with no indication that the second half belongs to the
// first.
const adoptGuidance = "Adoption is per object: select only the ones this\n" +
	"install should take over. Adopting one overwrites its\n" +
	"spec with this bundle's, and `oap agent uninstall` will\n" +
	"then delete it.\n" +
	"Selecting none aborts the install; nothing is written."

// adoptGuidanceFor prefixes the count and says why these objects are being
// offered at all — the two facts the rows cannot carry, since a row names one
// object and neither states that it already exists outside this install.
func adoptGuidanceFor(n int) string {
	return fmt.Sprintf("%d pre-existing object(s) are not managed by\nthis install.\n\n", n) + adoptGuidance
}

// adoptNoteBudget is the column budget the guidance above is measured against.
//
// Rail-LESS, matching the driver this actually presents over: the adopt
// question is one screen asked from the middle of an install, over the
// command's own rail-less presentation (installQuestionPresentation).
func adoptNoteBudget() tui.NoteBudget { return tui.RaillessNoteBudget() }

// newAdoptQuestion is the multi-select over the conflicting objects.
//
// Every row starts UNSELECTED — including Secrets, which is the point: adopting
// one must be a deliberate act, never a default. That is also what SILENCE
// answers with, since huh's accessible renderer returns the bound selection
// when its input runs out and reports no error doing so; here the empty
// selection aborts the install, which is the safe end of that.
//
// It records no summary line: this command renders no summary, and what was
// actually seized is reported by adoptedNotice from the install's own result.
func newAdoptQuestion(conflicts []install.Conflict) *tui.Question {
	options := make([]tui.Choice, 0, len(conflicts))
	for _, c := range conflicts {
		options = append(options, tui.Choice{Label: adoptOptionLabel(c), Value: c.Key()})
	}
	return tui.NewMultiChoice(tui.MultiChoiceOpts{
		QuestionOpts: tui.QuestionOpts{
			ID:       "adopt",
			Label:    "Adopt",
			Key:      keyAdopt,
			Title:    adoptQuestion,
			Guidance: func(*tui.State) string { return adoptGuidanceFor(len(conflicts)) },
			Budget:   adoptNoteBudget(),
		},
		Options: options,
	})
}

// newAdoptDecision builds the InstallOpts.AdoptDecision hook for the CLI.
// Install calls it only after every guard has run and before any cluster write,
// so declining is always safe.
//
// opts carries the presentation the question is asked over — this command's
// own driver, which is where the alt-screen decision is made for every question
// it asks (see InstallQuestionDriverParams).
//
// Nothing is printed to the stream before the question. The rows ARE the list
// of conflicting objects, and the guidance above them says what adopting one
// costs, so a block printed first would be the same text twice on the screen
// the operator is deciding from.
func newAdoptDecision(opts tui.Options) func(context.Context, []install.Conflict) ([]string, error) {
	return func(ctx context.Context, conflicts []install.Conflict) ([]string, error) {
		answered, err := tui.Run(ctx, []tui.Screen{newAdoptQuestion(conflicts)}, opts)
		if err != nil {
			// Stripped here, at the call site that produced the framing: `tui:
			// present screen "adopt":` in front of a sentence about an object
			// the operator recognises is this command's plumbing showing
			// through, and the screen ID names nothing they can act on.
			return nil, tui.UserFacing(err)
		}
		return answered.All(keyAdopt), nil
	}
}

// wrapConflictError appends the CLI's own --adopt remediation to a
// *install.ConflictError before RunE returns it to cobra for printing.
// install.ConflictError.Error() deliberately says only what is true on every
// install surface (see its doc comment) — it does NOT name a flag, because
// the same error also reaches the menu-bar desktop (which has no flag to
// type) and admind's HTTP response body. The CLI is the one surface where
// "--adopt=Kind/Name" IS the right next step, so it is appended here rather
// than in the library. Any other error is returned unchanged.
func wrapConflictError(err error) error {
	var ce *install.ConflictError
	if !errors.As(err, &ce) {
		return err
	}
	var b strings.Builder
	var adoptable []install.Conflict
	var refused []string
	for _, conflict := range ce.Conflicts {
		if conflict.AdoptionRefused() {
			refused = append(refused, conflict.Key())
		} else {
			adoptable = append(adoptable, conflict)
		}
	}
	if len(adoptable) > 0 {
		b.WriteString("\nadopt the eligible objects with --adopt=Kind/Name")
	}
	if secretKeys := conflictSecretKeys(adoptable); len(secretKeys) > 0 {
		// A Secret is never covered by a blanket --adopt, so telling the
		// operator "use --adopt" alone would send them into a second failure.
		fmt.Fprintf(&b, " (a Secret must be named individually: --adopt=%s)", strings.Join(secretKeys, " --adopt="))
	} else if len(adoptable) > 0 {
		b.WriteString(" (or bare --adopt for all of them)")
	}
	if len(refused) > 0 {
		fmt.Fprintf(&b, "\n%s cannot be adopted; choose a different channel name or remove the conflicting Secret out of band", strings.Join(refused, ", "))
	}
	return fmt.Errorf("%w%s", err, b.String())
}

// conflictSecretKeys returns the "Kind/Name" of every Secret conflict, in the
// order given — the CLI-only counterpart of the derivation
// install.checkResourceOwnership does internally; this package only has
// Conflict's exported fields to work from.
func conflictSecretKeys(conflicts []install.Conflict) []string {
	var keys []string
	for _, c := range conflicts {
		if c.Secret {
			keys = append(keys, c.Key())
		}
	}
	return keys
}
