import "@testing-library/jest-dom/vitest";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { ActionsProvider } from "./actions";
import { renderNode } from "./renderNode";
import type { Node } from "./types";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function node(component: string, props?: Record<string, unknown>): Node {
  return { component, props } as Node;
}

const ROWS = [
  { name: "Acme", domain: "acme.example", score: 82 },
  { name: "Globex", domain: "globex.example", score: 55 },
];

function tableNode(extra?: Record<string, unknown>): Node {
  return node("ap:table", {
    columns: [
      { key: "name", header: "Company" },
      { key: "domain", header: "Domain" },
    ],
    rows: ROWS,
    ...extra,
  });
}

function renderTable(n: Node, invoke = vi.fn()) {
  render(
    <ActionsProvider value={{ states: {}, invoke, answer: vi.fn() }}>
      {renderNode(n)}
    </ActionsProvider>,
  );
  return invoke;
}

// A table of records is the shape a viewer most often wants to act on one row
// at a time, and until now there was no way to say so: the declaration would
// have had to name the record in advance, which is exactly what it cannot do
// when the rows come from a binding.
describe("ap:table row actions", () => {
  it("renders no row control when the table declares none", () => {
    renderTable(tableNode());
    expect(screen.queryAllByTestId("agent-ui-row-action")).toHaveLength(0);
  });

  it("renders one control per row, labelled by the declaration", () => {
    renderTable(
      tableNode({ rowAction: "company_detail", rowActionLabel: "Ask about" }),
    );
    const buttons = screen.getAllByTestId("agent-ui-row-action");
    expect(buttons).toHaveLength(ROWS.length);
    expect(buttons[0]).toHaveTextContent("Ask about");
  });

  // A control whose effect nobody can guess is worse than no control.
  it("falls back to a readable label when the author gave none", () => {
    renderTable(tableNode({ rowAction: "company_detail" }));
    expect(screen.getAllByTestId("agent-ui-row-action")[0]).toHaveTextContent(
      "Details",
    );
  });

  // The point of the whole feature: the row's OWN values reach the action, so
  // the sentence it composes can be about that record.
  it("fires the action with the values of the row whose control was pressed", () => {
    const invoke = renderTable(tableNode({ rowAction: "company_detail" }));
    fireEvent.click(screen.getAllByTestId("agent-ui-row-action")[1]);
    expect(invoke).toHaveBeenCalledWith("company_detail", {
      name: "Globex",
      domain: "globex.example",
      score: "55",
    });
  });

  // Inputs are a flat string map. A value no text field can honestly carry is
  // dropped rather than stringified into "[object Object]", which would push
  // the mistake into a sentence the agent reads and make it harder to see.
  it("drops a row value that is not a scalar", () => {
    const invoke = renderTable(
      node("ap:table", {
        columns: [{ key: "name" }],
        rows: [{ name: "Acme", nested: { a: 1 }, list: [1, 2], missing: null }],
        rowAction: "company_detail",
      }),
    );
    fireEvent.click(screen.getByTestId("agent-ui-row-action"));
    expect(invoke).toHaveBeenCalledWith("company_detail", { name: "Acme" });
  });
});

// ap:table became a React component to reach useActions(), which moved its
// body past renderNode's try/catch — the guard that makes ONE malformed node
// fail alone instead of unmounting the page. renderNode's own doc comment
// records why a React error boundary cannot stand in for that catch. The shape
// is therefore read in the eagerly-run registry entry, inside the guard.
describe("a malformed table still fails alone", () => {
  it("renders a failure card instead of throwing through React", () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    const failures: string[] = [];
    expect(() =>
      render(
        <ActionsProvider
          value={{ states: {}, invoke: vi.fn(), answer: vi.fn() }}
        >
          {renderNode(
            node("ap:table", { columns: { key: "name" }, rows: [] }),
            undefined,
            (c) => failures.push(c),
          )}
        </ActionsProvider>,
      ),
    ).not.toThrow();
    expect(failures).toContain("ap:table");
  });
});
