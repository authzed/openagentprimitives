import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { JSONView } from "./JSONView";

afterEach(cleanup);

describe("JSONView", () => {
  it("renders an object value as a keyed tree", () => {
    render(<JSONView value={{ verb: "get", count: 3, nested: { pod: "web" } }} />);
    // Keys are labeled; primitives render (strings JSON-quoted).
    expect(screen.getByText("verb:")).toBeTruthy();
    expect(screen.getByText('"get"')).toBeTruthy();
    expect(screen.getByText("count:")).toBeTruthy();
    expect(screen.getByText("3")).toBeTruthy();
    // Nested objects recurse.
    expect(screen.getByText("nested:")).toBeTruthy();
    expect(screen.getByText("pod:")).toBeTruthy();
  });

  it("the raw toggle swaps the tree for JSON.stringify(value, null, 2)", () => {
    const value = { verb: "get", resource: "pods" };
    render(<JSONView value={value} />);
    // Starts in tree mode; toggle is labeled "raw".
    fireEvent.click(screen.getByRole("button", { name: "raw" }));
    // After toggling, the button flips to "tree" and the raw pre appears.
    expect(screen.getByRole("button", { name: "tree" })).toBeTruthy();
    const expected = JSON.stringify(value, null, 2);
    expect(
      screen.getByText((_, el) => el?.tagName === "PRE" && el.textContent === expected),
    ).toBeTruthy();
  });

  it("renders a plain-string value (no keys), so tool output that is a string works", () => {
    render(<JSONView value={"hello world"} />);
    expect(screen.getByText('"hello world"')).toBeTruthy();
  });

  const NESTED = {
    results: [{ id: 56426022008, properties: { domain: "al.st" }, displayName: "Allstate" }],
    total: 4,
  };

  it("keys of object/array values live inside the toggle line, so children indent from the row start (not after the key)", () => {
    render(<JSONView value={NESTED} />);

    // The key of a nested object is part of its collapse toggle — the layout
    // that makes children hang at a fixed indent from the row start instead of
    // being pushed right by the key's width.
    const key = screen.getByText("properties:");
    const toggle = key.closest("button");
    expect(toggle).not.toBeNull();
    expect(toggle!.getAttribute("aria-expanded")).toBe("true");

    // Collapsing via that toggle hides the enclosed children.
    fireEvent.click(toggle!);
    expect(toggle!.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByText("domain:")).toBeNull();
  });

  it("nests children at a shallow ~2-character indent per level", () => {
    render(<JSONView value={NESTED} />);

    // The child-row container indents ml-2 + pl-2 (~16px ≈ 2 mono chars), not
    // the previous ml-3 + pl-3.
    const gutter = screen.getByText("domain:").closest('div[class*="border-l"]');
    expect(gutter).not.toBeNull();
    expect(gutter!.className).toContain("pl-2");
    expect(gutter!.className).toContain("ml-2");
  });

  it("array items carry no synthetic index key", () => {
    render(<JSONView value={NESTED} />);

    expect(screen.getByText('"Allstate"')).toBeTruthy();
    expect(screen.queryByText("0:")).toBeNull();
  });
});
