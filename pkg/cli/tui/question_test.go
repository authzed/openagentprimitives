package tui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runQuestions drives screens over a scripted input through the plain driver —
// the same one an off-TTY run gets — and returns the answered State.
func runQuestions(t *testing.T, screens []Screen, st *State, script string) (*State, string, error) {
	t.Helper()
	var out bytes.Buffer
	answered, err := RunWith(context.Background(), screens, Options{
		Theme: NewTheme(Caps{}),
		In:    strings.NewReader(script),
		Out:   &out,
	}, st)
	return answered, out.String(), err
}

func textQuestion(o TextOpts) *Question {
	o.ID, o.Label, o.Key, o.Title = "q", "Q", "q", "A question"
	return NewText(o)
}

// TestQuestionConsultsStateBeforeAsking is the convention the whole
// non-interactive story rests on: an answer already recorded is not asked
// again. Without it a seeded run prompts anyway, reads whatever is next on the
// input and overwrites the answer with it — and off-TTY that happens with no
// error, because huh's accessible renderer has no error channel.
func TestQuestionConsultsStateBeforeAsking(t *testing.T) {
	st := NewState()
	st.Set("q", "seeded-answer")

	answered, printed, err := runQuestions(t, []Screen{textQuestion(TextOpts{})}, st, "typed-answer\n")
	require.NoError(t, err)
	assert.Empty(t, printed, "an answered screen must ask nothing")
	assert.Equal(t, "seeded-answer", answered.Get("q"), "the seeded answer must survive the run")
}

func TestTextQuestionDefaultAndOptional(t *testing.T) {
	cases := []struct {
		name     string
		opts     TextOpts
		seed     string
		script   string
		want     string
		wantNote string // "" = assert no summary line was recorded
		wantErr  string
	}{
		{
			name:   "typed answer wins over the pre-filled default",
			opts:   TextOpts{Default: func() string { return "stored" }},
			script: "typed\n",
			want:   "typed",
		},
		{
			name:   "bare Enter on a pre-filled field keeps the default",
			opts:   TextOpts{Default: func() string { return "stored" }},
			script: "\n",
			want:   "stored",
		},
		{
			name:   "bare Enter on an optional field records an explicit blank",
			opts:   TextOpts{Optional: true},
			script: "\n",
			want:   "",
		},
		{
			// The empty check must run BEFORE NoteValue, not after it. Every
			// credential field in the tree passes credmask.Mask here, and Mask
			// returns "****" for anything under twelve characters — including
			// "" — so a NoteValue consulted first turns "the user left this
			// empty" into a line that looks like a redacted secret. On the one
			// field this happens to (oauth-mcp's client secret) empty IS the
			// answer: it says the client is public, and the summary is where a
			// user goes to re-read which kind they set up.
			name: "bare Enter on an optional field is summarised as absent, never handed to NoteValue",
			opts: TextOpts{
				Optional: true,
				QuestionOpts: QuestionOpts{
					NoteLabel: "Secret",
					NoteValue: func(string) string { return "****" },
				},
			},
			script:   "\n",
			want:     "",
			wantNote: "not set",
		},
		{
			name:    "input runs out on a required field: refuses, naming the question",
			opts:    TextOpts{},
			script:  "",
			wantErr: "A question: nothing was supplied",
		},
		{
			name: "a seeded answer is still checked",
			opts: TextOpts{Check: func(_ *State, v string) error {
				if v == "bad" {
					return errors.New("that value is not usable")
				}
				return nil
			}},
			seed:    "bad",
			script:  "",
			wantErr: "that value is not usable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := NewState()
			if tc.seed != "" {
				st.Set("q", tc.seed)
			}
			answered, _, err := runQuestions(t, []Screen{textQuestion(tc.opts)}, st, tc.script)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, UserFacing(err).Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, answered.Get("q"))
			// An optional field that was ASKED must record an answer, or a
			// fail-closed run cannot tell it from one that was never asked.
			// Get alone does not prove that: it returns "" for both.
			assert.True(t, answered.Has("q"),
				"a question that was asked must record an answer, even an empty one")

			if tc.wantNote == "" {
				assert.Empty(t, answered.Notes(), "this question keeps no summary line")
				return
			}
			require.Len(t, answered.Notes(), 1)
			assert.Equal(t, tc.wantNote, answered.Notes()[0].Value)
		})
	}
}

// TestTextCheckSeesEarlierAnswers is the capability flowscreens' Validate could
// not express, and the reason Check takes a State: a confirmation field is only
// correct relative to the field above it, and huh hands a validator nothing but
// the typed string.
func TestTextCheckSeesEarlierAnswers(t *testing.T) {
	first := NewText(TextOpts{
		QuestionOpts: QuestionOpts{ID: "a", Label: "A", Key: "a", Title: "First"},
	})
	second := NewText(TextOpts{
		QuestionOpts: QuestionOpts{ID: "b", Label: "B", Key: "b", Title: "Second"},
		Check: func(st *State, v string) error {
			if v != st.Get("a") {
				return errors.New("the two answers do not match")
			}
			return nil
		},
	})

	_, _, err := runQuestions(t, []Screen{first, second}, NewState(), "one\ntwo\n")
	require.Error(t, err)
	assert.Contains(t, UserFacing(err).Error(), "do not match")

	answered, _, err := runQuestions(t, []Screen{first, second}, NewState(), "one\none\n")
	require.NoError(t, err)
	assert.Equal(t, "one", answered.Get("b"))
}

// TestQuestionSkipRecordsNothing pins the difference between a skipped screen
// and an answered one: a skipped screen is not part of the run at all, so a
// later screen reading its key must see nothing rather than an empty answer.
func TestQuestionSkipRecordsNothing(t *testing.T) {
	scr := textQuestion(TextOpts{QuestionOpts: QuestionOpts{Skip: func(*State) bool { return true }}})
	answered, printed, err := runQuestions(t, []Screen{scr}, NewState(), "")
	require.NoError(t, err)
	assert.Empty(t, printed)
	assert.False(t, answered.Has("q"), "a skipped screen must record no answer at all")
}

// TestNoteValueDescribesRatherThanRecords is the credential guard. The summary
// is plain scrollback that outlives the terminal a value was typed into, so a
// field collecting something authenticating must describe the outcome rather
// than record — or mask — the value.
func TestNoteValueDescribesRatherThanRecords(t *testing.T) {
	scr := textQuestion(TextOpts{
		QuestionOpts: QuestionOpts{
			NoteLabel: "Secret",
			NoteValue: func(string) string { return "updated" },
		},
	})
	answered, _, err := runQuestions(t, []Screen{scr}, NewState(), "a-credential\n")
	require.NoError(t, err)

	require.Len(t, answered.Notes(), 1)
	assert.Equal(t, "updated", answered.Notes()[0].Value)
	assert.NotContains(t, answered.Notes()[0].Value, "a-credential")
}

func TestAddressDelivery(t *testing.T) {
	budget := RailedNoteBudget()
	short := Address{Label: "Docs", URL: "https://docs.demo-provider.example/tokens"}
	long := Address{Label: "Docs", URL: "https://docs.demo-provider.example/" + strings.Repeat("x", budget.Columns())}

	require.True(t, short.Fits(budget), "this fixture must fit, or the test proves nothing")
	require.False(t, long.Fits(budget), "this fixture must not fit, or the test proves nothing")

	t.Run("an address the note can carry goes into the guidance whole, not the summary", func(t *testing.T) {
		scr := textQuestion(TextOpts{QuestionOpts: QuestionOpts{Addresses: []Address{short}}})
		answered, printed, err := runQuestions(t, []Screen{scr}, NewState(), "typed\n")
		require.NoError(t, err)
		assert.Contains(t, printed, short.URL, "the address must be in front of the user while they answer")
		assert.Empty(t, answered.Notes(), "an address the note carried whole needs no summary line")
	})

	// The rule this pins is the one `idpscreens` and `flowscreens` had to agree
	// on: an address too wide for a note is CUT into it, not withheld.
	// Withholding it leaves the question naming nothing the user can recognise
	// — a browser confirmation with no page, guidance saying "the address
	// below" with nothing below it — while a cut one still says which host and
	// which page, and its ellipsis says it is incomplete.
	t.Run("an address the note cannot carry is cut into it and recorded whole", func(t *testing.T) {
		scr := textQuestion(TextOpts{QuestionOpts: QuestionOpts{Addresses: []Address{long}}})
		answered, printed, err := runQuestions(t, []Screen{scr}, NewState(), "typed\n")
		require.NoError(t, err)
		assert.NotContains(t, printed, long.URL,
			"the whole address must not be put in a note, where huh would break it in two")
		assert.Contains(t, printed, "https://docs.demo-provider.example/",
			"what the note keeps must still name the host and the start of the path")
		assert.Contains(t, printed, "…", "and must mark that it was cut")
		assert.Empty(t, budget.Overflows(long.block(budget)),
			"what the note keeps must fit the budget it was cut to")
		require.Len(t, answered.Notes(), 1, "the summary must carry what the note could not")
		assert.Equal(t, long.URL, answered.Notes()[0].Value, "the summary wraps nothing, so it carries the address whole")
	})

	t.Run("two addresses on one question are each delivered by the same rule", func(t *testing.T) {
		scr := textQuestion(TextOpts{QuestionOpts: QuestionOpts{
			Addresses: []Address{short, long, {Label: "Skipped", URL: "  "}},
		}})
		answered, printed, err := runQuestions(t, []Screen{scr}, NewState(), "typed\n")
		require.NoError(t, err)
		assert.Contains(t, printed, short.URL, "the one that fits is carried whole")
		assert.NotContains(t, printed, long.URL, "the one that does not is cut")
		assert.NotContains(t, printed, "Skipped", "an address with no URL offers nothing to show")
		require.Len(t, answered.Notes(), 1, "only the cut address owes the summary a line")
		assert.Equal(t, long.URL, answered.Notes()[0].Value)
	})

	t.Run("a question answered ahead of time has no address to cut", func(t *testing.T) {
		scr := textQuestion(TextOpts{QuestionOpts: QuestionOpts{Addresses: []Address{long}}})
		st := NewState()
		st.Set("q", "seeded")
		answered, _, err := runQuestions(t, []Screen{scr}, st, "")
		require.NoError(t, err)
		assert.Empty(t, answered.Notes(), "no guidance was rendered, so no address was cut")
	})
}

// TestAddressPutsItsLabelOnItsOwnLine pins the budget decision: inline, a label
// spends its own width on text nobody copies, which is enough to push an
// ordinary address over the note budget — and, once addresses are cut rather
// than withheld, to cost the path segment that says which page it points at.
func TestAddressPutsItsLabelOnItsOwnLine(t *testing.T) {
	a := Address{Label: "Redirect URI", URL: "https://ap.demo-cluster.example/oidc/callback/idp"}
	lines := strings.Split(a.block(RailedNoteBudget()), "\n")
	require.Len(t, lines, 2)
	assert.Equal(t, "Redirect URI:", lines[0])
	assert.Equal(t, a.URL, lines[1], "the address must occupy its line alone, unprefixed")

	assert.False(t, Address{Label: "X", URL: "   "}.Fits(RailedNoteBudget()), "a blank address fits nothing")
	assert.Empty(t, Address{Label: "X", URL: "   "}.block(RailedNoteBudget()), "a blank address renders nothing")
}

func TestChoiceQuestion(t *testing.T) {
	opts := func(def func() string) ChoiceOpts {
		return ChoiceOpts{
			QuestionOpts: QuestionOpts{ID: "c", Label: "C", Key: "c", Title: "Pick one"},
			Options:      []Choice{{Label: "Safe", Value: "safe"}, {Label: "Risky", Value: "risky"}},
			Default:      def,
		}
	}

	t.Run("silence lands on the first option when there is no default", func(t *testing.T) {
		answered, _, err := runQuestions(t, []Screen{NewChoice(opts(nil))}, NewState(), "")
		require.NoError(t, err)
		assert.Equal(t, "safe", answered.Get("c"))
	})

	t.Run("silence lands on the default when there is one", func(t *testing.T) {
		answered, _, err := runQuestions(t, []Screen{NewChoice(opts(func() string { return "risky" }))}, NewState(), "")
		require.NoError(t, err)
		assert.Equal(t, "risky", answered.Get("c"),
			"a supplied default binds over the first option, so it is what silence takes")
	})

	t.Run("a seeded value no option offered is refused, naming the choices", func(t *testing.T) {
		st := NewState()
		st.Set("c", "somethingelse")
		_, _, err := runQuestions(t, []Screen{NewChoice(opts(nil))}, st, "")
		require.Error(t, err)
		msg := UserFacing(err).Error()
		assert.Contains(t, msg, "not one of the choices")
		assert.Contains(t, msg, "safe")
	})

	t.Run("a choice question with no options refuses rather than rendering an empty list", func(t *testing.T) {
		scr := NewChoice(ChoiceOpts{QuestionOpts: QuestionOpts{ID: "c", Label: "C", Key: "c", Title: "Pick one"}})
		_, _, err := runQuestions(t, []Screen{scr}, NewState(), "")
		require.Error(t, err)
		assert.Contains(t, UserFacing(err).Error(), "nothing to choose from")
	})
}

func TestMultiChoiceQuestion(t *testing.T) {
	opts := func(def func() []string) MultiChoiceOpts {
		return MultiChoiceOpts{
			QuestionOpts: QuestionOpts{ID: "m", Label: "M", Key: "m", Title: "Pick any"},
			Options: []Choice{
				{Label: "Metrics", Value: "metrics"},
				{Label: "Tracing", Value: "tracing"},
				{Label: "Audit", Value: "audit"},
			},
			Default: def,
		}
	}

	// The scripted selections are neither the pre-selection nor the empty set,
	// because huh's accessible renderer answers a question it ran out of input
	// for with whatever is bound and reports a nil error — an expectation
	// matching either of those would pass on a question that never rendered.
	t.Run("the picked rows are the answer, in the order the rows offer them", func(t *testing.T) {
		answered, printed, err := runQuestions(t, []Screen{NewMultiChoice(opts(nil))}, NewState(), "3\n1\n0\n")
		require.NoError(t, err)
		assert.Contains(t, printed, "Pick any", "the question must actually have been rendered")
		assert.Equal(t, []string{"metrics", "audit"}, answered.All("m"))
	})

	// These two expect exactly what a question that never rendered would also
	// leave bound, so each asserts the render separately. MultiSelect's
	// accessible renderer writes its title before its first read, so an unasked
	// question prints nothing at all.
	t.Run("silence takes the pre-selection, which is what a default is for", func(t *testing.T) {
		scr := NewMultiChoice(opts(func() []string { return []string{"tracing"} }))
		answered, printed, err := runQuestions(t, []Screen{scr}, NewState(), "")
		require.NoError(t, err)
		assert.Contains(t, printed, "Pick any", "the question must actually have been rendered")
		assert.Equal(t, []string{"tracing"}, answered.All("m"),
			"a supplied default binds the selection, so it is what silence takes")
	})

	t.Run("a default naming a row that does not exist keeps the rows that do", func(t *testing.T) {
		scr := NewMultiChoice(opts(func() []string { return []string{"gone", "audit"} }))
		answered, printed, err := runQuestions(t, []Screen{scr}, NewState(), "")
		require.NoError(t, err)
		assert.Contains(t, printed, "Pick any", "the question must actually have been rendered")
		assert.Equal(t, []string{"audit"}, answered.All("m"),
			"one stale entry must not discard the rest of a stored answer")
	})

	t.Run("picking nothing records an answer, not an absent one", func(t *testing.T) {
		answered, printed, err := runQuestions(t, []Screen{NewMultiChoice(opts(nil))}, NewState(), "0\n")
		require.NoError(t, err)
		assert.Contains(t, printed, "Pick any", "the question must actually have been rendered")
		assert.Empty(t, answered.All("m"))
		assert.True(t, answered.Has("m"),
			"picking none of them must stay distinguishable from never being asked")
	})

	t.Run("a seeded selection no row offered is refused, naming the choices", func(t *testing.T) {
		st := NewState()
		st.SetAll("m", []string{"metrics", "somethingelse"})
		_, _, err := runQuestions(t, []Screen{NewMultiChoice(opts(nil))}, st, "")
		require.Error(t, err)
		msg := UserFacing(err).Error()
		assert.Contains(t, msg, "not one of the choices")
		assert.Contains(t, msg, "tracing")
	})

	t.Run("a seeded selection is not re-asked", func(t *testing.T) {
		st := NewState()
		st.SetAll("m", []string{"audit"})
		answered, printed, err := runQuestions(t, []Screen{NewMultiChoice(opts(nil))}, st, "1\n0\n")
		require.NoError(t, err)
		assert.Empty(t, printed, "an answered screen must ask nothing")
		assert.Equal(t, []string{"audit"}, answered.All("m"), "the seeded answer must survive the run")
	})

	t.Run("no rows at all refuses rather than rendering an empty list", func(t *testing.T) {
		scr := NewMultiChoice(MultiChoiceOpts{QuestionOpts: QuestionOpts{ID: "m", Label: "M", Key: "m", Title: "Pick any"}})
		_, _, err := runQuestions(t, []Screen{scr}, NewState(), "")
		require.Error(t, err)
		assert.Contains(t, UserFacing(err).Error(), "nothing to choose from")
	})

	// The empty case is NoteAbsent's, never NoteValue's — the same split the
	// text question keeps, and for the same reason: every masking NoteValue in
	// this tree reports a redacted value for an empty one.
	t.Run("the summary lists the selection, and says so when there is none", func(t *testing.T) {
		noted := func(script string) []Note {
			t.Helper()
			o := opts(nil)
			o.NoteLabel, o.NoteAbsent = "Add-ons", "none"
			answered, printed, err := runQuestions(t, []Screen{NewMultiChoice(o)}, NewState(), script)
			require.NoError(t, err)
			// "none" is also what an unasked question would summarise, so the
			// render is asserted rather than inferred from the note.
			require.Contains(t, printed, "Pick any", "the question must actually have been rendered")
			return answered.Notes()
		}
		picked := noted("2\n3\n0\n")
		require.Len(t, picked, 1)
		assert.Equal(t, "tracing, audit", picked[0].Value)

		none := noted("0\n")
		require.Len(t, none, 1)
		assert.Equal(t, "none", none[0].Value, "an empty selection is NoteAbsent's, not NoteValue's")
	})
}

func TestConfirmQuestion(t *testing.T) {
	newConfirm := func(def bool) *Question {
		return NewConfirm(ConfirmOpts{
			QuestionOpts: QuestionOpts{ID: "y", Label: "Y", Key: "y", Title: "Proceed?", NoteLabel: "Proceed"},
			Default:      def,
		})
	}

	// Both values, because false is the zero value: a test that only asserted
	// the false case would pass with prepareValue reduced to a no-op, which is
	// the only thing binding Default at all. NewConfirm has no production
	// caller yet, so this is its whole proof.
	t.Run("silence takes the default: no, which is why a guard defaults to no", func(t *testing.T) {
		answered, _, err := runQuestions(t, []Screen{newConfirm(false)}, NewState(), "")
		require.NoError(t, err)
		assert.False(t, answered.Bool("y"))
		assert.Equal(t, "no", answered.Notes()[0].Value)
	})

	t.Run("silence takes the default: yes, when the caller asked for yes", func(t *testing.T) {
		answered, _, err := runQuestions(t, []Screen{newConfirm(true)}, NewState(), "")
		require.NoError(t, err)
		assert.True(t, answered.Bool("y"), "the caller's default must be what an unanswered run takes")
		assert.Equal(t, "yes", answered.Notes()[0].Value)
	})

	t.Run("an address a note cannot carry whole is cut into it and recorded whole", func(t *testing.T) {
		budget := RailedNoteBudget()
		long := Address{Label: "Page", URL: "https://demo-provider.example/" + strings.Repeat("x", budget.Columns())}
		require.False(t, long.Fits(budget), "this fixture must not fit, or the test proves nothing")

		scr := NewConfirm(ConfirmOpts{QuestionOpts: QuestionOpts{
			ID: "y", Label: "Y", Key: "y", Title: "Open it?", Addresses: []Address{long},
		}})
		answered, printed, err := runQuestions(t, []Screen{scr}, NewState(), "y\n")
		require.NoError(t, err)
		assert.NotContains(t, printed, long.URL, "the whole address must not be put in a note")
		assert.Contains(t, printed, "https://demo-provider.example/",
			"a confirmation must still name the page it is about")
		require.Len(t, answered.Notes(), 1, "the summary must carry what the note could not")
		assert.Equal(t, long.URL, answered.Notes()[0].Value)
	})

	t.Run("a seeded answer is not re-asked", func(t *testing.T) {
		st := NewState()
		st.SetBool("y", true)
		answered, printed, err := runQuestions(t, []Screen{newConfirm(false)}, st, "")
		require.NoError(t, err)
		assert.Empty(t, printed)
		assert.True(t, answered.Bool("y"), "the seeded answer must survive the run")
	})
}

// TestEveryQuestionKindDeclaresItsAnswerKey keeps the three constructors
// agreeing on the contract cmd/oap duck-types to name a missing answer.
func TestEveryQuestionKindDeclaresItsAnswerKey(t *testing.T) {
	base := QuestionOpts{ID: "k", Label: "K", Key: "the-key", Title: "T"}
	rows := []Choice{{Label: "a", Value: "a"}}
	for name, scr := range map[string]*Question{
		"text":        NewText(TextOpts{QuestionOpts: base}),
		"choice":      NewChoice(ChoiceOpts{QuestionOpts: base, Options: rows}),
		"multichoice": NewMultiChoice(MultiChoiceOpts{QuestionOpts: base, Options: rows}),
		"confirm":     NewConfirm(ConfirmOpts{QuestionOpts: base}),
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, []string{"the-key"}, scr.AnswerKeys())
			assert.Equal(t, "k", scr.ID())
			assert.Equal(t, "K", scr.Label())
		})
	}
}
