import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
// No global jest-dom setupFile is wired for this package (see
// renderNode.test.tsx) — import the vitest adapter locally.
import "@testing-library/jest-dom/vitest";
import { renderNode } from "./renderNode";
import type { Node } from "./types";

// vitest.config.ts sets globals: false, so @testing-library/react never finds
// a global afterEach to register its cleanup with. Without this, a render()
// from one test can still be in document.body when the next test's query
// runs — see renderNode.test.tsx for the same repo pattern.
afterEach(cleanup);

// A representative Tier-0 declaration: the shape an author packages and that
// must render complete, working UI with no agent turn involved. The page is
// ONE tree, and the region the agent may later write is a hook INSIDE it —
// which is what makes the Tier-0 claim checkable here: everything below renders
// from the author's own nodes, with no agent turn and no fill. Fabricated names
// only — this fixture lives in the test, never under the repo's demo directory
// (see AGENTS.md: tests must not reference that tree).
const rootView: Node = {
  component: "oap:generative",
  props: { name: "root", allowedComponents: ["*"] },
  children: [
    {
      component: "ap:stack",
      props: { gap: "lg" },
      children: [
        { component: "ap:heading", props: { text: "Demo Console", level: 1 } },
        {
          component: "ap:grid",
          props: { columns: 2 },
          children: [
            {
              component: "ap:metric",
              props: { label: "Open", value: "42", delta: "+3", trend: "up" },
            },
            {
              component: "ap:metric",
              props: {
                label: "Closed",
                value: "17",
                delta: "-1",
                trend: "down",
              },
            },
          ],
        },
        {
          component: "ap:card",
          props: { title: "Recent" },
          children: [
            {
              component: "ap:table",
              props: {
                columns: [
                  { key: "name", header: "Name" },
                  { key: "stage", header: "Stage" },
                ],
                rows: [
                  { name: "alpha", stage: "new" },
                  { name: "beta", stage: "review" },
                ],
              },
            },
          ],
        },
        { component: "ap:markdown", props: { body: "Updated **hourly**." } },
      ],
    },
  ],
};

describe("a Tier-0 declaration renders end to end", () => {
  it("renders every node in the tree", () => {
    render(renderNode(rootView));

    expect(
      screen.getByRole("heading", { name: "Demo Console" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Open")).toBeInTheDocument();
    expect(screen.getByText("42")).toBeInTheDocument();
    // ap:grid's SECOND child — without this, a renderer bug that mapped only
    // children[0] would leave every other assertion in this file passing.
    expect(screen.getByText("Closed")).toBeInTheDocument();
    expect(screen.getByText("17")).toBeInTheDocument();
    expect(screen.getByText("Recent")).toBeInTheDocument();
    expect(screen.getByText("alpha")).toBeInTheDocument();
    expect(screen.getByText("review")).toBeInTheDocument();
    expect(screen.getByText("hourly")).toBeInTheDocument();
  });

  it("names the hook so a live update can address it", () => {
    // data-hook is how update_view's target region is found in the rendered
    // page: the name on the wire and the name in the DOM are the same string.
    const { container } = render(renderNode(rootView));
    expect(container.querySelector('[data-hook="root"]')).toBeInTheDocument();
  });
});
