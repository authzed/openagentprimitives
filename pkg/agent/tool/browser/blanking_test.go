package browser

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// blankingLoginPage serves a form with a username and password input and a
// submit button whose default GET submission navigates to /landed. Unlike
// fill_test.go's loginPage, it carries NO script that mirrors a typed value
// elsewhere in the DOM: the only way a filled field's value could reach
// read/find is the input's OWN accessibility value — exactly the vector Task
// C5 closes. (A page that mirrors a typed value into some OTHER element is a
// separate, still-open gap — the spec's Phase D taint tracking, not this
// task; fill_test.go's mirror exists only to prove a value reached the page,
// not to exercise C5.)
func blankingLoginPage() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// no-store disables the back-forward cache, so chromedp.NavigateBack does
		// a real reload that fires a load event (a bfcache restore does not, and
		// NavigateBack's internal load-wait would hang on it).
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/landed" {
			_, _ = w.Write([]byte(`<!doctype html><html><body><h1>Welcome</h1></body></html>`))
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><html><body>` +
			`<form method="GET" action="/landed">` +
			`<input type="text" aria-label="UsernameField">` +
			`<input type="password" aria-label="PasswordField">` +
			`<button type="submit">Log in</button>` +
			`</form>` +
			`</body></html>`))
	})
}

// blankingFixture boots the chrome fixture in act mode with a one-entry
// catalog for the fixture site and serves handler, returning the synthesized
// tools, the shared session (for direct blanked-set assertions), and the site
// base URL. Mirrors fill_test.go's browserFillFixture but takes its own
// handler.
func blankingFixture(t *testing.T, handler http.Handler) (tools []agenttool.Tool, sess *Session, siteBase string) {
	t.Helper()
	fx := startFixture(t, handler)
	fx.rt.Spec.Interaction = "act"
	fx.rt.IdentityName = "site-identity"
	fx.rt.Catalog = []spiceboxv1alpha1.BrowserLoginRef{{
		ID:    "login-site",
		Label: "Site",
		Hosts: []spiceboxv1alpha1.BrowserLoginHost{{Host: "site.test"}},
	}}
	ff := &fakeFetcher{}
	var err error
	tools, err = Synthesize(fx.rt, fx.gateURL, chromeToken, true, ff.fetch(), nil, nil)
	require.NoError(t, err)
	require.NotEmpty(t, tools)
	sess = tools[0].(*browserTool).sess
	t.Cleanup(func() { sess.Close() })
	return tools, sess, fx.siteBase
}

// fillUsernameAndPassword navigates to base and types the username/password
// credential placeholders into the page's two inputs, returning their refs.
func fillUsernameAndPassword(t *testing.T, tools []agenttool.Tool, base string) (userRef, passRef string) {
	t.Helper()
	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": base + "/"}).IsError)

	userRef = refByName(t, tools, "UsernameField")
	passRef = refByName(t, tools, "PasswordField")

	uRes := run(t, toolBy(t, tools, "type"), map[string]any{"ref": userRef, "text": "{{login.site.test.username}}"})
	require.False(t, uRes.IsError, "type username: %s", uRes.Content)
	pRes := run(t, toolBy(t, tools, "type"), map[string]any{"ref": passRef, "text": "{{login.site.test.password}}"})
	require.False(t, pRes.IsError, "type password: %s", pRes.Content)
	return userRef, passRef
}

func TestReadOmitsBlankedFieldValue(t *testing.T) {
	tools, sess, base := blankingFixture(t, blankingLoginPage())
	fillUsernameAndPassword(t, tools, base)
	require.Len(t, sess.blankedNodes(), 2, "typing a credential into both fields must mark both blanked")

	read := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, read.IsError, "read: %s", read.Content)
	assert.NotContains(t, read.Content, stubUsername, "a blanked field's value must never reach read")
	assert.NotContains(t, read.Content, stubPassword, "a blanked field's value must never reach read")
	assert.Contains(t, read.Content, "UsernameField", "the element itself must still appear (role/name)")
	assert.Contains(t, read.Content, "PasswordField", "the element itself must still appear (role/name)")
	assert.Contains(t, read.Content, "redacted", "a blanked field must say so, not just render as empty")
}

func TestFindDoesNotRevealBlankedFieldValue(t *testing.T) {
	tools, sess, base := blankingFixture(t, blankingLoginPage())
	fillUsernameAndPassword(t, tools, base)
	require.Len(t, sess.blankedNodes(), 2)

	found := run(t, toolBy(t, tools, "find"), map[string]any{"text": "UsernameField"})
	require.False(t, found.IsError, "find: %s", found.Content)
	assert.NotContains(t, found.Content, stubUsername, "find must not reveal a blanked field's value")

	foundPw := run(t, toolBy(t, tools, "find"), map[string]any{"text": "PasswordField"})
	require.False(t, foundPw.IsError, "find: %s", foundPw.Content)
	assert.NotContains(t, foundPw.Content, stubPassword, "find must not reveal a blanked field's value")
}

func TestScreenshotRefusedWhileFieldBlanked(t *testing.T) {
	tools, sess, base := blankingFixture(t, blankingLoginPage())
	fillUsernameAndPassword(t, tools, base)
	require.NotEmpty(t, sess.blankedNodes())

	res := run(t, toolBy(t, tools, "screenshot"), map[string]any{})
	assert.True(t, res.IsError, "screenshot must be refused while a filled field is unsubmitted")
	assert.Contains(t, res.Content, "screenshot refused")
}

func TestNavigateClearsBlankedAndAllowsScreenshot(t *testing.T) {
	tools, sess, base := blankingFixture(t, blankingLoginPage())
	fillUsernameAndPassword(t, tools, base)
	require.NotEmpty(t, sess.blankedNodes())

	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": base + "/landed"}).IsError)
	assert.Empty(t, sess.blankedNodes(), "navigation must clear the blanked set")

	shot := run(t, toolBy(t, tools, "screenshot"), map[string]any{})
	assert.False(t, shot.IsError, "screenshot must be allowed again after navigation: %s", shot.Content)

	read := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, read.IsError, "read: %s", read.Content)
	assert.NotContains(t, read.Content, "redacted", "a fresh page has nothing blanked to redact")
}

func TestBackClearsBlanked(t *testing.T) {
	tools, sess, base := blankingFixture(t, blankingLoginPage())
	// Start on /landed, navigate to the login page, fill it, then go back. The
	// back() must clear the blanked set just as navigate does — both replace the
	// document.
	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": base + "/landed"}).IsError)
	fillUsernameAndPassword(t, tools, base)
	require.NotEmpty(t, sess.blankedNodes())

	require.False(t, run(t, toolBy(t, tools, "back"), map[string]any{}).IsError)
	assert.Empty(t, sess.blankedNodes(), "back must clear the blanked set")

	shot := run(t, toolBy(t, tools, "screenshot"), map[string]any{})
	assert.False(t, shot.IsError, "screenshot must be allowed again after back: %s", shot.Content)
}

// TestSubmitThatNavigatesDoesNotLeakClearsOnNextNavigate proves the Option-A
// semantics: a submit that really navigates does NOT itself clear the blanked
// set (submit is not a reliable "fields gone" signal), but it also cannot leak
// — the old fields are detached, so read of the new document carries none of
// their values. The stale blanked ids clear on the next genuine navigation.
func TestSubmitThatNavigatesDoesNotLeakClearsOnNextNavigate(t *testing.T) {
	tools, sess, base := blankingFixture(t, blankingLoginPage())
	userRef, _ := fillUsernameAndPassword(t, tools, base)
	require.NotEmpty(t, sess.blankedNodes())

	// The form's default GET submission navigates to /landed, detaching the inputs.
	sub := run(t, toolBy(t, tools, "submit"), map[string]any{"ref": userRef})
	require.False(t, sub.IsError, "submit: %s", sub.Content)

	// The new document holds none of the filled values — no leak, regardless of
	// whether the stale blanked ids have been cleared yet.
	read := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, read.IsError, "read after submit+navigate: %s", read.Content)
	assert.Contains(t, read.Content, "Welcome", "the page must have actually navigated to /landed")
	assert.NotContains(t, read.Content, stubUsername, "a navigated-away document holds no filled value")
	assert.NotContains(t, read.Content, stubPassword, "a navigated-away document holds no filled value")

	// An explicit navigation clears the stale set and re-enables screenshot.
	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": base + "/landed"}).IsError)
	assert.Empty(t, sess.blankedNodes(), "a genuine navigation clears the stale blanked ids")
	shot := run(t, toolBy(t, tools, "screenshot"), map[string]any{})
	assert.False(t, shot.IsError, "screenshot must be allowed after a real navigation: %s", shot.Content)
}

// hideShowPage serves a form plus two buttons that hide / re-show the USERNAME
// input via style.display, WITHOUT removing it from the DOM or clearing its
// value. This is the Critical the AX-presence liveness prune introduced:
// Chromium omits a display:none subtree from the accessibility tree, so a pruned
// field re-renders its real value on re-show. The username (a text input) is the
// vector — its value appears in plaintext in the AX tree, unlike a masked
// password.
func hideShowPage() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body>` +
			`<form>` +
			`<input type="text" aria-label="UsernameField" id="u">` +
			`<input type="password" aria-label="PasswordField">` +
			`</form>` +
			`<button type="button" aria-label="HideBtn" onclick="document.getElementById('u').style.display='none'">Hide</button>` +
			`<button type="button" aria-label="ShowBtn" onclick="document.getElementById('u').style.display=''">Show</button>` +
			`</body></html>`))
	})
}

// TestHiddenThenReshownFieldStaysBlanked is the re-review regression test: a
// filled field hidden via display:none (omitted from the AX tree, still in the
// DOM with its value) and then re-shown must STILL be blanked — read must not
// render its value, and screenshot must stay refused. An AX-presence liveness
// prune would drop it while hidden and leak it on re-show.
func TestHiddenThenReshownFieldStaysBlanked(t *testing.T) {
	tools, sess, base := blankingFixture(t, hideShowPage())
	fillUsernameAndPassword(t, tools, base)
	require.NotEmpty(t, sess.blankedNodes())

	// Baseline: the filled username is redacted, not leaked.
	base0 := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, base0.IsError, "read: %s", base0.Content)
	assert.NotContains(t, base0.Content, stubUsername)

	// Hide the username field (display:none → omitted from the AX tree). The read
	// while hidden is exactly where an AX-presence prune would drop the id.
	hideRef := refByName(t, tools, "HideBtn")
	require.False(t, run(t, toolBy(t, tools, "click"), map[string]any{"ref": hideRef}).IsError)
	hidden := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, hidden.IsError, "read while hidden: %s", hidden.Content)
	assert.NotContains(t, hidden.Content, stubUsername, "a hidden field must not render its value")

	// Re-show the username field WITHOUT retyping; its value is still in the DOM.
	showRef := refByName(t, tools, "ShowBtn")
	require.False(t, run(t, toolBy(t, tools, "click"), map[string]any{"ref": showRef}).IsError)

	reshown := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, reshown.IsError, "read after re-show: %s", reshown.Content)
	assert.NotContains(t, reshown.Content, stubUsername, "a re-shown filled field must STILL be blanked (not re-exposed)")
	assert.Contains(t, reshown.Content, "redacted", "the re-shown field must still carry the redaction marker")
	assert.NotEmpty(t, sess.blankedNodes(), "hide/show must not drop the field from the blanked set")

	shot := run(t, toolBy(t, tools, "screenshot"), map[string]any{})
	assert.True(t, shot.IsError, "screenshot must stay refused while a filled field (even hidden/re-shown) is present")
}

// spaLoginPage serves a form whose onsubmit changes the URL via history.pushState
// and cancels the real submission (return false), so the password input stays
// MOUNTED after "submit". This is the §3.6 vector a URL-change heuristic would
// get wrong: the URL moves to /dashboard but the credential-filled field is
// still on the page, so it must remain blanked.
func spaLoginPage() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body>` +
			`<form onsubmit="history.pushState({},'','/dashboard');return false">` +
			`<input type="text" aria-label="UsernameField">` +
			`<input type="password" aria-label="PasswordField">` +
			`<button type="submit">Log in</button>` +
			`</form></body></html>`))
	})
}

// TestSPASubmitKeepsMountedFieldBlanked is the Finding-1 regression test: a
// submit that only pushState's a new route (URL changes, field stays mounted)
// must NOT un-blank the field. A URL-based heuristic would wrongly clear here.
func TestSPASubmitKeepsMountedFieldBlanked(t *testing.T) {
	tools, sess, base := blankingFixture(t, spaLoginPage())
	userRef, _ := fillUsernameAndPassword(t, tools, base)
	require.NotEmpty(t, sess.blankedNodes())

	// requestSubmit() fires onsubmit → pushState('/dashboard') → return false
	// cancels the real submit, so the inputs stay mounted while the URL changes.
	sub := run(t, toolBy(t, tools, "submit"), map[string]any{"ref": userRef})
	require.False(t, sub.IsError, "submit: %s", sub.Content)

	read := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, read.IsError, "read after SPA submit: %s", read.Content)
	assert.NotContains(t, read.Content, stubPassword, "a still-mounted filled field must stay blanked despite the URL change")
	assert.NotContains(t, read.Content, stubUsername, "a still-mounted filled field must stay blanked despite the URL change")
	assert.Contains(t, read.Content, "redacted", "the mounted field must still carry the redaction marker")
	assert.NotEmpty(t, sess.blankedNodes(), "a submit that did not navigate must leave the blanked set intact")

	shot := run(t, toolBy(t, tools, "screenshot"), map[string]any{})
	assert.True(t, shot.IsError, "screenshot must stay refused while the filled field is still mounted")
	assert.Contains(t, shot.Content, "screenshot refused")
}
