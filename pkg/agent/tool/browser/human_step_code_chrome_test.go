package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// codePage serves a single text input (a login "code" field). Its oninput advances
// the page to a sentinel ONLY when the exact code is typed — so a subsequent read
// proves the code reached the field WITHOUT the page (or the test) ever echoing the
// raw code back: the sentinel is deliberately not the code.
func codePage() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body>` +
			`<form>` +
			`<input type="text" aria-label="CodeField" ` +
			`oninput="if(this.value==='123456'){document.getElementById('status').textContent='CODE-ACCEPTED-SIGNED-IN';}">` +
			`</form>` +
			`<p id="status">AWAITING-CODE</p>` +
			`</body></html>`))
	})
}

// browserCodeFixture boots the chrome fixture in act mode with a scripted
// HumanStepRequester that answers the code card, returning the synthesized tools,
// the fake (for its call counter), the shared session (for the blanked set), and
// the site base URL.
func browserCodeFixture(t *testing.T, hs *fakeHumanStep) (tools []agenttool.Tool, sess *Session, siteBase string) {
	t.Helper()
	fx := startFixture(t, codePage())
	fx.rt.Spec.Interaction = "act"
	var err error
	tools, err = Synthesize(fx.rt, fx.gateURL, chromeToken, false, nil, hs, nil)
	require.NoError(t, err)
	require.NotEmpty(t, tools)
	sess = tools[0].(*browserTool).sess
	t.Cleanup(func() { sess.Close() })
	return tools, sess, fx.siteBase
}

// TestRequestHumanStepCode_RoundTripNoLeak is the C7 keystone (Review Focus 3): a
// human provides "123456"; the RUNNER types it into the code field (not a model
// tool call); the page advances; and the code "123456" appears NOWHERE the model
// can see it — not in the tool result, not in the request_human_step call args, and
// not in a later read of the page (the field is blanked).
func TestRequestHumanStepCode_RoundTripNoLeak(t *testing.T) {
	const code = "123456"
	hs := &fakeHumanStep{codeOutcome: HumanStepCodeOutcome{Approved: true, Code: code, DecidedBy: "alice"}}
	tools, sess, siteBase := browserCodeFixture(t, hs)

	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": siteBase + "/"}).IsError)

	codeRef := refByName(t, tools, "CodeField")

	// The model's request_human_step call carries ONLY kind/prompt/ref — never the
	// code (the model does not have it; the human supplies it out of band). Capture
	// the raw args the transcript would record and prove the code is absent.
	humanStepTool := toolBy(t, tools, "request_human_step")
	rawArgs, err := json.Marshal(map[string]any{"kind": "code", "prompt": "enter the 6-digit code we emailed you", "ref": codeRef})
	require.NoError(t, err)
	argsSnapshot := append(json.RawMessage(nil), rawArgs...)

	res, err := humanStepTool.Execute(context.Background(), rawArgs, &agenttool.SessionContext{})
	require.NoError(t, err, "a tool-level outcome is never a Go error")
	require.False(t, res.IsError, "the code round-trip must succeed: %s", res.Content)

	assert.Equal(t, 1, hs.codeCalls, "the code card must have blocked through the requester once")
	assert.Equal(t, 0, hs.confirmCalls, "the code path must not use the confirm path")

	// 1) The tool RESULT is a status only — never the code.
	assert.NotContains(t, res.Content, code, "the tool result must never contain the typed code")
	assert.Contains(t, res.Content, codeRef, "the result names the ref it entered the code into")

	// 2) The recorded tool-call args are unchanged and never held the code.
	assert.Equal(t, argsSnapshot, json.RawMessage(rawArgs), "Execute must not rewrite the recorded args")
	assert.NotContains(t, string(rawArgs), code, "the model's tool-call args never carry the code")

	// 3) The page actually ADVANCED — the code reached the field (the sentinel only
	// appears when the exact code was typed), proving the runner typed it for real.
	after := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, after.IsError, "read after code: %s", after.Content)
	assert.Contains(t, after.Content, "CODE-ACCEPTED-SIGNED-IN", "the page must have advanced, proving the code landed")

	// 4) …and that later read does NOT surface the code: the field was blanked.
	assert.NotContains(t, after.Content, code, "a read after the code must never reveal it (the field is blanked)")

	// The code field was marked for C5 blanking, exactly like a filled credential.
	assert.Len(t, sess.blankedNodes(), 1, "the code field must be marked for blanking")
}

// TestRequestHumanStepCode_TimeoutFailsClosedNoType is the fail-closed half with a
// real Chrome: a lapsed code card types NOTHING into the page and reports an error.
func TestRequestHumanStepCode_TimeoutFailsClosedNoType(t *testing.T) {
	hs := &fakeHumanStep{codeOutcome: HumanStepCodeOutcome{TimedOut: true}}
	tools, sess, siteBase := browserCodeFixture(t, hs)

	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": siteBase + "/"}).IsError)
	codeRef := refByName(t, tools, "CodeField")

	res := run(t, toolBy(t, tools, "request_human_step"), map[string]any{"kind": "code", "prompt": "enter the code", "ref": codeRef})
	assert.True(t, res.IsError, "a lapsed code card must fail closed")
	assert.Contains(t, res.Content, "timed out")

	// Nothing was typed: the page never advanced and no field was blanked.
	after := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, after.IsError)
	assert.NotContains(t, after.Content, "CODE-ACCEPTED-SIGNED-IN", "a timed-out code must type nothing")
	assert.Empty(t, sess.blankedNodes(), "a timed-out code must blank no field")
}
