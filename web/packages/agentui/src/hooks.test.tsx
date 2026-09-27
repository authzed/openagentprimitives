import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { renderNode } from "./renderNode";
import {
  GenerativeHook,
  HookStateProvider,
  EMPTY_HOOK_STATE,
  useEnclosingHook,
  type HookState,
} from "./hooks";
import { PageLayoutContext } from "./pageLayout";
import type { Node } from "./types";

// vitest.config.ts sets globals: false, so @testing-library/react never finds a
// global afterEach to register its cleanup with — every render() past the first
// would accumulate in document.body, and hookEl's document-wide query would
// then match an earlier test's leftover region. See renderNode.test.tsx for the
// same repo pattern.
afterEach(cleanup);

const hook = (name: string, children: Node[] = []): Node => ({
  component: "oap:generative",
  props: { name, allowedComponents: ["*"] },
  children,
});
const withState = (n: Node, partial: Partial<HookState>) =>
  render(
    <HookStateProvider value={{ ...EMPTY_HOOK_STATE, ...partial }}>
      {renderNode(n)}
    </HookStateProvider>,
  );
const hookEl = (name: string) =>
  document.querySelector(`[data-hook="${name}"]`) as HTMLElement;

describe("oap:generative", () => {
  it("renders its author children in place", () => {
    render(
      renderNode(
        hook("brief", [
          { component: "ap:text", props: { text: "author copy" } },
        ]),
      ),
    );
    expect(hookEl("brief")).toBeInTheDocument();
    expect(screen.getByText("author copy")).toBeInTheDocument();
  });

  it("collapses in place when it has no children: the element exists, nothing shows", () => {
    render(renderNode(hook("later")));
    const el = hookEl("later");
    expect(el).toBeInTheDocument();
    expect(el.textContent).toBe("");
  });

  it("marks a hook the agent composed, in the attribute and in what a person sees", () => {
    withState(
      hook("panel", [
        { component: "ap:markdown", props: { body: "agent copy" } },
      ]),
      { composed: new Set(["panel"]) },
    );
    const el = hookEl("panel");
    expect(el).toHaveAttribute("data-agent-composed", "true");
    // The visible half of the same fact. Asserted together with the attribute
    // because the attribute alone would stay green if the styling were dropped,
    // and the styling IS the marker a person sees.
    expect(el.className).toContain("bg-state/[0.05]");
  });

  it("wears the composed mark as a wash on the region's own surface, with no edge and no indent", () => {
    const { container } = withState(
      hook("r", [{ component: "ap:markdown", props: { body: "x" } }]),
      { composed: new Set(["r"]) },
    );
    const section = container.querySelector('section[data-hook="r"]')!;
    expect(section.className).toContain("bg-state/[0.05]");
    expect(section.className).not.toMatch(/border-l|pl-3|border-state/);
    expect(section.className).toContain("w-full");
  });

  it("does not mark a hook the agent has not written", () => {
    withState(
      hook("notes", [{ component: "ap:text", props: { text: "author" } }]),
      {},
    );
    expect(hookEl("notes")).not.toHaveAttribute("data-agent-composed");
  });

  it("shows the updated cue for the hook named, including an EMPTY (cleared) one", () => {
    withState(
      {
        component: "ap:stack",
        children: [
          hook("a", [{ component: "ap:text", props: { text: "x" } }]),
          hook("b"),
        ],
      },
      { updated: new Map([["b", 1]]) },
    );
    expect(hookEl("b")).toHaveAttribute("data-hook-updated", "true");
    expect(hookEl("a")).not.toHaveAttribute("data-hook-updated");
    expect(screen.getByTestId("agent-ui-hook-updated")).toBeInTheDocument();
  });

  it("the mark survives every removal the vocabulary can express", () => {
    // A hostile fill: nesting, an error node, raw_html, an empty stack. None of
    // these can reach outside renderNode's own subtree, which is why the mark
    // is carried by the hook's own platform-emitted <section> instead.
    withState(
      hook("panel", [
        { component: "ap:error", props: { title: "x", body: "y" } },
        {
          component: "ap:raw_html",
          props: { html: "<style>*{display:none}</style>" },
        },
        { component: "ap:stack", children: [] },
      ]),
      { composed: new Set(["panel"]) },
    );
    expect(hookEl("panel")).toHaveAttribute("data-agent-composed", "true");
  });

  it("marks a hook the agent composed as EMPTY", () => {
    // A hook with no children still carries the mark: "the agent wrote this
    // region, and what it wrote was nothing" is a different statement from
    // "the author left this region alone", and only the mark distinguishes
    // them. The server puts a cleared hook's name in agentComposed for exactly
    // this reason.
    withState(hook("panel"), { composed: new Set(["panel"]) });
    expect(hookEl("panel")).toHaveAttribute("data-agent-composed", "true");
  });

  it("adds no readable text of its own to a marked region", () => {
    // The marker costs the declaration no vertical space and injects no words
    // into the agent's own content: it is the region's own appearance, with no
    // caption of its own and no legend anywhere naming it.
    withState(
      hook("panel", [{ component: "ap:text", props: { text: "agent copy" } }]),
      { composed: new Set(["panel"]) },
    );
    expect(hookEl("panel").textContent).toBe("agent copy");
  });

  it("shows the refreshing indicator for a stale region, and marks it aria-busy", () => {
    // Stale is a THIRD statement, distinct from both composed and updated: the
    // content below is a moment out of date while its bindings re-resolve, not
    // wrong and not newly written. It gets its own testid so a selector can
    // never pass one for the other.
    withState(
      hook("data", [{ component: "ap:text", props: { text: "last answer" } }]),
      { stale: new Set(["data"]) },
    );
    const el = hookEl("data");
    expect(el).toHaveAttribute("aria-busy", "true");
    expect(screen.getByTestId("agent-ui-hook-refreshing")).toBeInTheDocument();
    // The previous answer stays readable and in place: it is the best available
    // one until the next arrives.
    expect(screen.getByText("last answer")).toBeInTheDocument();
  });

  it("shows only the updated cue when a region is both updated and stale", () => {
    // Two indicators anchored to the same corner would overlap. "The agent just
    // rewrote this" is the newer and more consequential fact, so it wins.
    withState(hook("both"), {
      updated: new Map([["both", 1]]),
      stale: new Set(["both"]),
    });
    expect(screen.getByTestId("agent-ui-hook-updated")).toBeInTheDocument();
    expect(
      screen.queryByTestId("agent-ui-hook-refreshing"),
    ).not.toBeInTheDocument();
  });

  it("marks a hook the page has flagged as waiting on the viewer, and names it for a person", () => {
    // waiting is computed by the PAGE from the tree (a hook whose subtree holds
    // an ap:question) — never something a node claims about itself, hence no
    // node prop drives this case, only the provided HookState.
    withState(
      { component: "ap:stack", children: [hook("brief"), hook("other")] },
      { waiting: new Set(["brief"]) },
    );
    expect(hookEl("brief")).toHaveAttribute("data-hook-waiting", "true");
    expect(screen.getByText("Waiting on you")).toBeInTheDocument();
    expect(hookEl("other")).not.toHaveAttribute("data-hook-waiting");
  });

  it("reads EMPTY_HOOK_STATE outside a provider, rather than failing to render", () => {
    // The package is usable standalone — the fixture suite renders a whole view
    // with no provider at all — so the context default must be a real, empty
    // state, not undefined.
    render(
      renderNode(
        hook("solo", [{ component: "ap:text", props: { text: "standalone" } }]),
      ),
    );
    const el = hookEl("solo");
    expect(el).not.toHaveAttribute("data-agent-composed");
    expect(el).not.toHaveAttribute("data-hook-updated");
    expect(screen.getByText("standalone")).toBeInTheDocument();
  });

  it("folds a hook the page says is collapsed, titled by the page, and marks it", () => {
    withState(
      hook("brief", [{ component: "ap:text", props: { text: "the brief" } }]),
      { collapsed: new Map([["brief", "Intake"]]) },
    );
    const el = hookEl("brief");
    expect(el).toHaveAttribute("data-hook-collapsed", "true");
    const fold = el.querySelector(
      '[data-testid="agent-ui-hook-collapsed"]',
    ) as HTMLDetailsElement;
    expect(fold).not.toHaveAttribute("open");
    expect(fold.querySelector("summary")).toHaveTextContent("Intake");
    expect(screen.getByText("the brief")).toBeInTheDocument();
  });

  it("does not fold an empty hook: there is nothing to hide and a fold would show a stray header", () => {
    withState(hook("brief"), { collapsed: new Map([["brief", "Intake"]]) });
    expect(hookEl("brief")).not.toHaveAttribute("data-hook-collapsed");
    expect(hookEl("brief").querySelector("details")).toBeNull();
  });

  it("re-folds when the page's verdict returns, and a manual reopen holds only until it changes", () => {
    // GenerativeHook renders Disclosure with collapsed always true and lets
    // the fold UNMOUNT when the hook leaves the collapsed map: that unmount
    // is the whole mechanism behind "the person's toggle wins until the step
    // changes", so this test drives the map, not the prop.
    const n = hook("brief", [
      { component: "ap:text", props: { text: "the brief" } },
    ]);
    const folded = {
      ...EMPTY_HOOK_STATE,
      collapsed: new Map([["brief", "Intake"]]),
    };
    const { rerender } = render(
      <HookStateProvider value={folded}>{renderNode(n)}</HookStateProvider>,
    );
    const details = () =>
      hookEl("brief").querySelector("details") as HTMLDetailsElement | null;
    expect(details()).not.toHaveAttribute("open");
    fireEvent.click(details()!.querySelector("summary")!);
    expect(details()).toHaveAttribute("open");
    // The same verdict again (a repaint that changed nothing here): the reopen stands.
    rerender(
      <HookStateProvider
        value={{ ...folded, collapsed: new Map([["brief", "Intake"]]) }}
      >
        {renderNode(n)}
      </HookStateProvider>,
    );
    expect(details()).toHaveAttribute("open");
    // The step is active again: no fold at all.
    rerender(
      <HookStateProvider value={EMPTY_HOOK_STATE}>
        {renderNode(n)}
      </HookStateProvider>,
    );
    expect(details()).toBeNull();
    expect(hookEl("brief")).not.toHaveAttribute("data-hook-collapsed");
    // Done once more: folded closed, the earlier reopen forgotten.
    rerender(
      <HookStateProvider value={folded}>{renderNode(n)}</HookStateProvider>,
    );
    expect(details()).not.toHaveAttribute("open");
  });
});

// useEnclosingHook is question.tsx's only way to know which hook it is
// rendering inside — GenerativeHook is the sole provider of that context, so
// a component reads its own enclosing region without any node ever naming
// itself.
describe("useEnclosingHook", () => {
  function Probe() {
    const name = useEnclosingHook();
    return <span data-testid="probe">{name === null ? "null" : name}</span>;
  }

  it("returns the hook's name for a component rendered inside it", () => {
    render(
      <GenerativeHook name="brief">
        <Probe />
      </GenerativeHook>,
    );
    expect(screen.getByTestId("probe")).toHaveTextContent("brief");
  });

  it("returns null for a component rendered outside any hook", () => {
    render(<Probe />);
    expect(screen.getByTestId("probe")).toHaveTextContent("null");
  });
});

describe("oap:generative — what a person sees of the wash and the cues", () => {
  it("paints no wash for a hook the agent cleared: the mark stays in the attribute, nothing shows", () => {
    // A cleared hook is composed (the server says so) but has nothing in it.
    // The wash covers CONTENT; around nothing it would be a stray tinted box
    // on the page.
    withState(
      { component: "ap:stack", children: [hook("intake")] },
      { composed: new Set(["intake"]) },
    );
    expect(hookEl("intake")).toHaveAttribute("data-agent-composed", "true");
    expect(hookEl("intake").className).not.toContain("bg-state");
    expect(hookEl("intake")).toBeEmptyDOMElement();
  });

  it("washes a composed hook that has content", () => {
    withState(
      {
        component: "ap:stack",
        children: [
          hook("brief", [{ component: "ap:text", props: { text: "Summary" } }]),
        ],
      },
      { composed: new Set(["brief"]) },
    );
    expect(hookEl("brief").className).toContain("bg-state/[0.05]");
  });

  it("puts the cues above the content, in flow, never over it", () => {
    withState(
      {
        component: "ap:stack",
        children: [
          hook("brief", [
            { component: "ap:text", props: { text: "First line" } },
          ]),
        ],
      },
      {
        composed: new Set(["brief"]),
        waiting: new Set(["brief"]),
        updated: new Map([["brief", Date.now()]]),
      },
    );
    const waiting = document.querySelector(
      '[data-testid="agent-ui-hook-waiting"]',
    ) as HTMLElement;
    const updated = document.querySelector(
      '[data-testid="agent-ui-hook-updated"]',
    ) as HTMLElement;
    const content = Array.from(
      document.querySelectorAll('[data-hook="brief"] *'),
    ).find((e) => e.textContent === "First line") as HTMLElement;
    expect(waiting.className).not.toMatch(/absolute/);
    expect(updated.className).not.toMatch(/absolute/);
    // DOM order: the cue row precedes the content, so it reserves its own space.
    expect(
      waiting.compareDocumentPosition(content) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });
});

// Under a rail layout the page stages a hook by its step and nothing folds, so
// the Disclosure branch — the only path that ever drew a hook's own title —
// never renders. Without a heading of its own an agent-written region arrives
// on the stage as unlabelled prose.
describe("oap:generative under a rail layout", () => {
  const titled = (
    name: string,
    title: string,
    children: Node[] = [],
  ): Node => ({
    component: "oap:generative",
    props: { name, title, allowedComponents: ["*"] },
    children,
  });
  const inRail = (n: Node, partial: Partial<HookState> = {}) =>
    render(
      <PageLayoutContext.Provider
        value={{ layout: "rail", selectedStep: null, selectStep: () => {} }}
      >
        <HookStateProvider value={{ ...EMPTY_HOOK_STATE, ...partial }}>
          {renderNode(n)}
        </HookStateProvider>
      </PageLayoutContext.Provider>,
    );

  it("heads a titled hook's content with its own title", () => {
    inRail(
      titled("brief", "Brief", [
        { component: "ap:markdown", props: { body: "the running summary" } },
      ]),
    );
    const heading = screen.getByTestId("agent-ui-hook-title");
    expect(heading).toHaveTextContent("Brief");
    expect(heading.tagName).toBe("H3");
    // First inside the region, above the content it names.
    expect(hookEl("brief").firstElementChild).toBe(heading);
    expect(screen.getByText("the running summary")).toBeInTheDocument();
  });

  it("draws no heading over an empty hook: a header with nothing under it is chrome over nothing", () => {
    inRail(titled("brief", "Brief"));
    expect(screen.queryByTestId("agent-ui-hook-title")).toBeNull();
  });

  it("draws no heading for a hook the author gave no title", () => {
    inRail(hook("brief", [{ component: "ap:markdown", props: { body: "x" } }]));
    expect(screen.queryByTestId("agent-ui-hook-title")).toBeNull();
  });

  it("draws no heading under a column layout: there the fold's own title is the header", () => {
    withState(
      titled("brief", "Brief", [
        { component: "ap:markdown", props: { body: "the running summary" } },
      ]),
      {
        collapsed: new Map([["brief", "Intake"]]),
      },
    );
    expect(screen.queryByTestId("agent-ui-hook-title")).toBeNull();
    expect(hookEl("brief").querySelector("summary")).toHaveTextContent(
      "Intake",
    );
  });

  // Under a rail the question card IS the ask, and a hook the agent is waiting
  // on is always the one staged — so the chip labels what the person is
  // already looking at. The attribute stays: it is the machine-readable fact,
  // and the page's own staging reads it.
  it("shows no waiting chip under a rail: the staged card is the ask, and the attribute still says so", () => {
    inRail(
      hook("brief", [{ component: "ap:markdown", props: { body: "x" } }]),
      { waiting: new Set(["brief"]) },
    );
    expect(hookEl("brief")).toHaveAttribute("data-hook-waiting", "true");
    expect(screen.queryByTestId("agent-ui-hook-waiting")).toBeNull();
    expect(screen.queryByTestId("agent-ui-hook-cues")).toBeNull();
  });

  it("keeps the waiting chip under a column layout, where nothing else marks the ask", () => {
    withState(
      hook("brief", [{ component: "ap:markdown", props: { body: "x" } }]),
      { waiting: new Set(["brief"]) },
    );
    expect(screen.getByTestId("agent-ui-hook-waiting")).toBeInTheDocument();
  });
});
