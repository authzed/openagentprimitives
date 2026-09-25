import * as React from "react";
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, act, cleanup, fireEvent } from "@testing-library/react";
import type { LiveMessage } from "./types";

// useLiveSocket reaches for a real WebSocket; mock it so tests stay in jsdom.
// The mock captures the onMessage callback so tests can fire live messages.
let capturedOnMessage: ((m: LiveMessage) => void) | null = null;

vi.mock("./useLiveSocket", () => ({
  useLiveSocket: (onMessage: (m: LiveMessage) => void) => {
    capturedOnMessage = onMessage;
    return { state: "connected" as const };
  },
}));

// useHostBridge does iframe DOM manipulation; replace with a no-op.
vi.mock("./useHostBridge", () => ({
  useHostBridge: () => (_u: { hostUrl?: string; contentUrl?: string }) => {},
}));

import { ArtifactView } from "./ArtifactView";

afterEach(() => {
  cleanup();
  capturedOnMessage = null;
});

const baseProps = {
  artifactName: "My Artifact",
  artifactDescription: "",
  hostUrl: "",
  contentUrl: "",
  sandboxOrigin: "https://example.com",
  backLink: "/thread/1",
  channelKind: "slack",
  ns: "default",
  name: "sess-1",
  artifactId: "art-1",
  interactions: [] as string[],
};

describe("ArtifactView – toolbar download", () => {
  it("offers a same-origin Download link for the current artifact", () => {
    render(<ArtifactView {...baseProps} artifactId="art-9" ns="myns" name="mysess" />);
    const link = screen.getByRole("link", { name: "Download" }) as HTMLAnchorElement;
    const href = link.getAttribute("href") ?? "";
    expect(href).toContain("/artifact-download?");
    expect(href).toContain("artifactId=art-9");
    expect(href).toContain("sessionRef=myns%2Fmysess");
  });
});

describe("ArtifactView – Generating preview overlay", () => {
  it("shows the overlay when hostUrl is empty and no live message has arrived", () => {
    render(<ArtifactView {...baseProps} hostUrl="" contentUrl="" />);
    expect(screen.getByText("Generating preview…")).toBeTruthy();
  });

  it("does not show the overlay when hostUrl is non-empty on first render", () => {
    render(<ArtifactView {...baseProps} hostUrl="https://example.com/artifact-host?ct=1" contentUrl="https://example.com/preview/1" />);
    expect(screen.queryByText("Generating preview…")).toBeNull();
  });

  it("hides the overlay after a live message with a non-empty hostUrl/contentUrl arrives", () => {
    render(<ArtifactView {...baseProps} hostUrl="" contentUrl="" />);
    expect(screen.getByText("Generating preview…")).toBeTruthy();

    act(() => {
      capturedOnMessage!({
        type: "snapshot",
        current: { seq: 1, revisionId: "r1", hostUrl: "https://example.com/artifact-host?ct=r1", contentUrl: "https://example.com/preview/r1" },
        revisions: [],
      });
    });

    expect(screen.queryByText("Generating preview…")).toBeNull();
  });

  it("keeps the overlay when a live message arrives without a hostUrl/contentUrl", () => {
    render(<ArtifactView {...baseProps} hostUrl="" contentUrl="" />);

    act(() => {
      capturedOnMessage!({
        type: "snapshot",
        revisions: [],
      });
    });

    expect(screen.getByText("Generating preview…")).toBeTruthy();
  });
});

describe("ArtifactView – iframe framing", () => {
  it("frames the host URL with sandbox=allow-scripts allow-same-origin", () => {
    render(<ArtifactView {...baseProps} hostUrl="https://example.com/artifact-host?ct=1" contentUrl="https://example.com/preview/1" />);
    const iframe = screen.getByTitle("artifact content") as HTMLIFrameElement;
    expect(iframe.getAttribute("src")).toBe("https://example.com/artifact-host?ct=1");
    expect(iframe.getAttribute("sandbox")).toBe("allow-scripts allow-same-origin");
  });

  it("leaves src unset when hostUrl is empty", () => {
    render(<ArtifactView {...baseProps} hostUrl="" contentUrl="" />);
    const iframe = screen.getByTitle("artifact content") as HTMLIFrameElement;
    expect(iframe.getAttribute("src")).toBeNull();
  });
});

// dispatchAnnotSend simulates the host doc (sandbox iframe) posting an
// annotation bundle up to the shell — the exact { ap:"annot", cmd:"send" }
// postMessage ArtifactView listens for. It stamps origin + source so the
// shell's origin/source guards accept it.
function dispatchAnnotSend(iframe: HTMLIFrameElement, origin: string) {
  const ev = new MessageEvent("message", {
    data: { ap: "annot", cmd: "send", bundle: { artifactId: "", annotations: [{ index: 1, comment: "reword", target: "element" }] } },
    origin,
    source: iframe.contentWindow,
  });
  window.dispatchEvent(ev);
}

describe("ArtifactView – annotation send failure surfacing", () => {
  it("shows a visible error alert when the annotation send returns a non-2xx", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 502,
      json: async () => ({ error: "no responders available for request" }),
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<ArtifactView {...baseProps} sandboxOrigin="https://example.com" />);
    const iframe = screen.getByTitle("artifact content") as HTMLIFrameElement;

    act(() => dispatchAnnotSend(iframe, "https://example.com"));

    // The handler must actually run (origin/source guards passed).
    expect(fetchMock).toHaveBeenCalled();
    // The failure must be surfaced to the user, not just console.error'd.
    const alert = await screen.findByRole("alert");
    expect(alert.textContent ?? "").toMatch(/annotation/i);

    vi.unstubAllGlobals();
  });

  it("shows a visible error alert when the annotation send rejects at the network level", async () => {
    const fetchMock = vi.fn().mockRejectedValue(new Error("network down"));
    vi.stubGlobal("fetch", fetchMock);

    render(<ArtifactView {...baseProps} sandboxOrigin="https://example.com" />);
    const iframe = screen.getByTitle("artifact content") as HTMLIFrameElement;

    act(() => dispatchAnnotSend(iframe, "https://example.com"));

    expect(fetchMock).toHaveBeenCalled();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent ?? "").toMatch(/annotation/i);

    vi.unstubAllGlobals();
  });
});

describe("ArtifactView – chat panel gating", () => {
  it("does not render the chat panel when interactions is empty", () => {
    render(<ArtifactView {...baseProps} interactions={[]} />);
    expect(screen.queryByPlaceholderText("Message the agent…")).toBeNull();
  });

  it("renders a read-only chat panel (no compose box) when interactions doesn't include user_message", () => {
    render(<ArtifactView {...baseProps} interactions={["some_other_kind"]} />);
    expect(screen.queryByPlaceholderText("Message the agent…")).toBeNull();
  });

  it("renders the compose box when interactions includes user_message", () => {
    render(<ArtifactView {...baseProps} interactions={["user_message"]} />);
    expect(screen.getByPlaceholderText("Message the agent…")).toBeTruthy();
  });

  it("appends an incoming message frame to the chat panel", () => {
    render(<ArtifactView {...baseProps} interactions={["user_message"]} />);

    act(() => {
      capturedOnMessage!({
        type: "message",
        message: { role: "agent", text: "done with the edit", author: "agent", at: "2026-01-01T00:00:00Z" },
      });
    });

    expect(screen.getByText("done with the edit")).toBeTruthy();
  });

  it("posts a user_message to /interact on send and clears the draft on success", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({}) });
    vi.stubGlobal("fetch", fetchMock);

    render(<ArtifactView {...baseProps} ns="myns" name="mysess" artifactId="art-9" interactions={["user_message"]} />);

    const input = screen.getByPlaceholderText("Message the agent…") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "hello agent" } });
    await act(async () => {
      fireEvent.click(screen.getByText("Send"));
      await Promise.resolve();
    });

    expect(fetchMock).toHaveBeenCalledWith(
      "/session/myns/mysess/interact",
      expect.objectContaining({
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ kind: "user_message", artifactId: "art-9", payload: { text: "hello agent" } }),
      }),
    );
    // success clears the draft; the sent text itself is never appended
    // optimistically — it only renders once its user_echo round-trips.
    expect((screen.getByPlaceholderText("Message the agent…") as HTMLInputElement).value).toBe("");

    vi.unstubAllGlobals();
  });
});
