import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ActionsProvider } from "./actions";
import { EMPTY_HOOK_STATE, HookStateProvider } from "./hooks";
import { renderNode } from "./renderNode";
import type { Node } from "./types";

afterEach(cleanup);

function withAnswer(n: Node, answer = vi.fn(async () => {})) {
  render(<ActionsProvider value={{ states: {}, invoke: vi.fn(), answer }}>{renderNode(n)}</ActionsProvider>);
  return answer;
}

describe("ap:question", () => {
  it("renders the prompt and a text field, defaulting the button to Answer", () => {
    withAnswer({ component: "ap:question", props: { prompt: "Which repo should it watch?", placeholder: "org/repo" } });
    expect(screen.getByText("Which repo should it watch?")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("org/repo")).toBeInTheDocument();
    // The prompt doubles as the text field's accessible name (kind=text only —
    // a choice question's own options are each their own accessible label).
    expect(screen.getByLabelText("Which repo should it watch?")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Answer" })).toBeDisabled();
  });

  it("sends the typed text through answer() and then shows the sent state", async () => {
    const answer = withAnswer({ component: "ap:question", props: { prompt: "Name?", submitLabel: "Send" } });
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "notebot" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(answer).toHaveBeenCalledWith("notebot"));
    expect(await screen.findByTestId("ap-question-sent")).toBeInTheDocument();
  });

  it("sends the chosen choice's value for kind=choice", async () => {
    const answer = withAnswer({ component: "ap:question", props: { prompt: "Where?", kind: "choice",
      choices: [{ label: "Slack", value: "slack" }, { label: "GitHub", value: "github" }] } });
    fireEvent.click(screen.getByLabelText("GitHub"));
    fireEvent.click(screen.getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(answer).toHaveBeenCalledWith("github"));
  });

  it("keeps the form and shows an alert when sending fails", async () => {
    // The visible alert is one half of the no-silent-errors contract; the
    // other half is a logged line naming what failed, same as renderNode's own
    // fail-visible cards (see renderNode.tsx's console.error precedent).
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});
    withAnswer({ component: "ap:question", props: { prompt: "Name?" } }, vi.fn(async () => { throw new Error("down"); }));
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "Answer" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not send your answer");
    expect(screen.getByRole("textbox")).toBeInTheDocument();
    expect(consoleError).toHaveBeenCalled();
    consoleError.mockRestore();
  });

  // The agent asks one question at a time by REPAINTING the hook it asked in:
  // the tree position is identical, only the node's props differ. oap:generative
  // renders children by index and renderNode overwrites the element key with
  // that index, so React keeps the fiber — and with it, "sent". A viewer would
  // see the next question as "Sent — waiting for the agent." with no form, and
  // nothing on screen would ever ask again.
  it("a repainted question is a NEW question: a different prompt at the same position renders its own form", async () => {
    const answer = vi.fn(async () => {});
    const tree = (props: Record<string, unknown>) => (
      <ActionsProvider value={{ states: {}, invoke: vi.fn(), answer }}>
        {renderNode({ component: "ap:question", props } as Node)}
      </ActionsProvider>
    );
    const { rerender } = render(tree({ prompt: "Which repository should it watch?" }));

    fireEvent.change(screen.getByRole("textbox"), { target: { value: "org/repo" } });
    fireEvent.click(screen.getByRole("button", { name: "Answer" }));
    expect(await screen.findByTestId("ap-question-sent")).toBeInTheDocument();

    rerender(tree({ prompt: "Which channel should the digest go to?" }));

    expect(screen.getByText("Which channel should the digest go to?")).toBeInTheDocument();
    expect(screen.getByTestId("ap-question")).toBeInTheDocument();
    expect(screen.queryByTestId("ap-question-sent")).toBeNull();
    // Fresh state, not merely a fresh form: the earlier answer must not be
    // sitting in the field, and the button is back to disabled-until-typed.
    expect(screen.getByRole("textbox")).toHaveValue("");
    expect(screen.getByRole("button", { name: "Answer" })).toBeDisabled();
  });

  // A repaint that changes NOTHING is not a new question: the viewer's
  // half-typed answer must survive a re-render of the same node.
  it("keeps what the viewer typed when the same question is re-rendered", () => {
    const answer = vi.fn(async () => {});
    const tree = () => (
      <ActionsProvider value={{ states: {}, invoke: vi.fn(), answer }}>
        {renderNode({ component: "ap:question", props: { prompt: "Which repository should it watch?" } } as Node)}
      </ActionsProvider>
    );
    const { rerender } = render(tree());
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "org/re" } });
    rerender(tree());
    expect(screen.getByRole("textbox")).toHaveValue("org/re");
  });

  // choices is agent-declared and reaches this component unvalidated (the
  // registry entry does not shape-check it the way ap:table/ap:steps do) — a
  // non-array must degrade to "no options" rather than throw past renderNode's
  // try/catch and take the whole tree down with it.
  it("does not throw and still shows the prompt when choices is not an array", () => {
    expect(() =>
      withAnswer({
        component: "ap:question",
        props: { prompt: "Where?", kind: "choice", choices: "nope" },
      }),
    ).not.toThrow();
    expect(screen.getByText("Where?")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Answer" })).toBeDisabled();
  });
});

// "Sent" made server-computable: a hook the server says the viewer already
// answered renders its ap:question as sent on the FIRST paint, with no local
// `sent` state having ever fired — the whole point being that a reload must
// not re-offer a question the viewer already replied to (or moved past).
describe("ap:question in a hook the viewer already answered", () => {
  it("renders as sent, not as a question, when the page says the hook is answered", () => {
    const page: Node = {
      component: "oap:generative",
      props: { name: "brief", allowedComponents: ["*"] },
      children: [{ component: "ap:question", props: { prompt: "Ready?", submitLabel: "Confirm" } }],
    };
    render(
      <HookStateProvider value={{ ...EMPTY_HOOK_STATE, composed: new Set(["brief"]), answered: new Set(["brief"]) }}>
        <ActionsProvider value={{ states: {}, invoke: vi.fn(), answer: vi.fn(async () => {}) }}>{renderNode(page)}</ActionsProvider>
      </HookStateProvider>,
    );
    expect(screen.getByTestId("ap-question-sent")).toHaveTextContent("Sent — waiting for the agent.");
    expect(screen.queryByRole("textbox")).toBeNull();
  });
});

describe("ap:question while the agent is busy", () => {
  it("disables the answer control until the turn ends", () => {
    const n: Node = { component: "ap:question", props: { prompt: "Ready?", submitLabel: "Confirm" } };
    const value = { states: {}, invoke: vi.fn(), answer: vi.fn(async () => {}) };
    const { rerender } = render(<ActionsProvider value={{ ...value, busy: true }}>{renderNode(n)}</ActionsProvider>);
    expect(screen.getByRole("textbox")).toBeDisabled();
    expect(screen.getByRole("button", { name: "Confirm" })).toBeDisabled();
    rerender(<ActionsProvider value={{ ...value, busy: false }}>{renderNode(n)}</ActionsProvider>);
    expect(screen.getByRole("textbox")).toBeEnabled();
  });
});
