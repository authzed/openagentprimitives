package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// theSecret is long and distinctive so its presence in a rendered field is
// unambiguous — a masked field renders one asterisk per rune, which must not
// be mistakable for the value itself.
const theSecret = "ngrok-authtoken-supersecret"

// renderWith builds the question's field, binds it to value, and returns what
// the field draws.
//
// The assertions below are about what an operator SEES, read off the rendered
// field rather than off huh's unexported echo mode. Reading the mode would
// test that the constructor set a flag; this tests that the flag does what it
// is set for, and it keeps working if huh renames the field.
func renderWith(t *testing.T, q *Question, value string) string {
	t.Helper()
	require.NotNil(t, q.field, "the constructor must build a field")
	in, ok := q.field().(*huh.Input)
	require.True(t, ok, "a secret question is answered by typing, so its field is an Input")
	in.Value(&value)
	in.Init()
	return in.View()
}

// TestNewSecret_HidesTheValueOnlyWhereTheDriverCanHonorIt is the whole reason
// this constructor exists rather than each caller setting the echo mode.
//
// Under the plain driver huh's accessible renderer takes the password branch
// only when the reader has an Fd (field_input.go), and Form.runAccessible
// DISCARDS the error when it does not. A masked field off-TTY therefore prints
// no prompt, consumes no input line, and leaves the bound value empty — an
// empty credential, reported as success. Echoing is the lesser harm, so the
// mode follows the driver.
func TestNewSecret_HidesTheValueOnlyWhereTheDriverCanHonorIt(t *testing.T) {
	cases := []struct {
		name   string
		caps   Caps
		hidden bool
	}{
		{
			name:   "a TTY can hide the value, so the operator never sees it echoed",
			caps:   Caps{TTY: true, Color: true, Width: 80},
			hidden: true,
		},
		{
			name:   "off-TTY it echoes: masking there collects nothing, in silence",
			caps:   Caps{},
			hidden: false,
		},
		{
			name:   "a colorless TTY still hides — masking follows the reader, not the palette",
			caps:   Caps{TTY: true, Width: 80},
			hidden: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view := renderWith(t, NewSecret(tc.caps, TextOpts{
				QuestionOpts: QuestionOpts{Key: "token", Title: "Token"},
			}), theSecret)

			if tc.hidden {
				assert.NotContains(t, view, theSecret, "the credential must not be rendered")
				assert.Contains(t, view, strings.Repeat("*", len(theSecret)),
					"a masked field renders one mask character per rune")
				return
			}
			assert.Contains(t, view, theSecret,
				"off-TTY the value is echoed, because the alternative is an empty credential")
		})
	}
}

// TestNewSecret_DiffersFromPlainTextOnlyInWhatIsShown pins the scope of this
// constructor. A secret that stopped validating, or stopped recording its
// answer, would be a worse bug than an echoed one — so the parts that are NOT
// meant to change are asserted to be unchanged.
func TestNewSecret_DiffersFromPlainTextOnlyInWhatIsShown(t *testing.T) {
	var checked string
	o := TextOpts{
		QuestionOpts: QuestionOpts{Key: "token", Title: "Token"},
		Check: func(_ *State, in string) error {
			checked = in
			return nil
		},
	}

	// The one intended difference, stated as a difference: a constructor that
	// quietly stopped masking would render the two identically and fail here.
	assert.NotEqual(t,
		renderWith(t, NewText(o), theSecret),
		renderWith(t, NewSecret(Caps{TTY: true}, o), theSecret),
		"under a TTY a secret must not render like plain text")

	// ...and off-TTY they are the same field, which is the point of the
	// fallback rather than an accident of it.
	assert.Equal(t,
		renderWith(t, NewText(o), theSecret),
		renderWith(t, NewSecret(Caps{}, o), theSecret),
		"off-TTY a secret IS a plain text field")

	q := NewSecret(Caps{TTY: true}, o)
	require.NoError(t, q.checkTyped(o, "typed-value"))
	assert.Equal(t, "typed-value", checked, "the caller's Check must still run")
	assert.Equal(t, "token", q.opts.Key, "and the answer is still recorded under the caller's key")
}
