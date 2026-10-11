package browser

// stale_ref_gate_test.go covers the fix for a stale element ref (the page
// re-rendered since the agent last read it) no longer fail-closing stampWebAction
// to a MUTATE. A stale ref means the acted-on element is simply GONE: the call
// cannot execute, so it cannot mutate anything, and gating it only produced a
// spurious web_action approval the user had to dismiss with no grant ever minted
// (because the call never got to use one). Every OTHER fact-read fault (a CDP
// transport error, a JS exception) must still fail closed to a mutate, since
// those mean "we don't know what this would do," not "this cannot run."
//
// No real Chrome is needed: ensureConnected is a no-op once Session.connected is
// true, so a fake Session with connected:true and a plain context.Background()
// browseCtx drives actionFacts' real code path — the stale case returns before
// ever touching chromedp (lookupRef fails first), and the non-stale case
// deliberately lets a lookupRef HIT fall through to a real chromedp.Run call
// against a context with no chromedp Executor, which chromedp itself fails fast
// and synchronously with ErrInvalidContext — a genuine, non-stale CDP-shaped
// fault, not a hand-wavy stand-in for one.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"golang.org/x/sync/semaphore"

	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// fakeConnectedSession builds a Session that believes it is already connected
// (so do() never dials a real browser) over refs, enough to drive actionFacts'
// real lookupRef + (for a hit) real chromedp.Run code paths deterministically.
func fakeConnectedSession(host string, refs map[string]refEntry) *Session {
	return &Session{
		mu:          semaphore.NewWeighted(1),
		connected:   true,
		browseCtx:   context.Background(),
		refs:        refs,
		currentHost: func() string { return host },
	}
}

// TestStampWebActionStaleRefDoesNotGate proves a stale-ref actionFacts error
// stamps NO web_action args (the call is not gated as a mutate) and never
// consults the judge — the whole point being that a call that cannot execute
// needs no per-action revalidation.
func TestStampWebActionStaleRefDoesNotGate(t *testing.T) {
	sess := fakeConnectedSession("site.test", map[string]refEntry{}) // empty: every ref is stale
	cj := &countingJudge{verdict: mutateVerdict()}
	bt := &browserTool{op: "click", rtName: "rt", sess: sess, judge: cj}

	named, ok := bt.NamedArgs(map[string]any{"ref": "e1"})
	require.True(t, ok)

	assert.Equal(t, "site.test", named[hostArgKey], "the host is still stamped for an acting op")
	for _, k := range []string{browserschema.WebActionRequiredArg, browserschema.WebActionKeyArg, browserschema.WebActionFingerprintArg} {
		_, present := named[k]
		assert.False(t, present, "a stale ref must NOT stamp %s — the call cannot execute, so gating it is pointless", k)
	}
	desc, _ := named[browserschema.WebActionDescriptionArg].(string)
	assert.NotEmpty(t, desc, "the description is still stamped for every call, mutate or not (the web_host grant may still need one)")
	assert.Equal(t, 0, cj.calls, "a stale ref must return before ever consulting the judge")
}

// TestStampWebActionNonStaleFaultStillGates proves a fact-read fault that is
// NOT a stale ref (here, a real chromedp.Run failure once past the lookupRef
// hit) still fails closed to a mutate stamp exactly as before this fix.
func TestStampWebActionNonStaleFaultStillGates(t *testing.T) {
	// The ref IS present (lookupRef hits), so actionFacts proceeds past the
	// stale check into a real chromedp call against a context with no chromedp
	// Executor attached — which chromedp itself fails synchronously and
	// immediately (ErrInvalidContext), never hanging. This is a genuine
	// non-stale fact-read fault, not a contrived string.
	sess := fakeConnectedSession("site.test", map[string]refEntry{"e1": {backendNodeID: 1, role: "button"}})
	cj := &countingJudge{verdict: mutateVerdict()}
	bt := &browserTool{op: "click", rtName: "rt", sess: sess, judge: cj}

	named, ok := bt.NamedArgs(map[string]any{"ref": "e1"})
	require.True(t, ok)

	assert.Equal(t, true, named[browserschema.WebActionRequiredArg],
		"a non-stale fact-read fault must still be treated as a mutate (fail-closed)")
	key, _ := named[browserschema.WebActionKeyArg].(string)
	assert.NotEmpty(t, key, "a fail-closed mutate still stamps a key so the second check runs")
	assert.Equal(t, "", named[browserschema.WebActionFingerprintArg],
		"a fail-closed mutate stamps an EMPTY fingerprint, which the web_action check's pagestate_matches caveat refuses")
	desc, _ := named[browserschema.WebActionDescriptionArg].(string)
	assert.NotEmpty(t, desc)
	assert.Equal(t, 0, cj.calls, "a fact-read fault returns before consulting the judge, same as before this fix")
}

// TestExecuteClickOnStaleRefReturnsCleanToolError proves that NOT gating a
// stale ref (the fix above) is safe: Execute's own click handler independently
// re-resolves the ref via lookupRef and refuses with a clean IsError tool
// result before touching the page — so skipping the web_action gate for a
// stale ref never lets a call actually run un-gated.
func TestExecuteClickOnStaleRefReturnsCleanToolError(t *testing.T) {
	sess := fakeConnectedSession("site.test", map[string]refEntry{}) // every ref is stale
	bt := &browserTool{op: "click", rtName: "rt", sess: sess}

	res, err := bt.Execute(context.Background(), []byte(`{"ref":"e1"}`), nil)
	require.NoError(t, err, "a tool-level failure is an IsError Result, never a Go error")
	assert.True(t, res.IsError, "a stale ref must be refused as a clean tool error, not silently succeed")
	assert.Contains(t, res.Content, "stale", "the error should tell the model to read the page again")
}
