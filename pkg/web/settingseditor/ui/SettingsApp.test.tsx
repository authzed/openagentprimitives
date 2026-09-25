import { render, screen, waitFor, fireEvent, cleanup } from "@testing-library/react";
import { describe, expect, it, vi, afterEach } from "vitest";
import { SettingsApp } from "./SettingsApp";

// FakeEventSource stands in for jsdom's missing EventSource. Unlike
// adminui's session stream (named addEventListener channels), the settings
// server's GET /api/events (handleEvents) writes plain unnamed
// `data: <json>` frames, so a real browser EventSource delivers them via
// onmessage — this fake mirrors that, not the named-event shape.
class FakeEventSource {
  static last: FakeEventSource | undefined;
  url: string;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onerror: (() => void) | null = null;
  closed = false;
  constructor(url: string) {
    this.url = url;
    FakeEventSource.last = this;
  }
  close() {
    this.closed = true;
  }
}

function stubFetch(state: unknown, status = 200) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/api/state")) {
        return new Response(JSON.stringify(state), { status });
      }
      return new Response(JSON.stringify({ error: "not found" }), { status: 404 });
    }),
  );
  vi.stubGlobal("EventSource", FakeEventSource as unknown as typeof EventSource);
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("SettingsApp", () => {
  it("renders five tabs and shows the stopped phase from stubbed state", async () => {
    stubFetch({ running: false, phase: "stopped" });
    render(<SettingsApp apiBase="/api" />);

    const tabs = screen.getAllByRole("tab");
    expect(tabs.map((t) => t.textContent)).toEqual(["General", "Model", "Cluster", "Advanced", "About"]);

    await waitFor(() => expect(screen.getByText("stopped")).toBeTruthy());
  });

  it("switching tabs shows the target panel", async () => {
    stubFetch({ running: false, phase: "stopped" });
    render(<SettingsApp apiBase="/api" />);
    await waitFor(() => expect(screen.getByText("stopped")).toBeTruthy());

    // Only the General panel's stub text is visible before switching.
    expect(screen.getByRole("tabpanel").textContent).toMatch(/General/i);

    // Radix's TabsTrigger activates on mousedown (not click) — see
    // @radix-ui/react-tabs' TabsTrigger, which wires selection to onMouseDown.
    fireEvent.mouseDown(screen.getByRole("tab", { name: "Model" }));
    await waitFor(() => expect(screen.getByRole("tabpanel").textContent).toMatch(/Model/i));
  });

  it("uses a bounded viewport shell so tab content scrolls internally, not the document", async () => {
    stubFetch({ running: false, phase: "stopped" });
    const { container } = render(<SettingsApp apiBase="/api" />);
    await waitFor(() => expect(screen.getByText("stopped")).toBeTruthy());

    const root = container.firstChild as HTMLElement;
    expect(root.className).toMatch(/h-screen/);
    const panel = screen.getByRole("tabpanel");
    expect(panel.className).toMatch(/flex-1/);
    expect(panel.className).toMatch(/min-h-0/);
  });

  it("shows the step alongside phase while starting", async () => {
    stubFetch({ running: false, phase: "starting", step: "waiting for SpiceDB" });
    render(<SettingsApp apiBase="/api" />);
    await waitFor(() => expect(screen.getByText(/starting/i)).toBeTruthy());
    expect(screen.getByText(/waiting for SpiceDB/)).toBeTruthy();
  });
});
