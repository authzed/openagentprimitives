import { render, screen, waitFor, fireEvent, cleanup } from "@testing-library/react";
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { LiveSessions } from "./LiveSessions";
import type { SessionState } from "../lib/api";

// Five minutes ago, computed at load so fmtRelative reads "5m ago" when the test
// runs milliseconds later (well clear of the 45s "just now" threshold).
const fiveMinAgo = new Date(Date.now() - 5 * 60 * 1000).toISOString();

const slackSession: SessionState = {
  namespace: "default", name: "s1", class: "support-bot", channelKind: "slack",
  channelName: "support-slack", startedAt: fiveMinAgo, startedBy: "service:hubspot-bot",
  phase: "Running", turnCount: 2, inputTokens: 12400, outputTokens: 3100,
  toolCallCount: 5, elapsedSeconds: 84, active: true, statusText: "running kubectl get pods",
  pendingToolGrants: 1, pendingLeakageApprovals: 0, lastEventAt: new Date().toISOString(),
};

const fakeSession: SessionState = {
  namespace: "team", name: "s2", class: "billing-bot", channelKind: "fake",
  phase: "Idle", turnCount: 0, inputTokens: 0, outputTokens: 0,
  toolCallCount: 0, elapsedSeconds: 0, active: false,
  pendingToolGrants: 0, pendingLeakageApprovals: 0, lastEventAt: new Date().toISOString(),
};

// FakeEventSource stands in for jsdom's missing EventSource and lets a test
// drive the stream: emit() delivers a named frame, drop() reproduces a
// connection loss (the browser's own auto-reconnect is invisible to the page,
// so a reconnect is just drop() followed by the server's re-sent snapshot).
class FakeEventSource {
  static last: FakeEventSource | undefined;
  listeners = new Map<string, (ev: MessageEvent) => void>();
  onerror: (() => void) | undefined;
  closed = false;
  constructor() { FakeEventSource.last = this; }
  addEventListener(type: string, cb: (ev: MessageEvent) => void) { this.listeners.set(type, cb); }
  close() { this.closed = true; }
  emit(type: string, data: unknown) {
    this.listeners.get(type)?.({ data: JSON.stringify(data) } as MessageEvent);
  }
  drop() { this.onerror?.(); }
}

// stream returns the live FakeEventSource the component opened.
function stream(): FakeEventSource {
  const es = FakeEventSource.last;
  if (!es) throw new Error("no EventSource opened");
  return es;
}

function stub(sessions: SessionState[]): void {
  vi.stubGlobal("fetch", vi.fn(async () =>
    new Response(JSON.stringify(sessions), { status: 200, headers: { "Content-Type": "application/json" } })));
  vi.stubGlobal("EventSource", FakeEventSource as unknown as typeof EventSource);
}

// stubDeferred stubs fetch with a promise the test resolves by hand, so a
// stream frame can be interleaved with an in-flight GET.
function stubDeferred(): (sessions: SessionState[]) => void {
  let release!: (sessions: SessionState[]) => void;
  const body = new Promise<SessionState[]>((res) => { release = res; });
  vi.stubGlobal("fetch", vi.fn(async () =>
    new Response(JSON.stringify(await body), { status: 200, headers: { "Content-Type": "application/json" } })));
  vi.stubGlobal("EventSource", FakeEventSource as unknown as typeof EventSource);
  return release;
}

beforeEach(() => {
  window.history.pushState({}, "", "/admin/sessions");
  document.cookie = "ap_admin_sessions_view=; path=/; Max-Age=0";
  FakeEventSource.last = undefined;
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

describe("LiveSessions", () => {
  it("renders a card per session with class, phase, status, and counters", async () => {
    stub([slackSession]);
    render(<LiveSessions apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/support-bot/)).toBeTruthy());
    expect(screen.getByText(/Running/)).toBeTruthy();
    expect(screen.getByText(/running kubectl get pods/)).toBeTruthy();
    expect(screen.getByText(/12\.4K in/)).toBeTruthy();
    expect(screen.getByText(/1 approval/)).toBeTruthy();
  });

  it("card click navigates to the session page (not an overlay)", async () => {
    stub([slackSession]);
    render(<LiveSessions apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/support-bot/)).toBeTruthy());
    fireEvent.click(screen.getByText(/support-bot/));
    expect(window.location.pathname).toBe("/admin/session/default/s1");
  });

  it("kind filter narrows the list to the selected channelKind", async () => {
    stub([slackSession, fakeSession]);
    render(<LiveSessions apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/support-bot/)).toBeTruthy());
    expect(screen.getByText(/billing-bot/)).toBeTruthy();
    // Click the "fake" kind chip → only the fake-transport session remains.
    fireEvent.click(screen.getByRole("button", { name: "fake" }));
    await waitFor(() => expect(screen.queryByText(/support-bot/)).toBeNull());
    expect(screen.getByText(/billing-bot/)).toBeTruthy();
  });

  it("ViewToggle switches from the card grid to a table", async () => {
    stub([slackSession]);
    render(<LiveSessions apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/support-bot/)).toBeTruthy());
    // Grid has no column headers; the table does.
    expect(screen.queryByRole("columnheader", { name: "Session" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Table view" }));
    await waitFor(() => expect(screen.getByRole("columnheader", { name: "Session" })).toBeTruthy());
    // Table row is clickable to the page too. The default namespace is hidden in
    // the Session cell, so the visible text is the bare name.
    fireEvent.click(screen.getByText("s1"));
    expect(window.location.pathname).toBe("/admin/session/default/s1");
  });

  it("table: phase is a color-coded pill, Channel/Class link out, Started is humanized", async () => {
    stub([slackSession]);
    render(<LiveSessions apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/support-bot/)).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Table view" }));
    await waitFor(() => expect(screen.getByRole("columnheader", { name: "Channel" })).toBeTruthy());
    // No stale "Kind" header — it was renamed to Channel.
    expect(screen.queryByRole("columnheader", { name: "Kind" })).toBeNull();

    // Phase → a rounded status pill in the primary (Running) tone.
    const phase = screen.getByText("Running");
    expect(phase.className).toContain("rounded-full");
    expect(phase.className).toContain("text-primary");

    // Class links to the agent detail page.
    expect(screen.getByRole("link", { name: "support-bot" }).getAttribute("href")).toBe(
      "/admin/agent/default/support-bot",
    );
    // Channel links to the channel detail page (name shown, kind as a KindBadge
    // in the same cell — scoped so it doesn't collide with the KindFilter chip).
    const channelLink = screen.getByRole("link", { name: /support-slack/ });
    expect(channelLink.getAttribute("href")).toBe("/admin/channel/default/support-slack");
    const channelCell = channelLink.closest("td") as HTMLElement;
    expect(channelCell.textContent).toContain("slack");
    // The channel kind renders as a color-coded KindBadge (a stable color dot).
    expect(channelCell.querySelector('span[aria-hidden="true"][style*="background-color"]')).toBeTruthy();

    // Started is humanized relative time, with startedBy decoded beneath.
    expect(screen.getByText(/ago|just now/)).toBeTruthy();
    expect(screen.getByText("service:hubspot-bot")).toBeTruthy();
  });

  // A session that ends while the browser is disconnected produces no `remove`
  // frame for this client: the server re-sends its snapshot on reconnect, and a
  // snapshot can only RE-ADD. Absence must therefore be reconciled from an
  // authoritative list when the stream comes back, or the row sits in the grid
  // with a stale phase until a full page reload.
  it("session ended during a disconnect is dropped once the stream recovers", async () => {
    stub([slackSession, fakeSession]);
    render(<LiveSessions apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/billing-bot/)).toBeTruthy());

    // The stream drops; while it is down, s2 ends and is deleted server-side.
    stream().drop();
    vi.stubGlobal("fetch", vi.fn(async () =>
      new Response(JSON.stringify([slackSession]), { status: 200, headers: { "Content-Type": "application/json" } })));
    // Reconnect: the server re-sends its snapshot, which no longer has s2.
    stream().emit("session", slackSession);

    await waitFor(() => expect(screen.queryByText(/billing-bot/)).toBeNull());
    expect(screen.getByText(/support-bot/)).toBeTruthy();
  });

  // The initial GET and the stream are two sources for one map. A `remove` that
  // lands while the GET is in flight predates nothing in that GET's list, and
  // DeleteSession broadcasts no follow-up upsert — so if the GET replaces the
  // map wholesale, that row is resurrected and never corrected.
  it("remove during the initial fetch is not resurrected when the fetch lands", async () => {
    const release = stubDeferred();
    render(<LiveSessions apiBase="/admin/api" />);
    await waitFor(() => expect(FakeEventSource.last).toBeTruthy());

    // s2 is deleted while GET /sessions is still in flight...
    stream().emit("remove", { namespace: fakeSession.namespace, name: fakeSession.name });
    // ...and the older GET result, which still lists it, lands afterwards.
    release([slackSession, fakeSession]);

    await waitFor(() => expect(screen.getByText(/support-bot/)).toBeTruthy());
    expect(screen.queryByText(/billing-bot/)).toBeNull();
  });

  // getJSON is the only path that surfaces a hard API error (the stream's
  // onerror only marks the feed stale), so the reconcile must keep it.
  it("failed session fetch surfaces the error alert", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("nope", { status: 500 })));
    vi.stubGlobal("EventSource", FakeEventSource as unknown as typeof EventSource);
    render(<LiveSessions apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/Could not load sessions/)).toBeTruthy());
  });
});
