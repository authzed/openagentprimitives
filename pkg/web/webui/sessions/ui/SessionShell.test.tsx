import "@testing-library/jest-dom/vitest";
import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { ChatFrame } from "../../chat/ui/types";
import type { SelectedView, ShellBootstrapProps } from "./types";

// The transcript's live socket is mocked for the whole file: the shell renders
// the REAL ChatView for a chat-kind view, and jsdom has no WebSocket. Capturing
// the hook's arguments is also how the "which session does the transcript
// open" row is checked, and capturing onFrame is what lets the away-notification
// rows drive a reply without a server.
let chatSocketFor: { ns: string; name: string } | null = null;
let chatOnFrame: ((f: ChatFrame) => void) | null = null;
vi.mock("../../chat/ui/useChatSocket", () => ({
  useChatSocket: (ns: string, name: string, onFrame: (f: ChatFrame) => void) => {
    chatSocketFor = { ns, name };
    chatOnFrame = onFrame;
    return { state: "connected" };
  },
}));

import { SessionShell } from "./SessionShell";
import { classOptionLabels, NewSessionDialog, parseStartResponse } from "./NewSessionDialog";
import golden from "./testdata/shell.golden.json";
import originsGolden from "./testdata/origins.golden.json";
import startGolden from "./testdata/start.golden.json";
import liveGolden from "../../agentui/ui/testdata/live.golden.json";
import type { LiveActionEntry, LiveActionMessage } from "../../agentui/ui/useActionLifecycle";

// FakeSocket stands in for the real WebSocket the agent-defined view opens
// against GET /agent-ui/{ns}/{name}/live. jsdom has no WebSocket global at
// all, and this Node runtime's real one would attempt a genuine network
// connection, so tests that want a live channel install this instead.
//
// Defined here rather than shared with pkg/web/webui/agentui/ui's own suite:
// per-file fakes are this area's existing convention, and a shared helper
// would couple two suites' fixtures for no gain.
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

// stubSocket installs FakeSocket as the global WebSocket and returns a handle
// a test uses to drive the live channel. `frame` delivers one frame's raw JSON
// (every caller passes a golden frame, so no row here re-spells a wire field
// name); `drop` closes the connection the way a laptop sleep or a webd rollout
// does, which makes the view schedule a reconnect; `sockets` reports how many
// connections have been opened, so a row can wait for that reconnect rather
// than framing into a socket the view has already abandoned.
function stubSocket() {
  FakeSocket.instances = [];
  vi.stubGlobal("WebSocket", FakeSocket);
  const latest = () => FakeSocket.instances[FakeSocket.instances.length - 1];
  return {
    frame(msg: LiveActionMessage | Record<string, unknown>) {
      latest()?.onmessage?.({ data: JSON.stringify(msg) });
    },
    drop() {
      latest()?.onclose?.();
    },
    sockets: () => FakeSocket.instances.length,
  };
}

// The frames below are the REAL bytes GET .../live writes, captured into
// pkg/web/webui/agentui/ui/testdata/live.golden.json by that package's own
// live_golden_internal_test.go. Hand-written literals were what left this
// seam unpinned once already.
const goldenApprovalEvent = liveGolden.approvalEvent as LiveActionMessage;
const goldenSnapshot = liveGolden.snapshot as LiveActionMessage;

// snapshotOf builds a snapshot frame around arbitrary entries WITHOUT
// re-spelling live.go's field names: the envelope comes from the golden's own
// snapshot frame and the entries are golden entries, so this helper cannot
// drift from the wire the way a literal can.
function snapshotOf(entries: LiveActionEntry[]): LiveActionMessage {
  return { ...goldenSnapshot, actions: entries };
}

const goldenApprovalEntry = goldenApprovalEvent.action as LiveActionEntry;

const shellGolden = golden as unknown as ShellBootstrapProps;

// withSelected returns the golden with its selected session's fields
// overridden — used where a row needs a different view kind or origin than
// the fixture's own. Everything not named keeps the golden's real value, so
// these variants never drift into hand-built props.
function withSelected(overrides: Partial<SelectedView>): ShellBootstrapProps {
  const selected = shellGolden.selected as SelectedView;
  return { ...shellGolden, selected: { ...selected, ...overrides } };
}

// The chrome regions the collapse toggle must reduce and can never remove.
const chromeRegionTestIds = [
  "session-shell-identity",
  "session-shell-session-origin",
  "session-shell-chat-escape-hatch",
  // Every fixture in this sweep selects the golden's session, whose agent
  // declares a view, so the switch's agent-view half renders in all of them and
  // is under the same collapse discipline as the chat half beside it.
  "session-shell-agent-view-switch",
  "session-shell-header-actions",
  "session-shell-session-info",
  "session-shell-approval-surface",
  "session-shell-sidebar",
  "session-shell-session-list",
];

// MockNotification stands in for the Web Notifications API, which jsdom does
// not implement. Only the away-notification rows install it.
class MockNotification {
  static permission: NotificationPermission = "granted";
  static requestPermission = vi.fn(async () => "granted" as NotificationPermission);
  static instances: MockNotification[] = [];
  onclick: (() => void) | null = null;
  constructor(public title: string, public options?: { body?: string }) {
    MockNotification.instances.push(this);
  }
  close() {}
}

function setTabHidden() {
  Object.defineProperty(document, "visibilityState", { configurable: true, value: "hidden" });
  document.dispatchEvent(new Event("visibilitychange"));
}

// chatSelection is the golden with its selection switched to the transcript
// view — the case the shell now serves inline rather than behind a link.
function chatSelection(overrides: Partial<SelectedView> = {}): ShellBootstrapProps {
  return withSelected({
    offersAgentUI: false,
    view: { kind: "chat", chat: { ns: "demo-ns", name: "alpha" } },
    ...overrides,
  });
}

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) =>
      String(url).endsWith("/actions")
        ? (({ ok: true, json: async () => ({ requestId: "r1", state: "submitted" }) }) as unknown as Response)
        : (({ ok: true, json: async () => ({ bindings: {}, timeline: [] }) }) as unknown as Response),
    ),
  );
  vi.stubGlobal("WebSocket", FakeSocket);
  FakeSocket.instances = [];
  chatSocketFor = null;
  chatOnFrame = null;
  MockNotification.instances = [];
  Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

// testdata/shell.golden.json is the ONE artifact both halves of the
// /sessions bootstrap seam read — see page_golden_internal_test.go's own doc
// comment for why (the "sessionOrigin"->"origin" rename that stayed green on
// both sides before pkg/web/webui/agentui adopted this same pattern).
//
// The tests below give two DIFFERENT guarantees, and neither alone is the
// whole contract — see shellProps' own doc comment (page.go) for the full
// breakdown:
//   - the render rows drive the golden through the REAL component, so they
//     catch a rename OR a value change on every field a component reads.
//   - "carries buildSessionList's real, joined content" reads
//     golden.sessions/golden.notices/golden.startableClasses directly off
//     the PARSED JSON. This is the "read the wire's own keys" pattern
//     pkg/web/webui/agentui/props_golden_internal_test.go's
//     firstDeclarationSlotKeys uses for the identical reason: it catches a
//     RENAMED or DROPPED key (the property would simply be absent, so the
//     assertion fails), and a VALUE change too, since the assertions below
//     pin real field values rather than merely checking that the arrays are
//     non-empty. Every field named here is ALSO rendered somewhere in this
//     file, so the direct reads are a second, independent pin rather than the
//     only one. Fixture values are demo-ns/demo-agent fabrications, not read
//     from `examples/`.
describe("SessionShell — the golden bootstrap props page.go actually emits", () => {
  it("carries buildSessionList's real, joined content for sessions/notices/startableClasses", () => {
    // Two joinable sessions, newest StartedAt first (see
    // pkg/web/webui/sessions/list_test.go's TestBuildSessionList_Ordering for
    // the sort this pins): the Active one, then the one waiting on the
    // viewer.
    expect(golden.sessions).toHaveLength(2);
    expect(golden.sessions[0]).toMatchObject({
      ns: "demo-ns",
      name: "alpha",
      class: "demo-agent",
      title: "Demo Agent",
      phase: "Active",
      awaitingHuman: false,
      ended: false,
      // startedAt was previously omitted here, so a coordinate rename of its
      // json tag in both list.go and this golden left both suites green —
      // see list_test.go's TZ=UTC coverage for the companion .UTC() gap this
      // pins the wire side of.
      startedAt: "2026-01-02T00:00:00Z",
    });
    expect(golden.sessions[1]).toMatchObject({
      ns: "demo-ns",
      name: "beta",
      class: "demo-agent",
      title: "Demo Agent",
      phase: "Waiting for you",
      awaitingHuman: true,
      ended: false,
      startedAt: "2026-01-01T00:00:00Z",
    });

    // notices.unavailable counts the lookup hit with no Kubernetes object
    // PLUS the unrepresentable id the golden's Go fixture seeds — see
    // page_golden_internal_test.go's goldenShellFixtureDeps.
    expect(golden.notices).toEqual({ unavailable: 2, truncated: false, bootstrapUnavailable: false });

    // startable is pinned TRUE, not merely present: the browser gates its
    // submit on this field, and a golden carrying a boolean's zero value pins
    // nothing — a builder that hardcoded false would match it exactly.
    // offersUI is pinned TRUE for the same reason as startable: it decides
    // whether the dialog demands an opening message and which view the start
    // lands on, and a golden carrying false would be matched exactly by a
    // builder that never read the AgentClass at all.
    expect(golden.startableClasses).toEqual([
      { ns: "demo-ns", class: "demo-agent", title: "Demo Agent", startable: true, offersUI: true },
    ]);

    // canStartSessions was the one wire field with no cross-language artifact
    // pinning it: every render row below supplies it explicitly through
    // startProps(), so none of them reads the golden's value, leaving a Go
    // test's hardcoded literal on one side and the browser's independent
    // literal on the other. A three-way coordinated rename (json tag + golden
    // + Go test literal) would silently disable the start control everywhere.
    expect(golden.canStartSessions).toBe(true);
  });

  // The render half of the same pin: the golden's OWN two start facts, spread
  // unmodified, must put the control on screen. Together with the direct read
  // above, a rename of either key now fails here rather than passing because
  // the fixture happened to re-supply it.
  it("renders the start control from the golden's own start facts, unmodified", () => {
    render(<SessionShell {...{ ...shellGolden, selected: undefined }} />);
    expect(screen.getByTestId("session-shell-new-session")).toBeVisible();
  });

  // selected (view.go's selectedView) is a THREE-WAY discriminated union
  // nested under shellProps — strictly more surface than the flat fields
  // above, and the standing briefing's defect #2 ("renaming a Go json tag
  // `sessionOrigin` -> `origin` left Go AND all 39 frontend tests green") is
  // exactly the failure shape a coordinated rename of any of these keys would
  // reproduce. offersAgentUI is additionally rendered — it is what puts the
  // switch's agent-view half on screen — so this direct read is the second pin
  // on it, catching a renamed or dropped key rather than only a wrong value.
  it("carries a populated `selected` agent-ui view, read by key from the golden", () => {
    expect(golden).toHaveProperty("selected");
    const selected = golden.selected as unknown as SelectedView;

    expect(selected.ns).toBe("demo-ns");
    expect(selected.name).toBe("alpha");
    expect(selected.sessionOrigin).toBe("attached");
    expect(selected.offersAgentUI).toBe(true);

    expect(selected.view.kind).toBe("agent-ui");
    expect(selected.view.ui?.ns).toBe("demo-ns");
    expect(selected.view.ui?.name).toBe("alpha");
  });

  // A golden pins nothing if the field it should pin is `omitempty` and
  // absent. chromeProps.InitialState carries json:"initialState,omitempty",
  // so a regenerated golden whose fixture stopped setting it would still LOOK
  // like a pin while the honors-initialState test below passed vacuously (an
  // absent request is the platform default, shown — and that assertion is
  // `not.toBeVisible`, which would then fail loudly; but `sessionOrigin` and
  // a non-empty `sessions` have no such luck and would pass silently). Assert
  // the FILE.
  it("the golden file itself carries the optional fields the tests above depend on", () => {
    const raw = golden as Record<string, any>;
    expect(raw.selected?.chrome?.initialState).toBe("collapsed");
    expect(raw.selected?.sessionOrigin).toBe("attached");
    expect(Array.isArray(raw.sessions) && raw.sessions.length).toBeGreaterThan(0);
  });
});

// The five spanning tests below moved here from the agent-UI view's own test
// file, where they rendered testdata/props.golden.json through the page that
// used to own chrome. Four of the five facts are the SHELL's now, and the
// props that carry them come from a different Go builder — so they read
// shell.golden.json, which pkg/web/webui/sessions' own page_golden_internal_test.go
// asserts shellPageBuild's real output against. Their assertions are
// unchanged apart from the escape hatch's href, which the hoist deliberately
// changed: this is the same guarantee, checked through the component that now
// provides it.
//
// The Critical these caught: renaming a Go json tag (sessionOrigin -> origin)
// left the Go suite AND the whole frontend suite green while the shipped page
// silently lost its session disclosure. The golden is the one artifact both
// sides read, which is what makes a rename on either side fail the other.
//
// All five hold with chrome COLLAPSED, which is what lets one fixture serve
// both the initialState assertion and the other four: toHaveTextContent and
// toHaveAttribute read hidden elements, and the content region carries no
// `hidden`.
describe("SessionShell — the golden shell props shellPageBuild actually emits", () => {
  it("renders the viewer's identity from the golden `subject`", () => {
    render(<SessionShell {...shellGolden} />);
    expect(screen.getByTestId("session-shell-identity")).toHaveTextContent("Acting as alice@example.com");
  });

  it("renders the session disclosure from the golden `selected.sessionOrigin`", () => {
    render(<SessionShell {...shellGolden} />);
    expect(screen.getByTestId("session-shell-session-origin")).toHaveTextContent("Resumed your session.");
  });

  it("links the chat escape hatch at the golden's selected session, without leaving the shell", () => {
    render(<SessionShell {...shellGolden} />);
    expect(screen.getByTestId("session-shell-chat-escape-hatch")).toHaveAttribute(
      "href",
      "/sessions?session=demo-ns%2Falpha&view=chat",
    );
  });

  // The RETURN leg, driven by the golden's own `selected.offersAgentUI`. Both
  // hrefs are asserted exactly and differ only in the `&view=` they carry,
  // which is the whole of what distinguishes the two controls: a switch whose
  // halves address the same view is a one-way trip with two buttons.
  it("renders the switch's agent-view half from the golden `selected.offersAgentUI`, marked as the current view", () => {
    render(<SessionShell {...shellGolden} />);
    const back = screen.getByTestId("session-shell-agent-view-switch");
    expect(back).toHaveAttribute("href", "/sessions?session=demo-ns%2Falpha&view=ui");
    // The golden's selected view IS the agent's, so this half is where the
    // viewer is and the chat half is where they can go.
    expect(back).toHaveAttribute("aria-current", "page");
    expect(screen.getByTestId("session-shell-chat-escape-hatch")).not.toHaveAttribute("aria-current");
  });

  it("renders the golden view's declaration inside the content region", () => {
    render(<SessionShell {...shellGolden} />);
    expect(
      within(screen.getByTestId("session-shell-content-region")).getByRole("heading", { name: "Fleet Console" }),
    ).toBeInTheDocument();
  });

  it("honors the golden `selected.chrome.initialState` request", () => {
    render(<SessionShell {...shellGolden} />);
    expect(screen.getByTestId("session-shell-identity")).not.toBeVisible();
    expect(screen.getByTestId("session-shell-identity")).toBeInTheDocument();
  });
});

// The lift: an approval the VIEW's live lifecycle learns about must reach the
// SHELL's approval surface. Asserting it on either component alone proves
// nothing — Chrome renders whatever approvals it is handed, and the view
// computes them correctly today; the defect this branch already shipped was a
// prop nobody passed between the two.
describe("SessionShell — the view's live signals reach the shell's chrome", () => {
  it("an approval the view's live lifecycle reports appears in the shell's approval surface", async () => {
    const sock = stubSocket();
    render(<SessionShell {...shellGolden} />);

    sock.frame(goldenApprovalEvent);

    await waitFor(() =>
      expect(screen.getByTestId("session-shell-approval-surface")).toHaveTextContent(/your approval/i),
    );
  });

  it("that same approval, addressed to the viewer, auto-reveals the collapsed chrome", async () => {
    const sock = stubSocket();
    render(<SessionShell {...shellGolden} />);
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

  // The negative control for the row above. Decision 3's approval table
  // auto-reveals chrome for the APPROVER, never for the requester of someone
  // else's pending approval — auto-revealing for every approval regardless of
  // who must act is how chrome starts crying wolf in a shared session. The
  // notice still reaches the surface; only the reveal is withheld.
  it("an approval addressed to SOMEONE ELSE reaches the surface without revealing chrome", async () => {
    const sock = stubSocket();
    render(<SessionShell {...shellGolden} />);

    sock.frame({ ...goldenApprovalEvent, action: { ...goldenApprovalEntry, approvalAddressedToViewer: false } });

    await waitFor(() =>
      expect(screen.getByTestId("session-shell-approval-surface")).toHaveTextContent(/approval from someone else/i),
    );
    expect(screen.getByTestId("session-shell-identity")).not.toBeVisible();
  });

  // The reload arm: memory is authoritative, so a browser that (re)connects
  // mid-approval learns the pending state from the SNAPSHOT alone, with no
  // event frame and nothing clicked in this tab. Chrome must reveal from that
  // path too, or a viewer who reloaded during a ten-minute approval sees a
  // collapsed chrome and no sign anyone is waiting on them.
  it("a reload mid-approval reveals chrome from the snapshot alone", async () => {
    const sock = stubSocket();
    render(<SessionShell {...shellGolden} />);
    expect(screen.getByTestId("session-shell-identity")).not.toBeVisible();

    sock.frame(snapshotOf([goldenApprovalEntry]));

    await waitFor(() => expect(screen.getByTestId("session-shell-identity")).toBeVisible());
    expect(screen.getByTestId("session-shell-approval-surface")).toHaveTextContent(/your approval/i);
  });

  // The RECONNECT arm, which the reload arm above does not cover: this tab
  // never reloads. The socket drops (a laptop sleep, a webd rollout), the
  // approval is raised while it is down — so no event frame for it is ever
  // delivered — and the snapshot on the NEW connection is the only carrier of
  // it. Chrome must reveal from that path too.
  //
  // Without this, a viewer whose connection blipped during a ten-minute
  // approval sits in front of a collapsed chrome with nothing indicating that
  // anyone is waiting on them, and no reload to trigger the arm above. The
  // spec's one non-negotiable is that a trust event ALWAYS reveals chrome; a
  // delivery path where it does not is the same failure in a narrower window.
  it("an approval raised while the socket was DOWN reveals chrome from the reconnect snapshot", async () => {
    const sock = stubSocket();
    render(<SessionShell {...shellGolden} />);
    expect(screen.getByTestId("session-shell-identity")).not.toBeVisible();

    // The connection drops, and the view schedules a reconnect. Waiting for
    // the SECOND socket matters: framing into the first one would deliver into
    // a connection the view has already abandoned, and the row would then be
    // testing the reload arm again under a different name.
    sock.drop();
    await waitFor(() => expect(sock.sockets()).toBeGreaterThan(1), { timeout: 5000 });

    sock.frame(snapshotOf([goldenApprovalEntry]));

    await waitFor(() => expect(screen.getByTestId("session-shell-identity")).toBeVisible());
    expect(screen.getByTestId("session-shell-approval-surface")).toHaveTextContent(/your approval/i);
  });

  // Chrome's trustEvents is UNRANKED. A render failure reveals chrome first
  // (a DIFFERENT source than an approval), the viewer re-collapses (their
  // own explicit choice — legitimate, and it must not make them unreachable
  // by the NEXT source), and THEN a genuinely new approval addressed to the
  // viewer arrives. Asserting the second reveal here is the regression guard
  // on nobody reintroducing a `??` precedence between the sources.
  //
  // Order is load-bearing, and deliberately the OPPOSITE of "approval first,
  // then break a node": collapsing `[renderFailure, ...approvalTrustEvents]`
  // to `[renderFailure ?? approvalTrustEvents[0]]` is invisible whenever only
  // ONE source is ever active at a time (the mutated single-slot list and the
  // unranked list compute the identical active-id set in that case — Chrome's
  // own `flatMap` already drops the nulls either way). It is only observable
  // when a HIGHER-priority slot in the `??` chain (renderFailure) is already,
  // and remains, non-null while a LOWER-priority slot's fresh event (an
  // approval) arrives: the mutated chain keeps resolving to the
  // already-revealed renderFailure id and never reaches the new one.
  it("the render-error trust event does not starve a LATER approval — the unranked-list regression", async () => {
    const sock = stubSocket();
    const selected = shellGolden.selected as SelectedView;
    render(
      <SessionShell
        {...withSelected({
          view: {
            ...selected.view,
            ui: { ...selected.view.ui!, declaration: { view: { component: "ap:not-a-real-component" } } },
          },
        })}
      />,
    );
    await waitFor(() => expect(screen.getByTestId("session-shell-identity")).toBeVisible());

    await userEvent.click(screen.getByTestId("session-shell-chrome-toggle"));
    expect(screen.getByTestId("session-shell-identity")).not.toBeVisible();

    sock.frame(goldenApprovalEvent);

    await waitFor(() => expect(screen.getByTestId("session-shell-identity")).toBeVisible());
  });
});

// The shell owns the approval surface for EVERY listed session — but only the
// SELECTED session's own live signal is a trust event. The golden's second
// row (beta) is awaitingHuman and is NOT the selection, which is what makes
// this pair testable against one fixture.
describe("SessionShell — cross-session approval notices are surfaced, never auto-revealing", () => {
  it("surfaces an unselected session's awaiting-a-human notice in the approval surface", () => {
    render(<SessionShell {...shellGolden} />);
    expect(screen.getByTestId("session-shell-approval-surface")).toHaveTextContent(
      "Demo Agent is waiting on a person in another session.",
    );
  });

  it("does NOT auto-reveal collapsed chrome for an unselected session's notice", () => {
    render(<SessionShell {...shellGolden} />);
    // The notice is present (asserted above) while chrome stays as the UI
    // requested it: the phase says a human is needed, not WHICH human.
    expect(screen.getByTestId("session-shell-identity")).not.toBeVisible();
  });

  it("does not repeat the SELECTED session's own row as a cross-session notice", () => {
    // alpha is the selection and is not awaitingHuman; beta is. Exactly one
    // line, so a filter that dropped the selection check would show two.
    render(<SessionShell {...shellGolden} />);
    const region = screen.getByTestId("session-shell-approval-surface");
    expect(region.querySelectorAll("li")).toHaveLength(1);
  });
});

describe("SessionShell — the content region renders whichever view the server chose", () => {
  // The transcript itself, not a link to one: the composer is the thing a
  // viewer can only have if the real view mounted, so it is what this asserts
  // rather than a container testid a placeholder could also carry. The
  // aria-current half is unchanged from when the arm WAS a placeholder — it is
  // the half that had to survive the swap.
  it("renders the transcript — composer and all — inside the content region, and marks the escape hatch as the current page", async () => {
    render(<SessionShell {...chatSelection()} />);
    const region = within(screen.getByTestId("session-shell-content-region"));
    expect(region.getByTestId("chat-view")).toBeInTheDocument();
    expect(await region.findByPlaceholderText("Message the agent…")).toBeInTheDocument();
    expect(screen.getByTestId("session-shell-chat-escape-hatch")).toHaveAttribute("aria-current", "page");
  });

  // The defect this pair exists for: a viewer on an agent-defined view follows
  // the chat control, and the control they just clicked is now the one marked
  // "you are here". Unless the way back renders too, the only recoveries are
  // the sidebar row — also marked "you are here" — and editing the URL, which
  // is the exact problem this whole feature removes.
  it("offers the way back to the agent's view while the transcript is the shown one", () => {
    render(
      <SessionShell
        {...withSelected({
          // chrome: {} drops the golden's own collapse REQUEST, so this row
          // asserts on what a viewer can actually see rather than on a hidden
          // element's attributes.
          chrome: {},
          offersAgentUI: true,
          view: { kind: "chat", chat: { ns: "demo-ns", name: "alpha" } },
        })}
      />,
    );
    const back = screen.getByTestId("session-shell-agent-view-switch");
    expect(back).toBeVisible();
    expect(back).toHaveAttribute("href", "/sessions?session=demo-ns%2Falpha&view=ui");
    expect(back).not.toHaveAttribute("aria-current");
    expect(screen.getByTestId("session-shell-chat-escape-hatch")).toHaveAttribute("aria-current", "page");
  });

  // The negative control. A session whose agent declares no view has nothing
  // for that half to address, so it must not render at all — chatSelection()
  // is exactly that case (offersAgentUI: false).
  it("renders no way back for a session whose agent offers no view of its own", () => {
    render(<SessionShell {...chatSelection()} />);
    expect(screen.queryByTestId("session-shell-agent-view-switch")).toBeNull();
    expect(screen.getByTestId("session-shell-chat-escape-hatch")).toBeInTheDocument();
  });

  // The broken-view arm: the class DOES declare a view, so the way back stays
  // offered (viewFor's offersAgentUI is true for a Valid=False/Unknown UI).
  //
  // The agent-view tab is CURRENT here, which changed when the switch became a
  // segmented control. Under two independent buttons, "neither is current" was
  // the honest reading of a content region showing neither sibling. Under
  // tabs it is not: a two-segment control with neither segment filled reads as
  // broken chrome, and it is also untrue — the viewer IS on the agent-view
  // tab; what that tab contains is a disclosure that the view could not be
  // resolved. The card carries that fact, and the tab says where you are.
  it("marks the agent-view tab current while its inline unavailable card is showing, and keeps chat followable", () => {
    render(
      <SessionShell
        {...withSelected({
          offersAgentUI: true,
          view: { kind: "unavailable", unavailable: { title: "UI not ready", message: "This UI is not valid." } },
        })}
      />,
    );
    expect(screen.getByTestId("session-shell-agent-view-switch")).toHaveAttribute("aria-current", "page");
    expect(screen.getByTestId("session-shell-chat-escape-hatch")).not.toHaveAttribute("aria-current");
    expect(screen.getByTestId("session-shell-view-unavailable")).toHaveTextContent("This UI is not valid.");
  });

  // The shell chose the session; the transcript must open THAT one. Nothing in
  // the transcript's own props can be re-derived from the URL, so this is the
  // only place the shell's selection and the socket's session are compared.
  it("opens the transcript against the selected session, and no other", () => {
    render(<SessionShell {...chatSelection()} />);
    expect(chatSocketFor).toEqual({ ns: "demo-ns", name: "alpha" });
  });

  // An ended session still renders its transcript — read-only. The shell
  // derives `readOnly` from the same value chrome discloses.
  it("renders an ended session's transcript read-only rather than withholding it", async () => {
    render(<SessionShell {...chatSelection({ sessionOrigin: "ended" })} />);
    const region = within(screen.getByTestId("session-shell-content-region"));
    const composer = (await region.findByPlaceholderText("This conversation has ended.")) as HTMLTextAreaElement;
    expect(composer.disabled).toBe(true);
    expect(region.getByTestId("chat-view")).toBeInTheDocument();
  });

  // The bootstrap's reason has to reach the chrome that discloses it: the
  // server decides WHY a session ended, this component only carries it, and
  // a carrier that drops it leaves both sides passing their own tests while
  // the page says nothing.
  it("forwards a boot-failed session's reason from the bootstrap into the chrome disclosure", () => {
    const reason = "The agent could not be started, so nothing ran.";
    render(<SessionShell {...chatSelection({ sessionOrigin: "ended", endedReason: reason })} />);
    expect(screen.getByTestId("session-shell-session-origin")).toHaveTextContent(reason);
  });

  // `chat` is the discriminated union's own payload and it is what addresses
  // the transcript. Reading `selected.ns/name` instead would make a rename of
  // its json tag invisible, because the two carry the same value today.
  it("addresses the transcript from the chat payload, not from the selection", () => {
    render(
      <SessionShell {...chatSelection({ view: { kind: "chat", chat: { ns: "other-ns", name: "other-session" } } })} />,
    );
    expect(chatSocketFor).toEqual({ ns: "other-ns", name: "other-session" });
  });

  it("answers a chat kind with no payload inline, and logs the cause", () => {
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    render(<SessionShell {...withSelected({ chrome: {}, view: { kind: "chat" } })} />);
    expect(within(screen.getByTestId("session-shell-content-region")).getByTestId("session-shell-view-unavailable")).toBeInTheDocument();
    expect(screen.queryByTestId("chat-view")).toBeNull();
    expect(errorSpy).toHaveBeenCalledWith(
      "sessions: cannot render the selected view",
      expect.objectContaining({ reason: "kind is chat but the view payload is absent" }),
    );
  });

  // The AgentSession's Kubernetes object name is internal vocabulary and must
  // never reach a browser surface — including the transcript's own chrome-free
  // region, which has no reason to name it at all.
  it("never renders the session's internal object name in the content region", () => {
    render(<SessionShell {...chatSelection()} />);
    expect(screen.getByTestId("session-shell-content-region").textContent).not.toContain("alpha");
  });

  // The wire's `kind` is a closed three-way union. A fourth value is a server
  // the browser has not caught up with, and falling through to the chat arm
  // would silently offer a transcript for a view that asked to be something
  // else.
  it("answers an unrecognized view kind inline, and logs which kind arrived", () => {
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    render(
      <SessionShell
        {...withSelected({
          chrome: {},
          view: { kind: "something-new" as unknown as "chat" },
        })}
      />,
    );
    expect(within(screen.getByTestId("session-shell-content-region")).getByTestId("session-shell-view-unavailable")).toBeInTheDocument();
    expect(screen.queryByTestId("chat-view")).toBeNull();
    expect(errorSpy).toHaveBeenCalledWith(
      "sessions: cannot render the selected view",
      expect.objectContaining({ kind: "something-new", ns: "demo-ns", name: "alpha" }),
    );
  });

  // kind says agent-ui but the payload is absent — a wire the server cannot
  // produce today. Rendered AND logged: the card is what the viewer can act
  // on, the log is the only place the cause survives.
  it("answers an agent-ui kind with no payload inline, and logs the cause", () => {
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    render(<SessionShell {...withSelected({ chrome: {}, view: { kind: "agent-ui" } })} />);
    expect(within(screen.getByTestId("session-shell-content-region")).getByTestId("session-shell-view-unavailable")).toBeInTheDocument();
    expect(errorSpy).toHaveBeenCalledWith(
      "sessions: cannot render the selected view",
      expect.objectContaining({ reason: "agent view shown but no ui or unavailable payload" }),
    );
  });

  // The same arm, with the server's own disclosure present. A DECLARED-but-
  // broken UI arrives as kind=unavailable with no `ui` payload, and selecting
  // the agent-view tab must show that message rather than the generic
  // could-not-be-loaded card — and must NOT log, because nothing is wrong with
  // the wire. Without this row the two cases are indistinguishable, and the
  // careful Valid=False-vs-Unknown copy viewFor authored would be discarded on
  // the way to the screen.
  it("shows the server's own disclosure for a declared-but-broken view, and logs nothing", () => {
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    render(
      <SessionShell
        {...withSelected({
          offersAgentUI: true,
          view: { kind: "unavailable", unavailable: { title: "UI not ready", message: "This UI is not valid." } },
        })}
      />,
    );
    expect(screen.getByTestId("session-shell-view-unavailable")).toHaveTextContent("This UI is not valid.");
    expect(errorSpy).not.toHaveBeenCalled();
  });

  // A broken view costs the CONTENT REGION only. This is the whole reason
  // viewFor renders an inline card instead of answering the page with a
  // *webui.PageError: chrome, the session list and the escape hatch survive.
  it("renders an unavailable view inline while every chrome region stays visible", () => {
    render(
      <SessionShell
        {...withSelected({
          chrome: {},
          view: { kind: "unavailable", unavailable: { title: "UI not ready", message: "This UI is not valid." } },
        })}
      />,
    );
    const region = within(screen.getByTestId("session-shell-content-region"));
    expect(region.getByText("UI not ready")).toBeInTheDocument();
    expect(region.getByText("This UI is not valid.")).toBeInTheDocument();
    for (const id of chromeRegionTestIds) {
      expect(screen.getByTestId(id)).toBeVisible();
    }
  });

  it("renders the additive notice alongside the content it accompanies, not instead of it", () => {
    render(
      <SessionShell
        {...withSelected({
          chrome: {},
          view: {
            kind: "chat",
            chat: { ns: "demo-ns", name: "alpha" },
            notice: { title: "Session ended", message: "Its transcript is shown below." },
          },
        })}
      />,
    );
    const region = within(screen.getByTestId("session-shell-content-region"));
    expect(region.getByTestId("session-shell-view-notice")).toHaveTextContent("Session ended");
    expect(region.getByTestId("chat-view")).toBeInTheDocument();
  });
});

// `selected` is a frozen bootstrap prop and the 30s poll deliberately never
// touches it, so a session that ends WHILE the viewer is watching it has no
// other way to reach chrome. Without this edge the content region says the
// conversation ended while the disclosure two inches above it still reads
// "Resumed your session." — indefinitely, not for 30s — and every control gated
// on the session being over stays unreachable until a reload.
describe("SessionShell — a session that ends while the viewer is watching it", () => {
  function endsNow() {
    act(() => {
      chatOnFrame!({
        type: "session_ended",
        session: { namespace: "demo-ns", name: "alpha" },
        payload: { session: { namespace: "demo-ns", name: "alpha" }, reason: "idle_timeout" },
      } as ChatFrame);
    });
  }

  it("re-discloses the session as ended in chrome, not just in the transcript", async () => {
    render(<SessionShell {...chatSelection({ chrome: {} })} />);
    // The server's answer at page load.
    expect(screen.getByTestId("session-shell-session-origin")).toHaveTextContent("Resumed your session.");
    await waitFor(() => expect(chatOnFrame).not.toBeNull());

    endsNow();

    // The transcript says so…
    await waitFor(() =>
      expect(screen.getByTestId("session-shell-content-region")).toHaveTextContent(
        "Conversation ended after being idle too long.",
      ),
    );
    // …and so does chrome, which is the half nothing else on the page updates.
    await waitFor(() =>
      expect(screen.getByTestId("session-shell-session-origin")).toHaveTextContent(
        "This session has ended. You can start a new one.",
      ),
    );
  });

  // The bootstrap said this session's agent offers a view; the live channel
  // then said the session is over, and an ended session's agent-UI routes all
  // answer 410. The switch's agent-view half must go with it, without a reload
  // — otherwise the shell offers a control whose destination stopped existing
  // while the viewer was looking at it. The server reaches the same answer for
  // a session that had ALREADY ended at page load (viewFor returns
  // offersAgentUI false there, with a notice beside the transcript).
  it("retracts the way back to the agent's view, because an ended session no longer has one", async () => {
    render(<SessionShell {...chatSelection({ chrome: {}, offersAgentUI: true })} />);
    expect(screen.getByTestId("session-shell-agent-view-switch")).toBeInTheDocument();
    await waitFor(() => expect(chatOnFrame).not.toBeNull());

    endsNow();

    await waitFor(() => expect(screen.queryByTestId("session-shell-agent-view-switch")).toBeNull());
  });

  // The OTHER arm. `endedLive` used to be reachable only while the transcript
  // was mounted, so a session that ended under an agent-defined view left
  // chrome saying "Resumed your session." indefinitely — the one disclosure
  // that arm could never reach. The agent-UI routes answer 410 for a terminal
  // session, and the view reports that up the same way the transcript reports
  // its own frame.
  //
  // The fixture answers 410 for the bindings POST specifically, because that is
  // the route the view actually drives: the live socket carries no terminal
  // frame for an ended session, and its handshake refusal would not be readable
  // as a status from the browser anyway.
  it("re-discloses an ended session from the AGENT-DEFINED view too, not only from the transcript", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) =>
        String(url).endsWith("/bindings")
          ? ({ ok: false, status: 410, json: async () => ({ error: "This session has ended." }) } as unknown as Response)
          : ({ ok: true, json: async () => ({ bindings: {}, timeline: [] }) } as unknown as Response),
      ),
    );

    render(<SessionShell {...withSelected({ chrome: {} })} />);
    expect(screen.getByTestId("session-shell-session-origin")).toHaveTextContent("Resumed your session.");

    await waitFor(() =>
      expect(screen.getByTestId("session-shell-session-origin")).toHaveTextContent(
        "This session has ended. You can start a new one.",
      ),
    );
    // …and the way back to a view that no longer exists goes with it.
    expect(screen.queryByTestId("session-shell-agent-view-switch")).toBeNull();
  });

  // The socket arm, which is the one the agent-defined view cannot reach on
  // its own. A session that ends while the AGENT VIEW is shown publishes a
  // `session_ended` frame like any other, but that frame used to reach nobody:
  // the shell's own ear on the session socket routed replies and decision
  // cards and nothing else, and the transcript — the only other reader — is
  // unmounted while the agent view is up. The 410 arm above covers only a
  // session already over when the view next asks the server something; this is
  // the end arriving while the viewer is simply looking at the page.
  it("re-discloses the session as ended from a live frame while the AGENT view is shown", async () => {
    render(<SessionShell {...withSelected({ chrome: {} })} />);
    expect(screen.getByTestId("session-shell-session-origin")).toHaveTextContent("Resumed your session.");
    await waitFor(() => expect(chatOnFrame).not.toBeNull());

    endsNow();

    await waitFor(() =>
      expect(screen.getByTestId("session-shell-session-origin")).toHaveTextContent(
        "This session has ended. You can start a new one.",
      ),
    );
  });

  // The negative control for the row above, and the reason the report is
  // matched on the STATUS rather than on "the request failed": a session is
  // over exactly when the server says 410. An authorization outage, a bad
  // gateway, a dropped connection are all failures of THIS REQUEST, and
  // disclosing "This session has ended" for any of them would be a false
  // statement about a live session — one the shell latches and cannot take
  // back, since a session cannot un-end.
  it("does not disclose an ended session for a bindings failure that is not the session's own end", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) =>
        String(url).endsWith("/bindings")
          ? ({ ok: false, status: 503, json: async () => ({ error: "Try again in a moment." }) } as unknown as Response)
          : ({ ok: true, json: async () => ({ bindings: {}, timeline: [] }) } as unknown as Response),
      ),
    );

    render(<SessionShell {...withSelected({ chrome: {} })} />);

    // Wait for the failure to have been HANDLED before asserting nothing
    // moved: the view's page-level "Data unavailable" notice is written from
    // the same catch that would have reported the end, so its presence is
    // proof the report path ran and declined — without it this row would pass
    // by asserting on a disclosure the failure had not reached yet.
    await screen.findByText("Data unavailable");
    expect(screen.getByTestId("session-shell-session-origin")).toHaveTextContent("Resumed your session.");
    expect(screen.getByTestId("session-shell-agent-view-switch")).toBeInTheDocument();
  });

  // `readOnly` and the disclosure above are derived from ONE value in the shell
  // (see `sessionOrigin`/`selectedEnded`), which is what makes the control the
  // start-a-replacement task hangs off this same value arm at the right moment.
  it("hands the transcript readOnly from the same value, so the composer is locked out", async () => {
    render(<SessionShell {...chatSelection({ chrome: {} })} />);
    await waitFor(() => expect(chatOnFrame).not.toBeNull());

    endsNow();

    const composer = (await screen.findByPlaceholderText("This conversation has ended.")) as HTMLTextAreaElement;
    expect(composer.disabled).toBe(true);
  });

  // The session's own row in the list still reads as live. This is the behavior
  // the transcript's own sidebar-refetch used to provide before the split; the
  // shell owns it now, and it costs one read rather than a 30s wait.
  it("re-reads the session list immediately rather than leaving the row stale until the next poll", async () => {
    const calls: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        calls.push(String(url));
        if (String(url).includes("/sessions/api/sessions")) {
          return {
            ok: true,
            json: async () => ({
              sessions: [
                { ns: "demo-ns", name: "alpha", class: "demo-agent", title: "Demo Agent", phase: "Finished", awaitingHuman: false, ended: true },
              ],
              notices: { unavailable: 0, truncated: false, bootstrapUnavailable: false },
              startableClasses: [],
            }),
          } as unknown as Response;
        }
        return { ok: true, json: async () => ({ bindings: {}, timeline: [] }) } as unknown as Response;
      }),
    );
    render(<SessionShell {...chatSelection({ chrome: {} })} />);
    await waitFor(() => expect(chatOnFrame).not.toBeNull());
    expect(calls.filter((u) => u.includes("/sessions/api/sessions"))).toHaveLength(0);

    endsNow();

    await waitFor(() => expect(calls.filter((u) => u.includes("/sessions/api/sessions"))).toHaveLength(1));
    await waitFor(() =>
      expect(screen.getByTestId("session-shell-session-row-phase-demo-ns/alpha")).toHaveTextContent("Finished"),
    );
  });
});

// Before the agent starts, the shell — not either view — says what is in the
// way, from the health watcher's session_startup frames, so a person on the
// agent-defined page and one on the transcript read the same line.
describe("SessionShell — the startup line before the agent starts", () => {
  function startupFrame(payload: Record<string, unknown>) {
    act(() => {
      chatOnFrame!({
        type: "session_startup",
        session: { namespace: "demo-ns", name: "alpha" },
        payload: { session: { namespace: "demo-ns", name: "alpha" }, ...payload },
      } as ChatFrame);
    });
  }

  it("shows the caption, adds still trying, and clears on started", async () => {
    render(<SessionShell {...chatSelection({ chrome: {} })} />);
    await waitFor(() => expect(chatOnFrame).not.toBeNull());
    expect(screen.queryByTestId("session-shell-startup")).toBeNull();

    startupFrame({ text: "Can't start yet — the agent was not allowed the resources it needs to start: exceeded quota" });
    expect(screen.getByTestId("session-shell-startup")).toHaveTextContent("exceeded quota");
    expect(screen.queryByTestId("session-shell-startup-still-trying")).toBeNull();

    startupFrame({ text: "Can't start yet — the agent was not allowed the resources it needs to start: exceeded quota", stillTrying: true });
    expect(screen.getByTestId("session-shell-startup-still-trying")).toBeInTheDocument();

    startupFrame({ text: "", started: true });
    expect(screen.queryByTestId("session-shell-startup")).toBeNull();
  });
});

// The details panel describes the SESSION, not the transcript, so it is chrome:
// it must be reachable from the agent-defined view too, and the content region
// must not be able to cover or remove it. It is also the only browser caller of
// the session-scoped detail route.
describe("SessionShell — the session-details panel", () => {
  function stubDetail(body: unknown, status = 200) {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (String(url).endsWith("/detail")) {
          return new Response(JSON.stringify(body), {
            status,
            headers: { "Content-Type": "application/json" },
          });
        }
        return { ok: true, json: async () => ({ bindings: {}, timeline: [] }) } as unknown as Response;
      }),
    );
  }

  const detail = {
    sessionId: "alpha",
    agentClass: "demo-agent",
    model: { provider: "anthropic", name: "claude-opus-4-8" },
    maxTokens: 120000,
    maxTurns: 40,
    maxDuration: "30m0s",
    owner: "user:me",
    createdAt: "2026-07-04T09:00:00Z",
    phase: "Running",
  };

  it("opens on the chrome control and shows the detail route's model, budget and owner", async () => {
    stubDetail(detail);
    render(<SessionShell {...withSelected({ chrome: {} })} />);

    await userEvent.click(screen.getByTestId("session-shell-session-info"));

    // Model renders via <ModelName>: provider and model name are separate text
    // nodes (a provider badge + a font-mono name), not one composed string.
    await waitFor(() => expect(screen.getByText("anthropic")).toBeInTheDocument());
    expect(screen.getByText("claude-opus-4-8")).toBeInTheDocument();
    expect(screen.getByText("user:me")).toBeInTheDocument();
    expect(screen.getByText("40")).toBeInTheDocument(); // max turns
    expect(screen.getByText("30m0s")).toBeInTheDocument(); // max duration
  });

  it("reads the detail route only once the viewer asks for it", () => {
    stubDetail(detail);
    render(<SessionShell {...withSelected({ chrome: {} })} />);
    const calls = (globalThis.fetch as unknown as { mock: { calls: unknown[][] } }).mock.calls;
    expect(calls.filter(([u]) => String(u).endsWith("/detail"))).toHaveLength(0);
  });

  // The route-level gate answers an unauthorized caller with an HTML page, so
  // the panel must not read a failure as JSON. It says what happened instead of
  // sitting on "Loading…".
  it("says the viewer has no access when the route gate refuses, rather than loading forever", async () => {
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (String(url).endsWith("/detail")) {
          return new Response("<!doctype html><title>Access denied</title>", {
            status: 403,
            headers: { "Content-Type": "text/html; charset=utf-8" },
          });
        }
        return { ok: true, json: async () => ({ bindings: {}, timeline: [] }) } as unknown as Response;
      }),
    );
    render(<SessionShell {...withSelected({ chrome: {} })} />);

    await userEvent.click(screen.getByTestId("session-shell-session-info"));

    await waitFor(() =>
      expect(screen.getByTestId("session-shell-session-info-error")).toHaveTextContent(
        "you do not have access to this session",
      ),
    );
    expect(errorSpy).toHaveBeenCalledWith("sessions: load session detail failed", {
      ns: "demo-ns",
      name: "alpha",
      status: 403,
    });
  });
});

// The browser tab is the shell's, and so is the ear that hears about it: the
// shell reads the session's own socket, decides whether a frame is worth a
// badge or an OS notification, and names it by the SELECTED session's agent —
// never by the session's object name, which must not reach a browser surface
// at all, let alone an OS one.
//
// Reading the socket directly is what makes these facts independent of WHICH
// view is on screen. A viewer sitting on the agent-defined page is told the
// agent replied, and told a card is waiting on them, exactly as a viewer on
// the transcript is.
describe("SessionShell — the tab title and away notifications", () => {
  function replyArrives(text: string) {
    act(() => {
      chatOnFrame!({
        type: "user_message",
        session: { namespace: "demo-ns", name: "alpha" },
        payload: { session: { namespace: "demo-ns", name: "alpha" }, text },
      } as ChatFrame);
    });
  }

  // The interaction prompt's payload is double-nested (types.ts's
  // InteractionRequestPayload) — the inner body is what carries the lead and
  // the requestRef the shell reports once per.
  function decisionArrives(requestRef: string, lead: string) {
    const S = { namespace: "demo-ns", name: "alpha" };
    act(() => {
      chatOnFrame!({
        type: "interaction_request",
        session: S,
        payload: {
          session: S,
          payload: {
            agentSessionRef: S,
            category: "plan_phase",
            requestRef,
            lead,
            actions: [{ id: "approve", label: "Approve", kind: "decision" }],
            audience: { scope: "requester" },
          },
        },
      } as ChatFrame);
    });
  }

  // A selection whose view is the agent-defined one: the shell seeds `shown`
  // to "ui", so the transcript is never mounted for it.
  function agentUISelection() {
    return withSelected({
      offersAgentUI: true,
      view: {
        kind: "agent-ui",
        ui: { ns: "demo-ns", name: "alpha", declaration: { view: { component: "ap:stack" } } },
        chat: { ns: "demo-ns", name: "alpha" },
      },
    });
  }

  it("titles the tab with the selected session's agent, never its object name", async () => {
    vi.spyOn(document, "hasFocus").mockReturnValue(true);
    render(<SessionShell {...chatSelection({ chrome: {} })} />);
    await waitFor(() => expect(document.title).toBe("Demo Agent · oap sessions"));
    expect(document.title).not.toContain("alpha");
  });

  it("fires a Notification and badges the title when a reply arrives while the tab is hidden", async () => {
    vi.stubGlobal("Notification", MockNotification);
    render(<SessionShell {...chatSelection({ chrome: {} })} />);
    await waitFor(() => expect(chatOnFrame).not.toBeNull());

    act(() => setTabHidden());
    replyArrives("the away reply");

    await waitFor(() => expect(MockNotification.instances).toHaveLength(1));
    expect(MockNotification.instances[0].title).toBe("Demo Agent");
    expect(MockNotification.instances[0].options?.body).toBe("the away reply");
    await waitFor(() => expect(document.title).toBe("(1) Demo Agent · oap sessions"));
  });

  // Browsers only honor Notification.requestPermission() from inside a real
  // user gesture, and the only gesture this page has is the viewer sending a
  // message — which happens in the TRANSCRIPT, not here. The shell arms the
  // request off that report; without the wiring the permission prompt would
  // never appear and the notification above could never fire for a first-time
  // viewer.
  it("asks for notification permission on the viewer's own send, the one gesture this page has", async () => {
    MockNotification.permission = "default";
    MockNotification.requestPermission = vi.fn(async () => "granted" as NotificationPermission);
    vi.stubGlobal("Notification", MockNotification);
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => ({ ok: true, json: async () => ({ timeline: [], routed: true, ended: false }) }) as unknown as Response),
    );
    render(<SessionShell {...chatSelection({ chrome: {} })} />);

    const composer = await screen.findByPlaceholderText("Message the agent…");
    await userEvent.type(composer, "hello");
    await userEvent.click(screen.getByTitle("Send"));

    await waitFor(() => expect(MockNotification.requestPermission).toHaveBeenCalledTimes(1));
    MockNotification.permission = "granted";
  });

  it("does NOT notify when the reply arrives while the tab is active", async () => {
    vi.spyOn(document, "hasFocus").mockReturnValue(true);
    vi.stubGlobal("Notification", MockNotification);
    render(<SessionShell {...chatSelection({ chrome: {} })} />);
    await waitFor(() => expect(chatOnFrame).not.toBeNull());

    replyArrives("an active reply");

    // The reply renders, but no OS notification and no unread badge.
    await waitFor(() =>
      expect(screen.getByTestId("session-shell-content-region")).toHaveTextContent("an active reply"),
    );
    expect(MockNotification.instances).toHaveLength(0);
    expect(document.title).toBe("Demo Agent · oap sessions");
  });

  // A card is a different kind of waiting from a reply: a turn that ended can
  // sit unread indefinitely, a card is BLOCKING the agent until someone
  // clicks. The headline says so; the body is the card's own lead.
  it("notifies with the card's lead when a prompt needing a decision arrives while the tab is hidden", async () => {
    vi.stubGlobal("Notification", MockNotification);
    render(<SessionShell {...chatSelection({ chrome: {} })} />);
    await waitFor(() => expect(chatOnFrame).not.toBeNull());

    act(() => setTabHidden());
    decisionArrives("req-1", "Approval needed");

    await waitFor(() => expect(MockNotification.instances).toHaveLength(1));
    // The whole headline, not a substring of it: the agent's name is what tells
    // a viewer with several sessions open WHICH one is blocked, and a
    // toContain would pass with it missing entirely.
    expect(MockNotification.instances[0].title).toBe("Demo Agent needs your decision");
    expect(MockNotification.instances[0].options?.body).toBe("Approval needed");
  });

  // A republished prompt (a restart resurfacing a parked card) refreshes the
  // card in place; notifying again would ping the viewer twice for one pending
  // decision.
  it("does not re-notify for a republished prompt with the same requestRef", async () => {
    vi.stubGlobal("Notification", MockNotification);
    render(<SessionShell {...chatSelection({ chrome: {} })} />);
    await waitFor(() => expect(chatOnFrame).not.toBeNull());

    act(() => setTabHidden());
    decisionArrives("req-1", "Approval needed");
    decisionArrives("req-1", "Approval needed");
    decisionArrives("req-2", "Connect your account");

    await waitFor(() => expect(MockNotification.instances).toHaveLength(2));
    expect(MockNotification.instances[1].options?.body).toBe("Connect your account");
  });

  // The case the shell's own ear exists for. While the agent-defined view is
  // shown the transcript is not mounted at all, so a notification that
  // depended on it would simply never fire — the viewer would sit on a page
  // that never said the agent had answered.
  it("notifies for a reply that arrives while the agent-defined view is shown and the transcript is unmounted", async () => {
    vi.stubGlobal("Notification", MockNotification);
    render(<SessionShell {...agentUISelection()} />);
    await waitFor(() => expect(chatOnFrame).not.toBeNull());
    // The transcript really is absent — without this the row would pass on a
    // build where ChatView is quietly still mounted behind the agent view.
    expect(screen.queryByTestId("chat-view")).toBeNull();

    act(() => setTabHidden());
    replyArrives("the away reply");

    await waitFor(() => expect(MockNotification.instances).toHaveLength(1));
    expect(MockNotification.instances[0].options?.body).toBe("the away reply");
    await waitFor(() => expect(document.title).toBe("(1) Demo Agent · oap sessions"));
  });
});

// The poll is the shell's only client-side data fetch, and the next task
// builds a start control on top of the list it maintains. Three properties
// matter and none of them are observable from a render test: that it fires at
// all, that it NEVER moves the selection (that is the URL's), and that a
// failure leaves what is on screen alone rather than blanking the list — the
// silent-failure shape this repo forbids.
describe("SessionShell — the session-list poll", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  function stubListFetch(impl: () => Promise<unknown>) {
    const calls: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        calls.push(String(url));
        if (String(url).includes("/sessions/api/sessions")) return impl() as Promise<Response>;
        return { ok: true, json: async () => ({ bindings: {} }) } as unknown as Response;
      }),
    );
    return calls;
  }

  it("refreshes the list on the interval and leaves the selection where the URL put it", async () => {
    vi.useFakeTimers();
    const calls = stubListFetch(async () => ({
      ok: true,
      json: async () => ({
        sessions: [{ ns: "demo-ns", name: "alpha", class: "demo-agent", title: "Demo Agent", phase: "Waiting for you", awaitingHuman: true, ended: false }],
        notices: { unavailable: 0, truncated: false, bootstrapUnavailable: false },
        startableClasses: [],
      }),
    }));

    render(<SessionShell {...shellGolden} />);
    // Nothing is fetched at first paint — the bootstrap props ARE the first
    // paint, so a mount-time fetch would be a second, redundant lookup.
    expect(calls.filter((u) => u.includes("/sessions/api/sessions"))).toHaveLength(0);

    await vi.advanceTimersByTimeAsync(10_000);

    expect(calls.filter((u) => u.includes("/sessions/api/sessions"))).toHaveLength(1);
    // The refreshed list replaced the rows: beta is gone, alpha's phase moved.
    expect(screen.queryByTestId("session-shell-session-row-demo-ns/beta")).toBeNull();
    expect(screen.getByTestId("session-shell-session-row-phase-demo-ns/alpha")).toHaveTextContent("Waiting for you");
    // ...and the selection did NOT move: the content region still shows the
    // selected session's declaration, and its row is still the current one.
    expect(screen.getByTestId("session-shell-session-row-demo-ns/alpha")).toHaveAttribute("aria-current", "page");
    expect(
      within(screen.getByTestId("session-shell-content-region")).getByRole("heading", { name: "Fleet Console" }),
    ).toBeInTheDocument();
  });

  it("alerts when a goal creates a session while another session is open", async () => {
    sessionStorage.clear();
    vi.useFakeTimers();
    stubListFetch(async () => ({ ok: true, json: async () => ({
      sessions: [...shellGolden.sessions, { ns: "demo-ns", name: "new-goal", uid: "new-uid", class: "demo-agent", title: "Demo Agent", phase: "Starting", awaitingHuman: false, ended: false, goalCreated: true, openingSummary: "Your stretch reminder started." }],
      notices: shellGolden.notices, startableClasses: shellGolden.startableClasses,
    }) }));
    render(<SessionShell {...shellGolden} />);
    expect(screen.queryByLabelText("New goal sessions")).toBeNull();
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(screen.getByLabelText("New goal sessions")).toHaveTextContent("Your stretch reminder started.");
    expect(screen.getByRole("link", { name: "Open session" })).toHaveAttribute("href", "/sessions?session=demo-ns%2Fnew-goal");
    expect(screen.getByTestId("session-shell-session-row-demo-ns/alpha")).toHaveAttribute("aria-current", "page");
    sessionStorage.clear();
  });

  it("keeps the rows on screen and logs when the refresh answers non-2xx", async () => {
    vi.useFakeTimers();
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    stubListFetch(async () => ({ ok: false, status: 503, json: async () => ({}) }));

    render(<SessionShell {...shellGolden} />);
    await vi.advanceTimersByTimeAsync(30_000);

    expect(screen.getByTestId("session-shell-session-row-demo-ns/alpha")).toBeInTheDocument();
    expect(screen.getByTestId("session-shell-session-row-demo-ns/beta")).toBeInTheDocument();
    expect(errorSpy).toHaveBeenCalledWith("sessions: session-list refresh failed", { status: 503 });
  });

  // A 200 whose body is not this endpoint's shape is NOT "you have no
  // sessions". `body.sessions ?? []` cannot tell them apart, and taking the
  // second for the first produces exactly the blank list the poll's own
  // comment forbids.
  it("keeps the rows on screen and logs when a 200 carries an unexpected body", async () => {
    vi.useFakeTimers();
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    stubListFetch(async () => ({ ok: true, json: async () => ({ unexpected: true }) }));

    render(<SessionShell {...shellGolden} />);
    await vi.advanceTimersByTimeAsync(30_000);

    expect(screen.getByTestId("session-shell-session-row-demo-ns/alpha")).toBeInTheDocument();
    expect(screen.getByTestId("session-shell-session-row-demo-ns/beta")).toBeInTheDocument();
    expect(errorSpy).toHaveBeenCalledWith(
      "sessions: session-list refresh returned an unexpected body; keeping the current list",
    );
  });
});

describe("SessionShell — no session selected", () => {
  const empty: ShellBootstrapProps = {
    subject: "alice@example.com",
    sessions: [],
    notices: { unavailable: 0, truncated: false, bootstrapUnavailable: false },
    startableClasses: [],
  };

  it("renders the empty list state and keeps identity visible", () => {
    render(<SessionShell {...empty} />);
    expect(screen.getByTestId("session-shell-empty-list")).toBeVisible();
    expect(screen.getByTestId("session-shell-identity")).toBeVisible();
    expect(screen.getByTestId("session-shell-no-selection")).toBeVisible();
  });

  // Opening the dashboard with nothing selected must not open a conversation:
  // no transcript, no socket, and nothing sent. This is what the old sidebar's
  // "lists sessions on load without starting one" guarded, now that the list
  // and the transcript live on the same page.
  it("opens no conversation at all when nothing is selected", () => {
    render(<SessionShell {...empty} />);
    expect(screen.queryByTestId("chat-view")).toBeNull();
    expect(chatSocketFor).toBeNull();
    const calls = (globalThis.fetch as unknown as { mock: { calls: unknown[][] } }).mock.calls;
    expect(calls.filter(([u]) => String(u).endsWith("/message"))).toHaveLength(0);
  });

  it("titles the tab with the bare product name when there is no session to name", async () => {
    render(<SessionShell {...empty} />);
    await waitFor(() => expect(document.title).toBe("oap sessions"));
  });

  it("makes no session-origin disclosure when there is no session to disclose", () => {
    render(<SessionShell {...empty} />);
    expect(screen.queryByTestId("session-shell-session-origin")).toBeNull();
  });
});

describe("SessionShell — the session list", () => {
  it("renders one row per listed session, each addressing that session by URL", () => {
    render(<SessionShell {...shellGolden} />);
    // querySelectorAll, not getAllByRole: the golden requests collapsed
    // chrome, so the whole sidebar carries `hidden` and role queries — which
    // exclude inaccessible subtrees by design — would find nothing. DOM
    // presence is exactly what "never suppressible" is about here.
    const list = screen.getByTestId("session-shell-session-list");
    expect(list.querySelectorAll("a[href]")).toHaveLength(2);
    expect(screen.getByTestId("session-shell-session-row-demo-ns/alpha")).toHaveAttribute(
      "href",
      "/sessions?session=demo-ns%2Falpha",
    );
    expect(screen.getByTestId("session-shell-session-row-demo-ns/beta")).toHaveAttribute(
      "href",
      "/sessions?session=demo-ns%2Fbeta",
    );
  });

  it("marks the selected row as the current page, and only that row", () => {
    render(<SessionShell {...shellGolden} />);
    expect(screen.getByTestId("session-shell-session-row-demo-ns/alpha")).toHaveAttribute("aria-current", "page");
    expect(screen.getByTestId("session-shell-session-row-demo-ns/beta")).not.toHaveAttribute("aria-current");
  });

  it("renders each row's human phase copy, including the waiting-on-a-human one", () => {
    render(<SessionShell {...shellGolden} />);
    expect(screen.getByTestId("session-shell-session-row-phase-demo-ns/alpha")).toHaveTextContent("Active");
    expect(screen.getByTestId("session-shell-session-row-phase-demo-ns/beta")).toHaveTextContent("Waiting for you");
  });

  // notices.unavailable is non-zero in the golden. An incomplete list that
  // said nothing would be indistinguishable from a complete one.
  it("states that some sessions could not be loaded rather than silently omitting them", () => {
    render(<SessionShell {...shellGolden} />);
    expect(screen.getByTestId("session-shell-list-notices")).toHaveTextContent(
      "2 sessions could not be loaded and are not shown.",
    );
  });
});

// testdata/origins.golden.json is agentui.AllBranches, written from Go (see
// origins_golden_test.go). Driving every value through the shell is what makes
// a branch ADDED in Go with no case written in sessionOriginCopy fail here:
// the new value falls through to the defensive default, whose copy this test
// requires to be distinct from every real branch's.
describe("SessionShell — every ladder branch discloses distinctly", () => {
  const branches = originsGolden as string[];

  function disclosureFor(origin: string): string {
    const { unmount } = render(<SessionShell {...withSelected({ sessionOrigin: origin, chrome: {} })} />);
    const text = screen.getByTestId("session-shell-session-origin").textContent ?? "";
    unmount();
    return text;
  }

  it("gives each branch copy no other branch shares, and none the defensive default's", () => {
    expect(branches.length).toBeGreaterThan(0);
    const defaultCopy = disclosureFor("a-branch-with-no-case");
    const seen = new Map<string, string>();
    for (const b of branches) {
      const copy = disclosureFor(b);
      expect(copy, `branch ${b} fell through to the defensive default copy`).not.toBe(defaultCopy);
      expect(copy.length, `branch ${b} discloses nothing`).toBeGreaterThan(0);
      for (const [other, otherCopy] of seen) {
        expect(copy, `branches ${b} and ${other} disclose identically`).not.toBe(otherCopy);
      }
      seen.set(b, copy);
    }
  });

  // The disclosure must be human copy, not the branch value echoed back.
  // Note this is NOT a ban on the branch's WORD appearing: "asleep" and
  // "ended" are ordinary English and the copy for those branches uses them.
  // What is banned is echoing the value itself, and any control-plane
  // vocabulary the browser must never see.
  it("never echoes the raw branch value or control-plane vocabulary into the disclosure", () => {
    for (const b of branches) {
      const copy = disclosureFor(b);
      expect(copy.trim()).not.toBe(b);
      expect(copy).not.toMatch(/AgentSession|AgentClass|WakeEligible|CRD/i);
    }
  });
});

// "Hideable, never suppressible" (Decision 3, rule 3) — the shell's own half
// of the guarantee Chrome.test.tsx proves for the component. The list is a
// chrome region now, so it is in this sweep too.
describe("SessionShell — collapsed means visually reduced, NOT absent from the DOM", () => {
  it.each(chromeRegionTestIds)("keeps %s queryable in the DOM while chrome is collapsed", (testId) => {
    render(<SessionShell {...shellGolden} />);
    expect(screen.getByTestId("session-shell-identity")).not.toBeVisible();
    expect(screen.queryByTestId(testId)).not.toBeNull();
    expect(screen.getByTestId(testId)).toBeInTheDocument();
  });

  it("keeps the content region and its contents mounted while chrome is collapsed", () => {
    render(<SessionShell {...shellGolden} />);
    expect(
      within(screen.getByTestId("session-shell-content-region")).getByRole("heading", { name: "Fleet Console" }),
    ).toBeVisible();
  });
});

// The structural guarantee behind "hidden never means absent": every chrome
// region is a DOM SIBLING of the content container, never a descendant of it,
// so nothing an agent declares can reach, restyle, cover or remove one.
//
// It is asserted against a deep, real agent-authored tree rather than the
// golden's single heading, because a guarantee about what agent content cannot
// reach proves nothing when the content is one node. Moved here from the
// agent-UI suite: the view has one host now, and only the host composes the
// regions this is about.
describe("SessionShell — chrome regions are siblings of the content region, not descendants", () => {
  const nestedTree = {
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
        { component: "ap:markdown", props: { body: "Refreshed **hourly**." } },
      ],
    },
  };

  it("keeps chrome outside the content region, unsuppressed, alongside a real rendered tree", () => {
    const selected = shellGolden.selected as SelectedView;
    render(
      <SessionShell
        {...withSelected({
          chrome: {},
          view: { ...selected.view, ui: { ...selected.view.ui!, declaration: nestedTree as SelectedView["view"]["ui"]["declaration"] } },
        })}
      />,
    );

    const region = screen.getByTestId("session-shell-content-region");
    expect(within(region).getByRole("heading", { name: "Fleet Console" })).toBeVisible();
    for (const id of chromeRegionTestIds) {
      expect(region.querySelector(`[data-testid="${id}"]`)).toBeNull();
      expect(screen.getByTestId(id)).toBeVisible();
    }
  });
});

// The start controls. Two of them, deliberately: the dashboard control names
// a class (there is no selected session to read one from) and POSTs to the
// derived-authorization route; the ended-session control names nothing and
// POSTs to the session-scoped route, whose gate is interact on the session
// the viewer is already looking at.
describe("SessionShell — starting a session", () => {
  // startProps is the golden with a selection dropped and the two start facts
  // set explicitly. Both are required, and neither implies the other.
  function startProps(overrides: Partial<ShellBootstrapProps> = {}): ShellBootstrapProps {
    return {
      ...shellGolden,
      selected: undefined,
      startableClasses: [
        { ns: "demo-ns", class: "demo-agent", title: "Demo Agent", startable: true },
        { ns: "demo-ns", class: "other-agent", title: "Other Agent", startable: true },
      ],
      canStartSessions: true,
      ...overrides,
    };
  }

  // Shared by both describes below that need the picker to arrive already
  // primed from the URL (`?new=1` alone, and `?new=1` plus a preselect):
  // replaces the address so the shell's one-shot reads see it, and returns a
  // restore so a later test in this file does not inherit it.
  function withSearch(search: string) {
    const original = window.location.href;
    window.history.replaceState({}, "", search === "" ? "/sessions" : `/sessions${search}`);
    return () => window.history.replaceState({}, "", original);
  }

  it("renders no start control and explains why when the viewer has no startable classes", () => {
    render(<SessionShell {...startProps({ startableClasses: [], canStartSessions: true })} />);

    expect(screen.queryByTestId("session-shell-new-session")).toBeNull();
    const explanation = screen.getByTestId("session-shell-no-startable-classes");
    expect(explanation).toBeInTheDocument();
    // The empty dashboard must explain how access actually arrives, in copy
    // that names no CRD kind, no permission and no Kubernetes object.
    expect(explanation.textContent ?? "").toMatch(/added to a session/i);
    expect(explanation).toHaveTextContent(/install at least one agent/i);
    expect(explanation.textContent ?? "").not.toMatch(
      /AgentSession|AgentClass|SpiceDB|kubectl|interact|namespace|CRD/i,
    );
  });

  it("explains a New chat request with no available agents and lets the viewer dismiss it", async () => {
    const restore = withSearch("?new=1");
    try {
      const user = userEvent.setup();
      const props = startProps({ startableClasses: [], canStartSessions: true });
      const { rerender } = render(<SessionShell {...props} />);
      const dialog = screen.getByRole("dialog");
      expect(dialog).toHaveTextContent("No agents available");
      expect(dialog).toHaveTextContent(/install at least one agent/i);
      expect(screen.queryByTestId("session-shell-new-session-class")).toBeNull();
      await user.click(within(dialog).getByRole("button", { name: "Got it", exact: true }));
      rerender(<SessionShell {...props} />);
      expect(screen.queryByRole("dialog")).toBeNull();
    } finally {
      restore();
    }
  });

  // The two facts are independent, and the control needs BOTH. A viewer with
  // standing on a webd that mounts no start route must not be offered a button
  // whose every press 404s.
  it("renders no start control when the route was never mounted, even with startable classes", () => {
    render(<SessionShell {...startProps({ canStartSessions: false })} />);
    expect(screen.queryByTestId("session-shell-new-session")).toBeNull();
  });

  // ?new=1 is what the desktop menu bar's "New chat" links to. Without it that
  // item lands on a list with the control merely available, which is not what
  // a menu entry named "New chat" promises.
  describe("?new=1 opens the picker on arrival", () => {
    it("opens the picker on first paint", () => {
      const restore = withSearch("?new=1");
      try {
        render(<SessionShell {...startProps()} />);
        expect(screen.getByTestId("session-shell-new-session-class")).toBeInTheDocument();
      } finally {
        restore();
      }
    });

    it("leaves the picker closed without it — the default arrival is still a list", () => {
      const restore = withSearch("");
      try {
        render(<SessionShell {...startProps()} />);
        // The trigger is present; the picker's own controls are not.
        expect(screen.getByTestId("session-shell-new-session")).toBeInTheDocument();
        expect(screen.queryByTestId("session-shell-new-session-class")).toBeNull();
      } finally {
        restore();
      }
    });

    it("stays closed once dismissed — it is a request to open, not a mode", async () => {
      const restore = withSearch("?new=1");
      try {
        const user = userEvent.setup();
        render(<SessionShell {...startProps()} />);
        expect(screen.getByTestId("session-shell-new-session-class")).toBeInTheDocument();

        await user.keyboard("{Escape}");

        // The query string is still ?new=1. A re-read per render would
        // resurrect the dialog here, which is the bug the useMemo prevents.
        expect(screen.queryByTestId("session-shell-new-session-class")).toBeNull();
      } finally {
        restore();
      }
    });
  });

  // A link (the builder page's "Try it yourself") can ask for the picker
  // open with an agent already named and a message already typed. `ns` and
  // `agentClass` name the pair the SAME way a start request does; `prompt`
  // seeds the message box. All three are read once, exactly like `new`.
  describe("?new=1&ns=&agentClass=&prompt= preselects the agent", () => {
    it("selects the named class and prefills the prompt", async () => {
      const restore = withSearch(
        "?new=1&ns=ws-1&agentClass=demo-agent&prompt=linear%20issues%20for%20PR%20%231234",
      );
      try {
        render(
          <SessionShell
            {...startProps({
              startableClasses: [
                { ns: "default", class: "other", title: "Other", startable: true },
                { ns: "ws-1", class: "demo-agent", title: "Demo", startable: true },
              ],
            })}
          />,
        );
        const select = await screen.findByRole("combobox");
        expect((select as HTMLSelectElement).value).toBe("1");
        const textbox = await screen.findByRole("textbox");
        expect((textbox as HTMLTextAreaElement).value).toContain("linear issues for PR #1234");
      } finally {
        restore();
      }
    });

    // The named pair is not among what this viewer may start — a stale link,
    // a typo, or an agent this viewer lost standing on since the link was
    // made. Refusing on screen is the honest answer; silently falling back to
    // whatever is first in the list would start a session nobody asked for.
    it("refuses a class that is not startable from here instead of picking another", async () => {
      const restore = withSearch("?new=1&ns=ws-9&agentClass=nope");
      try {
        render(
          <SessionShell
            {...startProps({
              startableClasses: [{ ns: "default", class: "other", title: "Other", startable: true }],
            })}
          />,
        );
        expect(await screen.findByText(/can't start that agent from here/i)).toBeInTheDocument();
        expect(screen.queryByRole("button", { name: /^start/i })).toBeDisabled();
      } finally {
        restore();
      }
    });

    // A refusal explains a LINK, not a lock on the picker: once the viewer
    // deliberately picks a different, startable class themselves, the link's
    // named pair is no longer what would be submitted, so the banner — and
    // the disabled submit it drove — must clear, and the class that actually
    // posts is the one the viewer picked, not the refused one.
    it("clears the refusal once the viewer picks a different, startable class themselves", async () => {
      const user = userEvent.setup();
      const seen: { url: string; body: unknown }[] = [];
      vi.stubGlobal(
        "fetch",
        vi.fn(async (url: string, init?: RequestInit) => {
          seen.push({ url: String(url), body: JSON.parse(String(init?.body)) });
          return {
            ok: true,
            json: async () => ({ ns: "default", name: "another-1a2b3c4d", href: "/sessions?session=default%2Fnew" }),
          } as unknown as Response;
        }),
      );
      const restore = withSearch("?new=1&ns=ws-9&agentClass=nope");
      try {
        render(
          <SessionShell
            {...startProps({
              startableClasses: [
                { ns: "default", class: "other", title: "Other", startable: true },
                { ns: "default", class: "another", title: "Another", startable: true },
              ],
            })}
          />,
        );
        expect(await screen.findByText(/can't start that agent from here/i)).toBeInTheDocument();

        await user.selectOptions(screen.getByTestId("session-shell-new-session-class"), "1");
        await user.type(screen.getByTestId("session-shell-new-session-prompt"), "hello");

        expect(screen.queryByTestId("session-shell-preselect-refused")).toBeNull();
        const submit = screen.getByTestId("session-shell-new-session-submit") as HTMLButtonElement;
        expect(submit.disabled).toBe(false);

        await user.click(submit);
        await waitFor(() => expect(seen).toHaveLength(1));
        expect(seen[0].body).toEqual({ ns: "default", agentClass: "another", prompt: "hello" });
      } finally {
        restore();
      }
    });
  });

  it("lists each startable pair's title when the control renders", async () => {
    const user = userEvent.setup();
    render(<SessionShell {...startProps()} />);

    await user.click(screen.getByTestId("session-shell-new-session"));

    const picker = screen.getByTestId("session-shell-new-session-class") as HTMLSelectElement;
    expect([...picker.options].map((o) => o.textContent)).toEqual(["Demo Agent", "Other Agent"]);
  });

  // The compounding defect: a viewer's standing usually comes from a session
  // in a channel's namespace, which this server holds no create RBAC in — so
  // on a shared cluster the picker used to be populated with entries whose
  // every press 500s, with nothing saying why. The entry stays (the viewer DOES
  // have access to that agent) and the limit is stated instead.
  it("keeps an agent this server cannot start in the picker, marked, with the reason shown and the submit refused", async () => {
    const user = userEvent.setup();
    render(
      <SessionShell
        {...startProps({
          startableClasses: [{ ns: "team-a", class: "support-bot", title: "Support Bot", startable: false }],
        })}
      />,
    );

    await user.click(screen.getByTestId("session-shell-new-session"));

    const picker = screen.getByTestId("session-shell-new-session-class") as HTMLSelectElement;
    expect([...picker.options].map((o) => o.textContent)).toEqual(["Support Bot — cannot start here"]);

    const reason = screen.getByTestId("session-shell-new-session-unstartable");
    expect(reason).toBeVisible();
    // The viewer must learn WHY, in copy that names no namespace, no
    // permission, no CRD kind and no API object — and that does not tell a
    // viewer who demonstrably holds access that they have none.
    expect(reason.textContent ?? "").toMatch(/cannot start new sessions/i);
    expect(reason.textContent ?? "").not.toMatch(
      /AgentSession|AgentClass|Channel|Secret|SpiceDB|kubectl|RBAC|Role|namespace|team-a|Forbidden|CRD/i,
    );
    expect(reason.textContent ?? "").not.toMatch(/do not have access|no access/i);

    await user.type(screen.getByTestId("session-shell-new-session-prompt"), "please help");
    expect((screen.getByTestId("session-shell-new-session-submit") as HTMLButtonElement).disabled).toBe(true);
  });

  // The negative control for the row above: a reachable pair shows no reason
  // line and submits normally, so the marker cannot be a permanent fixture.
  it("shows no such reason, and submits, for an agent this server can start", async () => {
    const user = userEvent.setup();
    render(<SessionShell {...startProps()} />);

    await user.click(screen.getByTestId("session-shell-new-session"));
    expect(screen.queryByTestId("session-shell-new-session-unstartable")).toBeNull();

    await user.type(screen.getByTestId("session-shell-new-session-prompt"), "please help");
    expect((screen.getByTestId("session-shell-new-session-submit") as HTMLButtonElement).disabled).toBe(false);
  });

  // An agent whose point is its own view was being started through a dialog
  // built for conversation: it demanded an opening message before the button
  // would enable, so opening a dashboard meant first describing what you were
  // about to be shown.
  describe("an agent that declares its own view is opened, not conversed with", () => {
    function uiProps(overrides?: Record<string, unknown>) {
      return startProps({
        startableClasses: [{ ns: "demo-ns", class: "demo-agent", title: "Demo Agent", startable: true, offersUI: true }],
        ...overrides,
      });
    }

    it("submits with no message at all, and names the view in the button", async () => {
      const user = userEvent.setup();
      render(<SessionShell {...uiProps()} />);
      await user.click(screen.getByTestId("session-shell-new-session"));

      const submit = screen.getByTestId("session-shell-new-session-submit") as HTMLButtonElement;
      expect(submit.disabled).toBe(false);
      expect(submit.textContent ?? "").toMatch(/open ui/i);
    });

    // The message is folded away, not removed: it is how a viewer opens the
    // view somewhere other than its defaults, and the only route by which a
    // starting sentence can reach the agent at all.
    it("keeps the message available behind a disclosure", async () => {
      const user = userEvent.setup();
      render(<SessionShell {...uiProps()} />);
      await user.click(screen.getByTestId("session-shell-new-session"));

      expect(screen.getByTestId("session-shell-new-session-prompt-disclosure")).toBeInTheDocument();
      await user.type(screen.getByTestId("session-shell-new-session-prompt"), "last seven days");
      expect((screen.getByTestId("session-shell-new-session-prompt") as HTMLTextAreaElement).value).toBe("last seven days");
    });

    // The address asks for the view BY NAME. Left to the shell's default, a
    // view that is momentarily unresolvable falls back to the transcript
    // silently — landing the viewer somewhere they did not ask to be, with no
    // disclosure that the view they asked for was the thing that failed.
    // Rendered directly rather than through the shell, which does not expose
    // onNavigate — this is the one assertion that needs to observe the address
    // a successful start sends the browser to.
    it("navigates to the agent view explicitly", async () => {
      const user = userEvent.setup();
      vi.stubGlobal("fetch", vi.fn(async () => ({
        ok: true,
        json: async () => ({ ns: "demo-ns", name: "demo-agent-1", href: "/sessions?session=demo-ns%2Fdemo-agent-1" }),
      }) as Response));
      const navigated: string[] = [];

      render(
        <NewSessionDialog
          classes={[{ ns: "demo-ns", class: "demo-agent", title: "Demo Agent", startable: true, offersUI: true }]}
          onNavigate={(href) => navigated.push(href)}
          openInitially
        />,
      );
      await user.click(screen.getByTestId("session-shell-new-session-submit"));

      await waitFor(() => expect(navigated).toHaveLength(1));
      expect(navigated[0]).toContain("view=ui");
      expect(navigated[0]).toContain("demo-ns%2Fdemo-agent-1");
    });

    // The same route for an agent with no view of its own must NOT acquire the
    // parameter: asking for a view that was never declared is answered with a
    // "no custom view" card, which is a worse landing than the transcript.
    it("does not ask for the agent view when the agent declares none", async () => {
      const user = userEvent.setup();
      vi.stubGlobal("fetch", vi.fn(async () => ({
        ok: true,
        json: async () => ({ ns: "demo-ns", name: "demo-agent-1", href: "/sessions?session=demo-ns%2Fdemo-agent-1" }),
      }) as Response));
      const navigated: string[] = [];

      render(
        <NewSessionDialog
          classes={[{ ns: "demo-ns", class: "demo-agent", title: "Demo Agent", startable: true }]}
          onNavigate={(href) => navigated.push(href)}
          openInitially
        />,
      );
      await user.type(screen.getByTestId("session-shell-new-session-prompt"), "please help");
      await user.click(screen.getByTestId("session-shell-new-session-submit"));

      await waitFor(() => expect(navigated).toHaveLength(1));
      expect(navigated[0]).not.toContain("view=");
    });

    // The negative control: an agent with no view of its own keeps the
    // conversation-shaped flow, message required and all.
    it("leaves a transcript-only agent's dialog exactly as it was", async () => {
      const user = userEvent.setup();
      render(<SessionShell {...startProps()} />);
      await user.click(screen.getByTestId("session-shell-new-session"));

      const submit = screen.getByTestId("session-shell-new-session-submit") as HTMLButtonElement;
      expect(submit.disabled).toBe(true); // no message yet
      expect(submit.textContent ?? "").toMatch(/start session/i);
      expect(screen.queryByTestId("session-shell-new-session-prompt-disclosure")).toBeNull();
    });
  });

  // The ONLY mitigation for the missing server-side double-submit guard. It
  // must latch on the click, not after the response.
  it("disables the submit on click and leaves it disabled until the navigation happens", async () => {
    const user = userEvent.setup();
    let resolve!: (v: Response) => void;
    const pending = new Promise<Response>((r) => {
      resolve = r;
    });
    const calls: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        calls.push(String(url));
        return pending;
      }),
    );

    render(<SessionShell {...startProps()} />);
    await user.click(screen.getByTestId("session-shell-new-session"));
    await user.type(screen.getByTestId("session-shell-new-session-prompt"), "do the thing");

    const submit = screen.getByTestId("session-shell-new-session-submit") as HTMLButtonElement;
    expect(submit.disabled).toBe(false);
    await user.click(submit);

    expect(submit.disabled).toBe(true);
    // A second click while the first is in flight must reach no second POST.
    await user.click(submit);
    expect(calls.filter((u) => u.includes("/sessions/api/start"))).toHaveLength(1);

    resolve({
      ok: true,
      json: async () => ({ ns: "demo-ns", name: "demo-agent-1a2b3c4d", href: "/sessions?session=demo-ns%2Fnew" }),
    } as unknown as Response);
    await waitFor(() => expect(submit.disabled).toBe(true));
  });

  it("POSTs the selected pair and the prompt to the derived-authorization route", async () => {
    const user = userEvent.setup();
    const seen: { url: string; body: unknown }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init?: RequestInit) => {
        seen.push({ url: String(url), body: JSON.parse(String(init?.body)) });
        return {
          ok: true,
          json: async () => ({ ns: "demo-ns", name: "other-agent-9f9f", href: "/sessions?session=demo-ns%2Fnew" }),
        } as unknown as Response;
      }),
    );

    render(<SessionShell {...startProps()} />);
    await user.click(screen.getByTestId("session-shell-new-session"));
    await user.selectOptions(screen.getByTestId("session-shell-new-session-class"), "1");
    await user.type(screen.getByTestId("session-shell-new-session-prompt"), "hello");
    await user.click(screen.getByTestId("session-shell-new-session-submit"));

    await waitFor(() => expect(seen).toHaveLength(1));
    expect(seen[0].url).toBe("/sessions/api/start");
    // The SECOND pair, not the first: a control that ignored the picker would
    // still post something and still 200.
    expect(seen[0].body).toEqual({ ns: "demo-ns", agentClass: "other-agent", prompt: "hello" });
  });

  it("surfaces the server's own refusal copy rather than a generic failure", async () => {
    const user = userEvent.setup();
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          ({
            ok: false,
            status: 409,
            json: async () => ({ error: "That agent is not available to start a session with." }),
          }) as unknown as Response,
      ),
    );

    render(<SessionShell {...startProps()} />);
    await user.click(screen.getByTestId("session-shell-new-session"));
    await user.type(screen.getByTestId("session-shell-new-session-prompt"), "hello");
    await user.click(screen.getByTestId("session-shell-new-session-submit"));

    const err = await screen.findByTestId("session-shell-new-session-error");
    expect(err).toHaveTextContent("That agent is not available to start a session with.");
    // ...and the submit is usable again, so a transient refusal is not a dead end.
    expect((screen.getByTestId("session-shell-new-session-submit") as HTMLButtonElement).disabled).toBe(false);
  });

  // The replacement control gates on the SHELL's live answer for whether the
  // session is over. These two rows fix the bootstrap-time behaviour; the row
  // after them is what makes the gate discriminating, since a control that
  // read only the frozen bootstrap prop would pass both of these and fail that
  // one.
  it("renders the replacement control for an ended session", () => {
    render(<SessionShell {...chatSelection({ sessionOrigin: "ended" })} />);
    expect(screen.getByTestId("session-shell-start-replacement")).toBeInTheDocument();
  });

  it("withholds the replacement control for a live session", () => {
    render(<SessionShell {...chatSelection({ sessionOrigin: "attached" })} />);
    expect(screen.queryByTestId("session-shell-start-replacement")).toBeNull();
  });

  // The shell's own live answer is what gates this control, so a session that
  // ENDS while the viewer is watching must surface it with no reload. This is
  // the row that makes the shell-state wiring (not merely the frozen prop)
  // load-bearing: ChatView's own readOnly latches from the same frame, so
  // without an independent consumer the wiring could be removed and the
  // readOnly assertions alone would not notice.
  it("surfaces the replacement control when the session ends live, with no reload", async () => {
    render(<SessionShell {...chatSelection({ sessionOrigin: "attached" })} />);
    expect(screen.queryByTestId("session-shell-start-replacement")).toBeNull();
    await waitFor(() => expect(chatOnFrame).not.toBeNull());

    // The REAL session_ended frame shape (pkg/web/webui/chat/ui/types.ts), not a
    // hand-shortened literal: a control that surfaced on any frame at all
    // would pass against a made-up one.
    act(() => {
      chatOnFrame!({
        type: "session_ended",
        session: { namespace: "demo-ns", name: "alpha" },
        payload: { session: { namespace: "demo-ns", name: "alpha" }, reason: "succeeded" },
      } as ChatFrame);
    });

    await waitFor(() => expect(screen.getByTestId("session-shell-start-replacement")).toBeInTheDocument());
  });

  it("POSTs the replacement to the session-scoped start route, naming no class", async () => {
    const user = userEvent.setup();
    const seen: { url: string; body: unknown }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init?: RequestInit) => {
        seen.push({ url: String(url), body: init?.body ? JSON.parse(String(init.body)) : null });
        return {
          ok: true,
          json: async () => ({ ns: "demo-ns", name: "demo-agent-4444", href: "/sessions?session=demo-ns%2Fnew" }),
        } as unknown as Response;
      }),
    );

    render(<SessionShell {...chatSelection({ sessionOrigin: "ended" })} />);
    await user.click(screen.getByTestId("session-shell-start-replacement"));

    // Filtered to the start route: the transcript reads its own messages
    // endpoint on mount, so an unfiltered count would be asserting the wrong
    // thing and would break whenever an unrelated read is added.
    const starts = () => seen.filter((s) => s.url.endsWith("/start"));
    await waitFor(() => expect(starts()).toHaveLength(1));
    expect(starts()[0].url).toBe("/agent-ui/demo-ns/alpha/start");
    // No class on the wire: this route reads it from the server's own copy of
    // the session in the URL, which is what makes its narrower gate correct.
    expect(starts()[0].body).not.toHaveProperty("agentClass");
  });
});

// The start response's wire shape. ONE golden, three readers: both Go
// producers (pkg/web/webui/sessions' StartResponse and pkg/web/webui/agentui's
// startResponse) and the single parse helper below. A rename on either
// producer fails its own Go golden test; a rename here fails this one.
describe("start.golden.json — the shared start-response contract", () => {
  it("parses into {ns, name, href} with all three fields non-empty", () => {
    const parsed = parseStartResponse(startGolden);
    expect(parsed.ns).not.toBe("");
    expect(parsed.name).not.toBe("");
    expect(parsed.href).not.toBe("");
    // The href is a same-origin RELATIVE path addressing the session shell —
    // never an absolute URL a caller would have to trust.
    expect(parsed.href.startsWith("/sessions?session=")).toBe(true);
    const selection = new URL(parsed.href, "https://placeholder.invalid").searchParams.get("session");
    expect(selection).toBe(`${parsed.ns}/${parsed.name}`);
  });

  it("refuses a body missing any of the three fields rather than navigating to undefined", () => {
    for (const key of ["ns", "name", "href"] as const) {
      const partial: Record<string, unknown> = { ...startGolden };
      delete partial[key];
      expect(() => parseStartResponse(partial)).toThrow();
    }
  });
});

// The re-enable rule. It is the difference between "a refusal you can retry"
// and "we do not know whether a session was created" — and getting it wrong is
// the only sequence in which a user who intended one session gets two without
// doing anything wrong, because neither start route carries an idempotency
// key.
describe("SessionShell — a failed start decides whether retrying is safe", () => {
  function startProps(overrides: Partial<ShellBootstrapProps> = {}): ShellBootstrapProps {
    return {
      ...shellGolden,
      selected: undefined,
      startableClasses: [{ ns: "demo-ns", class: "demo-agent", title: "Demo Agent", startable: true }],
      canStartSessions: true,
      ...overrides,
    };
  }

  async function openAndSubmit(user: ReturnType<typeof userEvent.setup>) {
    await user.click(screen.getByTestId("session-shell-new-session"));
    await user.type(screen.getByTestId("session-shell-new-session-prompt"), "hello");
    await user.click(screen.getByTestId("session-shell-new-session-submit"));
  }

  // The server ANSWERED. By construction no session was created, so the button
  // comes back and the viewer can fix the problem and try again.
  it("re-enables the submit for a refusal the server answered", async () => {
    const user = userEvent.setup();
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          ({
            ok: false,
            status: 409,
            json: async () => ({ error: "That agent is not available to start a session with." }),
          }) as unknown as Response,
      ),
    );

    render(<SessionShell {...startProps()} />);
    await openAndSubmit(user);

    await screen.findByTestId("session-shell-new-session-error");
    expect((screen.getByTestId("session-shell-new-session-submit") as HTMLButtonElement).disabled).toBe(false);
  });

  // No answer at all. The request may have succeeded server-side — the POST
  // waits on the session becoming ready, so a proxy or an impatient user can
  // cut it off long after the session was created. Re-enabling here is what
  // turns one intent into two sessions.
  it("keeps the submit latched when the request never completed, and says the session may already exist", async () => {
    const user = userEvent.setup();
    const calls: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => {
        calls.push(String(url));
        return Promise.reject(new TypeError("Failed to fetch"));
      }),
    );

    render(<SessionShell {...startProps()} />);
    await openAndSubmit(user);

    const err = await screen.findByTestId("session-shell-new-session-error");
    expect(err.textContent ?? "").toMatch(/could not confirm|reload/i);
    const submit = screen.getByTestId("session-shell-new-session-submit") as HTMLButtonElement;
    expect(submit.disabled).toBe(true);

    // And clicking again reaches no second POST.
    await user.click(submit);
    expect(calls.filter((u) => u.includes("/sessions/api/start"))).toHaveLength(1);
  });

  // A 200 whose body we cannot address is also indeterminate — the session
  // almost certainly EXISTS, we simply cannot navigate to it.
  it("keeps the submit latched for a 200 whose answer does not address a session", async () => {
    const user = userEvent.setup();
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => ({ ok: true, json: async () => ({ ns: "demo-ns" }) }) as unknown as Response),
    );

    render(<SessionShell {...startProps()} />);
    await openAndSubmit(user);

    await screen.findByTestId("session-shell-new-session-error");
    expect((screen.getByTestId("session-shell-new-session-submit") as HTMLButtonElement).disabled).toBe(true);
  });
});

// classOptionLabels — the picker must not ask the viewer to choose blind.
describe("classOptionLabels", () => {
  it("shows the title alone when it is unambiguous", () => {
    expect(
      classOptionLabels([
        { ns: "demo-ns", class: "demo-agent", title: "Demo Agent", startable: true },
        { ns: "demo-ns", class: "other-agent", title: "Other Agent", startable: true },
      ]),
    ).toEqual(["Demo Agent", "Other Agent"]);
  });

  // The whole reason the authorization rule checks the (namespace, class) PAIR
  // is that the same-named agent in another namespace may be an entirely
  // different agent with entirely different tools. Two identical labels ask the
  // viewer to pick one of them blind.
  it("appends the namespace only to the titles that repeat", () => {
    expect(
      classOptionLabels([
        { ns: "team-a", class: "helper", title: "Helper", startable: true },
        { ns: "team-b", class: "helper", title: "Helper", startable: true },
        { ns: "team-a", class: "solo", title: "Solo Agent", startable: true },
      ]),
    ).toEqual(["Helper (in team-a)", "Helper (in team-b)", "Solo Agent"]);
  });

  // The limit belongs on the entry itself, not only on the reason line under
  // it: a viewer scanning the list should see which agents this server can
  // actually start before picking one. Both qualifiers coexist — the
  // disambiguating namespace answers "which Helper", the marker answers
  // "and can I start it here".
  it("marks the entries this server cannot start, alongside any disambiguating namespace", () => {
    expect(
      classOptionLabels([
        { ns: "team-a", class: "helper", title: "Helper", startable: false },
        { ns: "team-b", class: "helper", title: "Helper", startable: true },
        { ns: "team-a", class: "solo", title: "Solo Agent", startable: false },
      ]),
    ).toEqual(["Helper (in team-a) — cannot start here", "Helper (in team-b)", "Solo Agent — cannot start here"]);
  });

  it("renders the disambiguated labels in the real picker", async () => {
    const user = userEvent.setup();
    render(
      <SessionShell
        {...{
          ...shellGolden,
          selected: undefined,
          startableClasses: [
            { ns: "team-a", class: "helper", title: "Helper", startable: true },
            { ns: "team-b", class: "helper", title: "Helper", startable: true },
          ],
          canStartSessions: true,
        }}
      />,
    );

    await user.click(screen.getByTestId("session-shell-new-session"));

    const picker = screen.getByTestId("session-shell-new-session-class") as HTMLSelectElement;
    expect([...picker.options].map((o) => o.textContent)).toEqual(["Helper (in team-a)", "Helper (in team-b)"]);
  });
});

// Chrome mounts its header-actions container on truthiness alone, so an
// always-truthy slot would leave an empty container (and its testid) in the DOM
// for every future caller, quietly making that guard meaningless.
describe("SessionShell — the header-actions slot stays meaningful", () => {
  it("passes no header actions when there is neither a selection nor a start control", () => {
    render(
      <SessionShell
        {...{ ...shellGolden, selected: undefined, startableClasses: [], canStartSessions: false }}
      />,
    );
    expect(screen.queryByTestId("session-shell-header-actions")).toBeNull();
  });

  it("mounts the slot for a start control with no selection", () => {
    render(
      <SessionShell
        {...{
          ...shellGolden,
          selected: undefined,
          startableClasses: [{ ns: "demo-ns", class: "demo-agent", title: "Demo Agent", startable: true }],
          canStartSessions: true,
        }}
      />,
    );
    expect(screen.getByTestId("session-shell-header-actions")).toBeInTheDocument();
  });
});

describe("SessionShell — switching views is local, not a navigation", () => {
  // The switch used to be a plain anchor: every tab click was a full page load
  // that tore down the shell, the session list and the approval surface to
  // change ONE region — and took every piece of unsaved view state with it,
  // which is how a viewer lost the filters they had just picked by glancing at
  // the transcript.
  function bothViews() {
    return withSelected({
      offersAgentUI: true,
      view: {
        kind: "agent-ui",
        ui: { ns: "demo-ns", name: "alpha", declaration: { view: { component: "ap:stack" } } },
        chat: { ns: "demo-ns", name: "alpha" },
      },
    });
  }

  it("swaps the content region without unloading the page", () => {
    const pushed: string[] = [];
    const realPush = window.history.pushState.bind(window.history);
    const spy = vi.spyOn(window.history, "pushState").mockImplementation((s, t, url) => {
      pushed.push(String(url));
      realPush(s, t, url as string);
    });

    render(<SessionShell {...bothViews()} />);
    expect(screen.getByTestId("session-shell-ui-region")).not.toHaveClass("hidden");

    fireEvent.click(screen.getByTestId("session-shell-chat-escape-hatch"));

    // The transcript is now showing — from a state change, not a document
    // unload.
    expect(screen.getByTestId("chat-view")).toBeInTheDocument();
    // Exactly one history entry, carrying the view it switched to.
    expect(pushed).toHaveLength(1);
    expect(pushed[0]).toContain("view=chat");
    spy.mockRestore();
  });

  // The agent view is HIDDEN, not unmounted, and the difference is the whole
  // point. Unmounting discarded its resolved bindings, its live socket and the
  // declaration the agent had composed, so returning re-ran a full server-side
  // binding fan-out to rebuild the page that had just been thrown away — a
  // reload in each direction for what should be a glance at the transcript.
  //
  // Asserted on the DOM rather than on a render count: "still mounted" is what
  // preserves the state, and a hidden subtree is the observable form of it.
  it("keeps the agent view mounted while the transcript is shown, so switching back is instant", () => {
    render(<SessionShell {...bothViews()} />);
    expect(screen.getByTestId("session-shell-ui-region")).not.toHaveClass("hidden");
    const before = screen.getByTestId("session-shell-ui-region");

    fireEvent.click(screen.getByTestId("session-shell-chat-escape-hatch"));

    const region = screen.getByTestId("session-shell-ui-region");
    expect(region).toHaveClass("hidden");
    // The SAME element, not merely an element with the same testid. Presence
    // alone passed for a subtree React was tearing down and rebuilding on every
    // switch — which is the failure this row exists to catch, since a rebuilt
    // view has lost exactly the state the `hidden` class was chosen to keep.
    expect(region).toBe(before);

    fireEvent.click(screen.getByTestId("session-shell-agent-view-switch"));
    expect(screen.getByTestId("session-shell-ui-region")).toBe(before);
    expect(screen.getByTestId("session-shell-ui-region")).not.toHaveClass("hidden");
  });

  // The other side of keeping it mounted: a hidden view must not put anything
  // on screen. Its reply modal renders through a portal to document.body, which
  // no ancestor's display:none can contain, so "hidden" alone would let a turn
  // ending awaiting_reply drop a focus-trapping dialog over the transcript —
  // a surface that already shows the reply and has a composer of its own.
  //
  // The switch back is the second half of the claim, and the reason this row
  // is not simply "no modal on the transcript": the modal records which reply
  // it settled, so a dialog dismissed on the wrong surface would have silenced
  // it for the agent view the viewer had not reached yet.
  it("keeps the hidden agent view's reply modal off the transcript, and opens it on the way back", async () => {
    render(<SessionShell {...bothViews()} />);
    await waitFor(() => expect(chatOnFrame).not.toBeNull());

    fireEvent.click(screen.getByTestId("session-shell-chat-escape-hatch"));
    expect(screen.getByTestId("session-shell-ui-region")).toHaveClass("hidden");

    // The REAL frames a turn that ends waiting on the viewer publishes: the
    // agent's finalized reply, then the turn going inactive with the cause
    // that distinguishes "waiting on you" from every other way a turn ends.
    act(() => {
      chatOnFrame!({
        type: "user_message",
        session: { namespace: "demo-ns", name: "alpha" },
        payload: { session: { namespace: "demo-ns", name: "alpha" }, text: "Which repo should I use?" },
      } as ChatFrame);
      chatOnFrame!({
        type: "turn_activity",
        session: { namespace: "demo-ns", name: "alpha" },
        payload: { session: { namespace: "demo-ns", name: "alpha" }, active: false, cause: "awaiting_reply" },
      } as ChatFrame);
    });

    expect(screen.queryByTestId("agent-ui-reply-modal")).toBeNull();

    fireEvent.click(screen.getByTestId("session-shell-agent-view-switch"));
    expect(await screen.findByTestId("agent-ui-reply-modal")).toHaveTextContent("Which repo should I use?");
  });

  // Lazily, though: a viewer who never opens the agent view never pays for
  // one. Without this the shell would open a socket and run a binding fan-out
  // for a page nobody asked to see.
  it("does not mount the agent view for a viewer who has only ever seen the transcript", () => {
    const props = bothViews();
    render(<SessionShell {...props} selected={{ ...props.selected!, view: { ...props.selected!.view, kind: "chat" } }} />);

    expect(screen.getByTestId("chat-view")).toBeInTheDocument();
    expect(screen.queryByTestId("session-shell-ui-region")).toBeNull();
  });

  it("moves the current marker with the switch, in both directions", () => {
    render(<SessionShell {...bothViews()} />);
    expect(screen.getByTestId("session-shell-agent-view-switch")).toHaveAttribute("aria-current", "page");

    fireEvent.click(screen.getByTestId("session-shell-chat-escape-hatch"));
    expect(screen.getByTestId("session-shell-chat-escape-hatch")).toHaveAttribute("aria-current", "page");
    expect(screen.getByTestId("session-shell-agent-view-switch")).not.toHaveAttribute("aria-current");

    fireEvent.click(screen.getByTestId("session-shell-agent-view-switch"));
    expect(screen.getByTestId("session-shell-agent-view-switch")).toHaveAttribute("aria-current", "page");
    expect(screen.getByTestId("session-shell-ui-region")).not.toHaveClass("hidden");
  });

  // Middle-click and cmd/ctrl-click must still open a real tab. That only
  // works if the browser is allowed to follow the anchor, so the handler has
  // to decline these rather than preventDefault everything.
  it("leaves a modified click to the browser", () => {
    const spy = vi.spyOn(window.history, "pushState");
    render(<SessionShell {...bothViews()} />);

    fireEvent.click(screen.getByTestId("session-shell-chat-escape-hatch"), { metaKey: true });

    expect(spy).not.toHaveBeenCalled();
    // Still on the agent view: the shell did not handle the click.
    expect(screen.getByTestId("session-shell-ui-region")).not.toHaveClass("hidden");
    spy.mockRestore();
  });

  it("keeps a real href on both tabs so they can be opened in a new tab", () => {
    render(<SessionShell {...bothViews()} />);
    expect(screen.getByTestId("session-shell-chat-escape-hatch")).toHaveAttribute(
      "href",
      "/sessions?session=demo-ns%2Falpha&view=chat",
    );
    expect(screen.getByTestId("session-shell-agent-view-switch")).toHaveAttribute(
      "href",
      "/sessions?session=demo-ns%2Falpha&view=ui",
    );
  });

  it("follows Back to the sibling it came from", () => {
    // pushState alone changes the address; the browser does not re-render for
    // Back, so the shell has to listen. Without it Back would move the URL and
    // leave the content region where it was — the address and the page
    // disagreeing, which is worse than not supporting Back at all.
    render(<SessionShell {...bothViews()} />);
    fireEvent.click(screen.getByTestId("session-shell-chat-escape-hatch"));
    expect(screen.getByTestId("chat-view")).toBeInTheDocument();

    window.history.replaceState(null, "", "/sessions?session=demo-ns%2Falpha&view=ui");
    fireEvent.popState(window);

    expect(screen.getByTestId("session-shell-ui-region")).not.toHaveClass("hidden");
  });
});

describe("shell.golden.json — the wire carries both siblings", () => {
  // The golden is the pin between viewFor's Go output and what this browser
  // renders. Its `view.chat` key is only pinned if the TS side READS it — a
  // golden nobody consumes proves the server emits a key, never that the
  // client does anything with it.
  //
  // So this drives the switch from the GOLDEN rather than a hand-built
  // fixture: delete `Chat` from decideView's agent-ui branch and the Go golden
  // test fails, while this one fails too, at the behaviour that field exists
  // for. Between them, the field cannot become decorative.
  it("switches to the transcript using the golden's own chat payload, with no navigation", () => {
    render(<SessionShell {...shellGolden} />);
    expect(screen.getByTestId("session-shell-ui-region")).not.toHaveClass("hidden");

    fireEvent.click(screen.getByTestId("session-shell-chat-escape-hatch"));

    expect(screen.getByTestId("chat-view")).toBeInTheDocument();
    // The transcript opened against the session the golden's chat payload
    // names — not one re-derived from the selection, which would still pass
    // with the payload absent.
    expect(chatSocketFor).toEqual({ ns: "demo-ns", name: "alpha" });
  });
});
