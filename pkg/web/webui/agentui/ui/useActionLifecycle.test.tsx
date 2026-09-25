import "@testing-library/jest-dom/vitest";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { SessionShell, type ShellBootstrapProps } from "../../sessions/ui/SessionShell";
import type { SelectedView } from "../../sessions/ui/types";
import shellGoldenRaw from "../../sessions/ui/testdata/shell.golden.json";
import type { Declaration } from "@ap/agentui";
import { useActionLifecycle, type LiveActionEntry, type LiveActionMessage } from "./useActionLifecycle";
import liveGolden from "./testdata/live.golden.json";

// The shell holds ONE session socket for the selected session, above both
// views (chat/ui's SessionSocket), and this suite renders the REAL shell — so
// that socket is opened here too. It is mocked away because the FakeSocket
// below stands in for the AGENT-UI live channel specifically: a second
// stand-in on the same global would be the instance this file's `latest()`
// reaches for, and every frame would be delivered to the wrong connection.
vi.mock("../../chat/ui/useChatSocket", () => ({
  useChatSocket: () => ({ state: "connected" }),
}));

// This suite drives the lifecycle through the REAL host — the session shell —
// because the lifecycle's whole point is that a live signal reaches chrome:
// an approval addressed to the viewer must auto-reveal it, and a control's
// disabled state must survive the same frames. Rendering the view alone would
// leave every chrome assertion below untestable, and rendering a stand-in host
// would prove only that the stand-in works.
//
// PageFixture is the (ns, name, chrome, declaration) quadruple each fixture in
// this file actually varies; shellPropsFor lifts one into the shell's own
// bootstrap props, taking everything else (the session list, the subject, the
// notices) from the shell's golden so no fixture here hand-builds shell props.
type PageFixture = {
  ns: string;
  name: string;
  chrome?: { initialState?: string };
  declaration: unknown;
};

const shellGolden = shellGoldenRaw as unknown as ShellBootstrapProps;

function shellPropsFor(f: PageFixture): ShellBootstrapProps {
  const selected = shellGolden.selected as SelectedView;
  return {
    ...shellGolden,
    selected: {
      ...selected,
      ns: f.ns,
      name: f.name,
      chrome: f.chrome ?? {},
      view: {
        ...selected.view,
        kind: "agent-ui",
        ui: { ns: f.ns, name: f.name, declaration: f.declaration },
      },
    },
  } as unknown as ShellBootstrapProps;
}

function renderPage(f: PageFixture) {
  return render(<SessionShell {...shellPropsFor(f)} />);
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

// The frames this suite delivers are the REAL bytes GET .../live writes,
// captured into testdata/live.golden.json by
// pkg/web/webui/agentui/live_golden_internal_test.go. Hand-written literals were
// what made this seam unpinned: a one-sided json-tag rename on live.go left
// every Go test green (they decode through the same Go struct) AND every test
// here green (they built their own frames), while in production
// approvalAddressedToViewer arrived undefined and chrome stopped auto-revealing.
// liveGolden.test.tsx asserts the file's fields BY NAME; this file asserts what
// the REAL page does when handed those exact bytes.
const goldenSnapshot = liveGolden.snapshot as LiveActionMessage;
const goldenApprovalEvent = liveGolden.approvalEvent as LiveActionMessage;
const goldenSettledEvent = liveGolden.settledEvent as LiveActionMessage;

// GOLDEN_ACTION is the action name every golden frame carries. The fixture
// declaration below names the same action so the frames drive a real control
// rather than landing on a key nothing renders.
const GOLDEN_ACTION = (goldenApprovalEvent.action as LiveActionEntry).action;

// snapshotOf builds a snapshot frame around arbitrary entries WITHOUT
// re-spelling live.go's field names: the envelope comes from the golden's own
// snapshot frame and the entries are golden entries (optionally with one field
// overridden), so this helper cannot drift from the wire the way a literal can.
function snapshotOf(entries: LiveActionEntry[]): LiveActionMessage {
  return { ...goldenSnapshot, actions: entries };
}

// A declaration whose chrome is REQUESTED COLLAPSED. That is load-bearing for
// the J1 test: chrome that was already visible would pass the assertion
// without anything having revealed it.
const declaration = {
  actions: [{ name: GOLDEN_ACTION, tool: "crm_advance_stage", inputs: [] }],
  view: {
    component: "ap:stack",
    children: [{ component: "ap:button", props: { label: "Advance", action: GOLDEN_ACTION } }],
  },
};

const props: PageFixture = {
  ns: "workshop",
  name: "sess-live",
  chrome: { initialState: "collapsed" },
  declaration,
};

// The chrome regions the shell composes around this view. No composed-by-agent
// legend appears in either list: the marker an agent-composed hook wears stands
// alone, and nothing renders a legend for it.
const chromeRegionTestIds = [
  "session-shell-identity",
  "session-shell-session-origin",
  "session-shell-chat-escape-hatch",
  "session-shell-approval-surface",
  "session-shell-sidebar",
];

// FakeSocket stands in for the real WebSocket the page opens against GET
// .../live. jsdom has no WebSocket global at all (see
// pkg/web/webui/chat/ui/useChatSocket.test.ts and pkg/web/webui/sessionview/ui/
// SessionView.test.tsx for the same fact), so tests that want a live
// connection install this in place of the global.
class FakeSocket {
  static instances: FakeSocket[] = [];
  url: string;
  onopen: (() => void) | null = null;
  onmessage: ((ev: { data: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  onclose: (() => void) | null = null;
  close = vi.fn();
  constructor(url: string) {
    this.url = url;
    FakeSocket.instances.push(this);
  }
}

// stubSocket installs FakeSocket as the global WebSocket and returns a handle
// a test uses to deliver frames exactly as live.go would. `frame` delivers one
// frame's raw JSON — every caller passes a golden frame, so no test here
// re-spells a wire field name. `drop` closes the connection the way a laptop
// sleep or a webd rollout does, which makes the hook schedule a reconnect;
// `sockets` exposes how many connections have been opened so a test can wait
// for that reconnect to actually happen.
function stubSocket() {
  FakeSocket.instances = [];
  vi.stubGlobal("WebSocket", FakeSocket);
  const latest = () => FakeSocket.instances[FakeSocket.instances.length - 1];
  return {
    frame(msg: LiveActionMessage | Record<string, unknown>) {
      latest()?.onmessage?.({ data: JSON.stringify(msg) });
    },
    opened() {
      latest()?.onopen?.();
    },
    drop() {
      latest()?.onclose?.();
    },
    sockets: () => FakeSocket.instances.length,
  };
}

// deferredActionsFetch stubs fetch so /actions hangs until the test resolves
// it, and /bindings answers immediately. Holding the POST open is the only way
// to construct the orderings this suite is about: a socket frame that lands
// while the synchronous answer is still in flight, and two invocations
// dispatched before the first request returns.
function deferredActionsFetch() {
  let resolveWith: (body: { requestId: string; state: string; message?: string }) => void = () => {};
  const pending = new Promise<{ requestId: string; state: string; message?: string }>((res) => {
    resolveWith = res;
  });
  const fetchMock = vi.fn(async (url: string) =>
    String(url).endsWith("/actions")
      ? ({ ok: true, json: async () => pending } as unknown as Response)
      : ({ ok: true, json: async () => ({ bindings: {} }) } as Response),
  );
  vi.stubGlobal("fetch", fetchMock);
  return {
    fetchMock,
    resolve: (body: { requestId: string; state: string; message?: string }) => resolveWith(body),
    actionsCalls: () => fetchMock.mock.calls.filter(([u]) => String(u).endsWith("/actions")).length,
  };
}

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) =>
      String(url).endsWith("/actions")
        ? ({ ok: true, json: async () => ({ requestId: "r1", state: "submitted" }) } as Response)
        : ({ ok: true, json: async () => ({ bindings: {} }) } as Response),
    ),
  );
});

describe("the action lifecycle, end to end through the real page", () => {
  it("clicking a control POSTs the declared action name", async () => {
    stubSocket();
    renderPage(props);
    await userEvent.click(screen.getByRole("button", { name: "Advance" }));
    await waitFor(() => {
      const call = (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.find(([u]) => String(u).endsWith("/actions"));
      expect(JSON.parse(String((call![1] as RequestInit).body)).action).toBe(GOLDEN_ACTION);
    });
  });

  it("disables the control once the POST reports submitted", async () => {
    stubSocket();
    renderPage(props);
    await userEvent.click(screen.getByRole("button", { name: "Advance" }));
    await waitFor(() => expect(screen.getByRole("button", { name: /Advance/ })).toBeDisabled());
  });

  // ===== J1: the spanning test =====
  //
  // A server-raised approval addressed to the VIEWER must auto-reveal chrome.
  // Every hop is real here: the frame is live.go's own captured bytes, the
  // page's socket handler receives it, useActionLifecycle stores it,
  // AgentUIView derives the TrustEvent and reports it up, the standalone page
  // hands it to Chrome, and Chrome.s shipped reducer reveals. Nothing in this
  // test hands Chrome a trustEvents prop — that is exactly the wiring that
  // once shipped untested.
  it("an approval ADDRESSED TO THE VIEWER auto-reveals collapsed chrome", async () => {
    const sock = stubSocket();
    renderPage(props);
    for (const id of chromeRegionTestIds) {
      expect(screen.getByTestId(id)).not.toBeVisible();
    }

    sock.frame(goldenApprovalEvent);

    await waitFor(() => {
      for (const id of chromeRegionTestIds) {
        expect(screen.getByTestId(id)).toBeVisible();
      }
    });
  });

  it("an approval addressed to SOMEONE ELSE does not reveal, and says so on the control", async () => {
    const sock = stubSocket();
    renderPage(props);
    const entry = goldenApprovalEvent.action as LiveActionEntry;
    sock.frame({ ...goldenApprovalEvent, action: { ...entry, approvalAddressedToViewer: false } });

    await waitFor(() => expect(screen.getByRole("button", { name: /Advance/ })).toBeDisabled());
    expect(screen.getByTestId("session-shell-identity")).not.toBeVisible();
  });

  it("a pending approval addressed to the viewer appears in the chrome approval surface", async () => {
    const sock = stubSocket();
    renderPage(props);
    sock.frame(goldenApprovalEvent);
    await waitFor(() => expect(screen.getByTestId("session-shell-approval-surface")).toHaveTextContent(/your approval/i));
  });

  it("a settled action re-evaluates the data bindings, so the panel re-renders", async () => {
    const sock = stubSocket();
    renderPage(props);
    const bindingsCalls = () => (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.filter(([u]) => String(u).endsWith("/bindings")).length;

    // useBindings' own first-mount POST is issued via a setTimeout(0) — a
    // macrotask, not synchronous with render() — so reading `before` right
    // after render() risks racing that deferred call rather than the settle
    // this test is actually about: `after > before` would then hold from the
    // mount's own call landing late, independent of whether onSettled ever
    // fires. Waiting for the first call to land before snapshotting `before`
    // is what makes the LATER assertion diagnostic of the settle, and only
    // the settle.
    await waitFor(() => expect(bindingsCalls()).toBeGreaterThanOrEqual(1));
    const before = bindingsCalls();

    sock.frame(goldenSettledEvent);

    await waitFor(() => expect(bindingsCalls()).toBeGreaterThan(before));
  });

  // Fresh mount, no click. The socket's FIRST frame is a snapshot carrying a
  // pending entry, exactly as live.go's reload arm sends it (memory is
  // authoritative; a browser that (re)connects mid-flight must learn the
  // pending state from the snapshot alone, with nothing clicked in this tab).
  it("a RELOAD mid-flight re-renders the pending state from the snapshot alone", async () => {
    const sock = stubSocket();
    renderPage(props);

    sock.frame(goldenSnapshot);

    await waitFor(() => expect(screen.getByRole("button", { name: /Advance/ })).toBeDisabled());
  });

  // The same reload arm, for an approval: the snapshot alone must both disable
  // the control and reveal chrome, with no event frame involved.
  it("a RELOAD mid-approval reveals chrome from the snapshot alone", async () => {
    const sock = stubSocket();
    renderPage(props);

    sock.frame(snapshotOf([goldenApprovalEvent.action as LiveActionEntry]));

    // Two separate waits, because these are two facts at opposite ends of a
    // three-commit chain, each gated on the previous commit's PASSIVE effects:
    // the snapshot's setStates disables the button; the view's reporting effect
    // then lifts the approval up as a trustEvent; only then does Chrome's own
    // trust-event effect dispatch and un-collapse. `waitFor` resolves on the
    // FIRST of those commits, so asserting the reveal synchronously after it
    // races two flushes that a loaded machine will not have run yet.
    //
    // This does not weaken the claim: a reveal that never fires still fails
    // here — verified by disabling the trust-event push, which reddens this row
    // either way. It only tolerates a reveal that is a tick late.
    await waitFor(() => expect(screen.getByRole("button", { name: /Advance/ })).toBeDisabled());
    await waitFor(() => expect(screen.getByTestId("session-shell-identity")).toBeVisible());
  });

  it("a settled action re-enables its control", async () => {
    const sock = stubSocket();
    renderPage(props);
    await userEvent.click(screen.getByRole("button", { name: "Advance" }));
    await waitFor(() => expect(screen.getByRole("button", { name: /Advance/ })).toBeDisabled());

    sock.frame(goldenSettledEvent);

    await waitFor(() => expect(screen.getByRole("button", { name: /Advance/ })).toBeEnabled());
  });

  // A dead socket must not brick every button on the page. The POST path
  // still works; only live updates are lost. Stubbed as a constructor that
  // THROWS — one real reason a live channel can be entirely unavailable (a
  // proxy that rejects the Upgrade outright) — rather than relying on jsdom
  // having no ambient WebSocket at all: this Node runtime ships a real
  // WebSocket global, so `new WebSocket(...)` would otherwise attempt (and
  // eventually fail) a genuine, asynchronous network connection instead of
  // failing synchronously the way this test needs to assert against.
  it("a socket that never opens leaves controls clickable, and logs", async () => {
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    vi.stubGlobal(
      "WebSocket",
      class {
        constructor() {
          throw new Error("connection refused");
        }
      },
    );
    renderPage(props);
    expect(errorSpy).toHaveBeenCalled();

    await userEvent.click(screen.getByRole("button", { name: "Advance" }));
    await waitFor(() => {
      const call = (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.find(([u]) => String(u).endsWith("/actions"));
      expect(call).toBeDefined();
    });
  });
});

// ===== J4: an update_view lands on an open page =====
//
// The server side (live.go's view arm) is covered by
// pkg/web/webui/agentui/live_test.go's TestLiveViewUpdatePushesWhatWEBDResolved.
// This describe block is the browser half: the SAME socket the view already
// opens for the action arm also carries `view` frames, and this hook's onView
// must swap the declaration, reconcile params, and trigger a bindings
// re-evaluation — through the real page, not a stand-in component handed a
// prop the test itself set.
describe("the view arm: a live update_view lands on an open page", () => {
  // A dedicated page tree: a param control ("span") outside every hook, and a
  // hook ("panel") a `view` frame will rewrite. Separate from this file's
  // shared `declaration` (an action button) so these tests exercise the view
  // arm in isolation from the action lifecycle.
  const panelHook = (children: unknown[]) => ({
    component: "oap:generative",
    props: { name: "panel", allowedComponents: ["*"] },
    children,
  });
  const viewDeclaration = {
    view: {
      component: "ap:stack",
      children: [
        {
          component: "ap:select",
          props: { param: "span", value: "7d", options: [{ value: "7d", label: "7 days" }, { value: "30d", label: "30 days" }] },
        },
        panelHook([{ component: "ap:text", props: { text: "original" } }]),
      ],
    },
  };
  const viewProps: PageFixture = {
    ns: "workshop",
    name: "sess-live",
    declaration: viewDeclaration,
  };

  const bindingsCalls = () => (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.filter(([u]) => String(u).endsWith("/bindings")).length;

  // The REAL bytes GET .../live writes for a ui_view_update push, captured by
  // live_golden_internal_test.go. Delivered VERBATIM below: nothing here
  // re-spells `declaration`, `hook`, or `agentComposed`, so a coordinated
  // rename (change the Go json tag AND regenerate this file) leaves the page
  // handed a frame it cannot read — handleFrame's guard rejects it, the
  // markdown never appears, and this test goes red. liveGolden.test.tsx
  // carries the field-name half; this one carries the consequence.
  it("the REAL pushed view frame swaps the declaration and marks the composed hook", async () => {
    const sock = stubSocket();
    renderPage(viewProps);
    await waitFor(() => expect(bindingsCalls()).toBeGreaterThanOrEqual(1));
    const before = bindingsCalls();

    sock.frame(liveGolden.viewUpdate as unknown as Record<string, unknown>);

    await waitFor(() => expect(screen.getByText("agent copy")).toBeVisible());
    // The mark is the hook region's own treatment, read via its data
    // attribute — see hooks.tsx's COMPOSED_CLASS.
    expect(document.querySelector('[data-agent-composed="true"]')).not.toBeNull();
    await waitFor(() => expect(bindingsCalls()).toBeGreaterThan(before));
  });

  // The open-time echo. live.go sends a `view` frame on EVERY connect, and on
  // a plain page load its declaration is byte-identical to the bootstrap prop
  // — both are the same declarationWire the same resolveView marshalled. This
  // test wires exactly that: the page is bootstrapped from the golden's own
  // open-time frame and then handed that frame.
  //
  // Applying it would bump bindingsGeneration, whose effect cleanup aborts
  // useBindings' in-flight mount fetch and re-issues it — and aborting does
  // NOT stop the server-side fan-out already dispatched, so every page load
  // and every reconnect would cost a second full binding resolution nothing
  // reads. Asserting a non-event, so the wait must outlast useBindings'
  // 250ms debounce.
  it("the open-time view frame is an echo: no second binding fan-out per page load", async () => {
    const sock = stubSocket();
    const openFrame = liveGolden.viewOpen as unknown as { declaration: unknown };
    renderPage({ ...viewProps, declaration: openFrame.declaration });
    await waitFor(() => expect(bindingsCalls()).toBe(1));

    sock.frame(liveGolden.viewOpen as unknown as Record<string, unknown>);
    await new Promise((r) => setTimeout(r, 400));
    expect(bindingsCalls()).toBe(1);

    // …and the guard is not "never bump": a frame that really does change the
    // declaration still re-evaluates.
    sock.frame(liveGolden.viewUpdate as unknown as Record<string, unknown>);
    await waitFor(() => expect(bindingsCalls()).toBeGreaterThan(1));
  });

  it("a live view update swaps the declaration and re-evaluates the bindings", async () => {
    const sock = stubSocket();
    renderPage(viewProps);
    await waitFor(() => expect(bindingsCalls()).toBeGreaterThanOrEqual(1));
    const before = bindingsCalls();

    sock.frame({
      type: "view",
      hook: "panel",
      declaration: {
        view: {
          ...viewDeclaration.view,
          children: [
            viewDeclaration.view.children[0],
            panelHook([{ component: "ap:markdown", props: { body: "agent copy" } }]),
          ],
        },
        agentComposed: ["panel"],
      },
    });

    await waitFor(() => expect(screen.getByText("agent copy")).toBeVisible());
    // The mark is the hook region's own treatment, read via its data
    // attribute — see hooks.tsx's COMPOSED_CLASS.
    expect(document.querySelector('[data-agent-composed="true"]')).not.toBeNull();
    await waitFor(() => expect(bindingsCalls()).toBeGreaterThan(before));
  });

  it("a live view update keeps the viewer's current parameter choices", async () => {
    const sock = stubSocket();
    renderPage(viewProps);
    await waitFor(() => expect(bindingsCalls()).toBeGreaterThanOrEqual(1));

    // The viewer picks 30d.
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "30d" } });
    await waitFor(() => {
      const call = (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.filter(([u]) => String(u).endsWith("/bindings")).at(-1);
      expect(JSON.parse(String((call![1] as RequestInit).body)).params).toEqual({ span: "30d" });
    });

    // The agent rewrites the UNRELATED "panel" hook. The declared control's
    // own literal default ("7d") is unchanged — if this reset `span`, every
    // agent turn would silently discard whatever the viewer had picked.
    sock.frame({
      type: "view",
      hook: "panel",
      declaration: {
        view: {
          ...viewDeclaration.view,
          children: [
            viewDeclaration.view.children[0],
            panelHook([{ component: "ap:markdown", props: { body: "agent copy" } }]),
          ],
        },
      },
    });

    await waitFor(() => expect(screen.getByText("agent copy")).toBeVisible());
    expect(screen.getByRole("combobox")).toHaveValue("30d");
  });

  // The frame is server-built (live.go always marshals a `view` tree), so
  // this is defence in depth — but a thrown parse error inside a socket
  // handler must not unmount the app: a blank page is a far worse failure
  // than a stale one. Skipped AND logged, never swallowed: the viewer keeps a
  // stale-but-visible page and the operator gets the frame that caused it.
  it.each([
    ["no `declaration` at all", { type: "view" }],
    ["a `view` that is not an object", { type: "view", declaration: { view: "not-a-tree" } }],
    ["a `view` that is null", { type: "view", declaration: { view: null } }],
    ["a `view` with no component", { type: "view", declaration: { view: { children: [] } } }],
  ])("a malformed view frame (%s) leaves the page on its previous declaration", async (_name, frame) => {
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    const sock = stubSocket();
    renderPage(viewProps);
    expect(screen.getByText("original")).toBeVisible();

    sock.frame(frame as Record<string, unknown>);

    expect(screen.getByText("original")).toBeVisible();
    expect(errorSpy).toHaveBeenCalled();
  });
});

// The `hook` argument is what the per-hook updated cue is keyed on, and it is
// the ONE field of a view frame that reaches onView without also reaching the
// rendered declaration: a clear of an already-empty hook changes no bytes, so
// nothing about the declaration says the agent acted. A hook dropped on the way
// through this callback is therefore invisible to every render-level assertion
// — the cue simply never appears, on a well-formed frame — which is why it is
// read directly off the call here.
describe("the view arm: onView receives the hook the frame named", () => {
  function Probe({ onView }: { onView: (d: Declaration, p?: Record<string, string>, hook?: string) => void }) {
    useActionLifecycle("workshop", "sess-live", {}, () => {}, onView, []);
    return null;
  }

  it("passes the frame's `hook` as onView's third argument, and undefined when the frame names none", async () => {
    const sock = stubSocket();
    const calls: Array<[Declaration, Record<string, string> | undefined, string | undefined]> = [];
    render(<Probe onView={(d, p, hook) => calls.push([d, p, hook])} />);

    sock.frame(liveGolden.viewUpdate as unknown as Record<string, unknown>);
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0][2]).toBe("panel");

    sock.frame(liveGolden.viewOpen as unknown as Record<string, unknown>);
    await waitFor(() => expect(calls).toHaveLength(2));
    expect(calls[1][2]).toBeUndefined();
  });
});

// ===== The two producers of one fact =====
//
// The POST answer and the live socket both describe the same action. The
// socket reads memory, which the runner writes BEFORE it answers the POST, so
// a socket frame is never staler than a POST answer that raced it. These tests
// pin the consequences of that asymmetry.
describe("a POST answer never overwrites newer socket state", () => {
  // This is the NORMAL ordering for an approval-gated action, not a rare race:
  // the runner records+publishes awaiting_approval from the detached exec while
  // webd is still relaying the synchronous "submitted" reply. The POST response
  // envelope has no approvalAddressedToViewer field at all, so applying it
  // afterwards used to un-tell the viewer that THEY are the approver — and no
  // further frame is due until the approver acts, so the approval simply
  // expired.
  it("an approval frame that lands while the POST is in flight survives the POST answer", async () => {
    const sock = stubSocket();
    const f = deferredActionsFetch();
    renderPage(props);

    await userEvent.click(screen.getByRole("button", { name: "Advance" }));
    await waitFor(() => expect(f.actionsCalls()).toBe(1));

    // The socket wins the race, as it normally does. The surface copy and the
    // reveal are one passive-effect flush apart — the copy renders when Chrome
    // receives the new approvals, the reveal only after Chrome's own
    // trust-event effect dispatches — so each gets its own wait, for the same
    // reason the RELOAD-mid-approval row above does.
    sock.frame(goldenApprovalEvent);
    await waitFor(() => expect(screen.getByTestId("session-shell-approval-surface")).toHaveTextContent(/your approval/i));
    await waitFor(() => expect(screen.getByTestId("session-shell-identity")).toBeVisible());

    // ...and only now does the synchronous answer arrive.
    f.resolve({ requestId: "r1", state: "submitted", message: "Request submitted." });

    // Nothing about the approval may be lost: not the surface copy, not the
    // revealed chrome, not the caption on the control itself.
    await waitFor(() => expect(f.actionsCalls()).toBe(1));
    expect(screen.getByTestId("session-shell-approval-surface")).toHaveTextContent(/your approval/i);
    expect(screen.getByTestId("session-shell-identity")).toBeVisible();
    expect(screen.getByTestId("agent-ui-action-status")).toHaveTextContent(/your approval is needed/i);
  });

  // The mirror case: with no socket frame in between, the POST answer is the
  // only thing that knows anything, and it must still be applied.
  it("applies the POST answer when no socket frame superseded it", async () => {
    stubSocket();
    const f = deferredActionsFetch();
    renderPage(props);

    await userEvent.click(screen.getByRole("button", { name: "Advance" }));
    await waitFor(() => expect(f.actionsCalls()).toBe(1));
    f.resolve({ requestId: "r1", state: "denied", message: "The request was denied." });

    await waitFor(() => expect(screen.getByTestId("agent-ui-action-status")).toHaveTextContent("The request was denied."));
    expect(screen.getByRole("button", { name: /Advance/ })).toBeEnabled();
  });
});

// ===== The reconnect must REPAIR, not merely backfill =====
describe("a reconnect snapshot reconciles state the browser already holds", () => {
  // The spec's "survives a tab reload and an approval that takes ten minutes",
  // for the case where the tab never reloads: the socket drops mid-approval,
  // the action settles while it is down, and the snapshot on reconnect is the
  // ONLY carrier of that settle. A snapshot that filled only unknown keys left
  // the control disabled forever and never re-evaluated the page's bindings.
  it("a settle that happened while disconnected reaches the page on reconnect", async () => {
    const sock = stubSocket();
    renderPage(props);
    const bindingsCalls = () => (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.filter(([u]) => String(u).endsWith("/bindings")).length;
    await waitFor(() => expect(bindingsCalls()).toBeGreaterThanOrEqual(1));

    await userEvent.click(screen.getByRole("button", { name: "Advance" }));
    await waitFor(() => expect(screen.getByRole("button", { name: /Advance/ })).toBeDisabled());
    const before = bindingsCalls();

    // The socket drops. The action settles server-side while it is down, so
    // nothing about that settle is ever delivered as an event frame.
    sock.drop();
    await waitFor(() => expect(sock.sockets()).toBeGreaterThan(1), { timeout: 5000 });

    sock.frame(snapshotOf([goldenSettledEvent.action as LiveActionEntry]));

    await waitFor(() => expect(screen.getByRole("button", { name: /Advance/ })).toBeEnabled());
    await waitFor(() => expect(bindingsCalls()).toBeGreaterThan(before));
  });

  // Dedup is FRAME-LOCAL, not map-wide: uiaction.List is newest-first, so the
  // first entry for an action in one frame wins and a later, older duplicate
  // in the SAME frame must not overwrite it.
  it("keeps the newest entry when one snapshot carries two records for one action", async () => {
    const sock = stubSocket();
    renderPage(props);

    const settled = goldenSettledEvent.action as LiveActionEntry;
    const approval = goldenApprovalEvent.action as LiveActionEntry;
    sock.frame(snapshotOf([settled, approval])); // newest first, then the older one

    await waitFor(() => expect(screen.getByRole("button", { name: /Advance/ })).toBeEnabled());
    expect(screen.getByTestId("session-shell-identity")).not.toBeVisible();
  });

  // A snapshot read before this click's own record existed must not re-enable
  // a control whose write has not answered yet.
  it("does not let a stale snapshot re-enable a control with a POST still in flight", async () => {
    const sock = stubSocket();
    const f = deferredActionsFetch();
    renderPage(props);

    await userEvent.click(screen.getByRole("button", { name: "Advance" }));
    await waitFor(() => expect(screen.getByRole("button", { name: /Advance/ })).toBeDisabled());

    sock.frame(snapshotOf([goldenSettledEvent.action as LiveActionEntry]));

    await waitFor(() => expect(f.actionsCalls()).toBe(1));
    expect(screen.getByRole("button", { name: /Advance/ })).toBeDisabled();
  });
});

// ===== The double-submit guard =====
//
// The guard is a `useRef` Set checked and added to synchronously at the top of
// invoke, before fetch. Nothing in this tree could previously tell one POST
// from two — every assertion used mock.calls.find, i.e. existence only — so
// deleting the guard left the whole suite green. For a write path, a duplicate
// is a duplicate MUTATION.
describe("the double-submit guard", () => {
  // A second userEvent.click proves nothing: the rendered `disabled` attribute
  // swallows it, so the test would pass with the guard deleted. Submitting the
  // ap:form twice via requestSubmit dispatches two real submit events in one
  // task, before React can commit the first click's "submitted" phase — which
  // is the exact window the ref guard exists to close.
  const formProps = {
    ...props,
    declaration: {
      actions: [{ name: GOLDEN_ACTION, tool: "crm_advance_stage", inputs: ["note"] }],
      view: {
        component: "ap:form",
        props: { action: GOLDEN_ACTION, submitLabel: "Advance", fields: [{ name: "note", label: "Note" }] },
      },
    },
  };

  it("issues exactly ONE /actions POST for two submits dispatched before the first returns", async () => {
    stubSocket();
    const f = deferredActionsFetch();
    const { container } = renderPage(formProps);

    const form = container.querySelector("form");
    expect(form).not.toBeNull();
    form!.requestSubmit();
    form!.requestSubmit();

    await waitFor(() => expect(f.actionsCalls()).toBe(1));
    // Give any second request a chance to appear before claiming there is none.
    await new Promise((r) => setTimeout(r, 50));
    expect(f.actionsCalls()).toBe(1);
  });

  it("allows a fresh invocation once the first request has settled", async () => {
    stubSocket();
    const f = deferredActionsFetch();
    const { container } = renderPage(formProps);

    const form = container.querySelector("form");
    form!.requestSubmit();
    await waitFor(() => expect(f.actionsCalls()).toBe(1));

    f.resolve({ requestId: "r1", state: "failed", message: "The request failed. You can try again." });
    await waitFor(() => expect(screen.getByRole("button", { name: /Advance/ })).toBeEnabled());

    form!.requestSubmit();
    await waitFor(() => expect(f.actionsCalls()).toBe(2));
  });
});

// ===== Failure surfaces =====
describe("failures the viewer is told about", () => {
  // A transport-failed POST must degrade exactly the control that was clicked.
  // Firing onSettled from the catch arm re-POSTed /bindings over the same dead
  // connection, which failed identically, and useBindings then replaced EVERY
  // resolved binding on the page with a transport-error card: one offline click
  // turned a fully rendered dashboard into "This view's data could not be
  // loaded."
  it("a transport-failed POST does not re-evaluate the page's bindings", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    stubSocket();
    const fetchMock = vi.fn(async (url: string) => {
      if (String(url).endsWith("/actions")) throw new Error("offline");
      return { ok: true, json: async () => ({ bindings: {} }) } as Response;
    });
    vi.stubGlobal("fetch", fetchMock);
    const bindingsCalls = () => fetchMock.mock.calls.filter(([u]) => String(u).endsWith("/bindings")).length;

    renderPage(props);
    await waitFor(() => expect(bindingsCalls()).toBeGreaterThanOrEqual(1));
    const before = bindingsCalls();

    await userEvent.click(screen.getByRole("button", { name: "Advance" }));
    await waitFor(() => expect(screen.getByRole("button", { name: /Advance/ })).toBeEnabled());

    // Comfortably past useBindings' own DEBOUNCE_MS (250ms): a re-evaluation
    // triggered by the failed click would be ISSUED after that delay, so a
    // shorter wait here would let the defect survive this assertion — it did,
    // when this wait was 50ms.
    await new Promise((r) => setTimeout(r, 600));
    expect(bindingsCalls()).toBe(before);
  });

  // live.go authors browser-safe copy on its "error" frame and then closes.
  // Discarding it left the viewer looking at controls that silently never
  // update while the server had already written the sentence explaining why.
  it("an error frame's server-authored copy reaches the viewer", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    const sock = stubSocket();
    renderPage(props);

    sock.frame({ type: "error", message: "this view's live updates are unavailable right now" });

    await waitFor(() =>
      expect(screen.getByTestId("agent-ui-live-error")).toHaveTextContent("this view's live updates are unavailable right now"),
    );
  });

  it("a snapshot clears a previous error notice — live updates are working again", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    const sock = stubSocket();
    renderPage(props);

    sock.frame({ type: "error", message: "this view's pending actions could not be loaded" });
    await waitFor(() => expect(screen.getByTestId("agent-ui-live-error")).toBeInTheDocument());

    sock.frame(goldenSnapshot);
    await waitFor(() => expect(screen.queryByTestId("agent-ui-live-error")).toBeNull());
  });
});
