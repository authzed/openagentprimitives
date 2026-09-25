package agentcmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
)

func TestSplitAdopt(t *testing.T) {
	cases := []struct {
		name     string
		in       []string
		wantAll  bool
		wantKeys []string
		wantErr  string
	}{
		{name: "unset: no adopt at all", in: nil},
		{name: "bare --adopt: all, no keys", in: []string{adoptAllSentinel}, wantAll: true},
		{name: "targeted keys: keys, not all", in: []string{"AgentClass/demo-agent", "Secret/demo-token"},
			wantKeys: []string{"AgentClass/demo-agent", "Secret/demo-token"}},
		{name: "bare plus targeted: all wins and keys are kept", in: []string{adoptAllSentinel, "Secret/demo-token"},
			wantAll: true, wantKeys: []string{"Secret/demo-token"}},
		{name: "value without a slash: error naming the expected form", in: []string{"AgentClass"},
			wantErr: `--adopt value "AgentClass" must be Kind/Name`},
		{name: "empty Kind (leading slash): rejected, not accepted as a key", in: []string{"/foo"},
			wantErr: `--adopt value "/foo" must be Kind/Name`},
		{name: "empty Name (trailing slash): rejected, not accepted as a key", in: []string{"Foo/"},
			wantErr: `--adopt value "Foo/" must be Kind/Name`},
		{name: "extra slash: rejected, can never match a Conflict.Key()", in: []string{"Foo/Bar/Baz"},
			wantErr: `--adopt value "Foo/Bar/Baz" must be Kind/Name`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			all, keys, err := splitAdopt(tc.in)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantAll, all)
			assert.Equal(t, tc.wantKeys, keys)
		})
	}
}

func TestAdoptAwareArgs(t *testing.T) {
	newCmd := func(t *testing.T, adoptChanged bool) *cobra.Command {
		t.Helper()
		cmd := &cobra.Command{Use: "install"}
		var adopt []string
		cmd.Flags().StringSliceVar(&adopt, "adopt", nil, "")
		cmd.Flags().Lookup("adopt").NoOptDefVal = adoptAllSentinel
		if adoptChanged {
			require.NoError(t, cmd.Flags().Set("adopt", adoptAllSentinel))
		}
		return cmd
	}

	t.Run("one arg: accepted", func(t *testing.T) {
		require.NoError(t, adoptAwareArgs(newCmd(t, false), []string{"./demo.oap"}))
	})

	t.Run("space-separated --adopt Kind/Name: explains the '=' form", func(t *testing.T) {
		err := adoptAwareArgs(newCmd(t, true), []string{"AgentClass/demo-agent", "./demo.oap"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--adopt=AgentClass/demo-agent")
	})

	t.Run("two plain args without --adopt: ordinary arity error", func(t *testing.T) {
		err := adoptAwareArgs(newCmd(t, false), []string{"./a.oap", "./b.oap"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "accepts 1 arg(s), received 2")
	})
}

func TestAdoptedNotice(t *testing.T) {
	t.Run("empty when nothing was adopted", func(t *testing.T) {
		assert.Empty(t, adoptedNotice(nil))
	})

	t.Run("names each seized object and its uninstall consequence", func(t *testing.T) {
		out := adoptedNotice([]string{"AgentClass/demo-agent", "Secret/demo-token"})
		assert.Contains(t, out, "AgentClass/demo-agent")
		assert.Contains(t, out, "Secret/demo-token")
		assert.Contains(t, out, "oap agent uninstall")
	})
}

func TestAdoptOptionLabel(t *testing.T) {
	t.Run("namespaced CR: kind, namespace and name", func(t *testing.T) {
		label := adoptOptionLabel(install.Conflict{Kind: "AgentClass", Namespace: "demo", Name: "demo-agent"})
		assert.Contains(t, label, "AgentClass demo/demo-agent")
		assert.NotContains(t, label, "overwrites")
	})

	t.Run("Secret: label warns that its data is overwritten", func(t *testing.T) {
		label := adoptOptionLabel(install.Conflict{Kind: "Secret", Namespace: "demo", Name: "demo-token", Secret: true})
		assert.Contains(t, label, "Secret demo/demo-token")
		assert.Contains(t, label, "overwrites")
	})
}

// TestWrapConflictError pins the text that Finding-3's fix moved OUT of
// install.ConflictError.Error() (which must stay flag-free — see its doc
// comment, and pkg/platform/oap/install/conflict_test.go's
// "AdoptAll: non-Secret adopted, Secret still refused" for the library-side
// half of this regression) and INTO the CLI's own rendering here.
func TestWrapConflictError(t *testing.T) {
	t.Run("non-ConflictError: returned unchanged", func(t *testing.T) {
		sentinel := errors.New("some other failure")
		assert.Same(t, sentinel, wrapConflictError(sentinel))
	})

	t.Run("no Secret conflicts: bare --adopt guidance", func(t *testing.T) {
		ce := &install.ConflictError{Conflicts: []install.Conflict{
			{Kind: "AgentClass", Namespace: "demo", Name: "demo-agent"},
		}}
		err := wrapConflictError(ce)
		assert.Contains(t, err.Error(), "AgentClass demo/demo-agent", "the library's own object list must still be present")
		assert.Contains(t, err.Error(), "--adopt=Kind/Name")
		assert.Contains(t, err.Error(), "or bare --adopt for all of them")
	})

	t.Run("a Secret conflict: individual --adopt=Secret/Name guidance, no bare-adopt offer", func(t *testing.T) {
		ce := &install.ConflictError{Conflicts: []install.Conflict{
			{Kind: "Secret", Namespace: "demo", Name: "demo-token", Secret: true},
		}}
		err := wrapConflictError(ce)
		assert.Contains(t, err.Error(), "--adopt=Secret/demo-token", "a Secret must be named individually")
		assert.NotContains(t, err.Error(), "bare --adopt", "a blanket adopt never covers a Secret")
	})
}

// adoptConflicts is the pair every case below decides about: one CR and one
// Secret, because the two rows say different things and only the Secret's
// wording is conditional.
func adoptConflicts() []install.Conflict {
	return []install.Conflict{
		{Kind: "AgentClass", Namespace: "demo", Name: "demo-agent"},
		{Kind: "Secret", Namespace: "demo", Name: "demo-token", Secret: true},
	}
}

// adoptOpts presents the question over the line-oriented driver, reading a
// script. Nothing here may reach the process's own stdin — a test that let it
// would be exercising whatever terminal the test runner happened to have.
func adoptOpts(t *testing.T, script string, out io.Writer) tui.Options {
	t.Helper()
	return tui.Options{
		Theme: tui.NewTheme(tui.Caps{}),
		In:    strings.NewReader(script),
		Out:   out,
	}
}

// The accessible renderer drives a multi-select by number: each line toggles
// one row, and 0 confirms whatever is toggled. Every case answers to
// completion and asserts on the SELECTION, never on err — that renderer has no
// error channel, so a short script yields an empty selection and a nil error.
func TestNewAdoptDecision(t *testing.T) {
	t.Run("a toggled row: the conflicts are offered as rows and that row is returned", func(t *testing.T) {
		var out bytes.Buffer
		got, err := newAdoptDecision(adoptOpts(t, "1\n0\n", &out))(context.Background(), adoptConflicts())

		require.NoError(t, err)
		assert.Equal(t, []string{"AgentClass/demo-agent"}, got)
		assert.Contains(t, out.String(), "AgentClass demo/demo-agent")
		assert.Contains(t, out.String(), "Secret demo/demo-token")
		assert.Contains(t, out.String(), "oap agent uninstall",
			"what adopting costs must reach the operator, and the rows cannot say it")
	})

	// Toggled in reverse, returned in offer order — huh's multi-select hands
	// back its values in option order regardless of the toggle sequence. What
	// this pins is that nothing between the rows and the caller re-orders them.
	t.Run("both rows toggled in reverse: both returned, in the order they were offered", func(t *testing.T) {
		var out bytes.Buffer
		got, err := newAdoptDecision(adoptOpts(t, "2\n1\n0\n", &out))(context.Background(), adoptConflicts())

		require.NoError(t, err)
		assert.Equal(t, []string{"AgentClass/demo-agent", "Secret/demo-token"}, got)
	})

	t.Run("nothing toggled: an empty selection, which aborts the install", func(t *testing.T) {
		var out bytes.Buffer
		got, err := newAdoptDecision(adoptOpts(t, "0\n", &out))(context.Background(), adoptConflicts())

		require.NoError(t, err)
		assert.Empty(t, got, "a Secret must never be adopted by default")
		// Empty is ALSO what a question that never rendered returns, and that
		// renderer reports the difference nowhere: "the operator declined
		// everything" and "nothing was ever shown" have the same return value
		// and must not have the same test.
		assert.Contains(t, out.String(), adoptQuestion,
			"the operator must actually have been shown the question")
	})

	// The list of conflicting objects reaches the operator exactly once per
	// prompt. It used to be printed to the stream as well as offered as rows —
	// compensation for an alternate screen that covered whatever was above it —
	// and with the run inline that block would be the same objects twice on the
	// screen somebody is deciding from.
	t.Run("the objects are offered once, not printed to the stream and offered again", func(t *testing.T) {
		var out bytes.Buffer
		_, err := newAdoptDecision(adoptOpts(t, "0\n", &out))(context.Background(), adoptConflicts())

		require.NoError(t, err)
		assert.Equal(t, 1, strings.Count(out.String(), "AgentClass demo/demo-agent"),
			"one prompt, one list")
	})

	// What the presenter fails with is the sequencer's business; what reaches
	// the operator must be the sentence the screen wrote. `tui: present screen
	// "adopt":` in front of it is this command's plumbing showing through, and
	// the screen ID names nothing the operator can act on.
	t.Run("a presenter failure: the refusal carries no framing and no screen ID", func(t *testing.T) {
		var out bytes.Buffer
		opts := adoptOpts(t, "", &out)
		sentinel := errors.New("the terminal went away")
		opts.Driver = failingDriver{err: sentinel}

		_, err := newAdoptDecision(opts)(context.Background(), adoptConflicts())

		require.Error(t, err)
		assert.ErrorIs(t, err, sentinel, "the original chain must stay matchable")
		assert.Equal(t, sentinel.Error(), err.Error())
		assert.NotContains(t, err.Error(), "tui: ")
		assert.NotContains(t, err.Error(), "screen")
	})
}

// failingDriver stands in for a presenter that cannot run. Options.Driver is
// the documented seam for exactly this, and using it keeps the decision of
// WHICH driver a real run gets in the one place that makes it.
type failingDriver struct{ err error }

func (d failingDriver) Present(context.Context, string, *huh.Group) error { return d.err }

// The State-first rule: an adoption already decided is not asked about again.
// Without it a caller that pre-answers the question would still be handed a
// group, and a driver with nothing behind it would refuse or hang on it.
func TestAdoptQuestionConsultsStateFirst(t *testing.T) {
	q := newAdoptQuestion(adoptConflicts())
	st := tui.NewState()
	st.SetAll(keyAdopt, []string{"Secret/demo-token"})

	g, err := q.Prepare(context.Background(), st)

	require.NoError(t, err)
	// Compared rather than assert.Nil-ed: a huh.Group renders as several
	// thousand lines of bubbletea state, which buries the one fact the
	// failure is about.
	assert.True(t, g == nil, "an answered question must not be asked")
	require.NoError(t, q.Apply(context.Background(), st))
	assert.Equal(t, []string{"Secret/demo-token"}, st.All(keyAdopt))
}

// An adopt key that names nothing on offer can never match a real conflict, so
// it would silently adopt nothing at all — the same failure --adopt's own
// validation exists to prevent, arriving by the other door. Reachable only from
// a seeded answer, which never went through the rows.
func TestAdoptQuestionRefusesAnUnofferedKey(t *testing.T) {
	q := newAdoptQuestion(adoptConflicts())
	st := tui.NewState()
	st.SetAll(keyAdopt, []string{"ConfigMap/not-in-this-install"})

	err := q.Apply(context.Background(), st)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ConfigMap/not-in-this-install")
	assert.Contains(t, err.Error(), "AgentClass/demo-agent", "the refusal must say what WAS on offer")
}

// A note's lines are hard-wrapped at the form's body width with no indication
// that the second half belongs to the first, so guidance that overflows reads
// as two unrelated fragments. Measured in display columns against the budget
// the question itself declares, so the block and the thing that renders it
// cannot be measured against two different widths.
func TestAdoptGuidanceFitsTheNoteWidth(t *testing.T) {
	budget := adoptNoteBudget()
	require.Positive(t, budget.Columns(), "or the test proves nothing")

	assert.Empty(t, budget.Overflows(adoptGuidance),
		"every guidance line must fit the note this question is asked in")

	// The rendered block is what the operator actually reads, and it carries a
	// count line the constant above does not — measured at a count wide enough
	// that a plausible install cannot exceed it.
	assert.Empty(t, budget.Overflows(adoptGuidanceFor(9999)),
		"the count line must fit too, not just the static guidance")
}

// TestInstallQuestionPresentation covers the presentation `oap agent install`
// asks every pre-write question over.
//
// It is a separate function from the command body because the command body's
// interactive branch cannot be reached from a test at all: it is gated on
// apcmd.StdinIsInteractive(os.Stdin), which reads the real process stdin, and under
// `go test` that is never a character device. Extracting it is what makes the
// two facts below assertable rather than merely intended.
func TestInstallQuestionPresentation(t *testing.T) {
	t.Run("nobody at stdin: no driver, so the questions are never presented", func(t *testing.T) {
		drv, th := installQuestionPresentation(io.Discard, strings.NewReader(""), false, false)
		assert.Nil(t, drv, "a run nobody is at must not be handed a reader that answers for it")
		assert.Nil(t, th, "and no theme to go with it")
	})

	t.Run("somebody at stdin: capabilities come from this command's own stream and flag", func(t *testing.T) {
		var out bytes.Buffer
		drv, th := installQuestionPresentation(&out, strings.NewReader(""), true, true)
		require.NotNil(t, drv, "a run somebody is watching must be able to ask")
		require.NotNil(t, th, "the theme the driver was built with")

		// Measured against the stream this command writes to, NOT against
		// os.Stderr — which is what install.Resolve falls back to when no
		// presentation is supplied, and which reports a different width (and,
		// on a developer's machine, a different TTY-ness) than a buffer does.
		// Comparing Caps is what distinguishes "derived from the caller" from
		// "re-detected somewhere else".
		assert.Equal(t, apcmd.DetectCaps(&out, true), th.Caps,
			"the theme must come from this command's stream and its --no-color flag")
	})
}
