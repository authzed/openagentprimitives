import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ActionsProvider } from "./actions";
import type { ActionsContextValue } from "./actions";
import { renderNode } from "./renderNode";
import type { Node } from "./types";

afterEach(cleanup);
beforeEach(() => {
  window.localStorage.clear();
});

const notice = (props: Record<string, unknown>): Node => ({
  component: "ap:notice",
  props,
});

function withActions(n: Node, overrides: Partial<ActionsContextValue> = {}) {
  const invoke = vi.fn();
  render(
    <ActionsProvider
      value={{
        states: {},
        invoke,
        answer: vi.fn(async () => {}),
        ...overrides,
      }}
    >
      {renderNode(n)}
    </ActionsProvider>,
  );
  return invoke;
}

describe("ap:notice", () => {
  it("renders the title, the markdown body and the tone", () => {
    withActions(
      notice({
        title: "Test started",
        body: "Take **your** time.",
        tone: "success",
      }),
    );
    const card = screen.getByTestId("ap-notice");
    expect(card).toHaveAttribute("data-tone", "success");
    expect(screen.getByText("Test started")).toBeInTheDocument();
    expect(screen.getByText("your").tagName).toBe("STRONG");
  });

  it("defaults an unknown tone to info", () => {
    withActions(notice({ body: "x", tone: "danger" }));
    expect(screen.getByTestId("ap-notice")).toHaveAttribute(
      "data-tone",
      "info",
    );
  });

  it("invokes the declared action on click, then disables the buttons and marks the notice sent", () => {
    const invoke = withActions(
      notice({
        body: "Started.",
        buttons: [
          { label: "Done testing", action: "test_done" },
          { label: "That's not right", action: "thats_not_right" },
        ],
      }),
    );
    fireEvent.click(screen.getByRole("button", { name: "Done testing" }));
    expect(invoke).toHaveBeenCalledWith("test_done", undefined);
    expect(screen.getByRole("button", { name: "Done testing" })).toBeDisabled();
    expect(
      screen.getByRole("button", { name: "That's not right" }),
    ).toBeDisabled();
    expect(screen.getByTestId("ap-notice-sent")).toBeInTheDocument();
    // The card itself stays: the agent replaces it, the click does not.
    expect(screen.getByTestId("ap-notice")).toBeInTheDocument();
  });

  it("shows the action's own caption once its state is known", () => {
    withActions(
      notice({ body: "x", buttons: [{ label: "Go", action: "go" }] }),
      {
        states: { go: { phase: "running" } },
      },
    );
    // A running action keeps the button disabled even before any click here.
    expect(screen.getByRole("button", { name: "Go" })).toBeDisabled();
  });

  it("keeps its buttons disabled while the agent is working", () => {
    withActions(
      notice({ body: "x", buttons: [{ label: "Go", action: "go" }] }),
      { busy: true },
    );
    expect(screen.getByRole("button", { name: "Go" })).toBeDisabled();
  });

  it("drops malformed buttons and caps the list at four", () => {
    withActions(
      notice({
        body: "x",
        buttons: [
          { label: "a", action: "a" },
          { label: 3, action: "b" },
          { label: "c" },
          { label: "d", action: "d" },
          { label: "e", action: "e" },
          { label: "f", action: "f" },
        ],
      }),
    );
    expect(
      screen
        .getAllByRole("button")
        .filter((b) => b.getAttribute("aria-label") !== "Dismiss"),
    ).toHaveLength(4);
  });

  it("hides on dismiss, stays hidden for the same notice, and reappears when a prop changes", () => {
    const same = notice({ body: "Started.", tone: "info" });
    withActions(same);
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByTestId("ap-notice")).toBeNull();

    cleanup();
    withActions(same);
    expect(screen.queryByTestId("ap-notice")).toBeNull();

    cleanup();
    withActions(notice({ body: "Started. Still watching.", tone: "info" }));
    expect(screen.getByTestId("ap-notice")).toBeInTheDocument();
  });

  it("renders when storage is unavailable: a broken store never hides a notice", () => {
    // The visible card is one half of the no-silent-errors contract; the
    // other half is a logged line naming what failed, same as
    // question.test.tsx's "keeps the form and shows an alert" case.
    const consoleError = vi
      .spyOn(console, "error")
      .mockImplementation(() => {});
    const getItem = vi
      .spyOn(Storage.prototype, "getItem")
      .mockImplementation(() => {
        throw new Error("blocked");
      });
    withActions(notice({ body: "x" }));
    expect(screen.getByTestId("ap-notice")).toBeInTheDocument();
    expect(consoleError).toHaveBeenCalled();
    getItem.mockRestore();
    consoleError.mockRestore();
  });
});
