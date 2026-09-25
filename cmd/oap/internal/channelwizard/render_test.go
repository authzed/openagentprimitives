package channelwizard

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// driveScreens runs screens to completion over a scripted line-oriented
// driver and returns the resulting State.
//
// It exists so a test can tell "the widget this Type dispatches to actually
// ran and recorded the typed value" apart from "the call merely returned a
// nil error" — huh's accessible renderer answers a field it ran out of input
// for with that field's own default and reports no error doing so, so a case
// that only checked err == nil would pass whether or not the real widget for
// that Type was ever reached. Every want below is chosen to differ from what
// an undriven field would produce (empty, for every question in this file,
// since none of them carries a Default).
func driveScreens(t *testing.T, screens []tui.Screen, script string) *tui.State {
	t.Helper()
	th := tui.NewTheme(tui.Caps{})
	st, err := tui.RunWith(context.Background(), screens,
		tui.Options{Theme: th, Driver: tui.Plain(strings.NewReader(script), io.Discard, th)},
		tui.NewState())
	require.NoError(t, err, "RunWith")
	return st
}

// TestRenderQuestions_SeededAnswersAreNotAsked covers the one channel-specific
// thing renderQuestions does: a question already answered via --answer is
// dropped from the returned screens entirely (not merely asked-and-skipped —
// see questionscreen's own State-first convention for that), and its answer
// still reaches st, through the same seedAnswer path --answer already uses
// for every other screen in a run.
func TestRenderQuestions_SeededAnswersAreNotAsked(t *testing.T) {
	qs := []oap.Question{
		{Name: "org", Type: oap.QString, Prompt: "GitHub organization"},
		{Name: wizardkeys.KeyChannelName, Type: oap.QString, Prompt: "Channel name"},
	}

	st := tui.NewState()
	screens, err := renderQuestions(qs, map[string]string{wizardkeys.KeyChannelName: "demo-channel"}, st, tui.Caps{})
	require.NoError(t, err)

	assert.Equal(t, "demo-channel", st.Get(wizardkeys.KeyChannelName),
		"a seeded answer must reach the State without being asked")
	require.Len(t, screens, 1, "only the unseeded question gets a screen")
	assert.Equal(t, "org", screens[0].ID(), "the unseeded question is the one left to ask")
}

// TestRenderQuestions_EveryQuestionTypeRenders drives the real widget for each
// of the six declarable QuestionTypes over a scripted terminal and checks the
// recorded answer, not just that renderQuestions returned no error (an
// err-only check passes whether or not a widget was actually reached — see
// driveScreens).
//
// Only the QEnum row has discriminating power against a dispatch regression:
// questionscreen.NewScreen has exactly two non-default arms (QEnum, and
// QResourceList WITH Enum values), so QString/QInt/QBool/QSecret and this
// table's QResourceList (declared with no Enum) are all already exercising
// the SAME default free-text widget — there is no "wrong widget" for a
// dispatch bug to fall through to on those five rows, and driving them still
// earns its keep by confirming that widget genuinely round-trips each type's
// typed value rather than merely accepting it. QEnum is the one row here
// whose widget differs from the default, so it is the one row that would
// catch NewScreen's QEnum case silently degrading to text.
// TestRenderQuestions_ResourceListEnumUsesMultiChoice below covers the
// renderer's other non-default arm the same way.
func TestRenderQuestions_EveryQuestionTypeRenders(t *testing.T) {
	cases := []struct {
		ty     oap.QuestionType
		enum   []string
		script string
		want   string
	}{
		{ty: oap.QString, script: "demo-org\n", want: "demo-org"},
		{ty: oap.QInt, script: "42\n", want: "42"},
		{ty: oap.QBool, script: "true\n", want: "true"},
		// Picking the SECOND of two enum values: an undriven field's own
		// default would be the FIRST (see tui.ChoiceOpts.Default), and a
		// dispatch that fell through to the text widget would record the
		// literal typed line "2", not the option it names.
		{ty: oap.QEnum, enum: []string{"a", "b"}, script: "2\n", want: "b"},
		{ty: oap.QSecret, script: "s3cr3t\n", want: "s3cr3t"},
		{ty: oap.QResourceList, script: "items\n", want: "items"},
	}
	for _, tc := range cases {
		t.Run(string(tc.ty), func(t *testing.T) {
			q := oap.Question{Name: "demo", Type: tc.ty, Prompt: "Demo", Enum: tc.enum}
			screens, err := renderQuestions([]oap.Question{q}, nil, tui.NewState(), tui.Caps{})
			require.NoError(t, err, "every question type a bundle may declare must render")
			require.Len(t, screens, 1)

			st := driveScreens(t, screens, tc.script)
			assert.Equal(t, tc.want, st.Get("demo"),
				"the recorded answer must come from actually driving this type's own widget")
		})
	}
}

// TestRenderQuestions_ResourceListEnumUsesMultiChoice covers the one
// QResourceList shape TestRenderQuestions_EveryQuestionTypeRenders does not:
// with Enum values declared, questionscreen.NewScreen dispatches to a
// multi-select rather than the free-text default every other QResourceList
// question gets. This IS a discriminating case — the multi-select's answer
// (a list, via st.All) cannot come from the default text widget (which
// records a single string via st.Get) — so a dispatch that quietly fell
// through to text here would fail this test's assertion, not merely produce
// an unexpected value: st.All("addons") would be nil.
func TestRenderQuestions_ResourceListEnumUsesMultiChoice(t *testing.T) {
	q := oap.Question{Name: "addons", Type: oap.QResourceList, Prompt: "Addons", Enum: []string{"metrics", "tracing", "audit"}}
	screens, err := renderQuestions([]oap.Question{q}, nil, tui.NewState(), tui.Caps{})
	require.NoError(t, err)
	require.Len(t, screens, 1)

	st := driveScreens(t, screens, "2\n0\n")
	assert.Equal(t, []string{"tracing"}, st.All("addons"),
		"an enum-constrained resourceList must render as a multi-select, not free text")
}

// TestRenderQuestions_BindingRefused proves ValidateInputs is actually wired
// into renderQuestions, not merely defined and unwired: a Binding is a
// bundle-install concept (it overlays onto a CR field), and a channel
// question's Wizard.Result applies the answer directly, so a channel
// input carrying one is refused here, not just by calling ValidateInputs
// directly.
func TestRenderQuestions_BindingRefused(t *testing.T) {
	qs := []oap.Question{{
		Name:    "org",
		Type:    oap.QString,
		Prompt:  "GitHub organization",
		Binding: []oap.Binding{{Target: "AgentClass/demo-agent#spec.org"}},
	}}
	_, err := renderQuestions(qs, nil, tui.NewState(), tui.Caps{})
	require.Error(t, err, "a channel input carrying a bundle-install Binding must be refused")
	assert.Contains(t, err.Error(), "org", "the question is named")
}

// TestRenderQuestions_NilStateRefused covers the mode asymmetry a nil st used
// to hide: an unseeded call tolerated it silently (nothing ever touched st),
// while a seeded call panicked inside seedAnswer -> State.Set on the nil
// pointer the moment one --answer key matched a question. renderQuestions now
// refuses a nil st unconditionally, before ValidateInputs or the seeding loop
// ever run, so both paths fail the same way — a named error, not a crash
// that depends on which questions happen to be seeded on a given run.
func TestRenderQuestions_NilStateRefused(t *testing.T) {
	qs := []oap.Question{{Name: "org", Type: oap.QString, Prompt: "GitHub organization"}}

	t.Run("nothing seeded: a nil st must still be refused, not silently tolerated", func(t *testing.T) {
		_, err := renderQuestions(qs, nil, nil, tui.Caps{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "st must not be nil")
	})

	t.Run("a seeded answer: a nil st must be refused, not left to panic inside seedAnswer", func(t *testing.T) {
		_, err := renderQuestions(qs, map[string]string{"org": "demo-org"}, nil, tui.Caps{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "st must not be nil")
	})
}

// TestRenderQuestions_UnknownTypeRefused covers the requirement that a Type
// this renderer does not recognize is an error naming the question and the
// type, never a silent fall-through to a text box.
func TestRenderQuestions_UnknownTypeRefused(t *testing.T) {
	qs := []oap.Question{{Name: "demo", Type: oap.QuestionType("bogus"), Prompt: "Demo"}}
	_, err := renderQuestions(qs, nil, tui.NewState(), tui.Caps{})
	require.Error(t, err, "an unrecognized Type must be refused")
	assert.Contains(t, err.Error(), "demo", "the question is named")
	assert.Contains(t, err.Error(), "bogus", "the offending type is named")
}
