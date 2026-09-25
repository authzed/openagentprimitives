import * as React from "react";
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, act, fireEvent } from "@testing-library/react";

import { SessionView, type SessionViewProps } from "./SessionView";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

// SessionView opens a real WebSocket (its own local useLiveSocket, not a
// separately-mockable module) — jsdom has no WebSocket global, so `new
// WebSocket(...)` throws synchronously and is caught (see useLiveSocket's
// try/catch -> onDrop), leaving the socket "reconnecting" forever. That never
// touches the postMessage bridge under test here, which is a fully separate
// `window.addEventListener("message", ...)` effect.
const baseProps: SessionViewProps = {
  ns: "default",
  name: "sess-1",
  sandboxOrigin: "https://sandbox.example",
  activeWidgets: [{ artifactId: "w1", hostUrl: "https://sandbox.example/mcpui-host?ct=tok-w1" }],
};

// dispatchWidgetMessage simulates a message arriving from a widget's outer
// /mcpui-host iframe (or an impostor) — the exact postMessage shape
// McpUiHost.tsx's logUIAction sends up.
function dispatchWidgetMessage(data: unknown, origin: string, source: Window | null) {
  window.dispatchEvent(new MessageEvent("message", { data, origin, source }));
}

function mountedIframe(container: HTMLElement): HTMLIFrameElement {
  const iframe = container.querySelector("iframe");
  if (!iframe) throw new Error("expected a mounted widget iframe");
  return iframe as HTMLIFrameElement;
}

describe("SessionView – widget action bridge: valid origin+source+tag -> POST /interact", () => {
  it("POSTs mcp_ui_action to /session/{ns}/{name}/interact, flattened, with no artifactId", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({}) });
    vi.stubGlobal("fetch", fetchMock);

    const { container } = render(<SessionView {...baseProps} />);
    const iframe = mountedIframe(container);

    await act(async () => {
      dispatchWidgetMessage(
        { ap: "mcpui", cmd: "action", result: { type: "tool", payload: { toolName: "refresh_data", params: { id: 1 } } } },
        "https://sandbox.example",
        iframe.contentWindow,
      );
      await Promise.resolve();
    });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, opts] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/session/default/sess-1/interact");
    expect(opts.method).toBe("POST");
    expect(opts.credentials).toBe("same-origin");
    const body = JSON.parse(opts.body as string);
    expect(body).toEqual({
      kind: "mcp_ui_action",
      payload: { type: "tool", toolName: "refresh_data", params: { id: 1 } },
    });
    expect(body.artifactId).toBeUndefined();
  });

  it("surfaces a non-2xx response visibly rather than swallowing it", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: false, status: 403, json: async () => ({ error: "not permitted" }) });
    vi.stubGlobal("fetch", fetchMock);

    const { container, findByText } = render(<SessionView {...baseProps} />);
    const iframe = mountedIframe(container);

    await act(async () => {
      dispatchWidgetMessage(
        { ap: "mcpui", cmd: "action", result: { type: "notify", payload: { message: "hi" } } },
        "https://sandbox.example",
        iframe.contentWindow,
      );
      await Promise.resolve();
    });

    expect(await findByText("not permitted")).toBeTruthy();
  });
});

describe("SessionView – widget action bridge: security assertions have teeth (NO fetch on failure)", () => {
  it("does NOT fetch when the message origin is not this session's sandbox origin", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const { container } = render(<SessionView {...baseProps} />);
    const iframe = mountedIframe(container);

    act(() => {
      dispatchWidgetMessage(
        { ap: "mcpui", cmd: "action", result: { type: "notify", payload: { message: "hi" } } },
        "https://evil.example",
        iframe.contentWindow,
      );
    });

    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("does NOT fetch when the message source is not a currently-mounted widget iframe", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    render(<SessionView {...baseProps} />);

    act(() => {
      // The correct origin and tag, but the source is the top window itself
      // (or any other frame) — never a mounted widget's contentWindow.
      dispatchWidgetMessage(
        { ap: "mcpui", cmd: "action", result: { type: "notify", payload: { message: "hi" } } },
        "https://sandbox.example",
        window,
      );
    });

    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("does NOT fetch when the message tag isn't exactly {ap:'mcpui', cmd:'action'} with a result", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const { container } = render(<SessionView {...baseProps} />);
    const iframe = mountedIframe(container);

    act(() => {
      dispatchWidgetMessage({ ap: "other", cmd: "action", result: { type: "notify", payload: {} } }, "https://sandbox.example", iframe.contentWindow);
      dispatchWidgetMessage({ ap: "mcpui", cmd: "other", result: { type: "notify", payload: {} } }, "https://sandbox.example", iframe.contentWindow);
      dispatchWidgetMessage({ ap: "mcpui", cmd: "action" }, "https://sandbox.example", iframe.contentWindow);
      dispatchWidgetMessage({ ap: "annot", cmd: "send", bundle: {} }, "https://sandbox.example", iframe.contentWindow);
    });

    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("does NOT fetch once a widget's frame unmounts (its source is no longer in the mounted map)", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const { container, unmount } = render(<SessionView {...baseProps} />);
    const iframe = mountedIframe(container);
    const source = iframe.contentWindow;

    unmount();

    act(() => {
      dispatchWidgetMessage({ ap: "mcpui", cmd: "action", result: { type: "notify", payload: { message: "hi" } } }, "https://sandbox.example", source);
    });

    expect(fetchMock).not.toHaveBeenCalled();
  });
});

// jsonResponse builds a minimal fetch Response stub for these tests — only
// the members the listener under test actually calls (ok, status, json()).
function jsonResponse(status: number, body: unknown): { ok: boolean; status: number; json: () => Promise<unknown> } {
  return { ok: status >= 200 && status < 300, status, json: async () => body };
}

describe("SessionView – appToolCall bridge: correlated /app-tool-call round-trip + response mapping + reply-down", () => {
  it("POSTs {toolName,args} from d.result.payload to /app-tool-call, then replies appToolCallReply with the mapped ok result", async () => {
    const fetchMock = vi.fn().mockImplementation((url: string) => {
      if (url === "/session/default/sess-1/app-tool-call") {
        return Promise.resolve(jsonResponse(200, { status: "ok", result: "42 widgets refreshed", isError: false }));
      }
      throw new Error(`unexpected fetch: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { container } = render(<SessionView {...baseProps} />);
    const iframe = mountedIframe(container);
    const contentWindow = iframe.contentWindow as Window;
    const postMessageSpy = vi.spyOn(contentWindow, "postMessage");

    await act(async () => {
      dispatchWidgetMessage(
        { ap: "mcpui", cmd: "appToolCall", corrId: "corr-1", result: { type: "tool", payload: { toolName: "refresh_data", params: { id: 7 } } } },
        "https://sandbox.example",
        contentWindow,
      );
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, opts] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/session/default/sess-1/app-tool-call");
    expect(opts.method).toBe("POST");
    expect(opts.credentials).toBe("same-origin");
    // artifactId names the CALLING widget, so the server can resolve which MCP
    // server authored it and refuse a call reaching for another server's tool.
    expect(JSON.parse(opts.body as string)).toEqual({ toolName: "refresh_data", args: { id: 7 }, artifactId: "w1" });

    expect(postMessageSpy).toHaveBeenCalledTimes(1);
    const [reply, targetOrigin] = postMessageSpy.mock.calls[0] as [unknown, string];
    expect(targetOrigin).toBe("https://sandbox.example");
    expect(reply).toEqual({
      ap: "mcpui",
      cmd: "appToolCallReply",
      corrId: "corr-1",
      response: { content: [{ type: "text", text: "42 widgets refreshed" }], isError: false },
    });
  });

  it("gate failure (non-2xx from the FIRST /app-tool-call fetch, e.g. an auth/CSRF/malformed-body rejection): surfaces setSendError AND still replies down an isError result (no hang)", async () => {
    const fetchMock = vi.fn().mockImplementation((url: string) => {
      if (url === "/session/default/sess-1/app-tool-call") {
        return Promise.resolve(jsonResponse(403, { error: "nope" }));
      }
      throw new Error(`unexpected fetch: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { container, findByText } = render(<SessionView {...baseProps} />);
    const iframe = mountedIframe(container);
    const contentWindow = iframe.contentWindow as Window;
    const postMessageSpy = vi.spyOn(contentWindow, "postMessage");

    await act(async () => {
      dispatchWidgetMessage(
        { ap: "mcpui", cmd: "appToolCall", corrId: "corr-5", result: { type: "tool", payload: { toolName: "refresh_data", params: {} } } },
        "https://sandbox.example",
        contentWindow,
      );
      await Promise.resolve();
      await Promise.resolve();
    });

    // Only the gate-failing fetch fires — this is NOT the not_found path, so
    // there must be no /interact fallback call.
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(await findByText("nope")).toBeTruthy();

    expect(postMessageSpy).toHaveBeenCalledTimes(1);
    const [reply] = postMessageSpy.mock.calls[0] as [unknown, string];
    expect(reply).toEqual({
      ap: "mcpui",
      cmd: "appToolCallReply",
      corrId: "corr-5",
      response: { content: [{ type: "text", text: "The action could not be sent." }], isError: true },
    });
  });

  it("not_found: falls back to POST /interact, then replies down a benign ack (widget never hangs)", async () => {
    const fetchMock = vi.fn().mockImplementation((url: string) => {
      if (url === "/session/default/sess-1/app-tool-call") {
        return Promise.resolve(jsonResponse(200, { status: "not_found" }));
      }
      if (url === "/session/default/sess-1/interact") {
        return Promise.resolve(jsonResponse(200, {}));
      }
      throw new Error(`unexpected fetch: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { container } = render(<SessionView {...baseProps} />);
    const iframe = mountedIframe(container);
    const contentWindow = iframe.contentWindow as Window;
    const postMessageSpy = vi.spyOn(contentWindow, "postMessage");

    await act(async () => {
      dispatchWidgetMessage(
        { ap: "mcpui", cmd: "appToolCall", corrId: "corr-2", result: { type: "tool", payload: { toolName: "unknown_tool", params: {} } } },
        "https://sandbox.example",
        contentWindow,
      );
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(fetchMock).toHaveBeenCalledTimes(2);
    const urls = fetchMock.mock.calls.map((c) => c[0]);
    expect(urls).toContain("/session/default/sess-1/app-tool-call");
    expect(urls).toContain("/session/default/sess-1/interact");
    const interactCall = fetchMock.mock.calls.find((c) => c[0] === "/session/default/sess-1/interact") as [string, RequestInit];
    expect(JSON.parse(interactCall[1].body as string)).toEqual({
      kind: "mcp_ui_action",
      payload: { type: "tool", toolName: "unknown_tool", params: {} },
    });

    expect(postMessageSpy).toHaveBeenCalledTimes(1);
    const [reply] = postMessageSpy.mock.calls[0] as [unknown, string];
    expect(reply).toEqual({
      ap: "mcpui",
      cmd: "appToolCallReply",
      corrId: "corr-2",
      response: { content: [{ type: "text", text: "Sent to the agent." }], isError: false },
    });
  });

  it("network failure: surfaces setSendError AND still replies down an isError result (no hang)", async () => {
    const fetchMock = vi.fn().mockRejectedValue(new Error("network down"));
    vi.stubGlobal("fetch", fetchMock);

    const { container, findByText } = render(<SessionView {...baseProps} />);
    const iframe = mountedIframe(container);
    const contentWindow = iframe.contentWindow as Window;
    const postMessageSpy = vi.spyOn(contentWindow, "postMessage");

    await act(async () => {
      dispatchWidgetMessage(
        { ap: "mcpui", cmd: "appToolCall", corrId: "corr-3", result: { type: "tool", payload: { toolName: "refresh_data", params: {} } } },
        "https://sandbox.example",
        contentWindow,
      );
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(await findByText("The server was unreachable while sending the widget action.")).toBeTruthy();

    expect(postMessageSpy).toHaveBeenCalledTimes(1);
    const [reply] = postMessageSpy.mock.calls[0] as [{ response?: { isError?: boolean } }, string];
    expect(reply.response?.isError).toBe(true);
  });

  it("a non-tool appToolCall-tagged message is ignored (result.type must be 'tool')", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const { container } = render(<SessionView {...baseProps} />);
    const iframe = mountedIframe(container);

    act(() => {
      dispatchWidgetMessage(
        { ap: "mcpui", cmd: "appToolCall", corrId: "corr-4", result: { type: "notify", payload: { message: "hi" } } },
        "https://sandbox.example",
        iframe.contentWindow,
      );
    });

    expect(fetchMock).not.toHaveBeenCalled();
  });

  // This page frames SEVERAL widgets at once, each authored by a different MCP
  // server, which is exactly why the source match has to say WHICH one sent the
  // message rather than only that one did. Sending the wrong artifactId would
  // pin the call to the wrong server — a check that runs and answers about
  // somebody else.
  it("sends the artifactId of the widget that actually sent the message, not the first mounted one", async () => {
    const fetchMock = vi.fn().mockImplementation((url: string) => {
      if (url === "/session/default/sess-1/app-tool-call") {
        return Promise.resolve(jsonResponse(200, { status: "ok", result: "done", isError: false }));
      }
      throw new Error(`unexpected fetch: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { container } = render(
      <SessionView
        {...baseProps}
        activeWidgets={[
          { artifactId: "w1", hostUrl: "https://sandbox.example/mcpui-host?ct=tok-w1" },
          { artifactId: "w2", hostUrl: "https://sandbox.example/mcpui-host?ct=tok-w2" },
        ]}
      />,
    );
    const frames = Array.from(container.querySelectorAll("iframe")) as HTMLIFrameElement[];
    expect(frames.length).toBe(2);
    const second = frames[1].contentWindow as Window;

    await act(async () => {
      dispatchWidgetMessage(
        { ap: "mcpui", cmd: "appToolCall", corrId: "corr-6", result: { type: "tool", payload: { toolName: "refresh_data", params: {} } } },
        "https://sandbox.example",
        second,
      );
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [, opts] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(JSON.parse(opts.body as string).artifactId).toBe("w2");
  });
});

describe("SessionView – a server notice on a 200 response", () => {
  // sampleNotice is the wire shape interactResponse.Notice
  // (pkg/web/webui/interact/handlers.go) carries: channelevents.NoticeWire, built
  // from the same pipeline decision the chat path returns. Modelled on
  // refusalNotice — a trusted lead/body, an untrusted excerpt reason, and the
  // nextStep this tone requires.
  const sampleNotice = {
    category: "session_ended",
    tone: "critical",
    terminal: true,
    lead: "This session has ended and can't continue",
    body: "The agent hit an unrecoverable failure.",
    nextStep: "Start a new thread to begin again.",
    excerpt: { label: "Reason", content: "the runner pod was evicted" },
  };

  function typeAndSend(text: string) {
    const ta = document.querySelector("textarea") as HTMLTextAreaElement;
    if (!ta) throw new Error("expected the composer textarea");
    fireEvent.change(ta, { target: { value: text } });
    fireEvent.click(document.querySelector('button[title="Send"]') as HTMLButtonElement);
  }

  it("a composer send answered 200-with-a-notice shows it, rather than falling silent", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { outcome: "refused", notice: sampleNotice }));
    vi.stubGlobal("fetch", fetchMock);

    const { findByText } = render(<SessionView {...baseProps} />);
    await act(async () => {
      typeAndSend("are you there?");
      await Promise.resolve();
    });

    expect(await findByText(sampleNotice.lead)).toBeTruthy();
    expect(await findByText(sampleNotice.nextStep)).toBeTruthy();
  });

  it("a widget action answered 200-with-a-notice shows it too", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { outcome: "refused", notice: sampleNotice }));
    vi.stubGlobal("fetch", fetchMock);

    const { container, findByText } = render(<SessionView {...baseProps} />);
    const iframe = mountedIframe(container);

    await act(async () => {
      dispatchWidgetMessage(
        { ap: "mcpui", cmd: "action", result: { type: "notify", payload: { message: "hi" } } },
        "https://sandbox.example",
        iframe.contentWindow,
      );
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(await findByText(sampleNotice.lead)).toBeTruthy();
  });

  it("a clean 200 with no notice shows no banner", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { outcome: "routed" }));
    vi.stubGlobal("fetch", fetchMock);

    const { queryByTestId } = render(<SessionView {...baseProps} />);
    await act(async () => {
      typeAndSend("hello");
      await Promise.resolve();
    });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(queryByTestId("notice-banner")).toBeNull();
  });

  it("a later send clears the previous send's notice — the banner shows the LATEST outcome only", async () => {
    let calls = 0;
    const fetchMock = vi.fn().mockImplementation(() => {
      calls += 1;
      return Promise.resolve(
        calls === 1 ? jsonResponse(200, { outcome: "refused", notice: sampleNotice }) : jsonResponse(200, { outcome: "routed" }),
      );
    });
    vi.stubGlobal("fetch", fetchMock);

    const { findByTestId, queryByTestId } = render(<SessionView {...baseProps} />);
    await act(async () => {
      typeAndSend("first");
      await Promise.resolve();
    });
    expect(await findByTestId("notice-banner")).toBeTruthy();

    await act(async () => {
      typeAndSend("second");
      await Promise.resolve();
    });
    // A stale notice beside a fresh, accepted send would misreport what just
    // happened — the second send routed cleanly, so the slot must be empty.
    expect(queryByTestId("notice-banner")).toBeNull();
  });
});
