package agentui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiaction"
)

// goldenLivePath is the ONE artifact both halves of the LIVE-FRAME seam read:
// live.go's liveActionMessage/liveActionEntry/liveViewMessage on the Go side,
// and ui/useActionLifecycle.ts's LiveActionMessage/LiveActionEntry/
// LiveViewMessage on the browser side. It covers BOTH arms of the socket's
// message union — every frame type handleFrame switches on has a captured
// instance here. It is a SEPARATE file from the other four goldens in this
// package for the same reason each of those is separate from the others:
//
//   - ui/testdata/props.golden.json pins the GET bootstrap seam.
//   - ui/testdata/props.actions.golden.json pins the declaration's action
//     TABLE shape (J3).
//   - ui/testdata/bindings.golden.json pins the POST .../bindings seam.
//   - ui/testdata/actions.golden.json pins the POST .../actions request and
//     response envelopes (J4).
//   - This file pins the frames GET .../live writes — the delivery path for
//     the whole observable lifecycle.
//
// Merging any two would force an edit to one contract to churn the other's
// fixture and tests.
//
// # Why this exists
//
// The live frame carries approvalAddressedToViewer — the field useActionLifecycle.ts
// turns into the chrome-revealing trust event the design spec makes
// non-negotiable — and, on the view arm, the whole merged declaration an
// update_view lands on an open page as. Before this file, a ONE-SIDED rename
// of any json tag on either arm left the Go suite green (every Go test
// decoded frames back through the same Go struct) AND the frontend suite
// green (every TS test hand-built its own frame literals), while in
// production toActionState yielded `approvalAddressedToViewer: undefined`, no
// trust event was ever pushed, and chrome silently stopped auto-revealing —
// or handleFrame's shape guard rejected every view frame and the page stopped
// updating until reload.
//
// With this file in place there is no edit to one side alone that keeps both
// suites green: rename a tag and this test fails; regenerate the golden to
// match and ui/liveGolden.test.tsx fails instead, because it reads every
// field BY NAME through the TS mirror and then drives the REAL page with the
// frame's own bytes — the old name it still asks for is simply absent from
// the regenerated file.
//
// To change the contract deliberately: update live.go, regenerate this file
// (see writeLiveGolden), and update useActionLifecycle.ts's mirror together.
const goldenLivePath = "ui/testdata/live.golden.json"

// goldenLive is the golden's own shape: REAL frames captured off a real
// websocket, in the order a browser receives them across one action's life
// and one view update. Every field is json.RawMessage — this file compares
// the route's BYTES against the file's bytes, and decoding either through a
// Go struct on the way would launder exactly the tag renames the comparison
// exists to catch.
type goldenLive struct {
	// Snapshot is the frame runActionMirror sends immediately on connect —
	// the reload arm, read from memory.
	Snapshot json.RawMessage `json:"snapshot"`
	// ApprovalEvent is a live transition into awaiting_approval that IS
	// addressed to the connected viewer. The only frame in this feature that
	// must reveal chrome.
	ApprovalEvent json.RawMessage `json:"approvalEvent"`
	// SettledEvent is the terminal transition that re-enables the control.
	SettledEvent json.RawMessage `json:"settledEvent"`
	// ViewOpen is the view arm's OPEN-TIME frame: every connect sends one,
	// carrying the whole merged declaration and NO hook (the field is
	// omitempty and the open frame names none — it is a snapshot, not one
	// hook's update). useActionLifecycle.ts's onView compares it against the
	// declaration it already holds, so its exact bytes are what decides
	// whether a page load costs a second binding fan-out.
	ViewOpen json.RawMessage `json:"viewOpen"`
	// ViewUpdate is the frame a ui_view_update push produces: the same
	// envelope WITH a hook, carrying a merged declaration whose written hook
	// is named in agentComposed. This is the frame that makes an update_view
	// land on an already-open page (J4).
	ViewUpdate json.RawMessage `json:"viewUpdate"`
	// ErrorFrame is the action arm's "error" frame — the only producer of
	// liveActionMessage.Message, browser-safe copy useActionLifecycle renders
	// as its liveError notice. Captured from the nil-Memory fail-closed path,
	// which is the one production path that authors it.
	ErrorFrame json.RawMessage `json:"errorFrame"`
}

// goldenLiveTimes are the fixed UpdatedAt stamps the three frames carry.
// Fixed rather than time.Now() so the captured bytes are byte-stable across
// runs; the values themselves carry no meaning beyond being distinct and
// ordered.
var goldenLiveTimes = []time.Time{
	time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC),
	time.Date(2026, 1, 2, 15, 4, 6, 0, time.UTC),
	time.Date(2026, 1, 2, 15, 4, 7, 0, time.UTC),
}

const (
	goldenLiveSubject   = "user:viewer-golden"
	goldenLiveRequestID = "req-golden-1"
	goldenLiveAction    = "advance"
	// goldenLiveComposedNode is the fragment the capture writes through the
	// real uiview.Runtime so the view-update frame carries a slot the merge
	// actually marked agentComposed. Its rendered text is what
	// useActionLifecycle.test.tsx looks for after feeding the page these very
	// bytes.
	goldenLiveComposedNode = `{"component":"ap:markdown","props":{"body":"agent copy"}}`
)

// captureLiveGolden drives the REAL liveHandler over a REAL websocket and
// returns the three frames' raw bytes, exactly as a browser receives them.
// Nothing here constructs a liveActionMessage by hand: the point of the
// golden is that it is production output.
func captureLiveGolden(t *testing.T) goldenLive {
	t.Helper()
	f := newLiveFixture(t, goldenLiveSubject)

	// Seeded BEFORE the dial, with nobody subscribed, so this record reaches
	// the browser through the snapshot arm (memory) rather than the stream.
	f.settleFull(t, goldenLiveSubject, goldenLiveRequestID, goldenLiveAction, uiaction.StateSubmitted, false, goldenLiveTimes[0])

	conn := f.dial(t)
	snapshot := f.readRaw(t, conn)
	// The view arm's own open-time frame, sent right after the action
	// snapshot on every connect. Captured, not discarded: it is the frame
	// every page load receives, and the only one that carries NO slot.
	viewOpen := f.readViewRaw(t, conn)

	// The snapshot frame proves subscribe was CALLED; ensureSubscribed proves
	// the SUB frame reached the server, so the publishes below cannot race it.
	f.ensureSubscribed(t)
	f.settleFull(t, goldenLiveSubject, goldenLiveRequestID, goldenLiveAction, uiaction.StateAwaitingApproval, true, goldenLiveTimes[1])
	approval := f.readRaw(t, conn)

	f.settleFull(t, goldenLiveSubject, goldenLiveRequestID, goldenLiveAction, uiaction.StateSucceeded, false, goldenLiveTimes[2])
	settled := f.readRaw(t, conn)

	// A real update_view write through the real Runtime, then the push that
	// makes webd re-resolve and send it. Nothing here hand-builds a view
	// frame: the declaration on it is whatever resolveView actually produced.
	_, err := f.rt.Write(context.Background(), liveViewHook, mustNode(t, goldenLiveComposedNode))
	require.NoError(t, err)
	f.publishViewUpdate(t, liveViewHook)
	viewUpdate := f.readViewRaw(t, conn)

	return goldenLive{
		Snapshot: snapshot, ApprovalEvent: approval, SettledEvent: settled,
		ViewOpen: viewOpen, ViewUpdate: viewUpdate, ErrorFrame: captureLiveErrorFrame(t),
	}
}

// captureLiveErrorFrame drives the REAL route with no Memory configured — the
// one production path that authors liveActionMessage.Message — and returns
// that frame's bytes. A separate fixture because a nil Memory is a
// whole-connection condition: the route writes the error frame and closes,
// so it cannot be captured on the same socket as the frames above.
func captureLiveErrorFrame(t *testing.T) json.RawMessage {
	t.Helper()
	f := newLiveFixture(t, goldenLiveSubject)
	f.d.mem = nil
	return f.readRaw(t, f.dial(t))
}

// TestLiveFramesMatchTheGoldenTheBrowserParses pins liveActionMessage's and
// liveActionEntry's json tags AND the state/message literals they carry, by
// asserting the REAL route's bytes against the file ui/liveGolden.test.tsx
// reads.
//
// Set AP_UPDATE_GOLDEN=1 to rewrite the golden from the current route output
// — the only supported way to regenerate it, so a regeneration is always a
// deliberate act with a diff to review rather than a hand edit.
func TestLiveFramesMatchTheGoldenTheBrowserParses(t *testing.T) {
	got := captureLiveGolden(t)

	if os.Getenv("AP_UPDATE_GOLDEN") == "1" {
		writeLiveGolden(t, got)
	}

	raw, err := os.ReadFile(filepath.Clean(goldenLivePath))
	require.NoError(t, err, "the golden the frontend suite reads must exist")
	var want goldenLive
	require.NoError(t, json.Unmarshal(raw, &want), "the golden must be well-formed JSON")

	assert.JSONEq(t, string(want.Snapshot), string(got.Snapshot),
		"the live snapshot frame changed: update live.go, %s, and useActionLifecycle.ts's LiveActionMessage together", goldenLivePath)
	assert.JSONEq(t, string(want.ApprovalEvent), string(got.ApprovalEvent),
		"the live approval event frame changed: update live.go, %s, and useActionLifecycle.ts's LiveActionEntry together", goldenLivePath)
	assert.JSONEq(t, string(want.SettledEvent), string(got.SettledEvent),
		"the live settled event frame changed: update live.go, %s, and useActionLifecycle.ts's LiveActionEntry together", goldenLivePath)
	assert.JSONEq(t, string(want.ViewOpen), string(got.ViewOpen),
		"the open-time view frame changed: update live.go, %s, and useActionLifecycle.ts's LiveViewMessage together", goldenLivePath)
	assert.JSONEq(t, string(want.ViewUpdate), string(got.ViewUpdate),
		"the pushed view frame changed: update live.go, %s, and useActionLifecycle.ts's LiveViewMessage together", goldenLivePath)
	assert.JSONEq(t, string(want.ErrorFrame), string(got.ErrorFrame),
		"the error frame changed: update live.go, %s, and useActionLifecycle.ts's LiveActionMessage together", goldenLivePath)
}

// TestLiveGoldenCarriesTheFieldsTheBrowserDependsOn is the Go half of the
// pin's SEMANTIC arm: the golden is only worth reading if it actually
// exercises the fields the browser interprets. A golden whose approval frame
// happened to omit approvalAddressedToViewer (it is `omitempty`) would look
// like a pin while protecting nothing — the field would be absent from the
// file whether or not the tag was renamed.
func TestLiveGoldenCarriesTheFieldsTheBrowserDependsOn(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(goldenLivePath))
	require.NoError(t, err)
	var g goldenLive
	require.NoError(t, json.Unmarshal(raw, &g))

	var approval map[string]any
	require.NoError(t, json.Unmarshal(g.ApprovalEvent, &approval))
	entry, ok := approval["action"].(map[string]any)
	require.True(t, ok, "the approval event frame must carry an `action` object")
	assert.Equal(t, true, entry["approvalAddressedToViewer"],
		"the golden's approval frame must carry approvalAddressedToViewer=true, or the TS pin protects nothing")
	assert.Equal(t, string(uiaction.StateAwaitingApproval), entry["state"])

	var snapshot map[string]any
	require.NoError(t, json.Unmarshal(g.Snapshot, &snapshot))
	actions, ok := snapshot["actions"].([]any)
	require.True(t, ok, "the snapshot frame must carry an `actions` array")
	require.Len(t, actions, 1)

	// liveViewMessage.Hook is `omitempty`, so a golden captured only from the
	// open-time frame would carry no `hook` key at all and the TS pin on it
	// would protect nothing. Both polarities are asserted here: the pushed
	// frame MUST carry it, the open-time frame MUST NOT.
	var viewUpdate map[string]any
	require.NoError(t, json.Unmarshal(g.ViewUpdate, &viewUpdate))
	assert.Equal(t, "view", viewUpdate["type"])
	assert.Equal(t, liveViewHook, viewUpdate["hook"],
		"the golden's pushed view frame must carry `hook`, or the TS pin on that omitempty field protects nothing")

	var viewOpen map[string]any
	require.NoError(t, json.Unmarshal(g.ViewOpen, &viewOpen))
	assert.Equal(t, "view", viewOpen["type"])
	assert.NotContains(t, viewOpen, "hook",
		"the open-time frame is a whole-document snapshot; naming a hook on it would tell the browser one hook changed when none did")

	// declarationWire.AgentComposed is `omitempty` too — the marker the design
	// makes unsuppressible. The pushed frame's written hook must be named in it.
	decl, ok := viewUpdate["declaration"].(map[string]any)
	require.True(t, ok, "the view frame must carry a `declaration` object")
	composed, ok := decl["agentComposed"].([]any)
	require.True(t, ok, "the view frame's declaration must carry an `agentComposed` array")
	assert.Contains(t, composed, liveViewHook,
		"the golden's pushed view frame must mark its written hook agent-composed, or the TS pin on that omitempty field protects nothing")

	var errFrame map[string]any
	require.NoError(t, json.Unmarshal(g.ErrorFrame, &errFrame))
	assert.Equal(t, "error", errFrame["type"])
	assert.NotEmpty(t, errFrame["message"],
		"the golden's error frame must carry the browser-safe copy useActionLifecycle renders, or that field is unpinned")
}

// writeLiveGolden rewrites the golden with a stable, readable layout. Only
// reached under AP_UPDATE_GOLDEN=1.
func writeLiveGolden(t *testing.T, g goldenLive) {
	t.Helper()
	out, err := json.MarshalIndent(g, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Clean(goldenLivePath), append(out, '\n'), 0o600))
	t.Logf("rewrote %s", goldenLivePath)
}
