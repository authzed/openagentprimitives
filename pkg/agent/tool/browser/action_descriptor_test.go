package browser

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestHumanActionDescriptor covers the card-facing "What" line built from
// ActionFacts for every acting op, including the credential-safety rule for
// `type` and the generic/fail-closed fallbacks.
func TestHumanActionDescriptor(t *testing.T) {
	cases := []struct {
		name  string
		facts ActionFacts
		want  string
	}{
		{
			name:  "submit op names the form",
			facts: ActionFacts{Op: "submit", Host: "httpbin.org"},
			want:  "Submit the form on httpbin.org",
		},
		{
			name:  "a click on a submit control names its own label",
			facts: ActionFacts{Op: "click", Role: "button", Text: "submit order", Submits: true, Host: "httpbin.org"},
			want:  `Submit the form by clicking "submit order"`,
		},
		{
			name: "a radio selection names the option, never the role",
			facts: ActionFacts{
				Op: "click", Role: "radio", Text: "medium", Host: "httpbin.org",
			},
			want: `Select "medium"`,
		},
		{
			name:  "a checkbox with no accessible text falls back to the role",
			facts: ActionFacts{Op: "click", Role: "checkbox", Host: "httpbin.org"},
			want:  "Select a checkbox option on httpbin.org",
		},
		{
			name:  "a link names itself",
			facts: ActionFacts{Op: "click", Role: "link", Text: "pricing", Host: "example.com"},
			want:  `Click the "pricing" link`,
		},
		{
			name:  "a tab switch names the tab",
			facts: ActionFacts{Op: "click", Role: "tab", Text: "reviews", Host: "example.com"},
			want:  `Switch to the "reviews" tab`,
		},
		{
			name:  "a generic button click names its own label and role",
			facts: ActionFacts{Op: "click", Role: "button", Text: "Submit order", Host: "httpbin.org"},
			want:  `Click "Submit order" (button)`,
		},
		{
			name:  "a native select op names the control, not the chosen option",
			facts: ActionFacts{Op: "select", Role: "textbox", Text: "Pizza size", Host: "httpbin.org"},
			want:  `Change the "Pizza size" dropdown on httpbin.org`,
		},
		{
			name:  "a label/caption text click (the e13 shape) falls back to role when text is blank",
			facts: ActionFacts{Op: "click", Role: "text", Host: "httpbin.org"},
			want:  "Click a text element on httpbin.org",
		},
		{
			name:  "an empty host falls back to a generic phrase",
			facts: ActionFacts{Op: "click", Role: "button", Text: "go"},
			want:  `Click "go" (button)`,
		},
		{
			name:  "a totally empty fact set (the fail-closed fact-read branch) still names the op and host",
			facts: ActionFacts{Op: "click", Host: "httpbin.org"},
			want:  "Click on httpbin.org",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := humanActionDescriptor(tc.facts)
			assert.Equal(t, tc.want, got)
			assert.NotEmpty(t, got, "the descriptor must never be empty")
		})
	}
}

// TestHumanActionDescriptor_TypeNeverShowsText pins the credential-safety
// rule: ActionFacts.Text for a `type` op can resolve to the field's CURRENT
// VALUE (actionfacts.go's text() falls through to el.value for an input with
// no aria-label), which may be a previously-typed credential. The descriptor
// for a `type` call must never surface Text — only the field's role.
func TestHumanActionDescriptor_TypeNeverShowsText(t *testing.T) {
	const secretLooking = "sk-live-super-secret-password-value"
	facts := ActionFacts{Op: "type", Role: "textbox", Text: secretLooking, Host: "example.com"}

	got := humanActionDescriptor(facts)

	assert.NotContains(t, got, secretLooking, "a type call's descriptor must never surface the field's current value")
	assert.Equal(t, "Type into the text field on example.com", got)
}

// TestHumanActionDescriptor_NeverEmpty is a property check over a sample of
// degenerate fact combinations: the descriptor is a pure, total function and
// must always return a non-empty, single-line sentence, so an approval card
// never falls back to showing nothing.
func TestHumanActionDescriptor_NeverEmpty(t *testing.T) {
	ops := []string{"click", "type", "press", "select", "submit", ""}
	roles := []string{"", "button", "link", "tab", "text", "radio", "checkbox", "textbox"}
	for _, op := range ops {
		for _, role := range roles {
			f := ActionFacts{Op: op, Role: role}
			got := humanActionDescriptor(f)
			assert.NotEmpty(t, got, "op=%q role=%q must still produce a descriptor", op, role)
			assert.False(t, strings.Contains(got, "\n"), "the descriptor must be ONE line: %q", got)
		}
	}
}
