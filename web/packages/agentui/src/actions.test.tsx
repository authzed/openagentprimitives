import "@testing-library/jest-dom/vitest";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { ActionsProvider, isActionPending, type ActionState } from "./actions";
import { renderNode } from "./renderNode";
import type { Node } from "./types";

afterEach(cleanup);

const buttonNode: Node = { component: "ap:button", props: { label: "Advance", action: "advance" } };

const formNode: Node = {
  component: "ap:form",
  props: { action: "advance", submitLabel: "Send", fields: [{ name: "why", label: "Why" }] },
};

function withActions(n: Node, states: Record<string, ActionState>, invoke = vi.fn()) {
  render(<ActionsProvider value={{ states, invoke, answer: vi.fn() }}>{renderNode(n)}</ActionsProvider>);
  return invoke;
}

describe("ap:button", () => {
  it("is enabled and fires the DECLARED action name", async () => {
    const invoke = withActions(buttonNode, {});
    await userEvent.click(screen.getByRole("button", { name: "Advance" }));
    expect(invoke).toHaveBeenCalledWith("advance", undefined);
  });

  it("stays disabled when the declaration names no action", () => {
    render(renderNode({ component: "ap:button", props: { label: "Inert" } }));
    expect(screen.getByRole("button", { name: "Inert" })).toBeDisabled();
  });

  it.each([
    { phase: "submitted" as const },
    { phase: "awaiting_approval" as const },
    { phase: "running" as const },
  ])("is disabled while the action is $phase", ({ phase }) => {
    withActions(buttonNode, { advance: { phase } });
    expect(screen.getByRole("button", { name: /Advance/ })).toBeDisabled();
  });

  it("RE-ENABLES on expired — a lapsed approval must not leave a dead control", () => {
    withActions(buttonNode, { advance: { phase: "expired" } });
    expect(screen.getByRole("button", { name: /Advance/ })).toBeEnabled();
  });

  it("re-enables on denied, and says so", () => {
    withActions(buttonNode, { advance: { phase: "denied", message: "You cannot do that." } });
    expect(screen.getByRole("button", { name: /Advance/ })).toBeEnabled();
    expect(screen.getByText("You cannot do that.")).toBeVisible();
  });

  it("distinguishes an approval the viewer must act on from one they cannot", () => {
    withActions(buttonNode, { advance: { phase: "awaiting_approval", approvalAddressedToViewer: true } });
    const mine = screen.getByTestId("agent-ui-action-status").textContent;
    cleanup();
    withActions(buttonNode, { advance: { phase: "awaiting_approval", approvalAddressedToViewer: false } });
    expect(screen.getByTestId("agent-ui-action-status").textContent).not.toBe(mine);
  });

  it("surfaces a rate limit with a retry hint rather than going silent", () => {
    withActions(buttonNode, { advance: { phase: "rate_limited", message: "Too fast; try again shortly." } });
    expect(screen.getByText(/try again shortly/i)).toBeVisible();
  });

  it("never renders an internal identifier", () => {
    withActions(buttonNode, { advance: { phase: "awaiting_approval", requestId: "req-abc123" } });
    expect(document.body.textContent).not.toContain("req-abc123");
  });

  // A settled-happy control shows no status clutter — same "null for idle and
  // succeeded" contract isActionPending's sibling table below pins.
  it("shows no status caption once the action has succeeded", () => {
    withActions(buttonNode, { advance: { phase: "succeeded" } });
    expect(screen.queryByTestId("agent-ui-action-status")).not.toBeInTheDocument();
  });

  // Guards WHAT MUST BE TRUE #5: "a failed action degrades ONE control, not
  // the page" — the same rule bindings.ts's applyBindings already follows for
  // the read half (see bindings.test.tsx: "replaces only the failing node,
  // and the rest of the view keeps working"). Two independent actions live on
  // one page; only the failing one may be affected.
  it("a failed action only affects ITS OWN control — a sibling action stays live", async () => {
    const invoke = vi.fn();
    const twoButtons: Node = {
      component: "ap:stack",
      children: [
        { component: "ap:button", props: { label: "Advance", action: "advance" } },
        { component: "ap:button", props: { label: "Cancel", action: "cancel" } },
      ],
    };
    render(
      <ActionsProvider value={{ states: { advance: { phase: "failed", message: "Broke." } }, invoke, answer: vi.fn() }}>
        {renderNode(twoButtons)}
      </ActionsProvider>,
    );
    expect(screen.getByRole("button", { name: /Advance/ })).toBeEnabled();
    expect(screen.getByText("Broke.")).toBeVisible();
    const cancelButton = screen.getByRole("button", { name: "Cancel" });
    expect(cancelButton).toBeEnabled();
    await userEvent.click(cancelButton);
    expect(invoke).toHaveBeenCalledWith("cancel", undefined);
  });
});

describe("ap:form", () => {
  it("submits its field values as the action's inputs", async () => {
    const invoke = withActions(formNode, {});
    await userEvent.type(screen.getByLabelText("Why"), "ready");
    await userEvent.click(screen.getByRole("button", { name: "Send" }));
    expect(invoke).toHaveBeenCalledWith("advance", { why: "ready" });
  });

  // Every DECLARED field must ride the POST, whether or not the viewer typed
  // into it. `values` gains a key on the first keystroke, so submitting only
  // touched fields meant an untouched one was absent — and the action's args
  // template still references it, so the server answered ErrUnknownParam and
  // the viewer was told "this view is missing a setting it needs; reload the
  // page" for the ordinary act of leaving a field blank.
  it("submits every declared field, defaulting an untouched one to the empty string", async () => {
    const twoFieldNode: Node = {
      component: "ap:form",
      props: {
        action: "advance",
        submitLabel: "Send",
        fields: [
          { name: "why", label: "Why" },
          { name: "note", label: "Note" },
        ],
      },
    };
    const invoke = withActions(twoFieldNode, {});
    await userEvent.type(screen.getByLabelText("Why"), "ready");
    await userEvent.click(screen.getByRole("button", { name: "Send" }));
    expect(invoke).toHaveBeenCalledWith("advance", { why: "ready", note: "" });
  });

  it("does not navigate on submit — a reload would lose the whole lifecycle", async () => {
    // jsdom does not implement form navigation, so a missing preventDefault()
    // does not visibly reload anything — the direct claim is the submit
    // event's defaultPrevented flag, which is exactly what preventDefault()
    // flips. The listener goes on `document`, not on the form: React attaches
    // its own handler at the render root, which is a DESCENDANT of document,
    // so a form-level listener would observe the flag before React's handler
    // had run and read false no matter what the component does.
    let prevented: boolean | null = null;
    const observe = (e: Event) => {
      prevented = e.defaultPrevented;
    };
    document.addEventListener("submit", observe);
    const invoke = withActions(formNode, {});

    await userEvent.click(screen.getByRole("button", { name: "Send" }));
    document.removeEventListener("submit", observe);

    expect(invoke).toHaveBeenCalledTimes(1);
    expect(prevented).toBe(true);
  });

  it("disables its fields and its submit while in flight", () => {
    withActions(formNode, { advance: { phase: "awaiting_approval" } });
    expect(screen.getByLabelText("Why")).toBeDisabled();
    expect(screen.getByRole("button", { name: "Send" })).toBeDisabled();
  });

  it("stays inert when the declaration names no action", () => {
    render(renderNode({ component: "ap:form", props: { fields: [{ name: "why" }] } }));
    expect(screen.getByRole("button", { name: "Submit" })).toBeDisabled();
  });
});

describe("isActionPending", () => {
  it("treats idle as NOT pending, so an unclicked control stays clickable", () => {
    expect(isActionPending("idle")).toBe(false);
  });
  it.each(["submitted", "awaiting_approval", "running"] as const)("treats %s as pending", (p) => {
    expect(isActionPending(p)).toBe(true);
  });
  it.each(["succeeded", "failed", "denied", "expired", "rate_limited"] as const)("treats %s as settled", (p) => {
    expect(isActionPending(p)).toBe(false);
  });
});

// busy is the page-level fact "the agent is working": the view sets it from
// the session's turn_activity frames. A control's own lifecycle settles a
// Prompt action the moment the message is delivered (it cannot know when
// the agent will answer), so without this a form re-enables while the agent
// is still working on what it just sent — and a second submit lands as a
// second message mid-turn.
describe("busy (the agent's turn is active)", () => {
  const busyProvider = (n: Node, busy: boolean) =>
    render(<ActionsProvider value={{ states: {}, invoke: vi.fn(), answer: vi.fn(), busy }}>{renderNode(n)}</ActionsProvider>);

  it("disables a form's fields and submit while busy, and re-enables when the turn ends", () => {
    const { rerender } = busyProvider(formNode, true);
    expect(screen.getByRole("button", { name: "Send" })).toBeDisabled();
    expect(screen.getByLabelText("Why")).toBeDisabled();
    rerender(<ActionsProvider value={{ states: {}, invoke: vi.fn(), answer: vi.fn(), busy: false }}>{renderNode(formNode)}</ActionsProvider>);
    expect(screen.getByRole("button", { name: "Send" })).toBeEnabled();
    expect(screen.getByLabelText("Why")).toBeEnabled();
  });

  it("disables a button while busy", () => {
    busyProvider(buttonNode, true);
    expect(screen.getByRole("button", { name: "Advance" })).toBeDisabled();
  });

  it("leaves a control enabled when the provider says nothing about busy", () => {
    withActions(buttonNode, {});
    expect(screen.getByRole("button", { name: "Advance" })).toBeEnabled();
  });
});
