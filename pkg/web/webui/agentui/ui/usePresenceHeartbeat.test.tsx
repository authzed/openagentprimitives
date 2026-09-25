import "@testing-library/jest-dom/vitest";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render } from "@testing-library/react";
import * as React from "react";

import { PRESENCE_HEARTBEAT_MS, PRESENCE_INTERACTION_WINDOW_MS, usePresenceHeartbeat } from "./usePresenceHeartbeat";

afterEach(cleanup);

// The gating conditions ARE the design. A heartbeat holds a runner pod alive,
// so each condition that stops it from being sent is a cost bound, and each
// one that lets it through is a viewer not having their dashboard die
// underneath them. They are asserted individually because they fail
// independently.
function Harness({ send }: { send: () => void }) {
  usePresenceHeartbeat({ send });
  return null;
}

let visibility: DocumentVisibilityState = "visible";
let focused = true;

beforeEach(() => {
  vi.useFakeTimers();
  visibility = "visible";
  focused = true;
  vi.spyOn(document, "visibilityState", "get").mockImplementation(() => visibility);
  vi.spyOn(document, "hasFocus").mockImplementation(() => focused);
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("usePresenceHeartbeat", () => {
  it("reports immediately on mount, before any interval elapses", () => {
    // Opening the page IS an interaction. Without this a viewer who opens a
    // dashboard and reads it would send nothing for a whole interval — the
    // window in which a runner that is already near its TTL would exit.
    const send = vi.fn();
    render(<Harness send={send} />);
    expect(send).toHaveBeenCalledTimes(1);
  });

  it("keeps reporting on the interval while watched", () => {
    const send = vi.fn();
    render(<Harness send={send} />);
    act(() => void vi.advanceTimersByTime(PRESENCE_HEARTBEAT_MS * 3));
    expect(send.mock.calls.length).toBeGreaterThanOrEqual(4); // mount + 3
  });

  it("stops while the tab is hidden", () => {
    // A backgrounded tab is not somebody looking at a dashboard, and holding
    // a pod for one is the cost this condition exists to refuse.
    const send = vi.fn();
    render(<Harness send={send} />);
    send.mockClear();
    visibility = "hidden";
    act(() => void vi.advanceTimersByTime(PRESENCE_HEARTBEAT_MS * 3));
    expect(send).not.toHaveBeenCalled();
  });

  it("stops while the window is blurred", () => {
    // Visible but unfocused is the second-monitor case: the pixels are on
    // screen and nobody is there.
    const send = vi.fn();
    render(<Harness send={send} />);
    send.mockClear();
    focused = false;
    act(() => void vi.advanceTimersByTime(PRESENCE_HEARTBEAT_MS * 3));
    expect(send).not.toHaveBeenCalled();
  });

  it("stops once the interaction window lapses, even while visible and focused", () => {
    // The condition that bounds an abandoned-but-focused window. Without it a
    // dashboard left open on a wall display holds a runner indefinitely.
    const send = vi.fn();
    render(<Harness send={send} />);
    act(() => void vi.advanceTimersByTime(PRESENCE_INTERACTION_WINDOW_MS + PRESENCE_HEARTBEAT_MS));
    send.mockClear();
    act(() => void vi.advanceTimersByTime(PRESENCE_HEARTBEAT_MS * 3));
    expect(send).not.toHaveBeenCalled();
  });

  it("resumes when the viewer interacts again", () => {
    // The lapse must be recoverable without a reload: someone who returns to a
    // tab and scrolls is watching again.
    const send = vi.fn();
    render(<Harness send={send} />);
    act(() => void vi.advanceTimersByTime(PRESENCE_INTERACTION_WINDOW_MS + PRESENCE_HEARTBEAT_MS));
    send.mockClear();
    act(() => void vi.advanceTimersByTime(PRESENCE_HEARTBEAT_MS));
    expect(send).not.toHaveBeenCalled();

    act(() => void window.dispatchEvent(new Event("pointerdown")));
    act(() => void vi.advanceTimersByTime(PRESENCE_HEARTBEAT_MS));
    expect(send).toHaveBeenCalled();
  });

  it("counts scrolling as interaction, not just clicking", () => {
    // Reading a long table is use. A viewer scrolling one is exactly the
    // person who would notice the data going stale.
    const send = vi.fn();
    render(<Harness send={send} />);
    act(() => void vi.advanceTimersByTime(PRESENCE_INTERACTION_WINDOW_MS + PRESENCE_HEARTBEAT_MS));
    send.mockClear();

    act(() => void window.dispatchEvent(new Event("scroll")));
    act(() => void vi.advanceTimersByTime(PRESENCE_HEARTBEAT_MS));
    expect(send).toHaveBeenCalled();
  });

  it("sends nothing at all when disabled", () => {
    const send = vi.fn();
    function Disabled() {
      usePresenceHeartbeat({ send, enabled: false });
      return null;
    }
    render(<Disabled />);
    act(() => void vi.advanceTimersByTime(PRESENCE_HEARTBEAT_MS * 3));
    expect(send).not.toHaveBeenCalled();
  });

  it("stops on unmount", () => {
    // A view the viewer navigated away from must release its runner. Leaking
    // the interval would keep reporting for a page nobody is looking at.
    const send = vi.fn();
    const { unmount } = render(<Harness send={send} />);
    unmount();
    send.mockClear();
    act(() => void vi.advanceTimersByTime(PRESENCE_HEARTBEAT_MS * 3));
    expect(send).not.toHaveBeenCalled();
  });
});
