import "@testing-library/jest-dom/vitest";
import * as React from "react";
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { render, cleanup, fireEvent, screen, within, waitFor, act } from "@testing-library/react";

// The two seams the view's fallbacks reach the session through, both stubbed
// for this file. useSessionSignals is replaced by a settable value so a row
// can put the session in whatever live state it is about (mid-turn, waiting on
// a reply) without a socket; INITIAL_SESSION_SIGNALS is re-exported from the
// REAL module rather than re-spelled here, so a field added to the fold cannot
// silently default to something this file invented. sendSessionMessage is
// stubbed because a reply's whole claim is which route it travels, and the row
// asserts on the call, not on a server.
let mockSignals: SessionSignals;
vi.mock("../../chat/ui/sessionSignals", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../chat/ui/sessionSignals")>();
  return { ...actual, useSessionSignals: () => mockSignals };
});
// sessionPath/failureText are left as the REAL implementation (via
// importOriginal): useInteractionDecision.ts imports them from this same
// module to build the decision-route URL and read a failed response's error
// text, and the decisions-region tests below assert on both.
vi.mock("../../chat/ui/sessionMessage", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../chat/ui/sessionMessage")>();
  return { ...actual, sendSessionMessage: vi.fn(async () => {}) };
});
// useSessionFrames is the view's OTHER seam onto the session: this file mounts
// no SessionSocketProvider (that's the shell's — see SessionShell.test.tsx for
// the real socket), so a row that needs to deliver a session frame (a
// rejected decision) captures the handler the view subscribed, the same way
// mockSignals above stands in for the fold a real provider would run.
let mockFrameHandler: ((f: ChatFrame) => void) | null = null;
vi.mock("../../chat/ui/SessionSocket", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../chat/ui/SessionSocket")>();
  return {
    ...actual,
    useSessionFrames: (handler: (f: ChatFrame) => void) => {
      mockFrameHandler = handler;
    },
  };
});

import { noticeIdentity } from "@ap/agentui";
import { INITIAL_SESSION_SIGNALS, type SessionSignals } from "../../chat/ui/sessionSignals";
import type { ChatFrame, InteractionRequestInner } from "../../chat/ui/types";
import { sendSessionMessage } from "../../chat/ui/sessionMessage";
import { AgentUIView, HOOK_UPDATED_MS, sameChromeSignals, type AgentUIViewProps, type ChromeSignals } from "./AgentUIView";
import goldenProps from "./testdata/props.golden.json";
import actionsGolden from "./testdata/props.actions.golden.json";
import viewModelGolden from "./testdata/props.viewmodel.golden.json";

// Every block below renders AgentUIView alone, inside a stand-in content
// region carrying the SAME testid the shell's chrome gives the real one. The
// view has exactly one host now — the session shell — so every claim about the
// COMPOSITION of chrome around it (auto-reveal on a trust event, collapse,
// region structure) belongs to that host's own suite,
// pkg/web/webui/sessions/ui/SessionShell.test.tsx. Chrome's own behavior
// (precedence, never-suppressible) is proven where Chrome lives:
// pkg/web/webui/sessions/ui/Chrome.test.tsx.

// Every mount below fires a POST /agent-ui/{ns}/{name}/bindings on
// mount (useBindings.test.tsx owns that behavior's own coverage) and opens a
// GET .../live websocket (useActionLifecycle.test.tsx owns that behavior's
// own coverage, including J1) — none of the declarations in THIS file click
// an action, so an empty response is the correct stand-in for both fetch
// targets, keeping this file's tests about chrome/rendering, not the
// network. Without the fetch stub, jsdom's fetch would still be exercised
// for real (and safely swallowed by useBindings'/useActionLifecycle's own
// error handling — nothing here relies on it), but it would fail to resolve
// a relative URL against a base and log noise on every render. jsdom has no
// WebSocket global at all, so useActionLifecycle's own `new WebSocket(...)`
// already fails closed and logs without a stub — FakeSocket here only
// silences that expected noise so a real assertion failure isn't lost in it.
class FakeSocket {
  static instances: FakeSocket[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((ev: { data: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  onclose: (() => void) | null = null;
  close = vi.fn();
  constructor(_url: string) {
    FakeSocket.instances.push(this);
  }
}

// CONTENT_REGION is the testid the session shell's chrome puts on the region
// this view renders into. Rendering the view inside a stand-in carrying the
// same testid is what keeps every `within(...)` assertion below reading the
// same scope it read when the view still rendered its own chrome.
const CONTENT_REGION = "session-shell-content-region";

// ViewProps is AgentUIViewProps with `shown` optional. The real prop is
// required — the shell keeps this view mounted behind `hidden` and must say
// which one is on screen — but a view rendered ALONE is by definition the thing
// on screen, so renderView answers it (true) and no fixture below has to hold
// an opinion about a shell this file does not model. A row that is about being
// hidden passes shown: false for itself.
type ViewProps = Omit<AgentUIViewProps, "shown"> & { shown?: boolean };

function renderView(props: ViewProps) {
  return render(
    <div data-testid={CONTENT_REGION}>
      <AgentUIView shown {...props} />
    </div>,
  );
}

// renderViewWithLive renders the view and hands back a handle that delivers
// frames on the socket it opened, exactly as live.go would. It drives the SAME
// FakeSocket the whole file already installs rather than a second stand-in, so
// there is one answer here to "what is a websocket in this suite".
//
// `frame` wraps the delivery in act(): a socket message is a setState from
// outside React's own event system, and delivering one unwrapped both warns
// and leaves the assertion reading a DOM React has not committed yet.
function renderViewWithLive(props: ViewProps) {
  const result = renderView(props);
  return {
    ...result,
    frame(msg: Record<string, unknown>) {
      const sock = FakeSocket.instances[FakeSocket.instances.length - 1];
      // Asserted, not optional-chained away: with no socket constructed the
      // delivery would be a silent no-op, and every NEGATIVE cue assertion
      // ("nothing is marked") would pass without a frame ever arriving.
      expect(sock).toBeDefined();
      act(() => {
        sock.onmessage?.({ data: JSON.stringify(msg) });
      });
    },
  };
}

// deliverFrame simulates one frame arriving on the session socket — this
// file's stand-in for a live SessionSocketProvider is the useSessionFrames
// mock above, not FakeSocket (that one is the agent-UI live channel's own
// socket, a different transport). Wrapped in act() for the same reason
// renderViewWithLive's `frame` is: a frame delivery is a setState from outside
// React's own event system.
function deliverFrame(f: ChatFrame) {
  // Asserted, not optional-chained away: a null handler here means the view
  // never subscribed, and every NEGATIVE assertion below ("nothing changed")
  // would then pass without a frame ever reaching it.
  expect(mockFrameHandler).not.toBeNull();
  act(() => mockFrameHandler!(f));
}

beforeEach(() => {
  FakeSocket.instances = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) =>
      String(url).endsWith("/actions")
        ? ({ ok: true, json: async () => ({ requestId: "r1", state: "submitted" }) }) as unknown as Response
        : ({ ok: true, json: async () => ({ bindings: {} }) }) as unknown as Response,
    ),
  );
  vi.stubGlobal("WebSocket", FakeSocket);
  // Every row starts from an idle session; the fallback rows below say
  // otherwise for themselves. Reset here rather than in one describe, because
  // every mount in this file now reads these signals.
  mockSignals = INITIAL_SESSION_SIGNALS;
  mockFrameHandler = null;
  vi.mocked(sendSessionMessage).mockClear();
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

// hook builds one oap:generative node — the agent's writable region. Every
// fixture below that needs an ADDRESSABLE region declares one, because
// data-hook is what names a region in the DOM and only a hook carries it.
// allowedComponents ["*"] because these fixtures are about rendering, not
// about what an agent may put where (uicomponents owns that).
function hook(name: string, children?: unknown[]) {
  return { component: "oap:generative", props: { name, allowedComponents: ["*"] }, children };
}

// The hook regions are the VIEW's own output: @ap/agentui's renderNode draws a
// <section data-hook="…"> for every oap:generative node in the page tree, and a
// hook with no children renders as an empty-but-present region rather than
// being skipped. Asserting this on the view — inside the stand-in content
// region — is what keeps it a claim about rendering rather than about
// whichever host composes chrome around it.
describe("AgentUIView — every declared hook gets its own region", () => {
  const twoHookProps: ViewProps = {
    ns: "workshop",
    name: "sess-live",
    declaration: {
      view: {
        component: "ap:stack",
        children: [hook("root"), hook("sidebar")],
      },
    } as AgentUIViewProps["declaration"],
  };

  it("draws one region per declared hook", () => {
    renderView(twoHookProps);
    const region = screen.getByTestId(CONTENT_REGION);
    expect(region.querySelector('[data-hook="root"]')).toBeInTheDocument();
    expect(region.querySelector('[data-hook="sidebar"]')).toBeInTheDocument();
  });

  it("reflects a DIFFERENT declaration when given a different one (not a hardcoded placeholder)", () => {
    renderView({
      ...twoHookProps,
      declaration: { view: { component: "ap:stack", children: [hook("only-hook")] } } as AgentUIViewProps["declaration"],
    });
    const region = screen.getByTestId(CONTENT_REGION);
    expect(region.querySelector('[data-hook="only-hook"]')).toBeInTheDocument();
    expect(region.querySelector('[data-hook="sidebar"]')).toBeNull();
  });
});

// The per-hook updated cue is the whole of what this view says while an agent
// is at work. There is deliberately no page-wide "the agent is working on
// this…" spinner: an agent may answer a question entirely in prose and never
// touch the page, and a spinner on every writable region would then sit on a
// finished page claiming work nobody is doing. What a hook says instead is a
// fact after the event — this region just changed — keyed to the ONE region
// the frame named.
describe("AgentUIView — the per-hook updated cue is the floor", () => {
  const baseProps: ViewProps = { ns: "workshop", name: "sess-live", declaration: { view: { component: "ap:stack" } } };
  const twoHooks = {
    view: {
      component: "ap:stack",
      children: [
        hook("a", [{ component: "ap:text", props: { text: "alpha" } }]),
        hook("b", [{ component: "ap:text", props: { text: "beta" } }]),
      ],
    },
  } as AgentUIViewProps["declaration"];

  it("never shows a working spinner on every region", () => {
    renderView({ ...baseProps, declaration: twoHooks });
    expect(screen.queryByTestId("agent-ui-slot-working")).toBeNull();
    expect(screen.queryByText("The agent is working on this…")).toBeNull();
  });

  it("marks ONLY the hook a live view frame names as updated, then clears it", async () => {
    vi.useFakeTimers();
    try {
      const sock = renderViewWithLive({ ...baseProps, declaration: twoHooks });
      sock.frame({ type: "view", hook: "b", declaration: { ...twoHooks, agentComposed: ["b"] } });
      expect(document.querySelector('[data-hook="b"]')).toHaveAttribute("data-hook-updated", "true");
      expect(document.querySelector('[data-hook="a"]')).not.toHaveAttribute("data-hook-updated");
      await act(() => vi.advanceTimersByTimeAsync(HOOK_UPDATED_MS + 1));
      expect(document.querySelector('[data-hook="b"]')).not.toHaveAttribute("data-hook-updated");
    } finally {
      vi.useRealTimers();
    }
  });

  // A CLEAR shows the cue like any other write. This fixture empties a hook
  // that HAD children, so the declaration really does change — the row below is
  // the one that pins the ordering against the echo guard.
  it("shows the cue for a CLEAR too — an empty hook named by the frame", () => {
    const sock = renderViewWithLive({ ...baseProps, declaration: twoHooks });
    const cleared = {
      ...twoHooks,
      view: { ...twoHooks.view, children: [twoHooks.view.children![0], hook("b")] },
      agentComposed: ["b"],
    };
    sock.frame({ type: "view", hook: "b", declaration: cleared });
    expect(document.querySelector('[data-hook="b"]')).toHaveAttribute("data-hook-updated", "true");
    expect(document.querySelector('[data-hook="b"]')!.textContent).toContain("Updated");
  });

  // The ordering pin: the cue is marked BEFORE handleView's echo guard.
  //
  // This frame's declaration is BYTE-IDENTICAL to the one already applied —
  // the same object the view was bootstrapped with — so the guard returns
  // early and nothing downstream of it runs. That is the real shape of a
  // clear of an ALREADY-empty hook, and of a re-write that happens to
  // reproduce the current bytes: the agent acted, and the declaration cannot
  // say so. The frame's `hook` is the only evidence there is.
  //
  // It is distinguished from the row above precisely BECAUSE the bytes do not
  // move: that fixture drops `b`'s children and adds agentComposed, so it
  // passes the guard and stays green with markUpdated on either side of it.
  // Only this one fails when the call moves below the guard.
  it("marks the hook on a frame whose declaration is byte-identical — the cue is set before the echo guard", () => {
    const sock = renderViewWithLive({ ...baseProps, declaration: twoHooks });
    sock.frame({ type: "view", hook: "b", declaration: twoHooks });
    expect(document.querySelector('[data-hook="b"]')).toHaveAttribute("data-hook-updated", "true");
    // …and still only the named one: an echo frame must not spray the cue
    // across regions it did not name.
    expect(document.querySelector('[data-hook="a"]')).not.toHaveAttribute("data-hook-updated");
  });

  // live.go sends a `view` frame on EVERY connect, and that frame names no
  // hook because nothing changed — it is the browser catching up, not the
  // agent writing. Marking a region on it would put "Updated" on every region
  // of every page the moment a laptop woke up.
  it("does not mark anything on the open-time echo frame (no hook)", () => {
    const sock = renderViewWithLive({ ...baseProps, declaration: twoHooks });
    sock.frame({ type: "view", declaration: twoHooks });
    expect(document.querySelector("[data-hook-updated]")).toBeNull();
  });
});

// The composed-by-agent legend (the violet-region explainer that used to live
// here) was REMOVED, not hidden — see global-constraints.md's ruling 6. This
// row is what pins that: re-adding it later is a small, reviewable change,
// but nothing should reintroduce it silently.
it("AgentUIView no longer renders the removed composed-by-agent legend", () => {
  renderView({ ns: "workshop", name: "sess-live", declaration: { view: { component: "ap:heading", props: { text: "Fine", level: 1 } } } });
  expect(screen.queryByTestId("agent-ui-composed-by-agent-legend")).toBeNull();
  expect(screen.queryByText("Regions outlined in violet are composed by the agent")).toBeNull();
});

// A pending interaction_request (folded into signals.pendingInteractions by
// sessionSignals.ts) is the agent BLOCKED on the viewer — the same fact the
// chat's inline InteractionCard exists for. This view has no timeline to
// render one into, so the platform renders the SAME card (never a second
// "approval component" — InteractionCard already knows every category) in a
// region above the declared page, and posts through the SAME decision route
// via the shared useInteractionDecision hook (global-constraints.md's "one
// decision route").
describe("AgentUIView — a pending interaction shows in a 'Needs your decision' region", () => {
  const pendingRequest: InteractionRequestInner = {
    agentSessionRef: { namespace: "workshop", name: "sess-live" },
    category: "plan_amendment",
    requestRef: "req-1",
    lead: "The agent is asking to add workshop_apply to its approved plan.",
    actions: [{ id: "approve", label: "Approve", kind: "decision" }],
    audience: { scope: "requester" },
  };
  const viewProps: ViewProps = {
    ns: "demo-ns",
    name: "demo-session",
    declaration: { view: { component: "ap:heading", props: { text: "Fine", level: 1 } } },
  };

  it("renders the card's lead when a request is pending and the view is shown", () => {
    mockSignals = { ...INITIAL_SESSION_SIGNALS, pendingInteractions: [pendingRequest] };
    renderView(viewProps);
    const region = screen.getByTestId("agent-ui-decisions");
    expect(within(region).getByText(pendingRequest.lead)).toBeInTheDocument();
  });

  it("renders no region when the view is not shown", () => {
    mockSignals = { ...INITIAL_SESSION_SIGNALS, pendingInteractions: [pendingRequest] };
    renderView({ ...viewProps, shown: false });
    expect(screen.queryByTestId("agent-ui-decisions")).toBeNull();
  });

  it("renders no region when nothing is pending", () => {
    mockSignals = INITIAL_SESSION_SIGNALS;
    renderView(viewProps);
    expect(screen.queryByTestId("agent-ui-decisions")).toBeNull();
  });

  it("clicking the card's action posts category/requestRef/actionId to the session's decision route", async () => {
    mockSignals = { ...INITIAL_SESSION_SIGNALS, pendingInteractions: [pendingRequest] };
    const decisionCalls: { url: string; body: unknown }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init?: RequestInit) => {
        const u = String(url);
        if (u.endsWith("/decision")) {
          decisionCalls.push({ url: u, body: JSON.parse(String(init?.body ?? "{}")) });
          return { ok: true, json: async () => ({ ok: true }) } as Response;
        }
        if (u.endsWith("/actions")) return { ok: true, json: async () => ({ requestId: "r1", state: "submitted" }) } as Response;
        return { ok: true, json: async () => ({ bindings: {} }) } as Response;
      }),
    );

    renderView(viewProps);
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));

    await waitFor(() => expect(decisionCalls).toHaveLength(1));
    expect(decisionCalls[0].url).toBe("/sessions/api/demo-ns/demo-session/decision");
    expect(decisionCalls[0].body).toEqual({ category: "plan_amendment", requestRef: "req-1", actionId: "approve" });
  });
});

// channelsd can REJECT a decision — the click lacked standing, the request
// was already resolved, or the bound handler errored — and publishes that
// rejection as an interaction_decision_rejected frame on the session socket
// (see ChatView's own handling of the same frame). Unlike the chat timeline,
// this region has no append-only log to drop a new line into: the card IS
// the only place to decide, so a rejected click must show why AND let the
// viewer try again, rather than sitting disabled with no explanation (the
// live bug this fixes — a human clicked Approve twice on a card channelsd had
// refused both times, and saw nothing).
describe("AgentUIView — a rejected decision on the 'Needs your decision' card", () => {
  const pendingRequest: InteractionRequestInner = {
    agentSessionRef: { namespace: "demo-ns", name: "demo-session" },
    category: "plan_amendment",
    requestRef: "req-1",
    lead: "The agent is asking to add workshop_apply to its approved plan.",
    actions: [{ id: "approve", label: "Approve", kind: "decision" }],
    audience: { scope: "requester" },
  };
  const viewProps: ViewProps = {
    ns: "demo-ns",
    name: "demo-session",
    declaration: { view: { component: "ap:heading", props: { text: "Fine", level: 1 } } },
  };

  // rejectionFrame builds the double-nested wsFrame InteractionDecisionRejectedPayload
  // wire (see types.ts), overriding only the fields a row cares about — every
  // row starts from req-1's own handler_error, the class most calls will hit.
  function rejectionFrame(overrides: Record<string, unknown>): ChatFrame {
    return {
      type: "interaction_decision_rejected",
      session: { namespace: "demo-ns", name: "demo-session" },
      payload: {
        session: { namespace: "demo-ns", name: "demo-session" },
        payload: {
          agentSessionRef: { namespace: "demo-ns", name: "demo-session" },
          category: "plan_amendment",
          requestRef: "req-1",
          class: "handler_error",
          reason: "tool approval decision: bind failed",
          ...overrides,
        },
      },
    } as ChatFrame;
  }

  it("shows the rejection reason and re-enables the card's action for another try", () => {
    mockSignals = { ...INITIAL_SESSION_SIGNALS, pendingInteractions: [pendingRequest] };
    renderView(viewProps);

    // A first click latches InteractionCard's own `pending` state, disabling
    // every action on the card — the same disabling a successful decision
    // would leave in place until `applied` arrives.
    const approve = screen.getByRole("button", { name: "Approve" });
    fireEvent.click(approve);
    expect(approve).toBeDisabled();

    deliverFrame(rejectionFrame({ reason: "tool approval decision: bind failed" }));

    expect(screen.getByRole("alert")).toHaveTextContent("tool approval decision: bind failed.");
    // The card REMOUNTED (a fresh InteractionCard, its own `pending` reset to
    // false) rather than merely re-rendering the same instance — this is what
    // the attempt-counted key exists for.
    expect(screen.getByRole("button", { name: "Approve" })).toBeEnabled();
  });

  it("ignores a rejection for a requestRef this page isn't showing", () => {
    mockSignals = { ...INITIAL_SESSION_SIGNALS, pendingInteractions: [pendingRequest] };
    renderView(viewProps);

    deliverFrame(rejectionFrame({ requestRef: "req-unrelated", reason: "some other click" }));

    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByRole("button", { name: "Approve" })).toBeEnabled();
  });

  // already_resolved is informational, not a failure — someone else already
  // decided — but it is still worth showing here: a page silently sitting on
  // a card is exactly what a viewer cannot distinguish from "nothing has
  // happened yet".
  it("renders the already_resolved class's text too", () => {
    mockSignals = { ...INITIAL_SESSION_SIGNALS, pendingInteractions: [pendingRequest] };
    renderView(viewProps);

    deliverFrame(
      rejectionFrame({
        class: "already_resolved",
        originalDecider: { kind: "human", externalId: "u1", email: "otherperson@example.com" },
        originalOutcome: "approved",
      }),
    );

    expect(screen.getByRole("alert")).toHaveTextContent(
      "This request was already resolved by otherperson@example.com (approved).",
    );
  });
});

// sameChromeSignals is the mechanism this view's own doc and SessionShell both
// name as what closes the report loop (view effect -> host setState -> view
// re-render -> view effect). It cannot be reached by a mutation of the host,
// because each guard alone terminates the chain — the `useCallback` on the
// handler and this comparison are redundant with one another, and a mutation
// of either one survives. So it needs a DIRECT test, or the one comparison
// every host trusts is unobserved.
//
// The failure this pins: someone "simplifies" the comparison to check length
// only. Every render test stays green, but a view reporting [null] and then
// [{kind:"hard_error", id:"x"}] reads as unchanged, the host never
// re-renders, and chrome stops auto-revealing on a hard error — the one
// guarantee the whole hoist exists to relocate.
describe("sameChromeSignals — the report-loop guard", () => {
  const ev = { kind: "hard_error", id: "e1" } as const;
  const approval = { id: "a", label: "L", addressedToViewer: true };

  it.each([
    [
      "equal contents in different objects are the same report",
      { approvals: [{ ...approval }], trustEvents: [ev] },
      { approvals: [{ ...approval }], trustEvents: [{ ...ev }] },
      true,
    ],
    [
      "a changed label is a different report",
      { approvals: [{ ...approval }], trustEvents: [] },
      { approvals: [{ ...approval, label: "M" }], trustEvents: [] },
      false,
    ],
    [
      "a changed addressedToViewer is a different report",
      { approvals: [{ ...approval }], trustEvents: [] },
      { approvals: [{ ...approval, addressedToViewer: false }], trustEvents: [] },
      false,
    ],
    [
      "a source that went from holding nothing to holding a hard error is a different report",
      { approvals: [], trustEvents: [null] },
      { approvals: [], trustEvents: [ev] },
      false,
    ],
    [
      "a second event of the same kind under a new id is a different report",
      { approvals: [], trustEvents: [ev] },
      { approvals: [], trustEvents: [{ kind: "hard_error", id: "e2" } as const] },
      false,
    ],
    [
      "a different trust-event count is a different report",
      { approvals: [], trustEvents: [ev] },
      { approvals: [], trustEvents: [ev, ev] },
      false,
    ],
  ])("%s", (_name, a, b, want) => {
    expect(sameChromeSignals(a as ChromeSignals, b as ChromeSignals)).toBe(want);
    // Symmetric, because a host may compare in either direction depending on
    // which report arrived first.
    expect(sameChromeSignals(b as ChromeSignals, a as ChromeSignals)).toBe(want);
  });
});

// The lift, from the view's side: what the view REPORTS. The shell's side —
// that the report actually reaches chrome's approval surface — is
// pkg/web/webui/sessions/ui/SessionShell.test.tsx's, because neither half alone
// proves the seam is live.
describe("AgentUIView — reports its chrome signals up rather than rendering chrome", () => {
  it("renders no chrome of its own", () => {
    renderView({
      ns: "workshop",
      name: "sess-live",
      declaration: { view: { component: "ap:heading", props: { text: "Fine", level: 1 } } },
    });
    expect(screen.queryByTestId("session-shell-identity")).toBeNull();
    expect(screen.queryByTestId("session-shell-approval-surface")).toBeNull();
    expect(screen.queryByTestId("session-shell-chrome-toggle")).toBeNull();
  });

  it("reports a render failure as a hard_error trust event", async () => {
    const reports: unknown[] = [];
    renderView({
      ns: "workshop",
      name: "sess-live",
      declaration: { view: { component: "ap:not-a-real-component" } },
      onChromeSignals: (s) => reports.push(s),
    });
    await waitFor(() =>
      expect(reports.at(-1)).toMatchObject({
        trustEvents: [{ kind: "hard_error", id: "render-error:ap:not-a-real-component" }],
      }),
    );
  });

  // The report must SETTLE. A view that re-reports a fresh object every render
  // would drive its host's setState forever; the host's equality check is the
  // guard, but a view whose signals never stop changing would defeat it.
  it("stops reporting once its signals settle, rather than re-reporting every render", async () => {
    const reports: unknown[] = [];
    renderView({
      ns: "workshop",
      name: "sess-live",
      declaration: { view: { component: "ap:heading", props: { text: "Fine", level: 1 } } },
      onChromeSignals: (s) => reports.push(s),
    });
    await waitFor(() => expect(reports.length).toBeGreaterThan(0));
    const settled = reports.length;
    await new Promise((r) => setTimeout(r, 100));
    expect(reports.length).toBe(settled);
  });
});

describe("AgentUIView — rendering the declared page tree through @ap/agentui", () => {
  const baseProps: ViewProps = {
    ns: "workshop",
    name: "sess-live",
    declaration: { view: { component: "ap:stack" } },
  };

  // A representative Tier-0 page, fabricated for this test (not read from
  // examples/ — see AGENTS.md): a stack containing a heading, a grid of TWO
  // metrics, a card wrapping a table, and a markdown block. Every leaf below is
  // asserted on so a renderer that drops any one of them — not just the whole
  // tree — is caught.
  const nestedTreeDeclaration: AgentUIViewProps["declaration"] = {
    view: {
      component: "ap:stack",
      children: [
        { component: "ap:heading", props: { text: "Fleet Console", level: 1 } },
        {
          component: "ap:grid",
          props: { columns: 2 },
          children: [
            { component: "ap:metric", props: { label: "Open", value: "9" } },
            { component: "ap:metric", props: { label: "Closed", value: "3" } },
          ],
        },
        {
          component: "ap:card",
          props: { title: "Recent items" },
          children: [
            {
              component: "ap:table",
              props: {
                columns: [{ key: "name", header: "Name" }],
                rows: [{ name: "widget-a" }, { name: "widget-b" }],
              },
            },
          ],
        },
        { component: "ap:markdown", props: { body: "Refreshed **hourly**." } },
      ],
    },
  };

  it("renders every node of a nested declaration inside the content region", () => {
    renderView({ ...baseProps, declaration: nestedTreeDeclaration });
    const region = within(screen.getByTestId(CONTENT_REGION));

    expect(region.getByRole("heading", { name: "Fleet Console" })).toBeInTheDocument();
    expect(region.getByText("Open")).toBeInTheDocument();
    expect(region.getByText("9")).toBeInTheDocument();
    // ap:grid's SECOND child, inside ap:stack's SECOND child — a renderer
    // that only walked one level deep, or only the first child at each
    // level, would still pass every assertion above and fail only here.
    expect(region.getByText("Closed")).toBeInTheDocument();
    expect(region.getByText("3")).toBeInTheDocument();
    expect(region.getByText("Recent items")).toBeInTheDocument();
    expect(region.getByText("widget-a")).toBeInTheDocument();
    expect(region.getByText("widget-b")).toBeInTheDocument();
    // ap:markdown is ap:stack's LAST child — proves the walk reaches the end
    // of the tree, not just the front.
    expect(region.getByText("hourly")).toBeInTheDocument();
  });

  // @ap/agentui's renderNode already guarantees this (renderNode.test.tsx: "falls
  // back to a visible error for an unknown component instead of rendering
  // nothing") — re-asserted here at the page level because a version-skewed
  // client (a browser tab running an older bundle than the server that
  // validated the declaration) is exactly the scenario this page exists to
  // survive without going blank.
  it("renders the visible error card for an unknown component instead of blanking the page", () => {
    renderView({ ...baseProps, declaration: { view: { component: "ap:not-a-real-component" } } });
    const region = within(screen.getByTestId(CONTENT_REGION));
    expect(region.getByText(/Cannot render this section/)).toBeInTheDocument();
    expect(region.getByText(/ap:not-a-real-component/)).toBeInTheDocument();
  });

  it("renders a hook with no children as an addressable, empty region, without throwing", () => {
    const decl = { view: { component: "ap:stack", children: [hook("empty-hook")] } } as AgentUIViewProps["declaration"];
    expect(() => renderView({ ...baseProps, declaration: decl })).not.toThrow();
    const marker = screen.getByTestId(CONTENT_REGION).querySelector('[data-hook="empty-hook"]');
    expect(marker).toBeInTheDocument();
    expect(marker).toBeEmptyDOMElement();
  });

  // The server-computed `answered` list (see viewmodel.go's declarationWireFor)
  // is what keeps a question the viewer already replied to from asking again
  // after a reload: the SAME ap:question the agent painted renders as already
  // sent rather than as a fresh form, with no client-side "did I send this"
  // state of its own.
  it("renders an ap:question in an answered hook as already sent, not as a fresh question", () => {
    const decl = {
      view: {
        component: "ap:stack",
        children: [hook("brief", [{ component: "ap:question", props: { prompt: "Ready?", submitLabel: "Confirm" } }])],
      },
      answered: ["brief"],
    } as AgentUIViewProps["declaration"];
    renderView({ ...baseProps, declaration: decl });
    const region = within(screen.getByTestId(CONTENT_REGION));
    expect(region.getByTestId("ap-question-sent")).toHaveTextContent("Sent — waiting for the agent.");
    expect(region.queryByRole("textbox")).toBeNull();
  });

  // The structural guarantee — chrome regions are DOM siblings of the content
  // container, never descendants of it — moved to
  // pkg/web/webui/sessions/ui/SessionShell.test.tsx with the host that composes
  // them. It is asserted there against this same deep tree, because a
  // guarantee about what agent-authored content cannot reach is worth nothing
  // if it is only ever tested against an empty placeholder.
});


// The Go->React bootstrap seam. `readBootstrap` is an unchecked
// `JSON.parse(...) as T`, so a json-tag rename on the Go side turns a prop
// into `undefined` at runtime with nothing failing anywhere: a review proved
// renaming `sessionOrigin` -> `origin` (Go assertion updated with it) left the
// Go suite AND the whole frontend suite green while the shipped page silently
// lost its session disclosure. testdata/props.golden.json is the one artifact
// both sides read: page.go's own build output is asserted against it
// (props_golden_internal_test.go), and every row below renders THAT file, so a
// key present under a different name than this type declares fails here.
//
// The golden carries exactly the three fields ViewProps does — ns, name, and
// the declaration. The chrome fields it used to carry (subject, the session
// disclosure, the requested initial state) belong to the shell now, and its own
// golden (pkg/web/webui/sessions/ui/testdata/shell.golden.json) pins them through
// the component that renders them. Both goldens exist because both producers
// do.
describe("AgentUIView — the golden view props ViewFor actually emits", () => {
  const golden = goldenProps as unknown as ViewProps;

  it("addresses the session the golden names", () => {
    // Read BY KEY, not through a render: nothing the view draws displays ns or
    // name, so a rename of either would otherwise be invisible here — and both
    // are what every binding, action and live request this view makes is
    // addressed to.
    expect(golden.ns).toBe("workshop");
    expect(golden.name).toBe("sess-live");
  });

  it("renders the golden `declaration`'s node tree in the content region", () => {
    renderView(golden);
    expect(within(screen.getByTestId(CONTENT_REGION)).getByRole("heading", { name: "Fleet Console" })).toBeInTheDocument();
  });
});

// The action-table join: testdata/props.actions.golden.json is produced
// by ViewFor and asserted against there
// (props_golden_internal_test.go's TestShellBuildActionsGolden). Rendering it
// HERE through AgentUIViewProps is what makes a json-tag rename on
// either side fail the other side's suite.
//
// A render-only check is NOT enough to span this particular join: no control
// consults the action table's TOOL or ARGS to render anything — a live
// ap:button reads only the action NAME (registry.tsx's ActionButton) — so a
// coordinated rename of the wire's "actions", "tool", or "args" key (the Go
// producer AND this golden regenerated together) leaves a render-only
// assertion green. The first row below reads every field BY NAME through
// @ap/agentui's own `Declaration.actions` shape, independently of rendering,
// which is what actually catches that rename: a mutation test proved it, and
// proved that OMITTING a field from these assertions leaves it unprotected —
// `args` used to be omitted here, and a coordinated `args` -> `argsTemplate`
// rename survived both suites.
//
// toMatchObject is a SUBSET match, so a field it does not name is not
// asserted. Anything this join must protect has to be named explicitly.
describe("AgentUIView — the golden action-table props ViewFor actually emits (J3)", () => {
  const golden = actionsGolden as unknown as ViewProps;

  it("carries the full action table onto the wire, read by name from the golden", () => {
    expect(golden.declaration.actions?.map((a) => a.name)).toEqual(["advance", "archive"]);
    expect(golden.declaration.actions?.[0]).toMatchObject({ tool: "demo_advance_stage", inputs: ["note"] });
    expect(golden.declaration.actions?.[0]?.args).toEqual({ id: "42" });
    expect(golden.declaration.actions?.[1]).toMatchObject({ name: "archive", tool: "demo_archive_stage" });
  });

  it("renders the button that names one of the golden's declared actions", () => {
    renderView(golden);
    expect(screen.getByRole("button", { name: /advance/i })).toBeInTheDocument();
  });
});

// The view-model join: testdata/props.viewmodel.golden.json is
// produced by ViewFor and asserted against there
// (props_golden_internal_test.go's TestShellBuildViewModelGolden), for a page
// with one agent-composed hook ("panel") and one author-owned heading. Rendering
// it HERE through AgentUIViewProps is what makes a rename of the agentComposed
// json tag on either side fail the other side's suite — the same discipline as
// the actions golden above.
describe("AgentUIView — the golden view-model props ViewFor actually emits (J3)", () => {
  const golden = viewModelGolden as unknown as ViewProps;

  it("marks the agent-composed hook and only that hook", () => {
    renderView(golden);
    const panel = document.querySelector('[data-hook="panel"]')!;
    expect(panel).toHaveAttribute("data-agent-composed", "true");
    // Nothing ELSE on the page wears the mark. The golden's other region is an
    // author-owned heading, and a marker that spread beyond the named hook
    // would claim the agent wrote the whole page.
    expect(document.querySelectorAll("[data-agent-composed]")).toHaveLength(1);
  });

  it("reads agentComposed by key, not only by render", () => {
    // A render-only assertion would survive a coordinated rename in which the
    // Go tag and the TS field both moved; reading the key directly out of the
    // shared file is what makes the pin real. agentComposed is a top-level list
    // of hook NAMES — never a field on a node — so an agent cannot mark its own
    // work.
    const decl = (viewModelGolden as { declaration: { agentComposed?: string[] } }).declaration;
    expect(decl.agentComposed).toEqual(["panel"]);
  });
});

// The visibility guarantee: nothing the agent does on this view is silent.
//
// Two floors sit under the declared page, and which one applies is decided
// from the TREE, never from a flag anyone sets. A question the agent asked in
// prose surfaces as a modal — unless the page already asks it, in which case
// the agent's own card wins and the hook holding it wears the waiting cue. A
// running turn surfaces as a default progress region — unless the page already
// declares one.
describe("AgentUIView — the visibility guarantee", () => {
  const baseProps: ViewProps = { ns: "workshop", name: "sess-live", declaration: { view: { component: "ap:stack" } } };
  const twoHooks = {
    view: {
      component: "ap:stack",
      children: [
        hook("a", [{ component: "ap:text", props: { text: "alpha" } }]),
        hook("b", [{ component: "ap:text", props: { text: "beta" } }]),
      ],
    },
  } as AgentUIViewProps["declaration"];

  // withQuestionIn / withProgressIn put the agent's OWN card inside one named
  // hook of the same two-hook page, so each row differs from the one above it
  // by exactly the fact under test: what is on the page.
  function withQuestionIn(hookName: string): AgentUIViewProps["declaration"] {
    return {
      view: {
        component: "ap:stack",
        children: (twoHooks!.view.children ?? []).map((c) =>
          (c as { props?: { name?: string } }).props?.name === hookName
            ? hook(hookName, [{ component: "ap:question", props: { prompt: "Which repo?" } }])
            : c,
        ),
      },
    } as AgentUIViewProps["declaration"];
  }

  function withProgressIn(hookName: string): AgentUIViewProps["declaration"] {
    return {
      view: {
        component: "ap:stack",
        children: (twoHooks!.view.children ?? []).map((c) =>
          (c as { props?: { name?: string } }).props?.name === hookName
            ? hook(hookName, [{ component: "ap:progress", props: { now: "Reading the repository" } }])
            : c,
        ),
      },
    } as AgentUIViewProps["declaration"];
  }

  const awaiting: SessionSignals = {
    ...INITIAL_SESSION_SIGNALS,
    turnActive: false,
    awaitingReply: true,
    lastAgentReply: { text: "Which repo?", seq: 1 },
    replySeq: 1,
  };

  it("shows the reply modal when the agent asked in prose and no ap:question is on the page", () => {
    mockSignals = awaiting;
    renderView({ ...baseProps, declaration: twoHooks });
    expect(screen.getByTestId("agent-ui-reply-modal")).toHaveTextContent("Which repo?");
  });

  it("does NOT show the modal when a hook shows an ap:question — the generative path wins", () => {
    mockSignals = awaiting;
    renderView({ ...baseProps, declaration: withQuestionIn("b") });
    expect(screen.queryByTestId("agent-ui-reply-modal")).toBeNull();
    expect(document.querySelector('[data-hook="b"]')).toHaveAttribute("data-hook-waiting", "true");
    // …and only the hook that holds it. A cue on every region would say the
    // whole page is waiting on the viewer when one card is.
    expect(document.querySelector('[data-hook="a"]')).not.toHaveAttribute("data-hook-waiting");
  });

  function withNoticeIn(hookName: string): AgentUIViewProps["declaration"] {
    return {
      view: {
        component: "ap:stack",
        children: (twoHooks!.view.children ?? []).map((c) =>
          (c as { props?: { name?: string } }).props?.name === hookName
            ? hook(hookName, [{ component: "ap:notice", props: { body: "Test session started — I'll keep watching.", buttons: [{ label: "Done testing", action: "test_done" }] } }])
            : c,
        ),
      },
    } as AgentUIViewProps["declaration"];
  }

  it("does NOT show the modal when a hook shows an ap:notice — the agent told, it did not ask", () => {
    mockSignals = awaiting;
    renderView({ ...baseProps, declaration: withNoticeIn("b") });
    expect(screen.queryByTestId("agent-ui-reply-modal")).toBeNull();
    expect(screen.getByTestId("ap-notice")).toBeInTheDocument();
    // A notice is not a question: no hook wears the waiting cue for it.
    expect(document.querySelector('[data-hook="b"]')).not.toHaveAttribute("data-hook-waiting");
  });

  it("keeps the modal closed for a notice the viewer dismissed: the agent still declared it", () => {
    window.localStorage.setItem(
      "agentui:notice:dismissed:" +
        noticeIdentity({ component: "ap:notice", props: { body: "Test session started — I'll keep watching.", buttons: [{ label: "Done testing", action: "test_done" }] } }),
      "1",
    );
    mockSignals = awaiting;
    renderView({ ...baseProps, declaration: withNoticeIn("b") });
    expect(screen.queryByTestId("ap-notice")).toBeNull();
    expect(screen.queryByTestId("agent-ui-reply-modal")).toBeNull();
    window.localStorage.clear();
  });

  // The shell keeps this view MOUNTED behind `hidden` once it has been opened,
  // and a radix dialog renders through a portal to document.body — which no
  // ancestor's display:none can contain. So the modal has to be told whether
  // its own view is the one on screen: without that clause a hidden view would
  // put a focus-trapping dialog over the transcript, which already shows the
  // reply and has a composer of its own.
  it("does not open the modal while this view is not the one on screen", () => {
    mockSignals = awaiting;
    renderView({ ...baseProps, declaration: twoHooks, shown: false });
    expect(screen.queryByTestId("agent-ui-reply-modal")).toBeNull();
  });

  it("does not show the modal for a turn that simply completed", () => {
    mockSignals = { ...awaiting, awaitingReply: false };
    renderView({ ...baseProps, declaration: twoHooks });
    expect(screen.queryByTestId("agent-ui-reply-modal")).toBeNull();
  });

  // An ANSWERED question is not an asking question. `answered` is
  // server-computed (declarationWireFor, from the fill's write time against
  // the transcript's visible user entries), and the two fallbacks this block
  // is about both read it: the hook stops wearing "Waiting on you", and the
  // reply modal stops treating a sent card as the page still asking.
  //
  // Without the subtraction, a viewer who answered the brief's question and
  // got the agent's NEXT question in prose sees a card reading "Sent — waiting
  // for the agent." and nothing asking anything, with the modal suppressed by
  // the very card they already answered. The chat tab is the only escape, and
  // chrome may be collapsed.
  describe("an answered question stops counting as the page asking", () => {
    const briefOnly = {
      view: { component: "ap:stack", children: [hook("brief", [{ component: "ap:question", props: { prompt: "Which repo?" } }])] },
      answered: ["brief"],
    } as AgentUIViewProps["declaration"];

    it("drops the waiting cue from the hook whose question was answered", () => {
      mockSignals = awaiting;
      renderView({ ...baseProps, declaration: briefOnly });
      expect(document.querySelector('[data-hook="brief"]')).not.toHaveAttribute("data-hook-waiting");
    });

    it("opens the reply modal for the agent's next question asked in prose", () => {
      mockSignals = awaiting;
      renderView({ ...baseProps, declaration: briefOnly });
      expect(screen.getByTestId("agent-ui-reply-modal")).toHaveTextContent("Which repo?");
    });

    it("keeps the modal shut while ANOTHER hook's question is still unanswered", () => {
      mockSignals = awaiting;
      renderView({
        ...baseProps,
        declaration: {
          view: {
            component: "ap:stack",
            children: [
              hook("brief", [{ component: "ap:question", props: { prompt: "Which repo?" } }]),
              hook("b", [{ component: "ap:question", props: { prompt: "And which branch?" } }]),
            ],
          },
          answered: ["brief"],
        } as AgentUIViewProps["declaration"],
      });
      expect(screen.queryByTestId("agent-ui-reply-modal")).toBeNull();
      expect(document.querySelector('[data-hook="b"]')).toHaveAttribute("data-hook-waiting", "true");
      expect(document.querySelector('[data-hook="brief"]')).not.toHaveAttribute("data-hook-waiting");
    });
  });

  it("folds a hook bound to a finished step and keeps the active step's hook open", () => {
    mockSignals = INITIAL_SESSION_SIGNALS;
    const timeline = { component: "ap:steps", props: { pinned: true, steps: [{ id: "assess", label: "Intake", state: "done" }, { id: "tools", label: "Tools", state: "active" }] } };
    const boundHook = (name: string, step: string, text: string, title?: string) => ({
      component: "oap:generative", props: { name, allowedComponents: ["*"], step, ...(title !== undefined ? { title } : {}) }, children: [{ component: "ap:text", props: { text } }],
    });
    renderView({
      ...baseProps,
      declaration: { view: { component: "ap:stack", children: [timeline, boundHook("brief", "assess", "the brief", "Brief"), boundHook("tools", "tools", "the tools")] } } as AgentUIViewProps["declaration"],
    });
    const brief = document.querySelector('[data-hook="brief"]') as HTMLElement;
    expect(brief).toHaveAttribute("data-hook-collapsed", "true");
    expect(brief.querySelector("details")).not.toHaveAttribute("open");
    expect(brief.querySelector("summary")).toHaveTextContent("Brief");
    expect(document.querySelector('[data-hook="tools"]')).not.toHaveAttribute("data-hook-collapsed");
    expect(screen.getByTestId("agent-ui-steps-pinned")).toBeInTheDocument();
  });

  it("does not show the modal once the session is over — there is nobody left to answer", () => {
    mockSignals = { ...awaiting, ended: true };
    renderView({ ...baseProps, declaration: twoHooks });
    expect(screen.queryByTestId("agent-ui-reply-modal")).toBeNull();
  });

  it("replying from the modal posts to the transcript route and closes it", async () => {
    mockSignals = awaiting;
    renderView({ ...baseProps, declaration: twoHooks });
    fireEvent.change(screen.getByLabelText("Your reply"), { target: { value: "org/repo" } });
    fireEvent.click(screen.getByRole("button", { name: "Reply" }));
    await waitFor(() => expect(sendSessionMessage).toHaveBeenCalledWith(baseProps.ns, baseProps.name, "org/repo"));
    await waitFor(() => expect(screen.queryByTestId("agent-ui-reply-modal")).toBeNull());
  });

  // Dismissing is per-REPLY, not per-session: the seq the view remembers is
  // the one it dismissed, and the next question carries a higher one. A latch
  // that simply recorded "dismissed" would leave every later question silent.
  it("stays shut for the reply that was waved off, and opens again for the next one", async () => {
    mockSignals = awaiting;
    const { rerender } = renderView({ ...baseProps, declaration: twoHooks });
    fireEvent.click(screen.getByRole("button", { name: "Later" }));
    await waitFor(() => expect(screen.queryByTestId("agent-ui-reply-modal")).toBeNull());

    mockSignals = { ...awaiting, lastAgentReply: { text: "And which branch?", seq: 2 }, replySeq: 2 };
    rerender(
      <div data-testid={CONTENT_REGION}>
        <AgentUIView shown {...baseProps} declaration={twoHooks} />
      </div>,
    );
    expect(screen.getByTestId("agent-ui-reply-modal")).toHaveTextContent("And which branch?");
  });

  // The runner's idle exit (signals.pausedIdle): the agent replied and
  // stopped without asking anything at all. Before this, nothing rendered on
  // this view for it — the reply landed only in the transcript, and the page
  // looked dead. Same modal, "paused" variant, same dismissedSeq bookkeeping
  // as the "asked" case above.
  describe("the reply modal opens for a paused idle exit, not only an ask", () => {
    const paused: SessionSignals = {
      ...INITIAL_SESSION_SIGNALS,
      turnActive: false,
      awaitingReply: false,
      pausedIdle: true,
      lastAgentReply: { text: "I'll move on to the tools now.", seq: 1 },
      replySeq: 1,
    };

    it("opens in the paused variant with the agent's reply", () => {
      mockSignals = paused;
      renderView({ ...baseProps, declaration: twoHooks });
      const modal = screen.getByTestId("agent-ui-reply-modal");
      expect(modal).toHaveAttribute("data-variant", "paused");
      expect(modal).toHaveTextContent("I'll move on to the tools now.");
    });

    it("does not show the modal once the session is over", () => {
      mockSignals = { ...paused, ended: true };
      renderView({ ...baseProps, declaration: twoHooks });
      expect(screen.queryByTestId("agent-ui-reply-modal")).toBeNull();
    });

    it("keeps the modal shut while an ap:question is on the page", () => {
      mockSignals = paused;
      renderView({ ...baseProps, declaration: withQuestionIn("b") });
      expect(screen.queryByTestId("agent-ui-reply-modal")).toBeNull();
    });

    it("sends what was typed under Continue and closes the modal", async () => {
      mockSignals = paused;
      renderView({ ...baseProps, declaration: twoHooks });
      fireEvent.change(screen.getByLabelText("Your reply"), { target: { value: "go on" } });
      fireEvent.click(screen.getByRole("button", { name: "Continue" }));
      await waitFor(() => expect(sendSessionMessage).toHaveBeenCalledWith(baseProps.ns, baseProps.name, "go on"));
      await waitFor(() => expect(screen.queryByTestId("agent-ui-reply-modal")).toBeNull());
    });

    it("stays shut on a re-render carrying the same reply seq", async () => {
      mockSignals = paused;
      const { rerender } = renderView({ ...baseProps, declaration: twoHooks });
      fireEvent.click(screen.getByRole("button", { name: "Later" }));
      await waitFor(() => expect(screen.queryByTestId("agent-ui-reply-modal")).toBeNull());

      rerender(
        <div data-testid={CONTENT_REGION}>
          <AgentUIView shown {...baseProps} declaration={twoHooks} />
        </div>,
      );
      expect(screen.queryByTestId("agent-ui-reply-modal")).toBeNull();
    });
  });

  it("shows the default progress region while the turn runs and hides it when a hook shows ap:progress", () => {
    mockSignals = { ...INITIAL_SESSION_SIGNALS, turnActive: true, activity: { compactLine: "Connecting GitHub" } };
    renderView({ ...baseProps, declaration: twoHooks });
    expect(screen.getByTestId("agent-ui-progress-now")).toHaveTextContent("Connecting GitHub");
    cleanup();
    renderView({ ...baseProps, declaration: withProgressIn("a") });
    expect(screen.queryByTestId("agent-ui-default-progress")).toBeNull();
    expect(screen.getByTestId("ap-progress")).toBeInTheDocument();
  });

  it("answering an ap:question posts the answer to the transcript route", async () => {
    mockSignals = awaiting;
    renderView({ ...baseProps, declaration: withQuestionIn("b") });
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "notebot" } });
    fireEvent.click(screen.getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(sendSessionMessage).toHaveBeenCalledWith(baseProps.ns, baseProps.name, "notebot"));
  });

  it("provides its own session to the page, so an ap:attachment is looked up in it and nowhere else", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) =>
      String(input).startsWith("/artifact-view/meta")
        ? new Response(JSON.stringify({ name: "n", filename: "f.oap", size: 1, mime: "application/octet-stream" }), { status: 200, headers: { "Content-Type": "application/json" } })
        : new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } }),
    );
    vi.stubGlobal("fetch", fetchMock);
    mockSignals = INITIAL_SESSION_SIGNALS;
    renderView({ ...baseProps, declaration: { view: { component: "ap:stack", children: [hook("deliver", [{ component: "ap:attachment", props: { artifact: "artifact-0123456789abcdef" } }])] } } as AgentUIViewProps["declaration"] });
    await screen.findByTestId("ap-attachment");
    expect(fetchMock).toHaveBeenCalledWith("/artifact-view/meta?artifactId=artifact-0123456789abcdef&sessionRef=workshop%2Fsess-live", expect.anything());
  });
});

describe("the view root's width", () => {
  it("is the wide column for a rail page", async () => {
    renderView({
      ns: "workshop",
      name: "sess-live",
      declaration: {
        view: {
          component: "oap:page",
          props: { layout: "rail" },
          children: [{ component: "ap:heading", props: { text: "Fine", level: 1 } }],
        },
      } as AgentUIViewProps["declaration"],
    });
    const root = await screen.findByTestId("agent-ui-root");
    expect(root.className).toContain("max-w-6xl");
    expect(root.className).not.toContain("max-w-3xl");
  });

  it("stays the default column for every other page", async () => {
    renderView({ ns: "workshop", name: "sess-live", declaration: { view: { component: "ap:stack" } } });
    const root = await screen.findByTestId("agent-ui-root");
    expect(root.className).toContain("max-w-3xl");
    expect(root.className).not.toContain("max-w-6xl");
  });
});

describe("controls while the agent's turn is active", () => {
  const declaration = {
    actions: [{ name: "describe_agent", prompt: "The person said: {description}", inputs: ["description"] }],
    view: {
      component: "ap:stack",
      children: [
        hook("intake", [
          { component: "ap:form", props: { action: "describe_agent", submitLabel: "Start building", fields: [{ name: "description", label: "What?" }] } },
        ]),
      ],
    },
  } as AgentUIViewProps["declaration"];

  it("disables the page's controls while a turn is active, and re-enables when it ends", () => {
    mockSignals = { ...INITIAL_SESSION_SIGNALS, turnActive: true };
    const { rerender } = renderView({ ns: "demo-ns", name: "demo-session", declaration });
    expect(screen.getByRole("button", { name: "Start building" })).toBeDisabled();
    mockSignals = { ...INITIAL_SESSION_SIGNALS, turnActive: false };
    rerender(
      <div data-testid={CONTENT_REGION}>
        <AgentUIView shown ns="demo-ns" name="demo-session" declaration={declaration} />
      </div>,
    );
    expect(screen.getByRole("button", { name: "Start building" })).toBeEnabled();
  });
});
