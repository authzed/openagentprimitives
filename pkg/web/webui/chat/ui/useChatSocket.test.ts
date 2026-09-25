import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { renderHook, act, cleanup } from "@testing-library/react";
import { parseChatFrame, wsURLFor, useChatSocket } from "./useChatSocket";

describe("parseChatFrame", () => {
  it("parses a valid user_message frame", () => {
    const f = parseChatFrame(
      JSON.stringify({ type: "user_message", session: { namespace: "n", name: "s" }, payload: { text: "hi" } }),
    );
    expect(f?.type).toBe("user_message");
  });
  it("returns null on malformed JSON", () => {
    expect(parseChatFrame("not json")).toBeNull();
  });
  it("returns null when type is missing", () => {
    expect(parseChatFrame(JSON.stringify({ session: { namespace: "n", name: "s" } }))).toBeNull();
  });
});

describe("wsURLFor", () => {
  it("uses wss for https and addresses the session by path", () => {
    expect(wsURLFor({ protocol: "https:", host: "x.example" }, "demo-ns", "demo-session")).toBe(
      "wss://x.example/sessions/api/demo-ns/demo-session/ws",
    );
  });
  it("uses ws for http", () => {
    expect(wsURLFor({ protocol: "http:", host: "localhost:8080" }, "demo-ns", "demo-session")).toBe(
      "ws://localhost:8080/sessions/api/demo-ns/demo-session/ws",
    );
  });
  // Each half is ONE path segment. A value carrying a slash must not be able to
  // introduce another, or the socket would address a session the route-level
  // Authorize gate never checked.
  it("encodes each half so neither can introduce a path segment", () => {
    expect(wsURLFor({ protocol: "http:", host: "localhost:8080" }, "ns a/b", "name c/d")).toBe(
      "ws://localhost:8080/sessions/api/ns%20a%2Fb/name%20c%2Fd/ws",
    );
  });
});

// --- useChatSocket reconnect state machine ---------------------------------
//
// The pure helpers above are exercised in isolation; the hook's resilience
// logic (connecting → reconnecting → offline, the ~2s backoff, the
// OFFLINE_AFTER threshold, onopen resetting the failure counter, and cleanup on
// unmount / session change) is only reachable through the hook itself.
// jsdom has no WebSocket, so we install a fake that records every instance and
// exposes its assigned event handlers, then drive open/error/close by hand
// under fake timers.

class FakeWebSocket {
  static instances: FakeWebSocket[] = [];
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  url: string;
  onopen: (() => void) | null = null;
  onmessage: ((ev: { data: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  onclose: (() => void) | null = null;
  close = vi.fn();
  constructor(url: string) {
    this.url = url;
    FakeWebSocket.instances.push(this);
  }
}

describe("useChatSocket reconnect state machine", () => {
  const noop = () => {};
  const latest = () => FakeWebSocket.instances[FakeWebSocket.instances.length - 1];
  // Every handler the hook wires up calls setState, so each must run inside act.
  const open = () => act(() => latest().onopen?.());
  const drop = () => act(() => latest().onclose?.()); // onclose === onDrop
  // onerror precedes onclose on a real drop; firing both must still schedule
  // exactly ONE retry (onclose owns the retry, onerror only reflects state).
  const errorThenClose = () =>
    act(() => {
      latest().onerror?.();
      latest().onclose?.();
    });
  const advance = (ms: number) => act(() => void vi.advanceTimersByTime(ms));

  beforeEach(() => {
    FakeWebSocket.instances = [];
    vi.stubGlobal("WebSocket", FakeWebSocket);
    vi.useFakeTimers();
  });
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  // A session this hook cannot address opens nothing. Reporting "connecting"
  // forever would look like a slow network; reporting offline AND logging says
  // which of the two it is, and points at the caller that passed nothing.
  // Both halves, because the guard is a disjunction: a namespace-only or
  // name-only address is equally unaddressable, and covering one half would
  // leave the other free to be dropped.
  it.each([
    ["", "s1"],
    ["demo-ns", ""],
  ])("an unaddressable session (ns=%o name=%o) opens no socket, reports offline, and logs the cause", (ns, name) => {
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    const { result } = renderHook(() => useChatSocket(ns, name, noop));
    expect(result.current.state).toBe("offline");
    expect(FakeWebSocket.instances).toHaveLength(0);
    expect(errorSpy).toHaveBeenCalledWith(
      "chat: no live socket opened; this view was given no session to address",
      { ns, name },
    );
  });

  it("opens one socket for the session and reports connected on open", () => {
    const { result } = renderHook(() => useChatSocket("demo-ns", "s1", noop));
    expect(FakeWebSocket.instances).toHaveLength(1);
    expect(latest().url).toContain("/sessions/api/demo-ns/s1/ws");
    expect(result.current.state).toBe("connecting");
    open();
    expect(result.current.state).toBe("connected");
  });

  it("a single drop shows reconnecting and retries exactly once after ~2s", () => {
    const { result } = renderHook(() => useChatSocket("demo-ns", "s1", noop));
    open();
    expect(result.current.state).toBe("connected");

    errorThenClose();
    expect(result.current.state).toBe("reconnecting");

    // Nothing reconnects before the 2s backoff elapses.
    advance(1999);
    expect(FakeWebSocket.instances).toHaveLength(1);

    // At 2s exactly ONE new socket opens — the error+close pair did not
    // double-schedule.
    advance(1);
    expect(FakeWebSocket.instances).toHaveLength(2);
    expect(result.current.state).toBe("reconnecting");
  });

  it("goes offline after OFFLINE_AFTER consecutive drops and onopen resets the counter", () => {
    const { result } = renderHook(() => useChatSocket("demo-ns", "s1", noop));
    open();

    drop();
    expect(result.current.state).toBe("reconnecting"); // failure 1
    advance(2000);
    drop();
    expect(result.current.state).toBe("reconnecting"); // failure 2
    advance(2000);
    drop();
    expect(result.current.state).toBe("offline"); // failure 3 == OFFLINE_AFTER
    advance(2000);

    // initial socket + one retry per drop.
    expect(FakeWebSocket.instances).toHaveLength(4);

    // A successful open clears the failure counter...
    open();
    expect(result.current.state).toBe("connected");
    // ...so the very next drop reads as a fresh "reconnecting", not "offline".
    drop();
    expect(result.current.state).toBe("reconnecting");
  });

  it("unmount closes the socket and cancels the pending reconnect", () => {
    const { unmount } = renderHook(() => useChatSocket("demo-ns", "s1", noop));
    open();
    drop(); // schedules a 2s retry
    const socket = latest();

    unmount();
    expect(socket.close).toHaveBeenCalled();

    // The pending reconnect was cancelled on cleanup — time passing opens
    // nothing new.
    advance(5000);
    expect(FakeWebSocket.instances).toHaveLength(1);
  });

  it("changing the session tears down the old socket and connects a fresh one", () => {
    const { result, rerender } = renderHook(({ name }) => useChatSocket("demo-ns", name, noop), {
      initialProps: { name: "s1" },
    });
    open();
    expect(result.current.state).toBe("connected");
    const first = FakeWebSocket.instances[0];

    rerender({ name: "s2" });
    expect(first.close).toHaveBeenCalled();
    expect(FakeWebSocket.instances).toHaveLength(2);
    expect(latest().url).toContain("/sessions/api/demo-ns/s2/ws");
    expect(result.current.state).toBe("connecting"); // fresh effect, counter reset
  });
});
