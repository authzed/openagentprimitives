import "@testing-library/jest-dom/vitest";
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import type { ChatFrame, ConnState } from "./types";

// Fabricated session names, never read from `examples/`.
const NS = "demo-ns";
const NAME = "demo-session";
const S = { namespace: NS, name: NAME };

// The socket itself is mocked (jsdom has no WebSocket worth driving), so a row
// can deliver frames directly and, separately, decide what connection state the
// provider reports — which is the input this file is actually about.
let capturedOnFrame: ((f: ChatFrame) => void) | null = null;
let mockConnState: ConnState = "connected";

vi.mock("./useChatSocket", () => ({
  useChatSocket: (_ns: string, _name: string, onFrame: (f: ChatFrame) => void) => {
    capturedOnFrame = onFrame;
    return { state: mockConnState };
  },
}));

import { SessionSocketProvider } from "./SessionSocket";
import { useSessionSignals } from "./sessionSignals";

afterEach(() => {
  cleanup();
  capturedOnFrame = null;
  mockConnState = "connected";
});

// Probe renders the two signals this file asserts on, so a row reads the fold
// through the hook — the wiring under test — rather than calling the reducer.
function Probe() {
  const s = useSessionSignals();
  return <span data-testid="signals">{`${s.turnActive}|${s.awaitingReply}|${s.lastAgentReply?.text ?? ""}`}</span>;
}

// LateProbe is a SECOND consumer, mounted after frames have already arrived —
// the agent-defined view opened for the first time on a session whose prompt
// landed while the viewer was on the chat tab.
function LateProbe() {
  const s = useSessionSignals();
  return <span data-testid="late">{s.pendingInteractions.length}</span>;
}

function tree() {
  return (
    <SessionSocketProvider ns={NS} name={NAME}>
      <Probe />
    </SessionSocketProvider>
  );
}

describe("useSessionSignals across a dropped connection", () => {
  // The socket has no replay: the turn_activity(active:false) that ended the
  // turn while the connection was away never arrives, and nothing else clears
  // turnActive — the default progress region would spin "Working…" for the rest
  // of the session.
  it("stops claiming the agent is working once the connection comes back", () => {
    const { rerender } = render(tree());
    act(() => capturedOnFrame!({ type: "turn_activity", session: S, payload: { session: S, active: true } }));
    expect(screen.getByTestId("signals")).toHaveTextContent("true|false|");

    // Dropped: still working as far as anyone here knows.
    mockConnState = "reconnecting";
    rerender(tree());
    expect(screen.getByTestId("signals")).toHaveTextContent("true|false|");

    // Back. Whatever was missed, the honest reading is "not working".
    mockConnState = "connected";
    rerender(tree());
    expect(screen.getByTestId("signals")).toHaveTextContent("false|false|");
  });

  // The other half of the same rule: a gap does not answer a question. An ask
  // the person never replied to is still outstanding, so the reconnect must not
  // close the reply fallback out from under them.
  it("leaves an outstanding ask outstanding", () => {
    const { rerender } = render(tree());
    act(() => {
      capturedOnFrame!({ type: "turn_activity", session: S, payload: { session: S, active: true } });
      capturedOnFrame!({ type: "user_message", session: S, payload: { session: S, text: "Which repository should it watch?" } });
      capturedOnFrame!({ type: "turn_activity", session: S, payload: { session: S, active: false, cause: "awaiting_reply" } });
    });
    expect(screen.getByTestId("signals")).toHaveTextContent("false|true|Which repository should it watch?");

    mockConnState = "offline";
    rerender(tree());
    mockConnState = "connected";
    rerender(tree());

    expect(screen.getByTestId("signals")).toHaveTextContent("false|true|Which repository should it watch?");
  });

  // "connected" is where a healthy socket sits. Resetting on the VALUE rather
  // than the transition would wipe the fold on the first render of every mount.
  it("does not reset when the connection was never lost", () => {
    const { rerender } = render(tree());
    act(() => capturedOnFrame!({ type: "turn_activity", session: S, payload: { session: S, active: true } }));
    rerender(tree());
    expect(screen.getByTestId("signals")).toHaveTextContent("true|false|");
  });
});

// The fold belongs to the PROVIDER, not to each consumer, and this is the row
// that says so. The socket has no per-subscriber replay: a frame delivered
// before a component existed reaches it never. With the fold per-consumer, an
// AgentUIView opened for the first time after the plan-amendment request
// arrived started from INITIAL_SESSION_SIGNALS and showed no "Needs your
// decision" region while the session sat in AwaitingDecision — until a
// reconnect or an inbound message the waiting viewer never sends.
describe("useSessionSignals for a consumer that mounts late", () => {
  it("hands a consumer mounted after the fact the state folded before it existed", () => {
    const { rerender } = render(
      <SessionSocketProvider ns={NS} name={NAME}>
        <Probe />
      </SessionSocketProvider>,
    );
    act(() =>
      capturedOnFrame!({
        type: "interaction_request",
        session: S,
        payload: {
          session: S,
          payload: {
            agentSessionRef: S,
            category: "plan_amendment",
            requestRef: "req-1",
            lead: "The agent is asking to add a step to its approved plan.",
            actions: [{ id: "approve", label: "Approve", kind: "decision" }],
            audience: { scope: "requester" },
          },
        },
      }),
    );

    rerender(
      <SessionSocketProvider ns={NS} name={NAME}>
        <Probe />
        <LateProbe />
      </SessionSocketProvider>,
    );
    expect(screen.getByTestId("late")).toHaveTextContent("1");
  });
});
