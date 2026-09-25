import "@testing-library/jest-dom/vitest";
import * as React from "react";
import { describe, it, expect, afterEach, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { AgentReplyModal } from "./AgentReplyModal";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// The dialog renders into a PORTAL (radix), so every query below is
// document-wide via `screen` rather than scoped to render()'s container —
// `within(container)` would find nothing and every negative assertion would
// pass for the wrong reason.

// deferred hands a test the promise's own resolve/reject, so a row can hold
// the send in flight and assert on the state the modal shows while it waits.
function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function renderModal(over: Partial<React.ComponentProps<typeof AgentReplyModal>> = {}) {
  const props = {
    open: true,
    variant: "asked" as const,
    text: "Which repo should I open the change against?",
    replySeq: 1,
    onReply: vi.fn(async () => {}),
    onDismiss: vi.fn(),
    ...over,
  };
  return { ...render(<AgentReplyModal {...props} />), props };
}

describe("AgentReplyModal — the floor beneath a question the agent asked in prose", () => {
  it("renders nothing while closed", () => {
    renderModal({ open: false });
    expect(screen.queryByTestId("agent-ui-reply-modal")).toBeNull();
  });

  it("names what it is and shows the agent's own words", () => {
    renderModal();
    const modal = screen.getByTestId("agent-ui-reply-modal");
    expect(modal).toHaveTextContent("The agent is waiting for your reply");
    expect(modal).toHaveTextContent("Which repo should I open the change against?");
    expect(modal).toHaveAttribute("data-variant", "asked");
    expect(screen.getByRole("button", { name: "Reply" })).toBeInTheDocument();
  });

  // The "paused" variant is the runner's idle exit (signals.pausedIdle): the
  // agent replied and stopped without asking anything, and this is the only
  // place the pause becomes visible on the agent-defined view. Same body,
  // same seams — only the copy and the send-button label differ from "asked".
  it("renders the paused copy when the agent stopped without asking", () => {
    renderModal({ variant: "paused", text: "I'll move on to the tools now." });
    const modal = screen.getByTestId("agent-ui-reply-modal");
    expect(modal).toHaveAttribute("data-variant", "paused");
    expect(modal).toHaveTextContent("The agent paused");
    expect(modal).toHaveTextContent("It replied and stopped without asking anything. Send a message to continue.");
    expect(modal).toHaveTextContent("I'll move on to the tools now.");
    expect(screen.getByPlaceholderText("Tell it how to continue…")).toBeInTheDocument();
    expect(screen.queryByText("The agent is waiting for your reply")).toBeNull();
    fireEvent.change(screen.getByLabelText("Your reply"), { target: { value: "go on" } });
    expect(screen.getByRole("button", { name: "Continue" })).toBeEnabled();
  });

  // The agent's reply is the same text the transcript renders, so it goes
  // through the same markdown renderer and gets the same guarantee: markup in
  // it is escaped, never executed. A plain-text fallback here would make the
  // modal's copy read differently from the transcript's for identical bytes.
  it("renders the reply as markdown, exactly as the transcript does", () => {
    renderModal({ text: "Pick one: `alpha` or **beta**." });
    const modal = screen.getByTestId("agent-ui-reply-modal");
    expect(modal.querySelector("code")).toHaveTextContent("alpha");
    expect(modal.querySelector("strong")).toHaveTextContent("beta");
  });

  it("holds Reply disabled until there is something to send", () => {
    renderModal();
    expect(screen.getByRole("button", { name: "Reply" })).toBeDisabled();
    fireEvent.change(screen.getByLabelText("Your reply"), { target: { value: "org/repo" } });
    expect(screen.getByRole("button", { name: "Reply" })).toBeEnabled();
  });

  it("treats whitespace as nothing to send", () => {
    renderModal();
    fireEvent.change(screen.getByLabelText("Your reply"), { target: { value: "   " } });
    expect(screen.getByRole("button", { name: "Reply" })).toBeDisabled();
  });

  it("sends what was typed, and says so while the send is in flight", async () => {
    const d = deferred<void>();
    const onReply = vi.fn(() => d.promise);
    renderModal({ onReply });

    fireEvent.change(screen.getByLabelText("Your reply"), { target: { value: "org/repo" } });
    fireEvent.click(screen.getByRole("button", { name: "Reply" }));

    expect(onReply).toHaveBeenCalledWith("org/repo");
    // In flight: the control says what it is doing and cannot be fired twice.
    const sending = await screen.findByRole("button", { name: "Sending…" });
    expect(sending).toBeDisabled();

    d.resolve();
    await waitFor(() => expect(screen.getByRole("button", { name: "Reply" })).toBeInTheDocument());
  });

  // No-silent-errors: a send that failed must not look like one that landed.
  // The dialog stays open holding the typed text, so retrying costs nothing.
  it("says so when the send fails, keeps the dialog, and logs the cause", async () => {
    const err = new Error("network down");
    const logged = vi.spyOn(console, "error").mockImplementation(() => {});
    renderModal({ onReply: vi.fn(async () => { throw err; }) });

    fireEvent.change(screen.getByLabelText("Your reply"), { target: { value: "org/repo" } });
    fireEvent.click(screen.getByRole("button", { name: "Reply" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("Could not send your reply. Try again.");
    expect(screen.getByTestId("agent-ui-reply-modal")).toBeInTheDocument();
    expect(screen.getByLabelText("Your reply")).toHaveValue("org/repo");
    expect(logged).toHaveBeenCalled();
  });

  it("dismisses without sending when the viewer chooses Later", () => {
    const { props } = renderModal();
    fireEvent.click(screen.getByRole("button", { name: "Later" }));
    expect(props.onDismiss).toHaveBeenCalledTimes(1);
    expect(props.onReply).not.toHaveBeenCalled();
  });

  // Escape and the dialog's own close control are the same intent as Later:
  // "not now". Routing them anywhere else would leave the modal reopening on
  // the next render for a question the viewer already waved off.
  it("treats closing the dialog itself as Later", () => {
    const { props } = renderModal();
    fireEvent.keyDown(screen.getByTestId("agent-ui-reply-modal"), { key: "Escape" });
    expect(props.onDismiss).toHaveBeenCalled();
  });

  // A second question must not arrive pre-filled with the answer to the first.
  it("clears the field for a later question", () => {
    const { rerender, props } = renderModal();
    fireEvent.change(screen.getByLabelText("Your reply"), { target: { value: "org/repo" } });
    rerender(<AgentReplyModal {...props} open replySeq={2} text="And which branch?" />);
    expect(screen.getByLabelText("Your reply")).toHaveValue("");
  });

  // ...and closing is not a later question. The modal closes whenever the
  // viewer switches to the transcript tab, over the SAME ask; throwing the
  // draft away on that flip loses work the viewer did nothing to lose.
  it("keeps the draft across a close and reopen for the same reply", () => {
    const { rerender, props } = renderModal();
    fireEvent.change(screen.getByLabelText("Your reply"), { target: { value: "org/repo" } });
    rerender(<AgentReplyModal {...props} open={false} />);
    rerender(<AgentReplyModal {...props} open />);
    expect(screen.getByLabelText("Your reply")).toHaveValue("org/repo");
  });
});
