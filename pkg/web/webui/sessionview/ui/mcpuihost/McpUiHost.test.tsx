import * as React from "react";
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, cleanup } from "@testing-library/react";

import { McpUiHost, logUIAction, sendAppToolCall, routeUIAction, type CallToolResultLike } from "./McpUiHost";
import type { UIActionResultToolCall } from "@mcp-ui/client";

afterEach(() => {
  cleanup();
});

describe("McpUiHost – renders a UIResource via @mcp-ui/client", () => {
  it("mounts an iframe carrying the widget's HTML with sandbox=allow-scripts and NEVER allow-same-origin", () => {
    const { container } = render(
      <McpUiHost uri="ui://widget/w1" html="<p>hello widget</p><script>doStuff()</script>" trustedOrigin="https://shell.example" />,
    );
    const iframe = container.querySelector("iframe");
    expect(iframe).toBeTruthy();
    // The security-critical assertion for this task: @mcp-ui/client's srcDoc
    // render path (chosen because no `proxy` is passed and the resource's
    // mimeType is text/html) must produce EXACTLY "allow-scripts" — its other
    // ("src") mode unconditionally adds allow-same-origin, which this
    // sandbox-origin isolation model forbids (see McpUiHost.tsx's doc).
    expect(iframe!.getAttribute("sandbox")).toBe("allow-scripts");
    expect(iframe!.getAttribute("sandbox")).not.toContain("allow-same-origin");
    // srcDoc mode: the widget's HTML travels as the iframe's srcdoc, not a
    // src= URL — proves it actually rendered OUR resource, not a placeholder.
    expect(iframe!.getAttribute("srcdoc") ?? "").toContain("hello widget");
    expect(iframe!.getAttribute("srcdoc") ?? "").toContain("doStuff()");
    // Never point this iframe at a URL — that would only happen in the
    // library's unsafe "src" mode.
    expect(iframe!.getAttribute("src")).toBeNull();
  });

  it("gives the iframe an accessible title", () => {
    const { container } = render(<McpUiHost uri="ui://widget/w1" html="<p>hi</p>" title="My widget" trustedOrigin="https://shell.example" />);
    const iframe = container.querySelector("iframe");
    expect(iframe!.getAttribute("title")).toBe("My widget");
  });
});

describe("McpUiHost – onUIAction (logUIAction) relays UP to the trusted shell via postMessage", () => {
  it("logs the action, posts {ap:'mcpui',cmd:'action',result} to window.parent using the TRUSTED origin (never '*'), and resolves without fetching", async () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
    const postSpy = vi.spyOn(window.parent, "postMessage").mockImplementation(() => {});
    const logSpy = vi.spyOn(console, "log").mockImplementation(() => {});

    const result = { type: "tool", payload: { toolName: "example_tool", params: { x: 1 } } } as const;
    await expect(logUIAction(result, "https://shell.example")).resolves.toBeUndefined();

    expect(logSpy).toHaveBeenCalledTimes(1);
    // NOTHING about handling a UI action may reach the network directly — the
    // trusted shell is the only party that fetches /interact, after
    // re-validating this exact message itself (SessionView.tsx).
    expect(fetchSpy).not.toHaveBeenCalled();

    expect(postSpy).toHaveBeenCalledTimes(1);
    const [msg, targetOrigin] = postSpy.mock.calls[0]!;
    expect(msg).toEqual({ ap: "mcpui", cmd: "action", result });
    expect(targetOrigin).toBe("https://shell.example");
    expect(targetOrigin).not.toBe("*");

    postSpy.mockRestore();
    logSpy.mockRestore();
    vi.unstubAllGlobals();
  });

  it("never calls fetch, and always posts using the trusted origin (never '*'), for every UIActionResult variant (tool/prompt/link/intent/notify)", async () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
    const postSpy = vi.spyOn(window.parent, "postMessage").mockImplementation(() => {});
    const logSpy = vi.spyOn(console, "log").mockImplementation(() => {});

    await logUIAction({ type: "prompt", payload: { prompt: "do a thing" } }, "https://shell.example");
    await logUIAction({ type: "link", payload: { url: "https://example.com" } }, "https://shell.example");
    await logUIAction({ type: "intent", payload: { intent: "open_settings", params: {} } }, "https://shell.example");
    await logUIAction({ type: "notify", payload: { message: "hi" } }, "https://shell.example");

    expect(fetchSpy).not.toHaveBeenCalled();
    expect(logSpy).toHaveBeenCalledTimes(4);
    expect(postSpy).toHaveBeenCalledTimes(4);
    for (const call of postSpy.mock.calls) {
      expect(call[1]).toBe("https://shell.example");
      expect(call[1]).not.toBe("*");
    }

    postSpy.mockRestore();
    logSpy.mockRestore();
    vi.unstubAllGlobals();
  });
});

// dispatchAppToolCallReply simulates a reply arriving on window from the
// trusted shell (or an impostor) — the exact shape SessionView.tsx's Task 3
// listener is expected to post back down.
function dispatchAppToolCallReply(data: unknown, origin: string, source: Window | null) {
  window.dispatchEvent(new MessageEvent("message", { data, origin, source }));
}

const toolResult: UIActionResultToolCall = { type: "tool", payload: { toolName: "get_status", params: { id: 7 } } };

describe("McpUiHost – sendAppToolCall: correlated round-trip for `tool` UIActions", () => {
  let postSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    postSpy = vi.spyOn(window.parent, "postMessage").mockImplementation(() => {});
  });

  afterEach(() => {
    postSpy.mockRestore();
    vi.useRealTimers();
  });

  it("posts {ap:'mcpui',cmd:'appToolCall',corrId,result} to window.parent using the trusted origin, and resolves with the reply's response once a matching appToolCallReply arrives", async () => {
    const pending = sendAppToolCall(toolResult, "https://shell.example");

    expect(postSpy).toHaveBeenCalledTimes(1);
    const [msg, targetOrigin] = postSpy.mock.calls[0]!;
    expect(targetOrigin).toBe("https://shell.example");
    expect(msg).toMatchObject({ ap: "mcpui", cmd: "appToolCall", result: toolResult });
    const corrId = (msg as { corrId: string }).corrId;
    expect(typeof corrId).toBe("string");
    expect(corrId.length).toBeGreaterThan(0);

    const response: CallToolResultLike = { content: [{ type: "text", text: "status: ok" }], isError: false };
    dispatchAppToolCallReply({ ap: "mcpui", cmd: "appToolCallReply", corrId, response }, "https://shell.example", window.parent);

    await expect(pending).resolves.toEqual(response);
  });

  it("confused-deputy defense: a reply with the wrong origin, wrong source, or a non-matching corrId does NOT resolve; only the correct reply does", async () => {
    const pending = sendAppToolCall(toolResult, "https://shell.example");
    const corrId = (postSpy.mock.calls[0]![0] as { corrId: string }).corrId;
    const response: CallToolResultLike = { content: [{ type: "text", text: "ok" }], isError: false };
    const impostorSource = {} as Window;

    let resolvedWith: CallToolResultLike | undefined;
    pending.then((r) => {
      resolvedWith = r;
    });

    dispatchAppToolCallReply({ ap: "mcpui", cmd: "appToolCallReply", corrId, response }, "https://evil.example", window.parent);
    await Promise.resolve();
    expect(resolvedWith).toBeUndefined();

    dispatchAppToolCallReply({ ap: "mcpui", cmd: "appToolCallReply", corrId, response }, "https://shell.example", impostorSource);
    await Promise.resolve();
    expect(resolvedWith).toBeUndefined();

    dispatchAppToolCallReply({ ap: "mcpui", cmd: "appToolCallReply", corrId: "not-the-right-id", response }, "https://shell.example", window.parent);
    await Promise.resolve();
    expect(resolvedWith).toBeUndefined();

    // only the correctly-addressed, correctly-tagged, correctly-corrId'd reply resolves it
    dispatchAppToolCallReply({ ap: "mcpui", cmd: "appToolCallReply", corrId, response }, "https://shell.example", window.parent);
    await Promise.resolve();
    expect(resolvedWith).toEqual(response);
  });

  it("resolves an isError malformed-response result when the reply's `response` field doesn't match CallToolResultLike", async () => {
    const pending = sendAppToolCall(toolResult, "https://shell.example");
    const corrId = (postSpy.mock.calls[0]![0] as { corrId: string }).corrId;

    dispatchAppToolCallReply({ ap: "mcpui", cmd: "appToolCallReply", corrId, response: { garbage: true } }, "https://shell.example", window.parent);

    const result = await pending;
    expect(result.isError).toBe(true);
  });

  it("times out after the client-side budget and resolves an isError 'timed out' result so the widget never hangs", async () => {
    vi.useFakeTimers();

    const pending = sendAppToolCall(toolResult, "https://shell.example");
    let resolvedWith: CallToolResultLike | undefined;
    pending.then((r) => {
      resolvedWith = r;
    });

    await vi.advanceTimersByTimeAsync(35_000);

    expect(resolvedWith).toEqual({ content: [{ type: "text", text: "The action timed out." }], isError: true });
  });
});

describe("McpUiHost – routeUIAction: onUIAction's branch between the correlated and fire-and-forget paths", () => {
  it("routes a `tool` action through sendAppToolCall (posts cmd:'appToolCall', not cmd:'action')", async () => {
    const postSpy = vi.spyOn(window.parent, "postMessage").mockImplementation(() => {});

    const pending = routeUIAction(toolResult, "https://shell.example");

    expect(postSpy).toHaveBeenCalledTimes(1);
    const posted = postSpy.mock.calls[0]![0] as { cmd: string; corrId: string };
    expect(posted.cmd).toBe("appToolCall");

    // Resolve it so this test doesn't leave a dangling pending correlation
    // for later tests in this file.
    dispatchAppToolCallReply(
      { ap: "mcpui", cmd: "appToolCallReply", corrId: posted.corrId, response: { content: [], isError: false } },
      "https://shell.example",
      window.parent,
    );
    await pending;

    postSpy.mockRestore();
  });

  it("routes a non-tool action (prompt) through the fire-and-forget logUIAction path (posts cmd:'action', resolves void)", async () => {
    const postSpy = vi.spyOn(window.parent, "postMessage").mockImplementation(() => {});
    const logSpy = vi.spyOn(console, "log").mockImplementation(() => {});

    const result = await routeUIAction({ type: "prompt", payload: { prompt: "do a thing" } }, "https://shell.example");

    expect(result).toBeUndefined();
    expect(postSpy).toHaveBeenCalledTimes(1);
    expect(postSpy.mock.calls[0]![0]).toEqual({
      ap: "mcpui",
      cmd: "action",
      result: { type: "prompt", payload: { prompt: "do a thing" } },
    });

    postSpy.mockRestore();
    logSpy.mockRestore();
  });
});
