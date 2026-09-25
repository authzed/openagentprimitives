import "@testing-library/jest-dom/vitest";
import * as React from "react";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ChatFrame } from "../../chat/ui/types";

const NS = "ws-abc123456789";
const NAME = "demo-haiku-1a2b3c4d";
const API = `/sessions/api/${NS}/${NAME}`;

// The live socket is mocked the way SessionSocket.test.tsx and
// SessionShell.test.tsx mock it: useChatSocket reaches for a real WebSocket,
// which jsdom has none of, and ChatEmbed renders the REAL
// SessionSocketProvider/ChatView/StartupLine — nothing here stands in for
// those three.
vi.mock("../../chat/ui/useChatSocket", () => ({
  useChatSocket: (_ns: string, _name: string, _onFrame: (f: ChatFrame) => void) => ({ state: "connected" }),
}));

import { ChatEmbed } from "./ChatEmbed";

// stubFetch answers ChatView's mount-time reads (its detail load and its
// transcript replay), the same two endpoints ChatView.test.tsx's own
// stubFetch answers — anything else throws, so an address this component adds
// beyond the chat/startup pair would fail loudly rather than silently 404.
function stubFetch(): ReturnType<typeof vi.fn> {
  const spy = vi.fn(async (url: string) => {
    if (url === `${API}/messages`) return json({ timeline: [] });
    if (url === `${API}/detail`) {
      return json({
        sessionId: NAME,
        agentClass: "demo-agent",
        model: { provider: "anthropic", name: "demo-haiku" },
        maxTokens: 120000,
        maxTurns: 40,
        maxDuration: "30m0s",
        owner: "user:me",
        createdAt: "2026-01-01T00:00:00Z",
        phase: "Running",
      });
    }
    throw new Error("unexpected fetch: " + url);
  });
  vi.stubGlobal("fetch", spy);
  return spy;
}

function json(v: unknown, status = 200): Response {
  return new Response(JSON.stringify(v), { status, headers: { "Content-Type": "application/json" } });
}

afterEach(() => {
  vi.unstubAllGlobals();
  cleanup();
});

describe("ChatEmbed", () => {
  // The startup line is mounted but renders NOTHING until a session_startup
  // frame arrives (StartupLine returns null while signals.startup is null,
  // which is its initial value) — so "no startup line yet" is the correct
  // claim here, not "the startup line is shown".
  it("draws the chat and none of the shell's chrome, with no startup line until a signal arrives", () => {
    stubFetch();
    render(<ChatEmbed ns={NS} name={NAME} />);
    expect(screen.getByTestId("chat-embed-root")).toBeInTheDocument();
    expect(screen.queryByTestId("session-shell-startup")).toBeNull();
    expect(screen.queryByTestId("session-shell-tabs")).toBeNull();
    expect(screen.queryByText(/sessions/i, { selector: "nav *" })).toBeNull();
  });

  // The chat view underneath must address exactly the session it was given —
  // not a hardcoded default a coincidental match could hide. Rendering is not
  // evidence of that on its own: the READS it makes are, so the stub's calls
  // are what this asserts.
  it("addresses the chat view at exactly the given session, not a default", async () => {
    const fetchSpy = stubFetch();
    render(<ChatEmbed ns={NS} name={NAME} />);
    expect(screen.getByTestId("chat-view")).toBeInTheDocument();
    await waitFor(() => expect(fetchSpy).toHaveBeenCalled());
    const urls = fetchSpy.mock.calls.map((c) => c[0] as string);
    expect(urls).toContain(`${API}/messages`);
    for (const url of urls) {
      expect(url.startsWith(`${API}/`)).toBe(true);
    }
  });
});
