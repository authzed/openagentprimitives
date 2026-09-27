import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
} from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
// No existing test in this package uses jest-dom matchers, and the project
// doesn't wire a global setupFile for it (see packages/design's installs.test.tsx).
// Import the vitest adapter locally so toBeInTheDocument() is available here too.
import "@testing-library/jest-dom/vitest";
import { renderNode } from "./renderNode";
import { COMPONENTS } from "./registry";
import type { Node } from "./types";

// vitest.config.ts sets globals: false, so @testing-library/react never finds
// a global afterEach to auto-register its cleanup with — every render() past
// the first would otherwise accumulate in document.body instead of replacing
// it, and a later test's query can silently match an earlier test's leftover
// DOM. This file has enough same-role/same-text renders (multiple ap:alert
// variants, repeated ap:tabs runs) that a collision surfaces quickly without
// this; see packages/design's markdown.test.tsx for the same repo pattern.
afterEach(cleanup);

const node = (
  component: string,
  props: Record<string, unknown> = {},
  children: Node[] = [],
): Node => ({
  component,
  props,
  children,
});

describe("renderNode", () => {
  it("renders a known leaf component from its props", () => {
    render(renderNode(node("ap:heading", { text: "Leads", level: 2 })));
    expect(screen.getByRole("heading", { name: "Leads" })).toBeInTheDocument();
  });

  it("renders children of a container component", () => {
    render(
      renderNode(node("ap:stack", {}, [node("ap:text", { text: "inner" })])),
    );
    expect(screen.getByText("inner")).toBeInTheDocument();
  });

  it("falls back to a visible error for an unknown component instead of rendering nothing", () => {
    render(renderNode(node("ap:not-a-real-component")));
    expect(screen.getByText(/ap:not-a-real-component/)).toBeInTheDocument();
  });

  it("renders markdown prose as formatted text", () => {
    render(
      renderNode(
        node("ap:markdown", { body: "# Title\n\nsome **bold** text" }),
      ),
    );
    expect(screen.getByRole("heading", { name: "Title" })).toBeInTheDocument();
    expect(screen.getByText("bold")).toBeInTheDocument();
  });

  it("renders table rows from declared columns", () => {
    render(
      renderNode(
        node("ap:table", {
          columns: [{ key: "name", header: "Name" }],
          rows: [{ name: "alpha" }, { name: "beta" }],
        }),
      ),
    );
    expect(screen.getByText("Name")).toBeInTheDocument();
    expect(screen.getByText("alpha")).toBeInTheDocument();
    expect(screen.getByText("beta")).toBeInTheDocument();
  });

  it("renders an empty-state message when a table has no rows", () => {
    render(
      renderNode(
        node("ap:table", {
          columns: [{ key: "name" }],
          rows: [],
          empty: "no leads yet",
        }),
      ),
    );
    expect(screen.getByText("no leads yet")).toBeInTheDocument();
  });

  // @ap/design's CardContent is `p-6 pt-0`, which assumes a header above it.
  // With no title and no description there is no header, and the content sat
  // flush on the card's top border.
  it("pads the top of an untitled card's content, which has no header above it", () => {
    const { container } = render(
      renderNode(node("ap:card", {}, [node("ap:text", { text: "flush" })])),
    );
    const content = (container.firstElementChild as HTMLElement)
      .lastElementChild as HTMLElement;
    expect(content).toHaveTextContent("flush");
    expect(content.className).toContain("pt-6");
  });

  it("leaves a titled card's content alone: its header already holds the top padding", () => {
    const { container } = render(
      renderNode(
        node("ap:card", { title: "Titled" }, [
          node("ap:text", { text: "under" }),
        ]),
      ),
    );
    const content = (container.firstElementChild as HTMLElement)
      .lastElementChild as HTMLElement;
    expect(content).toHaveTextContent("under");
    expect(content.className).not.toContain("pt-6");
  });
});

// VOCABULARY mirrors pkg/web/uicomponents' registered types. It is duplicated here
// rather than derived because the TS test cannot read the Go registry — and
// that duplication is the POINT: this test is the tripwire for the two halves
// drifting apart. A type registered on the server with no renderer here would
// otherwise reach a browser and render as "Unknown component", which reads to a
// user as the agent having done nothing.
//
// oap:generative sits in the same list because the Go registry holds it in the
// same list: it is STRUCTURAL — never published to the agent, which is what the
// oap: namespace says — but it is still a node the browser must be able to
// render, and a page whose regions rendered as "Unknown component" is a page
// with no agent-written content at all.
const VOCABULARY = [
  "ap:agentlink",
  "ap:alert",
  "ap:attachment",
  "ap:badge",
  "ap:button",
  "ap:card",
  "ap:chart",
  "ap:chat",
  "ap:collapsible",
  "ap:daterange",
  "ap:empty",
  "ap:error",
  "ap:form",
  "ap:grid",
  "ap:heading",
  "ap:markdown",
  "ap:metric",
  "ap:notice",
  "ap:progress",
  "ap:question",
  "ap:raw_html",
  "ap:select",
  "ap:session_view",
  "ap:skeleton",
  "ap:stack",
  "ap:status",
  "ap:steps",
  "ap:table",
  "ap:tabs",
  "ap:text",
  "oap:generative",
  "oap:page",
];

describe("renderer coverage", () => {
  it("has a renderer for every registered vocabulary type", () => {
    const missing = VOCABULARY.filter((t) => !(t in COMPONENTS));
    expect(missing).toEqual([]);
  });

  it("registers no renderer for a type the server does not know", () => {
    const extra = Object.keys(COMPONENTS).filter(
      (t) => !VOCABULARY.includes(t),
    );
    expect(extra).toEqual([]);
  });

  it("renders every vocabulary type without throwing on empty props", () => {
    for (const type of VOCABULARY) {
      expect(
        () => render(renderNode({ component: type })),
        `${type} must render with no props`,
      ).not.toThrow();
    }
  });
});

// ap:raw_html is the one place in this package that touches agent-authored
// HTML at all. Everywhere else, "the vocabulary is closed" is enforced by
// COMPONENTS simply not having a branch that could inline it. This guard is
// the one place that invariant has to be proven directly: swapping the
// placeholder below for `dangerouslySetInnerHTML={{ __html: props.html }}`
// would make every other test in this file still pass.
describe("ap:raw_html security", () => {
  it("never inlines the declared HTML into the DOM, even when it carries a payload", () => {
    const { container } = render(
      renderNode(
        node("ap:raw_html", {
          html: '<img src=x onerror="alert(1)"><b>PWNED</b>',
        }),
      ),
    );
    expect(container.innerHTML).not.toContain("PWNED");
    expect(container.innerHTML).not.toContain("<img");
    expect(
      screen.getByText(/renders once the sandboxed content view is available/),
    ).toBeInTheDocument();
  });
});

describe("behavioral coverage: leaf and container logic", () => {
  it("renders a metric's label, value, and a trend-colored delta", () => {
    render(
      renderNode(
        node("ap:metric", {
          label: "MRR",
          value: "$12k",
          delta: "+4%",
          trend: "up",
        }),
      ),
    );
    expect(screen.getByText("MRR")).toBeInTheDocument();
    expect(screen.getByText("$12k")).toBeInTheDocument();
    expect(screen.getByText("+4%").className).toContain(
      "text-[hsl(var(--success))]",
    );
  });

  // ap:metric declares value/delta/caption BINDABLE (pkg/web/uicomponents), and a
  // binding's value comes from a live endpoint that has no idea the prop was
  // typed `string` in Go. A CRM answering `{"total": 42}` is the ordinary
  // case. Rejecting the number left the platform advertising a binding and
  // then silently dropping it: the metric rendered blank, indistinguishable
  // from genuinely empty, with nothing anywhere saying a value had arrived.
  it("renders a numeric bound value rather than dropping it for not being a string", () => {
    render(
      renderNode(node("ap:metric", { label: "Companies matched", value: 42 })),
    );
    expect(screen.getByText("42")).toBeInTheDocument();
  });

  it("renders a zero, which is the one number most easily mistaken for absent", () => {
    render(
      renderNode(node("ap:metric", { label: "Companies matched", value: 0 })),
    );
    expect(screen.getByText("0")).toBeInTheDocument();
  });

  // A selector pointed at an object or an array is an authoring mistake, and
  // "[object Object]" states it less clearly than an empty slot does. These
  // still fall back — the coercion is for scalars a viewer can read, not a
  // license to stringify anything.
  it("still falls back for a value no text slot can honestly display", () => {
    const { container } = render(
      renderNode(node("ap:metric", { label: "Broken", value: { a: 1 } })),
    );
    expect(container.textContent).not.toContain("object Object");
    const { container: arr } = render(
      renderNode(node("ap:metric", { label: "Broken", value: [1, 2] })),
    );
    expect(arr.textContent).not.toContain("1,2");
  });

  it("renders a no-data empty state when ap:chart has no series or data", () => {
    render(renderNode(node("ap:chart")));
    expect(screen.getByText("No data to chart.")).toBeInTheDocument();
  });

  it("renders one <option> per declared ap:select option", () => {
    const { container } = render(
      renderNode(
        node("ap:select", {
          options: [
            { value: "a", label: "Alpha" },
            { value: "b", label: "Beta" },
          ],
        }),
      ),
    );
    const options = container.querySelectorAll("option");
    expect(options).toHaveLength(2);
    expect(options[0]).toHaveTextContent("Alpha");
    expect(options[1]).toHaveTextContent("Beta");
  });

  it("renders ap:empty's default title when no title is declared", () => {
    render(renderNode(node("ap:empty")));
    expect(screen.getByText("Nothing here yet")).toBeInTheDocument();
  });

  it("renders ap:error's default title when no title is declared", () => {
    render(renderNode(node("ap:error")));
    expect(screen.getByText("Something went wrong")).toBeInTheDocument();
  });

  it("renders one trigger per declared ap:tabs tab", () => {
    render(
      renderNode(
        node("ap:tabs", {
          tabs: [
            { value: "a", label: "Alpha" },
            { value: "b", label: "Beta" },
          ],
        }),
      ),
    );
    expect(screen.getByRole("tab", { name: "Alpha" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Beta" })).toBeInTheDocument();
  });
});

// Every renderer in COMPONENTS is invoked EAGERLY by renderNode, so a TypeError
// inside one (calling .map on a prop that is not an array) propagates
// synchronously out through every ancestor and blanks the entire tree — not
// just the offending node. That is the failure UnknownComponent exists to
// prevent, one order of magnitude worse, and it arrives through the same door:
// version skew between a tab and the server that validated the declaration.
// Prop SHAPES skew at least as readily as type NAMES do.
//
// A "does not throw" assertion would not pin this. The invariant is that the
// SIBLINGS still render, so each case asserts on a sibling.
describe("a shape-mismatched node fails alone, not the whole tree", () => {
  let consoleError: typeof console.error;
  let logged: unknown[][];

  beforeEach(() => {
    // The fallback is deliberately paired with a console.error: the visible
    // element tells the user a section is missing, the log tells whoever is
    // debugging which node and why. Capturing it here both keeps the expected
    // errors out of the test output and lets the log itself be asserted.
    consoleError = console.error;
    logged = [];
    console.error = (...args: unknown[]) => {
      logged.push(args);
    };
  });

  afterEach(() => {
    console.error = consoleError;
  });

  const cases: { name: string; bad: Node }[] = [
    {
      name: "ap:table with columns as an object",
      bad: node("ap:table", { columns: {}, rows: [{ a: 1 }] }),
    },
    {
      name: "ap:chart with series as a string",
      bad: node("ap:chart", { series: "revenue", data: [{ x: 1 }] }),
    },
    {
      name: "ap:tabs with tabs as a number",
      bad: node("ap:tabs", { tabs: 3 }),
    },
    // A null STEP, not a null steps prop: the array is the right shape and one
    // of its elements is not an object. StepsNode is a mounted component, so a
    // throw from inside it would escape this catch entirely and unmount the
    // page — the entry has to refuse the element before React sees it.
    {
      name: "ap:steps with a null step in the list",
      bad: node("ap:steps", {
        steps: [null, { id: "tools", label: "Tools", state: "active" }],
      }),
    },
  ];

  for (const tc of cases) {
    it(`renders the surrounding siblings when a node is malformed: ${tc.name}`, () => {
      render(
        renderNode(
          node("ap:stack", {}, [
            node("ap:text", { text: "before" }),
            tc.bad,
            node("ap:text", { text: "after" }),
          ]),
        ),
      );
      expect(screen.getByText("before")).toBeInTheDocument();
      expect(screen.getByText("after")).toBeInTheDocument();
      expect(screen.getByText(/could not be displayed/)).toBeInTheDocument();
      expect(logged).toHaveLength(1);
      expect(String(logged[0][0])).toContain(tc.bad.component);
    });
  }
});

// The fail-visible cards are drawn INSIDE the content region, which is the
// agent-owned half of the page. The platform chrome that must auto-reveal on a
// hard error is a sibling of that region, not an ancestor of these elements —
// so a failure has to leave this module as data, not only as markup a caller
// would have to sniff for. RenderErrorSink is that channel; the card is still
// returned either way.
describe("render failures are reported to an onError sink, not only drawn", () => {
  let consoleError: typeof console.error;

  beforeEach(() => {
    // The malformed-node rows below reach renderNode's catch, which logs. Keep
    // the expected noise out of the test output.
    consoleError = console.error;
    console.error = () => {};
  });

  afterEach(() => {
    console.error = consoleError;
  });

  it("reports an unknown component's type", () => {
    const seen: string[] = [];
    render(
      renderNode(node("ap:not-a-real-component"), undefined, (c) =>
        seen.push(c),
      ),
    );
    expect(seen).toEqual(["ap:not-a-real-component"]);
  });

  it("reports a known component whose props are the wrong shape", () => {
    const seen: string[] = [];
    render(
      renderNode(
        node("ap:table", { columns: {}, rows: [{ a: 1 }] }),
        undefined,
        (c) => seen.push(c),
      ),
    );
    expect(seen).toEqual(["ap:table"]);
  });

  // A container renderer reaches its children through the callback renderNode
  // hands it, so a sink that were not threaded through that callback would
  // report nothing here while still reporting the two rows above.
  it("reports a failure nested inside a container's subtree", () => {
    const seen: string[] = [];
    render(
      renderNode(
        node("ap:stack", {}, [
          node("ap:text", { text: "fine" }),
          node("ap:card", {}, [node("ap:nope")]),
        ]),
        undefined,
        (c) => seen.push(c),
      ),
    );
    expect(seen).toEqual(["ap:nope"]);
  });

  it("reports nothing when every node renders", () => {
    const seen: string[] = [];
    render(
      renderNode(
        node("ap:stack", {}, [node("ap:text", { text: "fine" })]),
        undefined,
        (c) => seen.push(c),
      ),
    );
    expect(seen).toEqual([]);
  });

  it("forwards the sink into a hook's tree", () => {
    // A hook reaches its children through the same renderChild callback every
    // other container gets, so a renderer that rendered them by some other
    // route would report nothing here while the rows above still passed.
    const seen: string[] = [];
    render(
      renderNode(
        node("oap:generative", { name: "region" }, [node("ap:nope")]),
        undefined,
        (c) => seen.push(c),
      ),
    );
    expect(seen).toEqual(["ap:nope"]);
  });

  it("still draws the fail-visible card when a sink is supplied", () => {
    render(renderNode(node("ap:not-a-real-component"), undefined, () => {}));
    expect(screen.getByText(/Cannot render this section/)).toBeInTheDocument();
  });
});

// A hook is a node like any other: renderNode resolves it through COMPONENTS
// and recurses into its children. These two rows pin that it is ADDRESSABLE
// (data-hook names the region a live update targets) and that an empty one is
// still PRESENT — a hook the agent has yet to write, or has just cleared,
// leaves an element behind rather than vanishing from the page. hooks.test.tsx
// owns everything the region's own state adds on top.
describe("oap:generative is reached through renderNode", () => {
  it("renders a hook's children, tagged with its name", () => {
    const { container } = render(
      renderNode(
        node("oap:generative", { name: "header" }, [
          node("ap:text", { text: "hook content" }),
        ]),
      ),
    );
    expect(screen.getByText("hook content")).toBeInTheDocument();
    expect(container.querySelector('[data-hook="header"]')).toBeInTheDocument();
  });

  it("renders a present-but-empty region when the hook has no children", () => {
    const { container } = render(
      renderNode(node("oap:generative", { name: "footer" })),
    );
    const el = container.querySelector('[data-hook="footer"]');
    expect(el).toBeInTheDocument();
    expect(el).toBeEmptyDOMElement();
  });
});

// Every prop the Go vocabulary declares must have a VISIBLE effect here. The
// server's validateProps rejects a prop NAME it has never heard of, but it does
// not check enum VALUES and it cannot know whether the renderer reads a prop at
// all — so a prop this file does not honour reaches the browser marked
// "accepted" and then renders as if the agent had never written it. These are
// the props whose effect is a lookup or a branch rather than plain text, i.e.
// the ones where "accepted but ignored" is possible.
describe("every declared vocabulary prop has a visible effect", () => {
  it("gives ap:alert severity=warning a distinct treatment from info and error", () => {
    render(
      renderNode(node("ap:alert", { severity: "warning", title: "Heads up" })),
    );
    const alert = screen.getByRole("alert");
    expect(alert.className).toContain("text-warning");
    expect(alert.className).not.toContain("text-destructive");
  });

  it("center-aligns an ap:table column declared align=center", () => {
    render(
      renderNode(
        node("ap:table", {
          columns: [{ key: "status", header: "Status", align: "center" }],
          rows: [{ status: "ok" }],
        }),
      ),
    );
    expect(screen.getByText("ok").className).toContain("text-center");
  });

  it("renders a textarea for kind=textarea and a number input for kind=number", () => {
    render(
      renderNode(
        node("ap:form", {
          fields: [
            { name: "bio", label: "Bio", kind: "textarea" },
            { name: "age", label: "Age", kind: "number" },
          ],
        }),
      ),
    );
    expect(screen.getByLabelText("Bio").tagName).toBe("TEXTAREA");
    expect(screen.getByLabelText("Age")).toHaveAttribute("type", "number");
  });

  it("ap:form seeds its fields from values, and the person can still edit", () => {
    render(
      renderNode(
        node("ap:form", {
          action: "describe_agent",
          submitLabel: "Save",
          fields: [
            {
              name: "description",
              kind: "textarea",
              label: "What should it do?",
            },
          ],
          values: { description: "a haiku on any topic" },
        }),
      ),
    );
    const ta = screen.getByLabelText(
      "What should it do?",
    ) as HTMLTextAreaElement;
    expect(ta.value).toBe("a haiku on any topic");
    fireEvent.change(ta, {
      target: { value: "a haiku on any topic, in chat" },
    });
    expect(ta.value).toBe("a haiku on any topic, in chat");
  });

  it("renders ap:select's placeholder as the empty-value option", () => {
    const { container } = render(
      renderNode(
        node("ap:select", {
          placeholder: "Choose one",
          options: [{ value: "a", label: "Alpha" }],
        }),
      ),
    );
    const options = container.querySelectorAll("option");
    expect(options).toHaveLength(2);
    expect(options[0]).toHaveValue("");
    expect(options[0]).toHaveTextContent("Choose one");
  });

  it("maps ap:stack align=center to an items-center class", () => {
    const { container } = render(
      renderNode(
        node("ap:stack", { align: "center" }, [node("ap:text", { text: "x" })]),
      ),
    );
    expect(container.firstElementChild?.className).toContain("items-center");
  });

  it("clamps ap:grid columns to the documented 1..12 range", () => {
    const { container } = render(
      renderNode(node("ap:grid", { columns: 10000 })),
    );
    const style = (container.firstElementChild as HTMLElement).style
      .gridTemplateColumns;
    expect(style).toBe("repeat(12, minmax(0, 1fr))");
  });
});

// ap:chart's Kind switch (line/bar/area) can only be proven by inspecting
// Recharts' own internal layer classes, which requires an actual render —
// but jsdom has neither ResizeObserver (Recharts' ResponsiveContainer needs
// one to even construct) nor real layout (getBoundingClientRect always
// reports 0x0, and Recharts silently renders nothing at 0x0). Both are
// stubbed for just this block and restored in afterAll; vitest isolates
// jsdom per test FILE by default, so this never reaches another file, and
// nothing outside this describe block renders a chart with data.
describe("ap:chart kind dispatch", () => {
  let originalRect: typeof Element.prototype.getBoundingClientRect;
  let originalRO: typeof globalThis.ResizeObserver;

  beforeAll(() => {
    originalRect = Element.prototype.getBoundingClientRect;
    originalRO = globalThis.ResizeObserver;
    class StubResizeObserver {
      observe() {}
      unobserve() {}
      disconnect() {}
    }
    globalThis.ResizeObserver =
      StubResizeObserver as unknown as typeof ResizeObserver;
    Element.prototype.getBoundingClientRect = () =>
      ({
        width: 300,
        height: 200,
        top: 0,
        left: 0,
        bottom: 200,
        right: 300,
        x: 0,
        y: 0,
        toJSON() {},
      }) as DOMRect;
  });

  afterAll(() => {
    Element.prototype.getBoundingClientRect = originalRect;
    globalThis.ResizeObserver = originalRO;
  });

  const chartNode = (kind?: string) =>
    node("ap:chart", {
      ...(kind ? { kind } : {}),
      xKey: "x",
      series: [{ key: "y" }],
      data: [
        { x: 1, y: 2 },
        { x: 2, y: 3 },
      ],
    });

  it("renders Recharts' bar layer for kind=bar", () => {
    const { container } = render(renderNode(chartNode("bar")));
    expect(container.querySelector(".recharts-bar")).not.toBeNull();
    expect(container.querySelector(".recharts-line")).toBeNull();
  });

  it("renders Recharts' area layer for kind=area", () => {
    const { container } = render(renderNode(chartNode("area")));
    expect(container.querySelector(".recharts-area")).not.toBeNull();
    expect(container.querySelector(".recharts-bar")).toBeNull();
  });

  it("defaults to Recharts' line layer when kind is not declared", () => {
    const { container } = render(renderNode(chartNode()));
    expect(container.querySelector(".recharts-line")).not.toBeNull();
    expect(container.querySelector(".recharts-bar")).toBeNull();
  });
});

// ap:tabs must place every child somewhere reachable, including the two cases
// where the declaration gives it no obvious home: no tabs declared at all, and
// more children than tabs. Dropping a child in either case is the same "blank
// section reads as the agent did nothing" failure the UnknownComponent fallback
// exists to avoid for unknown types.
describe("ap:tabs renders every child", () => {
  it("renders children unwrapped when no tabs are declared", () => {
    render(
      renderNode(
        node("ap:tabs", {}, [node("ap:text", { text: "orphaned content" })]),
      ),
    );
    expect(screen.getByText("orphaned content")).toBeInTheDocument();
  });

  it("routes a child past the last declared tab into the last panel instead of dropping it", () => {
    render(
      renderNode(
        node(
          "ap:tabs",
          {
            tabs: [
              { value: "a", label: "A" },
              { value: "b", label: "B" },
            ],
            value: "b",
          },
          [
            node("ap:text", { text: "first" }),
            node("ap:text", { text: "second" }),
            node("ap:text", { text: "third" }),
          ],
        ),
      ),
    );
    // "b" (the 2nd, LAST declared tab) is made active via `value` so its
    // panel actually mounts into the DOM — Radix's TabsContent does not
    // render an inactive panel's content at all. Its panel should hold both
    // the child explicitly paired to it (index 1, "second") and the surplus
    // 3rd child (index 2, "third"), which has no tab of its own.
    expect(screen.getByText("second")).toBeInTheDocument();
    expect(screen.getByText("third")).toBeInTheDocument();
    // "first" is paired to tab "a", which is not active, so Radix never
    // mounts it — confirming children are still tab-scoped, not just dumped
    // into every panel.
    expect(screen.queryByText("first")).not.toBeInTheDocument();
  });
});

describe("ap:steps timeline", () => {
  it("renders each stage label and marks the active one aria-current", () => {
    render(
      renderNode({
        component: "ap:steps",
        props: {
          steps: [
            { label: "Intake", state: "done" },
            { label: "Tools", state: "active" },
            { label: "Deliver", state: "upcoming" },
          ],
        },
      }),
    );
    expect(screen.getByText("Intake")).toBeInTheDocument();
    expect(screen.getByText("Tools")).toBeInTheDocument();
    expect(screen.getByText("Deliver")).toBeInTheDocument();
    // The active stage is the one a screen reader lands on as "current step",
    // and the timeline moving is the whole reason this is not a row of tabs.
    const active = screen.getByText("Tools").closest("li");
    expect(active).toHaveAttribute("aria-current", "step");
  });

  it("fails one node, not the page, on a malformed steps prop", () => {
    // requireArray runs in the registry entry, before React renders, so a bad
    // shape is caught by renderNode and surfaces as its visible fallback
    // rather than throwing and unmounting the whole tree.
    render(renderNode({ component: "ap:steps", props: { steps: "nope" } }));
    expect(screen.getByText(/ap:steps/)).toBeInTheDocument();
  });

  it("pins the timeline in a sticky wrapper only when asked", () => {
    render(
      renderNode({
        component: "ap:steps",
        props: { pinned: true, steps: [{ label: "Intake", state: "active" }] },
      }),
    );
    expect(screen.getByTestId("agent-ui-steps-pinned").className).toContain(
      "sticky",
    );
    cleanup();
    render(
      renderNode({
        component: "ap:steps",
        props: { steps: [{ label: "Intake", state: "active" }] },
      }),
    );
    expect(screen.queryByTestId("agent-ui-steps-pinned")).toBeNull();
  });
});
