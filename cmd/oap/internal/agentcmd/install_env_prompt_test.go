package agentcmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// promptEnv is an installEnv wired for the line-oriented driver: a scripted
// reader, a buffer for output, and the uncolored theme. Nothing here may reach
// the process's own stdin — a test that let it would be exercising whatever
// terminal the test runner happened to have.
//
// interactive=false leaves the driver NIL, which is exactly what production
// hands an installEnv when nobody is at stdin (installQuestionPresentation
// returns no driver then). One driver serves every question one env asks, again
// as in production: the line-oriented driver buffers the stream it reads, so a
// driver apiece would let the first question's buffer swallow the line the next
// one is waiting for.
func promptEnv(t *testing.T, script string, interactive bool) (*installEnv, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	theme := tui.NewTheme(tui.Caps{})
	e := &installEnv{out: &out, theme: theme}
	if interactive {
		e.driver = tui.DriverFor(tui.DriverParams{
			Theme: theme,
			In:    strings.NewReader(script),
			Out:   &out,
		})
	}
	return e, &out
}

func TestInstallEnvConfirm(t *testing.T) {
	const prompt = "Image demo-image:dev is not available. Build it now?"

	cases := []struct {
		name        string
		script      string
		interactive bool
		want        bool
		// wantAsked pins whether the question reached the operator at all.
		//
		// Three of the four rows expect `false`, and `false` is ALSO what a
		// Confirm that never rendered anything returns — so without this the
		// table's whole non-interactive/exhausted distinction rests on a value
		// two different bugs produce. It matters most on the exhausted row,
		// which exists precisely because huh's accessible renderer reports EOF
		// as a defaulted answer with a nil error: "declined because the
		// operator said no" and "declined because nothing was ever shown" are
		// indistinguishable in the return value and must not be in the test.
		wantAsked bool
	}{
		{name: "non-interactive: declines without ever asking", script: "y\n", interactive: false, want: false, wantAsked: false},
		{name: "answered yes: asks, and builds", script: "y\n", interactive: true, want: true, wantAsked: true},
		{name: "answered no: asks, and declines", script: "n\n", interactive: true, want: false, wantAsked: true},
		{name: "input exhausted: asks, then declines rather than building unasked", script: "", interactive: true, want: false, wantAsked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, out := promptEnv(t, tc.script, tc.interactive)

			// Asserted on the returned decision, never on an error: huh's
			// accessible renderer has no error channel, so an exhausted script
			// yields a fully-defaulted answer with nothing reported.
			assert.Equal(t, tc.want, e.Confirm(prompt))

			if tc.wantAsked {
				assert.Contains(t, out.String(), prompt, "the operator must actually have been shown the question")
			} else {
				assert.Empty(t, out.String(), "a run that cannot ask must not print a question")
			}
		})
	}
}

func TestInstallEnvSecret(t *testing.T) {
	t.Run("supplied by flag: no prompt, and the scripted answer is untouched", func(t *testing.T) {
		e, out := promptEnv(t, "typed-instead\n", true)
		e.sets.secrets = map[string]string{"github_token": "literal-from-flag"}

		got, err := e.Secret("github_token")

		require.NoError(t, err)
		assert.Equal(t, "literal-from-flag", got)
		assert.Empty(t, out.String(), "a flag-supplied secret must not be asked for")
	})

	t.Run("non-interactive: names the flag that would have supplied it", func(t *testing.T) {
		e, _ := promptEnv(t, "", false)
		_, err := e.Secret("github_token")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--build-secret github_token=env:VAR")
	})

	t.Run("interactive: the typed value is returned", func(t *testing.T) {
		e, _ := promptEnv(t, "s3cr3t-value\n", true)
		got, err := e.Secret("github_token")
		require.NoError(t, err)
		assert.Equal(t, "s3cr3t-value", got)
	})

	// A secret is never worth a defaulted empty string: a build handed one
	// fails later, somewhere the operator cannot connect back to the question
	// nobody answered.
	t.Run("input exhausted: refuses, and the refusal carries no framing and no screen ID", func(t *testing.T) {
		e, _ := promptEnv(t, "", true)

		got, err := e.Secret("github_token")

		require.Error(t, err)
		assert.Empty(t, got)
		assert.Contains(t, err.Error(), "github_token")
		assert.NotContains(t, err.Error(), "tui: ", "the sequencer's framing is ours, not the operator's")
		assert.NotContains(t, err.Error(), "screen", "a screen ID is our vocabulary, not the operator's")
	})
}

func TestInstallEnvPath(t *testing.T) {
	t.Run("supplied by flag: no prompt", func(t *testing.T) {
		e, out := promptEnv(t, "/typed/instead\n", true)
		e.sets.contexts = map[string]string{"src": "/from/flag"}

		got, err := e.Path("src")

		require.NoError(t, err)
		assert.Equal(t, "/from/flag", got)
		assert.Empty(t, out.String())
	})

	t.Run("non-interactive: names the flag that would have supplied it", func(t *testing.T) {
		e, _ := promptEnv(t, "", false)
		_, err := e.Path("src")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--build-context src=/path")
	})

	t.Run("interactive: the typed path is returned", func(t *testing.T) {
		e, _ := promptEnv(t, "/home/dev/src\n", true)
		got, err := e.Path("src")
		require.NoError(t, err)
		assert.Equal(t, "/home/dev/src", got)
	})

	t.Run("input exhausted: refuses rather than building against an empty path", func(t *testing.T) {
		e, _ := promptEnv(t, "", true)
		got, err := e.Path("src")
		require.Error(t, err)
		assert.Empty(t, got)
		assert.NotContains(t, err.Error(), "tui: ")
		assert.NotContains(t, err.Error(), "screen")
	})
}

// The State-first rule for all three build-time questions: an answer already in
// State is not asked for again. Without it a caller that pre-answers a question
// — the only mechanism a run with no terminal has — would still be handed a
// group, and a driver with nothing behind it would refuse or hang on it.
func TestBuildQuestionsConsultStateFirst(t *testing.T) {
	cases := []struct {
		name     string
		question *tui.Question
		// seed answers the question ahead of the run; read takes the answer
		// back out afterwards, in the shape that question records.
		seed func(*tui.State)
		read func(*tui.State) any
		want any
	}{
		{
			name:     "build secret: a seeded key builds no group and is carried through",
			question: newBuildSecretQuestion("github_token"),
			seed:     func(st *tui.State) { st.Set(keyBuildSecret, "seeded-value") },
			read:     func(st *tui.State) any { return st.Get(keyBuildSecret) },
			want:     "seeded-value",
		},
		{
			name:     "build context: a seeded key builds no group and is carried through",
			question: newBuildContextQuestion("src"),
			seed:     func(st *tui.State) { st.Set(keyBuildContext, "/seeded/path") },
			read:     func(st *tui.State) any { return st.Get(keyBuildContext) },
			want:     "/seeded/path",
		},
		{
			name:     "build confirm: a seeded decision builds no group and is carried through",
			question: newBuildConfirmQuestion("Build it now?"),
			seed:     func(st *tui.State) { st.SetBool(keyBuildConfirm, true) },
			read:     func(st *tui.State) any { return st.Bool(keyBuildConfirm) },
			// true, which is NOT this question's default — a seeded answer that
			// matched the default would prove nothing about it being read.
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := tui.NewState()
			tc.seed(st)

			g, err := tc.question.Prepare(context.Background(), st)

			require.NoError(t, err)
			// Compared rather than assert.Nil-ed: a huh.Group renders as
			// several thousand lines of bubbletea state, which buries the one
			// fact the failure is about.
			assert.True(t, g == nil, "an answered question must not be asked")
			require.NoError(t, tc.question.Apply(context.Background(), st))
			assert.Equal(t, tc.want, tc.read(st), "Apply must carry the seeded answer out")
		})
	}
}

// A build secret must not survive the question that collected it. It is held in
// memory for the length of one docker build; a summary line, by contrast, is
// written into the operator's scrollback, where it outlives the terminal it was
// pasted into.
func TestBuildQuestionsRecordNoSummaryLine(t *testing.T) {
	for _, q := range []*tui.Question{
		newBuildSecretQuestion("github_token"),
		newBuildContextQuestion("src"),
	} {
		st := tui.NewState()
		st.Set(q.AnswerKeys()[0], "s3cr3t-value")

		require.NoError(t, q.Apply(context.Background(), st))

		assert.Empty(t, st.Notes(), "nothing a build question collects belongs in a summary")
	}

	// The confirm records no line either, though it collects nothing secret:
	// this command renders no summary at all, so a note would be written and
	// never read.
	st := tui.NewState()
	st.SetBool(keyBuildConfirm, true)
	require.NoError(t, newBuildConfirmQuestion("Build it now?").Apply(context.Background(), st))
	assert.Empty(t, st.Notes())
}

// newInstallEnv is the only place production wires these questions to a
// terminal, and it must not build a presentation of its own: every question one
// `oap agent install` asks goes over the driver the command already made, which
// is what keeps the several runs reading one stdin from each starting a buffer
// over bytes the last one swallowed — and what keeps them agreeing about the
// alternate screen.
func TestNewInstallEnvPresentsOverTheCommandsOwnDriver(t *testing.T) {
	g := &apcmd.Globals{Context: "kind-mycluster"}
	var out bytes.Buffer
	theme := tui.NewTheme(tui.Caps{})
	driver := tui.DriverFor(tui.DriverParams{Theme: theme, In: strings.NewReader(""), Out: &out})

	env, err := newInstallEnv(context.Background(), g, t.TempDir(), buildSets{}, &out, driver, theme)
	require.NoError(t, err)

	ie, ok := env.(*installEnv)
	require.True(t, ok, "newInstallEnv must return *installEnv")
	assert.Same(t, driver, ie.driver, "the build questions must present over the command's driver, not one built here")
	assert.Same(t, theme, ie.theme, "and be styled by the theme that driver was built with")
	assert.Same(t, &out, ie.out, "questions are written to the stream the command was handed")
	assert.True(t, ie.canAsk(), "a driver is what says somebody is there to answer")
}

// The other half of that: no driver is how the command says nobody is at
// stdin, and it must be the ONLY gate — a build question asked with no
// presentation would be resolved from a nil reader, which is huh's "read
// os.Stdin" signal.
func TestNewInstallEnvWithNoDriverAsksNothing(t *testing.T) {
	g := &apcmd.Globals{Context: "kind-mycluster"}
	var out bytes.Buffer

	env, err := newInstallEnv(context.Background(), g, t.TempDir(), buildSets{}, &out, nil, nil)
	require.NoError(t, err)

	ie, ok := env.(*installEnv)
	require.True(t, ok, "newInstallEnv must return *installEnv")
	assert.False(t, ie.canAsk(), "no driver means nobody is at stdin")

	_, askErr := ie.ask(newBuildSecretQuestion("github_token"))
	require.Error(t, askErr, "presenting with no driver must refuse rather than reach for os.Stdin")
	assert.Empty(t, out.String(), "and must print no question")
}
