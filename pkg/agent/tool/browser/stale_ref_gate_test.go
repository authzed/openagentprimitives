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
	"strconv"
	"strings"
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

// TestNamedArgsStripsModelSuppliedWebActionKeys proves a model cannot inject the
// reserved web_action keys: NamedArgs deletes any copy the model put in the call
// BEFORE stamping its own, so none survives on ANY path. This drives the stale-ref
// exemption path specifically — the benign, don't-gate path that previously let a
// model's own injected keys flow straight through to the dispatch layer.
func TestNamedArgsStripsModelSuppliedWebActionKeys(t *testing.T) {
	// A ref present in the map would be re-read; use a stale (absent) but NON-EMPTY
	// ref so stampWebAction takes the exemption branch and stamps only a description.
	sess := fakeConnectedSession("site.test", map[string]refEntry{})
	cj := &countingJudge{verdict: benignVerdict()}
	bt := &browserTool{op: "click", rtName: "rt", sess: sess, judge: cj}

	named, ok := bt.NamedArgs(map[string]any{
		"ref":                                 "s9e9", // non-empty, stale
		browserschema.WebActionRequiredArg:    true,
		browserschema.WebActionKeyArg:         "attacker.example/click/anything-deadbeef",
		browserschema.WebActionFingerprintArg: "forged-fingerprint",
		browserschema.WebActionDescriptionArg: "Innocuous-looking thing",
	})
	require.True(t, ok)

	_, hasReq := named[browserschema.WebActionRequiredArg]
	assert.False(t, hasReq, "the model's injected web_action_required must be stripped on the stale/don't-gate path")
	_, hasKey := named[browserschema.WebActionKeyArg]
	assert.False(t, hasKey, "the model's forged web_action_key must be stripped")
	_, hasFp := named[browserschema.WebActionFingerprintArg]
	assert.False(t, hasFp, "the model's forged fingerprint must be stripped")
	desc, _ := named[browserschema.WebActionDescriptionArg].(string)
	assert.NotEqual(t, "Innocuous-looking thing", desc,
		"the description must be the tool's own phrasing, never the model's injected one")
	assert.NotEmpty(t, desc, "the tool still stamps its own description for the coarse web_host card")
}

// TestStampPressClassifiesByKey proves the BLOCKER fix: a `press` carries no
// element ref (its Execute sends the key to document.activeElement, ignoring any
// ref), so it is classified from its KEY, not from a ref'd element. Per the
// "only submission commits" rule, a key that can COMMIT or ACTIVATE a form —
// Enter/Return/Space — is a mutate and gated; every other key edits or navigates
// local page state and is NOT, so a press that merely moves focus or scrolls
// never raises a spurious approval (the over-gating a blanket "gate every press"
// fix would have reintroduced). The judge is never consulted either way. In this
// unit env there is no real Chrome, so the focus-anchored fingerprint capture
// fails and a gated press stamps an empty fingerprint (fail-closed) — the live
// non-empty fingerprint is covered by the chrome suite.
func TestStampPressClassifiesByKey(t *testing.T) {
	cases := []struct {
		name      string
		key       string
		wantGated bool
	}{
		{name: "Enter commits a form → gated", key: "Enter", wantGated: true},
		{name: "Return commits a form → gated", key: "Return", wantGated: true},
		{name: "Space activates the focused control → gated", key: "Space", wantGated: true},
		{name: "ArrowDown only scrolls/moves focus → not gated", key: "ArrowDown", wantGated: false},
		{name: "Escape cancels → not gated", key: "Escape", wantGated: false},
		{name: "Tab moves focus → not gated", key: "Tab", wantGated: false},
		{name: "a single character edits a field → not gated", key: "a", wantGated: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := fakeConnectedSession("site.test", map[string]refEntry{})
			// A mutate verdict must never be reached — stampPress classifies by
			// key and never consults the judge, so cj.calls stays 0 regardless.
			cj := &countingJudge{verdict: mutateVerdict()}
			bt := &browserTool{op: "press", rtName: "rt", sess: sess, judge: cj}

			named, ok := bt.NamedArgs(map[string]any{"key": tc.key})
			require.True(t, ok)

			_, gated := named[browserschema.WebActionRequiredArg]
			assert.Equal(t, tc.wantGated, gated, "press %q gating", tc.key)
			if tc.wantGated {
				key, _ := named[browserschema.WebActionKeyArg].(string)
				assert.True(t, strings.HasPrefix(key, "site.test/press/"),
					"the key names the host and the press op, got %q", key)
				assert.Contains(t, key, strings.ToLower(tc.key),
					"the key names the committing key so Enter and Space don't collide, got %q", key)
				assert.Equal(t, "", named[browserschema.WebActionFingerprintArg],
					"no real Chrome in unit env → focus capture fails → empty fingerprint (fail-closed)")
			}
			desc, _ := named[browserschema.WebActionDescriptionArg].(string)
			assert.Contains(t, desc, tc.key, "every press card names the actual key")
			assert.Equal(t, 0, cj.calls, "press is classified by key, never via the judge")
		})
	}
}

// TestStampPressIgnoresModelSuppliedRef is the fail-open regression: the press
// schema has no `ref`, but args are not additionalProperties:false, so a model
// can smuggle one in. press's Execute ignores it (the key goes to the focused
// element), so stampWebAction must ignore it too. Honoring it was the hole: a
// bogus ref made actionFacts return ErrStaleRef and the old `ref != ""`
// exemption waved a committing press through UN-GATED; a benign-but-resolving ref
// would have mis-classified a press off the wrong element.
func TestStampPressIgnoresModelSuppliedRef(t *testing.T) {
	t.Run("Enter with a bogus (stale) ref is still gated", func(t *testing.T) {
		sess := fakeConnectedSession("site.test", map[string]refEntry{}) // e99 is stale
		bt := &browserTool{op: "press", rtName: "rt", sess: sess, judge: &countingJudge{verdict: benignVerdict()}}

		named, ok := bt.NamedArgs(map[string]any{"key": "Enter", "ref": "e99"})
		require.True(t, ok)
		assert.Equal(t, true, named[browserschema.WebActionRequiredArg],
			"a committing press must gate regardless of a model-supplied ref — the ref-escape fail-open")
	})
	t.Run("Escape with a live, mutating ref is still not gated", func(t *testing.T) {
		// e5 resolves to a button; a mutate verdict is ready. If press honored the
		// ref it would classify as a mutate — it must not, because press ignores
		// the ref entirely and Escape never commits.
		sess := fakeConnectedSession("site.test", map[string]refEntry{"e5": {backendNodeID: 5, role: "button"}})
		bt := &browserTool{op: "press", rtName: "rt", sess: sess, judge: &countingJudge{verdict: mutateVerdict()}}

		named, ok := bt.NamedArgs(map[string]any{"key": "Escape", "ref": "e5"})
		require.True(t, ok)
		_, gated := named[browserschema.WebActionRequiredArg]
		assert.False(t, gated, "a non-committing press stays un-gated no matter what ref is attached")
	})
}

// TestRefGenerationInvalidatesOldRefAcrossRemint proves the concurrent-dispatch
// fix at the session level: a ref minted under one generation is STALE after a
// re-mint, even though the element index ("e1") is identical, because the
// generation nonce in the name differs. This is the structural guarantee that a
// parallel web_read re-minting refs between a tool call's classify (NamedArgs)
// and its Execute can never rebind the model's ref to a different element.
func TestRefGenerationInvalidatesOldRefAcrossRemint(t *testing.T) {
	sess := &Session{mu: semaphore.NewWeighted(1), refs: map[string]refEntry{}}

	// First snapshot: generation 1, one element at index 1.
	gen1 := sess.nextRefGen()
	ref1 := "s" + strconv.FormatUint(gen1, 10) + "e1"
	sess.setRefs(map[string]refEntry{ref1: {backendNodeID: 11, role: "button"}})
	e, ok := sess.lookupRef(ref1)
	require.True(t, ok)
	assert.Equal(t, int64(11), e.backendNodeID)

	// A parallel read re-mints: generation 2, a DIFFERENT element under the SAME
	// index, as a page re-render / renumber would produce.
	gen2 := sess.nextRefGen()
	ref2 := "s" + strconv.FormatUint(gen2, 10) + "e1"
	sess.setRefs(map[string]refEntry{ref2: {backendNodeID: 22, role: "link"}})

	assert.NotEqual(t, ref1, ref2, "the re-mint renumbers under a fresh generation nonce")
	_, ok = sess.lookupRef(ref1)
	assert.False(t, ok, "the prior generation's ref must be stale after a re-mint, not rebind to the new element")
	got, ok := sess.lookupRef(ref2)
	require.True(t, ok)
	assert.Equal(t, int64(22), got.backendNodeID, "only the current generation's ref resolves")
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
