import "@testing-library/jest-dom/vitest";
import * as React from "react";
import { describe, it, expect, afterEach } from "vitest";
import { createPortal } from "react-dom";
import { render, cleanup, screen, fireEvent } from "@testing-library/react";

import { Chrome, type TrustEvent, type TrustEventKind } from "./Chrome";

afterEach(cleanup);

// baseProps mirrors what pkg/web/webui/sessions' shellPageBuild sends for a
// selected session, once the subject is decoded to display form.
const baseProps = {
  ns: "workshop",
  name: "sess-live",
  subject: "avery@example.com",
  sessionOrigin: "attached",
};

// The chrome regions the collapse toggle must reduce — every testid Chrome
// renders outside the content region and the toggle itself. No
// composed-by-agent legend appears here or in the view: the marker an
// agent-composed hook wears stands alone, with nothing anywhere naming it.
const chromeRegionTestIds = [
  "session-shell-identity",
  "session-shell-session-origin",
  "session-shell-chat-escape-hatch",
  "session-shell-approval-surface",
  "session-shell-sidebar",
];

function clickToggle() {
  fireEvent.click(screen.getByTestId("session-shell-chrome-toggle"));
}

describe("Chrome visibility — precedence: user's toggle > UI's requested initial state > platform default (shown)", () => {
  it.each([
    { name: "no request, no toggle -> platform default: shown", initialState: undefined, wantVisibleBeforeToggle: true },
    { name: "UI requests expanded, no toggle -> shown", initialState: "expanded", wantVisibleBeforeToggle: true },
    { name: "UI requests collapsed, no toggle -> collapsed (UI's request honored)", initialState: "collapsed", wantVisibleBeforeToggle: false },
    {
      name: "unrecognized request string, no toggle -> treated as default: shown",
      initialState: "sideways",
      wantVisibleBeforeToggle: true,
    },
  ])("$name", ({ initialState, wantVisibleBeforeToggle }) => {
    render(
      <Chrome {...baseProps} initialState={initialState}>
        content
      </Chrome>,
    );
    for (const testId of chromeRegionTestIds) {
      if (wantVisibleBeforeToggle) {
        expect(screen.getByTestId(testId)).toBeVisible();
      } else {
        expect(screen.getByTestId(testId)).not.toBeVisible();
      }
    }
  });

  // MUTATION-CHECK (b), witness 1: an implementation that makes the UI's
  // requested state override the viewer's toggle (e.g. deriving `collapsed`
  // straight from the `initialState` prop on every render, ignoring the
  // click) leaves this red — the click has no observable effect.
  it("the viewer's toggle overrides a UI request for collapsed — one click reveals it", () => {
    render(
      <Chrome {...baseProps} initialState="collapsed">
        content
      </Chrome>,
    );
    expect(screen.getByTestId("session-shell-identity")).not.toBeVisible();
    clickToggle();
    for (const testId of chromeRegionTestIds) {
      expect(screen.getByTestId(testId)).toBeVisible();
    }
  });

  it("the viewer's toggle overrides the platform default (shown) — one click hides it", () => {
    render(<Chrome {...baseProps}>content</Chrome>);
    expect(screen.getByTestId("session-shell-identity")).toBeVisible();
    clickToggle();
    for (const testId of chromeRegionTestIds) {
      expect(screen.getByTestId(testId)).not.toBeVisible();
    }
  });

  // MUTATION-CHECK (b), witness 2: this exercises the re-render path
  // specifically — the UI's requested state changes value (a genuinely new
  // prop, not a no-op re-render) AFTER the viewer's own toggle. An
  // implementation that re-syncs from `initialState` on every prop change
  // without gating on "has the viewer already chosen" flips back here, even
  // though witness 1 above (which never re-renders with new props) would
  // stay green.
  it("a later, genuinely different UI-requested state does not undo the viewer's earlier explicit choice", () => {
    const { rerender } = render(<Chrome {...baseProps}>content</Chrome>);
    clickToggle(); // viewer explicitly collapses, diverging from the (absent) request
    expect(screen.getByTestId("session-shell-identity")).not.toBeVisible();
    rerender(
      <Chrome {...baseProps} initialState="expanded">
        content
      </Chrome>,
    );
    expect(screen.getByTestId("session-shell-identity")).not.toBeVisible();
  });
});

describe("Chrome visibility — auto-reveal: any trust event reveals chrome regardless of current state", () => {
  const kinds: TrustEventKind[] = ["approval_addressed_to_viewer", "credential_request", "identity_question", "hard_error"];

  it.each(kinds)("kind=%s reveals chrome even though the viewer had explicitly collapsed it", (kind) => {
    const { rerender } = render(<Chrome {...baseProps}>content</Chrome>);
    clickToggle(); // viewer's own explicit choice: collapsed
    expect(screen.getByTestId("session-shell-identity")).not.toBeVisible();

    const event: TrustEvent = { kind, id: "evt-1" };
    // MUTATION-CHECK (c): deleting the auto-reveal effect (or the effect
    // that watches `trustEvent`) leaves chrome collapsed here — this
    // assertion is the one that goes red.
    rerender(
      <Chrome {...baseProps} trustEvents={[event]}>
        content
      </Chrome>,
    );
    for (const testId of chromeRegionTestIds) {
      expect(screen.getByTestId(testId)).toBeVisible();
    }
  });

  it("a SECOND distinct event of the same kind re-reveals even after the viewer re-collapses in between", () => {
    const first: TrustEvent = { kind: "hard_error", id: "evt-1" };
    const { rerender } = render(
      <Chrome {...baseProps} trustEvents={[first]}>
        content
      </Chrome>,
    );
    expect(screen.getByTestId("session-shell-identity")).toBeVisible();

    clickToggle(); // viewer collapses again after handling the first event
    expect(screen.getByTestId("session-shell-identity")).not.toBeVisible();

    const second: TrustEvent = { kind: "hard_error", id: "evt-2" };
    rerender(
      <Chrome {...baseProps} trustEvents={[second]}>
        content
      </Chrome>,
    );
    expect(screen.getByTestId("session-shell-identity")).toBeVisible();
  });

  // The reveal has to LATCH, not just fire once. `initialState` is a static
  // bootstrap prop today, so nothing re-renders Chrome with a different one —
  // but the moment props go live (a later plan mirrors session state into this
  // page), a UI that keeps re-requesting `collapsed` would undo an active
  // reveal for any viewer who has never toggled, which is precisely "a UI
  // suppressed chrome through a trust event". The precedence that must hold:
  // a trust reveal outranks the UI's request, and the viewer's own toggle
  // outranks both.
  it("a later UI request for collapsed does not undo an active trust reveal (viewer never toggled)", () => {
    const event: TrustEvent = { kind: "hard_error", id: "evt-1" };
    const { rerender } = render(
      <Chrome {...baseProps} initialState="expanded" trustEvents={[event]}>
        content
      </Chrome>,
    );
    expect(screen.getByTestId("session-shell-identity")).toBeVisible();

    rerender(
      <Chrome {...baseProps} initialState="collapsed" trustEvents={[event]}>
        content
      </Chrome>,
    );
    for (const testId of chromeRegionTestIds) {
      expect(screen.getByTestId(testId)).toBeVisible();
    }
  });

  it("an auto-reveal still lets the viewer collapse again afterward (it is not a permanent lock)", () => {
    const event: TrustEvent = { kind: "hard_error", id: "evt-1" };
    render(
      <Chrome {...baseProps} initialState="collapsed" trustEvents={[event]}>
        content
      </Chrome>,
    );
    expect(screen.getByTestId("session-shell-identity")).toBeVisible();
    clickToggle();
    expect(screen.getByTestId("session-shell-identity")).not.toBeVisible();
  });
});

describe("Chrome — approvals prop: the approval surface renders exactly what its caller derived", () => {
  // The guarantee is the region's RESERVATION, not any copy in it: the element
  // is always mounted, so an arriving approval cannot reflow the page out from
  // under a click and the content region can never cover or remove it. What
  // the region must NOT do when empty is assert a checked state — "No pending
  // approvals" would claim a verification Chrome has not made.
  //
  // These two assert the region exists and stays silent. They deliberately do
  // not assert emptiness of the whole element: the sibling-view tabs live at
  // its trailing edge, so "renders nothing at all" is the wrong bar and would
  // have to be rewritten every time the bar gains a neighbour.
  it("with no approvals prop, reserves the region and asserts nothing in it", () => {
    render(<Chrome {...baseProps}>content</Chrome>);
    const surface = screen.getByTestId("session-shell-approval-surface");
    expect(surface).toBeInTheDocument();
    expect(surface).not.toHaveTextContent("Approval requests appear here");
    expect(surface).not.toHaveTextContent(/no pending approvals/i);
  });

  it("with an empty approvals array, does the same", () => {
    render(<Chrome {...baseProps} approvals={[]}>content</Chrome>);
    const surface = screen.getByTestId("session-shell-approval-surface");
    expect(surface).toBeInTheDocument();
    expect(surface).not.toHaveTextContent("Approval requests appear here");
    expect(surface).not.toHaveTextContent(/no pending approvals/i);
  });

  it("renders each notice's own label", () => {
    render(
      <Chrome
        {...baseProps}
        approvals={[
          { id: "req-1", label: "Your approval is needed to continue.", addressedToViewer: true },
          { id: "req-2", label: "Waiting on approval from someone else.", addressedToViewer: false },
        ]}
      >
        content
      </Chrome>,
    );
    const region = screen.getByTestId("session-shell-approval-surface");
    expect(region).toHaveTextContent("Your approval is needed to continue.");
    expect(region).toHaveTextContent("Waiting on approval from someone else.");
    expect(region).not.toHaveTextContent("Approval requests appear here");
  });
});

describe("Chrome visibility — never suppressible: collapsed means visually reduced, NOT absent from the DOM (Decision 3, rule 3)", () => {
  // MUTATION-CHECK (a): an implementation that unmounts chrome on collapse
  // (e.g. `{!collapsed && <div data-testid="...">}` instead of the `hidden`
  // attribute) makes `queryByTestId` return null here — this is the
  // assertion that must go red under that mutation, and it asserts DOM
  // presence directly, not a `collapsed` flag.
  it.each(chromeRegionTestIds)("keeps %s queryable in the DOM while collapsed", (testId) => {
    render(
      <Chrome {...baseProps} initialState="collapsed">
        content
      </Chrome>,
    );
    expect(screen.queryByTestId(testId)).not.toBeNull();
    expect(screen.getByTestId(testId)).toBeInTheDocument();
  });

  it("keeps the content region's children mounted, and the toggle control itself reachable, while collapsed", () => {
    render(
      <Chrome {...baseProps} initialState="collapsed">
        <div data-testid="probe">agent content</div>
      </Chrome>,
    );
    expect(screen.getByTestId("probe")).toBeInTheDocument();
    expect(screen.getByTestId("session-shell-chrome-toggle")).toBeVisible();
  });

  it("the toggle is a real, labelled, keyboard-reachable control — not a clickable div", () => {
    render(<Chrome {...baseProps}>content</Chrome>);
    const toggleEl = screen.getByTestId("session-shell-chrome-toggle");
    expect(toggleEl.tagName).toBe("BUTTON");
    expect(toggleEl).toHaveAccessibleName();
    expect(toggleEl).toHaveAttribute("aria-expanded");
    expect(toggleEl).not.toHaveAttribute("tabindex", "-1");
  });
});

// The cases below followed Chrome here from the agent-UI view's own test
// file, where they rendered Chrome DIRECTLY (never the view) — so they are
// this component's coverage and belong beside it now that the component
// moved. Their assertions are unchanged except the escape hatch's href,
// which is the point of the hoist: the switch is a sibling-view link inside
// the shell, not a jump to another page.
describe("Chrome — platform-owned regions the content region cannot draw over or suppress", () => {
  it('renders session identity ("acting as ...")', () => {
    render(<Chrome {...baseProps}>content</Chrome>);
    expect(screen.getByTestId("session-shell-identity")).toHaveTextContent("Acting as avery@example.com");
  });

  it("discloses an attached session in human copy", () => {
    render(<Chrome {...baseProps}>content</Chrome>);
    expect(screen.getByTestId("session-shell-session-origin")).toHaveTextContent("Resumed your session.");
  });

  it("discloses an asleep session with DIFFERENT human copy than attached", () => {
    render(
      <Chrome {...baseProps} sessionOrigin="asleep">
        content
      </Chrome>,
    );
    expect(screen.getByTestId("session-shell-session-origin")).toHaveTextContent(
      "Your session is asleep; it resumes when you send a message.",
    );
  });

  // The platform must not claim an action it did not take. Nothing on this
  // page's request path stamps the wake-requested-at annotation (the only
  // production writers are channelsd's inbound Deliver path and the runner's
  // own status writer), so copy phrased as "we woke it" would assert a wake
  // that never happened. See session.go's Asleep branch.
  it("the asleep disclosure does not claim the platform performed a wake", () => {
    render(
      <Chrome {...baseProps} sessionOrigin="asleep">
        content
      </Chrome>,
    );
    expect(screen.getByTestId("session-shell-session-origin").textContent).not.toMatch(/\bwoke|waking|woken\b/i);
  });

  // The same discipline for the branch the hoist made reachable: an ended
  // session's disclosure states the condition and that a new session CAN be
  // started; it must not report that one already was, because serving this
  // page starts nothing.
  it("the ended disclosure states the condition without claiming a replacement was started", () => {
    render(
      <Chrome {...baseProps} sessionOrigin="ended">
        content
      </Chrome>,
    );
    const disclosure = screen.getByTestId("session-shell-session-origin");
    expect(disclosure).toHaveTextContent("This session has ended. You can start a new one.");
    expect(disclosure.textContent).not.toMatch(/\bstarted\b|\bwe started\b|\bnew session is\b/i);
  });

  // A session that never BOOTED is the case this exists for: it has no
  // transcript to explain itself, and the shell's ended line is the only
  // surface its viewer reaches. Without the reason the page says the same
  // sentence for a session refused seconds ago as for one that finished last
  // week, which reads as an expiry rather than as an answer.
  //
  // The reason gets the last word. A cap refusal already tells the viewer to
  // close a workshop BEFORE starting another, so a generic "You can start a
  // new one." bolted after it contradicts the very sentence it follows.
  it("an ended session that failed discloses the reason and nothing after it", () => {
    const reason =
      "You already have the maximum number of builder workshops open at once. Finish or close one before starting another.";
    render(
      <Chrome {...baseProps} sessionOrigin="ended" endedReason={reason}>
        content
      </Chrome>,
    );
    const disclosure = screen.getByTestId("session-shell-session-origin");
    expect(disclosure.textContent).toBe(`This session has ended: ${reason}`);
    // Truncated in the header by design, so the full sentence must stay
    // recoverable: a reason the viewer cannot finish reading is not a
    // disclosure.
    expect(disclosure).toHaveAttribute("title", expect.stringContaining(reason));
  });

  // The negative control on the same branch: nothing to disclose must not
  // render as a disclosure of nothing.
  it("an ended session with no recorded reason keeps the plain sentence, with no dangling punctuation", () => {
    render(
      <Chrome {...baseProps} sessionOrigin="ended">
        content
      </Chrome>,
    );
    const disclosure = screen.getByTestId("session-shell-session-origin");
    expect(disclosure.textContent).toBe("This session has ended. You can start a new one.");
  });

  // MUTATION-CHECK (a): deleting the session-origin disclosure rendering (the
  // `origin.text` span in Chrome.tsx) fails these rows and the two ended
  // disclosures above — they are the rows asserting on that testid's content
  // specifically, not the identity line above (a different testid, a
  // different field).
  //
  // The `ended` row is the one place this rule can now be broken from OUTSIDE
  // this component: every other sentence is a literal in Chrome.tsx, while an
  // ended session's reason is SERVER text rendered verbatim. The server only
  // forwards a message written for the person
  // (v1alpha1.FailureReasonWrittenForThePerson gates it); this row is the
  // assertion that the rendering does not reintroduce vocabulary of its own
  // before it, and `tail` is the assertion that it adds nothing after it
  // either — whatever the server said is the last thing the viewer reads.
  it.each([
    { label: "an attached session", extra: {}, tail: undefined },
    {
      label: "an ended session disclosing why it failed",
      extra: {
        sessionOrigin: "ended",
        endedReason:
          "You already have the maximum number of builder workshops open at once. Finish or close one before starting another.",
      },
      tail: "You already have the maximum number of builder workshops open at once. Finish or close one before starting another.",
    },
  ])("never leaks internal operational vocabulary in the session-origin disclosure: $label", ({ extra, tail }) => {
    render(
      <Chrome {...baseProps} {...extra}>
        content
      </Chrome>,
    );
    const disclosure = screen.getByTestId("session-shell-session-origin");
    expect(disclosure.textContent).not.toMatch(/AgentSession|AgentClass|WakeEligible|CRD|phase|attached|woken/i);
    if (tail !== undefined) {
      expect(disclosure.textContent?.endsWith(tail)).toBe(true);
    }
  });

  // The href is asserted EXACTLY: a mutation that breaks it leaves a link that
  // still resolves to SOMETHING, so only the literal catches it. It is also
  // now the only destination this control has — Chrome has one host, so the
  // per-host override it used to accept is gone.
  it("renders a chat escape hatch that switches THIS session's view without leaving the shell", () => {
    render(<Chrome {...baseProps}>content</Chrome>);
    expect(screen.getByTestId("session-shell-chat-escape-hatch")).toHaveAttribute(
      "href",
      "/sessions?session=workshop%2Fsess-live&view=chat",
    );
  });

  // `new`, `ns`, `agentClass` and `prompt` are the picker's one-shot request
  // keys, consumed once at mount by the shell. Carrying them into a sibling
  // tab's href would re-issue that request every time the tab is followed or
  // reloaded from — reopening a dialog the viewer already dismissed, or
  // re-preselecting an agent they navigated away from. An unrelated key
  // (`p.foo` here, standing in for AgentUIView's binding parameters) is kept:
  // only the one-shot keys are the ones this control drops.
  it("drops the picker's one-shot request keys (new, ns, agentClass, prompt) from a sibling tab's href, keeping unrelated keys", () => {
    const original = window.location.href;
    try {
      window.history.replaceState(
        {},
        "",
        "/sessions?new=1&ns=ws-1&agentClass=demo-agent&prompt=hello&p.foo=bar",
      );
      render(<Chrome {...baseProps}>content</Chrome>);
      const hatch = screen.getByTestId("session-shell-chat-escape-hatch");
      const href = hatch.getAttribute("href") ?? "";
      const query = new URLSearchParams(href.split("?")[1] ?? "");
      expect(query.has("new")).toBe(false);
      expect(query.has("ns")).toBe(false);
      expect(query.has("agentClass")).toBe(false);
      expect(query.has("prompt")).toBe(false);
      expect(query.get("p.foo")).toBe("bar");
    } finally {
      window.history.replaceState({}, "", original);
    }
  });

  // The escape hatch is the current view's own marker when chat is already
  // showing — present, not absent. A control that vanishes on arrival would
  // make this chrome region's contents depend on which sibling view is up.
  it("marks the escape hatch as the current page when chat is the shown view, rather than removing it", () => {
    render(
      <Chrome {...baseProps} currentView="chat">
        content
      </Chrome>,
    );
    const hatch = screen.getByTestId("session-shell-chat-escape-hatch");
    expect(hatch).toBeInTheDocument();
    expect(hatch).toHaveAttribute("aria-current", "page");
  });

  it("leaves the escape hatch unmarked when the agent-defined view is the shown one", () => {
    render(
      <Chrome {...baseProps} currentView="ui">
        content
      </Chrome>,
    );
    expect(screen.getByTestId("session-shell-chat-escape-hatch")).not.toHaveAttribute("aria-current");
  });

  // The return leg. The chat control alone is a one-way trip: a viewer who
  // follows it lands on a page whose only marked control says "you are here",
  // with nothing but the URL bar to get back to the agent's own view. The href
  // is asserted EXACTLY for the same reason the chat one is — a mutation that
  // breaks it still resolves to SOMETHING.
  it("renders the agent-view half of the switch, addressed at this session's agent view", () => {
    render(
      <Chrome {...baseProps} offersAgentUI currentView="chat">
        content
      </Chrome>,
    );
    const back = screen.getByTestId("session-shell-agent-view-switch");
    expect(back).toHaveAttribute("href", "/sessions?session=workshop%2Fsess-live&view=ui");
    expect(back).not.toHaveAttribute("aria-current");
  });

  it("marks the agent-view half as the current page when the agent's view is the shown one", () => {
    render(
      <Chrome {...baseProps} offersAgentUI currentView="ui">
        content
      </Chrome>,
    );
    expect(screen.getByTestId("session-shell-agent-view-switch")).toHaveAttribute("aria-current", "page");
    expect(screen.getByTestId("session-shell-chat-escape-hatch")).not.toHaveAttribute("aria-current");
  });

  // A session whose agent declares no view has nothing to switch to, so the
  // half that would offer it must not render at all — an always-broken control
  // is worse than no control. The chat half is unconditional: every session has
  // a transcript.
  it("renders no agent-view half when this session offers none, and keeps the chat half", () => {
    render(
      <Chrome {...baseProps} currentView="chat">
        content
      </Chrome>,
    );
    expect(screen.queryByTestId("session-shell-agent-view-switch")).toBeNull();
    expect(screen.getByTestId("session-shell-chat-escape-hatch")).toBeInTheDocument();
  });

  // With the content region showing neither sibling (the inline unavailable
  // card), neither control may claim to be where the viewer is — and both must
  // stay followable, because they are the only way out of that card.
  it("marks neither half when the content region shows neither sibling view", () => {
    render(
      <Chrome {...baseProps} offersAgentUI>
        content
      </Chrome>,
    );
    expect(screen.getByTestId("session-shell-agent-view-switch")).not.toHaveAttribute("aria-current");
    expect(screen.getByTestId("session-shell-chat-escape-hatch")).not.toHaveAttribute("aria-current");
  });

  // Chrome rendered with NO approvals prop — the region must exist anyway, so
  // a later approval has somewhere to land without a layout shift. Its live
  // behavior is covered where the approvals actually arrive
  // (SessionShell.test.tsx).
  it("renders an approval-surface region even with no approvals to show", () => {
    render(<Chrome {...baseProps}>content</Chrome>);
    expect(screen.getByTestId("session-shell-approval-surface")).toBeInTheDocument();
  });

  // The placeholder copy must not assert a CHECKED state: "No pending
  // approvals" reads as "we looked; there are none", and this render passed no
  // approvals at all, so nothing looked. A session that actually has a pending
  // approval would be actively misled by that phrasing.
  it("the approval-surface placeholder copy does not claim a checked state", () => {
    render(<Chrome {...baseProps}>content</Chrome>);
    const region = screen.getByTestId("session-shell-approval-surface");
    expect(region.textContent).not.toMatch(/no pending/i);
  });

  it("renders fully visible by platform default (no requested state, no toggle, no trust event)", () => {
    render(<Chrome {...baseProps}>content</Chrome>);
    for (const testId of chromeRegionTestIds) {
      expect(screen.getByTestId(testId)).toBeVisible();
    }
  });

  it("renders its content-region children alongside chrome, not instead of it", () => {
    render(
      <Chrome {...baseProps}>
        <div data-testid="probe">agent content</div>
      </Chrome>,
    );
    expect(screen.getByTestId("probe")).toBeInTheDocument();
    expect(screen.getByTestId("session-shell-identity")).toBeInTheDocument();
  });

  // MUTATION-CHECK (b): `function Chrome() { return null; }` fails every test
  // in this describe block (each queries a testid Chrome alone renders), but
  // this row is the narrowest single-purpose witness — it asserts nothing
  // else, so a null-returning Chrome fails it with no other explanation.
  it("renders something (a null Chrome renders no identity testid at all)", () => {
    render(<Chrome {...baseProps}>content</Chrome>);
    expect(screen.queryByTestId("session-shell-identity")).not.toBeNull();
  });
});

// The session list is passed IN (`sidebar`), so Chrome renders a region it
// does not understand. These rows pin the structural property that makes the
// list unsuppressible — it is a sibling of the content region, under the same
// collapse discipline — without Chrome learning anything about sessions.
describe("Chrome — the sidebar region holds the session list without Chrome knowing what a session is", () => {
  it("renders the sidebar's contents as a DOM sibling of the content region, never inside it", () => {
    render(
      <Chrome {...baseProps} sidebar={<div data-testid="list-probe">rows</div>}>
        <div data-testid="probe">agent content</div>
      </Chrome>,
    );
    expect(screen.getByTestId("list-probe")).toBeVisible();
    expect(screen.getByTestId("session-shell-content-region").querySelector('[data-testid="list-probe"]')).toBeNull();
  });

  it("keeps the sidebar region mounted while collapsed, exactly like every other chrome region", () => {
    render(
      <Chrome {...baseProps} initialState="collapsed" sidebar={<div data-testid="list-probe">rows</div>}>
        content
      </Chrome>,
    );
    expect(screen.getByTestId("session-shell-sidebar")).toBeInTheDocument();
    expect(screen.getByTestId("session-shell-sidebar")).not.toBeVisible();
    expect(screen.getByTestId("list-probe")).toBeInTheDocument();
  });
});

// headerActions is the same posture as `sidebar`, in the header: the host
// supplies session-scoped controls and Chrome only places them. The rows below
// are the structural ones — a control the content region could cover or a
// control collapse could unmount would both break the guarantee.
describe("Chrome — the header-actions region holds host controls without Chrome knowing what they do", () => {
  it("renders host controls as a DOM sibling of the content region, never inside it", () => {
    render(
      <Chrome {...baseProps} headerActions={<button data-testid="action-probe">Details</button>}>
        <div data-testid="probe">agent content</div>
      </Chrome>,
    );
    expect(screen.getByTestId("action-probe")).toBeVisible();
    expect(screen.getByTestId("session-shell-content-region").querySelector('[data-testid="action-probe"]')).toBeNull();
  });

  it("keeps the region mounted while collapsed, exactly like every other chrome region", () => {
    render(
      <Chrome {...baseProps} initialState="collapsed" headerActions={<button data-testid="action-probe">Details</button>}>
        content
      </Chrome>,
    );
    expect(screen.getByTestId("session-shell-header-actions")).toBeInTheDocument();
    expect(screen.getByTestId("session-shell-header-actions")).not.toBeVisible();
    expect(screen.getByTestId("action-probe")).toBeInTheDocument();
  });

  // A host with nothing to contribute must not leave an empty region in the
  // header taking up space — unlike `sidebar`, whose column is always present
  // (at zero width) because it is the session list's home.
  it("renders no region at all when the host supplies no controls", () => {
    render(<Chrome {...baseProps}>content</Chrome>);
    expect(screen.queryByTestId("session-shell-header-actions")).toBeNull();
  });

  // Collapse reduces the slot's own CONTAINER; it cannot reach content the host
  // renders through a portal, which lands on document.body outside this subtree.
  // Pinned rather than merely documented, because the alternative is a comment
  // asserting a discipline the slot's real production filling (a Radix Dialog)
  // escapes. It costs nothing structurally — portaled content is still the
  // host's and still unreachable by the content region — but a host opening a
  // modal from here owns closing it.
  it("does not reduce content the host renders through a portal", () => {
    function Portaled() {
      return createPortal(<div data-testid="portal-probe">detached</div>, document.body);
    }
    render(
      <Chrome {...baseProps} initialState="collapsed" headerActions={<Portaled />}>
        content
      </Chrome>,
    );
    expect(screen.getByTestId("session-shell-header-actions")).not.toBeVisible();
    expect(screen.getByTestId("portal-probe")).toBeVisible();
  });
});

// With no session selected there is nothing for the session-scoped regions to
// disclose or link to. Identity is not session-scoped and stays.
describe("Chrome — no selected session", () => {
  it("renders identity but no session-origin disclosure and no sibling-view switch", () => {
    render(<Chrome subject="avery@example.com">content</Chrome>);
    expect(screen.getByTestId("session-shell-identity")).toBeVisible();
    expect(screen.queryByTestId("session-shell-session-origin")).toBeNull();
    expect(screen.queryByTestId("session-shell-chat-escape-hatch")).toBeNull();
    expect(screen.queryByTestId("session-shell-agent-view-switch")).toBeNull();
  });

  it("still renders the approval surface, the sidebar region and the toggle", () => {
    render(<Chrome subject="avery@example.com">content</Chrome>);
    expect(screen.getByTestId("session-shell-approval-surface")).toBeVisible();
    expect(screen.getByTestId("session-shell-sidebar")).toBeVisible();
    expect(screen.getByTestId("session-shell-chrome-toggle")).toBeVisible();
  });
});

describe("Chrome — the sibling-view switch is a segmented control in the approval bar", () => {
  // Placement is a contract, not a preference: the switch is per-session chrome
  // and must live in a region the content region cannot cover or remove. The
  // approval surface is exactly that region, so asserting containment is what
  // stops a later refactor floating the tabs back into the header (shell-wide
  // controls) or, worse, into the content region (agent-reachable).
  it("renders both tabs INSIDE the approval surface, not the header", () => {
    render(
      <Chrome {...baseProps} offersAgentUI currentView="ui">
        content
      </Chrome>,
    );
    const surface = screen.getByTestId("session-shell-approval-surface");
    expect(surface).toContainElement(screen.getByTestId("session-shell-agent-view-switch"));
    expect(surface).toContainElement(screen.getByTestId("session-shell-chat-escape-hatch"));
  });

  // The shown view must be answerable from the control itself. aria-current and
  // the filled background are derived from ONE boolean precisely so a sighted
  // viewer and a screen-reader viewer cannot be told different things; asserting
  // both together is what would catch them drifting apart.
  it("marks the shown view current, and only that one", () => {
    render(
      <Chrome {...baseProps} offersAgentUI currentView="ui">
        content
      </Chrome>,
    );
    const ui = screen.getByTestId("session-shell-agent-view-switch");
    const chat = screen.getByTestId("session-shell-chat-escape-hatch");
    expect(ui).toHaveAttribute("aria-current", "page");
    expect(chat).not.toHaveAttribute("aria-current");
    expect(ui.className).toContain("bg-secondary");
    expect(chat.className).not.toContain("bg-secondary");
  });

  it("moves both signals to the other tab when the transcript is shown", () => {
    render(
      <Chrome {...baseProps} offersAgentUI currentView="chat">
        content
      </Chrome>,
    );
    const ui = screen.getByTestId("session-shell-agent-view-switch");
    const chat = screen.getByTestId("session-shell-chat-escape-hatch");
    expect(chat).toHaveAttribute("aria-current", "page");
    expect(ui).not.toHaveAttribute("aria-current");
    expect(chat.className).toContain("bg-secondary");
    expect(ui.className).not.toContain("bg-secondary");
  });

  // The switch is not a one-way hatch: the agent-view tab must stay reachable
  // WHILE the transcript is shown, or a viewer who follows Chat has no way back
  // from where they left.
  it("keeps the agent-view tab present while the transcript is shown", () => {
    render(
      <Chrome {...baseProps} offersAgentUI currentView="chat">
        content
      </Chrome>,
    );
    expect(screen.getByTestId("session-shell-agent-view-switch")).toBeInTheDocument();
  });

  it("omits the agent-view tab entirely when the agent declares no UI", () => {
    render(
      <Chrome {...baseProps} currentView="chat">
        content
      </Chrome>,
    );
    expect(screen.queryByTestId("session-shell-agent-view-switch")).toBeNull();
    expect(screen.getByTestId("session-shell-chat-escape-hatch")).toBeInTheDocument();
  });

  it("renders no tab group at all with no session selected", () => {
    const { ns: _ns, name: _name, ...noSelection } = baseProps;
    render(<Chrome {...noSelection}>content</Chrome>);
    expect(screen.queryByTestId("session-shell-view-tabs")).toBeNull();
  });
});

describe("Chrome — header ordering", () => {
  // The toggle leads the header, over the session-list column it shares, and is
  // the one control NOT under `hidden` — collapsing must never strand a viewer
  // with no way back. Identity sits at the far edge: it is shell-wide, not
  // about the session in front of you.
  it("puts the collapse toggle before the identity in DOM order", () => {
    render(<Chrome {...baseProps}>content</Chrome>);
    const toggle = screen.getByTestId("session-shell-chrome-toggle");
    const identity = screen.getByTestId("session-shell-identity");
    // compareDocumentPosition: FOLLOWING (4) means identity comes after toggle.
    expect(toggle.compareDocumentPosition(identity) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("keeps the toggle usable while collapsed, and the identity hidden", () => {
    render(
      <Chrome {...baseProps} initialState="collapsed">
        content
      </Chrome>,
    );
    expect(screen.getByTestId("session-shell-chrome-toggle")).toBeVisible();
    expect(screen.getByTestId("session-shell-identity")).not.toBeVisible();
  });
});
