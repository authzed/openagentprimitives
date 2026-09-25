import { describe, expect, it } from "vitest";
import type { LiveActionEntry, LiveActionMessage, LiveViewMessage } from "./useActionLifecycle";
import golden from "./testdata/live.golden.json";

// testdata/live.golden.json is the ONE artifact both halves of the LIVE-FRAME
// seam read: pkg/web/webui/agentui/live_golden_internal_test.go captures the REAL
// bytes GET .../live writes over a real websocket and asserts them against it;
// this file re-derives every field through useActionLifecycle's own
// LiveActionMessage/LiveActionEntry mirror and asserts them BY NAME.
//
// Asserting by name is what makes this a pin rather than decoration. A
// render-only assertion (does the chrome reveal, does the caption appear) does
// not catch a COORDINATED rename — rename the Go json tag and regenerate the
// golden, and a test that only echoes the file's own values back at itself
// stays green while the browser silently stops reading the field. The named
// reads below fail the instant the Go side and this golden rename a key
// together, because the OLD name this file still asks for is simply absent
// from the regenerated file.
//
// useActionLifecycle.test.tsx carries the other half: it drives the REAL page
// with these same frames' bytes and asserts the consequence (chrome reveals,
// the control disables and re-enables). Field names here, behavior there.

const snapshot = golden.snapshot as LiveActionMessage;
const approvalEvent = golden.approvalEvent as LiveActionMessage;
const settledEvent = golden.settledEvent as LiveActionMessage;
const viewOpen = golden.viewOpen as unknown as LiveViewMessage;
const viewUpdate = golden.viewUpdate as unknown as LiveViewMessage;
const errorFrame = golden.errorFrame as LiveActionMessage;

describe("the live-frame wire contract (live.go ↔ useActionLifecycle.ts)", () => {
  it("names the two frame types handleFrame switches on", () => {
    expect(snapshot.type).toBe("snapshot");
    expect(approvalEvent.type).toBe("event");
    expect(settledEvent.type).toBe("event");
  });

  it("carries the snapshot's entries under the key applySnapshot reads", () => {
    expect(snapshot.actions).toHaveLength(1);
    const entry = snapshot.actions?.[0] as LiveActionEntry;
    expect(entry.requestId).toBe("req-golden-1");
    expect(entry.action).toBe("advance");
    expect(entry.state).toBe("submitted");
    expect(entry.message).toBe("Request submitted.");
    expect(entry.updatedAt).toBe("2026-01-02T15:04:05Z");
  });

  it("carries approvalAddressedToViewer on the event frame — the field chrome auto-reveal depends on", () => {
    const entry = approvalEvent.action as LiveActionEntry;
    expect(entry.state).toBe("awaiting_approval");
    // The whole reason this golden exists: a one-sided rename of this tag used
    // to leave BOTH suites green while the spec's one non-negotiable (a trust
    // event auto-reveals chrome) silently stopped firing in production.
    expect(entry.approvalAddressedToViewer).toBe(true);
    expect(entry.message).toBe("Your approval is needed to continue.");
    expect(entry.requestId).toBe("req-golden-1");
    expect(entry.action).toBe("advance");
  });

  it("carries the terminal transition under the same entry shape", () => {
    const entry = settledEvent.action as LiveActionEntry;
    expect(entry.state).toBe("succeeded");
    expect(entry.action).toBe("advance");
    expect(entry.updatedAt).toBe("2026-01-02T15:04:07Z");
  });

  it("never carries the requester — live.go strips it, and this mirror must not invent it", () => {
    for (const entry of [snapshot.actions?.[0], approvalEvent.action, settledEvent.action]) {
      expect(entry).toBeDefined();
      expect(Object.keys(entry as object)).not.toContain("requester");
    }
  });

  it("carries the error frame's browser-safe copy under the key handleFrame reads", () => {
    expect(errorFrame.type).toBe("error");
    // handleFrame surfaces this as the page's liveError notice. A rename of
    // liveActionMessage.Message would leave that notice permanently blank
    // (LIVE_ERROR_FALLBACK) with no suite noticing — the frame still arrives,
    // it just says nothing the server actually wrote.
    expect(errorFrame.message).toBe("this view's live updates are unavailable right now");
  });
});

// The view arm shares this socket and this golden. Both frames are captured
// from the real route: `viewOpen` is what EVERY connect sends, `viewUpdate` is
// what a ui_view_update push produces. Read by name here for the same reason
// the action arm is: a coordinated rename (change the Go json tag AND
// regenerate the golden) leaves the Go side green, and only a by-name read of
// the OLD key here goes red.
describe("the live view-frame wire contract (live.go ↔ useActionLifecycle.ts)", () => {
  it("names the frame type handleFrame's view arm switches on", () => {
    expect(viewOpen.type).toBe("view");
    expect(viewUpdate.type).toBe("view");
  });

  it("carries the merged page tree under the key handleFrame's guard and onView read", () => {
    // handleFrame rejects a frame whose `declaration.view` is not a node and
    // keeps the previous declaration, so a rename of this key does not error —
    // it silently stops every live view update forever.
    expect(typeof viewUpdate.declaration.view).toBe("object");
    expect(viewUpdate.declaration.view.component).toBe("ap:stack");
    // The written region is an oap:generative hook INSIDE that tree — the one
    // structural type in the vocabulary, and the only thing update_view can
    // target. A frame whose written content arrived anywhere else would leave
    // the agent's write unaddressable.
    const written = viewUpdate.declaration.view.children?.[0];
    expect(written?.component).toBe("oap:generative");
    expect(written?.props?.name).toBe("panel");
    expect(written?.children?.[0]?.component).toBe("ap:markdown");
  });

  it("carries agentComposed as a top-level list of hook names, never as a node field", () => {
    // agentComposed is `omitempty` on the Go side and SERVER-COMPUTED: it names
    // the hooks whose content (or emptiness) is the agent's, and GenerativeHook
    // keys the unsuppressible marker off it via HookState.composed. A node field
    // would let a fill mark its own work; the Go validator rejects any
    // structural key it does not know on a node, and this read is what keeps
    // the wire honest about which of the two shapes it is.
    expect(viewUpdate.declaration.agentComposed).toEqual(["panel"]);
    const written = viewUpdate.declaration.view.children?.[0] as { agentComposed?: unknown };
    expect(written.agentComposed).toBeUndefined();
  });

  it("names the changed hook on a pushed frame and no hook on the open-time frame", () => {
    // `hook` is omitempty. Its presence on the push and absence on the open
    // frame is what tells a reader which of the two a frame is — and it is what
    // the per-hook updated cue is keyed on, which matters most for a CLEAR: the
    // open-time frame's byte-identical declaration is what handleView must
    // recognize as an echo rather than a change, so on a clear the hook name is
    // the only evidence the agent acted at all.
    expect(viewUpdate.hook).toBe("panel");
    expect(Object.keys(viewOpen)).not.toContain("hook");
    expect(viewOpen.declaration.view.children?.[0]?.children?.[0]?.component).toBe("ap:text");
  });
});
