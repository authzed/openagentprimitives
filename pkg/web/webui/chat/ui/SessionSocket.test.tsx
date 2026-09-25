import "@testing-library/jest-dom/vitest";
import * as React from "react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import type { ChatFrame, ConnState } from "./types";

// NS/NAME are the session the provider is told to hold a socket for.
// Fabricated names, never read from `examples/`.
const NS = "demo-ns";
const NAME = "demo-session";

// useChatSocket reaches for a real WebSocket; mock it so tests stay in jsdom
// and can drive frames directly — the same capture ChatView.test.tsx uses.
// capturedSocket is how a row asserts WHICH session the one socket was opened
// for, and that a subscriber coming or going did not re-open it.
let capturedOnFrame: ((f: ChatFrame) => void) | null = null;
let capturedSocket: { ns: string; name: string } | null = null;
let mockConnState: ConnState = "connected";

vi.mock("./useChatSocket", () => ({
  useChatSocket: (ns: string, name: string, onFrame: (f: ChatFrame) => void) => {
    capturedSocket = { ns, name };
    capturedOnFrame = onFrame;
    return { state: mockConnState };
  },
}));

import { SessionSocketProvider, useSessionConnState, useSessionFrames } from "./SessionSocket";

afterEach(() => {
  cleanup();
  capturedOnFrame = null;
  capturedSocket = null;
  mockConnState = "connected";
  vi.restoreAllMocks();
});

const S = { namespace: NS, name: NAME };

const activityFrame = (): ChatFrame => ({
  type: "turn_activity",
  session: S,
  payload: { session: S, active: true },
});

const progressFrame = (): ChatFrame => ({
  type: "turn_progress",
  session: S,
  payload: { session: S, payload: { inputTokens: 1, outputTokens: 1, elapsedSeconds: 1, seq: 1 } },
});

describe("SessionSocketProvider", () => {
  it("opens ONE socket for the session and fans every frame to every subscriber", () => {
    const seenA: string[] = [];
    const seenB: string[] = [];
    function A() {
      useSessionFrames((f) => seenA.push(f.type));
      return null;
    }
    function B() {
      useSessionFrames((f) => seenB.push(f.type));
      return null;
    }
    render(
      <SessionSocketProvider ns={NS} name={NAME}>
        <A />
        <B />
      </SessionSocketProvider>,
    );

    expect(capturedSocket).toEqual({ ns: NS, name: NAME });

    act(() => capturedOnFrame!(activityFrame()));

    expect(seenA).toEqual(["turn_activity"]);
    expect(seenB).toEqual(["turn_activity"]);
  });

  // The socket outliving a subscriber is the whole reason it is the shell's:
  // a view that goes away — the transcript, while the viewer reads the
  // agent-defined page — must stop receiving without taking the session's
  // only live channel with it.
  it("a subscriber that unmounts stops receiving; the socket stays open", () => {
    const seenA: string[] = [];
    const seenB: string[] = [];
    function A() {
      useSessionFrames((f) => seenA.push(f.type));
      return null;
    }
    function B() {
      useSessionFrames((f) => seenB.push(f.type));
      return null;
    }
    function Host() {
      const [withB, setWithB] = useState(true);
      return (
        <>
          <A />
          {withB && <B />}
          <button onClick={() => setWithB(false)}>drop</button>
        </>
      );
    }
    render(
      <SessionSocketProvider ns={NS} name={NAME}>
        <Host />
      </SessionSocketProvider>,
    );
    const beforeFan = capturedOnFrame;

    fireEvent.click(screen.getByRole("button", { name: "drop" }));
    act(() => capturedOnFrame!(progressFrame()));

    expect(seenA).toEqual(["turn_progress"]);
    expect(seenB).toEqual([]);
    // Still the same session, and still the same onFrame: the provider's `fan`
    // holds its identity across re-renders, which is what the real
    // useChatSocket's connect effect would have to see change (alongside
    // ns/name) before it tore a connection down. Asserted on the ARGUMENTS
    // rather than on the captured object's identity, because the provider now
    // folds frames itself and so re-renders — re-invoking the hook, which
    // opens nothing.
    expect(capturedSocket).toEqual({ ns: NS, name: NAME });
    expect(capturedOnFrame).toBe(beforeFan);
  });

  it("exposes the connection state and reports offline outside a provider", () => {
    function Probe() {
      return <span data-testid="s">{useSessionConnState()}</span>;
    }
    mockConnState = "reconnecting";
    render(
      <SessionSocketProvider ns={NS} name={NAME}>
        <Probe />
      </SessionSocketProvider>,
    );
    expect(screen.getByTestId("s")).toHaveTextContent("reconnecting");

    cleanup();

    render(<Probe />);
    expect(screen.getByTestId("s")).toHaveTextContent("offline");
  });

  // A subscriber mounted outside a provider would otherwise sit forever on a
  // socket nobody opened, looking exactly like a session that has gone quiet.
  // It says so instead — the no-silent-errors rule, in its browser form.
  it("says so when a subscriber is mounted outside a provider, rather than delivering nothing in silence", () => {
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    function Orphan() {
      useSessionFrames(() => {});
      return null;
    }

    render(<Orphan />);

    expect(errorSpy).toHaveBeenCalledWith(
      "chat: useSessionFrames outside a SessionSocketProvider; no frames will arrive",
    );
  });
});
