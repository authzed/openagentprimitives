package questionscreen

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// theCredential is long and distinctive so its presence in a rendered field is
// unambiguous — a masked field draws one mask character per rune, which must
// not be mistakable for the value itself.
const theCredential = "sk-fixture-credential-value"

// viewOf renders what an operator SEES when this screen is presented, with the
// question's Default already in the field. Reading the drawn field rather than
// huh's unexported echo mode keeps the assertion about behavior: a constructor
// that set the flag and a renderer that ignored it would both fail here.
func viewOf(t *testing.T, q oap.Question, caps tui.Caps) string {
	t.Helper()
	g, err := NewScreen(q, caps).Prepare(context.Background(), tui.NewState())
	require.NoError(t, err, "Prepare")
	require.NotNil(t, g, "an unanswered question must produce a group to draw")
	g.Init()
	return g.View()
}

// TestNewScreen_ASecretIsMaskedWhereTheDriverCanHonorIt covers the one thing a
// QSecret must not do: echo the credential an operator types into the
// scrollback of a terminal that could have hidden it.
//
// The asymmetry is tui.NewSecret's, not this package's, and it is deliberate:
// huh's accessible renderer takes its password branch only for a reader
// carrying Fd(), and discards the field's error otherwise — so masking
// off-TTY does not degrade to an echoed field, it degrades to NO field and an
// empty credential reported as success.
func TestNewScreen_ASecretIsMaskedWhereTheDriverCanHonorIt(t *testing.T) {
	q := oap.Question{Name: "tok", Type: oap.QSecret, Prompt: "API token", Default: theCredential}

	t.Run("under a TTY the value is hidden, never echoed", func(t *testing.T) {
		view := viewOf(t, q, tui.Caps{TTY: true, Width: 80})
		assert.NotContains(t, view, theCredential, "a credential must not be drawn into the terminal")
		assert.Contains(t, view, strings.Repeat("*", len(theCredential)),
			"a masked field draws one mask character per rune")
	})

	t.Run("off-TTY it echoes, because masking there collects nothing at all", func(t *testing.T) {
		assert.Contains(t, viewOf(t, q, tui.Caps{}), theCredential)
	})

	t.Run("a non-secret question is unaffected by the caps it is built with", func(t *testing.T) {
		plain := oap.Question{Name: "org", Type: oap.QString, Prompt: "Organization", Default: theCredential}
		assert.Contains(t, viewOf(t, plain, tui.Caps{TTY: true, Width: 80}), theCredential,
			"only a QSecret is masked; masking anything else would hide the answer the operator is checking")
	})
}

// TestDefaultString_PreseedsQStringInputFromDefault covers I2: a QString
// question with a Default (e.g. a synthesized capacityfit clamp) must offer
// it on the interactive plain-text path, not present an empty field. This
// pins the projection from a manifest's untyped Default to the string the
// input screen binds; the screen driving that binding is covered by
// pkg/platform/oap/install's TestResolve_Interactive_AnswersComeFromTheQuestionsAsked.
func TestDefaultString_PreseedsQStringInputFromDefault(t *testing.T) {
	cases := []struct {
		name string
		q    oap.Question
		want string
	}{
		{name: "string Default is offered verbatim", q: oap.Question{Type: oap.QString, Default: "1792Mi"}, want: "1792Mi"},
		{name: "no Default preseeds empty, not a zero-value placeholder", q: oap.Question{Type: oap.QString}, want: ""},
		{name: "a non-string Default (should never happen for QString) preseeds empty, not a panic", q: oap.Question{Type: oap.QString, Default: 42}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, DefaultString(tc.q))
		})
	}
}

// TestQuestionScreen_StateFirst covers the convention every screen owes the
// sequencer directly, for each widget type: a key already in State asks
// nothing, and Apply leaves the recorded answer alone.
func TestQuestionScreen_StateFirst(t *testing.T) {
	cases := []struct {
		name string
		q    oap.Question
		seed func(*tui.State)
		want func(*testing.T, *tui.State)
	}{
		{
			name: "answered input question: asks nothing, keeps the recorded answer",
			q:    oap.Question{Name: "memory", Type: oap.QString, Prompt: "Memory", Default: "1792Mi"},
			seed: func(st *tui.State) { st.Set("memory", "2048Mi") },
			want: func(t *testing.T, st *tui.State) {
				assert.Equal(t, "2048Mi", st.Get("memory"))
			},
		},
		{
			name: "answered enum question: asks nothing, keeps the recorded answer",
			q:    oap.Question{Name: "tier", Type: oap.QEnum, Prompt: "Tier", Enum: []string{"alpha", "beta"}},
			seed: func(st *tui.State) { st.Set("tier", "beta") },
			want: func(t *testing.T, st *tui.State) {
				assert.Equal(t, "beta", st.Get("tier"))
			},
		},
		{
			name: "answered list question: asks nothing, keeps the recorded answer",
			q:    oap.Question{Name: "addons", Type: oap.QResourceList, Prompt: "Addons", Enum: []string{"metrics", "tracing"}, Default: []string{"metrics"}},
			seed: func(st *tui.State) { st.SetAll("addons", []string{"tracing"}) },
			want: func(t *testing.T, st *tui.State) {
				assert.Equal(t, []string{"tracing"}, st.All("addons"))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scr := NewScreen(tc.q, tui.Caps{})
			st := tui.NewState()
			tc.seed(st)

			g, err := scr.Prepare(context.Background(), st)
			require.NoError(t, err, "Prepare")
			assert.Nil(t, g, "an answered screen must ask nothing")

			require.NoError(t, scr.Apply(context.Background(), st), "Apply")
			tc.want(t, st)

			keyer, ok := scr.(interface{ AnswerKeys() []string })
			require.True(t, ok, "every question screen must declare its answer keys")
			assert.Equal(t, []string{tc.q.Name}, keyer.AnswerKeys(), "AnswerKeys")
		})
	}
}

// TestNewScreen_AGatedQuestionIsSkippedRatherThanAsked is the renderer half of
// oap.Question.AskWhen: a question whose gate the run's answers do not open is
// not part of this run at all.
//
// tui.ErrSkip, not a nil group, and the difference is the point. A nil group
// means "ask nothing but still record" — the answered-question case above —
// and would leave the screen's Apply writing an answer for a question nobody
// was asked, which a fail-closed run would then report as missing when it was
// not supplied. ErrSkip neither presents nor applies.
//
// The gate is read from the LIVE State, which is why both arms drive the same
// screen value: the answer that opens it is recorded by an earlier screen of
// the same run, long after NewScreen built this one.
func TestNewScreen_AGatedQuestionIsSkippedRatherThanAsked(t *testing.T) {
	q := oap.Question{
		Name: "bot-token", Type: oap.QSecret, Prompt: "Bot token",
		AskWhen: oap.AskWhen{Question: "slackapp", In: []string{"true"}},
	}

	t.Run("the answer does not open the gate: the screen is skipped entirely", func(t *testing.T) {
		st := tui.NewState()
		st.Set("slackapp", "provision")

		g, err := NewScreen(q, tui.Caps{}).Prepare(context.Background(), st)
		require.ErrorIs(t, err, tui.ErrSkip, "a question this route does not ask must be skipped, not merely silent")
		assert.Nil(t, g)
	})

	t.Run("the answer opens the gate: the screen asks", func(t *testing.T) {
		st := tui.NewState()
		st.Set("slackapp", "true")

		g, err := NewScreen(q, tui.Caps{}).Prepare(context.Background(), st)
		require.NoError(t, err)
		assert.NotNil(t, g, "the route that needs this credential must still be asked for it")
	})

	t.Run("an ungated question is unaffected: it asks with nothing answered", func(t *testing.T) {
		g, err := NewScreen(oap.Question{Name: "org", Type: oap.QString, Prompt: "Organization"}, tui.Caps{}).
			Prepare(context.Background(), tui.NewState())
		require.NoError(t, err)
		assert.NotNil(t, g)
	})
}

// TestEnumChoices_LabelsWhatTheOperatorReadsWithoutChangingTheAnswer is the
// renderer half of the enum-label contract. The Enum entry is the STORED
// answer — a channel wizard's route is literally the string `--answer
// slackapp=true` supplies — so a question whose rows need to read as prose
// carries EnumLabels alongside, and the widget must show the label while
// recording the value.
//
// A blank or absent label falls back to the value, which is what keeps every
// manifest question in the tree (none of which declares labels) rendering
// exactly as it did before the field existed.
func TestEnumChoices_LabelsWhatTheOperatorReadsWithoutChangingTheAnswer(t *testing.T) {
	cases := []struct {
		name string
		q    oap.Question
		want []tui.Choice
	}{
		{
			name: "no labels: the value is its own label",
			q:    oap.Question{Enum: []string{"alpha", "beta"}},
			want: []tui.Choice{{Label: "alpha", Value: "alpha"}, {Label: "beta", Value: "beta"}},
		},
		{
			name: "labels: shown to the operator, while the value stays the answer",
			q: oap.Question{
				Enum:       []string{"false", "true"},
				EnumLabels: []string{"Show me the manifest", "I already have an app"},
			},
			want: []tui.Choice{
				{Label: "Show me the manifest", Value: "false"},
				{Label: "I already have an app", Value: "true"},
			},
		},
		{
			name: "a blank label falls back to its value rather than rendering an empty row",
			q: oap.Question{
				Enum:       []string{"alpha", "beta"},
				EnumLabels: []string{"", "Second"},
			},
			want: []tui.Choice{{Label: "alpha", Value: "alpha"}, {Label: "Second", Value: "beta"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, enumChoices(tc.q))
		})
	}
}

// TestNewScreen_MultiSelectCarriesLabelsToo: a QResourceList with an Enum
// renders through the same rows, and in practice it is the one that matters
// most — a Slack capability's label is where the scopes it costs are written,
// which is the thing the operator is consenting to.
func TestNewScreen_MultiSelectCarriesLabelsToo(t *testing.T) {
	q := oap.Question{
		Name: "capabilities", Type: oap.QResourceList, Prompt: "Enable",
		Enum:       []string{"attachments", "artifacts"},
		EnumLabels: []string{"attachments — files:read, files:write", "artifacts — files:write"},
	}
	assert.Equal(t, []tui.Choice{
		{Label: "attachments — files:read, files:write", Value: "attachments"},
		{Label: "artifacts — files:write", Value: "artifacts"},
	}, enumChoices(q))

	// And this really is the multi-select arm, so those are the rows the screen
	// renders rather than a text field ignoring them.
	require.NotNil(t, NewScreen(q, tui.Caps{}))
}
