package browser

// actionfacts_chrome_test.go exercises Task E3's DOM-derived action facts
// against a REAL Chrome (startFixture, chrome_test.go): the AX snapshot gives
// role/text but never form_action/container, so this is the one place those
// two fields can be proven at all. Skipped with a reason when no Chrome binary
// is present (spec §7), via requireLocalChrome inside startFixture.

import (
	"context"
	"net/http"
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
	})

	t.Run("stale ref is a wrapped error, not a silently-zeroed result", func(t *testing.T) {
		_, err := sess.actionFacts(ctx, "click", "e999")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "stale")
	})
}
