import "@testing-library/jest-dom/vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  within,
} from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { EMPTY_HOOK_STATE, HookStateProvider, type HookState } from "./hooks";
import { renderNode } from "./renderNode";
import type { Node } from "./types";

afterEach(cleanup);

function node(
  component: string,
  props?: Record<string, unknown>,
  children?: Node[],
): Node {
  return {
    component,
    ...(props ? { props } : {}),
    ...(children ? { children } : {}),
  };
}

const timeline = node(
  "oap:generative",
  { name: "phase", allowedComponents: ["ap:steps"] },
  [
    node("ap:steps", {
      pinned: true,
      steps: [
        {
          id: "assess",
          label: "Intake",
          state: "done",
          summary: "demo-haiku · a haiku on any topic",
        },
        { id: "tools", label: "Tools", state: "active", summary: "none" },
        { id: "deliver", label: "Deliver", state: "upcoming" },
      ],
    }),
  ],
);

function railPage(): Node {
  return node("oap:page", { layout: "rail" }, [
    node("ap:heading", { text: "Agent Builder", level: 2 }),
    timeline,
    node("oap:generative", { name: "questions", allowedComponents: ["*"] }, [
      node("ap:markdown", { body: "always shown" }),
    ]),
    node(
      "oap:generative",
      { name: "agent", step: "assess", allowedComponents: ["*"] },
      [node("ap:markdown", { body: "the agent panel" })],
    ),
    node(
      "oap:generative",
      { name: "tools", step: "tools", allowedComponents: ["*"] },
      [node("ap:markdown", { body: "tools panel" })],
    ),
  ]);
}

// steppedPage builds a rail page over two steps — `tools` and `permissions` —
// with one hook bound to each. `active` names the step the agent says it is
// on; `summary` repaints the first step's one-line fact. Every call returns a
// NEW object, the way a repaint arrives over the wire, so a rerender with it
// exercises the page's own memoization rather than reusing the same node.
function steppedPage(active: string, summary?: string): Node {
  return node("oap:page", { layout: "rail" }, [
    node("oap:generative", { name: "phase", allowedComponents: ["ap:steps"] }, [
      node("ap:steps", {
        steps: [
          {
            id: "tools",
            label: "Tools",
            state: active === "tools" ? "active" : "done",
            ...(summary ? { summary } : {}),
          },
          {
            id: "permissions",
            label: "Permissions",
            state: active === "permissions" ? "active" : "upcoming",
          },
        ],
      }),
    ]),
    node(
      "oap:generative",
      { name: "tools", step: "tools", allowedComponents: ["*"] },
      [node("ap:markdown", { body: "tools panel" })],
    ),
    node(
      "oap:generative",
      { name: "permissions", step: "permissions", allowedComponents: ["*"] },
      [node("ap:markdown", { body: "permissions panel" })],
    ),
  ]);
}

function pageTree(n: Node, partial: Partial<HookState> = {}) {
  return (
    <HookStateProvider value={{ ...EMPTY_HOOK_STATE, ...partial }}>
      {renderNode(n)}
    </HookStateProvider>
  );
}

function renderPage(n: Node, partial: Partial<HookState> = {}) {
  return render(pageTree(n, partial));
}

// The rail step whose data-step-id is `id`, and the click that selects it —
// the same route a person takes, rather than reaching into the page's state.
function railStep(id: string): HTMLElement {
  const li = screen
    .getAllByTestId("agent-ui-rail-step")
    .find((el) => el.getAttribute("data-step-id") === id);
  if (!li) throw new Error(`no rail step with data-step-id="${id}"`);
  return li;
}

function selectStep(id: string) {
  fireEvent.click(within(railStep(id)).getByRole("button"));
}

describe("oap:page layout=rail", () => {
  it("draws the timeline as a rail with each step's label and summary", () => {
    renderPage(railPage());
    const rail = screen.getByTestId("agent-ui-page-rail");
    const steps = within(rail).getAllByTestId("agent-ui-rail-step");
    expect(steps).toHaveLength(3);
    expect(steps[0]).toHaveTextContent("Intake");
    expect(steps[0]).toHaveTextContent("demo-haiku · a haiku on any topic");
    expect(steps[1]).toHaveTextContent("none");
    expect(steps[2]).not.toHaveTextContent("·");
  });

  it("shows the active step's hooks in the stage by default, and unbound hooks always", () => {
    renderPage(railPage());
    const stage = screen.getByTestId("agent-ui-page-stage");
    expect(within(stage).getByText("tools panel")).toBeInTheDocument();
    expect(within(stage).getByText("always shown")).toBeInTheDocument();
    expect(within(stage).queryByText("the agent panel")).toBeNull();
    expect(within(stage).getByText("Agent Builder")).toBeInTheDocument();
  });

  it("clicking a rail step shows that step's hooks instead", () => {
    renderPage(railPage());
    selectStep("assess");
    const stage = screen.getByTestId("agent-ui-page-stage");
    expect(within(stage).getByText("the agent panel")).toBeInTheDocument();
    expect(within(stage).queryByText("tools panel")).toBeNull();
    expect(railStep("assess")).toHaveAttribute("data-selected", "true");
  });

  it("never folds a hook under the rail: a done step's hook shows open when selected", () => {
    renderPage(railPage());
    selectStep("assess");
    expect(screen.queryByTestId("agent-ui-hook-collapsed")).toBeNull();
  });

  it("marks the ACTIVE step current for assistive tech, and the selected one pressed", () => {
    // The two facts are different: which phase the agent is in, and which one
    // the person is looking at. Tying aria-current to the selection would
    // leave a screen-reader user who clicked back with no way to hear the
    // first.
    renderPage(railPage());
    expect(within(railStep("tools")).getByRole("button")).toHaveAttribute(
      "aria-current",
      "step",
    );
    expect(within(railStep("tools")).getByRole("button")).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    selectStep("assess");
    expect(within(railStep("tools")).getByRole("button")).toHaveAttribute(
      "aria-current",
      "step",
    );
    expect(within(railStep("assess")).getByRole("button")).not.toHaveAttribute(
      "aria-current",
    );
    expect(within(railStep("assess")).getByRole("button")).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(within(railStep("tools")).getByRole("button")).toHaveAttribute(
      "aria-pressed",
      "false",
    );
  });

  it("names the rail itself, so its steps are not an unlabelled list", () => {
    renderPage(railPage());
    expect(
      within(screen.getByTestId("agent-ui-page-rail")).getByRole("list"),
    ).toHaveAttribute("aria-label", "Steps");
  });

  it("with no active step, shows the last finished step's hooks instead of nothing", () => {
    const allDone = node("oap:page", { layout: "rail" }, [
      node(
        "oap:generative",
        { name: "phase", allowedComponents: ["ap:steps"] },
        [
          node("ap:steps", {
            steps: [
              { id: "assess", label: "Intake", state: "done" },
              {
                id: "deliver",
                label: "Deliver",
                state: "done",
                summary: "draft saved",
              },
            ],
          }),
        ],
      ),
      node(
        "oap:generative",
        { name: "agent", step: "assess", allowedComponents: ["*"] },
        [node("ap:markdown", { body: "the agent panel" })],
      ),
      node(
        "oap:generative",
        { name: "deliver", step: "deliver", allowedComponents: ["*"] },
        [node("ap:markdown", { body: "the outcome" })],
      ),
    ]);
    renderPage(allDone);
    const stage = screen.getByTestId("agent-ui-page-stage");
    expect(within(stage).getByText("the outcome")).toBeInTheDocument();
    expect(within(stage).queryByText("the agent panel")).toBeNull();
    expect(railStep("deliver")).toHaveAttribute("data-selected", "true");
  });

  it("with every step upcoming, stages the first step's hook rather than an empty stage", () => {
    const untouched = node("oap:page", { layout: "rail" }, [
      node(
        "oap:generative",
        { name: "phase", allowedComponents: ["ap:steps"] },
        [
          node("ap:steps", {
            steps: [
              { id: "assess", label: "Intake", state: "upcoming" },
              { id: "deliver", label: "Deliver", state: "upcoming" },
            ],
          }),
        ],
      ),
      node(
        "oap:generative",
        { name: "agent", step: "assess", allowedComponents: ["*"] },
        [node("ap:markdown", { body: "the agent panel" })],
      ),
      node(
        "oap:generative",
        { name: "deliver", step: "deliver", allowedComponents: ["*"] },
        [node("ap:markdown", { body: "the outcome" })],
      ),
    ]);
    renderPage(untouched);
    const stage = screen.getByTestId("agent-ui-page-stage");
    expect(within(stage).getByText("the agent panel")).toBeInTheDocument();
    expect(within(stage).queryByText("the outcome")).toBeNull();
  });
});

// A question painted into a hook bound to a step nobody is looking at is
// still a question the page is waiting on: AgentUIView suppresses its own
// reply modal for the WHOLE declared tree (its questionShowing), so a hook
// the stage filtered out would leave the viewer with nothing to answer and no
// way to reach it.
describe("oap:page layout=rail — a waiting hook is always on the stage", () => {
  function askingLater(): Node {
    return node("oap:page", { layout: "rail" }, [
      node(
        "oap:generative",
        { name: "phase", allowedComponents: ["ap:steps"] },
        [
          node("ap:steps", {
            steps: [
              { id: "assess", label: "Intake", state: "active" },
              { id: "tools", label: "Tools", state: "upcoming" },
            ],
          }),
        ],
      ),
      node(
        "oap:generative",
        { name: "agent", step: "assess", allowedComponents: ["*"] },
        [node("ap:markdown", { body: "the agent panel" })],
      ),
      node(
        "oap:generative",
        { name: "tools", step: "tools", allowedComponents: ["*"] },
        [node("ap:markdown", { body: "tools panel" })],
      ),
    ]);
  }

  it("stages a hook the page is waiting on even though its step is not selected", () => {
    renderPage(askingLater(), { waiting: new Set(["tools"]) });
    const stage = screen.getByTestId("agent-ui-page-stage");
    expect(within(stage).getByText("tools panel")).toBeInTheDocument();
    expect(within(stage).getByText("the agent panel")).toBeInTheDocument();
  });

  it("leaves that same hook off the stage when nothing is waiting on it", () => {
    renderPage(askingLater());
    const stage = screen.getByTestId("agent-ui-page-stage");
    expect(within(stage).queryByText("tools panel")).toBeNull();
    expect(within(stage).getByText("the agent panel")).toBeInTheDocument();
  });
});

// A staged child's key must not depend on WHICH children are staged: React
// reconciles by key, so a key drawn from the filtered array hands one step's
// component instances to the next step's hook — a fold's open state, a form's
// typed values — with nothing in the markup to show for it.
describe("oap:page layout=rail — each step keeps its own component state", () => {
  function foldedSteps(): Node {
    return node("oap:page", { layout: "rail" }, [
      node(
        "oap:generative",
        { name: "phase", allowedComponents: ["ap:steps"] },
        [
          node("ap:steps", {
            steps: [
              { id: "tools", label: "Tools", state: "active" },
              { id: "permissions", label: "Permissions", state: "upcoming" },
            ],
          }),
        ],
      ),
      node(
        "oap:generative",
        { name: "tools", step: "tools", allowedComponents: ["*"] },
        [
          node("ap:collapsible", { title: "Tools", collapsed: true }, [
            node("ap:markdown", { body: "tools detail" }),
          ]),
        ],
      ),
      node(
        "oap:generative",
        { name: "permissions", step: "permissions", allowedComponents: ["*"] },
        [
          node("ap:collapsible", { title: "Permissions", collapsed: true }, [
            node("ap:markdown", { body: "permissions detail" }),
          ]),
        ],
      ),
    ]);
  }

  it("does not carry a fold opened under one step into the next step's fold", () => {
    renderPage(foldedSteps());
    const stage = () => screen.getByTestId("agent-ui-page-stage");
    const fold = () => within(stage()).getByTestId("ap-collapsible");
    expect(fold().querySelector("summary")).toHaveTextContent("Tools");
    fireEvent.click(fold().querySelector("summary")!);
    expect(fold()).toHaveAttribute("open");
    selectStep("permissions");
    expect(fold().querySelector("summary")).toHaveTextContent("Permissions");
    expect(fold()).not.toHaveAttribute("open");
  });
});

// The selection contract in both directions: the agent decides which step is
// DEFAULT by repainting the timeline, the person decides which step they are
// LOOKING at, and a repaint that does not move the phase never takes the
// person's excursion away from them.
describe("oap:page layout=rail — the selection follows the default step, and only it", () => {
  it("drops the pick when the timeline advances, so the new phase is what shows", () => {
    const { rerender } = renderPage(steppedPage("tools"));
    selectStep("tools");
    rerender(pageTree(steppedPage("permissions")));
    const stage = screen.getByTestId("agent-ui-page-stage");
    expect(within(stage).getByText("permissions panel")).toBeInTheDocument();
    expect(within(stage).queryByText("tools panel")).toBeNull();
  });

  it("keeps the pick through a repaint that leaves the active step where it was", () => {
    const { rerender } = renderPage(steppedPage("permissions"));
    selectStep("tools");
    rerender(pageTree(steppedPage("permissions", "2 selected")));
    const stage = screen.getByTestId("agent-ui-page-stage");
    expect(within(stage).getByText("tools panel")).toBeInTheDocument();
    expect(within(stage).queryByText("permissions panel")).toBeNull();
    expect(railStep("tools")).toHaveTextContent("2 selected");
  });
});

describe("oap:page layout=column and no page", () => {
  it("renders children in a single column, the timeline horizontal, exactly as without a page root", () => {
    const columnPage = node("oap:page", {}, [
      timeline,
      node("ap:markdown", { body: "below" }),
    ]);
    const { container: withRoot } = renderPage(columnPage);
    // Assert on withRoot before tearing it down: cleanup() unmounts every
    // render()ed root (not only the next one's), which would otherwise empty
    // withRoot's container before its own assertions ran.
    expect(
      withRoot.querySelector('[data-testid="agent-ui-steps"]'),
    ).not.toBeNull();
    expect(
      withRoot.querySelector('[data-testid="agent-ui-page-rail"]'),
    ).toBeNull();
    cleanup();
    const { container: withoutRoot } = renderPage(
      node("ap:stack", { direction: "vertical", gap: "sm" }, [
        timeline,
        node("ap:markdown", { body: "below" }),
      ]),
    );
    expect(
      withoutRoot.querySelector('[data-testid="agent-ui-steps"]'),
    ).not.toBeNull();
  });
});
