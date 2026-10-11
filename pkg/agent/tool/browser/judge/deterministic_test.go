package judge_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/browser/judge"
)

func TestDeterministicName(t *testing.T) {
	assert.Equal(t, "deterministic", judge.NewDeterministic().Name())
}

func TestDeterministicClassify(t *testing.T) {
	cases := []struct {
		name   string
		action judge.Action
		want   judge.Tier
	}{
		{
			name: "plain link-follow resolves to navigate",
			action: judge.Action{
				Op: "click", Role: "link", Text: "pricing",
				FormAction: "/pricing", Host: "example.com", PageURL: "https://example.com/",
			},
			want: judge.TierNavigate,
		},
		{
			name: "search role textbox entry resolves to navigate",
			action: judge.Action{
				Op: "type", Role: "searchbox", Text: "search", FormHasSubmit: true,
				Host: "example.com", PageURL: "https://example.com/",
			},
			want: judge.TierNavigate,
		},
		{
			name: "generic textbox labeled search resolves to navigate",
			action: judge.Action{
				Op: "type", Role: "textbox", Text: "search products", FormHasSubmit: true,
				Host: "example.com", PageURL: "https://example.com/catalog",
			},
			want: judge.TierNavigate,
		},
		{
			name: "tab switch resolves to observe",
			action: judge.Action{
				Op: "click", Role: "tab", Text: "reviews",
				Host: "example.com", PageURL: "https://example.com/product",
			},
			want: judge.TierObserve,
		},
		{
			name: "unknown consequential click on a button fails closed to mutate",
			action: judge.Action{
				Op: "click", Role: "button", Text: "buy now",
				FormAction: "/purchase", Host: "example.com", PageURL: "https://example.com/cart",
			},
			want: judge.TierMutate,
		},
		{
			name: "link text carrying a mutating verb fails closed to mutate",
			action: judge.Action{
				Op: "click", Role: "link", Text: "delete account",
				FormAction: "/account/delete", Host: "example.com", PageURL: "https://example.com/settings",
			},
			want: judge.TierMutate,
		},
		{
			name: "link with no real href fails closed to mutate",
			action: judge.Action{
				Op: "click", Role: "link", Text: "details",
				FormAction: "", Host: "example.com", PageURL: "https://example.com/",
			},
			want: judge.TierMutate,
		},
		{
			name: "typing into any textbox is a local field edit, resolves to navigate",
			action: judge.Action{
				Op: "type", Role: "textbox", Text: "amount", FormHasSubmit: true,
				Host: "example.com", PageURL: "https://example.com/transfer",
			},
			want: judge.TierNavigate,
		},
		{
			name:   "ambiguous empty action fails closed to mutate",
			action: judge.Action{},
			want:   judge.TierMutate,
		},
		{
			name: "destructive href disguised with benign link text fails closed to mutate",
			action: judge.Action{
				Op: "click", Role: "link", Text: "Yes, I'm sure",
				FormAction: "/account/delete", Host: "example.com", PageURL: "https://example.com/settings",
			},
			want: judge.TierMutate,
		},
		{
			name: "empty-text link with any href fails closed to mutate",
			action: judge.Action{
				Op: "click", Role: "link", Text: "",
				FormAction: "/foo", Host: "example.com", PageURL: "https://example.com/",
			},
			want: judge.TierMutate,
		},
		{
			name: "genuine navigation link with benign path and text resolves to navigate",
			action: judge.Action{
				Op: "click", Role: "link", Text: "My trips",
				FormAction: "/trips", Host: "example.com", PageURL: "https://example.com/",
			},
			want: judge.TierNavigate,
		},
		{
			name: "destructive href disguised as a tab click fails closed to mutate",
			action: judge.Action{
				Op: "click", Role: "tab", Text: "reviews",
				FormAction: "/account/delete", Host: "example.com", PageURL: "https://example.com/product",
			},
			want: judge.TierMutate,
		},
		{
			name: "typing into a field whose form action is destructive still resolves to navigate (typing commits nothing)",
			action: judge.Action{
				Op: "type", Role: "searchbox", Text: "50000", FormHasSubmit: true,
				FormAction: "/accounts/transfer/execute", Host: "example.com", PageURL: "https://example.com/transfer",
			},
			want: judge.TierNavigate,
		},
		{
			name: "typing into a textbox nested in a destructive form resolves to navigate",
			action: judge.Action{
				Op: "type", Role: "textbox", Text: "search", FormHasSubmit: true,
				FormAction: "/accounts/transfer/execute", Host: "example.com", PageURL: "https://example.com/transfer",
			},
			want: judge.TierNavigate,
		},
		{
			name: "genuine search box with a benign form action resolves to navigate",
			action: judge.Action{
				Op: "type", Role: "searchbox", Text: "shoes", FormHasSubmit: true,
				FormAction: "/search", Host: "example.com", PageURL: "https://example.com/",
			},
			want: judge.TierNavigate,
		},
		{
			// The exact regression: httpbin's pizza-order form posts to /post,
			// and "post"/"order" are mutatingVerbs; typing a field value must no
			// longer be dragged to mutate by the enclosing form's action. The form
			// has its own submit button, so FormHasSubmit is true.
			name: "typing a field value in a form that POSTs to /post resolves to navigate",
			action: judge.Action{
				Op: "type", Role: "textbox", Text: "jane doe", FormHasSubmit: true,
				FormAction: "/post", Host: "httpbin.org", PageURL: "https://httpbin.org/forms/post",
			},
			want: judge.TierNavigate,
		},
		{
			name: "selecting a radio resolves to navigate",
			action: judge.Action{
				Op: "click", Role: "radio", Text: "medium", FormHasSubmit: true,
				FormAction: "/post", Host: "httpbin.org", PageURL: "https://httpbin.org/forms/post",
			},
			want: judge.TierNavigate,
		},
		{
			name: "ticking a checkbox resolves to navigate",
			action: judge.Action{
				Op: "click", Role: "checkbox", Text: "extra cheese", FormHasSubmit: true,
				FormAction: "/post", Host: "httpbin.org", PageURL: "https://httpbin.org/forms/post",
			},
			want: judge.TierNavigate,
		},
		{
			name: "picking a listbox option resolves to navigate",
			action: judge.Action{
				Op: "click", Role: "option", Text: "large", FormHasSubmit: true,
				FormAction: "/order", Host: "example.com", PageURL: "https://example.com/order",
			},
			want: judge.TierNavigate,
		},
		{
			name: "choosing a menuitemradio resolves to navigate",
			action: judge.Action{
				Op: "click", Role: "menuitemradio", Text: "price: low to high", FormHasSubmit: true,
				Host: "example.com", PageURL: "https://example.com/results",
			},
			want: judge.TierNavigate,
		},
		{
			// The autosave-on-type fail-open the hardening closes: a field with NO
			// enclosing submit-bearing form (a live-doc editor, an autosave input
			// whose own oninput handler fetch(POST)s) must now fail closed to
			// mutate instead of being cleared unconditionally.
			name: "typing into a field with no enclosing submit-bearing form fails closed to mutate (autosave-on-input)",
			action: judge.Action{
				Op: "type", Role: "textbox", Text: "draft content", FormHasSubmit: false,
				Host: "example.com", PageURL: "https://example.com/notes/42",
			},
			want: judge.TierMutate,
		},
		{
			// The instant-save-toggle fail-open the hardening closes: a checkbox
			// with NO enclosing submit-bearing form (its own onchange handler
			// fetch(POST)s on every click) must now fail closed to mutate.
			name: "ticking a checkbox with no enclosing submit-bearing form fails closed to mutate (instant-save toggle)",
			action: judge.Action{
				Op: "click", Role: "checkbox", Text: "auto-publish", FormHasSubmit: false,
				Host: "example.com", PageURL: "https://example.com/settings",
			},
			want: judge.TierMutate,
		},
		{
			// A field inside a <form> that has NO submit control at all (not just
			// "no form") must also fail closed — FormHasSubmit, not mere form
			// membership, is the gate.
			name: "typing into a field inside a submit-less form fails closed to mutate",
			action: judge.Action{
				Op: "type", Role: "textbox", Text: "note", FormHasSubmit: false,
				FormAction: "/notes/42", Host: "example.com", PageURL: "https://example.com/notes/42",
			},
			want: judge.TierMutate,
		},
		{
			// A submit control is a COMMIT: it must NEVER be cleared, even though
			// its role is "button". Submits=true keeps it out of every benign rule.
			name: "click on a submit control (Submits=true) fails closed to mutate",
			action: judge.Action{
				Op: "click", Role: "button", Text: "submit order", Submits: true,
				FormAction: "/post", Host: "httpbin.org", PageURL: "https://httpbin.org/forms/post",
			},
			want: judge.TierMutate,
		},
		{
			// "Not type=submit" is not a reason to clear: a generic button may
			// still mutate via an onclick JS/fetch, so it falls through to mutate.
			name: "click on a generic button (Submits=false) fails closed to mutate",
			action: judge.Action{
				Op: "click", Role: "button", Text: "add to cart", Submits: false,
				Host: "example.com", PageURL: "https://example.com/dp/123",
			},
			want: judge.TierMutate,
		},
		{
			// The Submits guard beats the selection-toggle role: a mislabeled
			// submit control carrying role=radio must still gate.
			name: "a submit control mislabeled role=radio is not cleared (Submits guard wins)",
			action: judge.Action{
				Op: "click", Role: "radio", Text: "place order", Submits: true,
				FormAction: "/checkout/confirm", Host: "example.com", PageURL: "https://example.com/checkout",
			},
			want: judge.TierMutate,
		},
		{
			// A radio is a selection toggle, but op=press is not a click: presses
			// are never cleared (an Enter in a form field is a commit).
			name: "pressing a key (not a click) on a radio fails closed to mutate",
			action: judge.Action{
				Op: "press", Role: "radio", Text: "medium",
				Host: "example.com", PageURL: "https://example.com/order",
			},
			want: judge.TierMutate,
		},
		{
			// A menuitem (plain, not *radio/*checkbox) is an arbitrary command,
			// e.g. "Log out" — a selection role lookalike that must stay gated.
			name: "clicking a plain menuitem fails closed to mutate",
			action: judge.Action{
				Op: "click", Role: "menuitem", Text: "log out",
				FormAction: "/logout", Host: "example.com", PageURL: "https://example.com/account",
			},
			want: judge.TierMutate,
		},
		{
			// The e13 gap this closes: a click that resolves to a field's LABEL
			// text (role folded to "text" by snapshot.go's interestingRole)
			// rather than the input/radio it labels. Inside a submit-bearing
			// form this is exactly as benign as the field edit it was aiming for.
			name: "clicking a field's label text inside a submit-bearing form resolves to navigate",
			action: judge.Action{
				Op: "click", Role: "text", Text: "customer name", FormHasSubmit: true,
				FormAction: "/post", Host: "httpbin.org", PageURL: "https://httpbin.org/forms/post",
			},
			want: judge.TierNavigate,
		},
		{
			// Same label click, but with NO enclosing submit-bearing form: falls
			// through to mutate, mirroring the autosave/instant-save residual
			// isFieldEntry/isSelectionToggle already accept.
			name: "clicking inert text with no enclosing submit-bearing form fails closed to mutate",
			action: judge.Action{
				Op: "click", Role: "text", Text: "notes", FormHasSubmit: false,
				Host: "example.com", PageURL: "https://example.com/notes/42",
			},
			want: judge.TierMutate,
		},
		{
			// The disguise defense: label-looking text carrying a mutatingVerb is
			// never cleared, even inside a submit-bearing form.
			name: "clicking text carrying a mutating verb fails closed to mutate",
			action: judge.Action{
				Op: "click", Role: "text", Text: "delete my account", FormHasSubmit: true,
				FormAction: "/post", Host: "example.com", PageURL: "https://example.com/account",
			},
			want: judge.TierMutate,
		},
		{
			// Submits guard wins even for a mislabeled "text" role: a true submit
			// control must never be cleared regardless of how its role folded.
			name: "a submit control mislabeled role=text is not cleared (Submits guard wins)",
			action: judge.Action{
				Op: "click", Role: "text", Text: "place order", Submits: true, FormHasSubmit: true,
				FormAction: "/post", Host: "httpbin.org", PageURL: "https://httpbin.org/forms/post",
			},
			want: judge.TierMutate,
		},
	}

	d := judge.NewDeterministic()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := d.Classify(context.Background(), tc.action, judge.PageContext{})
			require.NoError(t, err, "the deterministic judge must never error")
			assert.Equal(t, tc.want, v.Tier)
			require.NotNil(t, v.Probs, "Probs must always be populated")
			assert.InDelta(t, 1.0, v.Probs[tc.want], 0.0001, "the chosen tier must carry full confidence")
			assert.NotEmpty(t, v.Rationale, "a judge must explain its verdict")

			if tc.want == judge.TierMutate {
				assert.Equal(t, map[judge.Tier]float64{judge.TierMutate: 1.0}, v.Probs,
					"the fail-closed default must be a one-hot mutate distribution")
			}
		})
	}
}

// TestDeterministicNeverClearsCommits pins the security invariant directly: a
// COMMIT — a submit control (Submits=true), or a link/tab whose href path or
// accessible text carries a mutatingVerb — is NEVER cleared as benign by the
// deterministic path, even when it wears a shape (a selection role, a nav link)
// that the benign recognizers otherwise clear. This is the property the whole
// task turns on: the fast-path may clear field EDITS, never the commit.
func TestDeterministicNeverClearsCommits(t *testing.T) {
	d := judge.NewDeterministic()
	assertMutate := func(t *testing.T, a judge.Action) {
		t.Helper()
		v, err := d.Classify(context.Background(), a, judge.PageContext{})
		require.NoError(t, err, "the deterministic judge must never error")
		assert.Equal(t, judge.TierMutate, v.Tier, "a commit must never be cleared as benign: %+v", a)
	}

	t.Run("Submits=true is never cleared, across every op/role the benign rules otherwise clear", func(t *testing.T) {
		benign := []judge.Action{
			{Op: "type", Role: "textbox", Text: "name"},
			{Op: "type", Role: "searchbox", Text: "search"},
			{Op: "click", Role: "radio", Text: "medium"},
			{Op: "click", Role: "checkbox", Text: "nonstop"},
			{Op: "click", Role: "option", Text: "large"},
			{Op: "click", Role: "menuitemradio", Text: "sort"},
			{Op: "click", Role: "menuitemcheckbox", Text: "wrap"},
			{Op: "click", Role: "text", Text: "medium"},
		}
		for _, a := range benign {
			a.Submits = true
			a.FormHasSubmit = true
			a.Host = "example.com"
			a.PageURL = "https://example.com/checkout"
			t.Run(a.Op+"/"+a.Role, func(t *testing.T) { assertMutate(t, a) })
		}
	})

	t.Run("FormHasSubmit=false is never cleared, across every op/role the benign rules otherwise clear", func(t *testing.T) {
		// The hardening's core property: a field/toggle with no enclosing
		// submit-bearing form (autosave-on-input, an instant-save toggle) must
		// fail closed to mutate even though its op/role is exactly the shape
		// isFieldEntry/isSelectionToggle otherwise clear.
		benign := []judge.Action{
			{Op: "type", Role: "textbox", Text: "name"},
			{Op: "type", Role: "searchbox", Text: "search"},
			{Op: "click", Role: "radio", Text: "medium"},
			{Op: "click", Role: "checkbox", Text: "nonstop"},
			{Op: "click", Role: "option", Text: "large"},
			{Op: "click", Role: "menuitemradio", Text: "sort"},
			{Op: "click", Role: "menuitemcheckbox", Text: "wrap"},
			{Op: "click", Role: "text", Text: "notes"},
		}
		for _, a := range benign {
			a.FormHasSubmit = false
			a.Host = "example.com"
			a.PageURL = "https://example.com/settings"
			t.Run(a.Op+"/"+a.Role, func(t *testing.T) { assertMutate(t, a) })
		}
	})

	t.Run("a link or tab whose href path carries a mutating verb is never cleared", func(t *testing.T) {
		for _, role := range []string{"link", "tab"} {
			a := judge.Action{Op: "click", Role: role, Text: "continue",
				FormAction: "/account/delete", Host: "example.com", PageURL: "https://example.com/settings"}
			t.Run(role, func(t *testing.T) { assertMutate(t, a) })
		}
	})

	t.Run("a link or tab whose accessible text carries a mutating verb is never cleared", func(t *testing.T) {
		for _, role := range []string{"link", "tab"} {
			a := judge.Action{Op: "click", Role: role, Text: "delete account",
				FormAction: "/safe/path", Host: "example.com", PageURL: "https://example.com/settings"}
			t.Run(role, func(t *testing.T) { assertMutate(t, a) })
		}
	})
}
