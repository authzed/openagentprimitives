package browser

// actionfacts_chrome_test.go exercises Task E3's DOM-derived action facts
// against a REAL Chrome (startFixture, chrome_test.go): the AX snapshot gives
// role/text but never form_action/container, so this is the one place those
// two fields can be proven at all. Skipped with a reason when no Chrome binary
// is present (spec §7), via requireLocalChrome inside startFixture.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// actionFactsFixturePage serves a button nested in a form nested in an
// identified container (the hashed class "css-x7k2p" alongside the stable id
// "seat-modal" is the point: Container must pick the id, never the churning
// hashed class), plus an unrelated nav link outside any form.
func actionFactsFixturePage() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body>` +
			`<nav><a href="/trips">My trips</a></nav>` +
			`<div id="seat-modal" class="modal css-x7k2p">` +
			`<form action="/trips/seats/update">` +
			`<button aria-label="Confirm seat 14C">Confirm seat 14C</button>` +
			`</form>` +
			`</div>` +
			`</body></html>`))
	})
}

func TestActionFactsFormButtonAndLink(t *testing.T) {
	fx := startFixture(t, actionFactsFixturePage())
	tools := fx.synthTools(t, false)
	sess := tools[0].(*browserTool).sess

	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": fx.siteBase + "/"}).IsError)

	read := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, read.IsError, "read: %s", read.Content)
	buttonRef := refForRole(t, read.Content, "button")
	linkRef := refForRole(t, read.Content, "link")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	t.Run("button inside a form inside an identified container", func(t *testing.T) {
		facts, err := sess.actionFacts(ctx, "click", buttonRef)
		require.NoError(t, err, "actionFacts must succeed for a live ref")

		assert.Equal(t, "click", facts.Op)
		assert.Equal(t, "button", facts.Role)
		assert.Equal(t, "confirm seat 14c", facts.Text, "normalized: trimmed, collapsed, lowercased")
		assert.Equal(t, "/trips/seats/update", facts.FormAction, "the enclosing form's action, url-pathed")
		assert.Equal(t, "seat-modal", facts.Container, "the id — NOT the hashed css-x7k2p class")
		assert.Equal(t, "site.test", facts.Host)
		assert.Contains(t, facts.PageURL, fx.siteBase)
		assert.True(t, facts.Submits, "a bare <button> inside a <form> is a submit control (HTML default type=submit)")
	})

	t.Run("plain link outside any form", func(t *testing.T) {
		facts, err := sess.actionFacts(ctx, "click", linkRef)
		require.NoError(t, err, "actionFacts must succeed for a live ref")

		assert.Equal(t, "click", facts.Op)
		assert.Equal(t, "link", facts.Role)
		assert.Equal(t, "my trips", facts.Text)
		assert.Equal(t, "/trips", facts.FormAction, "no enclosing form; falls back to the link's own href path")
		assert.Equal(t, "nav", facts.Container, "no id/class on any ancestor; falls back to the nearest ancestor's tag")
		assert.Equal(t, "site.test", facts.Host)
		assert.False(t, facts.Submits, "a plain <a> link is not a form submit control")
	})

	t.Run("stale ref is a wrapped error, not a silently-zeroed result", func(t *testing.T) {
		_, err := sess.actionFacts(ctx, "click", "e999")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "stale")
	})
}

// TestActionFactsNeverLeaksFilledFieldValue proves the credential-leak fix:
// ActionFacts.Text for a later acting call that targets a FILLED field is empty,
// never the typed value — so the value reaches neither the approval descriptor
// nor the web_action object id (both sinks). Two guards, tested separately:
// text() no longer falls through to el.value (a form control's current value is
// not an accessible name), and actionFacts blanks Text outright for any node in
// the session's blanked set (the vector text()'s per-property walk cannot cover,
// e.g. contenteditable).
func TestActionFactsNeverLeaksFilledFieldValue(t *testing.T) {
	const secret = "hunter2-should-never-leak"
	fx := startFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A text field pre-filled with a secret (standing in for a credential the
		// filler typed), with NO aria-label — so text() has nothing but el.value to
		// fall back to, which is exactly the leak this proves is gone.
		_, _ = w.Write([]byte(`<!doctype html><html><body>` +
			`<form action="/login">` +
			`<input type="text" value="` + secret + `">` +
			`<button>Sign in</button>` +
			`</form></body></html>`))
	}))
	fx.rt.Spec.Interaction = "act"
	tools := fx.synthTools(t, false)
	sess := tools[0].(*browserTool).sess

	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": fx.siteBase + "/"}).IsError)

	read := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, read.IsError, "read: %s", read.Content)
	fieldRef := refForRole(t, read.Content, "textbox")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	t.Run("el.value fallback is gone: a filled field has empty Text (no accessible name)", func(t *testing.T) {
		facts, err := sess.actionFacts(ctx, "click", fieldRef)
		require.NoError(t, err, "actionFacts must succeed for a live ref")
		assert.Empty(t, facts.Text, "a form control's current value is not an accessible name; Text must stay empty")
		assert.NotContains(t, ActionKey(facts), secret, "the web_action object id must never carry the typed value")
		assert.NotContains(t, humanActionDescriptor(facts), secret, "the approval descriptor must never carry the typed value")
	})

	t.Run("a blanked (credential-filled) node has its Text redacted outright", func(t *testing.T) {
		// Simulate the filler having typed a credential here: record the node in
		// the blanked set, exactly as the type tool does after a placeholder lands.
		e, ok := sess.lookupRef(fieldRef)
		require.True(t, ok, "fieldRef must still be live")
		sess.markBlanked(e.backendNodeID)

		facts, err := sess.actionFacts(ctx, "submit", fieldRef)
		require.NoError(t, err, "actionFacts must succeed for a live ref")
		assert.Empty(t, facts.Text, "a blanked node's Text must be blanked regardless of what the DOM walk returns")
		assert.NotContains(t, ActionKey(facts), secret)
		assert.NotContains(t, humanActionDescriptor(facts), secret)
	})
}

// formHasSubmitFixturePage serves three textbox fields exercising
// FormHasSubmit's three cases: inside a form WITH a submit control, inside a
// form with NO submit control, and with no enclosing form at all.
func formHasSubmitFixturePage() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body>` +
			`<form action="/order">` +
			`<input type="text" aria-label="Field with submit">` +
			`<button aria-label="Place order">Place order</button>` +
			`</form>` +
			`<form action="/notes/update">` +
			`<input type="text" aria-label="Field without submit">` +
			`</form>` +
			`<input type="text" aria-label="Standalone field">` +
			`</body></html>`))
	})
}

// refForRoleAndName extracts the ref id for the outline line whose rendered
// role AND accessible name both match. refForRole alone cannot disambiguate
// this fixture's three same-role ("textbox") fields.
func refForRoleAndName(t *testing.T, outline, role, name string) string {
	t.Helper()
	want := fmt.Sprintf("%s %q", role, name)
	for _, line := range strings.Split(outline, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "[") {
			continue
		}
		closeIdx := strings.Index(trimmed, "]")
		if closeIdx < 0 {
			continue
		}
		ref := trimmed[1:closeIdx]
		rest := strings.TrimSpace(trimmed[closeIdx+1:])
		if strings.HasPrefix(rest, want) {
			return ref
		}
	}
	t.Fatalf("no [eN] %s line found in outline:\n%s", want, outline)
	return ""
}

// TestActionFactsFormHasSubmit proves the FormHasSubmit action fact (the
// hardening this file backs): true for a field inside a form that contains a
// submit control, false for a field inside a submit-less form, and false for
// a field with no enclosing form at all.
func TestActionFactsFormHasSubmit(t *testing.T) {
	fx := startFixture(t, formHasSubmitFixturePage())
	tools := fx.synthTools(t, false)
	sess := tools[0].(*browserTool).sess

	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": fx.siteBase + "/"}).IsError)

	read := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, read.IsError, "read: %s", read.Content)

	withSubmitRef := refForRoleAndName(t, read.Content, "textbox", "Field with submit")
	withoutSubmitRef := refForRoleAndName(t, read.Content, "textbox", "Field without submit")
	standaloneRef := refForRoleAndName(t, read.Content, "textbox", "Standalone field")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	t.Run("field in a form with a submit button: FormHasSubmit=true", func(t *testing.T) {
		facts, err := sess.actionFacts(ctx, "type", withSubmitRef)
		require.NoError(t, err, "actionFacts must succeed for a live ref")
		assert.True(t, facts.FormHasSubmit, "the enclosing form has a submit button")
		assert.False(t, facts.Submits, "the field itself is not a submit control")
	})

	t.Run("field in a form with no submit control: FormHasSubmit=false", func(t *testing.T) {
		facts, err := sess.actionFacts(ctx, "type", withoutSubmitRef)
		require.NoError(t, err, "actionFacts must succeed for a live ref")
		assert.False(t, facts.FormHasSubmit, "the enclosing form has no submit control anywhere in it")
	})

	t.Run("standalone field with no enclosing form: FormHasSubmit=false", func(t *testing.T) {
		facts, err := sess.actionFacts(ctx, "type", standaloneRef)
		require.NoError(t, err, "actionFacts must succeed for a live ref")
		assert.False(t, facts.FormHasSubmit, "there is no enclosing form at all")
	})
}
