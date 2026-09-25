import "@testing-library/jest-dom/vitest";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
// AgentUIView subscribes to the shell's session socket for its own fallbacks;
// every mount below renders the view ALONE, outside that provider, and this
// file is not about those signals. Stubbed to the idle value so the provider's
// own "no frames will arrive" report — correct, and exactly what a view with no
// socket must say — does not bury this file's real output.
vi.mock("../../chat/ui/sessionSignals", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../chat/ui/sessionSignals")>();
  return { ...actual, useSessionSignals: () => actual.INITIAL_SESSION_SIGNALS };
});

import { AgentUIView } from "./AgentUIView";

// The URL is shared, mutable state in jsdom and AgentUIView both seeds from it
// and writes to it, so a test that did not reset it would inherit the previous
// test's filters — and here that would silently invert the very precedence
// these tests exist to pin.
afterEach(() => { cleanup(); vi.restoreAllMocks(); window.history.replaceState(null, "", "/"); });

const declaration = {
  view: { component: "ap:stack", children: [
    { component: "ap:select", props: { param: "span", value: "7d",
        options: [{ value: "7d", label: "7 days" }, { value: "30d", label: "30 days" }] } },
    { component: "ap:table", props: { columns: [{ key: "name" }], rows: [] },
        bindings: { rows: { source: "tool", ref: "crm_list_leads",
          args: { since: { $param: "span" } } } } },
  ] },
};

// Records the `span` each binding request was made with, which is the only
// evidence that matters: a parameter that does not reach the request has not
// been applied, however it renders.
function stubFetch() {
  const spans: string[] = [];
  vi.stubGlobal("fetch", vi.fn(async (_url: string, init: RequestInit) => {
    spans.push((JSON.parse(String(init.body)) as { params: { span: string } }).params.span);
    return { ok: true, json: async () => ({ bindings: {
      "/1#rows": { status: "ok", value: [{ name: "Acme" }] },
    } }) } as Response;
  }));
  return spans;
}

const props = { ns: "demo-ns", name: "demo-session", declaration, shown: true } as const;

// Three sources can supply a control's value, and they are NOT
// interchangeable. An author's default is what the page means with nobody
// having said anything; the agent's choice is somebody having said something
// ("the last seven days", in a channel, before this page was open); the
// viewer's own selection is a hand on the control. The ordering has to be
// exactly this, because getting it wrong in the other direction means an agent
// re-filtering a page out from under someone reading it.
describe("whose value a control shows", () => {
  it("uses the author's default when nobody has said anything", async () => {
    const spans = stubFetch();
    render(<AgentUIView {...props} />);
    await waitFor(() => expect(spans).toEqual(["7d"]));
  });

  it("prefers the agent's choice over the author's default", async () => {
    const spans = stubFetch();
    render(<AgentUIView {...props} agentParams={{ span: "30d" }} />);
    await waitFor(() => expect(spans).toEqual(["30d"]));
  });

  // A viewer who followed a link to a filtered view has stated that filter as
  // surely as one who picked it on the page. Treating the address bar as
  // weaker than the agent would mean a shared link silently opened somewhere
  // other than where it pointed.
  it("prefers the address bar over the agent's choice", async () => {
    window.history.replaceState(null, "", "/?p.span=7d");
    const spans = stubFetch();
    render(<AgentUIView {...props} agentParams={{ span: "30d" }} />);
    await waitFor(() => expect(spans).toEqual(["7d"]));
  });

  // The server filters stale keys too. Both filters are needed and neither is
  // redundant: this one stops a value stored against a control a redeploy
  // removed from being applied to a page that has no way to show or clear it.
  it("drops an agent value for a control this declaration does not have", async () => {
    const spans = stubFetch();
    render(<AgentUIView {...props} agentParams={{ span: "30d", "window.from": "2026-02-01T00:00:00Z" }} />);
    await waitFor(() => expect(spans).toEqual(["30d"]));
  });
});

// The live half. set_view_params changes no hook, so the declaration on its
// frame is byte-identical to the one already applied — and AgentUIView's echo
// guard returns early on exactly that. Applying the parameters before the
// guard is what stops the whole feature from silently doing nothing on the one
// frame that carries it.
describe("an agent setting a control while someone is watching", () => {
  function openSocket() {
    const sockets: { onmessage?: (e: { data: string }) => void }[] = [];
    class FakeSocket {
      onmessage?: (e: { data: string }) => void;
      onclose?: () => void;
      onerror?: () => void;
      onopen?: () => void;
      readyState = 1;
      constructor() { sockets.push(this); }
      close() {}
      send() {}
    }
    vi.stubGlobal("WebSocket", FakeSocket as unknown as typeof WebSocket);
    return sockets;
  }

  it("fills a control the viewer has not touched", async () => {
    const spans = stubFetch();
    const sockets = openSocket();
    render(<AgentUIView {...props} />);
    await waitFor(() => expect(spans).toEqual(["7d"]));
    await waitFor(() => expect(sockets.length).toBeGreaterThan(0));

    sockets[0].onmessage?.({ data: JSON.stringify({ type: "view", declaration, agentParams: { span: "30d" } }) });
    await waitFor(() => expect(spans).toContain("30d"));
  });

  // The rule that makes this safe to ship at all. An agent that can move a
  // filter under a reader turns a dashboard into something that changes what
  // it says while you are reading it.
  it("leaves a control the viewer HAS touched exactly where they put it", async () => {
    const spans = stubFetch();
    const sockets = openSocket();
    const { container } = render(<AgentUIView {...props} />);
    await waitFor(() => expect(spans).toEqual(["7d"]));
    await waitFor(() => expect(sockets.length).toBeGreaterThan(0));

    fireEvent.change(within(container).getByRole("combobox"), { target: { value: "30d" } });
    await waitFor(() => expect(spans).toContain("30d"));

    sockets[0].onmessage?.({ data: JSON.stringify({ type: "view", declaration, agentParams: { span: "7d" } }) });
    // Given time to be wrong: the assertion is that "7d" never comes back,
    // which needs a window in which it could have.
    await new Promise((r) => setTimeout(r, 400));
    expect(spans[spans.length - 1]).toBe("30d");
  });

  // An echo frame re-offering values already in effect must be inert. Not a
  // cosmetic point: every applied frame bumps the binding generation and costs
  // a full server-side fan-out, so a frame that changes nothing must cost
  // nothing.
  it("does not re-run bindings for a frame that changes nothing", async () => {
    const spans = stubFetch();
    const sockets = openSocket();
    render(<AgentUIView {...props} agentParams={{ span: "30d" }} />);
    await waitFor(() => expect(spans).toEqual(["30d"]));
    await waitFor(() => expect(sockets.length).toBeGreaterThan(0));

    sockets[0].onmessage?.({ data: JSON.stringify({ type: "view", declaration, agentParams: { span: "30d" } }) });
    await new Promise((r) => setTimeout(r, 400));
    expect(spans).toEqual(["30d"]);
  });
});
