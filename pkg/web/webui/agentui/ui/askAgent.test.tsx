import "@testing-library/jest-dom/vitest";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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

afterEach(() => { cleanup(); vi.restoreAllMocks(); window.history.replaceState(null, "", "/"); });

// One declaration carrying BOTH kinds of action, because the thing under test
// is that they are told apart — a fixture with only one kind would pass
// against a build that routed everything the same way.
const declaration = {
  actions: [
    { name: "summarize", prompt: "Summarize what's on screen." },
    { name: "company_detail", inputs: ["name"], prompt: "Tell me about {name}." },
    { name: "refresh", tool: "crm_refresh" },
  ],
  view: { component: "ap:stack", children: [
    { component: "ap:button", props: { action: "summarize", label: "Summarize this view" } },
    { component: "ap:button", props: { action: "refresh", label: "Refresh" } },
    { component: "ap:table", props: {
      columns: [{ key: "name" }],
      rows: [{ name: "Acme" }],
      rowAction: "company_detail",
      rowActionLabel: "Ask about",
    } },
  ] },
};

const props = { ns: "demo-ns", name: "demo-session", declaration, shown: true } as const;

// Records every request so a test can assert WHICH route a control reached.
// The route is the whole claim: asking the agent something is sending it a
// message, and a message belongs in the transcript where the viewer can see
// what was asked on their behalf and read the reply.
function stubFetch() {
  const calls: { url: string; body: Record<string, unknown> }[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, init: RequestInit) => {
    calls.push({ url: String(url), body: JSON.parse(String(init.body ?? "{}")) });
    if (String(url).endsWith("/bindings")) {
      return { ok: true, json: async () => ({ bindings: {} }) } as Response;
    }
    return { ok: true, json: async () => ({ requestId: "r1", state: "succeeded" }) } as Response;
  }));
  return calls;
}

const nonBinding = (calls: { url: string; body: Record<string, unknown> }[]) =>
  calls.filter((c) => !c.url.endsWith("/bindings"));

describe("an action that asks the agent", () => {
  it("sends a message to the transcript rather than posting to the actions route", async () => {
    const calls = stubFetch();
    const { container } = render(<AgentUIView {...props} />);

    fireEvent.click(within(container).getByText("Summarize this view"));

    await waitFor(() => expect(nonBinding(calls)).toHaveLength(1));
    const sent = nonBinding(calls)[0];
    expect(sent.url).toBe("/sessions/api/demo-ns/demo-session/message");
    expect(sent.body.text).toBe("Summarize what's on screen.");
  });

  // The negative control. A tool action must keep going to the actions route,
  // where the tool, its args template and its authorization live — routing it
  // through the transcript would turn a gated call into a sentence.
  it("leaves a tool action on the actions route", async () => {
    const calls = stubFetch();
    const { container } = render(<AgentUIView {...props} />);

    fireEvent.click(within(container).getByText("Refresh"));

    await waitFor(() => expect(nonBinding(calls)).toHaveLength(1));
    expect(nonBinding(calls)[0].url).toBe("/agent-ui/demo-ns/demo-session/actions");
    expect(nonBinding(calls)[0].body.action).toBe("refresh");
  });

  // The two halves of #7 meeting: a row's own value reaches the sentence, so
  // the question is about the record whose button was pressed.
  it("fills a row's value into the sentence a row control sends", async () => {
    const calls = stubFetch();
    const { container } = render(<AgentUIView {...props} />);

    fireEvent.click(within(container).getByTestId("agent-ui-row-action"));

    await waitFor(() => expect(nonBinding(calls)).toHaveLength(1));
    expect(nonBinding(calls)[0].body.text).toBe("Tell me about Acme.");
  });

  // Terminal on DELIVERY, not on an answer. The agent's reply arrives as a
  // turn and, if it composes one, a view update — both on their own channels.
  // Holding the control pending until then would leave it stuck forever for
  // any question answered in prose alone.
  //
  // The caption on the control is the WHOLE of what the ask announces. The page
  // itself says nothing while an answer is outstanding: a question may be
  // answered in prose and never touch a hook, so a page-level "working"
  // treatment would sit on a finished page describing work nobody is doing.
  // What the page reports instead is a fact after the event — the per-hook
  // updated cue, covered in AgentUIView.test.tsx.
  it("settles once the message is delivered, and the page itself claims nothing", async () => {
    stubFetch();
    const { container } = render(<AgentUIView {...props} />);

    fireEvent.click(within(container).getByText("Summarize this view"));
    await waitFor(() => expect(screen.getByTestId("agent-ui-action-status")).toHaveTextContent(/asked the agent/i));
    expect(container.querySelector('[data-testid="agent-ui-slot-working"]')).toBeNull();
    expect(screen.queryByText("The agent is working on this…")).toBeNull();
  });

  // A failure to reach the agent must be visible on the control that tried.
  // Silently doing nothing is the failure mode this whole button exists to
  // replace.
  it("says so when the agent cannot be reached", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    vi.stubGlobal("fetch", vi.fn(async (url: string) => {
      if (String(url).endsWith("/bindings")) return { ok: true, json: async () => ({ bindings: {} }) } as Response;
      return { ok: false, status: 503 } as Response;
    }));
    const { container } = render(<AgentUIView {...props} />);

    fireEvent.click(within(container).getByText("Summarize this view"));
    await waitFor(() => expect(screen.getByTestId("agent-ui-action-status")).toHaveTextContent(/could not reach/i));
  });
});
