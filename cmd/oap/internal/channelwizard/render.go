package channelwizard

import (
	"errors"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/questionscreen"
)

// renderQuestions turns a kind's declared inputs
// (channelkinds.Wizard.Inputs, or a HandoffSpec's FallbackInputs — both
// are rendered through this one function, so both get the same validation
// and the same widgets) into the screens a channel-create run still has to
// ask.
//
// The QuestionType -> widget mapping itself is NOT reimplemented here.
// pkg/platform/oap/questionscreen already carries it — it is what
// pkg/platform/oap/install's bundle-manifest questions render through — so a
// channel input and a manifest question end up on the exact same widgets:
// trimming, the masking a QSecret gets wherever caps says the driver can honor
// it (see questionscreen.NewScreen), and required-ness all included, rather
// than a second copy that drifts from the first one bug fix at a time. The one
// thing that IS genuinely channel-specific, and so lives here, is what to do
// about a question whose answer is already known.
//
// caps comes from the run's own theme, which is the same place its driver came
// from. A zero Caps — a caller with no theme — echoes a secret rather than
// masking it, which is the safe direction: masking over a driver that cannot
// mask collects nothing at all.
//
// qs is validated with channelkinds.ValidateInputs before anything is
// rendered, so a question this renderer cannot honor — an unknown Type, a
// QEnum with no Enum values, a Binding (a bundle-install concept a channel
// question has no business carrying), a non-empty Validation (see
// ValidateInputs' own doc for why that one is refused rather than silently
// ignored) — is refused up front, naming the question, rather than
// surfacing later as a panic or a silently wrong widget.
//
// seeded is keyed by Question.Name, exactly like --answer key=value: a
// question whose Name is a key of seeded is not built into a screen at all
// (dropped, not merely asked-and-skipped, so it costs the run's step rail
// nothing either). Its value is instead written into st through seedAnswer —
// the SAME function seedState uses for every `oap channel create --answer`
// (see create.go) — so a value seeded this way is readable back exactly the
// way a typed answer is (Get, Bool or All, whichever the question's own
// consumer expects), and a caller cannot tell the two apart once the run
// finishes. A second, parallel seeding path would be a second place for
// "what did this flag answer?" to be decided, and the two would drift.
//
// st must not be nil and is enforced, not merely documented: it is the same
// State the caller drives every other screen of the run against (tui.RunWith
// takes a caller-supplied State for exactly this reason: "seeding State from
// flags before Run is what makes non-interactive mode work") —
// renderQuestions writes a seeded answer straight into it rather than
// handing back a second, State-shaped value the caller would have to merge
// in itself. A nil st is harmless right up until the first seeded answer —
// seedAnswer's first call panics on a nil-pointer dereference inside
// State.Set — which is exactly the mode asymmetry that must fail the same
// way regardless of whether this particular run happens to seed anything.
func renderQuestions(qs []oap.Question, seeded map[string]string, st *tui.State, caps tui.Caps) ([]tui.Screen, error) {
	if st == nil {
		return nil, errors.New("renderQuestions: st must not be nil")
	}
	if err := channelkinds.ValidateInputs(qs); err != nil {
		return nil, err
	}

	screens := make([]tui.Screen, 0, len(qs))
	for _, q := range qs {
		if v, ok := seeded[q.Name]; ok {
			seedAnswer(st, q.Name, v)
			continue
		}
		screens = append(screens, questionscreen.NewScreen(q, caps))
	}
	return screens, nil
}

// screenCaps is the terminal capabilities a run's screens are built from: its
// theme's, or the zero value when it has no theme. One reader rather than a
// nil check at each call site, so a themeless run cannot crash one caller and
// silently echo a credential in another.
func screenCaps(opts tui.Options) tui.Caps {
	if opts.Theme == nil {
		return tui.Caps{}
	}
	return opts.Theme.Caps
}
