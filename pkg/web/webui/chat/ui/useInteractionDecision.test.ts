import { afterEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { useInteractionDecision } from "./useInteractionDecision";

// Fabricated session names, never read from `examples/`.
const NS = "demo-ns";
const NAME = "demo-session";
const API = `/sessions/api/${NS}/${NAME}`;

// json builds a response in the shape the chat handlers really write (see
// ChatView.test.tsx's own `json` helper — the same contract this hook was
// lifted out of).
function json(v: unknown, status = 200): Response {
  return new Response(JSON.stringify(v), { status, headers: { "Content-Type": "application/json" } });
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("useInteractionDecision", () => {
  it("posts category/requestRef/actionId to the session's decision route and calls no error on success", async () => {
    const fetchMock = vi.fn(async () => json({ ok: true }));
    vi.stubGlobal("fetch", fetchMock);
    const onError = vi.fn();
    const { result } = renderHook(() => useInteractionDecision(NS, NAME, onError));

    await act(async () => {
      result.current("req-1", "plan_amendment", "approve");
    });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toBe(`${API}/decision`);
    expect(JSON.parse(String((init as RequestInit).body))).toEqual({
      category: "plan_amendment",
      requestRef: "req-1",
      actionId: "approve",
    });
    expect(onError).not.toHaveBeenCalled();
  });

  it("calls onError with the failure text on a non-OK response", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => json({ error: "boom" }, 500)));
    const onError = vi.fn();
    const { result } = renderHook(() => useInteractionDecision(NS, NAME, onError));

    await act(async () => {
      result.current("req-1", "plan_amendment", "approve");
    });

    expect(onError).toHaveBeenCalledWith("boom");
  });

  it("calls onError with a network-error line on a rejected fetch", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => { throw new Error("offline"); }));
    const onError = vi.fn();
    const { result } = renderHook(() => useInteractionDecision(NS, NAME, onError));

    await act(async () => {
      result.current("req-1", "plan_amendment", "approve");
    });

    expect(onError).toHaveBeenCalledWith("Network error while trying to submit decision.");
  });
});
