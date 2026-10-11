package browser

// pagestate_chrome_test.go exercises Task E5's capturePageState/PageFingerprint
// against a REAL Chrome (startFixture, chrome_test.go), transcribing the laya
// spike's (spike_gate_pagestate.py) scenarios: a seat-change modal with a
// "Confirm seat 14C" button and a fee text, a seat-map, a promo banner, and a
// footer. Skipped with a reason when no Chrome binary is present (spec §7),
// via requireLocalChrome inside startFixture.
//
// The point of this file is the contrast between the two scenario groups:
// benign churn OUTSIDE the target's container ("seat-modal") must leave the
// fingerprint untouched, while anything meaningful IN or ABOUT the target
// (its container's price text, its form's action, its own disabled state, or
// a session-expired redirect) must change it.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pageStateFixtureHTML is the laya spike's BEFORE page, transcribed to real
// markup: a nav link, a seat-map container, the seat-modal container holding
// the fee text + the Confirm/Cancel buttons, a promo banner carrying a
// hashed-looking class, and a footer timestamp.
const pageStateFixtureHTML = `<!doctype html><html><body>` +
	`<nav><a href="/trips">My trips</a></nav>` +
	`<div id="seat-map">` +
	`<div class="seat-map-option selected">14C</div>` +
	`<div class="seat-map-option available">15A</div>` +
	`<p>Trip AA1234 - DFW to SFO - Oct 12</p>` +
	`</div>` +
	`<div id="seat-modal">` +
	`<p>Seat change fee: $342.50</p>` +
	`<form action="/trips/seats/update">` +
	`<button aria-label="Confirm seat 14C">Confirm seat 14C</button>` +
	`</form>` +
	`<button class="modal-cancel">Cancel</button>` +
	`</div>` +
	`<div id="promo-banner" class="css-8xk2p">` +
	`<button class="promo-cta">Shop hotels</button>` +
	`<p>Save 20% on Hilton stays this week!</p>` +
	`</div>` +
	`<footer><p id="footer-ts">Last updated 14:02:11</p></footer>` +
	`</body></html>`

// pageStateFixtureLoginHTML is served at /login: the spike's "session
// expired" page, structurally unrelated to the seats page.
const pageStateFixtureLoginHTML = `<!doctype html><html><body>` +
	`<div id="login-form">` +
	`<p>Your session has expired. Please log in again.</p>` +
	`<input type="text" aria-label="Email or AAdvantage number">` +
	`<form action="/login"><button>Log in</button></form>` +
	`</div></body></html>`

func pageStateFixturePage() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			_, _ = w.Write([]byte(pageStateFixtureLoginHTML))
			return
		}
		_, _ = w.Write([]byte(pageStateFixtureHTML))
	})
}

// mutate runs a JS statement against the session's current live page,
// discarding its result — used to simulate in-place page churn between an
// approval capture and a revalidation capture, without navigating away (which
// would invalidate the target ref under test).
func mutate(t *testing.T, s *Session, script string) {
	t.Helper()
	var ignore any
	evalOn(t, s, script+"; 0", &ignore)
}

func TestPageFingerprintStableAcrossBenignChurnChangesOnMeaningfulDrift(t *testing.T) {
	fx := startFixture(t, pageStateFixturePage())
	tools := fx.synthTools(t, false)
	sess := tools[0].(*browserTool).sess

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// freshState navigates to the pristine seats page, resolves the Confirm
	// button's ref, and captures its PageState + fingerprint — the "before" the
	// laya spike's scenarios are diffed against.
	freshState := func(t *testing.T) (ref string, facts ActionFacts, fp string) {
		t.Helper()
		require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": fx.siteBase + "/"}).IsError)
		read := run(t, toolBy(t, tools, "read"), map[string]any{})
		require.False(t, read.IsError, "read: %s", read.Content)
		ref = refForRole(t, read.Content, "button") // first button in document order: Confirm seat 14C
		var err error
		facts, err = sess.actionFacts(ctx, "click", ref)
		require.NoError(t, err)
		require.Equal(t, "seat-modal", facts.Container, "target's container must resolve to the modal's id")
		ps, err := sess.capturePageState(ctx, facts, ref)
		require.NoError(t, err)
		return ref, facts, PageFingerprint(ps)
	}

	t.Run("identical page produces the identical fingerprint", func(t *testing.T) {
		_, _, fp1 := freshState(t)
		_, _, fp2 := freshState(t)
		assert.Equal(t, fp1, fp2, "re-navigating to byte-identical content must reproduce the fingerprint")
	})

	t.Run("benign churn outside the target container does not change the fingerprint", func(t *testing.T) {
		ref, facts, fpBefore := freshState(t)
		// Ad copy changes, a hashed promo class churns, the footer timestamp
		// ticks, and an unrelated widget (the promo banner) reorders ahead of
		// the seat-map — none of it inside #seat-modal.
		mutate(t, sess, `
			document.querySelector('#promo-banner p').textContent = 'Flash sale: Cancun flights from $199!';
			var promoBtn = document.querySelector('#promo-banner .promo-cta');
			promoBtn.className = 'promo-cta css-99zzq';
			document.querySelector('#footer-ts').textContent = 'Last updated 14:03:05';
			var seatMap = document.getElementById('seat-map');
			var promo = document.getElementById('promo-banner');
			seatMap.parentNode.insertBefore(promo, seatMap);
		`)
		ps, err := sess.capturePageState(ctx, facts, ref)
		require.NoError(t, err)
		assert.Equal(t, fpBefore, PageFingerprint(ps),
			"promo text/class churn, a footer timestamp, and an unrelated widget reorder must not move the fingerprint")
	})

	t.Run("price change inside the target container changes the fingerprint (the TOCTOU case)", func(t *testing.T) {
		ref, facts, fpBefore := freshState(t)
		mutate(t, sess, `document.querySelector('#seat-modal p').textContent = 'Seat change fee: $402.50'`)
		ps, err := sess.capturePageState(ctx, facts, ref)
		require.NoError(t, err)
		assert.NotEqual(t, fpBefore, PageFingerprint(ps),
			"a price change inside the pending action's own modal must flip the fingerprint — approving $342.50 must never authorize $402.50")
	})

	t.Run("target form action change changes the fingerprint", func(t *testing.T) {
		ref, facts, fpBefore := freshState(t)
		mutate(t, sess, `document.querySelector('#seat-modal form').setAttribute('action', '/trips/seats/upgrade-bundle')`)
		ps, err := sess.capturePageState(ctx, facts, ref)
		require.NoError(t, err)
		assert.NotEqual(t, fpBefore, PageFingerprint(ps), "the target's own form action changing must flip the fingerprint")
	})

	t.Run("target's own text change changes the fingerprint (confirm seat vs confirm seat + insurance)", func(t *testing.T) {
		ref, facts, fpBefore := freshState(t)
		mutate(t, sess, `var b = document.querySelector('#seat-modal button[aria-label]');
			b.setAttribute('aria-label', 'Confirm seat 14C + travel insurance');
			b.textContent = 'Confirm seat 14C + travel insurance';`)
		ps, err := sess.capturePageState(ctx, facts, ref)
		require.NoError(t, err)
		assert.NotEqual(t, fpBefore, PageFingerprint(ps),
			"the target button bundling insurance changes its accessible text — approving \"confirm seat\" must not authorize \"confirm seat + insurance\"")
	})

	t.Run("target disabled changes the fingerprint", func(t *testing.T) {
		ref, facts, fpBefore := freshState(t)
		mutate(t, sess, `document.querySelector('#seat-modal button[aria-label]').disabled = true`)
		ps, err := sess.capturePageState(ctx, facts, ref)
		require.NoError(t, err)
		assert.NotEqual(t, fpBefore, PageFingerprint(ps), "the target becoming disabled must flip the fingerprint")
	})

	t.Run("navigating to a login page changes the fingerprint", func(t *testing.T) {
		_, _, fpBefore := freshState(t)
		require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": fx.siteBase + "/login"}).IsError)
		read := run(t, toolBy(t, tools, "read"), map[string]any{})
		require.False(t, read.IsError, "read: %s", read.Content)
		loginRef := refForRole(t, read.Content, "button")
		loginFacts, err := sess.actionFacts(ctx, "click", loginRef)
		require.NoError(t, err)
		ps, err := sess.capturePageState(ctx, loginFacts, loginRef)
		require.NoError(t, err)
		assert.NotEqual(t, fpBefore, PageFingerprint(ps),
			"a session-expired redirect to /login must flip the fingerprint")
	})
}
