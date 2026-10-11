package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// loginPage serves a form with a text username input and a password input, each
// mirroring its live value into a visible <p> via oninput — so a test can prove
// the typed value actually reached the page WITHOUT the browser tool echoing it.
func loginPage() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body>` +
			`<form>` +
			`<input type="text" aria-label="UsernameField" oninput="document.getElementById('uout').textContent='u:'+this.value">` +
			`<input type="password" aria-label="PasswordField" oninput="document.getElementById('pout').textContent='p:'+this.value">` +
			`</form>` +
			`<p id="uout">uidle</p><p id="pout">pidle</p>` +
			`</body></html>`))
	})
}

// browserFillFixture boots the chrome fixture in act mode with a one-entry
// catalog for the fixture site, returning the synthesized tools, the fake
// fetcher (for its call counter), the shared session (for the blanked set), and
// the site base URL.
func browserFillFixture(t *testing.T) (tools []agenttool.Tool, ff *fakeFetcher, sess *Session, siteBase string) {
	t.Helper()
	fx := startFixture(t, loginPage())
	fx.rt.Spec.Interaction = "act"
	fx.rt.IdentityName = "site-identity"
	fx.rt.Catalog = []spiceboxv1alpha1.BrowserLoginRef{{
		ID:    "login-site",
		Label: "Site",
		Hosts: []spiceboxv1alpha1.BrowserLoginHost{{Host: "site.test"}},
	}}
	ff = &fakeFetcher{}
	var err error
	tools, err = Synthesize(fx.rt, fx.gateURL, chromeToken, false, ff.fetch(), nil, nil, nil)
	require.NoError(t, err)
	require.NotEmpty(t, tools)
	sess = tools[0].(*browserTool).sess
	t.Cleanup(func() { sess.Close() })
	return tools, ff, sess, fx.siteBase
}

// refByName finds the first [eN] ref whose outline line contains name.
func refByName(t *testing.T, tools []agenttool.Tool, name string) string {
	t.Helper()
	res := run(t, toolBy(t, tools, "find"), map[string]any{"text": name})
	require.False(t, res.IsError, "find %q: %s", name, res.Content)
	for _, line := range strings.Split(res.Content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[e") {
			continue
		}
		if end := strings.IndexByte(line, ']'); end > 1 {
			return line[1:end]
		}
	}
	t.Fatalf("no ref found for %q in:\n%s", name, res.Content)
	return ""
}

func TestTypeFillsCredentialsWithoutLeaking(t *testing.T) {
	tools, ff, sess, siteBase := browserFillFixture(t)
	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": siteBase + "/"}).IsError)

	userRef := refByName(t, tools, "UsernameField")
	passRef := refByName(t, tools, "PasswordField")

	typeTool := toolBy(t, tools, "type")

	// Type the username placeholder into the text input. Capture the RAW args the
	// runner loop would record for the transcript, and prove Execute never rewrites
	// them to the substituted value (the fill runs strictly inside Execute).
	userArgs, err := json.Marshal(map[string]any{"ref": userRef, "text": "{{login.site.test.username}}"})
	require.NoError(t, err)
	userSnapshot := append(json.RawMessage(nil), userArgs...)
	uRes, err := typeTool.Execute(context.Background(), userArgs, &agenttool.SessionContext{})
	require.NoError(t, err)
	assert.False(t, uRes.IsError, "type username: %s", uRes.Content)
	assert.NotContains(t, uRes.Content, stubUsername, "the tool result must NOT echo the typed value")
	assert.Equal(t, userSnapshot, json.RawMessage(userArgs), "the recorded args must keep the placeholder, unchanged by Execute")
	assert.Contains(t, string(userArgs), "{{login.site.test.username}}")
	assert.NotContains(t, string(userArgs), stubUsername)

	// Type the password placeholder into the password input.
	pRes := run(t, typeTool, map[string]any{"ref": passRef, "text": "{{login.site.test.password}}"})
	assert.False(t, pRes.IsError, "type password: %s", pRes.Content)
	assert.NotContains(t, pRes.Content, stubPassword, "the tool result must NOT echo the typed value")

	// Both values actually reached the page (the oninput mirrors prove it).
	after := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, after.IsError, "read after type: %s", after.Content)
	assert.Contains(t, after.Content, "u:"+stubUsername, "the username value must have reached the page")
	assert.Contains(t, after.Content, "p:"+stubPassword, "the password value must have reached the page")

	// One Fetch for the host, despite two placeholders across two calls (the hold).
	assert.Equal(t, 1, ff.fetches, "the hold must Fetch once per host")

	// Both fields were recorded for C5 blanking.
	assert.Len(t, sess.blankedNodes(), 2, "both typed-into fields must be marked for blanking")
}

func TestTypeRule3PasswordIntoTextInputFailsClosed(t *testing.T) {
	tools, ff, _, siteBase := browserFillFixture(t)
	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": siteBase + "/"}).IsError)

	userRef := refByName(t, tools, "UsernameField") // a type=text input

	res := run(t, toolBy(t, tools, "type"), map[string]any{"ref": userRef, "text": "{{login.site.test.password}}"})
	assert.True(t, res.IsError, "a password placeholder into a non-password input must fail")
	assert.Contains(t, res.Content, "password input field")
	assert.NotContains(t, res.Content, stubPassword)
	assert.Equal(t, 0, ff.fetches, "rule 3 must deny before any Fetch")

	after := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, after.IsError)
	assert.NotContains(t, after.Content, "u:"+stubPassword, "nothing may have been typed on a refused fill")
}

// TestTypeNeverMutatesRecordedArgs is the no-Chrome guard on the transcript
// property: loop_dispatch records the RAW tool args before Execute (and only
// stripArgTags touches them — no expansion), so if Execute leaves the raw bytes
// untouched the transcript keeps the {{login...}} placeholder, never the value.
// Pointed at a dead gate so no Chrome is needed; the tool errors, but the point
// is that the recorded bytes are never rewritten.
func TestTypeNeverMutatesRecordedArgs(t *testing.T) {
	sess := newSession("http://127.0.0.1:1", "tok")
	// Fail the connect instantly: a pre-cancelled allocator context makes
	// ensureConnected's chromedp.Run return at once rather than dialing.
	sess.newAllocator = func(parent context.Context) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		cancel()
		return ctx, cancel
	}
	bt := &browserTool{op: "type", sess: sess, filler: newCredFiller(sess, nil, nil)}

	raw := json.RawMessage(`{"ref":"e3","text":"before {{login.aa.example.password}} after"}`)
	snapshot := append(json.RawMessage(nil), raw...)

	_, err := bt.Execute(context.Background(), raw, &agenttool.SessionContext{})
	require.NoError(t, err, "a tool-level failure is an IsError result, not a Go error")
	assert.Equal(t, snapshot, raw, "Execute must not rewrite the recorded args")
	assert.Contains(t, string(raw), "{{login.aa.example.password}}", "the placeholder must survive in the recorded args")
}
