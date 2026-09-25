import "@testing-library/jest-dom/vitest";
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { applyBindings, bindingPath } from "./bindings";
import { renderNode } from "./renderNode";
import type { Node } from "./types";

afterEach(cleanup);

const hook = (name: string, children: Node[]): Node => ({
  component: "oap:generative",
  props: { name, allowedComponents: ["*"] },
  children,
});

// The fixture puts bound nodes on BOTH sides of the region boundary, because
// the region rule is the thing under test: the two nodes inside the hook are
// addressed from the hook ("x/0#rows"), and the one outside it is addressed
// from the view root ("/0#text"). A walk that ignored hooks would number all
// three from the root and every key would miss.
const view: Node = {
  component: "ap:stack",
  children: [
    {
      component: "ap:text",
      props: { text: "outside" },
      bindings: { text: { source: "memory", ref: "m" } },
    },
    hook("x", [
      {
        component: "ap:table",
        props: { columns: [{ key: "name" }], rows: [] },
        bindings: { rows: { source: "tool", ref: "crm_list_leads" } },
      },
      {
        component: "ap:markdown",
        props: { body: "loading..." },
        bindings: { body: { source: "artifact", ref: "artifact-x" } },
      },
    ]),
  ],
};

describe("bindingPath", () => {
  // Readable documentation of the format, NOT the pin: a literal here and a
  // matching one in walk_test.go can be swept together with no suite noticing.
  // The pin is the shared golden both languages read —
  // pkg/web/webui/agentui/ui/bindingsGolden.test.tsx.
  it("spells out the format: <region>/<dotted node index>#<prop>", () => {
    expect(bindingPath("root", [], "rows")).toBe("root/#rows");
    expect(bindingPath("root", [0, 2], "rows")).toBe("root/0.2#rows");
    expect(bindingPath("side", [1], "body")).toBe("side/1#body");
    // Outside every hook the region is the empty string, so the key opens with
    // the separator. This is a real key the bindings route answers for, not a
    // degenerate one: a page may bind props with no hook above them at all.
    expect(bindingPath("", [0], "text")).toBe("/0#text");
  });
});

// The REGION rule, mirroring uicomponents.regionCursor: a node's region is its
// nearest enclosing hook's name ("" outside any hook), and its index path is
// measured from that region's root — the hook node, or the view root.
describe("applyBindings addresses a node by its region, not by its depth", () => {
  it("addresses a node inside a hook from the HOOK, not from the view root", () => {
    const out = applyBindings(view, {
      "x/0#rows": { status: "ok", value: [{ name: "Acme" }] },
    });
    render(renderNode(out));
    expect(screen.getByText("Acme")).toBeInTheDocument();
  });

  it("addresses a node outside every hook from the view root, with an empty region", () => {
    const out = applyBindings(view, {
      "/0#text": { status: "ok", value: "resolved copy" },
    });
    render(renderNode(out));
    expect(screen.getByText("resolved copy")).toBeInTheDocument();
  });

  // The view ROOT may itself be a hook — {"view":{"component":"oap:generative",…}}
  // is a legal authored page — and then everything under it, including a
  // binding on the root node itself, is in that hook's region. rootCursor is
  // the Go half of this; without its mirror here the root would be a silent
  // exception to the rule, since it is never reached through a parent.
  // What makes a node a region is its COMPONENT TYPE, never whether its name
  // prop decoded — uicomponents.hookOf's rule, and
  // TestWalkBindingsTreatsANamelessHookAsARegion is the Go half of this exact
  // tree. Reading a nameless hook as an ordinary container instead would put
  // the table below at "/1.0#rows" here while the server wrote "/0#rows", so
  // every binding under it would stick on its declared placeholder with the
  // response a clean 200. Validate rejects such a page long before a browser
  // sees it; the walks still have to agree on it, which is what lets the
  // rejection live in one place.
  it("treats a NAMELESS hook as a region too, opening the empty one", () => {
    const nameless: Node = {
      component: "ap:stack",
      children: [
        { component: "ap:text", props: { text: "outside" } },
        {
          component: "oap:generative",
          props: { allowedComponents: ["*"] },
          children: [
            {
              component: "ap:table",
              props: { columns: [{ key: "name" }], rows: [] },
              bindings: { rows: { source: "tool", ref: "crm_list_leads" } },
            },
          ],
        },
      ],
    };
    const out = applyBindings(nameless, {
      "/0#rows": { status: "ok", value: [{ name: "Acme" }] },
    });
    render(renderNode(out));
    expect(screen.getByText("Acme")).toBeInTheDocument();
  });

  it("seeds the region from the view root when the root IS a hook", () => {
    const rootIsHook = hook("whole", [
      {
        component: "ap:text",
        props: { text: "placeholder" },
        bindings: { text: { source: "tool", ref: "t" } },
      },
    ]);
    const out = applyBindings(rootIsHook, {
      "whole/0#text": { status: "ok", value: "in the root hook" },
    });
    render(renderNode(out));
    expect(screen.getByText("in the root hook")).toBeInTheDocument();
  });
});

describe("applyBindings", () => {
  it("replaces a bound prop with its resolved value", () => {
    const out = applyBindings(view, {
      "x/0#rows": { status: "ok", value: [{ name: "Acme" }] },
    });
    render(renderNode(out));
    expect(screen.getByText("Acme")).toBeInTheDocument();
  });

  // This SUPERSEDES the declared-placeholder pattern, deliberately. The rule
  // used to be that a bound node kept its declared literal while loading and
  // the author's own text ("loading...") was the loading UI.
  //
  // It did not survive contact: the literal an author actually writes is
  // content, not a spinner — a table declares `empty: "No companies in this
  // window."` — so a re-resolving section looked exactly like a finished empty
  // one. A viewer who changed a filter saw nothing happen and concluded the
  // control was broken. A placeholder that cannot be told apart from a result
  // is worse than none.
  it("shows a skeleton while a binding is loading, not the declared literal", () => {
    const out = applyBindings(view, { "x/1#body": { status: "loading" } });
    render(renderNode(out));
    // Every bound node in this fixture is unresolved — the others default to
    // loading — so all of them stand in. What matters is that none shows its
    // declared literal as though it were a result.
    expect(screen.getAllByTestId("ap-skeleton").length).toBeGreaterThan(0);
    expect(screen.queryByText("loading...")).not.toBeInTheDocument();
  });

  it("replaces only the failing node, and the rest of the view keeps working", () => {
    const out = applyBindings(view, {
      "x/0#rows": { status: "ok", value: [{ name: "Acme" }] },
      "x/1#body": {
        status: "error",
        message: "This view's data is not available.",
      },
    });
    render(renderNode(out));
    expect(screen.getByText("Acme")).toBeInTheDocument();
    expect(
      screen.getByText("This view's data is not available."),
    ).toBeInTheDocument();
    expect(screen.queryByText("loading...")).not.toBeInTheDocument();
  });

  it("is a pure function — the input view is not mutated", () => {
    const before = JSON.stringify(view);
    applyBindings(view, {
      "x/0#rows": { status: "ok", value: [{ name: "Acme" }] },
    });
    expect(JSON.stringify(view)).toBe(before);
  });

  it("returns a NEW tree rather than the one it was handed", () => {
    // Purity is not only "the input is unchanged": a rewrite that returned the
    // input object for an unbound subtree would leave the caller unable to tell
    // a resolved tree from a declared one by identity, and React's own
    // reconciliation would see no change where one had happened.
    const out = applyBindings(view, {});
    expect(out).not.toBe(view);
    expect(out.children?.[1]).not.toBe(view.children?.[1]);
  });

  it("leaves an unbound view untouched in VALUE", () => {
    const plain: Node = { component: "ap:text", props: { text: "hi" } };
    expect(applyBindings(plain, {})).toEqual(plain);
  });

  // A control is the ONLY thing that can call setParam, and useBindings
  // re-evaluates on a parameter change. Replacing a failed control with an
  // error card therefore removes the single affordance that could retry: the
  // parameter map can never change again, so no new request is ever issued and
  // a reload just re-runs the same failing tool. "The rest of the page keeps
  // working" has to include the page's own controls.
  const controlView: Node = {
    component: "ap:stack",
    children: [
      {
        component: "ap:select",
        props: {
          param: "span",
          value: "7d",
          options: [{ value: "7d", label: "7 days" }],
        },
        bindings: { options: { source: "tool", ref: "list_spans" } },
      },
    ],
  };

  it("keeps a failed CONTROL rendered and interactive, showing the error beside it", () => {
    const out = applyBindings(controlView, {
      "/0#options": {
        status: "error",
        message: "This view's data is not available.",
      },
    });
    render(renderNode(out));

    const select = screen.getByRole("combobox");
    expect(select).toBeVisible();
    expect(select).not.toBeDisabled();
    // Its declared literal options survive, so the viewer has something to
    // pick and therefore something that triggers a re-evaluation.
    expect(screen.getByText("7 days")).toBeInTheDocument();
    expect(
      screen.getByText("This view's data is not available."),
    ).toBeVisible();
  });
});

describe("applyBindings — waiting on the viewer is not a failure", () => {
  const bound = (): Node =>
    hook("companies", [
      {
        component: "ap:table",
        bindings: { rows: { source: "tool", ref: "t" } },
      },
    ]);

  // A control with no literal default — a date range that must be picked live,
  // because a baked-in default would go stale — leaves its binding unsatisfied
  // on first paint. That is the ordinary opening state of the page, not a
  // fault, and it used to render the same destructive "Cannot load this
  // section" card a real failure does: a red box over a filter nobody had
  // touched yet, announcing a problem the viewer had not caused.
  it("stands in with ap:empty, never ap:error", () => {
    const out = applyBindings(bound(), {
      "companies/0#rows": {
        status: "needs_input",
        message: "Choose a value above to load this data.",
      },
    });
    expect(out.children?.[0]?.component).toBe("ap:empty");
    expect(out.children?.[0]?.props?.body).toBe(
      "Choose a value above to load this data.",
    );
  });

  it("still uses ap:error for a genuine failure", () => {
    const out = applyBindings(bound(), {
      "companies/0#rows": {
        status: "error",
        message: "this view's data could not be loaded",
      },
    });
    expect(out.children?.[0]?.component).toBe("ap:error");
  });

  // Precedence, and it matters: a node with one binding broken and another
  // merely waiting must report the BREAK. Reporting the gentler of the two
  // would hide a real fault behind a prompt to pick a filter.
  it("reports the failure when one binding is broken and another is waiting", () => {
    const twoBindings: Node = hook("companies", [
      {
        component: "ap:table",
        bindings: {
          rows: { source: "tool", ref: "t" },
          footer: { source: "tool", ref: "u" },
        },
      },
    ]);
    const out = applyBindings(twoBindings, {
      "companies/0#footer": { status: "needs_input", message: "waiting" },
      "companies/0#rows": { status: "error", message: "broken" },
    });
    expect(out.children?.[0]?.component).toBe("ap:error");
    expect(out.children?.[0]?.props?.body).toBe("broken");
  });

  // Same rule the error arm follows: a CONTROL keeps rendering, with the
  // stand-in beside it, or the viewer loses the very control they are being
  // asked to use.
  it("keeps a control usable and puts the stand-in beside it", () => {
    const control: Node = hook("filters", [
      {
        component: "ap:select",
        props: { param: "minScore", options: [{ value: "0" }] },
        bindings: { options: { source: "tool", ref: "t" } },
      },
    ]);
    const out = applyBindings(control, {
      "filters/0#options": {
        status: "needs_input",
        message: "Choose a value above to load this data.",
      },
    });
    expect(out.children?.[0]?.component).toBe("ap:stack");
    expect(out.children?.[0]?.children?.[0]?.component).toBe("ap:select");
    expect(out.children?.[0]?.children?.[1]?.component).toBe("ap:empty");
  });
});
